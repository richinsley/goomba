package fetch

// Regression tests from the second WP3 review.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

// Repro A with honest records: while Fetch and Extracted of B loop, A is
// re-fetched and re-extracted; A's tree never holds B.
func TestReproAHonestRecords(t *testing.T) {
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	b := buildConda(t, map[string]string{"from-B.txt": "B"})
	const fn = "foo-1.0-0.conda"
	urlA := s.put("/chanA/"+fn, a)
	urlB := s.put("/chanB/"+fn, b)
	c := newCache(t, s)
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c.Fetch(ctx, urlB, sha256Hex(b), int64(len(b)))
				c.Extracted(ctx, fooRecord(urlB, b))
				os.RemoveAll(treePath(c, sha256Hex(b)))
			}
		}()
	}
	for i := range 150 {
		os.RemoveAll(treePath(c, sha256Hex(a)))
		os.Remove(archivePath(c, sha256Hex(a), fn))
		p, err := c.Extracted(ctx, fooRecord(urlA, a))
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if _, err := os.Lstat(filepath.Join(p, "from-B.txt")); err == nil {
			t.Fatalf("iteration %d: A's tree holds B", i)
		}
	}
	close(stop)
	wg.Wait()
}

// Repro B: a foreign writer renames B over A's archive path, or scribbles
// B's bytes into A's archive in place. Extracted may fail (closed) but never
// produces a tree with B's content.
func TestReproBForeignWriters(t *testing.T) {
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	b := buildConda(t, map[string]string{"from-B.txt": "B"})
	const fn = "foo-1.0-0.conda"
	url := s.put("/chanA/"+fn, a)
	for _, mode := range []string{"rename", "inplace"} {
		c := newCache(t, s)
		os.MkdirAll(filepath.Join(c.Dir, sha256Hex(a)), 0o755)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ap := archivePath(c, sha256Hex(a), fn)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if mode == "rename" {
					tmp := filepath.Join(c.Dir, fmt.Sprintf("other-%d", i))
					os.WriteFile(tmp, b, 0o644)
					os.Rename(tmp, ap)
				} else if f, err := os.OpenFile(ap, os.O_WRONLY, 0); err == nil {
					f.WriteAt(b[:min(len(a), len(b))], 0)
					f.Close()
				}
				time.Sleep(50 * time.Microsecond)
			}
		}()
		failed := 0
		for range 100 {
			os.RemoveAll(treePath(c, sha256Hex(a)))
			p, err := c.Extracted(context.Background(), fooRecord(url, a))
			if err != nil {
				if !errors.Is(err, ErrUnsafeCache) && !errors.Is(err, context.Canceled) {
					var ie *IntegrityError
					if !errors.As(err, &ie) {
						t.Errorf("%s: unexpected error %v", mode, err)
					}
				}
				failed++
				continue
			}
			if _, err := os.Lstat(filepath.Join(p, "from-B.txt")); err == nil {
				t.Fatalf("%s: tree holds B's content", mode)
			}
		}
		close(stop)
		wg.Wait()
		t.Logf("%s: %d of 100 failed closed", mode, failed)
	}
}

// A same-user rewrite between verification and extraction is compared
// against the verified state, not the state at extraction start.
func TestStampGap(t *testing.T) {
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	b := buildConda(t, map[string]string{"from-B.txt": "B"})
	const fn = "foo-1.0-0.conda"
	url := s.put("/c/"+fn, a)
	c := newCache(t, s)
	u, _, err := c.checkFetch(url, sha256Hex(a), 0)
	if err != nil {
		t.Fatal(err)
	}
	err = c.withLock(context.Background(), sha256Hex(a), func(dir string, key []byte) error {
		f, err := c.openArchive(context.Background(), u, dir, fn, sha256Hex(a), 0, key)
		if err != nil {
			return err
		}
		defer f.Close()
		w, _ := os.OpenFile(filepath.Join(dir, fn), os.O_WRONLY|os.O_TRUNC, 0)
		w.Write(b)
		w.Close()
		return c.extract(context.Background(), f, fn, dir, key, sha256Hex(a))
	})
	if !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("error %v, want ErrUnsafeCache", err)
	}
	if _, err := os.Lstat(treePath(c, sha256Hex(a))); err == nil {
		t.Fatal("tree published")
	}
}

// The key file is swapped between a good 0600 key and a 0644 one; the
// 0644 key must never be accepted.
func TestKeyReadIsAtomic(t *testing.T) {
	if !fileIDsSupported() {
		t.Skip("no Unix permissions")
	}
	dir := t.TempDir()
	good, bad, p := filepath.Join(dir, "good"), filepath.Join(dir, "bad"), filepath.Join(dir, keyName)
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			os.WriteFile(good, []byte(strings.Repeat("1", 64)+"\n"), 0o600)
			os.Chmod(good, 0o600)
			os.Rename(good, p)
			os.WriteFile(bad, []byte(strings.Repeat("b", 64)+"\n"), 0o644)
			os.Chmod(bad, 0o644)
			os.Rename(bad, p)
		}
	}()
	for range 50000 {
		if k, err := readKey(p); err == nil && k[0] == 0xbb {
			stop.Store(true)
			wg.Wait()
			t.Fatal("a 0644 key was accepted")
		}
	}
	stop.Store(true)
	wg.Wait()
}

// Entries planted where the content directory, its tree or its archive
// belong are replaced, and nothing outside the cache is touched.
func TestPlantedContentEntries(t *testing.T) {
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	const fn = "foo-1.0-0.conda"
	url := s.put("/c/"+fn, a)
	sha := sha256Hex(a)
	for _, kind := range []string{"pkg-symlink", "pkg-readonly", "dir-symlink", "archive-dir", "marker-from-other-sha"} {
		t.Run(kind, func(t *testing.T) {
			outside := t.TempDir()
			os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("k"), 0o644)
			c := newCache(t, s)
			cd := filepath.Join(c.Dir, sha)
			switch kind {
			case "pkg-symlink":
				os.MkdirAll(cd, 0o755)
				os.Symlink(outside, filepath.Join(cd, treeName))
			case "pkg-readonly":
				os.MkdirAll(filepath.Join(cd, treeName, "ro", "sub"), 0o755)
				os.WriteFile(filepath.Join(cd, treeName, "ro", "sub", "f"), []byte("x"), 0o444)
				os.Chmod(filepath.Join(cd, treeName, "ro", "sub"), 0o555)
				os.Chmod(filepath.Join(cd, treeName, "ro"), 0o555)
			case "dir-symlink":
				os.MkdirAll(c.Dir, 0o755)
				os.Symlink(outside, cd)
			case "archive-dir":
				os.MkdirAll(filepath.Join(cd, fn, "x"), 0o755)
			case "marker-from-other-sha":
				other := buildConda(t, map[string]string{"other.txt": "O"})
				op, err := c.Extracted(context.Background(), fooRecord(s.put("/c2/"+fn, other), other))
				if err != nil {
					t.Fatal(err)
				}
				os.MkdirAll(cd, 0o755)
				if err := os.Rename(op, filepath.Join(cd, treeName)); err != nil {
					t.Fatal(err)
				}
			}
			p, err := c.Extracted(context.Background(), fooRecord(url, a))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(p, "from-A.txt")); err != nil {
				t.Fatal("tree lacks A")
			}
			if got := namesOrNil(outside); len(got) != 1 || got[0] != "keep.txt" {
				t.Fatalf("outside directory holds %v", got)
			}
			if fi, err := os.Lstat(cd); err != nil || !fi.IsDir() {
				t.Fatalf("content directory %v", err)
			}
			assertClean(t, c.Dir)
		})
	}
}

// A process holding the old key rereads it when a marker does not verify,
// instead of replacing a tree another process extracted under the new key.
func TestKeyRecreatedNoFight(t *testing.T) {
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	url := s.put("/c/foo-1.0-0.conda", a)
	c1 := newCache(t, s)
	if _, err := c1.Extracted(context.Background(), fooRecord(url, a)); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(c1.Dir, keyName))
	c2 := &Cache{Dir: c1.Dir, Client: c1.Client, AllowHTTP: true}
	p2, err := c2.Extracted(context.Background(), fooRecord(url, a))
	if err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(filepath.Join(p2, "from-A.txt"))
	p3, err := c1.Extracted(context.Background(), fooRecord(url, a))
	if err != nil {
		t.Fatal(err)
	}
	fi3, _ := os.Stat(filepath.Join(p3, "from-A.txt"))
	if !os.SameFile(fi2, fi3) {
		t.Fatal("the first cache replaced the tree extracted under the new key")
	}
	// Sidecars are reread the same way: no new download.
	before := s.hits.Load()
	if _, err := c1.Fetch(context.Background(), url, sha256Hex(a), 0); err != nil || s.hits.Load() != before {
		t.Fatalf("Fetch after key change: %v, %d downloads", err, s.hits.Load()-before)
	}
}

// TestChildMixed runs as a child process of TestMultiProcessMixed.
func TestChildMixed(t *testing.T) {
	dir := os.Getenv("GOOMBA_TEST_MIXED_DIR")
	if dir == "" {
		t.Skip("child process only")
	}
	c := &Cache{Dir: dir, AllowHTTP: true}
	sha, url := os.Getenv("GOOMBA_TEST_MIXED_SHA"), os.Getenv("GOOMBA_TEST_MIXED_URL")
	for range 20 {
		if os.Getenv("GOOMBA_TEST_MIXED_MODE") == "fetch" {
			p, err := c.Fetch(context.Background(), url, sha, 0)
			if err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(p); sha256Hex(b) != sha {
				t.Fatal("fetched path holds other bytes")
			}
		} else if _, err := c.Extracted(context.Background(), &record.PackageRecord{Name: "foo", Version: "1.0", Build: "0", SHA256: sha, URL: url}); err != nil {
			t.Fatal(err)
		}
	}
}

// Ten processes, half fetching and half extracting one digest, 20 times
// each: one download, nothing left behind.
func TestMultiProcessMixed(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	url := s.put("/c/foo-1.0-0.conda", a)
	dir := t.TempDir()
	var wg sync.WaitGroup
	outs := make([]string, 10)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mode := "fetch"
			if i%2 == 1 {
				mode = "extract"
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestChildMixed$", "-test.count=1")
			cmd.Env = append(os.Environ(), "GOOMBA_TEST_MIXED_DIR="+dir, "GOOMBA_TEST_MIXED_SHA="+sha256Hex(a), "GOOMBA_TEST_MIXED_URL="+url, "GOOMBA_TEST_MIXED_MODE="+mode)
			if out, err := cmd.CombinedOutput(); err != nil {
				outs[i] = fmt.Sprintf("%v: %s", err, out)
			}
		}()
	}
	wg.Wait()
	for i, o := range outs {
		if o != "" {
			t.Errorf("child %d: %s", i, o)
		}
	}
	if n := s.hits.Load(); n != 1 {
		t.Errorf("%d downloads, want 1", n)
	}
	assertClean(t, dir)
}
