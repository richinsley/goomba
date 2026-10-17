package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostileCase is one tar archive that must be refused.
type hostileCase struct {
	name string
	tar  []byte
	want error
}

func hostileTars(outside string) []hostileCase {
	t := func() *rawTar { return &rawTar{} }
	return []hostileCase{
		{"dotdot", t().file("../escape.txt", []byte("x")).finish(), ErrUnsafePath},
		{"nested dotdot", t().file("a/../../escape.txt", []byte("x")).finish(), ErrUnsafePath},
		{"dotdot dir", t().dir("../escape/").finish(), ErrUnsafePath},
		{"backslash dotdot", t().file(`a\..\..\escape.txt`, []byte("x")).finish(), ErrUnsafePath},
		{"absolute", t().pax(&tar.Header{Typeflag: tar.TypeReg, Name: filepath.ToSlash(filepath.Join(outside, "abs.txt")), Mode: 0o644, Size: 1}, []byte("x")).finish(), ErrUnsafePath},
		{"absolute root", t().file("/etc/escape", []byte("x")).finish(), ErrUnsafePath},
		{"drive", t().file("C:/escape.txt", []byte("x")).finish(), ErrUnsafePath},
		{"drive relative", t().file("c:escape.txt", []byte("x")).finish(), ErrUnsafePath},
		{"unc", t().file(`\\server\share\escape.txt`, []byte("x")).finish(), ErrUnsafePath},
		{"names destination", t().file("./", []byte("")).symlink(".", "x").finish(), ErrUnsafePath},
		{"symlink to parent then file", t().symlink("escape", "..").file("escape/pwned.txt", []byte("x")).finish(), ErrUnsafePath},
		{"symlink inside then file through it", t().dir("real/").symlink("alias", "real").file("alias/x.txt", []byte("x")).finish(), ErrUnsafePath},
		{"symlink then dir through it", t().symlink("s", "sub").dir("s/deeper/").finish(), ErrUnsafePath},
		{"symlink escaping", t().symlink("escape", "../outside").finish(), ErrUnsafePath},
		{"symlink escaping deep", t().symlink("a/b/escape", "../../../x").finish(), ErrUnsafePath},
		{"symlink absolute", t().symlink("escape", filepath.ToSlash(outside)).finish(), ErrUnsafePath},
		{"symlink drive", t().symlink("escape", "C:/Windows").finish(), ErrUnsafePath},
		{"symlink empty", t().symlink("escape", "").finish(), ErrUnsafePath},
		// d/s resolves to the root, so t's ".." leaves it although its
		// target is lexically inside.
		{"symlink chain", t().symlink("d/s", "..").symlink("t", "d/s/../escape").finish(), ErrUnsafePath},
		// x is created after t; only the final tree shows that t escapes.
		{"symlink chain created later", t().symlink("t", "x/../escape").symlink("x", ".").finish(), ErrUnsafePath},
		{"symlink loop", t().symlink("a", "b").symlink("b", "a").finish(), ErrUnsafePath},
		{"hardlink outside", t().hardlink("h.txt", "../outside/secret.txt").finish(), ErrUnsafePath},
		{"hardlink absolute", t().hardlink("h.txt", filepath.ToSlash(filepath.Join(outside, "secret.txt"))).finish(), ErrUnsafePath},
		{"hardlink not yet extracted", t().hardlink("h.txt", "later.txt").file("later.txt", []byte("x")).finish(), ErrUnsafePath},
		{"hardlink to symlink", t().symlink("s.txt", "a.txt").file("a.txt", []byte("x")).hardlink("h.txt", "s.txt").finish(), ErrUnsafePath},
		{"hardlink to directory", t().dir("d/").hardlink("h", "d").finish(), ErrUnsafePath},
		{"hardlink to itself", t().file("a.txt", []byte("x")).hardlink("a.txt", "a.txt").finish(), ErrUnsafePath},
		{"char device", t().special(tar.TypeChar, "dev/null").finish(), ErrUnsupportedEntry},
		{"block device", t().special(tar.TypeBlock, "dev/sda").finish(), ErrUnsupportedEntry},
		{"fifo", t().special(tar.TypeFifo, "fifo").finish(), ErrUnsupportedEntry},
		{"contiguous file", t().special(tar.TypeCont, "cont").finish(), ErrUnsupportedEntry},
		{"unknown type", t().special('Z', "zzz").finish(), ErrUnsupportedEntry},
		{"pax sparse", t().paxSparse("sparse.txt").finish(), ErrUnsupportedEntry},
		{"gnu sparse", t().gnuSparse("sparse.txt").finish(), ErrUnsupportedEntry},
		{"file then dir", t().file("x", []byte("x")).dir("x/").finish(), ErrConflict},
		{"dir then file", t().dir("x/").file("x", []byte("x")).finish(), ErrConflict},
		{"implicit dir then file", t().file("d/a.txt", []byte("a")).file("d", []byte("x")).finish(), ErrConflict},
		{"dir then symlink", t().file("d/a.txt", []byte("a")).symlink("d", "..").file("d/b.txt", []byte("b")).finish(), ErrConflict},
		{"file then symlink", t().file("x", []byte("x")).symlink("x", "y").finish(), ErrConflict},
		{"symlink then file", t().symlink("x", "y").file("x", []byte("x")).finish(), ErrConflict},
		{"symlink then hardlink", t().file("a", []byte("a")).symlink("x", "a").hardlink("x", "a").finish(), ErrConflict},
		{"file under file", t().file("f", []byte("x")).file("f/g", []byte("y")).finish(), ErrConflict},
	}
}

// paxRecord formats one PAX extended header record.
func paxRecord(k, v string) string {
	body := " " + k + "=" + v + "\n"
	n := len(body)
	for {
		s := fmt.Sprintf("%d%s", n, body)
		if len(s) == n {
			return s
		}
		n = len(s)
	}
}

// paxSparse writes a regular entry marked sparse (GNU format 0.1) through
// PAX records.
func (t *rawTar) paxSparse(name string) *rawTar {
	recs := paxRecord("GNU.sparse.major", "0") + paxRecord("GNU.sparse.minor", "1") +
		paxRecord("GNU.sparse.numblocks", "0") + paxRecord("GNU.sparse.map", "") +
		paxRecord("GNU.sparse.size", "0")
	t.header(tar.TypeXHeader, "PaxHeaders/"+name, "", 0o644, int64(len(recs)), rawMtime, true)
	t.data([]byte(recs))
	t.header(tar.TypeReg, name, "", 0o644, 0, rawMtime, true)
	return t
}

// gnuSparse writes an old-GNU sparse entry ('S' with GNU magic).
func (t *rawTar) gnuSparse(name string) *rawTar {
	start := t.buf.Len()
	t.header(tar.TypeGNUSparse, name, "", 0o644, 0, rawMtime, true)
	h := t.buf.Bytes()[start : start+512]
	copy(h[257:265], "ustar  \x00")
	copy(h[148:156], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:155], fmt.Sprintf("%06o\x00", sum))
	h[155] = ' '
	return t
}

// TestHostileTars feeds every hostile tar through all three containers:
// .tar.bz2 by path, tar.zst by stream, and as the pkg tar of a .conda.
func TestHostileTars(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range hostileTars(outside) {
		t.Run(tc.name, func(t *testing.T) {
			runs := map[string]func(dest string) error{
				"tar.bz2": func(dest string) error { return extractBytes(t, "x.tar.bz2", bz2Compress(tc.tar), dest, Limits{}) },
				"tar.zst": func(dest string) error {
					return ExtractReader(context.Background(), bytes.NewReader(zstdCompress(tc.tar)), TarZst, dest, Limits{})
				},
				"conda": func(dest string) error {
					return extractBytes(t, "x-1.0-0.conda", buildConda("x", tc.tar), dest, Limits{})
				},
			}
			for _, kind := range []string{"tar.bz2", "tar.zst", "conda"} {
				parent, dest := sandbox(t)
				err := runs[kind](dest)
				if !errors.Is(err, tc.want) {
					t.Errorf("%s: error %v, want %v", kind, err, tc.want)
				}
				assertNothingLeft(t, parent)
				assertNothingLeft(t, outside, "secret.txt")
				if data, err := os.ReadFile(secret); err != nil || string(data) != "secret" {
					t.Fatalf("%s: outside file changed: %q %v", kind, data, err)
				}
				if fi, err := os.Stat(secret); err != nil || nlink(fi) != 1 {
					t.Fatalf("%s: outside file gained a hard link", kind)
				}
			}
		})
	}
}

func TestZipSlipMemberNamesAreNotPaths(t *testing.T) {
	// Zip member names only select the pkg and info tars; nothing is ever
	// written under them, so traversal in a member name has no effect.
	info := (&rawTar{}).file("info/index.json", []byte("{}")).finish()
	pkg := (&rawTar{}).file("ok.txt", []byte("fine")).finish()
	conda := buildZip(
		zipMember{"../../metadata.json", []byte(`{"conda_pkg_format_version": 2}`)},
		zipMember{"../../evil/pkg-x-1.0-0.tar.zst", zstdCompress(pkg)},
		zipMember{"/abs/info-x-1.0-0.tar.zst", zstdCompress(info)},
		zipMember{"../escape.txt", []byte("pwned")},
	)
	parent, dest := sandbox(t)
	if err := extractBytes(t, "x-1.0-0.conda", conda, dest, Limits{}); err != nil {
		t.Fatal(err)
	}
	assertNothingLeft(t, parent, "dest")
	got := snapshot(t, dest)
	if len(got) != 3 || got["ok.txt"].Type != "file" || got["info/index.json"].Type != "file" {
		t.Fatalf("unexpected tree %v", got)
	}
}

func TestZipSlipInPkgTar(t *testing.T) {
	pkg := (&rawTar{}).file("../../zipslip.txt", []byte("pwned")).finish()
	parent, dest := sandbox(t)
	err := extractBytes(t, "x-1.0-0.conda", buildConda("x", pkg), dest, Limits{})
	assertIs(t, err, ErrUnsafePath)
	assertNothingLeft(t, parent)
	if _, err := os.Lstat(filepath.Join(filepath.Dir(parent), "zipslip.txt")); err == nil {
		t.Fatal("zip slip wrote outside the destination")
	}
}

var errAny = errors.New("any error")

func TestMalformedConda(t *testing.T) {
	info := (&rawTar{}).file("info/index.json", []byte("{}")).finish()
	pkg := (&rawTar{}).file("ok.txt", []byte("fine")).finish()
	meta := func(s string) zipMember { return zipMember{"metadata.json", []byte(s)} }
	pkgM := zipMember{"pkg-x-1.0-0.tar.zst", zstdCompress(pkg)}
	infoM := zipMember{"info-x-1.0-0.tar.zst", zstdCompress(info)}
	cases := []struct {
		name string
		data []byte
		want error // nil: must succeed; errAny: any error
	}{
		{"ok", buildZip(meta(`{"conda_pkg_format_version": 2}`), pkgM, infoM), nil},
		{"empty metadata", buildZip(meta(""), pkgM, infoM), nil},
		{"no metadata", buildZip(pkgM, infoM), nil},
		{"unknown extra member", buildZip(meta(`{"conda_pkg_format_version": 2}`), pkgM, infoM, zipMember{"signature.bin", []byte("x")}), nil},
		{"format version 3", buildZip(meta(`{"conda_pkg_format_version": 3}`), pkgM, infoM), ErrFormat},
		{"metadata not json", buildZip(meta(`not json`), pkgM, infoM), ErrFormat},
		{"metadata trailing data", buildZip(meta(`{} {}`), pkgM, infoM), ErrFormat},
		{"metadata huge", buildZip(meta(`{"x":"`+strings.Repeat("a", maxMetadataSize)+`"}`), pkgM, infoM), ErrFormat},
		{"missing pkg", buildZip(meta(`{"conda_pkg_format_version": 2}`), infoM), ErrFormat},
		{"missing info", buildZip(meta(`{"conda_pkg_format_version": 2}`), pkgM), ErrFormat},
		{"two pkg tars", buildZip(pkgM, infoM, zipMember{"pkg-y-1.0-0.tar.zst", zstdCompress(pkg)}), ErrFormat},
		{"duplicate member", buildZip(pkgM, infoM, infoM), ErrFormat},
		{"not a zip", []byte("this is not a zip file at all"), ErrFormat},
		{"empty file", nil, ErrFormat},
		{"pkg not zstd", buildZip(zipMember{"pkg-x-1.0-0.tar.zst", pkg}, infoM), errAny},
		{"trailing bytes after central directory", append(buildZip(pkgM, infoM), bytes.Repeat([]byte{0xa5}, 1<<20)...), ErrFormat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent, dest := sandbox(t)
			err := extractBytes(t, "x-1.0-0.conda", tc.data, dest, Limits{})
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				assertNothingLeft(t, parent, "dest")
				return
			}
			if err == nil {
				t.Fatal("malformed archive extracted")
			}
			if tc.want != errAny {
				assertIs(t, err, tc.want)
			}
			assertNothingLeft(t, parent)
		})
	}
}

func TestEncryptedZipMemberRefused(t *testing.T) {
	info := (&rawTar{}).file("info/index.json", []byte("{}")).finish()
	data := buildZip(zipMember{"pkg-x-1.0-0.tar.zst", zstdCompress(info)}, zipMember{"info-x-1.0-0.tar.zst", zstdCompress(info)})
	// Set the "encrypted" general purpose flag on every header; archive/zip
	// does not check it.
	for _, sig := range [][]byte{{'P', 'K', 3, 4}, {'P', 'K', 1, 2}} {
		off := 6
		if sig[2] == 1 {
			off = 8
		}
		for i := 0; ; {
			j := bytes.Index(data[i:], sig)
			if j < 0 {
				break
			}
			data[i+j+off] |= 1
			i += j + 4
		}
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || zr.File[0].Flags&1 == 0 {
		t.Fatalf("test archive not marked encrypted: %v", err)
	}
	parent, dest := sandbox(t)
	assertIs(t, extractBytes(t, "x-1.0-0.conda", data, dest, Limits{}), ErrFormat)
	assertNothingLeft(t, parent)
}

func TestLimits(t *testing.T) {
	t.Parallel()
	small := Limits{MaxFileSize: 1 << 20, MaxTotalSize: 4 << 20, MaxEntries: 20}
	many := &rawTar{}
	for i := range 30 {
		many.file(fmt.Sprintf("f%02d", i), nil)
	}
	total := &rawTar{}
	for i := range 6 {
		total.file(fmt.Sprintf("f%d", i), make([]byte, 900<<10))
	}
	// One tiny entry, the end-of-archive marker, then 16 MiB of zeros: the
	// limit must count bytes drained after the tar ends.
	trailing := append((&rawTar{}).file("a", []byte("a")).finish(), make([]byte, 16<<20)...)
	cases := []struct {
		name string
		tar  []byte
	}{
		{"file over MaxFileSize", (&rawTar{}).file("big", make([]byte, 2<<20)).finish()},
		{"file header over MaxFileSize", (&rawTar{}).fileWithSize("big", nil, 5<<30).finish()},
		{"total over MaxTotalSize", total.finish()},
		{"entries over MaxEntries", many.finish()},
		{"trailing zeros bomb", trailing},
		// One entry whose name implies 40 parent directories.
		{"implicit directories over MaxEntries", (&rawTar{}).longFile(strings.Repeat("d/", 40)+"f", []byte("f")).finish()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, in := range []struct {
				name string
				data []byte
			}{{"x.tar.bz2", bz2Compress(tc.tar)}, {"x-1.0-0.conda", buildConda("x", tc.tar)}} {
				parent, dest := sandbox(t)
				assertIs(t, extractBytes(t, in.name, in.data, dest, small), ErrLimit)
				assertNothingLeft(t, parent)
			}
		})
	}
	// Exactly at the limits is fine.
	parent, dest := sandbox(t)
	exact := (&rawTar{}).file("exact", make([]byte, 1<<20)).finish()
	if err := extractBytes(t, "x.tar.bz2", bz2Compress(exact), dest, small); err != nil {
		t.Fatal(err)
	}
	assertNothingLeft(t, parent, "dest")
}

func TestDuplicateSameTypeLastWins(t *testing.T) {
	tarball := (&rawTar{}).
		file("a.txt", []byte("first")).
		hardlink("b.txt", "a.txt").
		file("a.txt", []byte("second")).
		dir("d/").dir("d/").dir("./d").
		symlink("l", "a.txt").symlink("l", "b.txt").
		finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dest, p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("a.txt"); got != "second" {
		t.Errorf("a.txt = %q, want the later entry", got)
	}
	if got := read("b.txt"); got != "first" {
		t.Errorf("b.txt = %q: replacing a.txt must not rewrite its earlier hard link", got)
	}
	if got, _ := os.Readlink(filepath.Join(dest, "l")); got != "b.txt" {
		t.Errorf("l -> %q, want b.txt", got)
	}
}

func TestDestinationMustNotExist(t *testing.T) {
	parent, dest := sandbox(t)
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := bz2Compress((&rawTar{}).file("a", []byte("a")).finish())
	assertIs(t, extractTarBz2(t, tarball, dest), fs.ErrExist)
	assertNothingLeft(t, parent, "dest")
	if len(entries(t, dest)) != 0 {
		t.Fatal("existing destination was written to")
	}
}

func TestDestinationParentIsCreated(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "a", "b", "dest")
	if err := extractTarBz2(t, bz2Compress((&rawTar{}).file("x", []byte("x")).finish()), dest); err != nil {
		t.Fatal(err)
	}
	assertNothingLeft(t, filepath.Join(parent, "a", "b"), "dest")
}

func TestUnknownFormat(t *testing.T) {
	_, dest := sandbox(t)
	for _, name := range []string{"x.tar", "x.zip", "x.tar.gz", "x.CONDA", "x.tar.bz2.part"} {
		assertIs(t, Extract(context.Background(), filepath.Join(t.TempDir(), name), dest, Limits{}), ErrUnknownFormat)
	}
	assertIs(t, ExtractReader(context.Background(), bytes.NewReader(nil), Format(9), dest, Limits{}), ErrUnknownFormat)
}

func TestModes(t *testing.T) {
	tarball := (&rawTar{}).
		fileMode("exec", []byte("#!/bin/sh"), 0o755).
		fileMode("group-write", []byte("x"), 0o664).
		fileMode("read-only", []byte("x"), 0o444).
		fileMode("setuid", []byte("x"), 0o4755).
		fileMode("world-write", []byte("x"), 0o666).
		finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"exec": "0755", "group-write": "0664", "read-only": "0444", "setuid": "0755", "world-write": "0664"}
	got := snapshot(t, dest)
	for name, mode := range want {
		if got[name].Mode != mode {
			t.Errorf("%s: mode %s, want %s", name, got[name].Mode, mode)
		}
	}
	// Owners are ignored: everything belongs to the extracting user.
	fi, err := os.Stat(filepath.Join(dest, "exec"))
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByMe(fi) {
		t.Error("extracted file not owned by the current user")
	}
}

func TestRootEntriesAndDotPrefixes(t *testing.T) {
	tarball := (&rawTar{}).dir("./").dir(".").file("./a/./b//c.txt", []byte("c")).finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "a", "b", "c.txt")); err != nil || string(b) != "c" {
		t.Fatalf("a/b/c.txt: %q %v", b, err)
	}
}

func TestDanglingSymlinksAreKept(t *testing.T) {
	// _openmp_mutex ships lib/libgomp.so.1 -> libgomp.so.1.0.0, a file of
	// the libgomp package.
	tarball := (&rawTar{}).symlink("lib/libgomp.so.1", "libgomp.so.1.0.0").
		symlink("lib/deep", "missing/../../lib/x").finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(dest, "lib", "libgomp.so.1")); got != "libgomp.so.1.0.0" {
		t.Fatalf("libgomp.so.1 -> %q", got)
	}
}

func TestSymlinksInsideAreKept(t *testing.T) {
	tarball := (&rawTar{}).
		file("lib/libz.so.1.3.1", []byte("elf")).
		symlink("lib/libz.so", "libz.so.1.3.1").
		symlink("lib/up", "..").
		symlink("bin/tool", "../lib/up/lib/./libz.so").
		symlink("self", ".").
		finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "bin", "tool")); err != nil || string(b) != "elf" {
		t.Fatalf("bin/tool: %q %v", b, err)
	}
}

func TestCleanName(t *testing.T) {
	ok := map[string]string{
		"a":            "a",
		"a/b":          "a/b",
		"./a/b/":       "a/b",
		"a//b/./c":     "a/b/c",
		".":            "",
		"./":           "",
		"a..b":         "a..b",
		"..a/b..":      "..a/b..",
		"perl::Mod.3":  "perl::Mod.3",
		"x/.../y":      "x/.../y",
		`back\slash`:   `back\slash`,
		"name with sp": "name with sp",
	}
	if onWindows {
		delete(ok, "perl::Mod.3")
		delete(ok, `back\slash`)
	}
	for in, want := range ok {
		got, err := cleanName(in, defaultLimits)
		if err != nil || got != want {
			t.Errorf("cleanName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "/", "/a", "../a", "a/..", "a/../b", `..\a`, `a\..`, `\a`, `\\server\share`,
		"C:", "C:/a", "c:a", "z:\\a", "a\x00b", "./\\", "./\\x", ".//C:/x", "./c:",
	} {
		if _, err := cleanName(in, defaultLimits); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("cleanName(%q) = %v, want ErrUnsafePath", in, err)
		}
	}
}
