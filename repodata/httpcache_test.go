package repodata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestFetcher(t *testing.T, opt Options) *fetcher {
	t.Helper()
	if opt.CacheDir == "" {
		opt.CacheDir = t.TempDir()
	}
	if opt.Platform == "" {
		opt.Platform = "noarch"
	}
	s, err := opt.settings()
	if err != nil {
		t.Fatal(err)
	}
	return newFetcher(s, nil, urlPolicy{httpLoopback: true, privateHosts: true})
}

func TestCacheEntryRoundTrip(t *testing.T) {
	f := newTestFetcher(t, Options{})
	const u = "https://example.com/t/secret-token/c/noarch/repodata.json"
	meta := cacheMeta{Status: http.StatusOK, ETag: `"x"`, Fetched: time.Unix(1700000000, 0).UTC()}
	if err := f.storeEntry(u, meta, []byte("body")); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(f.indexCachePath(u)); bytes.Contains(data, []byte("secret")) {
		t.Error("cache entry stores the token")
	}
	e, err := f.loadEntry(u)
	if err != nil || e == nil || string(e.body) != "body" || e.meta.ETag != `"x"` || !e.meta.Fetched.Equal(meta.Fetched) {
		t.Fatalf("entry %+v", e)
	}
	if e, _ := f.loadEntry("https://example.com/other"); e != nil {
		t.Error("entry found for another URL")
	}
	// No temp files are left behind.
	files, _ := filepath.Glob(filepath.Join(f.s.cacheDir, "index", ".tmp-*"))
	if len(files) != 0 {
		t.Errorf("temp files left: %q", files)
	}
}

func TestParseCacheEntryRejectsTampering(t *testing.T) {
	f := newTestFetcher(t, Options{})
	meta := cacheMeta{Status: http.StatusOK}
	if err := f.storeEntry("https://example.com/x", meta, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(f.indexCachePath("https://example.com/x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCacheEntry(good); err != nil {
		t.Fatalf("good entry: %v", err)
	}
	cases := map[string][]byte{
		"empty":           {},
		"bad magic":       append([]byte("x"), good[1:]...),
		"body changed":    bytes.Replace(good, []byte("hello"), []byte("jello"), 1),
		"body truncated":  good[:len(good)-1],
		"body extended":   append(append([]byte{}, good...), '!'),
		"no header end":   []byte(cacheMagic + `{"url_sha256":"x"`),
		"header garbage":  []byte(cacheMagic + "{]\nbody"),
		"unknown field":   []byte(cacheMagic + `{"url_sha256":"x","status":200,"fetched":"2026-01-01T00:00:00Z","size":0,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","evil":1}` + "\n"),
		"bad status":      []byte(cacheMagic + `{"url_sha256":"x","status":500,"fetched":"2026-01-01T00:00:00Z","size":0,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}` + "\n"),
		"404 with a body": []byte(cacheMagic + `{"url_sha256":"x","status":404,"fetched":"2026-01-01T00:00:00Z","size":1,"sha256":"ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"}` + "\na"),
		"two headers":     []byte(cacheMagic + `{"url_sha256":"x","status":404,"fetched":"2026-01-01T00:00:00Z","size":0,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"} {}` + "\n"),
		"huge header":     append([]byte(cacheMagic), bytes.Repeat([]byte(" "), maxCacheHeaderBytes+1)...),
	}
	for name, data := range cases {
		if _, err := parseCacheEntry(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A tampered cache file is ignored: the fetcher downloads again.
	ts := newTestServer(t)
	ts.put("/c/noarch/repodata.json", []byte(`{"packages": {}}`))
	f = newTestFetcher(t, Options{MaxAge: time.Hour})
	u := ts.url("/c/noarch/repodata.json")
	if _, err := f.fetchIndex(ctxT(t), u, 1<<20); err != nil {
		t.Fatal(err)
	}
	p := f.indexCachePath(u)
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, bytes.Replace(data, []byte("packages"), []byte("PACKAGES"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := f.fetchIndex(ctxT(t), u, 1<<20)
	if err != nil || !bytes.Contains(body, []byte(`"packages"`)) || ts.count("/c/noarch/repodata.json") != 2 {
		t.Errorf("tampered cache: body %q, err %v, %d requests", body, err, ts.count("/c/noarch/repodata.json"))
	}
}

func TestFetchIndexConditionalGET(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	path := "/c/noarch/repodata.json"
	ts.put(path, []byte("v1"))
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	f := newTestFetcher(t, Options{MaxAge: 20 * time.Minute})
	f.s.now = func() time.Time { return now }
	u := ts.url(path)
	get := func() string {
		t.Helper()
		b, err := f.fetchIndex(ctx, u, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if get() != "v1" || ts.count(path) != 1 {
		t.Fatal("first fetch")
	}
	now = now.Add(10 * time.Minute) // fresh: no request
	if get() != "v1" || ts.count(path) != 1 {
		t.Error("fresh entry revalidated")
	}
	now = now.Add(15 * time.Minute) // stale: conditional GET, 304
	if get() != "v1" || ts.count(path) != 2 || ts.notModified(path) != 1 {
		t.Error("stale entry not revalidated with a conditional GET")
	}
	now = now.Add(10 * time.Minute) // the 304 refreshed the entry
	if get() != "v1" || ts.count(path) != 2 {
		t.Error("304 did not refresh freshness")
	}
	ts.put(path, []byte("v2"))
	now = now.Add(time.Hour)
	if get() != "v2" || ts.count(path) != 3 {
		t.Error("changed file not downloaded")
	}
	// A clock that went backwards makes the entry stale.
	now = now.Add(-24 * time.Hour)
	if get() != "v2" || ts.count(path) != 4 {
		t.Error("future-dated entry trusted")
	}
	// Offline: any cached copy, whatever its age; no requests.
	f.s.offline = true
	now = now.Add(1000 * time.Hour)
	if get() != "v2" || ts.count(path) != 4 {
		t.Error("offline read touched the network")
	}
	if _, err := f.fetchIndex(ctx, ts.url("/other"), 1<<20); !errors.Is(err, ErrNotCached) {
		t.Errorf("offline miss: %v", err)
	}
}

func TestFetchIndexLastModifiedOnly(t *testing.T) {
	ts := newTestServer(t)
	path := "/c/noarch/repodata.json"
	var sawIMS string
	ts.handle(path, func(w http.ResponseWriter, r *http.Request) {
		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			sawIMS = ims
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lastModified)
		_, _ = w.Write([]byte("v1"))
	})
	f := newTestFetcher(t, Options{})
	for i := 0; i < 2; i++ {
		if b, err := f.fetchIndex(ctxT(t), ts.url(path), 1<<20); err != nil || string(b) != "v1" {
			t.Fatalf("got %q, %v", b, err)
		}
	}
	if sawIMS != lastModified {
		t.Errorf("If-Modified-Since = %q", sawIMS)
	}
}

func TestFetchIndexStatuses(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	f := newTestFetcher(t, Options{})
	for code, wantNotFound := range map[int]bool{404: true, 410: true, 501: true, 403: false, 400: false, 304: false} {
		path := fmt.Sprintf("/status/%d", code)
		ts.handle(path, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
		_, err := f.fetchIndex(ctx, ts.url(path), 1<<20)
		if errors.Is(err, ErrNotFound) != wantNotFound {
			t.Errorf("HTTP %d: err = %v", code, err)
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.StatusCode != code {
			t.Errorf("HTTP %d: no HTTPError in %v", code, err)
		}
	}
}

func TestFetchRetriesTransientErrors(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	path := "/c/noarch/repodata.json"
	ts.put(path, []byte("ok"))
	ts.failNext(path, 2)
	f := newTestFetcher(t, Options{})
	if b, err := f.fetchIndex(ctx, ts.url(path), 1<<20); err != nil || string(b) != "ok" {
		t.Fatalf("got %q, %v", b, err)
	}
	if ts.count(path) != 3 {
		t.Errorf("%d requests, want 3", ts.count(path))
	}
	ts.failNext(path, 5)
	f2 := newTestFetcher(t, Options{})
	var he *HTTPError
	if _, err := f2.fetchIndex(ctx, ts.url(path), 1<<20); !errors.As(err, &he) || he.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("err = %v", err)
	}
}

func TestFetchSizeLimit(t *testing.T) {
	ts := newTestServer(t)
	ts.put("/big", bytes.Repeat([]byte("x"), 1000))
	f := newTestFetcher(t, Options{})
	if _, err := f.fetchIndex(ctxT(t), ts.url("/big"), 999); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v", err)
	}
	if b, err := f.fetchIndex(ctxT(t), ts.url("/big"), 1000); err != nil || len(b) != 1000 {
		t.Errorf("at the limit: %d, %v", len(b), err)
	}
	if ts.count("/big") != 2 {
		t.Errorf("too-large response retried: %d requests", ts.count("/big"))
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "f")
	if err := writeFileAtomic(p, []byte("a"), []byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "c" {
		t.Errorf("content %q", b)
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
	// A file in place of the directory makes the write fail cleanly.
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "file", "x"), []byte("x")); err == nil {
		t.Error("write under a file succeeded")
	}
}

func TestSanitizeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"repodata.json.zst":           "repodata.json.zst",
		"repodata_shards.msgpack.zst": "repodata_shards.msgpack.zst",
		"a b/../c":                    "a_b_.._c",
		strings.Repeat("x", 100):      strings.Repeat("x", 64),
	} {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q", in, got)
		}
	}
}

// S5: once data is verified, a cache that cannot be written does not fail
// the operation.
func TestUnwritableCacheIsBestEffort(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	ts.putShards("/c", "linux-64", "", "", "", map[string][]byte{"foo": simpleShard(t, "foo", "1.0")})
	ts.putShards("/c", "noarch", "", "", "", nil)
	ts.put("/f/noarch/repodata.json", []byte(noarchDoc))
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opt := Options{CacheDir: filepath.Join(file, "cache"), Platform: "linux-64"}
	c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	if recs, err := c.Records(ctx, "foo"); err != nil || len(recs) != 1 {
		t.Errorf("Records: %v, %v", recs, err)
	}
	if _, err := NewFullIndex(ctx, ts.url("/f"), opt); err != nil {
		t.Errorf("NewFullIndex: %v", err)
	}
}

// S5: a 304 refreshes the entry through a small sidecar; the body file is
// not rewritten.
func TestNotModifiedWritesOnlyMetadata(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	path := "/c/noarch/repodata.json"
	ts.put(path, bytes.Repeat([]byte("x"), 1<<20))
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	f := newTestFetcher(t, Options{MaxAge: time.Minute})
	f.s.now = func() time.Time { return now }
	u := ts.url(path)
	if _, err := f.fetchIndex(ctx, u, 2<<20); err != nil {
		t.Fatal(err)
	}
	entryPath := f.indexCachePath(u)
	before, _ := os.Stat(entryPath)
	beforeData, _ := os.ReadFile(entryPath)
	now = now.Add(time.Hour)
	if _, err := f.fetchIndex(ctx, u, 2<<20); err != nil || ts.notModified(path) != 1 {
		t.Fatalf("revalidation: %v, %d 304s", err, ts.notModified(path))
	}
	after, _ := os.Stat(entryPath)
	afterData, _ := os.ReadFile(entryPath)
	if !after.ModTime().Equal(before.ModTime()) || !bytes.Equal(beforeData, afterData) {
		t.Error("a 304 rewrote the cached body")
	}
	if _, err := os.Stat(freshnessPath(entryPath)); err != nil {
		t.Errorf("no freshness sidecar: %v", err)
	}
	now = now.Add(30 * time.Second) // fresh thanks to the sidecar
	if _, err := f.fetchIndex(ctx, u, 2<<20); err != nil || ts.count(path) != 2 {
		t.Errorf("sidecar ignored: %d requests", ts.count(path))
	}
	// A sidecar naming another body is ignored.
	side, _ := json.Marshal(freshness{SHA256: strings.Repeat("0", 64), Fetched: now.Add(time.Hour)})
	if err := os.WriteFile(freshnessPath(entryPath), side, 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err := f.fetchIndex(ctx, u, 2<<20); err != nil || ts.count(path) != 3 {
		t.Errorf("foreign sidecar trusted: %d requests", ts.count(path))
	}
}

// N8: the not-found error is one line and still wraps both causes.
func TestNotFoundErrorIsOneLine(t *testing.T) {
	ts := newTestServer(t)
	f := newTestFetcher(t, Options{})
	_, err := f.fetchIndex(ctxT(t), ts.url("/missing"), 1<<20)
	var he *HTTPError
	if !errors.Is(err, ErrNotFound) || !errors.As(err, &he) || strings.Contains(err.Error(), "\n") {
		t.Errorf("err = %q", err)
	}
}

// N9: Retry-After on 429 is honoured, within bounds.
func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Duration{
		"":    0,
		"2":   2 * time.Second,
		"-5":  0,
		"999": maxRetryAfter,
		"x":   0,
		now.Add(3 * time.Second).Format(http.TimeFormat):  3 * time.Second,
		now.Add(-3 * time.Second).Format(http.TimeFormat): 0,
	} {
		if got := retryAfter(h, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", h, got, want)
		}
	}
	ts := newTestServer(t)
	path := "/c/noarch/repodata.json"
	var calls atomic.Int32
	ts.handle(path, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	f := newTestFetcher(t, Options{})
	start := time.Now()
	if b, err := f.fetchIndex(ctxT(t), ts.url(path), 1<<20); err != nil || string(b) != "ok" {
		t.Fatalf("got %q, %v", b, err)
	}
	if d := time.Since(start); d < time.Second {
		t.Errorf("retried after %v, before Retry-After", d)
	}
}
