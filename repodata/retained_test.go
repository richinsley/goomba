package repodata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// R3-1(a): base_url and shards_base_url are capped at maxBaseURL bytes.
func TestBaseURLCap(t *testing.T) {
	base := func(n int) string {
		return "https://mirror.example.com/" + strings.Repeat("a", n-len("https://mirror.example.com/"))
	}
	for _, c := range []struct {
		n  int
		ok bool
	}{{maxBaseURL, true}, {maxBaseURL + 1, false}} {
		doc := fmt.Sprintf(`{"info": {"subdir": "noarch", "base_url": %q}, "packages": {}}`, base(c.n))
		_, err := LoadRepodataJSON(strings.NewReader(doc), cfChannel, "noarch")
		if (err == nil) != c.ok {
			t.Errorf("JSON base_url of %d bytes: %v", c.n, err)
		}
		for _, key := range []string{"base_url", "shards_base_url"} {
			_, err := decodeShardIndex(mpEncode(t, omap{{"info", omap{{key, base(c.n)}}}, {"shards", omap{}}}))
			if (err == nil) != c.ok {
				t.Errorf("shard index %s of %d bytes: %v", key, c.n, err)
			}
		}
	}
}

// longBaseJSON is the reviewer's shape: many records whose URLs carry a
// long base URL and, optionally, long names that expand when escaped.
func longBaseJSON(nrec int, baseLen int, name string) []byte {
	base := "https://mirror.example.com/" + strings.Repeat("a", baseLen) + "/"
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"info":{"subdir":"noarch","base_url":%q},"packages.conda":{`, base)
	for i := 0; i < nrec; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%s-%d-0.conda":{"name":"%s","version":"%d","build":"0"}`, name, i, name, i)
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

// R3-1(b): record URLs are charged when placed, so retained memory stays
// within the budget even when every URL is long.
func TestRecordURLsAreCharged(t *testing.T) {
	ch, pol, _ := canonicalChannel(cfChannel)
	for _, c := range []struct {
		name string
		base int
		pkg  string
	}{
		{"short base", 0, "p"},
		{"480-byte base_url", 480, "p"},
		{"480-byte base_url, escaped non-ASCII name", 480, strings.Repeat("é", 300)},
	} {
		doc := longBaseJSON(20000, c.base, c.pkg)
		var b *budget
		got, keep := retained(func() any {
			recs, _, err := loadRepodataJSONBudget(bytes.NewReader(doc), ch, pol, "noarch", nil, func(in func() int64) *budget {
				b = &budget{base: 1 << 50, input: in}
				return b
			})
			if err != nil {
				t.Fatal(err)
			}
			return recs
		})
		runtime.KeepAlive(keep)
		t.Logf("%s: %.1f MiB input, retained %.1f MiB, charged %.1f MiB", c.name, mib(uint64(len(doc))), mib(got), mib(uint64(b.used)))
		if uint64(b.used) < got {
			t.Errorf("%s: charged %.1f MiB < retained %.1f MiB", c.name, mib(uint64(b.used)), mib(got))
		}
		// With the real budget the same document either fits or fails.
		var recs []*record.PackageRecord
		var err error
		got, keep = retained(func() any {
			recs, _, err = loadRepodataJSON(bytes.NewReader(doc), ch, pol, "noarch", nil)
			return recs
		})
		runtime.KeepAlive(keep)
		limit := uint64(jsonBudgetBase + jsonBudgetRatio*len(doc))
		if err == nil && got > limit {
			t.Errorf("%s: retained %.1f MiB over the %.1f MiB limit", c.name, mib(got), mib(limit))
		}
		if err != nil && !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// The shard path charges URLs too.
	d, _ := newZstdDecoder(maxShardDecoded, 1)
	idxURL := subdirURL(ch, "noarch").JoinPath(shardIndexFile)
	base, err := resolveBase(idxURL, "https://mirror.example.com/"+strings.Repeat("a", 480)+"/", pol)
	if err != nil {
		t.Fatal(err)
	}
	loc := &locator{channel: ch.String(), subdir: "noarch", base: base}
	name := strings.Repeat("é", 300)
	var p omap
	for i := 0; i < 8000; i++ {
		p = append(p, kv{fmt.Sprintf("%s-%d-0.conda", name, i), omap{{"name", name}, {"version", strconv.Itoa(i)}, {"build", "0"}, {"md5", bytes.Repeat([]byte{1}, 16)}}})
	}
	comp := zst(t, shardMP(t, nil, p))
	var charged int64
	got, keep := retained(func() any {
		recs, c, err := decodeShardFile(d, comp, name, loc, nil)
		if err != nil {
			t.Fatal(err)
		}
		charged = c
		return recs
	})
	runtime.KeepAlive(keep)
	t.Logf("shard with long URLs: retained %.1f MiB, charged %.1f MiB", mib(got), mib(uint64(charged)))
	if uint64(charged) < got {
		t.Errorf("shard: charged %.1f MiB < retained %.1f MiB", mib(uint64(charged)), mib(got))
	}
}

// chainShard is a shard of n records of name, each depending on next.
func chainShard(name, next string, n int) []byte {
	var recs bytes.Buffer
	pad := strings.Repeat("x", 100)
	for i := 0; i < n; i++ {
		ver := strconv.Itoa(i)
		fn := name + "-" + ver + "-0.conda"
		recs.WriteByte(byte(0xa0 | len(fn)))
		recs.WriteString(fn)
		recs.WriteByte(0x86)
		for _, f := range [][2]string{{"name", name}, {"version", ver}, {"build", "0"}} {
			recs.WriteByte(byte(0xa0 | len(f[0])))
			recs.WriteString(f[0])
			recs.WriteByte(byte(0xa0 | len(f[1])))
			recs.WriteString(f[1])
		}
		recs.Write([]byte{0xa3, 'm', 'd', '5', 0xc4, 16})
		recs.Write(make([]byte, 16))
		recs.Write([]byte{0xa7, 'd', 'e', 'p', 'e', 'n', 'd', 's', 0x91, byte(0xa0 | len(next))})
		recs.WriteString(next)
		recs.Write([]byte{0xa2, 'z', 'z', 0xd9, byte(len(pad))})
		recs.WriteString(pad)
	}
	var b bytes.Buffer
	b.Write([]byte{0x81, 0xae})
	b.WriteString("packages.conda")
	b.WriteByte(0xdf)
	_ = binary.Write(&b, binary.BigEndian, uint32(n))
	b.Write(recs.Bytes())
	return b.Bytes()
}

// R3-2: a channel's shards add up against Options.MaxRetained. The
// reviewer's shape: a chain of shards, each listing the next, that a walk
// loads one after another.
func TestMaxRetainedAcrossShards(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	shards := map[string][]byte{}
	const nshards, nrec = 10, 20000
	for s := 0; s < nshards; s++ {
		shards["q"+strconv.Itoa(s)] = chainShard("q"+strconv.Itoa(s), "q"+strconv.Itoa(s+1), nrec)
	}
	ts.putShards("/c", "noarch", "", "", "", shards)

	// Unbounded (the default is far above this), the walk keeps everything.
	full, err := NewShardedChannel(ctx, ts.url("/c"), testOptions(t, "noarch"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := full.Walk(ctx, []string{"q0"}); err != nil || len(got) != nshards {
		t.Fatalf("default limit: %q, %v", got, err)
	}
	perShard := full.Stats().RetainedBytes / nshards
	t.Logf("each shard is charged %.1f MiB", mib(uint64(perShard)))

	// A limit of about three shards: the walk stops with ErrTooLarge, the
	// channel keeps at most the limit, and the refused shard keeps nothing.
	opt := testOptions(t, "noarch")
	opt.MaxRetained = 3*perShard + perShard/2
	c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	var walkErr error
	var visited []string
	heap, keep := retained(func() any {
		visited, walkErr = c.Walk(ctx, []string{"q0"})
		return c
	})
	runtime.KeepAlive(keep)
	if !errors.Is(walkErr, ErrTooLarge) {
		t.Fatalf("walk: %v", walkErr)
	}
	s := c.Stats()
	t.Logf("limit %.1f MiB: visited %d shards, charged %.1f MiB, heap retained %.1f MiB", mib(uint64(opt.MaxRetained)), len(visited), mib(uint64(s.RetainedBytes)), mib(heap))
	if len(visited) < 2 || len(visited) >= nshards || s.RetainedBytes > opt.MaxRetained {
		t.Errorf("visited %q, charged %d over the limit %d", visited, s.RetainedBytes, opt.MaxRetained)
	}
	if heap > uint64(opt.MaxRetained) {
		t.Errorf("heap retained %.1f MiB over MaxRetained %.1f MiB", mib(heap), mib(uint64(opt.MaxRetained)))
	}
	// Asking again for the refused shard fails again and charges nothing.
	refused := "q" + strconv.Itoa(len(visited))
	before := c.Stats().RetainedBytes
	if _, err := c.Records(ctx, refused); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Records(%s): %v", refused, err)
	}
	if after := c.Stats().RetainedBytes; after != before {
		t.Errorf("a refused shard changed RetainedBytes %d -> %d", before, after)
	}
	// Walking again stops at the same shard.
	if _, err := c.Walk(ctx, []string{"q0"}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("second walk: %v", err)
	}
}

// R3-2: FullIndex honours MaxRetained over all its subdirs.
func TestMaxRetainedFullIndex(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	doc := longBaseJSON(20000, 0, "p")
	ts.put("/c/noarch/repodata.json", doc)
	opt := testOptions(t, "noarch")
	if _, err := NewFullIndex(ctx, ts.url("/c"), opt); err != nil {
		t.Fatal(err)
	}
	opt.MaxRetained = 1 << 20
	if _, err := NewFullIndex(ctx, ts.url("/c"), opt); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "MaxRetained") {
		t.Errorf("small MaxRetained: %v", err)
	}
	if _, err := (Options{CacheDir: "x", Platform: "noarch", MaxRetained: -1}).settings(); err == nil {
		t.Error("negative MaxRetained accepted")
	}
	if s, _ := (Options{CacheDir: "x", Platform: "noarch"}).settings(); s.maxRetained != DefaultMaxRetained {
		t.Errorf("default MaxRetained %d", s.maxRetained)
	}
}
