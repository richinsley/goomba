# WP8c — symlink targets that are not in normal form, prefix status, and a live install sweep

Read `AGENTS.md` first. You own `link/`, the root package `goomba` (only
for the prefix-status function below) and new test files. Edit nothing
else.

## Why

Today's conda-forge `ncurses-6.6-hdb14827_1` ships the symlink
`share/terminfo/31/1178 -> .././61/adm1178`. goomba refuses to install it:

```
goomba: link: ncurses-6.6-hdb14827_1: symlink share/terminfo/31/1178: unsafe path: symlink target ".././61/adm1178" is not in normal form
```

Every Python environment on linux-64 pulls in ncurses, so goomba cannot
create one from current conda-forge. The differentials did not catch this
because the frozen reference index predates that build. micromamba installs
the package and writes the target unchanged.

## 1. Accept harmless non-normal symlink targets

In `link/guard.go`, `symlinkTargetShape` refuses any `""`, `.` or `..`
component after the leading `..` run.

- **Accept** `.` components and empty components (`a//b`, a trailing
  slash) anywhere, including between leading `..` components
  (`.././../x`). In the kernel's path resolution they are no-ops, or only
  make resolution fail (`file/.` and `file/` give ENOTDIR). Drop them
  before the escape check, so the checked shape is the normalized one.
- **Keep refusing**:
  - `..` after a name component (`a/../b`), because `a` may be a symlink;
  - absolute targets;
  - NUL and backslash;
  - an empty target;
  - any component that names conda-meta (including `./conda-meta`).
- **Write the target exactly as the package has it**, as micromamba does,
  so the differential compares equal targets.
- Check every caller of `symlinkTargetShape` (`link.go` around lines 669
  and 760, and `PlannedPaths`). Look for anything else that assumes normal
  form. `archive` already extracted this package, so check how it treats
  such targets and keep the two packages consistent.
- **Tests:**
  - a table of accepted and refused targets, with the depth check at its
    limits (for example `.././../x` at depth 1 and 2);
  - the ncurses case as a testdata package or a constructed one;
  - a kernel cross-check: after `Link`, every accepted target resolves
    inside the prefix.
  - Extend the fuzz target if one covers this function.

## 2. Prefix status for callers that reuse environments

jumpboot reuses an existing environment directory and must tell an
environment another process is still installing from one a crash left
incomplete. Both have `conda-meta/.goomba-incomplete`. Add to the root
package, for example:

```go
// ErrIncomplete: the prefix holds an interrupted goomba transaction and
// no process is working on it.
var ErrIncomplete = errors.New("prefix holds an interrupted installation")

// CheckComplete reports whether prefix holds a finished environment.
// nil: no incomplete marker. ErrPrefixLocked: another process holds the
// prefix lock (an install is running). ErrIncomplete: the marker is
// present and nobody holds the lock.
func CheckComplete(prefix string) error
```

- Use the same lock as `InstallLock` (flock on Unix; whatever
  `prefixlock_other.go` does elsewhere). Take it without blocking and
  release it at once.
- Never create, remove or modify anything.
- Resolve the path with the same safedir rules `InstallLock` uses.
- Document the race: the answer can change as soon as it is returned.
- Name it as the code suggests and report the final API.
- **Tests:** a missing prefix, a complete prefix, a prefix with the marker
  and no lock, and a prefix with the marker whose lock another process
  holds. The last one needs a helper process or an fd held from another
  open file description: flock locks are per open file description, so a
  second open in the same process works.

## 3. Live install sweep (`GOOMBA_LIVE=1`)

Add `TestLiveCreateSweep` to the root package, skipped unless
`GOOMBA_LIVE=1`.

- Run `goomba.Create` from conda-forge, sharing one package cache and one
  repodata cache, for:
  - `python=3.9`, `3.10`, `3.11`, `3.12` and `3.13`;
  - `python=3.12 numpy pandas scipy matplotlib-base`;
  - `python=3.12 sphinx requests flask`.
- Each environment must install, and `python -c "import ssl, sqlite3,
  ctypes"` must succeed. Run the interpreter with an empty environment and
  the prefix's bin first; this runs only in the test.
- Report the download size, the time per environment, and any package
  goomba refused, with the reason. If the sweep finds other refusals of
  packages micromamba installs, treat each the same way: decide whether it
  is safe, fix it if it is, and report it if it is not.
- Keep it to these sets, about 1 GB of downloads at most. Use a scratch
  directory under the test's temp dir and remove it at the end.

## Gates

`scripts/gate.sh ./link/... .`, fuzz 60 s for any fuzz target you change,
`CGO_ENABLED=0` build, windows and darwin vet, check-paths, the micromamba
differentials (`TestDifferentialMicromamba`, `TestDifferentialCreate`) and
the live sweep once.
