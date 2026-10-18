# goomba — guide for implementing agents

goomba replaces micromamba with pure Go: it resolves conda match specs to
lockfiles and installs lockfiles into environment prefixes. The design is
summarised in `docs/architecture.md`; your brief in `docs/briefs/` restates
everything you need from the original design document.

The reason goomba exists is supply-chain safety. Treat every input (archives,
index data, lockfiles, entry-point strings, paths) as hostile, and fail closed.

## Your work package

You own exactly the directories your brief lists. Do not edit anything else.
If you need a change to a shared contract (`record/record.go`,
`repodata/source.go`, `go.mod` beyond adding an allowed dependency), do not
make it: describe it in your report.

Work only in the git worktree your prompt names, on its branch. Commit there
with clear messages; never push, never merge, never touch other worktrees.
End every commit message with:

    Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>

## Rules

- Go language level 1.24 (`go.mod`). The machine has Go 1.25.7: always run Go
  with `GOTOOLCHAIN=local`.
- Pure Go, `CGO_ENABLED=0` must build. No `os/exec` unless your brief allows a
  specific command.
- Dependencies: the standard library plus exactly these, at these versions:
  `github.com/klauspost/compress@v1.18.0`,
  `github.com/vmihailenco/msgpack/v5@v5.4.1`, `golang.org/x/sys@v0.40.0`.
  Never add another module.
- Library code never reads environment variables, config files or the home
  directory. Every setting arrives through function arguments or an
  `Options` struct. Tests may read the `GOOMBA_*` variables listed under
  Reference material.
- Nothing committed may contain a machine-specific path, user name or home
  directory: not code, tests, testdata, docs or commit messages. Refer to
  reference material through the `GOOMBA_*` variables; in prose, write them
  as `$GOOMBA_REFS/...`. `scripts/check-paths.sh` enforces this.
- No package-level mutable state, no `init()` side effects.
- Every operation that does I/O or can take long takes `ctx context.Context`
  first and stops promptly when it is cancelled.
- Deterministic output: never let map iteration order reach a result, a file
  or an error message. Sort.
- Errors: wrap with `%w`; export sentinel errors or error types for conditions
  callers must branch on; messages start lower-case and name the input
  (`spec: parse version "1..2": empty segment`).
- Every exported identifier has a doc comment. Document whether each exported
  type is safe for concurrent use.
- `gofmt` clean.

## Reference material

Three environment variables locate read-only reference material. Tests that
need one read it and call `t.Skip` when it is unset, so the suite passes on
any machine.

| Variable | Points to | How to get it |
| --- | --- | --- |
| `GOOMBA_REFS` | A directory holding clones `mamba/`, `resolvo/`, `rattler/`, `pydagger/` | `git clone` each of `github.com/mamba-org/mamba`, `github.com/prefix-dev/resolvo`, `github.com/conda/rattler`, `github.com/richinsley/pydagger` |
| `GOOMBA_REF_ROOT` | A micromamba root prefix holding `envs/base-3.11` and its `pkgs/` cache | `MAMBA_ROOT_PREFIX="$GOOMBA_REF_ROOT" micromamba create -n base-3.11 -c conda-forge python=3.11 -y` |
| `GOOMBA_MICROMAMBA` | A micromamba binary, used only as a test oracle | `github.com/mamba-org/micromamba-releases` |
| `GOOMBA_LIVE` | Set to `1` to run tests that use the network | — |

Prefer copying the small upstream file a test needs into `testdata/` (its
origin named in a header or a `testdata/README`) over reading `$GOOMBA_REFS`
at test time, so the tests stay self-contained.

## Porting

| Source | What |
| --- | --- |
| `$GOOMBA_REFS/mamba` | mamba / libmamba (C++), BSD-3 |
| `$GOOMBA_REFS/resolvo` | resolvo solver (Rust), BSD-3 |
| `$GOOMBA_REFS/rattler` | rattler conda crates (Rust), BSD-3 |
| `$GOOMBA_REFS/pydagger` | pydagger DAG (Python), MIT |

Port behaviour, not syntax: write idiomatic Go that produces the same results.
Each file that ports logic starts with a header naming its origin, e.g.

    // Ported from libmamba src/specs/version.cpp (BSD-3-Clause, see NOTICE).

When the references disagree, follow the one your brief names as primary, and
list the disagreement in your report.

The micromamba-built environment under `$GOOMBA_REF_ROOT` is read-only: never
modify it. `$GOOMBA_MICROMAMBA` may be run only with a scratch root prefix
inside your working tree, and always with `MAMBA_ROOT_PREFIX` set explicitly
(an inherited value would point it somewhere else).

## Tests

- Unit tests run offline. Live network tests start with
  `if os.Getenv("GOOMBA_LIVE") == "" { t.Skip("set GOOMBA_LIVE=1") }`.
- Port upstream test cases verbatim into table-driven tests. Put a comment on
  each table naming the source file, e.g.
  `// Cases from libmamba tests/src/specs/test_version.cpp`.
- Fixtures live in your package's `testdata/`, at most 5 MB per package. You
  may copy small archives from `$GOOMBA_REF_ROOT/pkgs` (for example
  `wheel-0.46.3-pyhd8ed1ab_0.conda`, 32 KB) or download small files from
  conda.anaconda.org.
- Every parser gets a Go fuzz target (`FuzzXxx`) seeded from your test cases.

## Resource limits

The machine is shared with other agents. Run every probe, stress test,
benchmark and differential run you write under a hard address-space cap in
the same shell, one heavy run at a time:

    ( ulimit -v 16000000; GOTOOLCHAIN=local go test -run TestMyProbe ./yourpkg )

The race detector cannot run under `ulimit -v`, so use `-race` only for the
committed suite (`scripts/gate.sh`). An unbounded probe once grew to 119 GB
and was killed by the kernel; a watchdog kills any test binary above 24 GB.
If a test needs more than a few GB, that is itself a finding: measure the
growth curve on small inputs instead of running the big one.

## Gates (all must pass before you report done)

1. `scripts/gate.sh ./yourpkg/...` — build, vet, `go test -race`, gofmt, and
   `scripts/check-paths.sh` over your package (testdata included).
2. Each fuzz target: `GOTOOLCHAIN=local go test -run='^$' -fuzz=FuzzXxx -fuzztime=60s ./yourpkg`.
   Commit any crasher it finds as a regression case after fixing it.
3. `CGO_ENABLED=0 GOTOOLCHAIN=local go build ./yourpkg/...`.

## Report

Finish with a report containing:

1. What you built: packages, files, exported API (signatures).
2. Upstream cases: how many you ported, from which files, and every case you
   skipped with the reason.
3. Deviations from the reference implementation, with reasons.
4. Contract change requests and open questions.
5. The gate output (last lines) and fuzz results.
6. The commit hash(es) on your branch.
