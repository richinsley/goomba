package repodata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/richinsley/goomba/record"
)

// Compile-time interface checks.
var (
	_ Source     = (*ShardedChannel)(nil)
	_ Prefetcher = (*ShardedChannel)(nil)
	_ Source     = (*FullIndex)(nil)
	_ Source     = (*Memory)(nil)
	_ Source     = (*Multi)(nil)
	_ Prefetcher = (*Multi)(nil)
)

// ---- msgpack building (independent encoder: vmihailenco/msgpack) ----

// kv is one map entry; omap keeps entry order (and allows odd keys).
type kv struct {
	k any
	v any
}
type omap []kv

// mpExtVal is a msgpack ext value.
type mpExtVal struct {
	typ  int8
	data []byte
}

// mpRaw is spliced into the output verbatim.
type mpRaw []byte

// mpI forces a signed integer encoding.
type mpI int64

func mpEncode(t testing.TB, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseCompactInts(true)
	if err := encodeMP(enc, &buf, v); err != nil {
		t.Fatalf("msgpack encode: %v", err)
	}
	return buf.Bytes()
}

func encodeMP(enc *msgpack.Encoder, buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		return enc.EncodeNil()
	case bool:
		return enc.EncodeBool(x)
	case string:
		return enc.EncodeString(x)
	case []byte:
		return enc.EncodeBytes(x)
	case int:
		return enc.EncodeInt(int64(x))
	case mpI:
		return enc.EncodeInt64(int64(x))
	case uint64:
		return enc.EncodeUint(x)
	case float64:
		return enc.EncodeFloat64(x)
	case mpExtVal:
		if err := enc.EncodeExtHeader(x.typ, len(x.data)); err != nil {
			return err
		}
		_, err := buf.Write(x.data)
		return err
	case mpRaw:
		_, err := buf.Write(x)
		return err
	case []string:
		if err := enc.EncodeArrayLen(len(x)); err != nil {
			return err
		}
		for _, s := range x {
			if err := enc.EncodeString(s); err != nil {
				return err
			}
		}
		return nil
	case []any:
		if err := enc.EncodeArrayLen(len(x)); err != nil {
			return err
		}
		for _, e := range x {
			if err := encodeMP(enc, buf, e); err != nil {
				return err
			}
		}
		return nil
	case omap:
		if err := enc.EncodeMapLen(len(x)); err != nil {
			return err
		}
		for _, e := range x {
			if err := encodeMP(enc, buf, e.k); err != nil {
				return err
			}
			if err := encodeMP(enc, buf, e.v); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("encodeMP: unsupported %T", v)
}

func zst(t testing.TB, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(true), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<17))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hashFormat mirrors libmamba's test HashFormat.
type hashFormat int

const (
	hashString hashFormat = iota
	hashBytes
	hashArrayBytes
)

func hashMP(t testing.TB, h string, f hashFormat) any {
	switch f {
	case hashBytes:
		return unhex(t, h)
	case hashArrayBytes:
		var a []any
		for _, b := range unhex(t, h) {
			a = append(a, uint64(b))
		}
		return a
	}
	return h
}

// recSpec mirrors the arguments of libmamba's
// create_shard_package_record_msgpack (tests/src/core/test_shard_utils.cpp).
type recSpec struct {
	name, version, build string
	buildNumber          uint64
	sha256, md5          string // "" = absent
	depends, constrains  []string
	noarch               string // "" = absent
	sha256Fmt, md5Fmt    hashFormat
	trackFeatures        []string
}

func (s recSpec) omap(t testing.TB) omap {
	m := omap{{"name", s.name}, {"version", s.version}, {"build", s.build}, {"build_number", s.buildNumber}}
	if s.sha256 != "" {
		m = append(m, kv{"sha256", hashMP(t, s.sha256, s.sha256Fmt)})
	}
	if s.md5 != "" {
		m = append(m, kv{"md5", hashMP(t, s.md5, s.md5Fmt)})
	}
	if len(s.depends) > 0 {
		m = append(m, kv{"depends", s.depends})
	}
	if len(s.constrains) > 0 {
		m = append(m, kv{"constrains", s.constrains})
	}
	if s.noarch != "" {
		m = append(m, kv{"noarch", s.noarch})
	}
	if len(s.trackFeatures) > 0 {
		m = append(m, kv{"track_features", s.trackFeatures})
	}
	return m
}

// shardMP builds {"packages": packages, "packages.conda": conda} (each
// omitted when nil).
func shardMP(t testing.TB, packages, conda omap) []byte {
	m := omap{}
	if packages != nil {
		m = append(m, kv{"packages", packages})
	}
	if conda != nil {
		m = append(m, kv{"packages.conda", conda})
	}
	return mpEncode(t, m)
}

// shardIndexMP mirrors create_shard_index_msgpack_with_version.
func shardIndexMP(t testing.TB, baseURL, shardsBaseURL, subdir string, version uint64, shards map[string][]byte) []byte {
	names := make([]string, 0, len(shards))
	for n := range shards {
		names = append(names, n)
	}
	sort.Strings(names)
	sh := omap{}
	for _, n := range names {
		sh = append(sh, kv{n, shards[n]})
	}
	return mpEncode(t, omap{
		{"info", omap{{"base_url", baseURL}, {"shards_base_url", shardsBaseURL}, {"subdir", subdir}}},
		{"version", version},
		{"shards", sh},
	})
}

const (
	testSHA = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	testMD5 = "12345678901234567890123456789012"
)

// ---- test channel server ----

// testServer serves files from memory with ETag / Last-Modified support and
// counts requests per path.
type testServer struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	files    map[string][]byte
	requests map[string]int
	notMod   map[string]int // 304 answers per path
	fail     map[string]int // path -> number of upcoming 503 answers
	override map[string]http.HandlerFunc
}

const lastModified = "Sat, 17 Oct 2026 07:00:00 GMT"

func newTestServer(t testing.TB) *testServer {
	ts := &testServer{
		t:        t,
		files:    map[string][]byte{},
		requests: map[string]int{},
		notMod:   map[string]int{},
		fail:     map[string]int{},
		override: map[string]http.HandlerFunc{},
	}
	ts.srv = httptest.NewServer(http.HandlerFunc(ts.serve))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *testServer) serve(w http.ResponseWriter, r *http.Request) {
	ts.mu.Lock()
	ts.requests[r.URL.Path]++
	if h := ts.override[r.URL.Path]; h != nil {
		ts.mu.Unlock()
		h(w, r)
		return
	}
	if ts.fail[r.URL.Path] > 0 {
		ts.fail[r.URL.Path]--
		ts.mu.Unlock()
		http.Error(w, "try later", http.StatusServiceUnavailable)
		return
	}
	data, ok := ts.files[r.URL.Path]
	if !ok {
		ts.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if r.Header.Get("If-None-Match") == etag {
		ts.notMod[r.URL.Path]++
		ts.mu.Unlock()
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	ts.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", lastModified)
	w.Header().Set("Cache-Control", "public, max-age=1200")
	_, _ = w.Write(data)
}

func (ts *testServer) put(path string, data []byte) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.files[path] = data
}

// handle installs a custom handler for one path.
func (ts *testServer) handle(path string, h http.HandlerFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.override[path] = h
}

// failNext makes the next n requests to path answer 503.
func (ts *testServer) failNext(path string, n int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.fail[path] = n
}

func (ts *testServer) del(path string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.files, path)
}

func (ts *testServer) count(path string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.requests[path]
}

func (ts *testServer) notModified(path string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.notMod[path]
}

// totalRequests counts requests to paths with the given prefix.
func (ts *testServer) totalRequests(prefix string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := 0
	for p, c := range ts.requests {
		if strings.HasPrefix(p, prefix) {
			n += c
		}
	}
	return n
}

// url returns the absolute URL of a server path.
func (ts *testServer) url(path string) string { return ts.srv.URL + path }

// putShards publishes a sharded subdir: shards maps package names to
// uncompressed shard msgpack. It returns the compressed shard bytes by name.
func (ts *testServer) putShards(channelPath, subdir, baseURL, shardsBaseURL, shardsDir string, shards map[string][]byte) map[string][]byte {
	hashes := map[string][]byte{}
	out := map[string][]byte{}
	for name, body := range shards {
		z := zst(ts.t, body)
		sum := sha256.Sum256(z)
		hashes[name] = sum[:]
		out[name] = z
		ts.put(channelPath+"/"+subdir+"/"+shardsDir+hex.EncodeToString(sum[:])+".msgpack.zst", z)
	}
	idx := shardIndexMP(ts.t, baseURL, shardsBaseURL, subdir, 1, hashes)
	ts.put(channelPath+"/"+subdir+"/"+shardIndexFile, zst(ts.t, idx))
	return out
}

// ---- misc ----

func mustURL(t testing.TB, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func testOptions(t testing.TB, platform string) Options {
	return Options{CacheDir: t.TempDir(), Platform: platform}
}

func readTestdata(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fns(recs []*record.PackageRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Fn
	}
	return out
}

func byFn(recs []*record.PackageRecord) map[string]*record.PackageRecord {
	m := map[string]*record.PackageRecord{}
	for _, r := range recs {
		m[r.Fn] = r
	}
	return m
}

func ctxT(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// decodeShardForTest decodes uncompressed shard msgpack into placed records.
func decodeShardForTest(t testing.TB, data []byte, name string, py *pythonMinor) ([]*record.PackageRecord, error) {
	t.Helper()
	sc, err := decodeShard(data, name, py, interner{})
	if err != nil {
		return nil, err
	}
	base, _ := resolveBase(mustURL(t, "https://anaconda.org/conda-forge/linux-64/repodata_shards.msgpack.zst"), "", urlPolicy{})
	loc := &locator{channel: "https://anaconda.org/conda-forge", subdir: "linux-64", base: base}
	recs, err := sc.place(loc)
	if err != nil {
		return nil, err
	}
	return finalize(recs, sc.removed), nil
}
