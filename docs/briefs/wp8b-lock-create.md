# WP8b — `goomba.Lock` / `goomba.Create`: specs to environments without micromamba

Read `AGENTS.md` first. You own the root package `goomba`, `cmd/goomba`, a
new `internal/` directory, and the specific changes listed below in
`repodata/`, `fetch/` and `link/`. Edit nothing else.

## Goal

Today goomba installs a lockfile. This package adds the step before that:
resolving match specs against conda channels into a lockfile, then
installing it: the equivalent of `micromamba create -n NAME python=3.11
-c conda-forge`.

jumpboot runs that micromamba command today, after downloading a
micromamba binary. WP9 adds goomba to jumpboot as a second backend, called
as a Go library; micromamba stays available. So the library API is the
main deliverable, and the CLI is a thin layer over it.

## Library API (root package `goomba`)

```go
// LockOptions describes what to resolve and where from.
type LockOptions struct {
    Specs        []string                // e.g. "python=3.11", "numpy>=1.26"
    Constraints  []string                // root constraints (never pull a package in)
    Channels     []string                // names ("conda-forge") or URLs, in strict priority order; required
    ChannelAlias string                  // "" = https://conda.anaconda.org
    Platform     string                  // "" = virtual.HostPlatform()
    Virtual      []*record.PackageRecord // nil = virtual.Detect for the host platform (error for another platform)
    RepodataDir  string                  // repodata cache; required
    Client       *http.Client            // nil = default (no proxy from env)
    Offline      bool
    MaxAge       time.Duration
    Progress     func(Event)             // optional; new event kinds for repodata and solving
}
func Lock(ctx context.Context, opt LockOptions) (*lock.Lockfile, error)
// Create resolves opt, then installs the lockfile into prefix with InstallLock.
// It returns the lockfile it installed.
func Create(ctx context.Context, prefix string, opt LockOptions, inst InstallOptions) (*lock.Lockfile, error)
```

Adjust names if the merged code suggests better ones, and report the final
API. Unsolvable requests return an error that `errors.As` matches as
`*solve.Unsolvable`.

## Behaviour

- **Channels.**
  - Parse with `spec.ParseChannel` and the alias.
  - Open each channel with `repodata.NewChannel`, which prefers the sharded
    index and falls back to the full index, for `Platform` plus `noarch`.
  - Combine them with `repodata.NewMulti` in the given order (strict
    priority).
  - The lockfile's `channels` list is the canonical URLs, in that order.
- **Python prefilter.**
  - `PythonMinor = solve.RequestedPythonMinor(Specs)`.
  - If the solve is unsolvable with the filter on, reopen the channels
    without the filter and solve once more, as libmamba does.
  - The cached shards make the retry cheap. Report which attempt succeeded
    via Progress.
- **Solve.**
  - `solve.Solve` with Specs, Constraints, Virtual and ChannelAlias.
  - The library does not impose a deadline; the caller's ctx decides (the
    CLI sets one).
  - `lock.FromRecords(platform, channels, specs, virt, records)` builds the
    lockfile. `virt` is the virtual set the solve used, in the lockfile's
    override convention (see `lock` docs).
- **Determinism.** The same channels, cache state and request give a
  byte-identical lockfile.
- **Create.** `Lock`, then `InstallLock(ctx, prefix, lf, inst)`. Pass
  `inst.Virtual` through when it is set; otherwise use the same virtual set
  as the solve.

## Security changes outside the root package (authorised)

1. **Repodata cache safety.** The index cache supplies every package's
   sha256, so a cache other users can write defeats verification. Move
   fetch's cache-directory safety check (owner = euid, StrictModes-style
   ancestors, the private-group exception, resolve once and use the
   resolved path) into `internal/safedir`. Use it in both `fetch` and
   `repodata`:
   - `repodata.Options.CacheDir` is refused with a new
     `repodata.ErrUnsafeCache` when unsafe;
   - repodata cache files are written 0600, or 0644 in a private directory;
     document which.
   - Port fetch's tests for the check, and add repodata ones.
2. **One network guard.** Move repodata's `netguard` (URL policy plus the
   dial-time check of the address actually dialled) into
   `internal/netguard`. Use it in `fetch` too, so package downloads get the
   same SSRF protection as index downloads. Keep every existing repodata
   netguard test passing, and add fetch tests: a redirect to a loopback or
   link-local address, and a DNS answer that maps to one.
3. **link exports the paths a package will create.**
   - Add an exported function (for example
     `link.PlannedPaths(pkgDir string, opt Options) ([]string, error)`)
     returning exactly the prefix-relative paths `Link` would create,
     including noarch relocation, `.pyc` candidates, entry points and the
     legacy `info/files`.
   - `link.ErrExists` errors carry the path, via a `*link.ExistsError` that
     unwraps to `ErrExists`.
   - Replace goomba's duplicated `packageDests` logic with `PlannedPaths`.
     Keep the drift test as a cross-check: PlannedPaths must equal what
     Link records, for every testdata package.

## CLI (`cmd/goomba`)

- **`goomba lock`:**
  `goomba lock [-c CHANNEL]... [--platform P] [--offline] -o FILE|- SPEC...`
  writes a lockfile. `--from-prefix` keeps working.
- **`goomba create`:**
  `goomba create -p PREFIX [-c CHANNEL]... SPEC...` resolves and installs.
  Specs and `--lock` are mutually exclusive.
- **Channels:** with no `-c`, use conda-forge and print a one-line notice.
  goomba has no config files.
- **Repodata cache:** `--repodata-dir`, defaulting to
  `<user cache dir>/goomba/repodata`.
- **`--timeout DURATION`:** default 10m, covering resolve plus install. The
  solver can take very long on adversarial inputs, so the CLI always sets a
  deadline.
- **Existing flags apply to specs-based create:** all of create's flags
  (`--pkgs-dir`, `--no-pyc`, `--virtual-override`, `--nvidia-smi`,
  `CONDA_OVERRIDE_*`). `--virtual-from-lock` is a usage error without
  `--lock`.
- **Unsolvable requests:** print the explanation (already sanitised by
  solve) and exit 1.

## Tests

- **Library (offline):** serve channels from `httptest` with small hand-made
  repodata, sharded and full. Lock and Create end to end with real or
  testdata `.conda` archives, covering:
  - multi-channel priority;
  - the prefilter retry;
  - unsolvable explanations;
  - determinism (the same lockfile bytes twice);
  - offline mode against a warm cache;
  - an unsafe RepodataDir refused;
  - ctx cancellation during repodata loading and during solving.
- **Differential against micromamba** (`GOOMBA_MICROMAMBA`; frozen
  conda-forge repodata copied from `$GOOMBA_REF_ROOT/pkgs/cache` into
  scratch and served from httptest, as solve's differential does):
  - `goomba create -c <url> python=3.11` and `micromamba create -p … -c
    <url> python=3.11` must install the same package set and the same files,
    with the documented differences.
  - Then the same for a few corpus requests (numpy, pandas with python 3.11;
    a noarch-heavy stack).
- **Live (`GOOMBA_LIVE=1`):** `goomba lock -c conda-forge python=3.12` over
  the sharded index from a cold cache. Report time, bytes fetched and peak
  memory.
- **Fuzz:** channel-argument parsing, and anything else new that parses.

## Out of scope

- `-n NAME` / root-prefix layouts. jumpboot will pass a prefix.
- Updating an existing environment, removing packages, pip integration and
  conditional dependencies.
