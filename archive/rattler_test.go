package archive

// Cases from rattler crates/rattler_package_streaming/tests/extract.rs
// (BSD-3-Clause, see NOTICE). Where rattler skips an unsafe entry and
// carries on, goomba refuses the whole archive; those cases assert an
// error and that nothing was written. Network-only cases live in package
// fetch's live tests; cases that need an async runtime are covered by their
// synchronous twins.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var errFlaky = errors.New("flaky")

// flakyReader fails with errFlaky after cutoff bytes.
type flakyReader struct {
	r      io.Reader
	cutoff int
	read   int
}

func (f *flakyReader) Read(p []byte) (int, error) {
	remaining := f.cutoff - f.read
	if remaining <= 0 {
		return 0, errFlaky
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := f.r.Read(p)
	f.read += n
	return n, err
}

// test_extract_flaky_conda: a reader failing part-way must produce an error,
// not a panic. rattler streams a .conda from conda-forge; goomba streams
// only tar formats, so the cutoffs are applied to the local .tar.bz2 and to
// the pkg tar.zst of the local .conda, and a .conda cut at each offset is
// extracted from a file.
func TestExtractFlaky(t *testing.T) {
	t.Parallel()
	cutoffs := []int{0, 1, 13, 50, 74, 150, 8096, 16384, 20000}
	tarBz2 := readFixture(t, "bzip2-1.0.8-h7f98852_4.tar.bz2")
	tarZst := zstdCompress(bz2Decompress(t, tarBz2))
	conda := readFixture(t, "bzip2-1.0.8-hd590300_5.conda")
	for _, cutoff := range cutoffs {
		for _, in := range []struct {
			format Format
			data   []byte
		}{{TarBz2, tarBz2}, {TarZst, tarZst}} {
			parent, dest := sandbox(t)
			err := ExtractReader(context.Background(), &flakyReader{r: bytes.NewReader(in.data), cutoff: cutoff}, in.format, dest, Limits{})
			if !errors.Is(err, errFlaky) {
				t.Errorf("%s cut at %d: error %v, want the reader's error", in.format, cutoff, err)
			}
			assertNothingLeft(t, parent)
		}
		parent, dest := sandbox(t)
		if err := extractBytes(t, "bzip2-1.0.8-hd590300_5.conda", conda[:cutoff], dest, Limits{}); err == nil {
			t.Errorf(".conda cut at %d extracted", cutoff)
		}
		assertNothingLeft(t, parent)
	}
}

func bz2Decompress(t *testing.T, b []byte) []byte {
	t.Helper()
	out, err := io.ReadAll(mustBz2(b))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustBz2(b []byte) io.Reader {
	r, _, _ := decompressor(bytes.NewReader(b), TarBz2)
	return r
}

// test_absolute_path_entry_does_not_set_mtime_outside_destination: rattler
// strips the root and extracts inside; goomba refuses absolute paths. Either
// way the file outside must keep its content and mtime.
func TestAbsolutePathEntryDoesNotTouchOutside(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "rattler_extract_traversal_victim.txt")
	if err := os.WriteFile(victim, []byte("data outside the extraction directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	untouched := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(victim, untouched, untouched); err != nil {
		t.Fatal(err)
	}
	tarball := (&rawTar{}).longFileMtime(filepath.ToSlash(victim), []byte("x"), 1).finish()
	parent, dest := sandbox(t)
	assertIs(t, extractTarBz2(t, bz2Compress(tarball), dest), ErrUnsafePath)
	assertNothingLeft(t, parent)
	fi, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(untouched) {
		t.Fatalf("mtime of a file outside the destination changed to %v", fi.ModTime())
	}
	if b, _ := os.ReadFile(victim); string(b) != "data outside the extraction directory" {
		t.Fatalf("victim content changed: %q", b)
	}
}

// test_extract_tar_with_pre_1980_mtime (and its async twin): an mtime of 1
// must not fail the extraction.
func TestExtractTarWithPre1980Mtime(t *testing.T) {
	tarball := (&rawTar{}).fileMtime("test_file.txt", []byte("hello world"), 1).finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatalf("extraction should not fail due to mtime=1: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "test_file.txt")); err != nil || string(b) != "hello world" {
		t.Fatalf("test_file.txt: %q %v", b, err)
	}
}

// sync_and_async_extract_tar_bz2_identical_trees: the path and stream
// entry points share the extraction code and must write identical trees.
func TestExtractAndExtractReaderIdenticalTrees(t *testing.T) {
	const file = "bzip2-1.0.8-h7f98852_4.tar.bz2"
	byPath := extractFixture(t, file)
	_, dest := sandbox(t)
	if err := ExtractReader(context.Background(), bytes.NewReader(readFixture(t, file)), TarBz2, dest, Limits{}); err != nil {
		t.Fatal(err)
	}
	a, b := snapshot(t, byPath), snapshot(t, dest)
	if len(a) == 0 {
		t.Fatal("empty tree")
	}
	diffSnapshots(t, "stream vs path", b, a)
}

// dotdot_entry_is_skipped_and_never_escapes: rattler skips ".." entries;
// goomba refuses the archive. Nothing may appear outside, and nothing at all
// is left behind.
func TestDotdotEntryNeverEscapes(t *testing.T) {
	tarball := (&rawTar{}).
		file("../escape_dotdot.txt", []byte("escaped")).
		file("nested/../../escape_dotdot2.txt", []byte("escaped")).
		file("ok.txt", []byte("fine")).
		finish()
	parent, dest := sandbox(t)
	assertIs(t, extractTarBz2(t, bz2Compress(tarball), dest), ErrUnsafePath)
	assertNothingLeft(t, parent)
	for _, name := range []string{"escape_dotdot.txt", "escape_dotdot2.txt"} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(parent), name)); err == nil {
			t.Fatalf("%s written outside the destination", name)
		}
	}
}

// file_under_symlink_pointing_outside_is_not_written_outside, with a
// relative and an absolute target.
func TestFileUnderSymlinkPointingOutside(t *testing.T) {
	outside := t.TempDir()
	for _, tc := range []struct{ label, target string }{{"rel", ".."}, {"abs", filepath.ToSlash(outside)}} {
		payload := "pwned_" + tc.label + ".txt"
		r := &rawTar{}
		r.symlink("escape", tc.target)
		r.file("escape/"+payload, []byte("pwned"))
		r.dir("escape/subdir/")
		r.file("escape/subdir/"+payload, []byte("pwned"))
		r.file("after.txt", []byte("after"))
		parent, dest := sandbox(t)
		err := extractTarBz2(t, bz2Compress(r.finish()), dest)
		if err == nil {
			t.Fatalf("%s: writing through an escaping symlink must fail", tc.label)
		}
		assertIs(t, err, ErrUnsafePath)
		assertNothingLeft(t, parent)
		assertNothingLeft(t, outside)
		if _, err := os.Lstat(filepath.Join(filepath.Dir(parent), payload)); err == nil {
			t.Fatalf("%s: file written outside the destination via symlink", tc.label)
		}
	}
}

// directory_entry_over_escaping_symlink_does_not_touch_outside_mtime.
func TestDirectoryEntryOverEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	old := time.Unix(1_500_000_000, 0)
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatal(err)
	}
	r := &rawTar{}
	r.symlink("s", filepath.ToSlash(outside))
	r.header('5', "s/", "", 0o755, 0, 1_600_000_000, true)
	parent, dest := sandbox(t)
	assertIs(t, extractTarBz2(t, bz2Compress(r.finish()), dest), ErrUnsafePath)
	assertNothingLeft(t, parent)
	fi, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Fatalf("directory entry over a symlink changed the mtime outside the destination: %v", fi.ModTime())
	}
}

// symlink_replacing_validated_directory_does_not_escape.
func TestSymlinkReplacingDirectoryDoesNotEscape(t *testing.T) {
	tarball := (&rawTar{}).
		file("d/a.txt", []byte("a")).
		symlink("d", "..").
		file("d/b_escape.txt", []byte("b")).
		finish()
	parent, dest := sandbox(t)
	assertIs(t, extractTarBz2(t, bz2Compress(tarball), dest), ErrConflict)
	assertNothingLeft(t, parent)
	if _, err := os.Lstat(filepath.Join(filepath.Dir(parent), "b_escape.txt")); err == nil {
		t.Fatal("file written outside destination")
	}
}

// hard_link_with_source_outside_destination_is_rejected, relative and
// absolute.
func TestHardLinkWithSourceOutsideRejected(t *testing.T) {
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(t.TempDir(), outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ label, target string }{
		{"relative", "../hardlink_outside/secret.txt"},
		{"relative to real file", filepath.ToSlash(rel)},
		{"absolute", filepath.ToSlash(outside)},
	} {
		tarball := (&rawTar{}).longLink("h.txt", tc.target).file("after.txt", []byte("after")).finish()
		parent, dest := sandbox(t)
		assertIs(t, extractTarBz2(t, bz2Compress(tarball), dest), ErrUnsafePath)
		assertNothingLeft(t, parent)
		if b, _ := os.ReadFile(outside); string(b) != "secret" {
			t.Fatalf("%s: outside file changed", tc.label)
		}
		if fi, _ := os.Stat(outside); nlink(fi) != 1 {
			t.Fatalf("%s: outside file gained a hard link", tc.label)
		}
	}
}

// hard_link_to_symlink_escaping_destination_is_rejected.
func TestHardLinkToEscapingSymlinkRejected(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &rawTar{}
	r.pax(symlinkHeader("s.txt", filepath.ToSlash(outside)), nil)
	r.hardlink("h.txt", "s.txt")
	parent, dest := sandbox(t)
	assertIs(t, extractTarBz2(t, bz2Compress(r.finish()), dest), ErrUnsafePath)
	assertNothingLeft(t, parent)
	if fi, _ := os.Stat(outside); nlink(fi) != 1 {
		t.Fatal("outside file gained a hard link")
	}
}

// hard_link_to_earlier_file_in_archive.
func TestHardLinkToEarlierFile(t *testing.T) {
	payload := noise(3000, 7)
	tarball := (&rawTar{}).
		file("a.txt", payload).
		hardlink("b.txt", "a.txt").
		file("sub/x.txt", []byte("x")).
		hardlink("sub/y.txt", "sub/x.txt").
		hardlink("z.txt", "./sub/x.txt").
		file("c.txt", []byte("c")).
		finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"a.txt": payload, "b.txt": payload, "sub/y.txt": []byte("x"), "z.txt": []byte("x"), "c.txt": []byte("c")} {
		if b, err := os.ReadFile(filepath.Join(dest, name)); err != nil || !bytes.Equal(b, want) {
			t.Errorf("%s: wrong content (%v)", name, err)
		}
	}
	if !onWindows {
		for name, want := range map[string]uint64{"a.txt": 2, "sub/x.txt": 3} {
			fi, err := os.Stat(filepath.Join(dest, name))
			if err != nil || nlink(fi) != want {
				t.Errorf("%s: nlink %d, want %d (%v)", name, nlink(fi), want, err)
			}
		}
	}
}

// empty_files_directories_and_long_names.
func TestEmptyFilesDirectoriesAndLongNames(t *testing.T) {
	longName := strings.Repeat("d", 120) + "/" + strings.Repeat("f", 60) + ".txt"
	tarball := (&rawTar{}).
		file("empty.txt", nil).
		dir("subdir/").
		file("subdir/inner.txt", []byte("inner")).
		dir("emptydir/").
		dir("nested/deeper/").
		oldDir("olddir/").
		file("olddir/inner.txt", []byte("old")).
		file("./dot/prefixed.txt", []byte("dot")).
		longFile(longName, []byte("long")).
		file("last.txt", []byte("last")).
		finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "empty.txt")); err != nil || fi.Size() != 0 {
		t.Errorf("empty.txt: %v", err)
	}
	for _, d := range []string{"subdir", "emptydir", "nested/deeper", "olddir"} {
		if fi, err := os.Stat(filepath.Join(dest, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s is not a directory (%v)", d, err)
		}
	}
	for name, want := range map[string]string{"subdir/inner.txt": "inner", "olddir/inner.txt": "old", "dot/prefixed.txt": "dot", longName: "long", "last.txt": "last"} {
		if b, err := os.ReadFile(filepath.Join(dest, name)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
}

// directories_longer_than_max_path.
func TestDirectoriesLongerThanMaxPath(t *testing.T) {
	deep := strings.Repeat(strings.Repeat("d", 50)+"/", 6)
	tarball := (&rawTar{}).longFile(deep+"a.txt", []byte("a")).longFile(deep+"sub/b.txt", []byte("b")).finish()
	_, dest := sandbox(t)
	if err := extractTarBz2(t, bz2Compress(tarball), dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, deep, "sub", "b.txt")); err != nil || string(b) != "b" {
		t.Fatalf("sub/b.txt: %q %v", b, err)
	}
}

// lying_size_headers_do_not_panic.
func TestLyingSizeHeaders(t *testing.T) {
	data := []byte("0123456789")

	// The header claims more bytes than the whole archive holds.
	parent, dest := sandbox(t)
	tooBig := (&rawTar{}).fileWithSize("big.txt", data, 5000).finish()
	tooBig = tooBig[:len(tooBig)-1024] // no end marker to pad it out
	if err := extractTarBz2(t, bz2Compress(tooBig), dest); err == nil {
		t.Fatal("size larger than the stream must fail")
	}
	assertNothingLeft(t, parent)

	// More bytes claimed than written, but within the padded block: the
	// entry is the data plus zero padding.
	_, dest = sandbox(t)
	within := (&rawTar{}).fileWithSize("padded.txt", data, 300).file("after.txt", []byte("after")).finish()
	if err := extractTarBz2(t, bz2Compress(within), dest); err != nil {
		t.Fatal(err)
	}
	padded, _ := os.ReadFile(filepath.Join(dest, "padded.txt"))
	if len(padded) != 300 || !bytes.Equal(padded[:10], data) || !bytes.Equal(padded[10:], make([]byte, 290)) {
		t.Fatalf("padded.txt: %q", padded)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "after.txt")); string(b) != "after" {
		t.Fatalf("after.txt: %q", b)
	}

	// Fewer bytes claimed than written: only the claimed prefix is the
	// file and the archive continues at the next block.
	_, dest = sandbox(t)
	short := (&rawTar{}).fileWithSize("short.txt", data, 3).file("after.txt", []byte("after")).finish()
	if err := extractTarBz2(t, bz2Compress(short), dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "short.txt")); string(b) != "012" {
		t.Fatalf("short.txt: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "after.txt")); string(b) != "after" {
		t.Fatalf("after.txt: %q", b)
	}
}

// file_entry_replaces_existing_symlink_without_following_it: rattler
// extracts into an existing directory holding a planted symlink. goomba
// never extracts into an existing directory, and within one archive a file
// may not replace a symlink.
func TestFileEntryNeverFollowsPlantedSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, dest := sandbox(t)
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "victim")); err != nil {
		t.Skip("cannot create symlinks:", err)
	}
	tarball := bz2Compress((&rawTar{}).file("victim", []byte("new")).finish())
	if err := extractTarBz2(t, tarball, dest); err == nil {
		t.Fatal("extracted into an existing directory")
	}
	assertNothingLeft(t, parent, "dest")

	r := &rawTar{}
	r.pax(symlinkHeader("victim", filepath.ToSlash(outside)), nil)
	r.file("victim", []byte("new"))
	parent, dest = sandbox(t)
	assertIs(t, extractTarBz2(t, bz2Compress(r.finish()), dest), ErrUnsafePath)
	assertNothingLeft(t, parent)
	if b, _ := os.ReadFile(outside); string(b) != "original" {
		t.Fatalf("write followed the symlink: %q", b)
	}
}

// async_conda_hashes_trailing_input_after_central_directory: rattler's
// streaming reader accepts input after a .conda's zip central directory.
// archive/zip locates the central directory from the end of the file, so a
// .conda with a large tail is refused.
func TestCondaTrailingInputAfterCentralDirectory(t *testing.T) {
	conda := buildConda("early-finish", (&rawTar{}).file("payload.bin", []byte("complete")).finish())
	parent, dest := sandbox(t)
	if err := extractBytes(t, "early-finish-1.0-0.conda", conda, dest, Limits{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "payload.bin")); string(b) != "complete" {
		t.Fatalf("payload.bin: %q", b)
	}
	assertNothingLeft(t, parent, "dest")
	tail := append(bytes.Clone(conda), bytes.Repeat([]byte{0xa5}, 16*128*1024)...)
	parent, dest = sandbox(t)
	assertIs(t, extractBytes(t, "early-finish-1.0-0.conda", tail, dest, Limits{}), ErrFormat)
	assertNothingLeft(t, parent)
}

// truncated_streams_return_errors_and_never_hang: every cut is an error for
// both formats. (rattler's streaming .conda reader can succeed when only the
// central directory is cut; reading the central directory first, goomba
// cannot.)
func TestTruncatedArchivesFail(t *testing.T) {
	t.Parallel()
	const chunk = 128 * 1024
	for _, file := range []string{"bzip2-1.0.8-hd590300_5.conda", "bzip2-1.0.8-h7f98852_4.tar.bz2"} {
		data := readFixture(t, file)
		n := len(data)
		for _, cutoff := range []int{0, 1, chunk - 1, chunk, chunk + 1, n / 2, n - 1} {
			if cutoff >= n {
				continue
			}
			parent, dest := sandbox(t)
			if err := extractBytes(t, file, data[:cutoff], dest, Limits{}); err == nil {
				t.Errorf("%s cut at %d/%d extracted without error", file, cutoff, n)
			}
			assertNothingLeft(t, parent)
		}
	}
}

// reader_error_is_reported_instead_of_truncation_error.
func TestReaderErrorIsReported(t *testing.T) {
	tarZst := zstdCompress(bz2Decompress(t, readFixture(t, "bzip2-1.0.8-h7f98852_4.tar.bz2")))
	errGaveUp := errors.New("reader gave up")
	for _, cutoff := range []int{10, 5000, 128 * 1024, len(tarZst) - 10} {
		parent, dest := sandbox(t)
		r := &failingReader{data: tarZst, cutoff: cutoff, err: errGaveUp}
		err := ExtractReader(context.Background(), r, TarZst, dest, Limits{})
		if !errors.Is(err, errGaveUp) {
			t.Errorf("cut at %d: expected the reader's own error, got: %v", cutoff, err)
		}
		assertNothingLeft(t, parent)
	}
}

// failingReader hands out at most 1024 bytes per read and fails with err
// after cutoff bytes.
type failingReader struct {
	data   []byte
	pos    int
	cutoff int
	err    error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= f.cutoff {
		return 0, f.err
	}
	n := min(len(p), f.cutoff-f.pos, len(f.data)-f.pos, 1024)
	if n == 0 {
		return 0, io.EOF
	}
	copy(p, f.data[f.pos:f.pos+n])
	f.pos += n
	return n, nil
}

// slowReader hands out chunk bytes every delay and counts reads.
type slowReader struct {
	data  []byte
	pos   int
	chunk int
	delay time.Duration
	reads atomic.Int64
}

func (s *slowReader) Read(p []byte) (int, error) {
	s.reads.Add(1)
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	n := copy(p[:min(len(p), s.chunk)], s.data[s.pos:])
	s.pos += n
	return n, nil
}

// cancelled_extraction_does_not_hang_runtime_shutdown: cancelling the
// context stops the extraction promptly and leaves nothing behind.
func TestCancelledExtractionStopsPromptly(t *testing.T) {
	data := readFixture(t, "bzip2-1.0.8-h7f98852_4.tar.bz2")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	parent, dest := sandbox(t)
	start := time.Now()
	r := &slowReader{data: data, chunk: 512, delay: 5 * time.Millisecond}
	err := ExtractReader(ctx, r, TarBz2, dest, Limits{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want the context's", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelling took %v", elapsed)
	}
	assertNothingLeft(t, parent)
	reads := r.reads.Load()
	time.Sleep(50 * time.Millisecond)
	if r.reads.Load() != reads {
		t.Fatal("reader still used after ExtractReader returned")
	}

	// A context cancelled before the call does nothing at all.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	parent, dest = sandbox(t)
	assertIs(t, Extract(cctx, filepath.Join("testdata", "wheel-0.46.3-pyhd8ed1ab_0.conda"), dest, Limits{}), context.Canceled)
	assertNothingLeft(t, parent)
}

// large_files_extract_intact: files around the 128 KiB chunk size and past
// 1 MiB, through both formats.
func TestLargeFilesExtractIntact(t *testing.T) {
	t.Parallel()
	const chunk = 128 * 1024
	files := []struct {
		name string
		data []byte
	}{
		{"minus_one_128k.bin", noise(chunk-1, 4)},
		{"exact_128k.bin", noise(chunk, 2)},
		{"plus_one_128k.bin", noise(chunk+1, 3)},
		{"one_mib_plus.bin", noise(1024*1024+7, 6)},
	}
	r := &rawTar{}
	for _, f := range files {
		r.file(f.name, f.data)
	}
	pkgTar := r.finish()
	for _, in := range []struct {
		name string
		data []byte
	}{{"big.tar.bz2", bz2Compress(pkgTar)}, {"big-1.0-0.conda", buildConda("big", pkgTar)}} {
		_, dest := sandbox(t)
		if err := extractBytes(t, in.name, in.data, dest, Limits{}); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			got, err := os.ReadFile(filepath.Join(dest, f.name))
			if err != nil {
				t.Fatalf("%s: missing %s: %v", in.name, f.name, err)
			}
			if !bytes.Equal(got, f.data) {
				t.Fatalf("%s: contents of %s differ", in.name, f.name)
			}
		}
	}
}

// symlinkHeader is a symlink entry for rawTar.pax, for targets too long for
// a raw header.
func symlinkHeader(name, target string) *tar.Header {
	return &tar.Header{Typeflag: '2', Name: name, Linkname: target, Mode: 0o777}
}

func (t *rawTar) longLink(name, target string) *rawTar {
	return t.pax(&tar.Header{Typeflag: '1', Name: name, Linkname: target, Mode: 0o644}, nil)
}

func (t *rawTar) longFileMtime(name string, data []byte, mtime int64) *rawTar {
	return t.pax(&tar.Header{Typeflag: '0', Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(mtime, 0)}, data)
}
