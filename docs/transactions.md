# Transaction guarantees

This page says what goomba promises about an environment while it is being
created and after it is created, and where those promises stop. The
[failure and retry guide](failures.md) turns this into what a caller does
when something goes wrong.

## What a transaction is

A transaction is one call to `InstallLock`, which turns a lockfile into a
new environment in one prefix. `Create` resolves first and then makes the
same `InstallLock` call; `goomba create` does the same. Resolving reads and
fills the index cache but never touches the prefix.

goomba only creates environments. It does not install into, update or remove
packages from an existing environment, so every transaction starts from a
prefix that is missing or an empty directory.

## The guarantees

These hold on Linux when the goomba process fails or stops, whether it
returns an error, is cancelled, panics, gets SIGKILL or is OOM-killed,
while the machine keeps running. macOS runs the same code but has not been
tested.

### A prefix reads as finished only when it is

`CheckComplete` reports one of these:

| What `CheckComplete` reports | Meaning |
| --- | --- |
| `nil` | A finished environment. |
| `ErrPrefixLocked` | A goomba process is installing it, rolling it back or removing it right now. |
| `ErrIncomplete` | An installation was killed, or its rollback could not finish. The prefix is unusable until it is removed. |
| an error wrapping `fs.ErrNotExist` | No environment: missing, an empty directory, or a directory without conda-meta or a history. |
| another error | The path cannot hold an environment: `ErrUnsafePrefix`, a dangling symlink (`fs.ErrExist`), a file where a directory belongs, or an I/O error. |

A half-installed environment never reads as finished. The mechanism is a
marker file, `conda-meta/.goomba-incomplete`:

- **Written first.** The marker is created before any package file.
- **Removed last.** Its removal is the commit, and nothing changes after it.
- **Rollback keeps it to the end.** A rollback empties the prefix but keeps
  the marker until nothing else is left, so a goomba killed while rolling
  back still leaves `ErrIncomplete`. goomba itself never leaves files
  without the marker; processes it or a hook started can (see below).

### Failures roll back

On an error, a cancellation or a panic, `InstallLock` removes everything it
wrote:

- a prefix it created is removed;
- a prefix that was an existing empty directory is emptied and gets its old
  mode back.

The package cache is kept, so a retry does not download again.

Rollback empties the prefix through the directory handle it locked, not by
path, so a directory renamed or replaced by a symlink meanwhile cannot
redirect the removal. It never follows a symlink and never enters a mount
point inside the prefix.

A rollback can fail. Something mounted inside the prefix stops it, and so
do files that keep appearing faster than it removes them, from a process a
`BeforeCommit` hook started and left running. The prefix then keeps its
marker, exactly as if the process had been killed. `InstallLock` returns
its original error joined with the rollback's error (`ErrMountPoint` for a
mount point), so `errors.Is` finds both. When the hook or a `Progress`
callback panicked, the panic goes on instead and a failed rollback is not
reported; `CheckComplete` still tells.

goomba cannot see writes that come after its rollback has finished. A
process that outlives a failed hook and writes more slowly can leave files
in the prefix with no marker and no conda-meta. The prefix then reads as a
directory of other files: `InstallLock` refuses it with `ErrPrefixNotEmpty`,
and `RemoveIncomplete` does not remove it. A hook must not return until
everything it started has exited.

### One installer at a time

goomba locks the prefix with `flock` from the moment it takes the prefix
until the commit or the end of the rollback.

- **Installs and removals exclude each other.** A second `InstallLock` or a
  `RemoveIncomplete` for the same prefix, in this process or another, fails
  at once with `ErrPrefixLocked` (or `ErrPrefixNotEmpty`). goomba never
  waits for another installer.
- **Checks do not block installs.** `CheckComplete` takes the lock shared and
  without waiting. An installer that meets a check holding it retries for
  about 50 ms instead of failing.
- **The lock is advisory.** It keeps out goomba callers, and every caller
  that asks `CheckComplete` before using an environment. It does not stop
  the user's own processes from running the environment's programs while it
  is being installed.

### Checks before the prefix is touched

These run before the prefix is created or locked, and a failure leaves
nothing behind:

- the lockfile is valid and for this platform;
- the machine's virtual packages (`__glibc`, `__osx`, `__cuda`, ...) satisfy
  every locked package;
- the prefix is missing or empty, and the paths to it and to the package
  cache pass the ownership checks (see
  [Other tools and other users](#other-tools-and-other-users));
- every archive is downloaded and checked against the lockfile's sha256
  (and size, when the lockfile records one) before it is extracted;
- no two packages install the same path.

Downloads happen before the prefix is taken, so a slow download never holds
the lock.

### What is checked inside the transaction

- **Symlinks.** After linking, every symlink in the prefix must resolve
  inside it.
- **The interpreter.** Before byte-compiling, goomba checks that the
  interpreter it is about to run is the file linked from the verified python
  package.
- **The caller's step.** `InstallOptions.BeforeCommit` runs last, under the
  lock and with the marker still present. Its error, a cancellation or a
  panic rolls the whole environment back. Use it for work that must finish
  before the environment counts as finished: checking the interpreter, or a
  pip install.

### Private while it is filled

The prefix directory is mode 0700 until the commit, then 0755, so other
users cannot reach a half-installed environment. An existing empty
directory keeps its own mode instead. That includes an empty 0700 directory
made by another installation: one killed just after it created the prefix,
one that created it but lost the race to lock it, or a `RemoveIncomplete`
through a symlinked path. An environment installed into such a directory
stays 0700.

## What the guarantees do not cover

### Power loss and kernel crashes

The guarantees above are about the goomba process failing, not the machine.

- **The prefix is not synced.** goomba does not fsync the files it links or
  copies into the prefix, or its directories, before it removes the marker.
- **Durability depends on the file system.** After a power cut or a kernel
  crash, an environment committed shortly before may lack data even though
  its marker is gone.
- **What is synced:** the package cache syncs each extracted package before
  marking it complete, and each conda-meta record is written with fsync and
  then renamed into place.

If the machine went down during or just after an installation, remove the
environment and create it again.

### Processes that outlive goomba

A killed goomba cannot stop the processes it started:

- **Byte-compilation.** The Python interpreter goomba runs to byte-compile
  keeps going until it has compiled every file it was given. On a large
  environment that takes seconds.
- **Hook processes.** Whatever a `BeforeCommit` hook started (pip and the
  build tools pip starts) keeps running too.

These processes write into the prefix by path. While they run,
`RemoveIncomplete` may fail because new files keep appearing. If it
succeeds anyway, they can write into the next installation in the same
place. Before recovering a prefix a killed goomba left, make sure they are
gone, for example by running goomba under a supervisor or in a cgroup that
is cleaned up as a whole.

### Anything outside the prefix

- **The package cache** (`PkgsDir`) is shared, kept and never rolled back.
  Its entries are content-addressed and locked separately.
- **The index cache** (`RepodataDir`) is not part of the transaction.
- **Side effects of `BeforeCommit` are the caller's.** goomba removes what
  the hook wrote inside the prefix. It does not remove files written
  elsewhere, undo network effects, or stop processes the hook started.

### What a finished environment holds

- **Contents are not compared.** `CheckComplete` says that a prefix holds a
  finished environment, not which packages. It does not compare them with
  any lockfile. A program that ships a new lockfile needs a new prefix, for
  example one named after the lockfile's hash.
- **Files are hard links into the package cache**, as with micromamba.
  Editing one in place, rather than replacing it, also changes the cached
  copy and every environment installed from it later.
- **Changes by other tools are not detected.** pip, conda or micromamba can
  change a finished environment, and goomba will not notice.

### Other tools and other users

- **conda and micromamba locks are not consulted.** `CheckComplete` reports
  an environment made by micromamba or conda as finished if it has a
  `conda-meta/history` and no goomba marker.
- **The user's own processes are trusted.** They can change the cache or the
  prefix while a transaction runs. The interpreter check runs just before
  the interpreter starts, not atomically with starting it.
- **Ownership rules (on Unix).** goomba refuses a prefix, with
  `ErrUnsafePrefix`, unless:
  - the prefix directory belongs to the current user and neither group nor
    others can write it;
  - every directory above it belongs to root or the current user and is not
    writable by group or others, unless it is sticky, as `/tmp` is;
  - symlinks on the way are followed only from directories nobody else can
    change.

  A group-writable directory of the user's own private group (its gid equals
  the uid and the effective gid) counts as the user's. The prefix's contents
  are not checked. An environment built by root and used by another account
  is refused.

### Moments the guarantees do not reach

- **A check is a snapshot.** A `CheckComplete` answer can be outdated as soon
  as it is returned.
- **A new install can briefly look like no environment.** An installation
  that has just taken an empty directory, but not yet created conda-meta, is
  reported as not an environment. Two callers that both decide to create the
  prefix are still kept apart by `InstallLock`.

### Mount points inside the prefix

If something is mounted inside the prefix, rollback and `RemoveIncomplete`
stop at it with an error that wraps `ErrMountPoint` and names it. Nothing
below the mount point is removed or changed. The prefix keeps its marker
until the mount is gone and `RemoveIncomplete` is run again.

On Linux 5.6 and later every mount point is detected, bind mounts of the
same file system included (where a seccomp filter refuses `openat2`, this
needs Linux 5.8). On older kernels, such as RHEL 8's 4.18, and on other Unix
systems goomba compares device numbers, so a bind mount of the same file
system is not detected there.

### Windows

Windows is not supported: packages with Python entry points cannot be
installed there, since goomba has no `.exe` launcher. The transaction code
builds, but without `flock`:

- there is no prefix lock;
- a prefix with the marker reads as `ErrIncomplete` even while it is being
  installed;
- `RemoveIncomplete` returns an error wrapping `errors.ErrUnsupported`;
- ownership is not checked.
