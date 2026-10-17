// Ported from rattler crates/rattler_virtual_packages/src/libc.rs and libmamba
// src/core/virtual_packages.cpp (BSD-3-Clause, see NOTICE). Both ask the
// running glibc for its version through cgo or dlopen; goomba has no cgo, so
// it reads the version definitions of libc.so.6 with debug/elf instead.

package virtual

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// maxLibcSize bounds the libc.so.6 candidates we parse. glibc's libc.so.6 is
// about 2 MB; anything far larger is not a C library.
const maxLibcSize = 64 << 20

// maxRodataScan bounds how much of .rodata is searched for the glibc banner.
const maxRodataScan = 8 << 20

// elfTarget describes the ELF identity a GOARCH's libc must have.
type elfTarget struct {
	machine elf.Machine
	class   elf.Class
	order   binary.ByteOrder
}

// elfTargetFor returns the ELF identity of shared objects the given GOARCH
// can load, or false for architectures goomba does not detect glibc on.
func elfTargetFor(goarch string) (elfTarget, bool) {
	le, be := binary.ByteOrder(binary.LittleEndian), binary.ByteOrder(binary.BigEndian)
	switch goarch {
	case "amd64":
		return elfTarget{elf.EM_X86_64, elf.ELFCLASS64, le}, true
	case "386":
		return elfTarget{elf.EM_386, elf.ELFCLASS32, le}, true
	case "arm64":
		return elfTarget{elf.EM_AARCH64, elf.ELFCLASS64, le}, true
	case "arm":
		return elfTarget{elf.EM_ARM, elf.ELFCLASS32, le}, true
	case "ppc64le":
		return elfTarget{elf.EM_PPC64, elf.ELFCLASS64, le}, true
	case "ppc64":
		return elfTarget{elf.EM_PPC64, elf.ELFCLASS64, be}, true
	case "s390x":
		return elfTarget{elf.EM_S390, elf.ELFCLASS64, be}, true
	case "riscv64":
		return elfTarget{elf.EM_RISCV, elf.ELFCLASS64, le}, true
	case "loong64":
		return elfTarget{elf.EM_LOONGARCH, elf.ELFCLASS64, le}, true
	}
	return elfTarget{}, false
}

// errNotGlibc reports that a file is readable but is not a usable glibc.
var errNotGlibc = errors.New("not a glibc libc.so.6")

// glibcFromELF returns the glibc version of the shared object in r: the
// highest GLIBC_x.y[.z] version definition, raised to the release in the
// "release version x.y" banner in .rodata when that banner is present and
// agrees on the major version (a glibc release need not add a symbol
// version). The object must be a shared library whose SONAME is libc.so.6
// and whose ELF identity matches want. Malformed input yields an error,
// never a panic.
func glibcFromELF(r io.ReaderAt, want elfTarget) (version string, err error) {
	defer func() {
		// debug/elf is not hardened against every malformed input; a panic
		// while reading a system file must not take the process down.
		if p := recover(); p != nil {
			version, err = "", fmt.Errorf("malformed ELF: %v", p)
		}
	}()
	if err := checkSectionHeaders(r); err != nil {
		return "", err
	}
	f, err := elf.NewFile(r)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if f.Type != elf.ET_DYN {
		return "", fmt.Errorf("%w: ELF type %v", errNotGlibc, f.Type)
	}
	if f.Machine != want.machine || f.Class != want.class || f.ByteOrder != want.order {
		return "", fmt.Errorf("%w: ELF %v %v is for another architecture", errNotGlibc, f.Class, f.Machine)
	}
	sonames, err := f.DynString(elf.DT_SONAME)
	if err != nil {
		return "", err
	}
	if len(sonames) != 1 || sonames[0] != "libc.so.6" {
		return "", fmt.Errorf("%w: SONAME %q", errNotGlibc, sonames)
	}
	names, err := versionDefinitions(f)
	if err != nil {
		return "", err
	}
	best := maxGlibcVersion(names)
	if best == "" {
		return "", fmt.Errorf("%w: no GLIBC_x.y version definitions", errNotGlibc)
	}
	if sec := f.Section(".rodata"); sec != nil && sec.Type != elf.SHT_NOBITS {
		data, err := io.ReadAll(io.LimitReader(sec.Open(), maxRodataScan))
		if err == nil {
			if banner := glibcBannerVersion(data); banner != "" &&
				majorOf(banner) == majorOf(best) && compareNumeric(banner, best) > 0 {
				best = banner
			}
		}
	}
	return best, nil
}

// maxSections bounds the section count of a libc.so.6 candidate; glibc has
// about 70 sections.
const maxSections = 4096

// checkSectionHeaders reads the ELF section header table itself, before
// debug/elf sees the file, and rejects compression on every section whose
// data glibcFromELF makes debug/elf read: the section-name table (NewFile
// reads it at once), every SHT_DYNAMIC and SHT_GNU_VERDEF section, and the
// sections their sh_link names (debug/elf reads a link target as a string
// table whatever its type). Such a section must not be SHF_COMPRESSED or
// start with "ZLIB" (the legacy .zdebug layout): debug/elf inflates
// compressed data to whatever size its header claims, so a few KB of
// crafted input could make it allocate gigabytes. Other sections, such as
// compressed .debug_* sections in an unstripped libc, are never read and
// are allowed; .rodata is only ever read through an 8 MiB limit. Extended
// section numbering (more than 0xff00 sections) is rejected.
func checkSectionHeaders(r io.ReaderAt) error {
	var ident [elf.EI_NIDENT]byte
	if _, err := r.ReadAt(ident[:], 0); err != nil {
		return fmt.Errorf("read ELF header: %w", err)
	}
	if string(ident[:4]) != elf.ELFMAG {
		return fmt.Errorf("%w: bad ELF magic", errNotGlibc)
	}
	var bo binary.ByteOrder
	switch elf.Data(ident[elf.EI_DATA]) {
	case elf.ELFDATA2LSB:
		bo = binary.LittleEndian
	case elf.ELFDATA2MSB:
		bo = binary.BigEndian
	default:
		return fmt.Errorf("%w: unknown ELF data encoding", errNotGlibc)
	}
	var wide bool
	var entsize int
	switch elf.Class(ident[elf.EI_CLASS]) {
	case elf.ELFCLASS64:
		wide, entsize = true, 64
	case elf.ELFCLASS32:
		wide, entsize = false, 40
	default:
		return fmt.Errorf("%w: unknown ELF class", errNotGlibc)
	}
	var hdr [64]byte
	var shoff uint64
	var shentsize, shnum, shstrndx uint16
	if wide {
		if _, err := r.ReadAt(hdr[:64], 0); err != nil {
			return fmt.Errorf("read ELF header: %w", err)
		}
		shoff = bo.Uint64(hdr[0x28:])
		shentsize, shnum, shstrndx = bo.Uint16(hdr[0x3a:]), bo.Uint16(hdr[0x3c:]), bo.Uint16(hdr[0x3e:])
	} else {
		if _, err := r.ReadAt(hdr[:52], 0); err != nil {
			return fmt.Errorf("read ELF header: %w", err)
		}
		shoff = uint64(bo.Uint32(hdr[0x20:]))
		shentsize, shnum, shstrndx = bo.Uint16(hdr[0x2e:]), bo.Uint16(hdr[0x30:]), bo.Uint16(hdr[0x32:])
	}
	if shoff == 0 {
		return fmt.Errorf("%w: no section headers", errNotGlibc)
	}
	if shnum == 0 || shstrndx == uint16(elf.SHN_XINDEX) || shnum > maxSections {
		return fmt.Errorf("%w: unsupported section count", errNotGlibc)
	}
	if int(shentsize) != entsize {
		return fmt.Errorf("%w: unexpected section header size %d", errNotGlibc, shentsize)
	}

	type section struct {
		typ             elf.SectionType
		flags, off, len uint64
		link            uint32
	}
	secs := make([]section, shnum)
	buf := make([]byte, entsize)
	for i := range secs {
		if _, err := r.ReadAt(buf, int64(shoff+uint64(i)*uint64(entsize))); err != nil {
			return fmt.Errorf("read section header %d: %w", i, err)
		}
		s := &secs[i]
		s.typ = elf.SectionType(bo.Uint32(buf[4:]))
		if wide {
			s.flags, s.off, s.len, s.link = bo.Uint64(buf[8:]), bo.Uint64(buf[24:]), bo.Uint64(buf[32:]), bo.Uint32(buf[40:])
		} else {
			s.flags, s.off, s.len, s.link = uint64(bo.Uint32(buf[8:])), uint64(bo.Uint32(buf[16:])), uint64(bo.Uint32(buf[20:])), bo.Uint32(buf[24:])
		}
	}

	read := map[int]bool{int(shstrndx): true}
	for i, s := range secs {
		if s.typ == elf.SHT_DYNAMIC || s.typ == elf.SHT_GNU_VERDEF {
			read[i] = true
			read[int(s.link)] = true
		}
	}
	for i, s := range secs {
		if !read[i] {
			continue
		}
		if s.flags&uint64(elf.SHF_COMPRESSED) != 0 {
			return fmt.Errorf("%w: section %d is compressed", errNotGlibc, i)
		}
		if s.typ == elf.SHT_NOBITS || s.len < 4 || s.off > math.MaxInt64-4 {
			continue
		}
		var magic [4]byte
		if _, err := r.ReadAt(magic[:], int64(s.off)); err == nil && string(magic[:]) == "ZLIB" {
			return fmt.Errorf("%w: section %d is zlib-compressed", errNotGlibc, i)
		}
	}
	return nil
}

// maxVerdefSize bounds the version definition section and its string table.
// glibc's are a few KB.
const maxVerdefSize = 1 << 20

// versionDefinitions returns the names of the version definitions
// (SHT_GNU_VERDEF entries) of f. It reads only the first name of each
// definition and at most 4096 definitions: debug/elf's DynamicVersions also
// collects every parent name, which a crafted file can make take minutes and
// gigabytes.
func versionDefinitions(f *elf.File) ([]string, error) {
	sec := f.SectionByType(elf.SHT_GNU_VERDEF)
	if sec == nil {
		return nil, fmt.Errorf("%w: no version definitions", errNotGlibc)
	}
	if sec.Size > maxVerdefSize || int(sec.Link) <= 0 || int(sec.Link) >= len(f.Sections) {
		return nil, fmt.Errorf("%w: malformed version definition section", errNotGlibc)
	}
	strs := f.Sections[sec.Link]
	if strs.Type != elf.SHT_STRTAB || strs.Size > maxVerdefSize {
		return nil, fmt.Errorf("%w: malformed version definition string table", errNotGlibc)
	}
	d, err := sec.Data()
	if err != nil {
		return nil, err
	}
	str, err := strs.Data()
	if err != nil {
		return nil, err
	}
	bo := f.ByteOrder
	var names []string
	for i, n := 0, 0; i+20 <= len(d) && n < 4096; n++ {
		// Elf_Verdef: version u16, flags u16, ndx u16, cnt u16, hash u32,
		// aux u32, next u32. Elf_Verdaux: name u32, next u32.
		if bo.Uint16(d[i:]) != 1 {
			return nil, fmt.Errorf("%w: unknown version definition revision", errNotGlibc)
		}
		cnt, aux, next := bo.Uint16(d[i+6:]), bo.Uint32(d[i+12:]), bo.Uint32(d[i+16:])
		if j := uint64(i) + uint64(aux); cnt > 0 && j+8 <= uint64(len(d)) {
			if name, ok := cString(str, bo.Uint32(d[j:])); ok {
				names = append(names, name)
			}
		}
		if next == 0 || uint64(i)+uint64(next) > uint64(len(d)) {
			break
		}
		i += int(next)
	}
	return names, nil
}

// cString returns the NUL-terminated string at off in a string table.
func cString(tab []byte, off uint32) (string, bool) {
	if uint64(off) >= uint64(len(tab)) {
		return "", false
	}
	s := tab[off:]
	end := bytes.IndexByte(s, 0)
	if end < 0 {
		return "", false
	}
	return string(s[:end]), true
}

// maxGlibcVersion returns the highest numeric version among names of the
// form GLIBC_<digits>(.<digits>)*, or "" if there is none. GLIBC_PRIVATE and
// other non-numeric definitions are ignored.
func maxGlibcVersion(names []string) string {
	best := ""
	for _, n := range names {
		v, ok := strings.CutPrefix(n, "GLIBC_")
		if !ok || numericPrefix(v, 2, 4) != v {
			continue
		}
		if best == "" || compareNumeric(v, best) > 0 {
			best = v
		}
	}
	return best
}

// glibcBannerVersion finds glibc's banner from csu/version.c, "GNU C Library
// (...) stable release version 2.35.", in data and returns its version, or
// "" if there is no such banner.
func glibcBannerVersion(data []byte) string {
	const (
		head   = "GNU C Library"
		marker = "release version "
	)
	for {
		i := bytes.Index(data, []byte(head))
		if i < 0 {
			return ""
		}
		data = data[i+len(head):]
		// The banner is one NUL-terminated string of well under 256 bytes; look
		// no further, which also keeps this loop linear in len(data).
		s := data[:min(len(data), 256)]
		if j := bytes.IndexByte(s, 0); j >= 0 {
			s = s[:j]
		}
		if k := bytes.Index(s, []byte(marker)); k >= 0 {
			if v := numericPrefix(string(s[k+len(marker):]), 2, 4); v != "" {
				return v
			}
		}
	}
}

// compareNumeric compares two dotted numeric versions segment by segment.
// Segments too long to parse compare by length, then lexically, which is the
// numeric order for digit strings without leading zeros.
func compareNumeric(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := "", "" // missing and zero segments both trim to ""
		if i < len(as) {
			x = strings.TrimLeft(as[i], "0")
		}
		if i < len(bs) {
			y = strings.TrimLeft(bs[i], "0")
		}
		if len(x) != len(y) {
			if len(x) < len(y) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func majorOf(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return strings.TrimLeft(m, "0")
}

// parseLibcVersionOutput extracts the glibc version from the output of
// `getconf GNU_LIBC_VERSION` ("glibc 2.35") or `ldd --version`
// ("ldd (Ubuntu GLIBC 2.35-0ubuntu3.1) 2.35"). It ports rattler's
// parse_glibc_ldd_version: the first line that names glibc, gentoo, GNU libc
// or Solus (any case) and ends in a dotted number yields that number. musl's
// output names none of these and yields "".
func parseLibcVersionOutput(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r \t")
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "glibc") && !strings.Contains(lower, "gentoo") &&
			!strings.Contains(lower, "gnu libc") && !strings.Contains(lower, "solus") {
			continue
		}
		last := line[strings.LastIndexAny(line, " \t")+1:]
		if v := numericPrefix(last, 1, 4); v != "" && v == last {
			return v
		}
	}
	return ""
}
