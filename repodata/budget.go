package repodata

import (
	"fmt"
	"sync/atomic"

	"github.com/richinsley/goomba/record"
)

// Size caps for what a decoder keeps. Compressed and decompressed byte
// limits alone do not bound memory: tiny encodings (an empty msgpack string
// is one byte, `"",` three) can expand into large Go structures. Real
// conda-forge data (2026-10) stays far below these: at most 126 depends,
// 100 constrains and 19 keys per record, strings of at most 278 bytes,
// 980 removed files per subdir, 24k names per shard index.
const (
	maxListEntries = 4096    // depends, constrains or track_features of one record
	maxFieldBytes  = 4096    // any string kept from a record, file name or removed entry
	maxRecordKeys  = 1024    // keys in one record
	maxRemoved     = 1 << 20 // entries of one "removed" list
	maxIndexNames  = 1 << 20 // names in one shard index
	maxKeyBytes    = 256     // object keys longer than this are skipped as unknown
	maxBaseURL     = 512     // info.base_url and shards_base_url; every record URL starts with one
)

// Estimated retained sizes charged to a budget, calibrated against measured
// heap (TestBudgetCoversRetainedHeap): Go rounds small allocations up to
// size classes and maps keep spare slots, so per-item costs are well above
// the payload.
const (
	costRecord      = 640 // PackageRecord (a 320-byte size class), its URL and slack
	costString      = 16  // size-class rounding of a string kept outside the interner
	costSliceSlot   = 16  // one string header in a slice, charged per unit of capacity
	costSetEntry    = 96  // a map slot with growth headroom, excluding the key bytes
	costInternEntry = 112 // an interner (map[string]string) slot plus string rounding
	costIndexEntry  = 192 // a shard index name: hash map slot plus case-folded slot

	jsonBudgetRatio = 4 // retained bytes allowed per input byte (conda-forge needs ~2.5)
	jsonBudgetBase  = 64 << 20
	mpBudgetRatio   = 6 // per decompressed shard byte (conda-forge needs ~3.3 at most)
	mpBudgetBase    = 1 << 20
)

// budget bounds the memory a decoder retains relative to its input: the
// estimated bytes it keeps may not exceed base + ratio × input bytes read so
// far. A hostile document that expands further fails with ErrTooLarge.
// Estimates are conservative (they include allocator and map overhead), so
// the retained heap stays below the limit; the peak heap during decoding is
// higher by the garbage collector's headroom (about 2× with the default
// GOGC=100) plus the input buffers.
//
// A budget may also feed a cumulative limit shared by everything one
// channel keeps (Options.MaxRetained).
type budget struct {
	used  int64
	base  int64
	ratio int64
	input func() int64
	cum   *cumulative // nil: none
}

func (b *budget) charge(n int) error {
	b.used += int64(n)
	if limit := b.base + b.ratio*b.input(); b.used > limit {
		return fmt.Errorf("%w: decoded data would exceed %d bytes", ErrTooLarge, limit)
	}
	if b.cum != nil {
		return b.cum.add(int64(n))
	}
	return nil
}

// cumulative bounds the charged bytes one channel retains in total, across
// its subdirs and every shard it keeps. It is safe for concurrent use.
type cumulative struct {
	max  int64
	used atomic.Int64
}

// add reserves n bytes, or fails with ErrTooLarge and reserves nothing.
func (c *cumulative) add(n int64) error {
	if c.used.Add(n) > c.max {
		c.used.Add(-n)
		return fmt.Errorf("%w: the channel would retain more than MaxRetained (%d bytes)", ErrTooLarge, c.max)
	}
	return nil
}

// sized is the heap a string of n bytes takes: allocations are rounded up
// to size classes, which waste up to 12.5% for large ones.
func sized(n int) int { return n + n/8 }

func fixedBudget(inputLen int, base, ratio int64) *budget {
	n := int64(inputLen)
	return &budget{base: base, ratio: ratio, input: func() int64 { return n }}
}

// interner de-duplicates strings that repeat across the records of one
// index or shard (dependency specs, names, versions, licenses). Records are
// interned only once accepted, so a dropped record leaves nothing behind.
type interner map[string]string

// add returns the shared copy of s and the bytes adding it cost.
func (in interner) add(s string) (string, int) {
	if s == "" {
		return "", 0
	}
	if v, ok := in[s]; ok {
		return v, 0
	}
	in[s] = s
	return s, costInternEntry + sized(len(s))
}

// addList interns the entries of l in place and returns the cost of l.
func (in interner) addList(l []string) int {
	n := costSliceSlot * cap(l)
	for i, s := range l {
		var c int
		l[i], c = in.add(s)
		n += c
	}
	return n
}

// keep interns an accepted record and returns the bytes it retains: the
// record, its own strings and new interned strings. Its URL is charged when
// the record is placed (pendingSet.place).
func (in interner) keep(r *record.PackageRecord) int {
	n := costRecord
	for _, s := range []string{r.Build, r.Features, r.MD5, r.SHA256, r.PythonSitePackagesPath} {
		if s != "" {
			n += costString + sized(len(s))
		}
	}
	var c int
	r.Name, c = in.add(r.Name)
	n += c
	r.Version, c = in.add(r.Version)
	n += c
	r.License, c = in.add(r.License)
	n += c
	n += in.addList(r.Depends) + in.addList(r.Constrains)
	n += costSliceSlot * cap(r.TrackFeatures)
	for _, s := range r.TrackFeatures {
		n += costString + sized(len(s))
	}
	return n
}
