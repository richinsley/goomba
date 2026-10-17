# WP2 — `repodata`: package index, sharded index, HTTP cache

Read `AGENTS.md` first. You own `repodata/` (keep `repodata/source.go`'s
interfaces as they are; add files beside it).

## Goal

Turn conda channels into `repodata.Source` implementations the resolver can
query one package name at a time. Never parse version strings here (that is
package `spec`); records carry raw strings.

Primary reference: libmamba `src/core/subdir_index.cpp`, `shards.cpp`,
`shard_index_loader.cpp`, `shard_traversal.cpp`,
`shard_python_minor_prefilter.cpp`, `specs/repo_data.cpp`. Cross-check with
rattler `crates/rattler_repodata_gateway` and
`crates/rattler_conda_types/src/repo_data`.

## Formats (verified against conda-forge on 2026-10-17)

Full index: `<channel>/<subdir>/repodata.json` (also `.json.zst`, `.json.bz2`).
Top level: `info` (`subdir`, optional `base_url` per CEP 15),
`packages` (`.tar.bz2` records keyed by file name), `packages.conda`
(`.conda` records), `removed`, `repodata_version`. conda-forge linux-64 is
59 MB as `.zst`.

Sharded index (CEP 16): `<channel>/<subdir>/repodata_shards.msgpack.zst`, a
zstd-compressed msgpack map:

    {version: 1,
     info: {base_url: "", shards_base_url: "", subdir: "linux-64", created_at: "..."},
     shards: {<package name>: <32-byte sha256>}}

conda-forge linux-64 lists 14,159 names; the compressed index is 0.57 MB.
A shard lives at `resolve(index URL, shards_base_url) + hex(sha256) +
".msgpack.zst"`; empty `shards_base_url` means the index's own directory. The
sha256 is of the **compressed** shard bytes (checked: pip's noarch shard,
6,326 bytes compressed). A shard is `{packages: {...}, "packages.conda": {...},
removed: [...]}`; in msgpack, `sha256` and `md5` are raw bytes. Package URLs
come from `info.base_url` (relative to the index URL) or the subdir URL.

## Record normalization (into `record.PackageRecord`)

- `track_features`: string (comma/space separated) or list → list, no empties.
- `noarch`: `true` → `"generic"`; string kept; absent → `""`.
- `timestamp` → `record.NormalizeTimestamp`.
- Hashes → lowercase hex.
- `Fn`, `URL`, `Channel` (canonical base URL, no subdir), `Subdir` filled.
- When the same `name-version-build` exists as `.conda` and `.tar.bz2`, keep
  only the `.conda` record (conda's behaviour).
- Records listed in `removed` are dropped.

## Required API

```go
// Options shared by channel sources.
type Options struct {
    Client    *http.Client   // nil = a default client
    CacheDir  string         // required for network sources
    Offline   bool           // never touch the network; use the cache only
    MaxAge    time.Duration  // index freshness before a conditional GET
    Platform  string         // e.g. "linux-64"; noarch is always added
}
func NewShardedChannel(ctx context.Context, channelURL string, opt Options) (*ShardedChannel, error)
func NewFullIndex(ctx context.Context, channelURL string, opt Options) (*FullIndex, error)
func NewMemory(records []*record.PackageRecord) *Memory          // test fixture source
func NewMulti(sources ...Source) *Multi                          // strict channel priority, first wins per name
func LoadRepodataJSON(r io.Reader, channelURL, subdir string) ([]*record.PackageRecord, error)
```

`ShardedChannel` and `FullIndex` both cover `<platform>` + `noarch`.
`ShardedChannel` implements `Prefetcher`: it fetches shards in parallel
(bounded, default 16) and walks dependency names breadth-first like
`shard_traversal.cpp`, extracting names from `depends` with a cheap split
(first token before space or operator), without package `spec`. Offer the
Python-minor prefilter as an option (drop records whose `depends` pin a
different `python` minor than the one requested), following
`shard_python_minor_prefilter.cpp`.

HTTP cache: index files are cached with ETag / Last-Modified and revalidated
after `MaxAge`; shards are content-addressed and cached forever, and every
read from the cache is re-hashed. Writes go to a temp file then rename.
Concurrent use from several goroutines and processes must be safe.
Only `https` URLs, except `http://127.0.0.1`/`localhost` for tests.

## Tests

- Port libmamba `tests/src/specs/test_repo_data.cpp` and the subdir and shard
  tests in `libmamba/tests/src/core/` that apply.
- `httptest` server fixtures: full index (`.json`, `.zst`, `.bz2`), sharded
  index with shards; hash mismatch is an error; removed records dropped;
  `.conda` preferred; `base_url` handling; conditional GET; offline mode.
- Small real fixtures: download conda-forge `noarch` shard index entries for 2–3
  small packages and commit them (keep under 5 MB).
- Live (`GOOMBA_LIVE=1`): open conda-forge linux-64 sharded, load `python`,
  check ≥ 1 record for 3.11 with a 64-hex sha256; prefetch `python` and report
  how many shards and bytes the walk fetched.

Fuzz: `FuzzLoadRepodataJSON`, `FuzzDecodeShard`, `FuzzDecodeShardIndex`.
