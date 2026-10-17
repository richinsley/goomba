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
// applies strict channel priority: only records from the highest-priority
// channel that has the name are returned. The slice may be returned in any
// order; callers must not modify the records. An unknown name returns an
// empty slice and a nil error.
type Source interface {
	Records(ctx context.Context, name string) ([]*record.PackageRecord, error)
}

// Prefetcher is an optional Source extension. Prefetch starts loading the
// given names in the background so later Records calls return quickly.
// It never returns an error; failures surface from Records.
type Prefetcher interface {
	Prefetch(ctx context.Context, names []string)
}
