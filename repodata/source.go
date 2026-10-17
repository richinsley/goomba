// Package repodata loads conda package records from channels: the full index
// (repodata.json, .zst or .bz2) and the sharded index (CEP 16), with HTTP
// caching. Shards are verified against the sha256 in the shard index.
package repodata

import (
	"context"

	"github.com/richinsley/goomba/record"
)

// Source supplies candidate records to the resolver, one package name at a
// time, so implementations can load lazily (e.g. one shard per name).
//
// Records returns every record for name across the source's subdirs (the
// platform subdir plus noarch). When a source spans several channels it
// applies strict channel priority: only the highest-priority channel that
// lists the name answers, even if none of its records survive filtering
// (see NameLister). The slice may be returned in any order; callers must
// not modify the records. An unknown name returns an empty slice and a nil
// error.
type Source interface {
	Records(ctx context.Context, name string) ([]*record.PackageRecord, error)
}

// Prefetcher is an optional Source extension. Prefetch starts loading the
// given names in the background so later Records calls return quickly.
// It never returns an error; failures surface from Records.
type Prefetcher interface {
	Prefetch(ctx context.Context, names []string)
}

// NameLister is an optional Source extension for strict channel priority.
// HasName reports whether the source lists name at all, before any
// filtering: records dropped as malformed, listed under "removed", lacking
// a checksum or removed by the python-minor prefilter still count. A
// multi-channel source must let the first channel that lists a name answer
// for it, even with an empty result, so that filtering in a private channel
// can never let a public channel supply a package of the same name
// (dependency confusion).
type NameLister interface {
	HasName(ctx context.Context, name string) (bool, error)
}
