# WP6 — `cdcl`: generic conflict-driven solver (port of resolvo)

Read `AGENTS.md` first. You own `cdcl/`.

## Goal

Port resolvo (`$GOOMBA_REFS/resolvo`, about 11,300 lines of
Rust) to Go as package `cdcl`: a package-agnostic CDCL solver with lazily
loaded candidates and human-readable unsatisfiability explanations. It must
know nothing about conda. WP7 will build the conda provider on top of it, so
the API you choose is the contract; keep it close to resolvo's so its design
notes still apply.

Primary reference: resolvo `src/` (`lib.rs`, `requirement.rs`,
`conditional_requirement.rs`, `conflict.rs`, `solver/*`, `internal/*`,
`utils/*` including the `Pool`). Read its README and docs first.

## Shape (adapt to Go; keep the concepts)

- IDs as small integer types: `NameID`, `SolvableID`, `VersionSetID`,
  `VersionSetUnionID`, `StringID`, `ConditionID`.
- `Interner` (display and lookups: `DisplayName`, `DisplaySolvable`,
  `DisplayVersionSet`, `DisplayString`, `VersionSetName`, `SolvableName`,
  `VersionSetsInUnion`, ...).
- `DependencyProvider` (embeds `Interner`): `GetCandidates(ctx, NameID)`
  returning candidates plus favored / locked / excluded-with-reason and a
  hint for dependencies known to be available; `SortCandidates(ctx, []SolvableID)`
  (best first); `FilterCandidates(candidates, VersionSetID, inverse bool)`;
  `GetDependencies(ctx, SolvableID)` returning requirements (single version
  sets or unions, optionally conditional) and constrains, or an "unknown"
  reason. Go methods are synchronous; they may block on I/O and must honour
  ctx.
- A `Pool` helper like resolvo's for interning names, solvables, version sets
  and strings, so providers do not reinvent it.
- `Problem`: root requirements, root constraints, soft requirements.
- `Solver.Solve(ctx, Problem) ([]SolvableID, error)`; unsat returns
  `*Conflict` whose `Display(interner)` produces resolvo's explanation text
  (same graph construction and wording, adapted to Go formatting).
  Cancellation returns `ctx.Err()` wrapped.

Internals: watched literals, clause kinds (requires, forbid-multiple-instances,
constrains, lock, learnt, excluded, root, conditional), decision tracker,
propagation, conflict analysis with learnt clauses and backjumping, the
decision heuristic and candidate ordering as resolvo does them. Keep the
solver deterministic: same provider answers → same solution.

## Tests

- Port resolvo's solver test suite: `tests/solver/main.rs` (105 `#[test]`s)
  with its toy provider `tests/solver/bundle_box/` (`BundleBoxProvider`,
  `Pack`, its spec parser) and the 39 snapshots in
  `tests/solver/snapshots/`. Port the provider into a `_test.go` helper (or
  `cdcl/internal/bundlebox`) and port the tests with the same names and
  expected output. Snapshot texts must match after you document any
  formatting differences.
- Also port the unit tests inside resolvo `src/` that test internals.
- Property test: random small problems (≤ 8 names, ≤ 4 versions each,
  random deps and constrains) solved by `cdcl` and by brute force; a
  solution must satisfy every clause, and unsat must agree with brute force.
- Benchmark: a synthetic problem of ~5,000 solvables; report time and
  allocations.

Fuzz: `FuzzSolveRandom` (decode random bytes into a small problem; never panic;
any solution verifies).
