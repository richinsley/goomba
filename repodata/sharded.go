// Ported from libmamba src/core/shards.cpp and src/core/shard_traversal.cpp
// (BSD-3-Clause, see NOTICE), cross-checked with rattler
// crates/rattler_repodata_gateway/src/gateway/sharded_subdir. Unlike both,
// every shard is verified against the sha256 in the shard index, on download
// and on every cache read.

package repodata

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"

	"github.com/richinsley/goomba/record"
)

// shardIndexFile is the CEP 16 shard index file name in each subdir.
const shardIndexFile = "repodata_shards.msgpack.zst"

// ShardedChannel is a Source backed by a channel's sharded index (CEP 16):
// one small index per subdir maps package names to the sha256 of a shard
// holding that package's records. Shards are fetched on demand, verified,
// cached on disk forever (content-addressed) and decoded once per channel.
// It implements Prefetcher. It is safe for concurrent use.
type ShardedChannel struct {
	s       *settings
	f       *fetcher
	channel string
	subdirs []*shardedSubdir
	zstd    *zstd.Decoder
	sem     chan struct{} // bounds concurrent shard downloads
	decSem  chan struct{} // bounds concurrent shard decoding (memory)

	mu      sync.Mutex
	entries map[shardKey]*shardEntry

	fetched   atomic.Int64
	bytes     atomic.Int64
	fromCache atomic.Int64
	retained  *cumulative // Options.MaxRetained over the indexes and kept shards
}

type shardedSubdir struct {
	name       string
	index      *shardIndex
	shardsBase *url.URL
	loc        *locator
}

type shardKey struct{ subdir, name string }

// shardEntry is a shard being loaded or loaded; done is closed when recs or
// err is set.
type shardEntry struct {
	done chan struct{}
	recs []*record.PackageRecord
	err  error
}

// ShardStats counts the work a ShardedChannel did so far. It is a snapshot
// value.
type ShardStats struct {
	// ShardsFetched is the number of shards downloaded from the network.
	ShardsFetched int64
	// BytesFetched is the number of compressed shard bytes downloaded.
	BytesFetched int64
	// ShardsFromCache is the number of shards read (and re-verified) from
	// the disk cache.
	ShardsFromCache int64
	// RetainedBytes is the estimated memory the channel keeps for its shard
	// indexes and loaded shards, as counted against Options.MaxRetained.
	RetainedBytes int64
}

// NewShardedChannel loads the shard indexes of <channelURL>/<opt.Platform>
// and <channelURL>/noarch. If either subdir has no shard index the error
// wraps ErrNotSharded; NewChannel then falls back to the full index.
func NewShardedChannel(ctx context.Context, channelURL string, opt Options) (*ShardedChannel, error) {
	s, err := opt.settings()
	if err != nil {
		return nil, err
	}
	ch, pol, err := canonicalChannel(channelURL)
	if err != nil {
		return nil, err
	}
	dec, err := newZstdDecoder(maxShardDecoded, decodeConcurrency)
	if err != nil {
		return nil, err
	}
	c := &ShardedChannel{
		s:        s,
		f:        newFetcher(s, s.client, pol),
		channel:  ch.String(),
		zstd:     dec,
		sem:      make(chan struct{}, s.concurrency),
		decSem:   make(chan struct{}, decodeConcurrency),
		entries:  map[shardKey]*shardEntry{},
		retained: &cumulative{max: s.maxRetained},
	}
	c.subdirs = make([]*shardedSubdir, len(s.subdirs))
	errs := make([]error, len(s.subdirs))
	var wg sync.WaitGroup
	for i, sd := range s.subdirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.subdirs[i], errs[i] = c.loadIndex(ctx, ch, sd)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *ShardedChannel) loadIndex(ctx context.Context, ch *url.URL, subdir string) (*shardedSubdir, error) {
	indexURL := subdirURL(ch, subdir).JoinPath(shardIndexFile)
	u := indexURL.String()
	body, err := c.f.fetchIndex(ctx, u, maxShardDownload)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("repodata: %s: %w", redactString(u), ErrNotSharded)
	}
	if err != nil {
		return nil, err
	}
	raw, err := decompressZstd(c.zstd, body)
	if err != nil {
		return nil, fmt.Errorf("repodata: %s: %w", redactString(u), err)
	}
	idx, err := decodeShardIndex(raw)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, redactString(u))
	}
	if err := c.retained.add(idx.charged); err != nil {
		return nil, fmt.Errorf("repodata: %s: %w", redactString(u), err)
	}
	if idx.subdir != "" && idx.subdir != subdir {
		return nil, fmt.Errorf("repodata: %s: info.subdir %q does not match subdir %q", redactString(u), idx.subdir, subdir)
	}
	shardsBase, err := resolveBase(indexURL, idx.shardsBaseURL, c.f.pol)
	if err != nil {
		return nil, fmt.Errorf("repodata: %s: shards_base_url: %w", redactString(u), err)
	}
	base, err := resolveBase(indexURL, idx.baseURL, c.f.pol)
	if err != nil {
		return nil, fmt.Errorf("repodata: %s: base_url: %w", redactString(u), err)
	}
	return &shardedSubdir{
		name:       subdir,
		index:      idx,
		shardsBase: shardsBase,
		loc:        &locator{channel: ch.String(), subdir: subdir, base: base},
	}, nil
}

// lookup returns the index keys a name refers to: the exact key if the
// index has it, else every key equal to name case-insensitively (an index
// keyed "MyPkg" answers for "mypkg", as a full index does).
func (sd *shardedSubdir) lookup(name string) []string {
	if _, ok := sd.index.shards[name]; ok {
		return []string{name}
	}
	return sd.index.folded[strings.ToLower(name)]
}

// shardURL returns where the shard with digest sum lives:
// resolve(index URL, shards_base_url) + hex(sum) + ".msgpack.zst".
func (sd *shardedSubdir) shardURL(sum [32]byte) string {
	return joinFile(sd.shardsBase, hex.EncodeToString(sum[:])+".msgpack.zst")
}

// Records implements Source. It loads the package's shard from each subdir
// that lists it; a download, verification or decoding failure is returned
// (and retried by the next call).
func (c *ShardedChannel) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []*record.PackageRecord
	for _, sd := range c.subdirs {
		recs, err := c.load(ctx, sd, name)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	sortRecords(out)
	return out, nil
}

// HasName implements NameLister: whether a shard index lists name, whether
// or not any of its records survive filtering.
func (c *ShardedChannel) HasName(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.has(name), nil
}

// Names returns the sorted names of all packages in the shard indexes.
func (c *ShardedChannel) Names() []string {
	seen := map[string]struct{}{}
	for _, sd := range c.subdirs {
		for n := range sd.index.shards {
			seen[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Stats returns counters of the work done so far.
func (c *ShardedChannel) Stats() ShardStats {
	return ShardStats{
		ShardsFetched:   c.fetched.Load(),
		BytesFetched:    c.bytes.Load(),
		ShardsFromCache: c.fromCache.Load(),
		RetainedBytes:   c.retained.used.Load(),
	}
}

func (c *ShardedChannel) has(name string) bool {
	for _, sd := range c.subdirs {
		if len(sd.lookup(name)) > 0 {
			return true
		}
	}
	return false
}

// load returns the records of name in one subdir.
func (c *ShardedChannel) load(ctx context.Context, sd *shardedSubdir, name string) ([]*record.PackageRecord, error) {
	var out []*record.PackageRecord
	for _, key := range sd.lookup(name) {
		recs, err := c.loadShard(ctx, sd, key, sd.index.shards[key])
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}

// loadShard returns the records of the shard of index key indexName,
// loading it once per channel even under concurrent calls. Failures are
// not cached.
func (c *ShardedChannel) loadShard(ctx context.Context, sd *shardedSubdir, indexName string, sum [32]byte) ([]*record.PackageRecord, error) {
	k := shardKey{sd.name, indexName}
	for {
		c.mu.Lock()
		e, loading := c.entries[k]
		if !loading {
			e = &shardEntry{done: make(chan struct{})}
			c.entries[k] = e
			c.mu.Unlock()
			func() {
				defer close(e.done)
				e.recs, e.err = c.fetchShard(ctx, sd, indexName, sum)
				if e.err != nil {
					c.mu.Lock()
					delete(c.entries, k)
					c.mu.Unlock()
				}
			}()
			return e.recs, e.err
		}
		c.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err != nil && isContextErr(e.err) && ctx.Err() == nil {
			continue // the loader's context ended, not ours: load again
		}
		return e.recs, e.err
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *ShardedChannel) fetchShard(ctx context.Context, sd *shardedSubdir, name string, sum [32]byte) ([]*record.PackageRecord, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	data, fromNetwork, err := c.f.fetchShard(ctx, sd.shardURL(sum), sum)
	<-c.sem
	if err != nil {
		return nil, err
	}
	if fromNetwork {
		c.fetched.Add(1)
		c.bytes.Add(int64(len(data)))
	} else {
		c.fromCache.Add(1)
	}
	select {
	case c.decSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.decSem }()
	recs, charged, err := decodeShardFile(c.zstd, data, name, sd.loc, c.s.python)
	if err != nil {
		return nil, err
	}
	if err := c.retained.add(charged); err != nil {
		return nil, fmt.Errorf("repodata: shard %q (%s): %w", name, sd.name, err)
	}
	return recs, nil
}

// decodeShardFile decompresses, decodes and places one shard's records. It
// also returns the bytes charged for them.
func decodeShardFile(d *zstd.Decoder, data []byte, name string, loc *locator, py *pythonMinor) ([]*record.PackageRecord, int64, error) {
	raw, err := decompressZstd(d, data)
	if err != nil {
		return nil, 0, fmt.Errorf("repodata: shard %q (%s): %w", name, loc.subdir, err)
	}
	sc, err := decodeShard(raw, name, py, interner{})
	if err != nil {
		return nil, 0, err
	}
	recs, err := sc.place(loc)
	if err != nil {
		return nil, 0, fmt.Errorf("repodata: shard %q (%s): %w", name, loc.subdir, err)
	}
	return finalize(recs, sc.removed), sc.budget.used, nil
}

// Prefetch implements Prefetcher: it starts Walk in the background and
// returns at once. Failures surface from later Records calls.
func (c *ShardedChannel) Prefetch(ctx context.Context, names []string) {
	go func() { _, _ = c.Walk(ctx, names) }()
}

// Walk loads the shards of names and, breadth-first, of every package named
// in the depends of the records found, until the dependency closure is
// loaded (libmamba RepodataSubset::reachable). Dependency names come from a
// cheap split of each spec (text before the first space or operator), not
// from parsing it. Only depends are followed: libmamba's traversal also
// follows constrains, but a constrained package matters only if something
// depends on it, and then the walk reaches it through that dependency. Up
// to Options.Concurrency shards load in parallel. Names missing from every
// index are skipped.
//
// Walk returns the sorted names it loaded and, if some failed, an error
// joining the failures in name order. It blocks until done or ctx ends.
func (c *ShardedChannel) Walk(ctx context.Context, names []string) ([]string, error) {
	w := &walker{c: c, seen: map[string]struct{}{}, errs: map[string]error{}}
	w.cond = sync.NewCond(&w.mu)
	for _, n := range names {
		w.enqueue(n)
	}
	stop := context.AfterFunc(ctx, func() {
		w.mu.Lock()
		w.cancelled = true
		w.cond.Broadcast()
		w.mu.Unlock()
	})
	defer stop()
	var wg sync.WaitGroup
	for i := 0; i < c.s.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.work(ctx)
		}()
	}
	wg.Wait()
	sort.Strings(w.visited)
	if err := ctx.Err(); err != nil {
		return w.visited, err
	}
	failed := make([]string, 0, len(w.errs))
	for n := range w.errs {
		failed = append(failed, n)
	}
	sort.Strings(failed)
	var errs []error
	for _, n := range failed {
		errs = append(errs, w.errs[n])
	}
	return w.visited, errors.Join(errs...)
}

// walker is the FIFO work queue of a Walk.
type walker struct {
	c         *ShardedChannel
	mu        sync.Mutex
	cond      *sync.Cond
	queue     []string
	active    int
	cancelled bool
	seen      map[string]struct{}
	visited   []string
	errs      map[string]error
}

// enqueue adds name if it is new and listed in some index. Callers hold mu
// (or run before the workers start).
func (w *walker) enqueue(name string) {
	if _, dup := w.seen[name]; dup || !w.c.has(name) {
		return
	}
	w.seen[name] = struct{}{}
	w.queue = append(w.queue, name)
}

func (w *walker) work(ctx context.Context) {
	for {
		w.mu.Lock()
		for len(w.queue) == 0 && w.active > 0 && !w.cancelled {
			w.cond.Wait()
		}
		if w.cancelled || len(w.queue) == 0 {
			w.cond.Broadcast()
			w.mu.Unlock()
			return
		}
		name := w.queue[0]
		w.queue = w.queue[1:]
		w.active++
		w.mu.Unlock()

		recs, err := w.c.Records(ctx, name)
		deps := dependencyNames(recs)

		w.mu.Lock()
		w.active--
		if err != nil {
			w.errs[name] = err
		} else {
			w.visited = append(w.visited, name)
		}
		for _, d := range deps {
			w.enqueue(d)
		}
		w.cond.Broadcast()
		w.mu.Unlock()
	}
}

// NewChannel opens channelURL as a ShardedChannel and falls back to a
// FullIndex when the channel publishes no sharded index, or, offline, when
// the cache holds no shard index (a full index may be cached instead).
func NewChannel(ctx context.Context, channelURL string, opt Options) (Source, error) {
	sc, err := NewShardedChannel(ctx, channelURL, opt)
	if err == nil {
		return sc, nil
	}
	if !errors.Is(err, ErrNotSharded) && !(opt.Offline && errors.Is(err, ErrNotCached)) {
		return nil, err
	}
	fi, ferr := NewFullIndex(ctx, channelURL, opt)
	if ferr != nil {
		return nil, ferr
	}
	return fi, nil
}
