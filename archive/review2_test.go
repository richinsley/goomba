package archive

// Regression tests from the second WP3 review.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// toZip64 rewrites the end of a small zip (no comment) to use a zip64
// end-of-central-directory record and locator; gap bytes are inserted
// between the central directory and the zip64 record, and gap2 between the
// zip64 record and its locator.
func toZip64(z []byte, gap, gap2 int) []byte {
	eocd := len(z) - 22
	entries := binary.LittleEndian.Uint16(z[eocd+10:])
	cdSize := binary.LittleEndian.Uint32(z[eocd+12:])
	cdOff := binary.LittleEndian.Uint32(z[eocd+16:])
	out := bytes.Clone(z[:eocd])
	out = append(out, bytes.Repeat([]byte{0xee}, gap)...)
	z64 := len(out)
	rec := make([]byte, 56)
	binary.LittleEndian.PutUint32(rec[0:], 0x06064b50)
	binary.LittleEndian.PutUint64(rec[4:], 44)
	binary.LittleEndian.PutUint16(rec[12:], 45)
	binary.LittleEndian.PutUint16(rec[14:], 45)
	binary.LittleEndian.PutUint64(rec[24:], uint64(entries))
	binary.LittleEndian.PutUint64(rec[32:], uint64(entries))
	binary.LittleEndian.PutUint64(rec[40:], uint64(cdSize))
	binary.LittleEndian.PutUint64(rec[48:], uint64(cdOff))
	out = append(out, rec...)
	out = append(out, bytes.Repeat([]byte{0xee}, gap2)...)
	loc := make([]byte, 20)
	binary.LittleEndian.PutUint32(loc[0:], 0x07064b50)
	binary.LittleEndian.PutUint64(loc[8:], uint64(z64))
	binary.LittleEndian.PutUint32(loc[16:], 1)
	out = append(out, loc...)
	end := bytes.Clone(z[eocd:])
	binary.LittleEndian.PutUint16(end[8:], 0xffff)
	binary.LittleEndian.PutUint16(end[10:], 0xffff)
	binary.LittleEndian.PutUint32(end[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(end[16:], 0xffffffff)
	return append(out, end...)
}

func TestZipSmuggling(t *testing.T) {
	m, p, i := condaParts()
	z := buildZip(m, p, i)
	// Garbage after the real end record, followed by a copy of it.
	eocd := bytes.Clone(z[len(z)-22:])
	smug := append(append(bytes.Clone(z), bytes.Repeat([]byte{0xa5}, 5000)...), eocd...)
	// Prepended bytes behind a fake local header whose name spans them and
	// the real first local header, so its data offset matches.
	n := 1000 + int(binary.LittleEndian.Uint16(z[26:])) + int(binary.LittleEndian.Uint16(z[28:]))
	fake := make([]byte, 30)
	copy(fake, "PK\x03\x04")
	binary.LittleEndian.PutUint16(fake[26:], uint16(n))
	smug2 := append(append(fake, bytes.Repeat([]byte{0x5a}, 1000-30)...), z...)
	cases := map[string][]byte{
		"trailing garbage + copied end record": smug,
		"prepended behind a fake local header": smug2,
		"zip64 with a gap before its record":   toZip64(z, 100, 0),
		"zip64 with a gap before its locator":  toZip64(z, 0, 100),
	}
	for name, data := range cases {
		parent, dest := sandbox(t)
		if err := extractBytes(t, "x-1.0-0.conda", data, dest, Limits{}); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v, want ErrFormat", name, err)
		}
		assertNothingLeft(t, parent)
	}
	// A well-formed zip64 layout is accepted.
	_, dest := sandbox(t)
	if err := extractBytes(t, "x-1.0-0.conda", toZip64(z, 0, 0), dest, Limits{}); err != nil {
		t.Fatalf("zip64: %v", err)
	}
	if got := snapshot(t, dest); got["ok.txt"].Type != "file" {
		t.Fatalf("zip64 tree %v", got)
	}
}

// symlinkChain builds a 127-deep directory and a chain of 40 links whose
// 255-component targets climb down and back up before naming the next
// link, plus extra links into the chain.
func symlinkChain(extra int) []byte {
	const k = 127
	down := strings.TrimSuffix(strings.Repeat("d/", k), "/")
	up := strings.TrimSuffix(strings.Repeat("../", k), "/")
	tb := &rawTar{}
	tb.pax(&tar.Header{Typeflag: tar.TypeDir, Name: down + "/", Mode: 0o755}, nil)
	for i := 1; i <= 40; i++ {
		next := fmt.Sprintf("s%d", i+1)
		if i == 40 {
			next = "x"
		}
		tb.pax(&tar.Header{Typeflag: tar.TypeSymlink, Name: fmt.Sprintf("s%d", i), Linkname: down + "/" + up + "/" + next, Mode: 0o777}, nil)
	}
	for j := range extra {
		tb.symlink(fmt.Sprintf("t%d", j), "s2")
	}
	return zstdCompress(tb.finish())
}

// Symlink chains used to cost seconds per link; each link is now resolved
// once, and a step budget bounds the rest.
func TestSymlinkChainCost(t *testing.T) {
	for _, n := range []int{0, 40, 400} {
		parent, dest := sandbox(t)
		start := time.Now()
		err := ExtractReader(context.Background(), bytes.NewReader(symlinkChain(n)), TarZst, dest, Limits{})
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%d extra links: took %v", n, d)
		}
		if err != nil {
			if !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrLimit) {
				t.Errorf("%d extra links: %v", n, err)
			}
			assertNothingLeft(t, parent)
		}
	}
	parent, dest := sandbox(t)
	err := ExtractReader(context.Background(), bytes.NewReader(symlinkChain(0)), TarZst, dest, Limits{MaxSymlinkSteps: 1000})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("MaxSymlinkSteps 1000: %v, want ErrLimit", err)
	}
	assertNothingLeft(t, parent)
}

func TestSymlinkResolutionMemoised(t *testing.T) {
	// Results are reused: a link through a chain, a loop, and hop counts
	// summed over nested links.
	cases := []struct {
		name string
		tar  []byte
		want error
	}{
		{"chain then reuse", (&rawTar{}).dir("a/b/").symlink("a/l1", "b").symlink("l2", "a/l1/../l1").symlink("l3", "l2/c").symlink("l4", "l3/../..").finish(), nil},
		{"reuse escaping", (&rawTar{}).dir("a/").symlink("a/up", "..").symlink("x", "a/up").symlink("y", "x/..").finish(), ErrUnsafePath},
		{"loop of three", (&rawTar{}).symlink("a", "b").symlink("b", "c").symlink("c", "a").finish(), ErrUnsafePath},
	}
	for _, c := range cases {
		parent, dest := sandbox(t)
		err := ExtractReader(context.Background(), bytes.NewReader(zstdCompress(c.tar)), TarZst, dest, Limits{})
		if c.want == nil && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.want != nil {
			if !errors.Is(err, c.want) {
				t.Errorf("%s: %v, want %v", c.name, err, c.want)
			}
			assertNothingLeft(t, parent)
		}
	}
	// 41 links in a row exceed the hop limit.
	tb := &rawTar{}
	for i := range 41 {
		tb.symlink(fmt.Sprintf("l%d", i), fmt.Sprintf("l%d", i+1))
	}
	parent, dest := sandbox(t)
	if err := ExtractReader(context.Background(), bytes.NewReader(zstdCompress(tb.finish())), TarZst, dest, Limits{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("41-link chain: %v", err)
	}
	assertNothingLeft(t, parent)
}

// Many entries each creating a fresh 256-deep directory chain are created
// relative to cached directory handles, not from the root each time.
func TestDeepChainsCost(t *testing.T) {
	tb := &rawTar{}
	deep := strings.TrimSuffix(strings.Repeat("x/", 254), "/")
	for i := range 100 {
		tb.longFile(fmt.Sprintf("p%d/%s/f", i, deep), nil)
	}
	_, dest := sandbox(t)
	start := time.Now()
	if err := ExtractReader(context.Background(), bytes.NewReader(zstdCompress(tb.finish())), TarZst, dest, Limits{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("took %v", d)
	}
}

// Swap the temporary directory for a symlink as early as possible: nothing
// may ever be written to the symlink's target.
func TestEarlySwapRace(t *testing.T) {
	outside := t.TempDir()
	data := zstdCompress((&rawTar{}).file("a", []byte("a")).symlink("l", "a").finish())
	swaps := 0
	for range 300 {
		parent, dest := sandbox(t)
		var stop atomic.Bool
		swapped := make(chan bool, 1)
		go func() {
			for !stop.Load() {
				des, _ := os.ReadDir(parent)
				for _, d := range des {
					if strings.HasPrefix(d.Name(), ".dest.tmp-") && d.IsDir() {
						tmp := filepath.Join(parent, d.Name())
						if os.Rename(tmp, tmp+"-moved") == nil {
							os.Symlink(outside, tmp)
							swapped <- true
							return
						}
					}
				}
			}
			swapped <- false
		}()
		err := ExtractReader(context.Background(), bytes.NewReader(data), TarZst, dest, Limits{})
		stop.Store(true)
		if <-swapped {
			swaps++
			if err == nil {
				t.Fatal("extraction succeeded although its directory was swapped")
			}
		}
		if des, _ := os.ReadDir(outside); len(des) > 0 {
			t.Fatalf("wrote into the outside directory: %d entries (err=%v)", len(des), err)
		}
		if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			t.Fatal("destination is a symlink")
		}
	}
	t.Logf("%d of 300 runs swapped the directory", swaps)
}

// A same-user process plants a symlink inside the private temporary
// directory during extraction; the final tree check refuses it.
func TestPlantInsideTemp(t *testing.T) {
	parent, dest := sandbox(t)
	head := (&rawTar{}).file("a", []byte("a")).buf.Bytes()
	rest := (&rawTar{}).file("b", []byte("b")).finish()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- ExtractReader(context.Background(), pr, TarZst, dest, Limits{}) }()
	pw.Write(zstdCompress(head))
	var tmp string
	for deadline := time.Now().Add(5 * time.Second); tmp == "" && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if m, _ := filepath.Glob(filepath.Join(parent, ".dest.tmp-*", "a")); len(m) == 1 {
			tmp = filepath.Dir(m[0])
		}
	}
	if tmp == "" {
		t.Fatal("temporary directory not found")
	}
	if err := os.Symlink("/", filepath.Join(tmp, "planted")); err != nil {
		t.Skip(err)
	}
	pw.Write(zstdCompress(rest))
	pw.Close()
	if err := <-done; !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("error %v, want ErrUnsafePath", err)
	}
	assertNothingLeft(t, parent)
}
