package fetch

// Regression tests from the WP3 review probes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

func fooRecord(url string, data []byte) *record.PackageRecord {
	return &record.PackageRecord{Name: "foo", Version: "1.0", Build: "0", SHA256: sha256Hex(data), Size: int64(len(data)), URL: url}
}

// Two channels publish different archives under one file name. Extracted
// of A runs while Fetch of B loops; A's tree must never hold B's content.
func TestSameFileNameDifferentDigests(t *testing.T) {
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
				if _, err := c.Fetch(ctx, urlB, sha256Hex(b), int64(len(b))); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := range 200 {
		if i%10 == 0 {
			os.RemoveAll(treePath(c, sha256Hex(a)))
		}
		p, err := c.Extracted(ctx, fooRecord(urlA, a))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(p, "from-B.txt")); err == nil {
			t.Fatalf("iteration %d: %s holds B's content", i, p)
		}
	}
	close(stop)
	wg.Wait()
	// A record whose dist differs from its URL's file name is refused.
	rec := fooRecord(urlA, a)
	rec.Build = "b1"
	if _, err := c.Extracted(ctx, rec); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mismatched record: %v", err)
	}
}

// A path Fetch returned keeps its bytes when the same file name is later
// fetched with another digest.
func TestFetchPathIsStable(t *testing.T) {
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	b := buildConda(t, map[string]string{"from-B.txt": "B"})
	const fn = "foo-1.0-0.conda"
	c := newCache(t, s)
	ctx := context.Background()
	pa, err := c.Fetch(ctx, s.put("/chanA/"+fn, a), sha256Hex(a), 0)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := c.Fetch(ctx, s.put("/chanB/"+fn, b), sha256Hex(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	if pa == pb {
		t.Fatal("two digests share one path")
	}
	if got, _ := os.ReadFile(pa); sha256Hex(got) != sha256Hex(a) {
		t.Fatal("path returned for A now holds other bytes")
	}
	ta, err := c.Extracted(ctx, fooRecord(s.URL+"/chanA/"+fn, a))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Extracted(ctx, fooRecord(s.URL+"/chanB/"+fn, b)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ta, "from-A.txt")); err != nil {
		t.Fatal("tree returned for A changed")
	}
}

// A process that can write the cache but not read the key plants an
// archive and a sidecar for the good digest; the sidecar's MAC fails, the
// archive is re-hashed, found wrong and downloaded again.
func TestForgedSidecarIsNotTrusted(t *testing.T) {
	s := newServer(t, false)
	good := buildConda(t, map[string]string{"good.txt": "good"})
	evil := buildConda(t, map[string]string{"evil.txt": "evil"})
	const fn = "foo-1.0-0.conda"
	url := s.put("/c/"+fn, good)
	c := newCache(t, s)
	if _, err := c.markerKey(); err != nil {
		t.Fatal(err)
	}
	final := archivePath(c, sha256Hex(good), fn)
	os.MkdirAll(filepath.Dir(final), 0o755)
	if err := os.WriteFile(final, evil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(final)
	st, _ := stampOf(f)
	f.Close()
	forged := sidecarContent(make([]byte, 32), fn, sha256Hex(good), st) // wrong key
	os.WriteFile(final+".sha256", []byte(forged), 0o644)
	p, err := c.Extracted(context.Background(), fooRecord(url, good))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p, "evil.txt")); err == nil {
		t.Fatal("planted archive was extracted")
	}
	if s.hits.Load() != 1 {
		t.Fatalf("%d downloads, want 1", s.hits.Load())
	}
}

func TestLockFilesNeverFollowSymlinks(t *testing.T) {
	s := newServer(t, false)
	data := buildConda(t, map[string]string{"x": "x"})
	const fn = "foo-1.0-0.conda"
	url := s.put("/c/"+fn, data)
	ctx := context.Background()

	c := newCache(t, s)
	outside := t.TempDir()
	target := filepath.Join(outside, "created-by-goomba")
	os.MkdirAll(filepath.Join(c.Dir, ".locks"), 0o755)
	if err := os.Symlink(target, filepath.Join(c.Dir, ".locks", sha256Hex(data)+".lock")); err != nil {
		t.Skip(err)
	}
	if _, err := c.Fetch(ctx, url, sha256Hex(data), 0); !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("symlinked lock file: %v", err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("a file was created outside the cache")
	}

	c = newCache(t, s)
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(c.Dir, ".locks")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(ctx, url, sha256Hex(data), 0); !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("symlinked lock directory: %v", err)
	}
	if len(namesOrNil(other)) != 0 {
		t.Fatal("lock created through a symlinked directory")
	}

	c = newCache(t, s)
	os.MkdirAll(filepath.Join(c.Dir, ".locks", sha256Hex(data)+".lock"), 0o755)
	if _, err := c.Fetch(ctx, url, sha256Hex(data), 0); err == nil {
		t.Fatal("a directory was used as a lock file")
	}
}

func TestKeyFileOwnershipAndMode(t *testing.T) {
	if !fileIDsSupported() {
		t.Skip("no Unix permissions")
	}
	dir := t.TempDir()
	if _, err := (&Cache{Dir: dir}).markerKey(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, keyName)
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o644} {
		os.Chmod(p, mode)
		if _, err := (&Cache{Dir: dir}).markerKey(); !errors.Is(err, ErrUnsafeCache) {
			t.Errorf("key with mode %v: %v", mode, err)
		}
	}
	os.Chmod(p, 0o600)
	if _, err := (&Cache{Dir: dir}).markerKey(); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	if err := os.Symlink(filepath.Join(t.TempDir(), "k"), p); err == nil {
		if _, err := (&Cache{Dir: dir}).markerKey(); err == nil {
			t.Error("symlinked key accepted")
		}
	}
}

// Anything planted at a content-addressed archive or tree path is replaced
// rather than failing every later call.
func TestPlantedEntriesAreReplaced(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	for name, plant := range map[string]func(p string){
		"directory": func(p string) { os.MkdirAll(filepath.Join(p, "sub"), 0o755) },
		"symlink":   func(p string) { os.Symlink("/", p) },
		"fifo-like": func(p string) { os.WriteFile(p, nil, 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			c := newCache(t, s)
			os.MkdirAll(filepath.Join(c.Dir, wheelSHA), 0o755)
			plant(archivePath(c, wheelSHA, wheelFn))
			if _, err := c.Fetch(context.Background(), url, wheelSHA, wheelSize); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Extracted(context.Background(), wheelRecord(url)); err != nil {
				t.Fatal(err)
			}
			assertClean(t, c.Dir)
		})
	}
	// A file or symlink planted where the content directory belongs.
	c := newCache(t, s)
	os.WriteFile(filepath.Join(c.Dir, wheelSHA), []byte("x"), 0o644)
	if _, err := c.Extracted(context.Background(), wheelRecord(url)); err != nil {
		t.Fatal(err)
	}
}

func TestRedirectDropsReferer(t *testing.T) {
	s := newServer(t, true)
	s.put("/real/"+wheelFn, wheelBytes(t))
	var referer atomic.Value
	referer.Store("unset")
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/t/secret-token/"+wheelFn {
			http.Redirect(w, r, "/real/"+wheelFn, http.StatusFound)
			return true
		}
		referer.Store(r.Header.Get("Referer"))
		return false
	}
	c := &Cache{Dir: t.TempDir(), Client: s.Client()}
	if _, err := c.Fetch(context.Background(), s.URL+"/t/secret-token/"+wheelFn, wheelSHA, wheelSize); err != nil {
		t.Fatal(err)
	}
	if got := referer.Load(); got != "" {
		t.Fatalf("redirect target received Referer %q", got)
	}
}

// Cancel Extracted at many points; nothing may be left behind.
func TestCancelEverywhere(t *testing.T) {
	s := newServer(t, false)
	data := buildConda(t, map[string]string{"x": strings.Repeat("y", 1<<20)})
	const fn = "foo-1.0-0.conda"
	url := s.put("/c/"+fn, data)
	c := newCache(t, s)
	for i := range 60 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = c.Extracted(ctx, fooRecord(url, data))
		}()
		time.Sleep(time.Duration(i) * 50 * time.Microsecond)
		cancel()
		<-done
		if i%3 == 0 {
			os.RemoveAll(treePath(c, sha256Hex(data)))
			os.Remove(archivePath(c, sha256Hex(data), fn))
		}
	}
	assertClean(t, c.Dir)
}

// Cancel a slow download and extraction after increasing delays; check for
// leftovers and leaked goroutines.
func TestCancelTimed(t *testing.T) {
	s := newServer(t, false)
	files := map[string]string{}
	for i := range 300 {
		files[fmt.Sprintf("d%d/f%d", i%9, i)] = strings.Repeat(fmt.Sprint(i), 20000)
	}
	data := buildConda(t, files)
	const fn = "foo-1.0-0.conda"
	url := s.put("/c/"+fn, data)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		for off := 0; off < len(data); off += 8192 {
			if _, err := w.Write(data[off:min(off+8192, len(data))]); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			time.Sleep(200 * time.Microsecond)
		}
		return true
	}
	c := newCache(t, s)
	g0 := runtime.NumGoroutine()
	for i := range 40 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i)*5*time.Millisecond)
		_, err := c.Extracted(ctx, fooRecord(url, data))
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("delay %d: %v", i, err)
		}
		assertClean(t, c.Dir)
		os.RemoveAll(treePath(c, sha256Hex(data)))
		if i%2 == 0 {
			os.Remove(archivePath(c, sha256Hex(data), fn))
		}
	}
	s.Client().Transport.(*http.Transport).CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > g0+5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > g0+5 {
		t.Fatalf("goroutines: %d before, %d after", g0, n)
	}
}

// Another tool renames other bytes over the content-addressed archive path
// in a loop while Extracted runs. Extracted may fail, but its tree never
// holds the other bytes.
func TestForeignWriterRace(t *testing.T) {
	s := newServer(t, false)
	a := buildConda(t, map[string]string{"from-A.txt": "A"})
	b := buildConda(t, map[string]string{"from-B.txt": "B"})
	const fn = "foo-1.0-0.conda"
	url := s.put("/chanA/"+fn, a)
	c := newCache(t, s)
	os.MkdirAll(filepath.Join(c.Dir, sha256Hex(a)), 0o755)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tmp := filepath.Join(c.Dir, fmt.Sprintf("other-tool-%d", i))
			os.WriteFile(tmp, b, 0o644)
			os.Rename(tmp, archivePath(c, sha256Hex(a), fn))
			time.Sleep(50 * time.Microsecond)
		}
	}()
	failed := 0
	for range 150 {
		os.RemoveAll(treePath(c, sha256Hex(a)))
		p, err := c.Extracted(context.Background(), fooRecord(url, a))
		if err != nil {
			failed++
			continue
		}
		if _, err := os.Lstat(filepath.Join(p, "from-B.txt")); err == nil {
			t.Fatal("tree holds the other tool's bytes")
		}
		if _, err := os.Lstat(filepath.Join(p, "from-A.txt")); err != nil {
			t.Fatal("tree lacks A's content")
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("%d of 150 extractions failed closed", failed)
}

// TestChildExtract runs as a child process of TestMultiProcess.
func TestChildExtract(t *testing.T) {
	dir := os.Getenv("GOOMBA_TEST_CHILD_DIR")
	if dir == "" {
		t.Skip("child process only")
	}
	c := &Cache{Dir: dir, AllowHTTP: true}
	rec := &record.PackageRecord{Name: "foo", Version: "1.0", Build: "0", SHA256: os.Getenv("GOOMBA_TEST_CHILD_SHA"), URL: os.Getenv("GOOMBA_TEST_CHILD_URL")}
	p, err := c.Extracted(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p, "good.txt")); err != nil {
		t.Fatal("missing good.txt")
	}
}

// Eight processes extract one package into one cache over an untrusted
// tree: one download, one consistent result, nothing left behind.
func TestMultiProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	s := newServer(t, false)
	good := buildConda(t, map[string]string{"good.txt": "good"})
	url := s.put("/c/foo-1.0-0.conda", good)
	dir := t.TempDir()
	stale := filepath.Join(dir, sha256Hex(good), treeName)
	os.MkdirAll(filepath.Join(stale, "info"), 0o755)
	os.WriteFile(filepath.Join(stale, "stale.txt"), []byte("old"), 0o644)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	outs := make([][]byte, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestChildExtract$", "-test.count=1")
			cmd.Env = append(os.Environ(), "GOOMBA_TEST_CHILD_DIR="+dir, "GOOMBA_TEST_CHILD_SHA="+sha256Hex(good), "GOOMBA_TEST_CHILD_URL="+url)
			outs[i], errs[i] = cmd.CombinedOutput()
		}()
	}
	wg.Wait()
	for i := range 8 {
		if errs[i] != nil {
			t.Errorf("child %d: %v\n%s", i, errs[i], outs[i])
		}
	}
	if n := s.hits.Load(); n != 1 {
		t.Errorf("%d downloads, want 1", n)
	}
	if _, err := os.Lstat(filepath.Join(stale, "stale.txt")); err == nil {
		t.Error("untrusted tree kept")
	}
	assertClean(t, dir)
}

// The archive is read through the verified handle; if the file behind it
// changes while extraction runs (here: its times are reset in a loop),
// the result is discarded instead of marked trusted.
func TestArchiveChangedDuringExtraction(t *testing.T) {
	if !fileIDsSupported() {
		t.Skip("no change times")
	}
	files := map[string]string{}
	for i := range 400 {
		files[fmt.Sprintf("d%d/f%d", i%9, i)] = strings.Repeat(fmt.Sprint(i), 5000)
	}
	data := buildConda(t, files)
	c := &Cache{Dir: t.TempDir()}
	key, err := c.markerKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(c.Dir, sha256Hex(data))
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "foo-1.0-0.conda")
	os.WriteFile(p, data, 0o644)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ts := time.Unix(1_600_000_000+int64(i), 0)
			os.Chtimes(p, ts, ts)
			time.Sleep(20 * time.Microsecond)
		}
	}()
	st, err := stampOf(f)
	if err != nil {
		t.Fatal(err)
	}
	err = c.extract(context.Background(), &archiveFile{File: f, verified: st}, "foo-1.0-0.conda", dir, key, sha256Hex(data))
	close(stop)
	wg.Wait()
	if !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("error %v, want ErrUnsafeCache", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, treeName)); err == nil {
		t.Fatal("tree published")
	}
	assertClean(t, c.Dir)
}
