package repodata

import (
	"context"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func liveContext(t *testing.T) context.Context {
	t.Helper()
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func isHex64(s string) bool {
	h, ok := normalizeHex(s, 32)
	return ok && h == s
}

// Opens conda-forge linux-64 sharded, loads python, and walks python's
// dependency closure from a cold cache.
func TestLiveCondaForgeSharded(t *testing.T) {
	ctx := liveContext(t)
	opt := Options{CacheDir: t.TempDir(), Platform: "linux-64", MaxAge: time.Hour}
	c, err := NewShardedChannel(ctx, cfChannel, opt)
	if err != nil {
		t.Fatal(err)
	}
	names := c.Names()
	t.Logf("shard indexes list %d names (linux-64 + noarch)", len(names))
	if len(names) < 10000 {
		t.Errorf("only %d names", len(names))
	}
	recs, err := c.Records(ctx, "python")
	if err != nil {
		t.Fatal(err)
	}
	n311 := 0
	for _, r := range recs {
		if strings.HasPrefix(r.Version, "3.11.") && isHex64(r.SHA256) {
			n311++
		}
		if r.Name != "python" || r.Channel != cfChannel || !strings.HasPrefix(r.URL, cfChannel+"/"+r.Subdir+"/") {
			t.Errorf("bad record %+v", *r)
		}
	}
	t.Logf("python: %d records, %d for 3.11 with a sha256", len(recs), n311)
	if n311 == 0 {
		t.Error("no python 3.11 record with a 64-hex sha256")
	}

	// A fresh channel and cache: the walk's cost from cold.
	cold, err := NewShardedChannel(ctx, cfChannel, Options{CacheDir: t.TempDir(), Platform: "linux-64"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	visited, err := cold.Walk(ctx, []string{"python"})
	if err != nil {
		t.Fatal(err)
	}
	s := cold.Stats()
	t.Logf("prefetch python: %d names, %d shards fetched, %d bytes (%.1f MB) in %v",
		len(visited), s.ShardsFetched, s.BytesFetched, float64(s.BytesFetched)/1e6, time.Since(start).Round(time.Millisecond))
	for _, want := range []string{"python", "openssl", "libzlib", "tzdata"} {
		found := false
		for _, v := range visited {
			found = found || v == want
		}
		if !found {
			t.Errorf("walk missed %s", want)
		}
	}
	// Prefetch (asynchronous) on the warm channel completes and adds nothing.
	before := cold.Stats()
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cold.Prefetch(pctx, []string{"python"})
	if again, err := cold.Walk(ctx, []string{"python"}); err != nil || !reflect.DeepEqual(again, visited) {
		t.Errorf("second walk: %d names, %v", len(again), err)
	}
	if after := cold.Stats(); after.ShardsFetched != before.ShardsFetched {
		t.Errorf("warm walk fetched %d more shards", after.ShardsFetched-before.ShardsFetched)
	}
}

// Port of libmamba tests/src/core/test_shard_index_loader.cpp "Download
// and parse numpy shard" (integration), against conda-forge.
func TestLiveNumpyShard(t *testing.T) {
	ctx := liveContext(t)
	c, err := NewShardedChannel(ctx, cfChannel, Options{CacheDir: t.TempDir(), Platform: "linux-64"})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := c.Records(ctx, "numpy")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("no numpy records")
	}
	for _, r := range recs {
		if r.Name != "numpy" || r.Version == "" || r.Build == "" {
			t.Errorf("bad record %+v", *r)
		}
	}
}

// The full noarch index and the sharded index of conda-forge agree on the
// records of a stable package.
func TestLiveFullIndexMatchesShards(t *testing.T) {
	ctx := liveContext(t)
	opt := Options{CacheDir: t.TempDir(), Platform: "noarch"}
	start := time.Now()
	fi, err := NewFullIndex(ctx, cfChannel, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("full noarch index: %d names in %v", len(fi.Names()), time.Since(start).Round(time.Millisecond))
	sc, err := NewShardedChannel(ctx, cfChannel, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"six", "wheel", "tzdata"} {
		a, err := fi.Records(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		b, err := sc.Records(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) == 0 {
			t.Errorf("%s: no records", name)
		}
		// The two indexes are published separately; compare the records
		// both have.
		fb := byFn(b)
		common := 0
		for _, r := range a {
			if s, ok := fb[r.Fn]; ok {
				common++
				if !reflect.DeepEqual(r, s) {
					t.Errorf("%s: full %+v\nshard %+v", r.Fn, *r, *s)
				}
			}
		}
		if common < len(a)*9/10 {
			t.Errorf("%s: only %d of %d records in both indexes", name, common, len(a))
		}
	}
}

// Real DNS names that resolve to loopback (nip.io, localtest.me) never
// connect from a public channel, with goomba's transport or a caller's
// http.Client{} (the DefaultTransport clone).
func TestLiveLoopbackNamesNeverConnect(t *testing.T) {
	liveContext(t)
	port, hits := countingListener(t)
	ln6, err := net.Listen("tcp6", "[::1]:"+port)
	if err == nil {
		t.Cleanup(func() { ln6.Close() })
		go func() {
			for {
				c, err := ln6.Accept()
				if err != nil {
					return
				}
				hits.Add(1)
				c.Close()
			}
		}()
	}
	for _, client := range []*http.Client{nil, {}} {
		f := publicFetcher(t, client, nil)
		for _, host := range []string{"127.0.0.1.nip.io", "localtest.me"} {
			if err := get(f, "https://"+host+":"+port+"/x"); err == nil {
				t.Errorf("%s: request succeeded", host)
			}
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listeners", n)
	}
}
