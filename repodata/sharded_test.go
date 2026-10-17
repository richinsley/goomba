package repodata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

// shardRec builds a shard record with a checksum.
func shardRec(name, version, build string, depends ...string) omap {
	m := omap{{"name", name}, {"version", version}, {"build", build}, {"sha256", testSHA}}
	if len(depends) > 0 {
		m = append(m, kv{"depends", depends})
	}
	return m
}

// simpleShard is a shard with one .conda record name-version-0.
func simpleShard(t testing.TB, name, version string, depends ...string) []byte {
	return shardMP(t, nil, omap{{name + "-" + version + "-0.conda", shardRec(name, version, "0", depends...)}})
}

// serveRealFixtures publishes testdata/conda-forge/noarch under /conda-forge.
func serveRealFixtures(t *testing.T) *testServer {
	ts := newTestServer(t)
	ts.put("/conda-forge/noarch/"+shardIndexFile, readTestdata(t, "conda-forge/noarch/"+shardIndexFile))
	for _, h := range realShards() {
		ts.put("/conda-forge/noarch/"+h+".msgpack.zst", readTestdata(t, "conda-forge/noarch/"+h+".msgpack.zst"))
	}
	return ts
}

func TestShardedChannelRealFixtures(t *testing.T) {
	ts := serveRealFixtures(t)
	opt := testOptions(t, "noarch")
	ctx := ctxT(t)
	c, err := NewShardedChannel(ctx, ts.url("/conda-forge"), opt)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.Names()); n != 23942 {
		t.Errorf("Names: %d", n)
	}
	recs, err := c.Records(ctx, "six")
	if err != nil {
		t.Fatal(err)
	}
	got := byFn(recs)["six-1.17.0-pyhd8ed1ab_0.conda"]
	if got == nil || got.URL != ts.url("/conda-forge/noarch/six-1.17.0-pyhd8ed1ab_0.conda") || got.Channel != ts.url("/conda-forge") ||
		got.SHA256 != "41db0180680cc67c3fa76544ffd48d6a5679d96f4b71d7498a759e94edc9a2db" {
		t.Errorf("six 1.17.0: %+v", got)
	}
	for _, name := range []string{"wheel", "packaging", "pip", "PIP"} {
		recs, err := c.Records(ctx, name)
		if err != nil || len(recs) == 0 {
			t.Errorf("%s: %d records, %v", name, len(recs), err)
		}
	}
	if s := c.Stats(); s.ShardsFetched != 4 || s.BytesFetched != 723+2408+2496+6326 || s.ShardsFromCache != 0 {
		t.Errorf("stats %+v", s)
	}
	// The shards are cached under their hash; the index under index/.
	for _, h := range realShards() {
		if _, err := os.Stat(filepath.Join(opt.CacheDir, "shards", h+".msgpack.zst")); err != nil {
			t.Error(err)
		}
	}
	// A new channel offline needs no network at all.
	ts.srv.Close()
	opt.Offline = true
	c2, err := NewShardedChannel(ctx, ts.url("/conda-forge"), opt)
	if err != nil {
		t.Fatal(err)
	}
	recs2, err := c2.Records(ctx, "six")
	if err != nil || !reflect.DeepEqual(recs, recs2[:len(recs)]) && len(recs) != len(recs2) {
		t.Errorf("offline: %v", err)
	}
	if s := c2.Stats(); s.ShardsFetched != 0 || s.ShardsFromCache != 1 {
		t.Errorf("offline stats %+v", s)
	}
	if _, err := c2.Records(ctx, "requests"); !errors.Is(err, ErrNotCached) {
		t.Errorf("uncached shard offline: %v", err)
	}
}

// newShardedFixture publishes linux-64 and noarch subdirs with the given
// shards and opens the channel.
func newShardedFixture(t *testing.T, linux, noarch map[string][]byte, opt Options) (*testServer, *ShardedChannel) {
	t.Helper()
	ts := newTestServer(t)
	ts.putShards("/chan", "linux-64", "", "", "", linux)
	ts.putShards("/chan", "noarch", "", "", "", noarch)
	c, err := NewShardedChannel(ctxT(t), ts.url("/chan"), opt)
	if err != nil {
		t.Fatal(err)
	}
	return ts, c
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shards - Basic
// operations", "Shards - build_repodata", "Shards - Error handling" and
// "Shards - fetch_shards with visited cache".
func TestShardedChannelOperations(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	pkg1 := shardMP(t, omap{
		{"pkg1-1.0.0-0.tar.bz2", append(shardRec("pkg1", "1.0.0", "0"),
			kv{"python_site_packages_path", "lib/python3.11/site-packages"}, kv{"legacy_bz2_md5", "legacy-md5"},
			kv{"legacy_bz2_size", uint64(1111)}, kv{"size", uint64(2222)}, kv{"arch", "x86_64"},
			kv{"platform", "linux-64"}, kv{"features", "feature_a"})},
		{"pkg1-2.0.0-0.tar.bz2", shardRec("pkg1", "2.0.0", "0")},
	}, nil)
	pkg2 := shardMP(t, nil, omap{{"pkg2-1.0.0-0.conda", shardRec("pkg2", "1.0.0", "0")}})
	ts.putShards("/conda-forge", "linux-64", "https://example.com/packages", "shards", "shards/", map[string][]byte{"pkg1": pkg1, "pkg2": pkg2})
	ts.putShards("/conda-forge", "noarch", "/", "", "", map[string][]byte{"pkg2": simpleShard(t, "pkg2", "0.5")})
	c, err := NewShardedChannel(ctx, ts.url("/conda-forge"), testOptions(t, "linux-64"))
	if err != nil {
		t.Fatal(err)
	}

	// package_names and contains
	if got := c.Names(); !reflect.DeepEqual(got, []string{"pkg1", "pkg2"}) {
		t.Errorf("Names = %q", got)
	}
	// Empty repodata: nothing loaded yet.
	if s := c.Stats(); s.ShardsFetched != 0 || s.ShardsFromCache != 0 || s.RetainedBytes <= 0 {
		t.Errorf("stats before any load: %+v (the indexes count as retained)", s)
	}
	// non-existent package: an empty answer, not an error (Source contract;
	// libmamba's fetch_shard reports an error).
	if recs, err := c.Records(ctx, "nonexistent"); err != nil || len(recs) != 0 {
		t.Errorf("nonexistent: %v, %v", recs, err)
	}
	// Build repodata with packages: extended fields survive; base_url is
	// absolute for linux-64 and the "/" sentinel (subdir) for noarch.
	recs, err := c.Records(ctx, "pkg1")
	if err != nil {
		t.Fatal(err)
	}
	if got := fns(recs); !reflect.DeepEqual(got, []string{"pkg1-1.0.0-0.tar.bz2", "pkg1-2.0.0-0.tar.bz2"}) {
		t.Errorf("pkg1: %q", got)
	}
	r := recs[0]
	if r.PythonSitePackagesPath != "lib/python3.11/site-packages" || r.Size != 2222 || r.Features != "feature_a" ||
		r.URL != "https://example.com/packages/pkg1-1.0.0-0.tar.bz2" || r.Subdir != "linux-64" {
		t.Errorf("pkg1 record %+v", *r)
	}
	// Build repodata with conda packages and multiple shards (both subdirs).
	recs, err = c.Records(ctx, "pkg2")
	if err != nil {
		t.Fatal(err)
	}
	if got := fns(recs); !reflect.DeepEqual(got, []string{"pkg2-1.0.0-0.conda", "pkg2-0.5-0.conda"}) {
		t.Errorf("pkg2: %q", got)
	}
	if recs[1].URL != ts.url("/conda-forge/noarch/pkg2-0.5-0.conda") {
		t.Errorf("noarch URL %s", recs[1].URL)
	}
	// Shards were fetched from shards_base_url "shards" (linux-64).
	if n := ts.totalRequests("/conda-forge/linux-64/shards/"); n != 2 {
		t.Errorf("linux-64 shard requests: %d", n)
	}
	// fetch_shards with visited cache: no new requests.
	before := ts.totalRequests("/")
	for i := 0; i < 3; i++ {
		if _, err := c.Records(ctx, "pkg1"); err != nil {
			t.Fatal(err)
		}
	}
	if after := ts.totalRequests("/"); after != before {
		t.Errorf("visited shards refetched: %d -> %d requests", before, after)
	}
	// Returned slices are fresh: appending does not leak into the cache.
	a, _ := c.Records(ctx, "pkg1")
	_ = append(a[:1], &record.PackageRecord{Name: "evil"})
	if b, _ := c.Records(ctx, "pkg1"); b[1].Name != "pkg1" {
		t.Error("Records shares its slice")
	}
}

func TestShardsBaseURLOtherHost(t *testing.T) {
	ctx := ctxT(t)
	shards := newTestServer(t)
	z := zst(t, simpleShard(t, "foo", "1.0"))
	sum := sha256.Sum256(z)
	shards.put("/store/"+hex.EncodeToString(sum[:])+".msgpack.zst", z)
	ts := newTestServer(t)
	ts.put("/c/noarch/"+shardIndexFile, zst(t, shardIndexMP(t, "", shards.url("/store/"), "noarch", 1, map[string][]byte{"foo": sum[:]})))
	c, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := c.Records(ctx, "foo")
	if err != nil || len(recs) != 1 || recs[0].URL != ts.url("/c/noarch/foo-1.0-0.conda") {
		t.Fatalf("got %+v, %v", recs, err)
	}
	if shards.count("/store/"+hex.EncodeToString(sum[:])+".msgpack.zst") != 1 {
		t.Error("shard not fetched from shards_base_url host")
	}
}

func TestShardHashMismatch(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	good := zst(t, simpleShard(t, "foo", "1.0"))
	sum := sha256.Sum256(good)
	path := "/c/noarch/" + hex.EncodeToString(sum[:]) + ".msgpack.zst"
	ts.put("/c/noarch/"+shardIndexFile, zst(t, shardIndexMP(t, "", "", "noarch", 1, map[string][]byte{"foo": sum[:]})))
	ts.put(path, zst(t, simpleShard(t, "foo", "6.6.6"))) // tampered
	opt := testOptions(t, "noarch")
	c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Records(ctx, "foo")
	var hm *HashMismatchError
	if !errors.As(err, &hm) || hm.Expected != hex.EncodeToString(sum[:]) {
		t.Fatalf("err = %v, want HashMismatchError", err)
	}
	if ts.count(path) != 1 {
		t.Errorf("hash mismatch retried: %d requests", ts.count(path))
	}
	if _, err := os.Stat(filepath.Join(opt.CacheDir, "shards", hex.EncodeToString(sum[:])+".msgpack.zst")); !os.IsNotExist(err) {
		t.Error("mismatching shard was cached")
	}
	// The failure is not cached: once the server is fixed, Records succeeds.
	ts.put(path, good)
	if recs, err := c.Records(ctx, "foo"); err != nil || len(recs) != 1 {
		t.Errorf("after fix: %v, %v", recs, err)
	}
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shards - Disk caching".
func TestShardDiskCache(t *testing.T) {
	ctx := ctxT(t)
	z := zst(t, shardMP(t, omap{{"test-pkg-1.0.0-0.tar.bz2", shardRec("test-pkg", "1.0.0", "0", "dep1")}}, nil))
	sum := sha256.Sum256(z)
	hexSum := hex.EncodeToString(sum[:])
	setup := func(t *testing.T, cached []byte) (*testServer, Options, string) {
		ts := newTestServer(t)
		ts.put("/c/noarch/"+shardIndexFile, zst(t, shardIndexMP(t, "", "", "noarch", 1, map[string][]byte{"test-pkg": sum[:]})))
		ts.put("/c/noarch/"+hexSum+".msgpack.zst", z)
		opt := testOptions(t, "noarch")
		p := filepath.Join(opt.CacheDir, "shards", hexSum+".msgpack.zst")
		if cached != nil {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, cached, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// Load the index online once so offline mode has it.
		if _, err := NewShardedChannel(ctx, ts.url("/c"), opt); err != nil {
			t.Fatal(err)
		}
		return ts, opt, p
	}
	open := func(t *testing.T, ts *testServer, opt Options) *ShardedChannel {
		c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	t.Run("cache hit - shard loaded from disk cache", func(t *testing.T) {
		ts, opt, _ := setup(t, z)
		opt.Offline = true
		recs, err := open(t, ts, opt).Records(ctx, "test-pkg")
		if err != nil || len(recs) != 1 || recs[0].Name != "test-pkg" || recs[0].Version != "1.0.0" {
			t.Fatalf("got %+v, %v", recs, err)
		}
	})
	t.Run("cache miss - shard not in cache", func(t *testing.T) {
		ts, opt, _ := setup(t, nil)
		opt.Offline = true
		if _, err := open(t, ts, opt).Records(ctx, "test-pkg"); !errors.Is(err, ErrNotCached) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cache invalidation - hash mismatch", func(t *testing.T) {
		ts, opt, p := setup(t, zst(t, shardMP(t, omap{{"wrong-pkg-2.0.0-1.tar.bz2", shardRec("wrong-pkg", "2.0.0", "1")}}, nil)))
		off := opt
		off.Offline = true
		if _, err := open(t, ts, off).Records(ctx, "test-pkg"); !errors.Is(err, ErrNotCached) {
			t.Fatalf("offline with a wrong cached shard: err = %v", err)
		}
		// Online, the bad cache file is replaced by the verified download.
		recs, err := open(t, ts, opt).Records(ctx, "test-pkg")
		if err != nil || len(recs) != 1 {
			t.Fatalf("online: %+v, %v", recs, err)
		}
		if data, _ := os.ReadFile(p); !bytes.Equal(data, z) {
			t.Error("cache file not replaced")
		}
	})
	t.Run("cache invalidation - corrupted file", func(t *testing.T) {
		ts, opt, _ := setup(t, []byte("corrupted data"))
		opt.Offline = true
		if _, err := open(t, ts, opt).Records(ctx, "test-pkg"); !errors.Is(err, ErrNotCached) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cache path computation - verify cache file location", func(t *testing.T) {
		ts, opt, p := setup(t, nil)
		if _, err := open(t, ts, opt).Records(ctx, "test-pkg"); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(p); err != nil || !bytes.Equal(data, z) {
			t.Fatalf("cache file %s: %v", p, err)
		}
		// Shards are content-addressed: never revalidated, also with MaxAge 0.
		before := ts.count("/c/noarch/" + hexSum + ".msgpack.zst")
		if _, err := open(t, ts, opt).Records(ctx, "test-pkg"); err != nil {
			t.Fatal(err)
		}
		if after := ts.count("/c/noarch/" + hexSum + ".msgpack.zst"); after != before {
			t.Errorf("cached shard refetched")
		}
	})
}

func TestShardedChannelErrors(t *testing.T) {
	ctx := ctxT(t)
	t.Run("not sharded", func(t *testing.T) {
		ts := newTestServer(t)
		ts.put("/c/noarch/repodata.json", []byte(`{"packages": {}}`))
		if _, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "linux-64")); !errors.Is(err, ErrNotSharded) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("platform not sharded", func(t *testing.T) {
		ts := newTestServer(t)
		ts.putShards("/c", "noarch", "", "", "", map[string][]byte{})
		if _, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "linux-64")); !errors.Is(err, ErrNotSharded) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("info.subdir mismatch", func(t *testing.T) {
		ts := newTestServer(t)
		ts.put("/c/noarch/"+shardIndexFile, zst(t, shardIndexMP(t, "", "", "linux-64", 1, nil)))
		if _, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch")); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad index", func(t *testing.T) {
		ts := newTestServer(t)
		ts.put("/c/noarch/"+shardIndexFile, []byte("not zstd"))
		if _, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch")); err == nil {
			t.Error("garbage index accepted")
		}
	})
	t.Run("http base_url", func(t *testing.T) {
		ts := newTestServer(t)
		ts.put("/c/noarch/"+shardIndexFile, zst(t, shardIndexMP(t, "http://evil.example.com/", "", "noarch", 1, nil)))
		if _, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch")); !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		ts := newTestServer(t)
		ts.handle("/c/noarch/"+shardIndexFile, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) })
		_, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch"))
		var he *HTTPError
		if !errors.As(err, &he) || he.StatusCode != http.StatusForbidden {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing shard", func(t *testing.T) {
		ts := newTestServer(t)
		ts.put("/c/noarch/"+shardIndexFile, zst(t, shardIndexMP(t, "", "", "noarch", 1, map[string][]byte{"foo": bytes.Repeat([]byte{1}, 32)})))
		c, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch"))
		if err != nil {
			t.Fatal(err)
		}
		var he *HTTPError
		if _, err := c.Records(ctx, "foo"); !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad options", func(t *testing.T) {
		for _, opt := range []Options{
			{Platform: "linux-64"},
			{CacheDir: t.TempDir()},
			{CacheDir: t.TempDir(), Platform: "linux/64"},
			{CacheDir: t.TempDir(), Platform: "linux-64", MaxAge: -1},
			{CacheDir: t.TempDir(), Platform: "linux-64", Concurrency: -1},
			{CacheDir: t.TempDir(), Platform: "linux-64", PythonMinor: "three"},
		} {
			if _, err := NewShardedChannel(ctx, "https://example.com/c", opt); err == nil {
				t.Errorf("options %+v accepted", opt)
			}
		}
		if _, err := NewShardedChannel(ctx, "http://example.com/c", testOptions(t, "noarch")); !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("http channel: %v", err)
		}
	})
}

// Cases from libmamba tests/src/core/test_shard_traversal.cpp
// "RepodataSubset reachable *".
func TestWalk(t *testing.T) {
	ctx := ctxT(t)
	linux := map[string][]byte{
		"python": simpleShard(t, "python", "3.11", "numpy", "libzstd >=1.5", "__glibc >=2.17"),
		"numpy":  simpleShard(t, "numpy", "1.24", "libffi", "python >=3.11,<3.12.0a0"),
		"libffi": simpleShard(t, "libffi", "1.0"),
		"unused": simpleShard(t, "unused", "1.0"),
	}
	noarch := map[string][]byte{
		"libzstd": simpleShard(t, "libzstd", "1.5", "tzdata"),
		"tzdata":  simpleShard(t, "tzdata", "2024a"),
	}

	for _, strategy := range []string{"bfs", "pipelined", "default"} {
		t.Run("reachable "+strategy, func(t *testing.T) {
			ts, c := newShardedFixture(t, linux, noarch, testOptions(t, "linux-64"))
			got, err := c.Walk(ctx, []string{"python"})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"libffi", "libzstd", "numpy", "python", "tzdata"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Walk = %q, want %q", got, want)
			}
			if s := c.Stats(); s.ShardsFetched != 5 {
				t.Errorf("stats %+v", s)
			}
			// Everything walked is answered without new requests.
			before := ts.totalRequests("/")
			for _, n := range want {
				if recs, err := c.Records(ctx, n); err != nil || len(recs) != 1 {
					t.Errorf("%s: %v %v", n, recs, err)
				}
			}
			if ts.totalRequests("/") != before {
				t.Error("walked shards refetched")
			}
		})
	}
	t.Run("empty root_packages returns early", func(t *testing.T) {
		_, c := newShardedFixture(t, linux, noarch, testOptions(t, "linux-64"))
		if got, err := c.Walk(ctx, nil); err != nil || len(got) != 0 {
			t.Errorf("got %q, %v", got, err)
		}
	})
	t.Run("package not in any shard", func(t *testing.T) {
		_, c := newShardedFixture(t, linux, noarch, testOptions(t, "linux-64"))
		if got, err := c.Walk(ctx, []string{"nonexistent_package"}); err != nil || len(got) != 0 {
			t.Errorf("got %q, %v", got, err)
		}
		if c.Stats().ShardsFetched != 0 {
			t.Error("fetched shards")
		}
	})
	t.Run("multiple channels", func(t *testing.T) {
		_, cf := newShardedFixture(t, map[string][]byte{"python": simpleShard(t, "python", "3.11")}, nil, testOptions(t, "linux-64"))
		_, defaults := newShardedFixture(t, map[string][]byte{"python": simpleShard(t, "python", "3.10")}, nil, testOptions(t, "linux-64"))
		m := NewMulti(cf, defaults)
		pctx, cancel := context.WithCancel(ctx)
		defer cancel()
		m.Prefetch(pctx, []string{"python"})
		deadline := time.Now().Add(time.Minute)
		for cf.Stats().ShardsFetched+defaults.Stats().ShardsFetched < 2 {
			if time.Now().After(deadline) {
				t.Fatal("prefetch did not finish")
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Strict priority: the first channel wins.
		recs, err := m.Records(ctx, "python")
		if err != nil || len(recs) != 1 || recs[0].Version != "3.11" {
			t.Errorf("Multi.Records = %+v, %v", recs, err)
		}
	})
	t.Run("failures are reported and do not stop the walk", func(t *testing.T) {
		ts, c := newShardedFixture(t, linux, noarch, testOptions(t, "linux-64"))
		z := zst(t, linux["numpy"])
		sum := sha256.Sum256(z)
		ts.del("/chan/linux-64/" + hex.EncodeToString(sum[:]) + ".msgpack.zst")
		got, err := c.Walk(ctx, []string{"python", "libffi"})
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("err = %v", err)
		}
		if want := []string{"libffi", "libzstd", "python", "tzdata"}; !reflect.DeepEqual(got, want) {
			t.Errorf("Walk = %q, want %q", got, want)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		_, c := newShardedFixture(t, linux, noarch, testOptions(t, "linux-64"))
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := c.Walk(cctx, []string{"python"}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("python minor prefilter prunes the walk", func(t *testing.T) {
		opt := testOptions(t, "linux-64")
		opt.PythonMinor = "3.12"
		_, c := newShardedFixture(t, linux, noarch, opt)
		got, err := c.Walk(ctx, []string{"python"})
		if err != nil {
			t.Fatal(err)
		}
		// numpy's only build needs python 3.11, so libffi is never reached.
		if want := []string{"libzstd", "numpy", "python", "tzdata"}; !reflect.DeepEqual(got, want) {
			t.Errorf("Walk = %q, want %q", got, want)
		}
		if recs, _ := c.Records(ctx, "numpy"); len(recs) != 0 {
			t.Errorf("numpy kept: %+v", recs)
		}
	})
}

// Many goroutines asking for the same names load each shard once.
func TestShardedConcurrentRecords(t *testing.T) {
	ctx := ctxT(t)
	shards := map[string][]byte{}
	for i := 0; i < 20; i++ {
		n := fmt.Sprintf("pkg%02d", i)
		shards[n] = simpleShard(t, n, "1.0")
	}
	ts, c := newShardedFixture(t, shards, nil, Options{CacheDir: t.TempDir(), Platform: "linux-64", Concurrency: 3})
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				recs, err := c.Records(ctx, fmt.Sprintf("pkg%02d", (i+g)%20))
				if err != nil || len(recs) != 1 {
					errs <- fmt.Errorf("%v %v", recs, err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := ts.totalRequests("/chan/linux-64/") - 1; n != 20 { // minus the index
		t.Errorf("%d shard requests, want 20", n)
	}
}

// A shared load whose initiator is cancelled is retried by other callers.
func TestShardedRecordsCancelledInitiator(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	z := zst(t, simpleShard(t, "foo", "1.0"))
	sum := sha256.Sum256(z)
	path := "/c/noarch/" + hex.EncodeToString(sum[:]) + ".msgpack.zst"
	ts.put("/c/noarch/"+shardIndexFile, zst(t, shardIndexMP(t, "", "", "noarch", 1, map[string][]byte{"foo": sum[:]})))
	release := make(chan struct{})
	var first sync.Once
	ts.handle(path, func(w http.ResponseWriter, r *http.Request) {
		block := false
		first.Do(func() { block = true })
		if block {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write(z)
	})
	c, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch"))
	if err != nil {
		t.Fatal(err)
	}
	ictx, cancel := context.WithCancel(ctx)
	initiator := make(chan error, 1)
	go func() { _, err := c.Records(ictx, "foo"); initiator <- err }()
	for ts.count(path) == 0 {
		time.Sleep(time.Millisecond)
	}
	waiter := make(chan error, 1)
	go func() {
		recs, err := c.Records(ctx, "foo")
		if err == nil && len(recs) != 1 {
			err = fmt.Errorf("got %d records", len(recs))
		}
		waiter <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-initiator; !errors.Is(err, context.Canceled) {
		t.Errorf("initiator: %v", err)
	}
	if err := <-waiter; err != nil {
		t.Errorf("waiter: %v", err)
	}
	close(release)
}

func TestShardedRedirectPolicy(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	ts.handle("/c/noarch/"+shardIndexFile, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/index", http.StatusFound)
	})
	_, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch"))
	if !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("err = %v", err)
	}
	if n := ts.count("/c/noarch/" + shardIndexFile); n != 1 {
		t.Errorf("refused redirect retried: %d", n)
	}
	// A redirect to another loopback path is fine.
	ts2 := newTestServer(t)
	ts2.putShards("/real", "noarch", "", "", "", map[string][]byte{"foo": simpleShard(t, "foo", "1")})
	ts.handle("/c/noarch/"+shardIndexFile, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ts2.url("/real/noarch/"+shardIndexFile), http.StatusFound)
	})
	if _, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch")); err != nil {
		t.Errorf("loopback redirect: %v", err)
	}
}

func TestNewChannelFallsBack(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	ts.put("/c/noarch/repodata.json", readTestdata(t, "micromamba/test-server/noarch/repodata.json"))
	src, err := NewChannel(ctx, ts.url("/c"), testOptions(t, "linux-64"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(*FullIndex); !ok {
		t.Fatalf("got %T, want *FullIndex", src)
	}
	recs, err := src.Records(ctx, "test-package")
	if err != nil || len(recs) != 1 {
		t.Errorf("got %+v, %v", recs, err)
	}
	ts.putShards("/s", "linux-64", "", "", "", nil)
	ts.putShards("/s", "noarch", "", "", "", nil)
	if src, err := NewChannel(ctx, ts.url("/s"), testOptions(t, "linux-64")); err != nil {
		t.Fatal(err)
	} else if _, ok := src.(*ShardedChannel); !ok {
		t.Fatalf("got %T, want *ShardedChannel", src)
	}
	if _, err := NewChannel(ctx, ts.url("/none"), testOptions(t, "linux-64")); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty channel: %v", err)
	}
}

func TestNetworkErrorsRedactTokens(t *testing.T) {
	ts := newTestServer(t)
	u := ts.url("/t/s3cr3t/c")
	ts.srv.Close() // connection refused
	_, err := NewShardedChannel(ctxT(t), u, testOptions(t, "noarch"))
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error %v leaks the token", err)
	}
}

// Independent channels (as separate processes would) share one cache
// directory concurrently; every write is atomic, so all of them succeed and
// the cache ends up valid.
func TestSharedCacheDirConcurrentChannels(t *testing.T) {
	ctx := ctxT(t)
	shards := map[string][]byte{}
	var names []string
	for i := 0; i < 10; i++ {
		n := fmt.Sprintf("pkg%d", i)
		shards[n] = simpleShard(t, n, "1.0")
		names = append(names, n)
	}
	ts := newTestServer(t)
	ts.putShards("/c", "linux-64", "", "", "", shards)
	ts.putShards("/c", "noarch", "", "", "", nil)
	ts.put("/f/noarch/repodata.json", []byte(noarchDoc))
	opt := testOptions(t, "linux-64")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
			if err != nil {
				errs <- err
				return
			}
			if _, err := c.Walk(ctx, names); err != nil {
				errs <- err
			}
			if _, err := NewFullIndex(ctx, ts.url("/f"), opt); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	opt.Offline = true
	c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Walk(ctx, names); err != nil || len(got) != 10 {
		t.Errorf("offline after concurrent writes: %q, %v", got, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(opt.CacheDir, "*", ".tmp-*")); len(leftovers) != 0 {
		t.Errorf("temp files left: %q", leftovers)
	}
}

// S6: offline, NewChannel falls back to a cached full index when no shard
// index was ever cached.
func TestNewChannelOfflineFallsBackToFullIndex(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	ts.put("/c/noarch/repodata.json", readTestdata(t, "micromamba/test-server/noarch/repodata.json"))
	opt := testOptions(t, "noarch")
	if _, err := NewFullIndex(ctx, ts.url("/c"), opt); err != nil { // caches the full index only
		t.Fatal(err)
	}
	ts.srv.Close()
	opt.Offline = true
	src, err := NewChannel(ctx, ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(*FullIndex); !ok {
		t.Fatalf("got %T", src)
	}
	if recs, _ := src.Records(ctx, "test-package"); len(recs) != 1 {
		t.Errorf("got %d records", len(recs))
	}
	// Offline with nothing cached at all is still an error.
	if _, err := NewChannel(ctx, ts.url("/other"), opt); !errors.Is(err, ErrNotCached) {
		t.Errorf("empty cache: %v", err)
	}
}

// N7: Options.Concurrency is capped.
func TestConcurrencyCap(t *testing.T) {
	for in, want := range map[int]int{0: DefaultConcurrency, 1: 1, 64: 64, 1000: MaxConcurrency} {
		s, err := Options{CacheDir: "x", Platform: "noarch", Concurrency: in}.settings()
		if err != nil || s.concurrency != want {
			t.Errorf("Concurrency %d: got %d, %v", in, s.concurrency, err)
		}
	}
}

// N3: a record is kept only under the file name name-version-build.ext.
func TestFileNameMustMatchRecord(t *testing.T) {
	r := shardRec("pkg", "1.0", "0")
	recs := decodeOne(t, shardMP(t, omap{
		{"pkg-1.0-0.tar.bz2", r},
		{"pkg-1.0-1.tar.bz2", r},
		{"other-1.0-0.tar.bz2", r},
		{"PKG-1.0-0.tar.bz2", r},
	}, omap{{"pkg-1.0-0.conda", r}, {"pkg-1.0-0.tar.bz2.conda", r}}))
	if got := fns(recs); !reflect.DeepEqual(got, []string{"pkg-1.0-0.conda"}) {
		t.Errorf("got %q", got)
	}
}

// N12: several processes share one cache directory. The test binary runs
// itself as helper processes (TestCacheHelperProcess) against one server.
func TestCacheMultiProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	ctx := ctxT(t)
	ts := newTestServer(t)
	shards := map[string][]byte{}
	var names []string
	for i := 0; i < 12; i++ {
		n := fmt.Sprintf("pkg%d", i)
		shards[n] = simpleShard(t, n, "1.0")
		names = append(names, n)
	}
	ts.putShards("/c", "linux-64", "", "", "", shards)
	ts.putShards("/c", "noarch", "", "", "", nil)
	ts.put("/f/noarch/repodata.json", []byte(noarchDoc))
	cacheDir := t.TempDir()
	var cmds []*exec.Cmd
	var outs []*bytes.Buffer
	for i := 0; i < 4; i++ {
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCacheHelperProcess$", "-test.count=1", "-test.v",
			"--", "repodata-cache-helper", ts.url("/c"), ts.url("/f"), cacheDir)
		out := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, outs = append(cmds, cmd), append(outs, out)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil || !strings.Contains(outs[i].String(), "helper ok") {
			t.Errorf("helper %d: %v\n%s", i, err, outs[i])
		}
	}
	opt := Options{CacheDir: cacheDir, Platform: "linux-64", Offline: true}
	c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Walk(ctx, names); err != nil || len(got) != len(names) {
		t.Errorf("offline after the helpers: %q, %v", got, err)
	}
	if _, err := NewFullIndex(ctx, ts.url("/f"), opt); err != nil {
		t.Errorf("full index offline: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(cacheDir, "*", ".tmp-*")); len(leftovers) != 0 {
		t.Errorf("temp files left: %q", leftovers)
	}
}

// TestCacheHelperProcess is the helper of TestCacheMultiProcess; run
// normally it skips.
func TestCacheHelperProcess(t *testing.T) {
	args := flag.Args()
	if len(args) != 4 || args[0] != "repodata-cache-helper" {
		t.Skip("helper process for TestCacheMultiProcess")
	}
	ctx := ctxT(t)
	opt := Options{CacheDir: args[3], Platform: "linux-64", Concurrency: 4}
	for round := 0; round < 3; round++ {
		c, err := NewShardedChannel(ctx, args[1], opt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Walk(ctx, c.Names()); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFullIndex(ctx, args[2], opt); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("helper ok")
}
