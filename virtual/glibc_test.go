package virtual

import (
	"bytes"
	"compress/zlib"
	"debug/elf"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var amd64ELF = elfTarget{elf.EM_X86_64, elf.ELFCLASS64, binary.LittleEndian}

// The fixtures are synthetic shared objects built by testdata/elf/gen.sh.
func TestGlibcFromELF(t *testing.T) {
	cases := []struct {
		file    string
		want    string
		wantErr bool
	}{
		{"libc-banner.so", "2.29", false},      // banner newer than GLIBC_2.28 wins
		{"libc-nobanner.so", "2.28", false},    // highest version definition
		{"libc-oldbanner.so", "2.28", false},   // banner older than the definitions loses
		{"libc-otherbanner.so", "2.28", false}, // banner with another major is ignored
		{"wrong-soname.so", "", true},
		{"musl.so", "", true},
		{"noversions.so", "", true},
	}
	for _, c := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", "elf", c.file))
		if err != nil {
			t.Fatal(err)
		}
		got, err := glibcFromELF(bytes.NewReader(data), amd64ELF)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("glibcFromELF(%s) = %q, %v; want %q, error %v", c.file, got, err, c.want, c.wantErr)
		}
		// The same file is rejected for another architecture.
		other := elfTarget{elf.EM_AARCH64, elf.ELFCLASS64, binary.LittleEndian}
		if _, err := glibcFromELF(bytes.NewReader(data), other); !errors.Is(err, errNotGlibc) {
			t.Errorf("glibcFromELF(%s) for aarch64: err = %v, want errNotGlibc", c.file, err)
		}
	}
	for _, junk := range [][]byte{nil, []byte("not an elf"), []byte("\x7fELF\x02\x01\x01")} {
		if _, err := glibcFromELF(bytes.NewReader(junk), amd64ELF); err == nil {
			t.Errorf("glibcFromELF(%q) = nil error", junk)
		}
	}
}

func TestMaxGlibcVersion(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"libc.so.6", "GLIBC_PRIVATE"}, ""},
		{[]string{"GLIBC_2.2.5", "GLIBC_2.17", "GLIBC_2.3.4", "GLIBC_2.9"}, "2.17"},
		{[]string{"GLIBC_2.35", "GLIBC_2.4", "GLIBC_2.34"}, "2.35"},
		{[]string{"GLIBC_2.0", "GLIBC_2.1.3"}, "2.1.3"},
		{[]string{"GLIBC_2", "GLIBC_2.x", "GLIBC_2.17a", "GLIBC_2.17"}, "2.17"},
		{[]string{"GLIBC_2.99999999999999999999", "GLIBC_2.5"}, "2.99999999999999999999"},
		{[]string{"GLIBC_2.010", "GLIBC_2.9"}, "2.010"},
	}
	for _, c := range cases {
		if got := maxGlibcVersion(c.names); got != c.want {
			t.Errorf("maxGlibcVersion(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}

func TestGlibcBannerVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x00GNU C Library (Ubuntu GLIBC 2.35-0ubuntu3.15) stable release version 2.35.\nCopyright\x00", "2.35"},
		{"GNU C Library (GNU libc) stable release version 2.38.", "2.38"},
		{"GNU C Library (GNU libc) development release version 2.39.9000.", "2.39.9000"},
		{"GNU C Library\x00 release version 2.35.", ""},
		{"release version 2.35", ""},
		{"GNU C Library (x) release version x", ""},
		{"GNU C Library GNU C Library (x) release version 2.40", "2.40"},
		{"", ""},
	}
	for _, c := range cases {
		if got := glibcBannerVersion([]byte(c.in)); got != c.want {
			t.Errorf("glibcBannerVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/libc.rs
// (test_parse_glibc_ldd_version), plus getconf and musl output.
func TestParseLibcVersionOutput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ldd (Ubuntu GLIBC 2.35-0ubuntu3.1) 2.35", "2.35"},
		{"ldd (Gentoo 2.39-r9 (patchset 9)) 2.39", "2.39"},
		{"ldd (GNU libc) 2.31", "2.31"},
		{"ldd (Solus) 2.39", "2.39"},
		// goomba additions
		{"glibc 2.35\n", "2.35"},
		{"glibc 2.17\r\n", "2.17"},
		{"musl libc (x86_64)\nVersion 1.2.4\n", ""},
		{"glibc", ""},
		{"glibc 2.35 extra", ""},
		{"", ""},
		{"ldd (GNU libc) 2.31\nCopyright (C) 2020 Free Software Foundation, Inc.", "2.31"},
	}
	for _, c := range cases {
		if got := parseLibcVersionOutput(c.in); got != c.want {
			t.Errorf("parseLibcVersionOutput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCompareNumeric(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.17", "2.17", 0},
		{"2.17", "2.9", 1},
		{"2.9", "2.17", -1},
		{"2.17", "2.17.0", 0},
		{"2.17.1", "2.17", 1},
		{"3.0", "2.99", 1},
		{"2.010", "2.10", 0},
	}
	for _, c := range cases {
		if got := compareNumeric(c.a, c.b); got != c.want {
			t.Errorf("compareNumeric(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// sectionIndex returns the index of the named section of an ELF file.
func sectionIndex(t testing.TB, data []byte, name string) int {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range f.Sections {
		if s.Name == name {
			return i
		}
	}
	t.Fatalf("no section %s", name)
	return -1
}

// compressSection returns a copy of data (a little-endian ELF64 file) whose
// section idx is replaced by compressed data appended to the file: content
// (length n) deflated with zlib behind a header claiming claimed bytes,
// either as an SHF_COMPRESSED gABI section (with SHF_ALLOC dropped, as
// debug/elf insists) or in the legacy "ZLIB" .zdebug layout.
func compressSection(t testing.TB, data []byte, idx int, content io.Reader, n int64, claimed uint64, gabi bool) []byte {
	t.Helper()
	var payload bytes.Buffer
	if gabi {
		// Elf64_Chdr: type, reserved, size, addralign.
		var ch [24]byte
		binary.LittleEndian.PutUint32(ch[0:], uint32(elf.COMPRESS_ZLIB))
		binary.LittleEndian.PutUint64(ch[8:], claimed)
		binary.LittleEndian.PutUint64(ch[16:], 1)
		payload.Write(ch[:])
	} else {
		payload.WriteString("ZLIB")
		var sz [8]byte
		binary.BigEndian.PutUint64(sz[:], claimed)
		payload.Write(sz[:])
	}
	zw := zlib.NewWriter(&payload)
	if _, err := io.CopyN(zw, content, n); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	shoff := binary.LittleEndian.Uint64(data[0x28:])
	off := uint64(len(data))
	out := append(append([]byte(nil), data...), payload.Bytes()...)
	hdr := out[shoff+uint64(idx)*64:]
	if gabi {
		flags := binary.LittleEndian.Uint64(hdr[8:])
		binary.LittleEndian.PutUint64(hdr[8:], (flags|uint64(elf.SHF_COMPRESSED))&^uint64(elf.SHF_ALLOC))
	}
	binary.LittleEndian.PutUint64(hdr[24:], off)
	binary.LittleEndian.PutUint64(hdr[32:], uint64(payload.Len()))
	return out
}

func nobanner(t testing.TB) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "elf", "libc-nobanner.so"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// craftCompressed returns libc-nobanner.so with its .dynstr replaced by a
// zlib stream whose header claims claimed bytes and which really inflates
// to inflated bytes.
func craftCompressed(t testing.TB, claimed uint64, inflated int64, gabi bool) []byte {
	t.Helper()
	data := nobanner(t)
	return compressSection(t, data, sectionIndex(t, data, ".dynstr"), zeroReader{}, inflated, claimed, gabi)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestCompressedSectionsRejected feeds the ELF reader a .dynstr that
// debug/elf would inflate to 16 MiB (claiming 768 MiB) and checks that it is
// refused before anything is inflated.
func TestCompressedSectionsRejected(t *testing.T) {
	for _, setFlag := range []bool{true, false} {
		data := craftCompressed(t, 768<<20, 16<<20, setFlag)
		if len(data) > 1<<20 {
			t.Fatalf("crafted file is %d bytes", len(data))
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := glibcFromELF(bytes.NewReader(data), amd64ELF)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, errNotGlibc) || !strings.Contains(err.Error(), "compressed") {
			t.Errorf("SHF_COMPRESSED=%v: err = %v, want a compressed-section rejection", setFlag, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
			t.Errorf("SHF_COMPRESSED=%v: allocated %d bytes", setFlag, grew)
		}
	}
	// The untouched fixture still parses.
	if v, err := glibcFromELF(bytes.NewReader(nobanner(t)), amd64ELF); err != nil || v != "2.28" {
		t.Errorf("fixture = %q, %v", v, err)
	}
}

// TestCompressionOnlyMattersWhereRead checks that compression is refused on
// exactly the sections the reader makes debug/elf read: a libc with a
// compressed section nobody reads (an unstripped libc's .debug_info, here
// .comment) still parses, even when that section is a bomb.
func TestCompressionOnlyMattersWhereRead(t *testing.T) {
	data := nobanner(t)
	comment := sectionIndex(t, data, ".comment")
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := f.Sections[comment].Data()
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string][]byte{
		"compressed .comment":             compressSection(t, data, comment, bytes.NewReader(plain), int64(len(plain)), uint64(len(plain)), true),
		"zlib-prefixed .comment":          compressSection(t, data, comment, bytes.NewReader(plain), int64(len(plain)), uint64(len(plain)), false),
		"compressed .comment bomb":        compressSection(t, data, comment, zeroReader{}, 16<<20, 768<<20, true),
		"compressed .gnu.hash (not read)": compressSection(t, data, sectionIndex(t, data, ".gnu.hash"), zeroReader{}, 1<<10, 1<<10, true),
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		v, err := glibcFromELF(bytes.NewReader(in), amd64ELF)
		runtime.ReadMemStats(&after)
		if err != nil || v != "2.28" {
			t.Errorf("%s: glibcFromELF = %q, %v; want 2.28", name, v, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
			t.Errorf("%s: allocated %d bytes", name, grew)
		}
	}

	refused := map[string][]byte{
		"compressed .shstrtab": compressSection(t, data, sectionIndex(t, data, ".shstrtab"), zeroReader{}, 1<<20, 768<<20, true),
		"compressed .dynamic":  compressSection(t, data, sectionIndex(t, data, ".dynamic"), zeroReader{}, 1<<20, 768<<20, true),
		"compressed verdef":    compressSection(t, data, sectionIndex(t, data, ".gnu.version_d"), zeroReader{}, 1<<20, 768<<20, true),
	}
	// A .dynamic whose sh_link points at a compressed non-string section:
	// debug/elf would read it as the string table.
	redirect := compressSection(t, data, comment, zeroReader{}, 1<<20, 768<<20, true)
	shoff := binary.LittleEndian.Uint64(redirect[0x28:])
	dyn := sectionIndex(t, data, ".dynamic")
	binary.LittleEndian.PutUint32(redirect[shoff+uint64(dyn)*64+40:], uint32(comment))
	refused["dynamic linked to compressed section"] = redirect
	for name, in := range refused {
		if _, err := glibcFromELF(bytes.NewReader(in), amd64ELF); !errors.Is(err, errNotGlibc) || !strings.Contains(err.Error(), "compressed") {
			t.Errorf("%s: err = %v, want a compressed-section rejection", name, err)
		}
	}
}

func TestCheckSectionHeaders(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "elf", "libc-nobanner.so"))
	if err != nil {
		t.Fatal(err)
	}
	patch := func(off int, b ...byte) []byte {
		out := append([]byte(nil), data...)
		copy(out[off:], b)
		return out
	}
	cases := map[string][]byte{
		"empty":          nil,
		"bad magic":      patch(0, 'X'),
		"bad class":      patch(elf.EI_CLASS, 9),
		"bad encoding":   patch(elf.EI_DATA, 9),
		"no sections":    patch(0x28, 0, 0, 0, 0, 0, 0, 0, 0),
		"section count":  patch(0x3c, 0, 0),
		"xindex":         patch(0x3e, 0xff, 0xff),
		"entry size":     patch(0x3a, 63, 0),
		"headers beyond": patch(0x28, 0xff, 0xff, 0xff, 0, 0, 0, 0, 0),
	}
	for name, in := range cases {
		if err := checkSectionHeaders(bytes.NewReader(in)); err == nil {
			t.Errorf("%s: checkSectionHeaders = nil", name)
		}
	}
	if err := checkSectionHeaders(bytes.NewReader(data)); err != nil {
		t.Errorf("fixture: %v", err)
	}
}
