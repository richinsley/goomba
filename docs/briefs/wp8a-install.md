# WP8a — `goomba`: install a lockfile into a prefix

Read `AGENTS.md` first. You own the module-root package files (`*.go` in the
repository root; you may extend `goomba.go`'s package comment),
`cmd/goomba/`, and new subpackages under `internal/`. Everything else is
read-only: if a merged package needs a change, describe it in your report.

## Goal

Turn a verified lockfile into a working conda environment with no resolver,
no micromamba and no network beyond fetching the locked archives. This is the
path poppi and jb-mesh will use at runtime. The result must match what
`micromamba create -f <explicit file>` produces from the same packages, file
for file, apart from the install path itself and the documented deviations
below.

Merged packages you build on (read their package docs first): `record`,
`spec`, `fetch` (content-addressed cache: `Cache.Extracted` returns
`<Dir>/<sha256>/pkg`), `archive`, `link` (`Link`, `WriteCondaMeta`,
`AppendHistoryEntry`, `NewPythonInfo`, `ReadCondaMeta`), `lock` (`Read`,
`Validate`, `Records`, `FromPrefix`, `WriteExplicit`), `virtual` (`Detect`,
`ApplyOverrides`, `CheckRecorded`), `dag` (`New`, `ConnectAll`, `Layers`).

References: libmamba `src/core/transaction.cpp`, `transaction_context.cpp`
(pyc compilation, ~lines 180–420), `data/compile_pyc.py`; rattler
`crates/rattler/src/install/driver.rs`, `installer/`, `transaction.rs`,
`clobber_registry.rs`.

## Required API

```go
type InstallOptions struct {
    PkgsDir      string                  // package cache (fetch.Cache.Dir); required
    Client       *http.Client            // nil = fetch's default (no proxy from env)
    Concurrency  int                     // downloads and links in flight; 0 = runtime.NumCPU(), max 64
    CompilePyc   bool                    // compile .pyc for noarch python packages (micromamba default: true)
    AllowScripts []string                // passed to link
    Virtual      []*record.PackageRecord // the host's virtual packages; nil = virtual.Detect
    Progress     func(Event)             // optional; may be called from any goroutine
}
// InstallLock creates prefix (which must not exist, or be an empty
// directory) from lf. On any error or cancellation the prefix is removed
// again if this call created it; the package cache is kept.
func InstallLock(ctx context.Context, prefix string, lf *lock.Lockfile, opt InstallOptions) error
type Event struct{ Kind EventKind; Package string; Done, Total int64 }
```

## Steps

1. `lf.Validate()`. `lf.Platform` must equal `virtual.HostPlatform()`.
2. Virtual packages: every `depends` entry of a locked package that names a
   virtual package (`__glibc`, `__osx`, `__cuda`, …) must be satisfied by
   `opt.Virtual` (parse with `spec.ParseMatchSpec`, test with
   `MatchesVersion`). Fail before any download, naming the package and the
   unmet spec.
3. Fetch and extract every package with `fetch.Cache.Extracted`, bounded by
   `Concurrency`, cancelling the rest on the first error.
4. Plan with `dag.New[string](3)`, one node per package name:
   - dimension 0, link order: an edge dependency → dependent for every
     `depends` name present in the lockfile (ignore virtual names);
   - choose the cycle-breaking edges explicitly before adding anything: an
     edge from a non-noarch package to a noarch package that lies on a cycle
     goes to dimension 1 instead (rattler `topological_sort.rs` rule: noarch
     packages link after the platform packages in their cycle). Never rely
     on edge insertion order; add each dimension's edges with `ConnectAll`.
     Any cycle left after that rule is an error naming the cycle;
   - dimension 2: `constrains` edges (stored for later impact analysis).
5. Prefix: create it, hold a file lock on it for the whole transaction, write
   `conda-meta/.goomba-incomplete` first and remove it last. (`link.Link`
   creates `conda-meta` itself and never removes it.)
6. Link layer by layer (`Layers(0)`), packages within a layer in parallel,
   with `link.Link` and `link.NewPythonInfo` from the locked python record
   (required when any `noarch: python` package is present). Clobbering:
   `link.ErrExists` because another package of this transaction wrote the
   path is an error naming both packages (micromamba warns and lets the
   later one win; goomba fails closed). After all layers, walk the prefix and
   verify every symlink resolves inside it (defence in depth; `link` already
   refuses escaping targets).
7. conda-meta: lockfile records carry no `track_features`, `timestamp`,
   `license` or `features`; before `link.WriteCondaMeta`, fill those record
   fields from the package's own `info/index.json` (`Installed.Index`,
   already hash-verified), so the records match micromamba's. Then one
   `link.AppendHistoryEntry` for the whole install, including the
   `# update specs` line from `lf.Specs`.
8. `.pyc`: when `CompilePyc` and python is in the lockfile, run the installed
   interpreter exactly as libmamba does: `<prefix>/<python> -Wi -u <script>`
   with libmamba's `compile_pyc.py` embedded (port it, keep its origin
   header), an empty environment, working directory = prefix, and the
   `Installed.PythonSources` paths on stdin. This is the only process goomba
   runs (`os/exec` is allowed here and nowhere else in your files); document
   it. It runs an interpreter whose archive was just verified against the
   lockfile.
9. On any failure after step 5: unlink what was linked, remove the prefix if
   this call created it, keep the package cache.

## CLI (`cmd/goomba`)

`goomba create --lock FILE -p PREFIX [--pkgs-dir DIR] [--no-pyc]`,
`goomba lock --from-prefix PREFIX -o FILE`, `goomba export --explicit
--lock FILE`, `goomba list -p PREFIX`. Flags only; the CLI reads
`HTTPS_PROXY`/`NO_PROXY` to build its `http.Client` (the library never does)
and nothing else from the environment. Exit codes: 0 ok, 1 failure, 2 usage.

## Tests

- Offline unit tests against a local `httptest` channel built from small real
  packages (copy a handful of tiny `.conda` files into testdata, ≤ 5 MB):
  ordering incl. a python↔pip-style cycle, clobber detection, virtual-package
  refusal, cancellation at many points leaving no prefix, concurrency,
  history and conda-meta contents.
- Differential test (`GOOMBA_LIVE=1` and `GOOMBA_MICROMAMBA` set): build a
  lockfile with `lock.FromPrefix` from `$GOOMBA_REF_ROOT/envs/base-3.11`,
  export it as `@EXPLICIT`, create one prefix with goomba and one with
  `micromamba create -y -p <scratch> -f explicit.txt` (scratch root prefix
  inside your worktree, explicit `MAMBA_ROOT_PREFIX`), and compare the trees:
  same paths, types, symlink targets, file contents after replacing each
  prefix string, the same `conda-meta` JSON after normalising
  prefix-dependent digests (micromamba 2.2.0 writes a stale constant digest
  for prefix-rewritten files), and the same `.pyc` set. Allowed, documented
  differences: entry-point mode 0755 vs micromamba's 0775, and goomba's real
  digests. Then run the goomba environment's python: `import ssl, sqlite3,
  ctypes, lzma, bz2, zlib, json` must succeed.
