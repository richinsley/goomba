package archive

import (
	"bytes"
	"compress/bzip2"
	"io"
	"sort"
	"testing"
)

// bz2Compress is a minimal bzip2 encoder for building test archives; the
// standard library only decodes bzip2. It emits valid level-9 streams: RLE1,
// a cyclic BWT by prefix doubling, MTF with RUNA/RUNB zero runs, and one
// fixed, complete Huffman table used for every group. The output decodes
// with compress/bzip2 and the reference bzip2 tool, but compresses poorly
// except for long runs.
func bz2Compress(data []byte) []byte {
	w := &bitWriter{}
	w.write([]byte("BZh9"))
	var combined uint32
	// RLE1 grows input by at most 5/4; stay well below the 900k block.
	const maxRaw = 600_000
	for len(data) > 0 {
		n := min(len(data), maxRaw)
		block := data[:n]
		data = data[n:]
		crc := bzCRC(block)
		combined = (combined<<1 | combined>>31) ^ crc
		w.block(block, crc)
	}
	w.bits(0x177245, 24)
	w.bits(0x385090, 24)
	w.bits(combined, 32)
	w.flush()
	return w.out
}

type bitWriter struct {
	out []byte
	acc uint64
	n   uint
}

func (w *bitWriter) bits(v uint32, n uint) {
	w.acc = w.acc<<n | uint64(v)&(1<<n-1)
	w.n += n
	for w.n >= 8 {
		w.out = append(w.out, byte(w.acc>>(w.n-8)))
		w.n -= 8
	}
}

func (w *bitWriter) write(b []byte) {
	for _, c := range b {
		w.bits(uint32(c), 8)
	}
}

func (w *bitWriter) flush() {
	if w.n > 0 {
		w.out = append(w.out, byte(w.acc<<(8-w.n)))
		w.n = 0
	}
}

func bzCRC(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, c := range b {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return ^crc
}

// rle1 applies bzip2's initial run-length encoding: runs of 4 to 255 equal
// bytes become the byte four times plus a count byte.
func rle1(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/4+1)
	for i := 0; i < len(b); {
		c := b[i]
		j := i
		for j < len(b) && b[j] == c && j-i < 255 {
			j++
		}
		if run := j - i; run >= 4 {
			out = append(out, c, c, c, c, byte(run-4))
		} else {
			out = append(out, b[i:j]...)
		}
		i = j
	}
	return out
}

// bwt returns the last column of the sorted cyclic rotations of s and the
// row holding s itself.
func bwt(s []byte) ([]byte, int) {
	n := len(s)
	sa := make([]int, n)
	rank := make([]int, n)
	tmp := make([]int, n)
	for i := range sa {
		sa[i] = i
		rank[i] = int(s[i])
	}
	for k := 1; ; k <<= 1 {
		less := func(a, b int) bool {
			if rank[a] != rank[b] {
				return rank[a] < rank[b]
			}
			return rank[(a+k)%n] < rank[(b+k)%n]
		}
		sort.Slice(sa, func(i, j int) bool { return less(sa[i], sa[j]) })
		tmp[sa[0]] = 0
		for i := 1; i < n; i++ {
			tmp[sa[i]] = tmp[sa[i-1]]
			if less(sa[i-1], sa[i]) {
				tmp[sa[i]]++
			}
		}
		copy(rank, tmp)
		if rank[sa[n-1]] == n-1 || k >= n {
			break
		}
	}
	last := make([]byte, n)
	orig := 0
	for i, p := range sa {
		last[i] = s[(p+n-1)%n]
		if p == 0 {
			orig = i
		}
	}
	return last, orig
}

func (w *bitWriter) block(raw []byte, crc uint32) {
	s := rle1(raw)
	last, orig := bwt(s)

	var inUse [256]bool
	for _, c := range last {
		inUse[c] = true
	}
	var mtf []byte
	for c := range 256 {
		if inUse[c] {
			mtf = append(mtf, byte(c))
		}
	}
	numInUse := len(mtf)
	var syms []int
	zeros := 0
	flushZeros := func() {
		for n := zeros; n > 0; {
			if n&1 == 1 {
				syms = append(syms, 0) // RUNA
				n = (n - 1) / 2
			} else {
				syms = append(syms, 1) // RUNB
				n = (n - 2) / 2
			}
		}
		zeros = 0
	}
	for _, c := range last {
		idx := bytes.IndexByte(mtf, c)
		if idx == 0 {
			zeros++
			continue
		}
		flushZeros()
		copy(mtf[1:idx+1], mtf[:idx])
		mtf[0] = c
		syms = append(syms, idx+1)
	}
	flushZeros()
	alphaSize := numInUse + 2
	syms = append(syms, alphaSize-1) // EOB

	// A complete code: k symbols of length L-1, the rest of length L.
	L := uint(1)
	for 1<<L < alphaSize {
		L++
	}
	k := 1<<L - alphaSize
	lens := make([]uint, alphaSize)
	for i := range lens {
		lens[i] = L
		if i < k {
			lens[i] = L - 1
		}
	}
	codes := make([]uint32, alphaSize)
	vec := uint32(0)
	for n := uint(1); n <= L; n++ {
		for i := range alphaSize {
			if lens[i] == n {
				codes[i] = vec
				vec++
			}
		}
		vec <<= 1
	}

	w.bits(0x314159, 24)
	w.bits(0x265359, 24)
	w.bits(crc, 32)
	w.bits(0, 1) // not randomised
	w.bits(uint32(orig), 24)
	var used16 uint32
	for i := range 16 {
		for j := range 16 {
			if inUse[i*16+j] {
				used16 |= 1 << (15 - i)
				break
			}
		}
	}
	w.bits(used16, 16)
	for i := range 16 {
		if used16&(1<<(15-i)) == 0 {
			continue
		}
		var bitsInUse uint32
		for j := range 16 {
			if inUse[i*16+j] {
				bitsInUse |= 1 << (15 - j)
			}
		}
		w.bits(bitsInUse, 16)
	}
	const nGroups = 2
	nSelectors := (len(syms) + 49) / 50
	w.bits(nGroups, 3)
	w.bits(uint32(nSelectors), 15)
	for range nSelectors {
		w.bits(0, 1) // selector 0, MTF index 0
	}
	for range nGroups {
		cur := lens[0]
		w.bits(uint32(cur), 5)
		for i := range alphaSize {
			for cur < lens[i] {
				w.bits(2, 2)
				cur++
			}
			for cur > lens[i] {
				w.bits(3, 2)
				cur--
			}
			w.bits(0, 1)
		}
	}
	for _, s := range syms {
		w.bits(codes[s], lens[s])
	}
}

func TestBz2CompressRoundTrip(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"empty":    nil,
		"one":      []byte("x"),
		"text":     []byte("hello, hello, hello world; banana bandana"),
		"zeros":    make([]byte, 100_000),
		"periodic": bytes.Repeat([]byte("abcab"), 50_000),
		"noise":    noise(700_000, 9), // two blocks
		"runs":     append(bytes.Repeat([]byte{7}, 1000), bytes.Repeat([]byte{8}, 3)...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(bz2Compress(data))))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(data))
			}
		})
	}
}
