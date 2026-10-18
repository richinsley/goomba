# Failures and retries

What happens to a prefix when creating an environment fails, what stays
locked, and what a caller has to handle itself. The guarantees behind this
page are in [transactions.md](transactions.md).

## The short version

- **An error rolls back.** When `InstallLock` or `Create` returns an error,
  including a cancellation or a timeout, it has removed what it wrote:
  - a prefix the call created is removed;
  - an empty directory it was given is empty again;
  - a prefix it refused (`ErrPrefixNotEmpty`, `ErrPrefixLocked`,
    `ErrUnsafePrefix`) was never touched.
- **A rollback that cannot finish says so.** Something mounted inside the
  prefix, or files appearing faster than goomba removes them, can stop the
  rollback. The prefix then keeps its marker, as after a kill, and the
  returned error joins the rollback's error (`ErrMountPoint` for a mount
  point) to the one that caused it.
- **Writes after the rollback are invisible.** A process a failed hook left
  running can write into the prefix after goomba has finished rolling back.
  Those files have no marker, so the prefix then reads as a directory of
  other files, which only manual deletion clears. A `BeforeCommit` hook must
  not return until everything it started has exited.
- **A killed process leaves a marked prefix.** If the process dies (SIGKILL,
  OOM, a crash), the prefix keeps `conda-meta/.goomba-incomplete`.
  `CheckComplete` then reports `ErrIncomplete`, and the prefix must be
  removed before anything is installed there again. goomba never resumes an
  installation.
- **The package cache is always kept**, so a retry does not download again.
- **goomba never waits for another installer.** It fails with
  `ErrPrefixLocked`; the caller decides whether to wait.

## What stays locked, and when

| Phase | Prefix lock |
| --- | --- |
| `Lock`: loading indexes and solving | none (the prefix is not involved) |
| `InstallLock` checks and downloads | none; the prefix is not taken yet |
| `InstallLock` from taking the prefix to the commit, including linking, byte-compilation and `BeforeCommit` | exclusive |
| rollback after a failure | exclusive, until the prefix is empty or removed, or the rollback gives up |
| `RemoveIncomplete` | exclusive |
| `CheckComplete` | shared, for a moment |
| after `InstallLock` returns, or the process dies | none |

The lock is `flock` on the prefix directory, so the kernel releases it when
the process dies. That is why a killed installation reads as `ErrIncomplete`
(marker present, nobody holding the lock) rather than `ErrPrefixLocked`.

The lock is advisory. It keeps out other goomba calls, and callers that ask
`CheckComplete` before using an environment. It does not stop the user's own
processes from running `bin/python` in a prefix that is still being
installed.

## Errors and what to do

Branch on these with `errors.Is` and `errors.As`. goomba wraps errors from
the packages it builds on unchanged, so their sentinels work too. These are
the errors callers branch on; the API reference lists the rest.

### Input and the machine

| Error | From | What to do |
| --- | --- | --- |
| `ErrInvalidOptions` | Lock, InstallLock, Create, RemoveIncomplete, the Validate functions | Fix the input: a spec that does not parse, a refused channel, a missing `PkgsDir` or `RepodataDir`, a cache inside the prefix, an empty prefix path. The CLI exits 2. |
| `lock.ErrInvalid` | `lock.Read`, InstallLock, Lock | The lockfile fails validation, or a channel places packages where a lockfile cannot name them (a `base_url` that does not end in `/<subdir>/`). |
| `*solve.Unsolvable` | Lock, Create | Print `Explanation` and change the specs. |
| `ErrPlatform` | InstallLock, Create | The lockfile is for another platform. Lock for this one. |
| `*VirtualError` (`ErrVirtual`) | InstallLock, Create | A package needs a virtual package this machine lacks (`__glibc` too old, no `__cuda`). Use another lockfile, or override the virtual package if you know better. |

Nothing has been done to the prefix after any of these.

### The prefix and the caches

| Error | From | State afterwards | What to do |
| --- | --- | --- | --- |
| `ErrPrefixNotEmpty` | InstallLock, Create | prefix untouched | Choose another prefix or, if another installer may have just filled it, ask `CheckComplete` again. |
| `ErrPrefixLocked` | InstallLock, Create, RemoveIncomplete, CheckComplete | prefix untouched | Another process is installing, rolling back or removing it. Wait, then ask `CheckComplete` again. |
| `ErrUnsafePrefix` | InstallLock, Create, CheckComplete, RemoveIncomplete, ValidateInstall | prefix untouched | The prefix, or a directory above it, can be changed by another user. `errors.As` with `*goomba.UnsafePathError` usually gives the offending `Path`. Fix its ownership or permissions, or use another path. |
| `fetch.ErrUnsafeCache`, `repodata.ErrUnsafeCache` | InstallLock, Lock, ValidateInstall | nothing done | The package or index cache is not private to the user. `*goomba.UnsafePathError` usually names the path here too; refusals of the cache's own files (its key, lock files) carry none. |
| an error wrapping `fs.ErrExist` | InstallLock, CheckComplete | prefix untouched | The prefix path is a dangling symlink. |
| `ErrIncomplete` | CheckComplete | prefix marked, unusable | A killed installation, or a rollback that could not finish, left it. Call `RemoveIncomplete`, then install again. |
| `ErrNotIncomplete` | RemoveIncomplete | prefix untouched | Nothing to remove: the prefix holds a finished environment or, if the error also wraps `fs.ErrNotExist`, no environment. Another caller may have removed and reinstalled it first; ask `CheckComplete` again. |
| `ErrMountPoint` | RemoveIncomplete; InstallLock, joined to the error that caused the rollback | prefix still marked | Something is mounted inside the prefix, and nothing below it was touched. Unmount it, then call `RemoveIncomplete`. |
| `errors.ErrUnsupported` | RemoveIncomplete | prefix untouched | No prefix lock on this platform (Windows), so goomba cannot tell an interrupted installation from a running one. |

### Downloads

| Error | From | What to do |
| --- | --- | --- |
| `*fetch.IntegrityError`, `*fetch.SizeError` | InstallLock | An archive does not match the lockfile. goomba never retries these. Treat it as a changed or tampered package. |
| `*fetch.HTTPError`, network errors | InstallLock | Transient failures were already retried 3 times. Retry later. |
| `*repodata.HTTPError`, `repodata.ErrNotFound`, network errors | Lock | A channel index could not be loaded; transient failures were tried 3 times in all. Check the channel name, or retry later. |
| `*repodata.HashMismatchError` | Lock | A shard does not match the hash its shard index lists. Treat it like a tampered package. |
| `repodata.ErrTooLarge` | Lock | An index is larger than goomba accepts. |
| `repodata.ErrNotCached` | Lock with `Offline` | The index cache lacks a channel. Run once online. |
| `fetch.ErrInsecureURL`, `repodata.ErrUnsupportedURL` | InstallLock, Lock | The network policy refused a URL, a redirect or the address a name resolved to (see [compatibility.md](compatibility.md#network-policy)). Never retried. |

After a download error the prefix has not been taken yet, so nothing is in
it.

### Packages and installing

Most of these are found before the prefix is taken, so nothing was written
to it. The others were rolled back (see above for a rollback that cannot
finish).

| Error | From | What to do |
| --- | --- | --- |
| `*ClobberError` (`ErrClobber`) | InstallLock, Create | Two packages install the same path. micromamba would overwrite one; goomba refuses. Change the specs. |
| `link.ErrLinkScript` | InstallLock, Create | A package ships a pre-link, post-link or pre-unlink script. goomba never runs them. A library caller can let the script be installed (not run) with `InstallOptions.AllowScripts`; the CLI cannot. |
| `link.ErrUnsafePath`, `link.ErrInvalidPackage`, `link.ErrInvalidEntryPoint`, `archive.ErrUnsafePath` | InstallLock, Create | A package is malformed or hostile: a path that escapes, metadata that does not parse, an entry point that is not `module:function`. Do not use the package. |
| `link.ErrUnsupported` | InstallLock, Create | Something goomba cannot install, such as Python entry points for Windows. |
| `link.ErrPrefixTooLong` | InstallLock, Create | A binary file's placeholder is shorter than the prefix path. Use a shorter path. |
| `ErrCompilePyc` | InstallLock, Create | The new interpreter could not be run to byte-compile. Retry without byte-compilation (`CompilePyc: false`, `--no-pyc`) if you do not need it. |
| `context.Canceled`, `context.DeadlineExceeded` | everything that takes a ctx | Retry, with a longer deadline if it timed out. |
| `goomba: before commit: ...` | InstallLock, Create | Your `BeforeCommit` hook failed. The error wraps the hook's own error. |

## Recovering an incomplete prefix

`ErrIncomplete` means a goomba process died while installing or rolling
back, or a rollback could not finish, and nobody holds the prefix lock.

- **Stop what the dead installer started.** Processes it started, the
  byte-compiling interpreter or what a `BeforeCommit` hook ran, can still
  be writing into the prefix. Make sure they are gone first; see
  [transactions.md](transactions.md#processes-that-outlive-goomba).
- **From Go:** call `goomba.RemoveIncomplete(prefix)`.
  - **Safe to race.** It takes the prefix lock, checks again that the
    prefix is incomplete, and only then removes it. Several callers can try
    at once. One removes it, and the others get one of:
    - `ErrPrefixLocked`;
    - an error wrapping `fs.ErrNotExist`, once the prefix is gone;
    - `ErrNotIncomplete`, once it has been reinstalled.

    Rarely, they get an error saying the prefix was replaced while it was
    being locked.
  - **It stops at mounts.** It never follows a symlink or enters a mount
    point.
  - **It removes the directory too.** That includes a directory you created
    before the install, with its mode. Only a prefix that is itself a mount
    point is emptied and kept.
  - **A symlinked prefix is kept, emptied.** If the path you gave reached
    the prefix through a symlink, the directory is emptied and kept, so the
    symlink still leads somewhere `InstallLock` can fill.
- **From the shell:** the CLI has no remove command.
  - `goomba list -p PREFIX` warns when a prefix is incomplete, and says
    whether a goomba process is working on it right now.
  - `goomba lock --from-prefix` refuses an incomplete prefix.
  - Make sure no process is still working on it, then delete the directory.

## A retry loop

A program that provisions its own environment at runtime has to handle
every state of the prefix, including other copies of itself doing the same
thing. This loop from [examples/provision](../examples/provision/main.go)
does that:

```go
// ensure returns once prefix holds a finished environment, installing lf
// there if it holds none.
func ensure(ctx context.Context, prefix string, lf *lock.Lockfile, opt goomba.InstallOptions) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := goomba.CheckComplete(prefix)
		switch {
		case err == nil:
			// Finished, by goomba, micromamba or conda: reuse it.
			return nil

		case errors.Is(err, goomba.ErrPrefixLocked):
			// Another process is installing, rolling back or removing it.
			if err := pause(ctx); err != nil {
				return err
			}

		case errors.Is(err, goomba.ErrIncomplete):
			// A killed installation left it. goomba never resumes one:
			// remove it and install again. Another caller may have got
			// there first: then the prefix is locked, gone, or already
			// reinstalled (ErrNotIncomplete), and we look again.
			err := goomba.RemoveIncomplete(prefix)
			switch {
			case errors.Is(err, goomba.ErrPrefixLocked):
				if err := pause(ctx); err != nil {
					return err
				}
			case err != nil && !errors.Is(err, goomba.ErrNotIncomplete) && !errors.Is(err, fs.ErrNotExist):
				return err // ErrMountPoint, ErrUnsafePrefix, I/O errors
			}

		case errors.Is(err, fs.ErrNotExist):
			// Missing, an empty directory, or a directory that holds no
			// environment, which InstallLock refuses with ErrPrefixNotEmpty.
			err := goomba.InstallLock(ctx, prefix, lf, opt)
			switch {
			case errors.Is(err, goomba.ErrPrefixLocked):
				// Another installer took the prefix first.
				if err := pause(ctx); err != nil {
					return err
				}
			case errors.Is(err, goomba.ErrPrefixNotEmpty):
				// Another installer's prefix, or a directory of something
				// else; only the second still looks like that now.
				if foreign(prefix) {
					return err
				}
			default:
				// nil, or the failure (failures.md says what it leaves).
				return err
			}

		default:
			// ErrUnsafePrefix, a dangling symlink (fs.ErrExist), I/O errors.
			return err
		}
	}
}

// pause waits a moment before ensure looks at the prefix again.
func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}

// foreign reports whether prefix is a directory that is not empty, holds no
// environment and is not being installed into: one InstallLock refuses.
func foreign(prefix string) bool {
	if !errors.Is(goomba.CheckComplete(prefix), fs.ErrNotExist) {
		return false
	}
	ents, err := os.ReadDir(prefix)
	return err == nil && len(ents) > 0
}
```

It reuses whatever finished environment it finds, without comparing it with
the lockfile, so each lockfile needs its own prefix.

These cases were run against the release candidate with this program:

- a fresh install;
- reuse of a finished environment;
- a pip failure inside `BeforeCommit`, which left nothing behind;
- a process killed with SIGKILL during its pip step, which left
  `ErrIncomplete` and was removed and reinstalled by the next run;
- two copies started at once on the same prefix: one installed, and the
  other waited and reused the result;
- a directory of other files at the prefix, which was refused with
  `ErrPrefixNotEmpty`.

The loop does not retry failed installs. `InstallLock` errors other than
the two races are returned, since retrying an unsolvable, tampered or
misconfigured install only repeats the failure.

## pip and other finishing work

goomba installs conda packages only. It never runs pip and never adds pip to
a request: ask for `pip` in the specs if you need it (micromamba adds it to
python by default; goomba does not).

If an environment is not usable until something else has run, such as a pip
install or a check of the interpreter, run that work in
`InstallOptions.BeforeCommit`. Then:

- **Nobody who checks can use it early.** Callers that ask `CheckComplete`
  see `ErrPrefixLocked` until your step has finished and goomba has
  committed.
- **Failure rolls everything back.** Your step's error, a cancellation or a
  panic rolls the whole environment back, so a retry starts clean. A process
  killed during your step leaves `ErrIncomplete`, like any other kill.

If you run pip after `InstallLock` returns instead:

- the environment is already finished;
- other callers can reuse it before pip has run;
- a pip failure leaves a finished-looking environment without your pip
  packages;
- `RemoveIncomplete` will not remove it (`ErrNotIncomplete`).

### What the caller handles

- **Stopping the hook.** goomba passes the hook its context and checks it
  again when the hook returns, but cannot stop a hook that ignores it: the
  prefix stays locked until the hook returns. Start pip with the context
  (`exec.CommandContext`). If you read pip's output through pipes, set a
  `WaitDelay` too.
- **Waiting for everything the hook started.** Do not return from the hook
  while a process it started may still write into the prefix: goomba cannot
  see writes that come after its rollback.
- **Killing everything pip started.** pip starts build backends and
  compilers, and killing pip does not kill them. To stop them too, run pip
  in its own process group and kill the group when the context ends. jumpboot
  does this, in `pipctx_unix.go`. If the whole program is killed, nothing
  kills them; see
  [transactions.md](transactions.md#processes-that-outlive-goomba).
- **The time limit.** The hook runs under the same context as the rest of
  `InstallLock`, so a deadline you set covers download, link and pip
  together. Leave room for pip.
- **pip's configuration.** pip reads `PIP_*` variables, pip.conf and the
  user's site directory, and Python reads `PYTHON*` variables. goomba
  controls none of that. The example runs `python -I -m pip install
  --isolated` with `PIP_CONFIG_FILE=/dev/null` to shut them all out:
  `--isolated` alone still reads the system's and the environment's
  pip.conf.
- **Verifying pip packages.** goomba verifies every conda package against
  the lockfile but never sees pip's downloads. Use pip's own
  `--require-hashes` if they must be pinned.
- **Records.** pip records its packages in its own `.dist-info`
  directories, not in conda-meta, so `goomba list` and
  `goomba lock --from-prefix` do not show them.
- **What the hook must not do.** It must not touch
  `conda-meta/.goomba-incomplete` or remove `conda-meta`.

### What goomba still guarantees during the hook

- **Everything the hook wrote into the prefix is removed on rollback**,
  unless a process the hook left running keeps writing. If it writes faster
  than goomba removes files, the rollback fails and says so; if it writes
  later, goomba cannot see it.
- **The prefix stays private.** It is mode 0700 during the hook. Your own
  processes run as the same user and can use it.
- **The hook knows when it starts.** `EventBeforeCommit` is reported just
  before the hook runs.

## Cancellation and timeouts

- **Pass a deadline to `Lock` and `Create`.** Solving is NP-complete, and a
  hostile channel can keep the solver busy for a very long time. `Lock`
  imposes no deadline of its own.
- **Cancellation stops goomba promptly:**
  - downloads stop;
  - the byte-compiling interpreter is killed;
  - the transaction rolls back before `InstallLock` returns.
- **The CLI's limits:**
  - `--timeout` (10 minutes by default) covers everything `create` and
    `lock` do: detecting virtual packages, resolving, downloading and
    installing. That includes `create --lock`. Raise it for large
    environments on slow links.
  - SIGINT, SIGTERM and SIGHUP cancel the command, and an interrupted create
    rolls back.
  - More signals during the rollback are ignored, so a second Ctrl-C does
    not leave a half-removed prefix.

## Two installers, one prefix

Callers do not need to agree among themselves who installs:

- **The first `InstallLock` to take the prefix wins.** The others get
  `ErrPrefixLocked`, or `ErrPrefixNotEmpty` if the prefix already has files,
  and can wait for the winner with `CheckComplete`.
- **Several callers can recover the same prefix.** If they all see
  `ErrIncomplete`, they can all call `RemoveIncomplete`; one removes it.
- **A later caller may download for nothing.** Downloads happen before the
  prefix is taken, so a caller that loses the race may already have fetched
  packages. They stay in the shared package cache.
