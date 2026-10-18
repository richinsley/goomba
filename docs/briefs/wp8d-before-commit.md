# WP8d — a caller hook inside the install transaction

Read `AGENTS.md` first. You own the root package `goomba`: `install.go`, `prefix.go`, `errors.go` and new test files. Edit nothing else unless this brief says so.

## Why

jumpboot finishes a goomba environment with work of its own:

- it checks `python --version` and `pip --version`;
- the JSON restore runs `pip install`.

Today that work runs after `InstallLock` has committed and released the prefix lock, which causes three problems:

- **Reuse of an unfinished environment.** Between goomba's commit and jumpboot's own lock, `CheckComplete` reports the environment finished. Another caller can then reuse it before pip ran.
- **A broken environment looks finished.** A failed post-install check leaves a complete-looking environment that every later call reuses. `RemoveIncomplete` refuses to remove it.
- **Cancellation is lost.** Cancelling the pip step cannot roll anything back.

The caller's finishing work has to happen inside goomba's transaction.

## API

```go
type InstallOptions struct {
    // ...existing fields...

    // BeforeCommit, if set, is called once, after every package is linked,
    // byte-compiled, recorded in conda-meta and verified, and before the
    // transaction commits. It runs while InstallLock still holds the prefix
    // lock and the incomplete marker is still present, so CheckComplete
    // reports ErrPrefixLocked to everyone else throughout. prefix is the
    // resolved prefix path InstallLock installs into.
    //
    // If it returns an error, or ctx is done when it returns, InstallLock
    // rolls the whole transaction back, as for any other failure, and
    // returns an error wrapping the hook's error (or ctx.Err()). A panic in
    // it rolls back and is re-raised.
    BeforeCommit func(ctx context.Context, prefix string) error
}
```

- Adjust the name or signature if the code suggests something better, and report the final API.
- `Create` passes it through, because it calls `InstallLock`.

## Behaviour

- **Call order.** Call the hook exactly once, at the latest point before commit:
  - after symlink verification and the history entry;
  - before `commit` sets the final mode and removes the marker.
- **Directory mode.** The prefix keeps its transaction mode (0700) during the hook. Document this; the caller's processes run as the same user.
- **Files the hook adds.** Anything the hook writes into the prefix is the caller's responsibility:
  - goomba does not verify it;
  - goomba removes it on rollback, because the whole prefix is emptied through the locked handle as today.
  - Document that the hook must not touch `conda-meta/.goomba-incomplete` or remove `conda-meta`.
- **Errors.** Wrap the hook's error unchanged inside goomba's context ("before commit"), so that `errors.Is` and `errors.As` reach it. Cancellation inside the hook surfaces as `ctx.Err()`.
- **Progress.** Emit an event kind (for example `EventBeforeCommit`) when the hook starts, if the event model makes that natural.
- **Docs.** Update the `InstallLock` and package docs: goomba still runs no process of its own other than byte-compilation, and a hook runs whatever the caller chooses.

## Tests

- **Hook success:** the hook runs once with the resolved prefix, while:
  - the marker exists;
  - `CheckComplete` from another goroutine or open file description returns `ErrPrefixLocked`;
  - the prefix is 0700.
  After commit, `CheckComplete` returns nil.
- **Hook failure:** a hook error rolls back fully (nothing left, or an existing directory restored to its mode), and the returned error wraps the hook's error.
- **Cancellation:** cancelling ctx while the hook runs (for example a hook that waits on ctx) rolls back and returns `context.Canceled`.
- **Panic:** a panic in the hook rolls back and re-panics.
- **Files the hook wrote** (files and directories) are removed on rollback.
- **Process isolation** (Linux): a second process that calls `InstallLock` or `RemoveIncomplete` for the same prefix while the hook runs gets `ErrPrefixLocked`.
- **Order:** the hook runs after the history entry and symlink verification, and before the marker is removed. Prove this with a hook that inspects the prefix.

## Gates

- `scripts/gate.sh .`
- `CGO_ENABLED=0` build
- windows, darwin and freebsd vet
- check-paths
- `TestDifferentialMicromamba` and `TestDifferentialCreate` once each
