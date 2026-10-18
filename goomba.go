// Package goomba creates and manages conda environments in pure Go: it
// resolves match specs to lockfiles and installs lockfiles into prefixes,
// with no micromamba or conda binary involved.
//
// Lock resolves match specs against conda channels into a lockfile: it
// opens the channels' indexes with package repodata (sharded when the
// channel publishes a sharded index, with the python-minor prefilter when
// the specs pin python), solves with package solve under strict channel
// priority and records the result with package lock. Create is Lock
// followed by InstallLock: the equivalent of `micromamba create -p PREFIX
// -c CHANNEL SPEC...`. The index cache (LockOptions.RepodataDir) must be
// private to the user, like the package cache, because it supplies the
// sha256 every archive is verified against. Lockfiles hold package URLs of
// the form <channel>/<subdir>/<file> only: a channel whose index points
// packages elsewhere with a base_url that does not end in /<subdir>/
// cannot be locked (Lock fails with lock.ErrInvalid).
//
// InstallLock turns a verified lockfile (package lock) into a working
// environment with no resolver and no network beyond fetching the locked
// archives. Every archive is checked against the lockfile's sha256 before
// it is extracted (package fetch), every package is linked with package
// link, and the result matches what `micromamba create -f <explicit file>`
// builds from the same packages, file for file, apart from the install path
// and these deliberate differences:
//
//   - no file or directory gives group or others write permission:
//     package files that archives ship 0664 are 0644 (the package cache
//     extracts them so, see package archive), entry point scripts are 0755
//     (micromamba: 0775), and directories lose g/o write whatever the
//     umask;
//   - conda-meta records carry the real sha256_in_prefix of files whose
//     prefix was rewritten and of symlinks to them (micromamba 2.2.0 writes
//     one stale digest), and the full repodata fields of the lockfile, with
//     timestamp, license and track_features from each package's
//     info/index.json (micromamba 2.2.0's explicit install leaves
//     build_number, depends, constrains, license, timestamp and sha256
//     empty), so they equal the records micromamba writes after a solve,
//     except that depends and constrains keep the index's text and order
//     (micromamba 2.2.0 records constrains as libsolv renders them,
//     "libgcc-ng ==15.2.0=*_18" as "libgcc-ng ==15.2.0 *_18", and lists
//     both in the order of its libsolv cache once it has one);
//   - requested_spec and the history's "# update specs" come from the
//     lockfile's specs, as after a solve, rather than one exact spec per
//     package, and the history lists packages in goomba's link order;
//   - two packages installing the same path are an error (micromamba warns
//     and overwrites), and so are dependencies on virtual packages the host
//     lacks (micromamba does not check them for explicit installs).
//
// The package keeps no global state and reads no environment variables or
// configuration files: everything comes from LockOptions and
// InstallOptions. The only
// process it starts is the new environment's Python interpreter, to
// byte-compile noarch: python packages as libmamba does (see InstallLock),
// except that it runs the interpreter isolated (-I -S, so no package code
// such as a .pth file runs), directly rather than through a shell script
// that activates the prefix, from a private directory inside conda-meta
// rather than from the prefix, with the script kept there rather than in
// the shared temporary directory, which would come first on sys.path.
//
// Symlink digests (sha256_in_prefix) are taken once every package is
// linked, so they do not depend on link timing; a symlink into a package
// linked after its own (a broken dependency cycle) records its target's
// digest where micromamba records the empty digest.
//
// # Trust and residual risks
//
// Package files are linked from the package cache by path after the cache
// verified and extracted them (link re-reads the tree by path). This is
// sound only because nobody but the user can change the cache: package
// fetch resolves the cache directory once and refuses, on Unix, one that
// another user could write or whose directories above could be swapped,
// and extracts files without group or other write; InstallLock resolves the
// prefix once, refuses one that another user could write or swap (the same
// rule for every directory on the way to it, and a symlink on the way is
// followed only from a directory nobody else can change, see package
// internal/safedir; the repodata index cache follows the same rules),
// takes group and other write off
// every directory and conda-meta file it makes, and checks the interpreter
// it runs, following a symlinked interpreter to the package file it is.
// What remains:
//
//   - Files in an environment are hard links into the cache, as with
//     micromamba: editing one in place (rather than replacing it) changes
//     the cached copy and so every environment installed from it later;
//     the cache does not re-verify file contents at link time.
//   - A user's own processes can change the cache or the prefix while
//     InstallLock runs; the interpreter is checked just before it starts,
//     not atomically with starting it.
//   - Ownership is not checked on Windows; there two installations that
//     start into the same empty prefix at the same moment can both pass
//     the emptiness check, and one then fails when it cannot create
//     conda-meta. The rollback there removes by path.
//   - A group-writable directory of the user's private group (gid equal to
//     the uid and the effective gid) counts as the user's; an administrator
//     who added another account to that group would let it in.
package goomba
