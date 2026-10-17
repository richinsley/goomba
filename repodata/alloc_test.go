package repodata

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// allocated reports the bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func mib(n uint64) float64 { return float64(n) / (1 << 20) }

// mpMapHeader is a map32 header claiming n entries.
func mpMapHeader(n uint32) []byte {
	return []byte{0xdf, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func mpArrayHeader(n uint32) []byte {
	return []byte{0xdd, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

// B1(a): a shard index whose "shards" map claims millions of entries (nil
// keys and values) must not presize a map from the claim.
func TestAllocShardIndexHugeMapClaim(t *testing.T) {
	const n = 8 << 20
	data := append([]byte{0x81, 0xa6, 's', 'h', 'a', 'r', 'd', 's'}, mpMapHeader(n)...)
	data = append(data, bytes.Repeat([]byte{0xc0}, 2*n)...)
	var idx *shardIndex
	var err error
	got := allocated(func() { idx, err = decodeShardIndex(data) })
	if err != nil || len(idx.shards) != 0 {
		t.Fatalf("got %v, %v", idx, err)
	}
	if got > 4<<20 {
		t.Errorf("decoding allocated %.1f MiB for a %.1f MiB input", mib(got), mib(uint64(len(data))))
	}
	// The same index compressed (3 KB) is refused at decompression.
	ts := newTestServer(t)
	big := append([]byte{0x81, 0xa6, 's', 'h', 'a', 'r', 'd', 's'}, mpMapHeader(52<<20)...)
	big = append(big, bytes.Repeat([]byte{0xc0}, 2*(52<<20))...)
	ts.put("/c/linux-64/"+shardIndexFile, zst(t, big))
	ts.put("/c/noarch/"+shardIndexFile, zst(t, big))
	got = allocated(func() { _, err = NewShardedChannel(ctxT(t), ts.url("/c"), testOptions(t, "linux-64")) })
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v", err)
	}
	if got > 128<<20 {
		t.Errorf("NewShardedChannel allocated %.1f MiB", mib(got))
	}
}

// B1(b): a "removed" array of millions of empty strings, and a record with
// millions of depends.
func TestAllocShardHugeLists(t *testing.T) {
	removedList := func(n int) []byte {
		data := append([]byte{0x81, 0xa7, 'r', 'e', 'm', 'o', 'v', 'e', 'd'}, mpArrayHeader(uint32(n))...)
		return append(data, bytes.Repeat([]byte{0xa0}, n)...)
	}
	var err error
	// Over the entry cap: an error, without materializing the list.
	got := allocated(func() { _, err = decodeShard(removedList(30<<20), "", nil, interner{}) })
	if err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("30M removed entries: err = %v", err)
	}
	if got > 64<<20 { // the 30 MiB input itself is built inside
		t.Errorf("removed: allocated %.1f MiB", mib(got))
	}
	// At the cap: streamed into a set (one entry).
	data := removedList(maxRemoved)
	got = allocated(func() { _, err = decodeShard(data, "", nil, interner{}) })
	if err != nil {
		t.Fatal(err)
	}
	if got > 1<<20 {
		t.Errorf("removed at the cap: allocated %.1f MiB", mib(got))
	}

	const deps = 10 << 20
	rec := mpEncode(t, shardRec("a", "1", "0"))
	rec[0]++ // one more key
	rec = append(rec, mpEncode(t, "depends")...)
	rec = append(rec, mpArrayHeader(deps)...)
	rec = append(rec, bytes.Repeat([]byte{0xa1, 'a'}, deps)...)
	shard := append(append([]byte{0x81, 0xa8, 'p', 'a', 'c', 'k', 'a', 'g', 'e', 's', 0x81}, mpEncode(t, "a-1-0.tar.bz2")...), rec...)
	var recs int
	got = allocated(func() {
		sc, e := decodeShard(shard, "", nil, interner{})
		err = e
		if sc != nil {
			placed, _ := sc.place(&locator{base: mustURL(t, "https://x/")})
			recs = len(placed)
		}
	})
	if err != nil || recs != 0 {
		t.Fatalf("record with %d depends: %d kept, %v", deps, recs, err)
	}
	if got > 4<<20 {
		t.Errorf("depends: allocated %.1f MiB", mib(got))
	}
	// At the cap the record is kept; one more drops it.
	for _, c := range []struct {
		n    int
		keep bool
	}{{maxListEntries, true}, {maxListEntries + 1, false}} {
		l := make([]string, c.n)
		for i := range l {
			l[i] = "a"
		}
		data := oneShard(t, append(shardRec("a", "1", "0"), kv{"depends", l}), "a-1-0.tar.bz2")
		if got := len(decodeOne(t, data)) == 1; got != c.keep {
			t.Errorf("%d depends: kept = %v", c.n, got)
		}
	}
	long := strings.Repeat("x", maxFieldBytes+1)
	if recs := decodeOne(t, oneShard(t, append(shardRec("a", "1", "0"), kv{"license", long}), "a-1-0.tar.bz2")); len(recs) != 0 {
		t.Error("over-long string kept")
	}
}

// B1(c): repodata.json with a huge "removed" list of empty strings, a
// record with a huge depends list, and a flood of minimal records.
func TestAllocJSONHugeLists(t *testing.T) {
	removedDoc := func(n int) string { return "{\"removed\": [" + strings.Repeat(`"",`, n-1) + `""]}` }
	var err error
	// 256 MiB of `"",` (the reported repro): an error after the entry cap,
	// reading no further.
	doc := removedDoc(256 << 20 / 3)
	start := time.Now()
	got := allocated(func() { _, err = LoadRepodataJSON(strings.NewReader(doc), cfChannel, "noarch") })
	if err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("huge removed list: err = %v", err)
	}
	if got > 4<<20 {
		t.Errorf("removed: allocated %.1f MiB for %.0f MiB of input", mib(got), mib(uint64(len(doc))))
	}
	t.Logf("256 MiB removed list: %.1f MiB allocated, rejected in %v", mib(got), time.Since(start).Round(time.Millisecond))
	// At the cap: streamed into a set.
	doc = removedDoc(maxRemoved)
	got = allocated(func() { _, err = LoadRepodataJSON(strings.NewReader(doc), cfChannel, "noarch") })
	if err != nil {
		t.Fatal(err)
	}
	if got > 1<<20 {
		t.Errorf("removed at the cap: allocated %.1f MiB", mib(got))
	}

	deps := `{"packages": {"a-1-0.tar.bz2": {"name": "a", "version": "1", "build": "0", "depends": [` +
		strings.Repeat(`"a",`, 4<<20) + `"a"]}}}`
	var recs int
	got = allocated(func() {
		r, e := LoadRepodataJSON(strings.NewReader(deps), cfChannel, "noarch")
		recs, err = len(r), e
	})
	if err != nil || recs != 0 {
		t.Fatalf("record with 4M depends: %d kept, %v", recs, err)
	}
	if got > 4<<20 {
		t.Errorf("depends: allocated %.1f MiB", mib(got))
	}

	// Distinct minimal records expand ~10x; the budget stops them.
	var b strings.Builder
	b.WriteString(`{"packages": {`)
	for i := 0; i < 250000; i++ {
		fmt.Fprintf(&b, `"p%d-1-0.tar.bz2":{"name":"p%d","version":"1","build":"0"},`, i, i)
	}
	b.WriteString(`"z-1-0.tar.bz2":{"name":"z","version":"1","build":"0"}}}`)
	flood := b.String()
	got = allocated(func() { _, err = LoadRepodataJSON(strings.NewReader(flood), cfChannel, "noarch") })
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("record flood: err = %v", err)
	}
	limit := uint64(jsonBudgetBase + jsonBudgetRatio*len(flood))
	if got > 3*limit {
		t.Errorf("record flood: allocated %.1f MiB, budget %.1f MiB", mib(got), mib(limit))
	}
}

// S1: whitespace between tokens is consumed once; runs over 64 KiB fail.
func TestJSONWhitespace(t *testing.T) {
	ws := func(n int) string { return strings.Repeat(" ", n) }
	start := time.Now()
	_, err := LoadRepodataJSON(strings.NewReader("{"+ws(64<<20)+`"info":{}}`), cfChannel, "noarch")
	if !errors.Is(err, errJSONWhitespace) {
		t.Errorf("64 MiB of whitespace: err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("rejecting took %v", d)
	}
	for _, doc := range []string{
		"{" + ws(maxJSONWhitespace) + `"info":{}}`,
		ws(maxJSONWhitespace) + "{}" + ws(maxJSONWhitespace),
		`{"info": {"note": "` + ws(maxFieldBytes) + `"}, "x": "` + ws(1<<20) + `"}`, // inside strings
	} {
		if _, err := LoadRepodataJSON(strings.NewReader(doc), cfChannel, "noarch"); err != nil {
			t.Errorf("%d bytes: %v", len(doc), err)
		}
	}
	// Many short runs are linear too.
	doc := "{" + strings.Repeat(`"k":`+ws(1000)+`0,`, 10000) + `"z":0}`
	start = time.Now()
	if _, err := LoadRepodataJSON(strings.NewReader(doc), cfChannel, "noarch"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 20*time.Second { // ~0.1 s; quadratic rescanning would take hours
		t.Errorf("10 MB with short whitespace runs took %v", d)
	}
}

// The full-index path is capped too (decompressed bytes).
func TestLimitedReaderCapsIndex(t *testing.T) {
	r := &limitedReader{r: io.MultiReader(strings.NewReader("{"), strings.NewReader(strings.Repeat(" ", 100))), left: 50}
	if _, err := io.ReadAll(r); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v", err)
	}
}
