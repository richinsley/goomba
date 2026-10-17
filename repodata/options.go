package repodata

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultConcurrency is the number of shards a ShardedChannel downloads in
// parallel when Options.Concurrency is zero.
const DefaultConcurrency = 16

// MaxConcurrency caps Options.Concurrency.
const MaxConcurrency = 64

// decodeConcurrency bounds how many shards a ShardedChannel decodes at
// once, which bounds the memory a hostile channel can make it use.
const decodeConcurrency = 4

// Size limits. Index files and shards are hostile input; these bound the
// memory a malicious or broken server can make goomba use.
// Real conda-forge sizes (2026-10): linux-64 repodata.json is 457 MB (59 MB
// as .zst); the largest shard seen (pyarrow) is 4 MB decompressed, the
// noarch shard index 1.1 MB. See also the decoder caps in budget.go.
//
// Every limit fails closed: a larger file is an error wrapping ErrTooLarge,
// never a truncated index. maxIndexJSON (1 GiB) is 2.2x today's largest
// real index; a channel that outgrows it stops loading rather than loading
// partially.
const (
	maxIndexDownload    = 512 << 20 // compressed repodata.json(.zst/.bz2) bytes
	maxIndexJSON        = 1 << 30   // decompressed repodata.json bytes (conda-forge linux-64: 457 MB)
	maxShardDownload    = 16 << 20  // compressed shard or shard index bytes
	maxShardDecoded     = 32 << 20  // decompressed shard or shard index bytes
	maxCacheHeaderBytes = 64 << 10  // header line of an index cache entry
)

// Options configures channel sources. The zero value is not usable: CacheDir
// and Platform are required. Options is a plain value: constructors copy
// what they need, so it may be reused and shared.
type Options struct {
	// Client performs HTTP requests. Nil means a default client with dial,
	// TLS and response-header timeouts and no proxy (library code never
	// reads HTTPS_PROXY; pass a Client to use one). The client is copied and
	// its CheckRedirect wrapped so that redirects to disallowed URLs fail.
	// If its Transport is an *http.Transport (or nil), a clone is used whose
	// connections are checked against the channel's URL policy on the
	// address actually dialled (only the dial to the proxy of a request the
	// transport sends through that proxy is exempt);
	// any other http.RoundTripper is used as is, and its owner is
	// responsible for network policy.
	Client *http.Client
	// CacheDir holds cached index files and shards. It is required, also
	// when Offline is set. Several processes may share it.
	CacheDir string
	// Offline forbids network access: only cached files are used, whatever
	// their age, and a missing file is ErrNotCached.
	Offline bool
	// MaxAge is how long a cached index file is used without asking the
	// server. Older entries are revalidated with a conditional GET
	// (If-None-Match / If-Modified-Since). Zero revalidates every time.
	// Shards are content-addressed and never expire.
	MaxAge time.Duration
	// Platform is the platform subdir, e.g. "linux-64". The "noarch" subdir
	// is always added. Platform "noarch" loads noarch only.
	Platform string
	// Concurrency bounds parallel shard downloads of a ShardedChannel;
	// zero means DefaultConcurrency, values above MaxConcurrency mean
	// MaxConcurrency.
	Concurrency int
	// PythonMinor, e.g. "3.12", enables the python-minor prefilter: records
	// whose depends constrain python to versions that exclude every 3.12.x
	// release are dropped while loading. Empty disables the filter. The
	// filter is conservative: it only evaluates dependencies of the
	// canonical form "python <version-spec> [<build>]", and one it cannot
	// interpret keeps the record. It applies to ShardedChannel and
	// FullIndex alike. Filtered names still count for strict channel
	// priority (see NameLister).
	PythonMinor string
	// MaxRetained bounds the memory one channel keeps for decoded index
	// data, in bytes of goomba's conservative estimate: a FullIndex's
	// records, or a ShardedChannel's shard indexes plus every shard it has
	// loaded so far. Zero means DefaultMaxRetained. Loading data that would
	// exceed it fails with ErrTooLarge (the FullIndex constructor, or the
	// Records, Walk or Prefetch call loading the shard) and keeps nothing
	// of that data.
	MaxRetained int64
}

// DefaultMaxRetained is the default Options.MaxRetained, 2 GiB. In 2026-10
// conda-forge's whole linux-64 plus noarch full index is charged 1.43 GiB
// (it actually retains about 0.9 GiB); a sharded walk over a thousand of
// its largest packages is charged under 0.2 GiB.
const DefaultMaxRetained = 2 << 30

// settings is the validated form of Options.
type settings struct {
	client      *http.Client // as given; fetchers wrap it with the channel's URL policy
	cacheDir    string
	offline     bool
	maxAge      time.Duration
	subdirs     []string // platform first, then noarch
	concurrency int
	python      *pythonMinor
	now         func() time.Time
	lookup      lookupFunc // nil = the system resolver; tests substitute it
	maxRetained int64
}

// pythonMinor is a parsed Options.PythonMinor.
type pythonMinor struct {
	major, minor uint64
}

func (o Options) settings() (*settings, error) {
	if o.CacheDir == "" {
		return nil, errors.New("repodata: options: CacheDir is required")
	}
	if err := checkPlatform(o.Platform); err != nil {
		return nil, err
	}
	if o.MaxAge < 0 {
		return nil, fmt.Errorf("repodata: options: negative MaxAge %v", o.MaxAge)
	}
	if o.Concurrency < 0 {
		return nil, fmt.Errorf("repodata: options: negative Concurrency %d", o.Concurrency)
	}
	if o.MaxRetained < 0 {
		return nil, fmt.Errorf("repodata: options: negative MaxRetained %d", o.MaxRetained)
	}
	s := &settings{
		client:      o.Client,
		cacheDir:    o.CacheDir,
		offline:     o.Offline,
		maxAge:      o.MaxAge,
		concurrency: o.Concurrency,
		now:         time.Now,
	}
	if s.concurrency == 0 {
		s.concurrency = DefaultConcurrency
	}
	s.concurrency = min(s.concurrency, MaxConcurrency)
	s.maxRetained = o.MaxRetained
	if s.maxRetained == 0 {
		s.maxRetained = DefaultMaxRetained
	}
	if o.Platform == "noarch" {
		s.subdirs = []string{"noarch"}
	} else {
		s.subdirs = []string{o.Platform, "noarch"}
	}
	if o.PythonMinor != "" {
		pm, err := parsePythonMinor(o.PythonMinor)
		if err != nil {
			return nil, err
		}
		s.python = pm
	}
	return s, nil
}

// checkPlatform accepts conda subdir names such as "linux-64", "osx-arm64"
// or "emscripten-wasm32": lowercase letters, digits, '-' and '_'.
func checkPlatform(p string) error {
	if p == "" {
		return errors.New("repodata: options: Platform is required")
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return fmt.Errorf("repodata: options: invalid Platform %q", p)
		}
	}
	return nil
}

// parsePythonMinor parses "3.12" into its two numbers.
func parsePythonMinor(s string) (*pythonMinor, error) {
	major, minor, ok := strings.Cut(s, ".")
	bad := fmt.Errorf("repodata: options: invalid PythonMinor %q: want MAJOR.MINOR such as \"3.12\"", s)
	if !ok || !allDigits(major) || !allDigits(minor) {
		return nil, bad
	}
	x, err1 := strconv.ParseUint(major, 10, 64)
	y, err2 := strconv.ParseUint(minor, 10, 64)
	if err1 != nil || err2 != nil {
		return nil, bad
	}
	return &pythonMinor{major: x, minor: y}, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
