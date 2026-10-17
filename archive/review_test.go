package archive

// Regression tests from the WP3 review probes.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// An attacker who can write the destination's parent swaps the private
// temporary directory for a symlink to another directory mid-extraction.
// Nothing may be created or changed there, and the destination must not
// become the attacker's symlink.
func TestTempDirSwapDetected(t *testing.T) {
	parent, dest := sandbox(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_600_000_000, 0)
	os.Chtimes(victim, old, old)
	os.Chmod(outside, 0o700)
	head := (&rawTar{}).file("a", []byte("a")).file("victim.txt", []byte("x")).buf.Bytes()
	rest := (&rawTar{}).symlink("evil-link", "a").hardlink("evil-hard", "a").fileMtime("victim.txt", []byte("y"), 12345).finish()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- ExtractReader(context.Background(), pr, TarZst, dest, Limits{}) }()
	pw.Write(zstdCompress(head))
	// Wait until the first entries are on disk, then swap.
	var tmp string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		m, _ := filepath.Glob(filepath.Join(parent, ".dest.tmp-*"))
		if len(m) == 1 {
			if _, err := os.Stat(filepath.Join(m[0], "victim.txt")); err == nil {
				tmp = m[0]
				break
			}
		}
	}
	if tmp == "" {
		t.Fatal("temporary directory not found")
	}
	if err := os.Rename(tmp, tmp+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tmp); err != nil {
		t.Skip("cannot create symlinks:", err)
	}
	pw.Write(zstdCompress(rest))
	pw.Close()
	err := <-done
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("error %v, want ErrUnsafePath for the replaced temporary directory", err)
	}
	if got := entries(t, outside); len(got) != 1 || got[0] != "victim.txt" {
		t.Errorf("outside directory now holds %q", got)
	}
	fi, _ := os.Stat(outside)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("outside directory mode changed to %v", fi.Mode())
	}
	vfi, _ := os.Stat(victim)
	if b, _ := os.ReadFile(victim); string(b) != "v" || !vfi.ModTime().Equal(old) {
		t.Errorf("victim changed: %q, mtime %v", b, vfi.ModTime())
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Error("destination was created")
	}
	if _, err := os.Lstat(tmp); err == nil {
		t.Error("planted symlink left in place")
	}
}

func condaParts() (zipMember, zipMember, zipMember) {
	info := (&rawTar{}).file("info/index.json", []byte("{}")).finish()
	pkg := (&rawTar{}).file("ok.txt", []byte("fine")).finish()
	return zipMember{"metadata.json", []byte(`{"conda_pkg_format_version": 2}`)},
		zipMember{"pkg-x-1.0-0.tar.zst", zstdCompress(pkg)},
		zipMember{"info-x-1.0-0.tar.zst", zstdCompress(info)}
}

func TestZipPrependedAndTrailingData(t *testing.T) {
	m, p, i := condaParts()
	z := buildZip(m, p, i)
	for _, n := range []int{1, 100, 4000, 60000} {
		parent, dest := sandbox(t)
		err := extractBytes(t, "x-1.0-0.conda", append(bytes.Clone(z), bytes.Repeat([]byte{0xa5}, n)...), dest, Limits{})
		if !errors.Is(err, ErrFormat) {
			t.Errorf("%d trailing bytes: %v, want ErrFormat", n, err)
		}
		assertNothingLeft(t, parent)
	}
	parent, dest := sandbox(t)
	if err := extractBytes(t, "x-1.0-0.conda", append(bytes.Repeat([]byte{0x5a}, 1000), z...), dest, Limits{}); !errors.Is(err, ErrFormat) {
		t.Errorf("1000 prepended bytes: %v, want ErrFormat", err)
	}
	assertNothingLeft(t, parent)
	// Prepended bytes that start like a local header but belong to no
	// member.
	fake := append([]byte("PK\x03\x04"), make([]byte, 26)...)
	fake = append(fake, bytes.Repeat([]byte{0x5a}, 100)...)
	parent, dest = sandbox(t)
	if err := extractBytes(t, "x-1.0-0.conda", append(fake, z...), dest, Limits{}); !errors.Is(err, ErrFormat) {
		t.Errorf("fake local header prepended: %v, want ErrFormat", err)
	}
	assertNothingLeft(t, parent)
	// A zip comment is part of the end record and is fine.
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, mem := range []zipMember{m, p, i} {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: mem.name, Method: zip.Store})
		w.Write(mem.data)
	}
	zw.SetComment("a comment")
	zw.Close()
	_, dest = sandbox(t)
	if err := extractBytes(t, "x-1.0-0.conda", b.Bytes(), dest, Limits{}); err != nil {
		t.Errorf("zip with a comment: %v", err)
	}
}

func TestMetadataStrict(t *testing.T) {
	_, p, i := condaParts()
	cases := []struct {
		meta string
		ok   bool
	}{
		{`{"conda_pkg_format_version": 2}`, true},
		{`{"conda_pkg_format_version":2,"x":[1,{"y":null}]}`, true},
		{"", true}, // libmamba accepts an empty metadata.json
		{"null", false},
		{"[]", false},
		{`"x"`, false},
		{"{}", false},
		{`{"x":1}`, false},
		{`{"conda_pkg_format_version": 2.0}`, false},
		{`{"conda_pkg_format_version": "2"}`, false},
		{`{"conda_pkg_format_version": 3}`, false},
		{`{"conda_pkg_format_version": 2, "conda_pkg_format_version": 3}`, false},
		{`{"conda_pkg_format_version": 2, "conda_pkg_format_version": 2}`, false},
		{`{"conda_pkg_format_version": 2} {}`, false},
		{`{"conda_pkg_format_version": 2}x`, false},
		{`{"conda_pkg_format_version": 2`, false},
	}
	for _, tc := range cases {
		parent, dest := sandbox(t)
		err := extractBytes(t, "x-1.0-0.conda", buildZip(zipMember{"metadata.json", []byte(tc.meta)}, p, i), dest, Limits{})
		if tc.ok && err != nil {
			t.Errorf("metadata %s: %v", tc.meta, err)
		}
		if !tc.ok {
			if !errors.Is(err, ErrFormat) {
				t.Errorf("metadata %s: %v, want ErrFormat", tc.meta, err)
			}
			assertNothingLeft(t, parent)
		}
	}
	// No metadata.json at all is accepted, as by libmamba and rattler.
	_, dest := sandbox(t)
	if err := extractBytes(t, "x-1.0-0.conda", buildZip(p, i), dest, Limits{}); err != nil {
		t.Errorf("no metadata.json: %v", err)
	}
}

// The first local header's name differs from the central directory's: the
// local header at offset 0 must be the first central-directory entry's.
func TestZipLocalNameMismatch(t *testing.T) {
	_, p, i := condaParts()
	z := buildZip(p, i)
	nl := int(binary.LittleEndian.Uint16(z[26:]))
	copy(z[30:30+nl], strings.Repeat("A", nl))
	parent, dest := sandbox(t)
	if err := extractBytes(t, "x-1.0-0.conda", z, dest, Limits{}); !errors.Is(err, ErrFormat) {
		t.Fatalf("local name mismatch: %v, want ErrFormat", err)
	}
	assertNothingLeft(t, parent)
}

func TestPathLimits(t *testing.T) {
	deep := strings.TrimSuffix(strings.Repeat(strings.Repeat("d", 200)+"/", 25), "/")
	cases := []struct {
		name string
		tar  []byte
		want error
	}{
		{"name over MaxPathLen", (&rawTar{}).longFile(deep+"/f.txt", []byte("x")).finish(), ErrLimit},
		{"symlink name over MaxPathLen", (&rawTar{}).pax(&tar.Header{Typeflag: tar.TypeSymlink, Name: deep + "/s", Linkname: "x", Mode: 0o777}, nil).finish(), ErrLimit},
		{"symlink target over MaxPathLen", (&rawTar{}).pax(&tar.Header{Typeflag: tar.TypeSymlink, Name: "s", Linkname: strings.Repeat("d/../", 1000) + "d", Mode: 0o777}, nil).finish(), ErrLimit},
		{"hard link name over MaxPathLen", (&rawTar{}).file("a", nil).pax(&tar.Header{Typeflag: tar.TypeLink, Name: "b", Linkname: deep, Mode: 0o644}, nil).finish(), ErrLimit},
		{"name over MaxPathDepth", (&rawTar{}).longFile(strings.Repeat("a/", 300)+"f", []byte("x")).finish(), ErrLimit},
		{"target over MaxPathDepth", (&rawTar{}).pax(&tar.Header{Typeflag: tar.TypeSymlink, Name: "s", Linkname: strings.Repeat("./", 300) + "x", Mode: 0o777}, nil).finish(), ErrLimit},
	}
	for _, tc := range cases {
		parent, dest := sandbox(t)
		if err := extractTarBz2(t, bz2Compress(tc.tar), dest); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
		assertNothingLeft(t, parent)
	}
	// A component longer than the filesystem allows fails cleanly.
	parent, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress((&rawTar{}).longFile(strings.Repeat("n", 300), []byte("x")).finish()), dest); err == nil {
		t.Error("300-byte component extracted")
	}
	assertNothingLeft(t, parent)
	// Custom limits apply.
	_, dest = sandbox(t)
	tb := (&rawTar{}).file("a/b/c/d", []byte("x")).finish()
	if err := ExtractReader(context.Background(), bytes.NewReader(zstdCompress(tb)), TarZst, dest, Limits{MaxPathDepth: 3}); !errors.Is(err, ErrLimit) {
		t.Errorf("MaxPathDepth 3: %v", err)
	}
}

// One entry with a ~1 MiB, 4000-component name, and symlinks with 1 MiB
// targets, used to cost minutes; the path limits refuse them at once.
func TestHugeNamesRefusedQuickly(t *testing.T) {
	prefix := strings.TrimSuffix(strings.Repeat(strings.Repeat("d", 250)+"/", 4000), "/")
	target := strings.Repeat("d/../", 200000) + "d"
	for name, gen := range map[string]func(tw *tar.Writer){
		"long name": func(tw *tar.Writer) {
			for i := range 8 {
				tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("%s/f%d", prefix, i), Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX})
			}
		},
		"long targets": func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755})
			for i := range 4 {
				tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("l%d", i), Linkname: target, Typeflag: tar.TypeSymlink, Mode: 0o777, Format: tar.FormatPAX})
			}
		},
	} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		gen(tw)
		tw.Close()
		parent, dest := sandbox(t)
		start := time.Now()
		err := ExtractReader(context.Background(), bytes.NewReader(zstdCompress(b.Bytes())), TarZst, dest, Limits{})
		if !errors.Is(err, ErrLimit) {
			t.Errorf("%s: %v, want ErrLimit", name, err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s took %v", name, d)
		}
		if len(err.Error()) > 2000 {
			t.Errorf("%s: %d-byte error message", name, len(err.Error()))
		}
		assertNothingLeft(t, parent)
	}
}

func TestReviewLinkLayouts(t *testing.T) {
	type tc struct {
		tar  []byte
		want error // nil: extracts
	}
	unixOnly := func(e error) error {
		if onWindows {
			return ErrUnsafePath
		}
		return e
	}
	cases := map[string]tc{
		"hardlink chain":                {(&rawTar{}).file("a", []byte("a")).hardlink("b", "a").hardlink("c", "b").finish(), nil},
		"hardlink then dir of its name": {(&rawTar{}).file("a", []byte("a")).hardlink("b", "a").dir("a/").finish(), ErrConflict},
		"hardlink dot prefix":           {(&rawTar{}).file("a", []byte("a")).hardlink("b", "./a").finish(), nil},
		"hardlink via symlinked dir":    {(&rawTar{}).file("real/a", []byte("a")).symlink("s", "real").hardlink("b", "s/a").finish(), ErrUnsafePath},
		"symlink to dir then same dir":  {(&rawTar{}).dir("real/").symlink("s", "real").dir("s/").finish(), ErrConflict},
		"symlink then symlink through":  {(&rawTar{}).symlink("s", ".").symlink("s/t", "..").finish(), ErrUnsafePath},
		"symlink . chain escape":        {(&rawTar{}).symlink("a/s", ".").symlink("a/t", "s/s/s/../../x").finish(), ErrUnsafePath},
		"symlink target trailing slash": {(&rawTar{}).symlink("a", "../").finish(), ErrUnsafePath},
		"symlink dotdot via file":       {(&rawTar{}).file("f", []byte("x")).symlink("l", "f/../..").finish(), ErrUnsafePath},
		"symlink to self dir dotdot":    {(&rawTar{}).dir("a/b/").symlink("a/b/l", "../../..").finish(), ErrUnsafePath},
		"symlink dotdot via missing":    {(&rawTar{}).symlink("a/l", "missing/../../..").finish(), ErrUnsafePath},
		"dir entry after files":         {(&rawTar{}).file("d/a", []byte("a")).dir("d/").finish(), nil},
		"dot and double slashes":        {(&rawTar{}).file(".//a//./b", []byte("a")).finish(), nil},
		"backslash name":                {(&rawTar{}).file(`a\b`, []byte("a")).finish(), unixOnly(nil)},
		"symlink backslash dotdot":      {(&rawTar{}).symlink("l", `..\..\x`).finish(), unixOnly(nil)},
	}
	for name, c := range cases {
		for _, kind := range []string{"x.tar.bz2", "x-1.0-0.conda"} {
			data := bz2Compress(c.tar)
			if kind != "x.tar.bz2" {
				data = buildConda("x", c.tar)
			}
			parent, dest := sandbox(t)
			err := extractBytes(t, kind, data, dest, Limits{})
			switch {
			case c.want == nil && err != nil:
				t.Errorf("%s/%s: %v", name, kind, err)
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("%s/%s: %v, want %v", name, kind, err, c.want)
			case c.want != nil:
				assertNothingLeft(t, parent)
			}
		}
	}
}

func TestCancelAtManyPoints(t *testing.T) {
	var tb rawTar
	for i := range 200 {
		tb.file(fmt.Sprintf("d%d/f%d", i%7, i), make([]byte, 10000))
	}
	src := fixture(t, "x-1.0-0.conda", buildConda("x", tb.finish()))
	parent := t.TempDir()
	for i := range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(time.Duration(i) * 100 * time.Microsecond)
			cancel()
		}()
		dest := filepath.Join(parent, "sub", fmt.Sprintf("d%d", i))
		err := Extract(ctx, src, dest, Limits{})
		cancel()
		if err == nil {
			os.RemoveAll(filepath.Join(parent, "sub"))
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("cancel %d: %v", i, err)
		}
		assertNothingLeft(t, parent)
	}
}

// Parent directories Extract creates for the destination are removed again
// when extraction fails.
func TestCreatedParentsRemovedOnError(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "a", "b", "dest")
	bad := bz2Compress((&rawTar{}).file("../x", nil).finish())
	if err := extractTarBz2(t, bad, dest); !errors.Is(err, ErrUnsafePath) {
		t.Fatal(err)
	}
	assertNothingLeft(t, parent)
	// Existing parents stay.
	os.Mkdir(filepath.Join(parent, "keep"), 0o755)
	if err := extractTarBz2(t, bad, filepath.Join(parent, "keep", "new", "dest")); err == nil {
		t.Fatal("hostile archive extracted")
	}
	assertNothingLeft(t, parent, "keep")
	assertNothingLeft(t, filepath.Join(parent, "keep"))
}

// zstd edge cases from the review: frames of a few bytes each, skippable
// frames, a declared but missing skippable payload, and a window beyond
// the decoder limit.
func TestZstdStreamEdgeCases(t *testing.T) {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "a", Typeflag: tar.TypeReg, Size: 3, Mode: 0o644})
	tw.Write([]byte("abc"))
	tw.Close()
	raw := tb.Bytes()
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	frame := enc.EncodeAll(raw, nil)
	var tiny []byte
	for i := 0; i < len(raw); i += 4 {
		tiny = enc.EncodeAll(raw[i:min(i+4, len(raw))], tiny)
	}
	skip := func(n int) []byte {
		return append([]byte{0x50, 0x2a, 0x4d, 0x18, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}, make([]byte, n)...)
	}
	cases := []struct {
		name string
		data []byte
		lim  Limits
		ok   bool
	}{
		{"tiny frames", tiny, Limits{}, true},
		{"skippable after", append(bytes.Clone(frame), skip(16)...), Limits{}, true},
		{"skippable before", append(skip(16), frame...), Limits{}, true},
		{"skippable declared 4 GiB, missing", append(bytes.Clone(frame), 0x50, 0x2a, 0x4d, 0x18, 0xff, 0xff, 0xff, 0xff), Limits{}, false},
		{"skippable over MaxTotalSize", append(bytes.Clone(frame), skip(8<<20)...), Limits{MaxTotalSize: 4 << 20}, false},
	}
	for _, c := range cases {
		parent, dest := sandbox(t)
		err := ExtractReader(context.Background(), bytes.NewReader(c.data), TarZst, dest, c.lim)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil {
			assertNothingLeft(t, parent)
		}
	}
	// A frame declaring a 256 MiB window is refused without allocating it.
	var big bytes.Buffer
	w, err := zstd.NewWriter(&big, zstd.WithWindowSize(1<<28), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	tw = tar.NewWriter(w)
	tw.WriteHeader(&tar.Header{Name: "a", Typeflag: tar.TypeReg, Size: 1 << 20, Mode: 0o644})
	tw.Write(make([]byte, 1<<20))
	tw.Close()
	w.Close()
	parent, dest := sandbox(t)
	if err := ExtractReader(context.Background(), &big, TarZst, dest, Limits{}); err == nil {
		t.Log("encoder chose a smaller window; nothing to check")
	} else {
		assertNothingLeft(t, parent)
	}
}

func TestBoundErr(t *testing.T) {
	long := strings.Repeat("x", 5000)
	err := boundErr(&fs.PathError{Op: "open", Path: long, Err: fs.ErrNotExist})
	if len(err.Error()) > 400 || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("PathError not bounded: %d bytes", len(err.Error()))
	}
	err = boundErr(&os.LinkError{Op: "link", Old: long, New: long, Err: fs.ErrExist})
	if len(err.Error()) > 700 || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("LinkError not bounded: %d bytes", len(err.Error()))
	}
}
