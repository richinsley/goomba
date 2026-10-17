package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	wheelFn   = "wheel-0.46.3-pyhd8ed1ab_0.conda"
	wheelSHA  = "d6cf2f0ebd5e09120c28ecba450556ce553752652d91795442f0e70f837126ae"
	wheelSize = 31858
)

func wheelBytes(t testing.TB) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", wheelFn))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// testServer counts requests and serves files by path unless handler
// handles the request first.
type testServer struct {
	*httptest.Server
	hits    atomic.Int64
	mu      sync.Mutex
	files   map[string][]byte
	handler func(w http.ResponseWriter, r *http.Request) bool
	delay   time.Duration
}

func newServer(t testing.TB, tls bool) *testServer {
	s := &testServer{files: map[string][]byte{}}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		if s.handler != nil && s.handler(w, r) {
			return
		}
		s.mu.Lock()
		b, ok := s.files[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Write(b)
	})
	if tls {
		s.Server = httptest.NewTLSServer(h)
	} else {
		s.Server = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s
}

func (s *testServer) put(path string, b []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = b
	return s.URL + path
}

// newCache returns a Cache in a fresh directory that may use plain http to
// s and retries almost immediately.
func newCache(t testing.TB, s *testServer) *Cache {
	t.Helper()
	return &Cache{Dir: t.TempDir(), Client: s.Client(), AllowHTTP: true, backoff0: int64(time.Millisecond)}
}

// archivePath and treePath are where a cache keeps an archive and its
// extracted tree.
func archivePath(c *Cache, sha, fn string) string { return filepath.Join(c.Dir, sha, fn) }
func treePath(c *Cache, sha string) string        { return filepath.Join(c.Dir, sha, treeName) }

// assertClean fails if dir, or a content directory in it, holds a partial
// download, a temporary file or a staging directory.
func assertClean(t testing.TB, dir string) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, d := range des {
		n := d.Name()
		if strings.Contains(n, ".partial-") || strings.Contains(n, ".tmp-") || strings.Contains(n, ".staging-") || strings.Contains(n, ".old-") {
			t.Errorf("leftover %s in %s", n, dir)
		}
		if d.IsDir() && isHexDigest(n) {
			assertClean(t, filepath.Join(dir, n))
		}
	}
}

func names(t testing.TB, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

func TestFetchHappyPath(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	for range 3 {
		p, err := c.Fetch(context.Background(), url, wheelSHA, wheelSize)
		if err != nil {
			t.Fatal(err)
		}
		if p != archivePath(c, wheelSHA, wheelFn) {
			t.Fatalf("path %s", p)
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, wheelBytes(t)) {
			t.Fatalf("cached archive differs: %v", err)
		}
	}
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	b, err := os.ReadFile(archivePath(c, wheelSHA, wheelFn) + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := c.markerKey()
	if sha, st, err := parseSidecar(b, key, wheelFn); err != nil || sha != wheelSHA || st.size != wheelSize {
		t.Fatalf("sidecar %q: %v", b, err)
	}
	assertClean(t, c.Dir)
	// A fresh Cache on the same directory trusts the sidecar.
	c2 := &Cache{Dir: c.Dir, Client: s.Client(), AllowHTTP: true}
	if _, err := c2.Fetch(context.Background(), url, wheelSHA, 0); err != nil {
		t.Fatal(err)
	}
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("%d requests after reuse, want 1", n)
	}
}

func TestFetchHashMismatch(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/x/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	want := strings.Repeat("ab", 32)
	_, err := c.Fetch(context.Background(), url, want, 0)
	var ie *IntegrityError
	if !errors.As(err, &ie) {
		t.Fatalf("error %v, want *IntegrityError", err)
	}
	if ie.Expected != want || ie.Actual != wheelSHA {
		t.Fatalf("IntegrityError %+v", ie)
	}
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("a hash mismatch was retried: %d requests", n)
	}
	if _, err := os.Lstat(archivePath(c, want, wheelFn)); err == nil {
		t.Fatal("unverified archive is visible")
	}
	assertClean(t, c.Dir)
}

func TestFetchSizeMismatch(t *testing.T) {
	data := wheelBytes(t)
	cases := []struct {
		name    string
		size    int64
		handler func(w http.ResponseWriter, r *http.Request) bool
		over    bool
	}{
		{"content-length differs", wheelSize + 1, nil, false},
		{"chunked body longer", 100, func(w http.ResponseWriter, r *http.Request) bool {
			w.(http.Flusher).Flush() // no Content-Length: chunked
			w.Write(data)
			return true
		}, true},
		{"chunked body shorter", wheelSize, func(w http.ResponseWriter, r *http.Request) bool {
			w.(http.Flusher).Flush()
			w.Write(data[:1000])
			return true
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, false)
			s.handler = tc.handler
			url := s.put("/x/"+wheelFn, data)
			c := newCache(t, s)
			_, err := c.Fetch(context.Background(), url, wheelSHA, tc.size)
			var se *SizeError
			if !errors.As(err, &se) || se.Over != tc.over || se.Expected != tc.size {
				t.Fatalf("error %v, want *SizeError (over=%v)", err, tc.over)
			}
			if n := s.hits.Load(); n != 1 {
				t.Fatalf("a size mismatch was retried: %d requests", n)
			}
			assertClean(t, c.Dir)
		})
	}
}

func TestFetchTruncatedBody(t *testing.T) {
	data := wheelBytes(t)
	s := newServer(t, false)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection mid-body
	}
	url := s.put("/x/"+wheelFn, data)
	c := newCache(t, s)
	c.Retries = 2
	_, err := c.Fetch(context.Background(), url, wheelSHA, wheelSize)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error %v, want unexpected EOF", err)
	}
	if n := s.hits.Load(); n != 3 {
		t.Fatalf("%d requests, want 1 + 2 retries", n)
	}
	assertClean(t, c.Dir)
	if _, err := os.Lstat(archivePath(c, wheelSHA, wheelFn)); err == nil {
		t.Fatal("truncated archive is visible")
	}
}

func TestFetchRetries(t *testing.T) {
	cases := []struct {
		name     string
		retries  int
		statuses []int // per request; 200 serves the file
		wantHits int64
		wantErr  bool
	}{
		{"5xx then success", 0, []int{503, 500, 200}, 3, false},
		{"429 then success", 0, []int{429, 200}, 2, false},
		{"408 then success", 1, []int{408, 200}, 2, false},
		{"5xx exhausts default retries", 0, []int{502, 502, 502, 502, 502}, 4, true},
		{"retries disabled", -1, []int{503, 200}, 1, true},
		{"404 not retried", 5, []int{404, 200}, 1, true},
		{"403 not retried", 5, []int{403, 200}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, false)
			var n atomic.Int64
			s.handler = func(w http.ResponseWriter, r *http.Request) bool {
				i := int(n.Add(1)) - 1
				if code := tc.statuses[min(i, len(tc.statuses)-1)]; code != 200 {
					w.WriteHeader(code)
					return true
				}
				return false
			}
			url := s.put("/x/"+wheelFn, wheelBytes(t))
			c := newCache(t, s)
			c.Retries = tc.retries
			_, err := c.Fetch(context.Background(), url, wheelSHA, wheelSize)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v, want error %v", err, tc.wantErr)
			}
			var he *HTTPError
			if tc.wantErr && !errors.As(err, &he) {
				t.Fatalf("error %v, want *HTTPError", err)
			}
			if got := s.hits.Load(); got != tc.wantHits {
				t.Fatalf("%d requests, want %d", got, tc.wantHits)
			}
			assertClean(t, c.Dir)
		})
	}
}

func TestFetchBackoff(t *testing.T) {
	c := &Cache{}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := c.backoff(i); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i, got, w)
		}
	}
}

func TestFetchCancelMidDownload(t *testing.T) {
	data := wheelBytes(t)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s := newServer(t, false)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		return true
	}
	url := s.put("/x/"+wheelFn, data)
	c := newCache(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.Fetch(ctx, url, wheelSHA, wheelSize)
		errc <- err
	}()
	<-started
	// The partial file exists while the download runs.
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(namesOrNil(filepath.Join(c.Dir, wheelSHA)), func(n string) bool { return strings.Contains(n, ".partial-") }) {
		if time.Now().After(deadline) {
			t.Fatal("no partial file during the download")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch did not stop after cancellation")
	}
	assertClean(t, c.Dir)
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("cancellation was retried: %d requests", n)
	}
}

func TestFetchRedirects(t *testing.T) {
	plain := newServer(t, false)
	plainURL := plain.put("/x/"+wheelFn, wheelBytes(t))
	s := newServer(t, true)
	s.put("/real/"+wheelFn, wheelBytes(t))
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/to-http/" + wheelFn:
			http.Redirect(w, r, plainURL, http.StatusFound)
		case "/to-https/" + wheelFn:
			http.Redirect(w, r, "/real/"+wheelFn, http.StatusMovedPermanently)
		case "/to-remote-http/" + wheelFn:
			http.Redirect(w, r, "http://example.invalid/"+wheelFn, http.StatusFound)
		case "/loop/" + wheelFn:
			http.Redirect(w, r, "/loop/"+wheelFn, http.StatusFound)
		default:
			return false
		}
		return true
	}
	ctx := context.Background()

	c := &Cache{Dir: t.TempDir(), Client: s.Client(), backoff0: int64(time.Millisecond)}
	if _, err := c.Fetch(ctx, s.URL+"/to-https/"+wheelFn, wheelSHA, wheelSize); err != nil {
		t.Fatalf("https redirect: %v", err)
	}
	for _, path := range []string{"/to-http/", "/to-remote-http/", "/loop/"} {
		c := &Cache{Dir: t.TempDir(), Client: s.Client(), backoff0: int64(time.Millisecond)}
		_, err := c.Fetch(ctx, s.URL+path+wheelFn, wheelSHA, wheelSize)
		if !errors.Is(err, ErrInsecureURL) {
			t.Fatalf("%s: error %v, want ErrInsecureURL", path, err)
		}
		assertClean(t, c.Dir)
	}
	if n := plain.hits.Load(); n != 0 {
		t.Fatalf("the http target was contacted %d times", n)
	}
	// AllowHTTP admits loopback http only, also as a redirect target.
	c = &Cache{Dir: t.TempDir(), Client: s.Client(), AllowHTTP: true}
	if _, err := c.Fetch(ctx, s.URL+"/to-remote-http/"+wheelFn, wheelSHA, wheelSize); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("redirect to remote http with AllowHTTP: %v", err)
	}
	// The caller's CheckRedirect still runs, after the cache's own check.
	called := false
	cl := *s.Client()
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { called = true; return errors.New("caller says no") }
	c = &Cache{Dir: t.TempDir(), Client: &cl}
	if _, err := c.Fetch(ctx, s.URL+"/to-https/"+wheelFn, wheelSHA, wheelSize); err == nil || !called {
		t.Fatalf("caller's CheckRedirect ignored: %v", err)
	}
	if cl.CheckRedirect == nil {
		t.Fatal("caller's client was modified")
	}
}

func TestFetchConcurrentOneDownload(t *testing.T) {
	s := newServer(t, false)
	s.delay = 100 * time.Millisecond
	url := s.put("/x/"+wheelFn, wheelBytes(t))
	dir := t.TempDir()
	// Four Cache values on one directory stand in for four processes: they
	// share no in-process lock, only the lock files.
	caches := make([]*Cache, 4)
	for i := range caches {
		caches[i] = &Cache{Dir: dir, Client: s.Client(), AllowHTTP: true}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := caches[i%len(caches)].Fetch(context.Background(), url, wheelSHA, wheelSize)
			if err == nil && p != filepath.Join(dir, wheelSHA, wheelFn) {
				err = fmt.Errorf("path %s", p)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("%d downloads, want 1", n)
	}
	assertClean(t, dir)
}

func TestFetchSameCacheConcurrent(t *testing.T) {
	s := newServer(t, false)
	s.delay = 50 * time.Millisecond
	url := s.put("/x/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Fetch(context.Background(), url, wheelSHA, wheelSize); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 || s.hits.Load() != 1 {
		t.Fatalf("%d failures, %d downloads", failed.Load(), s.hits.Load())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.locks) != 0 {
		t.Fatalf("lock table not emptied: %d entries", len(c.locks))
	}
}

func TestFetchLockWaitIsCancellable(t *testing.T) {
	release := make(chan struct{})
	s := newServer(t, false)
	data := wheelBytes(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		<-release
		w.Write(data)
		return true
	}
	url := s.put("/x/"+wheelFn, data)
	c := newCache(t, s)
	first := make(chan error, 1)
	go func() {
		_, err := c.Fetch(context.Background(), url, wheelSHA, wheelSize)
		first <- err
	}()
	for s.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for _, other := range []*Cache{c, {Dir: c.Dir, Client: s.Client(), AllowHTTP: true}} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := other.Fetch(ctx, url, wheelSHA, wheelSize)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
			t.Fatalf("waiting for the lock: %v after %v", err, time.Since(start))
		}
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestFetchInvalidInput(t *testing.T) {
	s := newServer(t, false)
	good := s.put("/x/"+wheelFn, wheelBytes(t))
	cases := []struct {
		name, url, sha string
		size           int64
		allowHTTP      bool
		want           error
	}{
		{"uppercase sha", good, strings.ToUpper(wheelSHA), 0, true, ErrInvalidInput},
		{"short sha", good, wheelSHA[:63], 0, true, ErrInvalidInput},
		{"empty sha", good, "", 0, true, ErrInvalidInput},
		{"negative size", good, wheelSHA, -1, true, ErrInvalidInput},
		{"http without AllowHTTP", good, wheelSHA, 0, false, ErrInsecureURL},
		{"http to a remote host", "http://example.invalid/" + wheelFn, wheelSHA, 0, true, ErrInsecureURL},
		{"ftp", "ftp://example.invalid/" + wheelFn, wheelSHA, 0, true, ErrInsecureURL},
		{"file", "file:///x/" + wheelFn, wheelSHA, 0, true, ErrInsecureURL},
		{"relative", "x/" + wheelFn, wheelSHA, 0, true, ErrInsecureURL},
		{"https without host", "https:///x/" + wheelFn, wheelSHA, 0, true, ErrInsecureURL},
		{"opaque", "https:" + wheelFn, wheelSHA, 0, true, ErrInsecureURL},
		{"directory url", s.URL + "/x/", wheelSHA, 0, true, ErrInvalidInput},
		{"not an archive", s.URL + "/x/wheel.zip", wheelSHA, 0, true, ErrInvalidInput},
		{"dot name", s.URL + "/x/.hidden.conda", wheelSHA, 0, true, ErrInvalidInput},
		{"escaped traversal", s.URL + "/x/..%2F..%2Fevil.conda", wheelSHA, 0, true, ErrInvalidInput},
		{"escaped slash", s.URL + "/x/a%5Cb.conda", wheelSHA, 0, true, ErrInvalidInput},
		{"nul", s.URL + "/x/a%00b.conda", wheelSHA, 0, true, ErrInvalidInput},
		{"space", s.URL + "/x/a%20b.conda", wheelSHA, 0, true, ErrInvalidInput},
		{"bad url", "https://[::1", wheelSHA, 0, true, ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Cache{Dir: t.TempDir(), Client: s.Client(), AllowHTTP: tc.allowHTTP}
			_, err := c.Fetch(context.Background(), tc.url, tc.sha, tc.size)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
		})
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("invalid input reached the server %d times", n)
	}
	if _, err := (&Cache{}).Fetch(context.Background(), good, wheelSHA, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty Dir: %v", err)
	}
}

func TestFetchRedactsCredentials(t *testing.T) {
	s := newServer(t, false)
	url := strings.Replace(s.put("/x/"+wheelFn, wheelBytes(t)), "http://", "http://user:hunter2@", 1)
	c := newCache(t, s)
	_, err := c.Fetch(context.Background(), url, strings.Repeat("0", 64), 0)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %v leaks the password", err)
	}
}

func TestFetchReverifiesCachedArchive(t *testing.T) {
	data := wheelBytes(t)
	s := newServer(t, false)
	url := s.put("/x/"+wheelFn, data)
	ctx := context.Background()

	t.Run("modified in place with mtime restored", func(t *testing.T) {
		c := newCache(t, s)
		p, err := c.Fetch(ctx, url, wheelSHA, wheelSize)
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		bad := bytes.Clone(data)
		bad[100] ^= 0xff
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt(bad[100:101], 100)
		f.Close()
		os.Chtimes(p, fi.ModTime(), fi.ModTime())
		before := s.hits.Load()
		if _, err := c.Fetch(ctx, url, wheelSHA, wheelSize); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
			t.Fatal("tampered archive was used")
		}
		if fileIDsSupported() && s.hits.Load() != before+1 {
			t.Fatalf("tampered archive not downloaded again")
		}
	})
	t.Run("archive left by another tool", func(t *testing.T) {
		c := newCache(t, s)
		os.MkdirAll(filepath.Join(c.Dir, wheelSHA), 0o755)
		os.WriteFile(archivePath(c, wheelSHA, wheelFn), data, 0o644)
		before := s.hits.Load()
		if _, err := c.Fetch(ctx, url, wheelSHA, wheelSize); err != nil {
			t.Fatal(err)
		}
		if s.hits.Load() != before {
			t.Fatal("a valid cached archive was downloaded again")
		}
		if _, err := os.Stat(archivePath(c, wheelSHA, wheelFn) + ".sha256"); err != nil {
			t.Fatal("no sidecar after re-hashing")
		}
	})
	t.Run("wrong archive left by another tool", func(t *testing.T) {
		c := newCache(t, s)
		os.MkdirAll(filepath.Join(c.Dir, wheelSHA), 0o755)
		os.WriteFile(archivePath(c, wheelSHA, wheelFn), bytes.Repeat([]byte{1}, wheelSize), 0o644)
		before := s.hits.Load()
		p, err := c.Fetch(ctx, url, wheelSHA, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, data) || s.hits.Load() != before+1 {
			t.Fatal("wrong cached archive kept")
		}
	})
	t.Run("forged sidecar for another file", func(t *testing.T) {
		c := newCache(t, s)
		p := archivePath(c, wheelSHA, wheelFn)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, bytes.Repeat([]byte{1}, wheelSize), 0o644)
		f, _ := os.Open(p)
		st, _ := stampOf(f)
		f.Close()
		// A sidecar from an earlier, different file at the same path.
		key, _ := c.markerKey()
		st.mtime--
		os.WriteFile(p+".sha256", []byte(sidecarContent(key, wheelFn, wheelSHA, st)), 0o644)
		if _, err := c.Fetch(ctx, url, wheelSHA, 0); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
			t.Fatal("stale sidecar vouched for the wrong file")
		}
	})
	t.Run("symlink in the cache", func(t *testing.T) {
		c := newCache(t, s)
		target := filepath.Join(t.TempDir(), "elsewhere.conda")
		os.WriteFile(target, data, 0o644)
		os.MkdirAll(filepath.Join(c.Dir, wheelSHA), 0o755)
		if err := os.Symlink(target, archivePath(c, wheelSHA, wheelFn)); err != nil {
			t.Skip(err)
		}
		before := s.hits.Load()
		p, err := c.Fetch(ctx, url, wheelSHA, 0)
		if err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || s.hits.Load() != before+1 {
			t.Fatal("a symlinked cache entry was used instead of replaced")
		}
	})
}

func TestFetchRemovesStalePartials(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/x/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	content := filepath.Join(c.Dir, wheelSHA)
	os.MkdirAll(content, 0o755)
	stale := filepath.Join(content, wheelFn+".partial-0123456789abcdef")
	keep := []string{wheelFn + ".partial-keep", wheelFn + ".partial-0123456789abcdef.conda", "other.conda.partial-0123456789abcdef"}
	os.WriteFile(stale, []byte("junk"), 0o644)
	for _, k := range keep {
		os.WriteFile(filepath.Join(content, k), []byte("keep"), 0o644)
	}
	if _, err := c.Fetch(context.Background(), url, wheelSHA, wheelSize); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Fatal("stale partial not removed")
	}
	for _, k := range keep {
		if _, err := os.Lstat(filepath.Join(content, k)); err != nil {
			t.Fatalf("%s removed", k)
		}
	}
}

func TestFetchRequestsIdentityEncoding(t *testing.T) {
	s := newServer(t, false)
	var enc atomic.Value
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		enc.Store(r.Header.Get("Accept-Encoding"))
		return false
	}
	url := s.put("/x/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	if _, err := c.Fetch(context.Background(), url, wheelSHA, 0); err != nil {
		t.Fatal(err)
	}
	if got := enc.Load(); got != "identity" {
		t.Fatalf("Accept-Encoding %q", got)
	}
}

func TestFetchDefaultClient(t *testing.T) {
	// The default client is built once, uses no proxy, and serves https.
	c := &Cache{Dir: t.TempDir()}
	a, b := c.httpClient(), c.httpClient()
	if a.Transport != b.Transport || a.Transport == nil {
		t.Fatal("default transport not shared")
	}
	if tr := a.Transport.(*http.Transport); tr.Proxy != nil || tr.TLSClientConfig.MinVersion == 0 {
		t.Fatal("default transport reads a proxy or allows old TLS")
	}
}

func TestParseSidecar(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	st := stamp{size: 31858, mtime: 1769139206503311400, ino: 42, ctime: 1769139206503311401}
	good := sidecarContent(key, wheelFn, wheelSHA, st)
	if sha, got, err := parseSidecar([]byte(good), key, wheelFn); err != nil || sha != wheelSHA || got != st {
		t.Fatalf("parseSidecar(good) = %s %+v, %v", sha, got, err)
	}
	if _, _, err := parseSidecar([]byte(good), bytes.Repeat([]byte{8}, 32), wheelFn); err == nil {
		t.Error("sidecar accepted under another key")
	}
	if _, _, err := parseSidecar([]byte(good), key, "other-1-0.conda"); err == nil {
		t.Error("sidecar accepted for another file name")
	}
	body := sidecarBody(wheelSHA, st)
	mac := sidecarMAC(key, wheelFn, body)
	for _, bad := range []string{
		"", "\n", good[:len(good)-1], good + "\n", strings.ToUpper(good[:64]) + good[64:],
		wheelSHA + " -1 1 1 1 " + mac + "\n", wheelSHA + " +1 1 1 1 " + mac + "\n",
		wheelSHA + " 01 1 1 1 " + mac + "\n", wheelSHA + " 1 1 1 " + mac + "\n",
		body + "  " + mac + "\n", body + " " + strings.Repeat("0", 64) + "\n", "x" + good,
		// A correct MAC over different numbers.
		sidecarBody(wheelSHA, stamp{size: 1}) + " " + mac + "\n",
	} {
		if _, _, err := parseSidecar([]byte(bad), key, wheelFn); err == nil {
			t.Errorf("parseSidecar(%q) accepted", bad)
		}
	}
}

func TestSafeName(t *testing.T) {
	for _, ok := range []string{wheelFn, "_openmp_mutex-4.5-2_gnu.tar.bz2", "a", "x-1.0+local-0", "pkg-2021!1.0-0"} {
		if !safeName(ok) {
			t.Errorf("safeName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "-x", "+x", "a/b", `a\b`, "a b", "a:b", "a\x00b", "a*b", "a?b", "a[b", strings.Repeat("a", maxNameLen+1)} {
		if safeName(bad) {
			t.Errorf("safeName(%q) = true", bad)
		}
	}
}

func FuzzParseSidecar(f *testing.F) {
	key := bytes.Repeat([]byte{7}, 32)
	f.Add([]byte(sidecarContent(key, wheelFn, wheelSHA, stamp{size: 31858, mtime: 1769139206503311400, ino: 42, ctime: 1})))
	f.Add([]byte(""))
	f.Add([]byte(wheelSHA + " 01 1 1 1 " + wheelSHA + "\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		sha, st, err := parseSidecar(b, key, wheelFn)
		if err != nil {
			return
		}
		if sidecarContent(key, wheelFn, sha, st) != string(b) || !isHexDigest(sha) || st.size < 0 {
			t.Fatalf("parseSidecar(%q) = %s %+v is not canonical", b, sha, st)
		}
	})
}

func FuzzParseURL(f *testing.F) {
	for _, u := range []string{"https://conda.anaconda.org/conda-forge/noarch/" + wheelFn, "http://127.0.0.1:1/x.tar.bz2", "https://h/a/..%2F.conda", "https://h/", "file:///x.conda"} {
		f.Add(u, true)
	}
	f.Fuzz(func(t *testing.T, raw string, allowHTTP bool) {
		c := &Cache{AllowHTTP: allowHTTP}
		u, fn, err := c.parseURL(raw)
		if err != nil {
			if !errors.Is(err, ErrInvalidInput) && !errors.Is(err, ErrInsecureURL) {
				t.Fatalf("parseURL(%q): untyped error %v", raw, err)
			}
			return
		}
		if !safeName(fn) || !isArchiveName(fn) || strings.ContainsAny(fn, `/\`) {
			t.Fatalf("parseURL(%q) accepted file name %q", raw, fn)
		}
		if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
			t.Fatalf("parseURL(%q) accepted scheme %q", raw, u.Scheme)
		}
		if filepath.Base(filepath.Join("cache", fn)) != fn {
			t.Fatalf("file name %q is not one path component", fn)
		}
	})
}

// fileIDsSupported reports whether sidecars record inode and change time
// on this platform.
func fileIDsSupported() bool {
	f, err := os.Open(os.Args[0])
	if err != nil {
		return false
	}
	defer f.Close()
	ino, _ := fileStamp(f)
	return ino != 0
}

// namesOrNil lists dir, or returns nil if it cannot be read.
func namesOrNil(dir string) []string {
	des, _ := os.ReadDir(dir)
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}
