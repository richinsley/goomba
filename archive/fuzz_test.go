package archive

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fuzzLimits keep every limit reachable by a fuzzer while letting the small
// real fixtures extract.
var fuzzLimits = Limits{MaxFileSize: 1 << 20, MaxTotalSize: 4 << 20, MaxEntries: 512}

// fuzzSeedTars are the uncompressed tars every fuzz target is seeded with.
func fuzzSeedTars() [][]byte {
	seeds := [][]byte{
		(&rawTar{}).finish(),
		(&rawTar{}).file("a.txt", []byte("hello")).dir("d/").file("d/b", []byte("b")).
			symlink("l", "a.txt").hardlink("h", "a.txt").finish(),
		(&rawTar{}).oldDir("old/").longFile(strings.Repeat("n", 150), []byte("long")).finish(),
		(&rawTar{}).symlink("lib/up", "..").symlink("bin/x", "../lib/up/a").finish(),
	}
	for _, h := range hostileTars("/outside") {
		seeds = append(seeds, h.tar)
	}
	return seeds
}

// checkFuzzResult verifies the invariants of one extraction attempt: no
// panic (implicit), nothing written beside the destination, nothing left
// on error, and on success a tree within the limits whose symlinks stay
// inside.
func checkFuzzResult(t testing.TB, root, dest string, err error, inputs ...string) {
	t.Helper()
	want := append([]string{"out"}, inputs...)
	got := entries(t, root)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("files beside the destination: %q", got)
	}
	out := entries(t, filepath.Join(root, "out"))
	if err != nil {
		if len(out) != 0 {
			t.Fatalf("error %v left %q behind", err, out)
		}
		return
	}
	if !slices.Equal(out, []string{"dest"}) {
		t.Fatalf("destination parent holds %q", out)
	}
	realDest, rerr := filepath.EvalSymlinks(dest)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var nodes int
	var total int64
	werr := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dest {
			return nil
		}
		nodes++
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode().IsRegular():
			if fi.Size() > fuzzLimits.MaxFileSize {
				t.Errorf("%s: %d bytes over MaxFileSize", p, fi.Size())
			}
			total += fi.Size()
		case fi.IsDir():
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				t.Errorf("%s: absolute target %q", p, target)
			}
			if real, err := filepath.EvalSymlinks(p); err == nil {
				if !within(realDest, real) {
					t.Errorf("%s resolves to %s, outside %s", p, real, realDest)
				}
			} else if !stepsInside(realDest, filepath.Dir(p), target, 0) {
				t.Errorf("dangling %s -> %q leaves the destination", p, target)
			}
		default:
			t.Errorf("%s: special file %v", p, fi.Mode())
		}
		return nil
	})
	if werr != nil {
		t.Fatal(werr)
	}
	if total > fuzzLimits.MaxTotalSize {
		t.Errorf("extracted %d bytes, over MaxTotalSize", total)
	}
	if nodes > fuzzLimits.MaxEntries {
		t.Errorf("extracted %d paths, over MaxEntries", nodes)
	}
}

// stepsInside resolves target from the real directory dir one component
// at a time, following symlinks and treating missing components as plain
// names, and reports whether every step stays within root. It is an
// independent re-implementation of the extractor's final symlink check.
func stepsInside(root, dir, target string, depth int) bool {
	if depth > 40 || filepath.IsAbs(target) {
		return false
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for _, c := range strings.Split(filepath.ToSlash(target), "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			dir = filepath.Dir(dir)
		default:
			next := filepath.Join(dir, c)
			fi, err := os.Lstat(next)
			if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				t, err := os.Readlink(next)
				if err != nil || !stepsInside(root, dir, t, depth+1) {
					return false
				}
				// Continue from where the link led, if it exists.
				if real, err := filepath.EvalSymlinks(next); err == nil {
					dir = real
					continue
				}
				// A dangling link: the rest cannot be resolved for real.
				return true
			}
			dir = next
		}
		if !within(root, dir) {
			return false
		}
	}
	return true
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// fuzzExtractFile writes data as name in a fresh root and extracts it into
// root/out/dest.
func fuzzExtractFile(t testing.TB, name string, data []byte) {
	root := t.TempDir()
	in := filepath.Join(root, name)
	if err := os.WriteFile(in, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "out", "dest")
	err := Extract(context.Background(), in, dest, fuzzLimits)
	if err != nil && errors.Is(err, fs.ErrExist) {
		t.Fatalf("unexpected: %v", err)
	}
	checkFuzzResult(t, root, dest, err, name)
}

func FuzzExtractConda(f *testing.F) {
	for _, tarball := range fuzzSeedTars() {
		f.Add(buildConda("x", tarball))
	}
	for _, name := range []string{"wheel-0.46.3-pyhd8ed1ab_0.conda", "libzlib-1.3.2-h25fd6f3_2.conda"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzExtractFile(t, "x-1.0-0.conda", data)
	})
}

func FuzzExtractTarBz2(f *testing.F) {
	for _, tarball := range fuzzSeedTars() {
		f.Add(bz2Compress(tarball))
	}
	f.Add([]byte("BZh9"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzExtractFile(t, "x.tar.bz2", data)
	})
}

func FuzzExtractTarZst(f *testing.F) {
	for _, tarball := range fuzzSeedTars() {
		f.Add(zstdCompress(tarball))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "out"), 0o755); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(root, "out", "dest")
		err := ExtractReader(context.Background(), bytes.NewReader(data), TarZst, dest, fuzzLimits)
		checkFuzzResult(t, root, dest, err)
	})
}

func FuzzCleanName(f *testing.F) {
	for _, s := range []string{"a/b", "./a", "../a", `a\..\b`, "/abs", "C:x", "a//b/", "\x00", ".", "a/./b/.."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		rel, err := cleanName(name, defaultLimits)
		if err != nil {
			if !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrLimit) {
				t.Fatalf("cleanName(%q): %v is not ErrUnsafePath or ErrLimit", name, err)
			}
			return
		}
		if strings.IndexByte(rel, 0) >= 0 || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) || hasDrive(rel) {
			t.Fatalf("cleanName(%q) = %q is unsafe", name, rel)
		}
		if rel == "" {
			return // names the destination itself
		}
		for p := range strings.SplitSeq(rel, "/") {
			if p == "" || p == "." || p == ".." {
				t.Fatalf("cleanName(%q) = %q has component %q", name, rel, p)
			}
			for q := range strings.SplitSeq(p, `\`) {
				if q == ".." {
					t.Fatalf("cleanName(%q) = %q has a backslash .. component", name, rel)
				}
			}
		}
		if again, err := cleanName(rel, defaultLimits); err != nil || again != rel {
			t.Fatalf("cleanName not idempotent on %q: %q, %v", rel, again, err)
		}
		if filepath.IsAbs(filepath.FromSlash(rel)) {
			t.Fatalf("cleanName(%q) = %q is absolute on this host", name, rel)
		}
	})
}
