package repodata

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// This file is a minimal MessagePack reader for shards and shard indexes.
// It works on an in-memory buffer, never allocates more than the input
// size, and skips values iteratively, so hostile nesting cannot exhaust the
// stack. (github.com/vmihailenco/msgpack skips recursively.)

var errMsgpackTruncated = errors.New("msgpack: truncated input")

// errBadField marks a value of the wrong type. The reader has consumed the
// whole value, so the caller can drop the enclosing record and continue.
var errBadField = errors.New("bad field")

type mpType uint8

const (
	mpNil mpType = iota
	mpBool
	mpInt  // signed encodings (fixint, int8..int64)
	mpUint // unsigned encodings (uint8..uint64)
	mpFloat
	mpStr
	mpBin
	mpArray
	mpMap
	mpExt
)

func (t mpType) String() string {
	switch t {
	case mpNil:
		return "nil"
	case mpBool:
		return "bool"
	case mpInt, mpUint:
		return "integer"
	case mpFloat:
		return "float"
	case mpStr:
		return "str"
	case mpBin:
		return "bin"
	case mpArray:
		return "array"
	case mpMap:
		return "map"
	default:
		return "ext"
	}
}

// mpValue is one decoded header. Scalars carry their value; str, bin and ext
// carry their payload (aliasing the input); arrays and maps carry their
// element count, and the caller reads (or skips) the elements next.
type mpValue struct {
	typ  mpType
	b    bool
	i    int64
	u    uint64
	f    float64
	data []byte
	n    int
}

// uint returns a non-negative integer value.
func (v mpValue) uint() (uint64, bool) {
	switch v.typ {
	case mpUint:
		return v.u, true
	case mpInt:
		if v.i >= 0 {
			return uint64(v.i), true
		}
	case mpFloat:
		if v.f >= 0 && v.f < 1<<64 && v.f == math.Trunc(v.f) {
			return uint64(v.f), true
		}
	}
	return 0, false
}

// str returns the value of a str, or of a bin (libmamba accepts both).
func (v mpValue) str() (string, bool) {
	if v.typ == mpStr || v.typ == mpBin {
		return string(v.data), true
	}
	return "", false
}

type mpReader struct {
	b   []byte
	off int
}

func (r *mpReader) remaining() int { return len(r.b) - r.off }

func (r *mpReader) take(n int) ([]byte, error) {
	if n < 0 || n > r.remaining() {
		return nil, errMsgpackTruncated
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p, nil
}

func (r *mpReader) uintN(size int) (uint64, error) {
	p, err := r.take(size)
	if err != nil {
		return 0, err
	}
	switch size {
	case 1:
		return uint64(p[0]), nil
	case 2:
		return uint64(binary.BigEndian.Uint16(p)), nil
	case 4:
		return uint64(binary.BigEndian.Uint32(p)), nil
	default:
		return binary.BigEndian.Uint64(p), nil
	}
}

// length reads a size-byte length prefix.
func (r *mpReader) length(size int) (int, error) {
	n, err := r.uintN(size)
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt32 {
		return 0, errMsgpackTruncated
	}
	return int(n), nil
}

// container checks that n elements (each at least one byte) can fit.
func (r *mpReader) container(t mpType, n int) (mpValue, error) {
	need := n
	if t == mpMap {
		need = 2 * n
	}
	if need > r.remaining() {
		return mpValue{}, errMsgpackTruncated
	}
	return mpValue{typ: t, n: n}, nil
}

func (r *mpReader) payload(t mpType, n int) (mpValue, error) {
	p, err := r.take(n)
	if err != nil {
		return mpValue{}, err
	}
	return mpValue{typ: t, data: p}, nil
}

func (r *mpReader) ext(n int) (mpValue, error) {
	if _, err := r.take(1); err != nil { // type byte
		return mpValue{}, err
	}
	return r.payload(mpExt, n)
}

// next decodes one header.
func (r *mpReader) next() (mpValue, error) {
	p, err := r.take(1)
	if err != nil {
		return mpValue{}, err
	}
	c := p[0]
	switch {
	case c <= 0x7f:
		return mpValue{typ: mpInt, i: int64(c)}, nil
	case c >= 0xe0:
		return mpValue{typ: mpInt, i: int64(int8(c))}, nil
	case c >= 0x80 && c <= 0x8f:
		return r.container(mpMap, int(c&0x0f))
	case c >= 0x90 && c <= 0x9f:
		return r.container(mpArray, int(c&0x0f))
	case c >= 0xa0 && c <= 0xbf:
		return r.payload(mpStr, int(c&0x1f))
	}
	switch c {
	case 0xc0:
		return mpValue{typ: mpNil}, nil
	case 0xc2, 0xc3:
		return mpValue{typ: mpBool, b: c == 0xc3}, nil
	case 0xc4, 0xc5, 0xc6:
		n, err := r.length(1 << (c - 0xc4))
		if err != nil {
			return mpValue{}, err
		}
		return r.payload(mpBin, n)
	case 0xc7, 0xc8, 0xc9:
		n, err := r.length(1 << (c - 0xc7))
		if err != nil {
			return mpValue{}, err
		}
		return r.ext(n)
	case 0xca:
		u, err := r.uintN(4)
		if err != nil {
			return mpValue{}, err
		}
		return mpValue{typ: mpFloat, f: float64(math.Float32frombits(uint32(u)))}, nil
	case 0xcb:
		u, err := r.uintN(8)
		if err != nil {
			return mpValue{}, err
		}
		return mpValue{typ: mpFloat, f: math.Float64frombits(u)}, nil
	case 0xcc, 0xcd, 0xce, 0xcf:
		u, err := r.uintN(1 << (c - 0xcc))
		if err != nil {
			return mpValue{}, err
		}
		return mpValue{typ: mpUint, u: u}, nil
	case 0xd0, 0xd1, 0xd2, 0xd3:
		size := 1 << (c - 0xd0)
		u, err := r.uintN(size)
		if err != nil {
			return mpValue{}, err
		}
		var i int64
		switch size {
		case 1:
			i = int64(int8(u))
		case 2:
			i = int64(int16(u))
		case 4:
			i = int64(int32(u))
		default:
			i = int64(u)
		}
		return mpValue{typ: mpInt, i: i}, nil
	case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8:
		return r.ext(1 << (c - 0xd4))
	case 0xd9, 0xda, 0xdb:
		n, err := r.length(1 << (c - 0xd9))
		if err != nil {
			return mpValue{}, err
		}
		return r.payload(mpStr, n)
	case 0xdc, 0xdd:
		n, err := r.length(2 << (c - 0xdc))
		if err != nil {
			return mpValue{}, err
		}
		return r.container(mpArray, n)
	case 0xde, 0xdf:
		n, err := r.length(2 << (c - 0xde))
		if err != nil {
			return mpValue{}, err
		}
		return r.container(mpMap, n)
	}
	return mpValue{}, fmt.Errorf("msgpack: invalid type byte 0x%02x at offset %d", c, r.off-1)
}

// skipRest skips the elements of a container header already read.
func (r *mpReader) skipRest(v mpValue) error {
	var pending int
	switch v.typ {
	case mpArray:
		pending = v.n
	case mpMap:
		pending = 2 * v.n
	}
	return r.skipN(pending)
}

// skip skips one complete value.
func (r *mpReader) skip() error { return r.skipN(1) }

// skipN skips n complete values without recursion.
func (r *mpReader) skipN(pending int) error {
	for pending > 0 {
		v, err := r.next()
		if err != nil {
			return err
		}
		pending--
		switch v.typ {
		case mpArray:
			pending += v.n
		case mpMap:
			pending += 2 * v.n
		}
		if pending > r.remaining() {
			return errMsgpackTruncated
		}
	}
	return nil
}

// stringList reads an array of strings; a nil value is an empty list. An
// array of more than maxListEntries elements, or with an element that is
// not a string or is longer than maxFieldBytes, is a bad field (consumed
// whole). Nothing is preallocated from the untrusted element count.
func (r *mpReader) stringList() ([]string, error) {
	v, err := r.next()
	if err != nil {
		return nil, err
	}
	if v.typ == mpNil {
		return nil, nil
	}
	if v.typ != mpArray {
		if err := r.skipRest(v); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: expected array, got %v", errBadField, v.typ)
	}
	if v.n > maxListEntries {
		if err := r.skipRest(v); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: more than %d elements", errBadField, maxListEntries)
	}
	var out []string
	for i := 0; i < v.n; i++ {
		e, err := r.next()
		if err != nil {
			return nil, err
		}
		if (e.typ != mpStr && e.typ != mpBin) || len(e.data) > maxFieldBytes {
			if err := r.skipRest(e); err != nil {
				return nil, err
			}
			if err := r.skipN(v.n - i - 1); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: array element %d: not a string of at most %d bytes", errBadField, i, maxFieldBytes)
		}
		out = append(out, string(e.data))
	}
	return out, nil
}
