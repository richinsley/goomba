package repodata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

// heapPeak runs f and returns the highest HeapAlloc above the level before
// it, sampled every millisecond (garbage not yet collected included).
func heapPeak(f func()) uint64 {
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > base.HeapAlloc && m.HeapAlloc-base.HeapAlloc > peak.Load() {
			peak.Store(m.HeapAlloc - base.HeapAlloc)
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			sample()
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	f()
	sample()
	close(stop)
	<-done
	return peak.Load()
}

// retained runs f and returns the heap its result retains after a
// collection, with the result.
func retained(f func() any) (uint64, any) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	keep := f()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(keep)
	if after.HeapAlloc < before.HeapAlloc {
		return 0, keep
	}
	return after.HeapAlloc - before.HeapAlloc, keep
}

func token(i int) string { return strconv.FormatInt(int64(i), 36) }

// uniqueDepsJSON is a repodata.json of records with maxListEntries unique
// short depends each, about size bytes. mismatch gives each record a file
// name other than its name-version-build, so every record is dropped.
func uniqueDepsJSON(size int, mismatch bool) []byte {
	var b bytes.Buffer
	b.Grow(size + 1<<16)
	b.WriteString(`{"info":{"subdir":"noarch"},"packages":{`)
	ctr := 0
	for rec := 0; b.Len() < size; rec++ {
		if rec > 0 {
			b.WriteByte(',')
		}
		fnBuild := "0"
		if mismatch {
			fnBuild = "1"
		}
		fmt.Fprintf(&b, `"p-%d-%s.tar.bz2":{"name":"p","version":"%d","build":"0","sha256":"%064x","depends":[`, rec, fnBuild, rec, rec)
		for k := 0; k < maxListEntries; k++ {
			if k > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"` + token(ctr) + `"`)
			ctr++
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

// uniqueDepsShard is the msgpack equivalent, about size bytes.
func uniqueDepsShard(size int, mismatch bool) []byte {
	var recs bytes.Buffer
	n, ctr := 0, 0
	for recs.Len() < size {
		fnBuild := "0"
		if mismatch {
			fnBuild = "1"
		}
		fn := fmt.Sprintf("p-%d-%s.conda", n, fnBuild)
		recs.WriteByte(byte(0xa0 | len(fn)))
		recs.WriteString(fn)
		recs.WriteByte(0x85)
		for _, f := range [][2]string{{"name", "p"}, {"version", strconv.Itoa(n)}, {"build", "0"}} {
			recs.WriteByte(byte(0xa0 | len(f[0])))
			recs.WriteString(f[0])
			recs.WriteByte(byte(0xa0 | len(f[1])))
			recs.WriteString(f[1])
		}
		recs.Write([]byte{0xa3, 'm', 'd', '5', 0xc4, 16})
		recs.Write(make([]byte, 16))
		recs.Write([]byte{0xa7, 'd', 'e', 'p', 'e', 'n', 'd', 's', 0xdc})
		_ = binary.Write(&recs, binary.BigEndian, uint16(maxListEntries))
		for k := 0; k < maxListEntries; k++ {
			s := token(ctr)
			ctr++
			recs.WriteByte(byte(0xa0 | len(s)))
			recs.WriteString(s)
		}
		n++
	}
	var b bytes.Buffer
	b.Write([]byte{0x81, 0xae})
	b.WriteString("packages.conda")
	b.WriteByte(0xdf)
	_ = binary.Write(&b, binary.BigEndian, uint32(n))
	b.Write(recs.Bytes())
	return b.Bytes()
}

var limitRE = regexp.MustCompile(`would exceed (\d+) bytes`)

func budgetLimit(t *testing.T, err error) uint64 {
	t.Helper()
	m := limitRE.FindStringSubmatch(fmt.Sprint(err))
	if !errors.Is(err, ErrTooLarge) || m == nil {
		t.Fatalf("want a budget error, got %v", err)
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n
}

// SF1: the shapes that expanded ~5x past the old, undercounting budget.
// The peak heap (garbage included) must stay within the GC headroom of
// the budget limit, and records dropped after decoding must not linger.
func TestHeapPeakUniqueDepends(t *testing.T) {
	ch, pol, _ := canonicalChannel(cfChannel)
	load := func(doc []byte) ([]*record.PackageRecord, error) {
		recs, _, err := loadRepodataJSON(bytes.NewReader(doc), ch, pol, "noarch", nil)
		return recs, err
	}
	t.Run("json kept", func(t *testing.T) {
		doc := uniqueDepsJSON(32<<20, false)
		var err error
		peak := heapPeak(func() { _, err = load(doc) })
		limit := budgetLimit(t, err)
		t.Logf("%d MiB document: peak %.1f MiB, budget limit %.1f MiB", len(doc)>>20, mib(peak), mib(limit))
		if peak > limit*5/2 {
			t.Errorf("peak %.1f MiB exceeds 2.5x the budget limit %.1f MiB", mib(peak), mib(limit))
		}
	})
	t.Run("json dropped", func(t *testing.T) {
		doc := uniqueDepsJSON(32<<20, true)
		var recs []*record.PackageRecord
		var err error
		peak := heapPeak(func() { recs, err = load(doc) })
		if err != nil || len(recs) != 0 {
			t.Fatalf("%d records, %v", len(recs), err)
		}
		t.Logf("%d MiB document, all records dropped: peak %.1f MiB", len(doc)>>20, mib(peak))
		// The collector lets garbage grow to about the live heap (here the
		// input) before running; nothing beyond that may accumulate.
		if peak > uint64(len(doc))+16<<20 {
			t.Errorf("dropped records left %.1f MiB on the heap", mib(peak))
		}
	})
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("shard mismatch=%v", mismatch), func(t *testing.T) {
			raw := uniqueDepsShard(maxShardDecoded-(1<<20), mismatch)
			var sc *shardContents
			var err error
			peak := heapPeak(func() { sc, err = decodeShard(raw, "p", nil, interner{}) })
			if mismatch {
				if err != nil || len(sc.conda) == 0 {
					t.Fatalf("got %v", err)
				}
				for _, r := range sc.conda {
					if r != nil {
						t.Fatal("mismatched record kept")
					}
				}
				t.Logf("%d MiB shard, all dropped: peak %.1f MiB", len(raw)>>20, mib(peak))
				if peak > uint64(len(raw))+16<<20 {
					t.Errorf("dropped records left %.1f MiB on the heap", mib(peak))
				}
				return
			}
			limit := budgetLimit(t, err)
			t.Logf("%d MiB shard: peak %.1f MiB, budget limit %.1f MiB", len(raw)>>20, mib(peak), mib(limit))
			if peak > limit*5/2 {
				t.Errorf("peak %.1f MiB exceeds 2.5x the budget limit %.1f MiB", mib(peak), mib(limit))
			}
		})
	}
}

// The budget's estimate covers the heap decoded data really retains, for
// each shape the decoders produce (so its stated bound holds).
func TestBudgetCoversRetainedHeap(t *testing.T) {
	ch, pol, _ := canonicalChannel(cfChannel)
	unlimited := func(b **budget) func(func() int64) *budget {
		return func(in func() int64) *budget {
			*b = &budget{base: 1 << 50, input: in}
			return *b
		}
	}
	minimal := func(n int) []byte {
		var b bytes.Buffer
		b.WriteString(`{"info":{"subdir":"noarch"},"packages.conda":{`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"p%d-1-0.conda":{"name":"p%d","version":"1","build":"0"}`, i, i)
		}
		b.WriteString(`}}`)
		return b.Bytes()
	}
	shared := func(n int) []byte {
		var b bytes.Buffer
		b.WriteString(`{"info":{"subdir":"noarch"},"packages.conda":{`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"p-%d-0.conda":{"name":"p","version":"%d","build":"0","md5":"%032x","sha256":"%064x","license":"MIT","depends":["python >=3.9","numpy >=1.20,<2","libgcc >=13"],"track_features":"a,b"}`, i, i, i, i)
		}
		b.WriteString(`}}`)
		return b.Bytes()
	}
	removed := func(n int) []byte {
		var b bytes.Buffer
		b.WriteString(`{"info":{"subdir":"noarch"},"removed":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"` + token(i) + `.conda"`)
		}
		b.WriteString(`]}`)
		return b.Bytes()
	}
	for name, doc := range map[string][]byte{
		"json minimal records":       minimal(100000),
		"json unique depends":        uniqueDepsJSON(16<<20, false),
		"json shared depends":        shared(50000),
		"json removed unique":        removed(400000),
		"json names of dropped recs": uniqueDepsJSON(4<<20, true),
	} {
		var b *budget
		got, keep := retained(func() any {
			recs, names, err := loadRepodataJSONBudget(bytes.NewReader(doc), ch, pol, "noarch", nil, unlimited(&b))
			if err != nil {
				t.Fatal(err)
			}
			return []any{recs, names}
		})
		runtime.KeepAlive(keep)
		t.Logf("%s: %.1f MiB input, retained %.1f MiB, charged %.1f MiB", name, mib(uint64(len(doc))), mib(got), mib(uint64(b.used)))
		if uint64(b.used) < got {
			t.Errorf("%s: charged %.1f MiB < retained %.1f MiB", name, mib(uint64(b.used)), mib(got))
		}
	}
	shardMinimal := func(n int) []byte {
		var p omap
		for i := 0; i < n; i++ {
			p = append(p, kv{fmt.Sprintf("p-%d-0.conda", i), omap{{"name", "p"}, {"version", strconv.Itoa(i)}, {"build", "0"}, {"md5", bytes.Repeat([]byte{1}, 16)}}})
		}
		return shardMP(t, nil, p)
	}
	for name, raw := range map[string][]byte{
		"shard minimal records":   shardMinimal(50000),
		"shard unique depends":    uniqueDepsShard(16<<20, false),
		"shard removed (dropped)": uniqueDepsShard(4<<20, true),
	} {
		b := fixedBudget(len(raw), 1<<50, 0)
		got, keep := retained(func() any {
			sc, err := decodeShardBody(&mpReader{b: raw}, "p", nil, interner{}, b)
			if err != nil {
				t.Fatal(err)
			}
			return sc
		})
		runtime.KeepAlive(keep)
		t.Logf("%s: %.1f MiB input, retained %.1f MiB, charged %.1f MiB", name, mib(uint64(len(raw))), mib(got), mib(uint64(b.used)))
		if uint64(b.used) < got {
			t.Errorf("%s: charged %.1f MiB < retained %.1f MiB", name, mib(uint64(b.used)), mib(got))
		}
	}
	shards := map[string][]byte{}
	for i := 0; i < 200000; i++ {
		shards["pkg-"+token(i)] = bytes.Repeat([]byte{byte(i)}, 32)
	}
	idx := shardIndexMP(t, "", "", "noarch", 1, shards)
	b := fixedBudget(len(idx), 1<<50, 0)
	got, keep := retained(func() any {
		x, err := decodeShardIndexBody(&mpReader{b: idx}, b)
		if err != nil {
			t.Fatal(err)
		}
		x.fold()
		return x
	})
	runtime.KeepAlive(keep)
	t.Logf("shard index: %.1f MiB input, retained %.1f MiB, charged %.1f MiB", mib(uint64(len(idx))), mib(got), mib(uint64(b.used)))
	if uint64(b.used) < got {
		t.Errorf("shard index: charged %.1f MiB < retained %.1f MiB", mib(uint64(b.used)), mib(got))
	}
}
