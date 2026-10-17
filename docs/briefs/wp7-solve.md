# WP7 — `solve`: the conda resolver on top of `cdcl`

Read `AGENTS.md` first. You own `solve/` only.

## Goal

Resolve conda match specs to an exact, valid set of package records using
`cdcl` (goomba's port of resolvo) with conda's rules, and explain
unsolvable requests readably. This is the piece that lets goomba produce
lockfiles without micromamba. Correctness first: a solution must always be
valid, and "unsolvable" must only be reported when no valid solution exists
under the rules below.

Primary reference: rattler `crates/rattler_solve/src/resolvo/` (`mod.rs`, the
`CondaDependencyProvider`; `conda_sorting.rs`) and `src/lib.rs`
(`SolverTask`, `SolveStrategy`, channel priority). Behavioural oracle:
micromamba (via `$GOOMBA_MICROMAMBA`).

Merged packages you build on (read their docs; do not change them — report
needed changes): `record`, `spec` (versions, `MatchSpec`, `ParseMatchSpec`,
`MatchesVersion`, `ChannelURL`), `repodata` (`Source`, `Prefetcher`,
`NameLister`, `Memory`, `Multi`, `LoadRepodataJSON`, `NewShardedChannel`,
`NewFullIndex`), `virtual` (`Detect`, `Defaults`, `ApplyOverrides`), `cdcl`
(`DependencyProvider`, `Pool`, `Solver`, `Problem`, `Conflict`).

Notes on `cdcl` as merged: IDs must be dense (use `cdcl.Pool`; anything
above `cdcl.MaxID` is refused); requirements are `ConditionalRequirement`
values (use `Requirement.Unconditional()`); there are no soft requirements —
prefer installed packages through `Candidates.Favored`/`Locked`;
`Conflict.Error()`/`Display` render a bounded explanation (repeated
candidates shown once, capped at 64 KiB / 2,000 lines) and
`Conflict.Graph()` returns `(*ConflictGraph, error)`; `Solve` never panics
on provider bugs and honours `ctx`. Read `cdcl/doc.go` first.

## Required API

```go
type Request struct {
    Specs       []string                // what the user asked for, e.g. "python=3.11"
    Constraints []string                // root constraints: restrict a package if it is installed, never require it
    Virtual     []*record.PackageRecord // the target machine's virtual packages
    Favored     []*record.PackageRecord // e.g. installed packages to keep when possible (phase 3; may be nil)
    Locked      []*record.PackageRecord // packages that must stay exactly as they are (phase 3; may be nil)
    ChannelAlias string                 // for channel-qualified specs; "" = https://conda.anaconda.org
}
type Solution struct {
    Records []*record.PackageRecord // sorted by name; no virtual packages
}
// Solve resolves req against src. An unsolvable request returns an error
// that errors.As to *Unsolvable, whose Error() is a readable explanation.
func Solve(ctx context.Context, src repodata.Source, req Request) (*Solution, error)
type Unsolvable struct{ /* explanation, plus the cdcl conflict for tools */ }
// RequestedPythonMinor returns "3.11" for specs that pin python to one
// minor (port of libmamba's extract_requested_python_minor), else "". The
// caller passes it to repodata.Options.PythonMinor when opening channels.
func RequestedPythonMinor(specs []string) string
// Validate checks that recs is a valid solution of req under conda's rules
// (every spec satisfied, every depends satisfied, every constrains holding,
// one record per name, virtual packages respected). Tests and the CLI use
// it as an independent check of every solution.
func Validate(req Request, recs []*record.PackageRecord) error
```

## Rules

- One record per package name. Every spec in `req.Specs` must be satisfied;
  every chosen record's `depends` satisfied; `constrains` of chosen records
  and `req.Constraints` hold whenever the named package is installed.
- Strict channel priority is the `Source`'s job (`repodata.Multi`); the solver
  never mixes channels for one name. A channel-qualified spec
  (`conda-forge::numpy`) only matches records whose `Channel` equals
  `spec.ChannelURL(alias)`.
- Virtual packages: names starting with `__` come only from `req.Virtual`;
  a dependency on a missing one makes that record uninstallable, with the
  reason in the explanation.
- Records are excluded (with a reason shown in explanations, via
  `cdcl.ExcludedSolvable`) when: they have no sha256 ("no sha256: cannot be
  verified"), any of their `depends`/`constrains` fail to parse ("unparsable
  dependency …"), or a `Locked` record of the same name exists.
- Candidate order: port `conda_sorting.rs` exactly — no `track_features`
  first; higher version; higher build number; then the dependency-version
  score; then newer timestamp. Default strategy only (highest).
- Match-spec matching goes through `spec`; cache parsed versions per record
  and match results per (version set, record) — `cdcl.SolverCache` already
  caches `MatchingCandidates`; do not re-parse in hot paths.
- Prefetch: before solving, if `src` implements `repodata.Prefetcher`, call
  `Prefetch` with the names in `req.Specs` so the sharded index walks
  dependencies in parallel.
- Determinism: the same request against the same records always gives the
  same solution and the same explanation text.
- Conditional (`; if`) dependencies and extras are out of scope for v1:
  records using them are excluded with a reason. Document it.

## Tests to port

- rattler `crates/rattler_solve/tests/backends/` for the resolvo backend:
  `solver_case_tests.rs`, `sorting_tests.rs`, `multichannel_tests.rs`, the
  relevant parts of `main.rs`, and `strategy_tests.rs` for the default
  strategy; skip conditional/extras/min-age/timestamp-policy/variant-flag
  tests and list them. Their fixture channels are in rattler
  `test-data/channels/` (dummy, multichannel-a/b/c, pytorch, …): copy what
  you need into `solve/testdata` with their origin noted (≤ 5 MB) and load
  them with `repodata.LoadRepodataJSON` into `repodata.Memory`.
- Snapshot expectations from rattler's `snapshots/` where they exist: the
  solution sets must match; explanation text follows cdcl's format (document
  any wording differences from rattler).
- Differential against micromamba (`GOOMBA_MICROMAMBA` set, plus
  `GOOMBA_LIVE=1` or a local repodata snapshot): serve a frozen copy of
  conda-forge `linux-64` + `noarch` repodata from an `httptest` loopback
  server (copy the cached repodata from `$GOOMBA_REF_ROOT/pkgs/cache` into a
  scratch dir inside your worktree; never modify the reference root), point
  both goomba (`repodata.NewFullIndex`) and `micromamba create --dry-run
  --json --override-channels -c <that URL>` (scratch root prefix, explicit
  `MAMBA_ROOT_PREFIX`) at it, and compare the solved package sets. Corpus:
  `python=3.9` … `python=3.13`; `numpy`, `scipy`, `pandas` with python 3.11;
  `pytorch` CPU; `jupyterlab`; a CUDA package with `__cuda=12.4` overridden;
  free-threaded `python=3.13` `*_cp313t`; a noarch-heavy stack; and a
  known-unsolvable request. Every difference must be explained: both
  solutions pass `Validate`, and the reason (usually a tie in candidate
  order) is recorded in the test with the exact records involved.
- Property test: random small channels (built in memory) solved by `Solve`,
  every solution checked with `Validate`, every unsolvable answer checked by
  brute force for small cases.
- Benchmarks: `python=3.11` against the frozen full index, and against a
  sharded channel from cold cache (live); report time and memory.

Fuzz: `FuzzSolveSmallChannel` (decode random bytes into a tiny channel and
request; never panic; every solution passes `Validate`).
