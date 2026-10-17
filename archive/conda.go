// .conda handling ported from libmamba src/core/package_handling.cpp
// (extract_conda) and rattler crates/rattler_package_streaming src/read.rs
// (BSD-3-Clause, see NOTICE).

package archive

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// maxZstdWindow matches the default window limit of the reference zstd
// library (window log 27), which rattler and libmamba decode with.
const maxZstdWindow = 1 << 27

// maxMetadataSize bounds metadata.json; the real file is 31 bytes.
const maxMetadataSize = 1 << 16

// maxCondaMembers bounds the zip members of a .conda; real ones have 3.
const maxCondaMembers = 64

func newZstdReader(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(maxZstdWindow),
		zstd.WithDecoderMaxMemory(maxZstdWindow),
	)
}

// condaMember classifies a .conda zip member by its base name, as libmamba
// and rattler do (re-zipped packages may nest members in a directory):
// "metadata", "pkg", "info", or "" for members that are ignored. Members are
// never written to disk under their own names.
func condaMember(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "metadata.json" {
		return "metadata"
	}
	if !strings.HasSuffix(name, ".tar.zst") {
		return ""
	}
	switch {
	case strings.HasPrefix(name, "pkg-"):
		return "pkg"
	case strings.HasPrefix(name, "info-"):
		return "info"
	}
	return ""
}

// conda extracts a .conda archive: a zip holding metadata.json,
// info-<dist>.tar.zst (most of info/) and pkg-<dist>.tar.zst (everything
// else, including info/licenses). Both tars are extracted in zip order, as
// libmamba and rattler do; exactly one of each is required, and nothing
// may precede the first zip entry or follow the end of the central
// directory.
func (x *extractor) conda(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("%w: zip: %w", ErrFormat, err)
	}
	if len(zr.File) > maxCondaMembers {
		return fmt.Errorf("%w: %d zip members, at most %d allowed", ErrFormat, len(zr.File), maxCondaMembers)
	}
	if err := checkZipBounds(r, size, zr); err != nil {
		return err
	}
	var meta, pkg, info *zip.File
	var tars []*zip.File
	seen := make(map[string]bool, len(zr.File))
	for _, zf := range zr.File {
		if err := x.countEntry(); err != nil {
			return err
		}
		if seen[zf.Name] {
			return fmt.Errorf("%w: duplicate zip member %s", ErrFormat, quote(zf.Name))
		}
		seen[zf.Name] = true
		switch condaMember(zf.Name) {
		case "metadata":
			if meta != nil {
				return fmt.Errorf("%w: zip members %s and %s are both metadata.json", ErrFormat, quote(meta.Name), quote(zf.Name))
			}
			meta = zf
		case "pkg":
			if pkg != nil {
				return fmt.Errorf("%w: zip members %s and %s are both pkg tars", ErrFormat, quote(pkg.Name), quote(zf.Name))
			}
			pkg = zf
			tars = append(tars, zf)
		case "info":
			if info != nil {
				return fmt.Errorf("%w: zip members %s and %s are both info tars", ErrFormat, quote(info.Name), quote(zf.Name))
			}
			info = zf
			tars = append(tars, zf)
		}
	}
	if pkg == nil || info == nil {
		return fmt.Errorf("%w: missing pkg-*.tar.zst or info-*.tar.zst member", ErrFormat)
	}
	if meta != nil {
		if err := x.checkMetadata(meta); err != nil {
			return fmt.Errorf("member %s: %w", quote(meta.Name), err)
		}
	}
	for _, zf := range tars {
		if err := x.member(zf); err != nil {
			return err
		}
	}
	return nil
}

// checkZipBounds refuses a .conda whose zip has bytes before its first
// local file header or anywhere around its central directory: the first
// central-directory entry's local header must sit at offset 0 under the
// same name, the central directory must end exactly where the zip64
// end-of-central-directory record (or, without zip64, the
// end-of-central-directory record) begins, and that record must end the
// file. archive/zip tolerates prepended and appended data; a package
// archive has neither.
func checkZipBounds(r io.ReaderAt, size int64, zr *zip.Reader) error {
	if len(zr.File) == 0 {
		return fmt.Errorf("%w: empty zip", ErrFormat)
	}
	var lh [30]byte
	if _, err := r.ReadAt(lh[:], 0); err != nil || binary.LittleEndian.Uint32(lh[:4]) != 0x04034b50 {
		return fmt.Errorf("%w: data before the first zip entry", ErrFormat)
	}
	n := int64(binary.LittleEndian.Uint16(lh[26:]))
	e := int64(binary.LittleEndian.Uint16(lh[28:]))
	first := zr.File[0]
	name := make([]byte, n)
	if _, err := r.ReadAt(name, int64(len(lh))); err != nil || string(name) != first.Name {
		return fmt.Errorf("%w: data before the first zip entry", ErrFormat)
	}
	if off, err := first.DataOffset(); err != nil || off != int64(len(lh))+n+e {
		return fmt.Errorf("%w: data before the first zip entry", ErrFormat)
	}

	// Find the end-of-central-directory record the way archive/zip does
	// (the last signature whose comment fits) and require it to end the
	// file exactly.
	const eocdLen = 22
	tail := min(size, eocdLen+0xffff)
	b := make([]byte, tail)
	if _, err := r.ReadAt(b, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: zip: %w", ErrFormat, err)
	}
	pos := -1
	for i := len(b) - eocdLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(b[i:]) != 0x06054b50 {
			continue
		}
		if i+eocdLen+int(binary.LittleEndian.Uint16(b[i+20:])) > len(b) {
			continue
		}
		pos = i
		break
	}
	if pos < 0 {
		return fmt.Errorf("%w: zip: no end of central directory", ErrFormat)
	}
	if pos+eocdLen+int(binary.LittleEndian.Uint16(b[pos+20:])) != len(b) {
		return fmt.Errorf("%w: data after the end of the zip archive", ErrFormat)
	}
	eocd := size - tail + int64(pos)
	cdSize := int64(binary.LittleEndian.Uint32(b[pos+12:]))
	cdOff := int64(binary.LittleEndian.Uint32(b[pos+16:]))
	cdEnd := eocd
	// A zip64 end-of-central-directory locator sits right before the
	// record and points at the zip64 record, which then precedes it.
	const locLen = 20
	if eocd >= locLen {
		var loc [locLen]byte
		if _, err := r.ReadAt(loc[:], eocd-locLen); err == nil && binary.LittleEndian.Uint32(loc[:]) == 0x07064b50 {
			z64 := int64(binary.LittleEndian.Uint64(loc[8:]))
			var rec [56]byte
			if z64 < 0 || z64 > eocd-locLen-int64(len(rec)) {
				return fmt.Errorf("%w: zip64 record out of place", ErrFormat)
			}
			if _, err := r.ReadAt(rec[:], z64); err != nil || binary.LittleEndian.Uint32(rec[:]) != 0x06064b50 {
				return fmt.Errorf("%w: zip64 record missing", ErrFormat)
			}
			if recEnd := z64 + 12 + int64(binary.LittleEndian.Uint64(rec[4:])); recEnd != eocd-locLen {
				return fmt.Errorf("%w: data between the zip64 record and its locator", ErrFormat)
			}
			cdSize = int64(binary.LittleEndian.Uint64(rec[40:]))
			cdOff = int64(binary.LittleEndian.Uint64(rec[48:]))
			cdEnd = z64
		}
	}
	if cdOff < 0 || cdSize < 0 || cdOff+cdSize != cdEnd {
		return fmt.Errorf("%w: data around the zip central directory", ErrFormat)
	}
	return nil
}

func openMember(zf *zip.File) (io.ReadCloser, error) {
	if zf.Flags&0x1 != 0 {
		return nil, fmt.Errorf("%w: encrypted zip member", ErrFormat)
	}
	if zf.Method != zip.Store && zf.Method != zip.Deflate {
		return nil, fmt.Errorf("%w: zip compression method %d", ErrFormat, zf.Method)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: zip: %w", ErrFormat, err)
	}
	return rc, nil
}

// member extracts one tar.zst zip member. Reading it to its end makes
// archive/zip verify the member's CRC-32 and size.
func (x *extractor) member(zf *zip.File) error {
	rc, err := openMember(zf)
	if err != nil {
		return fmt.Errorf("member %s: %w", quote(zf.Name), err)
	}
	defer rc.Close()
	if err := x.stream(rc, TarZst); err != nil {
		return fmt.Errorf("member %s: %w", quote(zf.Name), err)
	}
	return nil
}

// checkMetadata validates metadata.json. libmamba and rattler accept a
// .conda without it, and libmamba accepts an empty one, so goomba does too;
// otherwise it must be exactly one JSON object, without duplicate keys,
// whose conda_pkg_format_version is the integer 2. Other keys are allowed.
func (x *extractor) checkMetadata(zf *zip.File) error {
	rc, err := openMember(zf)
	if err != nil {
		return err
	}
	defer rc.Close()
	in := &meter{ctx: x.ctx, r: rc, n: &x.inBytes, max: x.lim.MaxTotalSize, what: "compressed input"}
	data, err := io.ReadAll(io.LimitReader(in, maxMetadataSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxMetadataSize {
		return fmt.Errorf("%w: over %d bytes", ErrFormat, maxMetadataSize)
	}
	if len(data) == 0 {
		return nil
	}
	if err := parseMetadata(data); err != nil {
		return fmt.Errorf("%w: %w", ErrFormat, err)
	}
	return nil
}

func parseMetadata(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	version := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		k, _ := tok.(string)
		if seen[k] {
			return fmt.Errorf("duplicate key %s", quote(k))
		}
		seen[k] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return err
		}
		if k == "conda_pkg_format_version" {
			if string(v) != "2" {
				return fmt.Errorf("unsupported conda_pkg_format_version %s", quote(string(v)))
			}
			version = true
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return errors.New("unterminated JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON object")
	}
	if !version {
		return errors.New("no conda_pkg_format_version")
	}
	return nil
}
