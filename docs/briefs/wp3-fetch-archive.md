# WP3 — `fetch` and `archive`: verified downloads and safe unpacking

Read `AGENTS.md` first. You own `fetch/` and `archive/`.

## Goal

Get package archives onto disk only if their bytes match the expected sha256,
and unpack them without ever writing outside the destination. These two
packages are the main supply-chain barrier; be paranoid.

References: libmamba `src/core/package_fetcher.cpp`, `package_handling.cpp`,
`package_cache.cpp`; rattler `crates/rattler_package_streaming` (and its
`tests/extract.rs`), `crates/rattler_cache`.

## `archive`

```go
type Limits struct {
    MaxFileSize  int64 // per entry, default 4 GiB
    MaxTotalSize int64 // default 16 GiB
    MaxEntries   int   // default 1,000,000
}
// Extract unpacks a .conda or .tar.bz2 archive (chosen by file name suffix)
// into destDir, which must not exist. It extracts into a sibling temp dir and
// renames on success, so destDir appears complete or not at all.
func Extract(ctx context.Context, archivePath, destDir string, lim Limits) error
// ExtractReader is the streaming form for .tar.bz2 / tar.zst content.
```

`.conda` format (checked on `pip-26.0.1-pyh8b19718_0.conda`): a zip holding
`metadata.json`, `pkg-<dist>.tar.zst` and `info-<dist>.tar.zst`. Extract both
tars into `destDir` (the info tar provides `info/`). `.tar.bz2` is one bzip2
tar. Use `archive/zip`, `archive/tar`, `compress/bzip2` and
`github.com/klauspost/compress/zstd`.

Refuse (error, nothing left behind): absolute paths, any `..` component,
Windows drive or UNC paths, NUL bytes, entries that clean to outside
`destDir`, symlinks whose target resolves outside `destDir` (relative targets
inside the package are normal, e.g. `lib/libz.so -> libz.so.1.3.1`), hard
links to anything outside or not yet extracted, device files, FIFOs, and
duplicate paths with conflicting types. Keep the executable bit; ignore
owners. Enforce `Limits` on bytes actually decompressed, not on headers.

## `fetch`

```go
type Cache struct {
    Dir         string        // package cache, e.g. <root>/pkgs
    Client      *http.Client  // nil = default with sane timeouts
    Retries     int           // transient errors only; default 3 with backoff
    AllowHTTP   bool          // tests only: allow http://127.0.0.1
}
// Fetch returns the path of a verified archive for url in the cache,
// downloading it if needed. sha256 is required (64 lowercase hex); size is
// checked when > 0.
func (c *Cache) Fetch(ctx context.Context, url, sha256 string, size int64) (string, error)
// Extracted fetches and unpacks rec, returning <Dir>/<rec.Dist()>.
func (c *Cache) Extracted(ctx context.Context, rec *record.PackageRecord) (string, error)
```

- Stream the download through sha256 into `<Dir>/<fn>.partial-<rand>`;
  verify hash and size; `fsync`; rename to `<Dir>/<fn>`. Never leave a
  `.partial` file behind on any error or cancellation.
- A hash mismatch is never retried and is reported as a distinct error type
  (`*IntegrityError` with expected and actual digests).
- Only `https` (plus `AllowHTTP` for loopback in tests); refuse redirects to
  non-https URLs.
- An archive already in the cache is re-hashed before use (keep a small
  `<fn>.sha256` sidecar holding digest, size and mtime so unchanged files skip
  the re-hash).
- `Extracted` writes `<Dir>/<dist>/.goomba-extracted` containing the archive
  sha256 last; a directory without a matching marker (for example one made by
  micromamba) is never trusted: extract again beside it and swap.
- Locking: an in-process mutex per key plus a cross-process `flock` on
  `<Dir>/.locks/<fn>.lock` (`golang.org/x/sys/unix`; on Windows,
  `LockFileEx` via `golang.org/x/sys/windows` must compile). Many goroutines
  and processes fetching the same package must download it once.

## Tests

- `httptest` fixtures: happy path; hash mismatch; size mismatch; truncated
  body; 5xx then success (retry); cancellation mid-download (no partial file);
  redirect to http refused; 20 concurrent `Fetch` of one URL → one download.
- Hostile archives, built in tests: zip slip, tar `../`, absolute path,
  symlink escaping, hardlink escaping, symlink then file through it, device
  file, zip bomb against `Limits`, duplicate entries.
- Real archives copied into `testdata/` from `$GOOMBA_REF_ROOT/pkgs`:
  `wheel-0.46.3-pyhd8ed1ab_0.conda`, `libzlib-1.3.2-h25fd6f3_2.conda`; and one
  small `.tar.bz2` (from rattler `test-data/` or conda-forge). After
  extraction, the file list and every file's sha256 must match the package's
  `info/paths.json` (`sha256` for files; symlinks match `path_type`).
- Compare extraction of each real fixture with micromamba's extracted copy in
  `$GOOMBA_REF_ROOT/pkgs/<dist>/` (same files, same modes, same
  symlink targets; micromamba adds `info/repodata_record.json`, ignore it).
- Port the applicable cases from rattler `crates/rattler_package_streaming/tests/extract.rs`.

Fuzz: `FuzzExtractConda`, `FuzzExtractTarBz2` (arbitrary bytes: no panic, no
file outside the temp root, all limits respected).
