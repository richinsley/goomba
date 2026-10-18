// Package rmtree removes directory trees that goomba made (environment
// prefixes being rolled back or removed, stale entries of the package
// cache) without ever following a symlink and without ever crossing a
// mount point below the tree: a volume or bind mount placed inside a
// prefix, as a container runtime may do when it restarts a container whose
// goomba was killed, is neither entered, nor emptied, nor chmod-ed. The
// removal stops there with an error wrapping ErrMountPoint that names the
// entry.
//
// On Linux every directory is opened with openat2(RESOLVE_NO_XDEV |
// RESOLVE_NO_SYMLINKS), which refuses any mount crossing, bind mounts of
// the same file system included. Where openat2 is missing (Linux before
// 5.6, or a seccomp filter that refuses it) the directory is opened with
// openat(O_NOFOLLOW) and its mount ID (statx STATX_MNT_ID, Linux 5.8) is
// compared with its parent's, or else its device number. On the other Unix
// systems the device numbers are compared, so a bind mount of the same file
// system is not detected there. Every check happens before anything inside
// the directory is changed.
//
// On platforms without openat (Windows and others) RemoveAll is
// os.RemoveAll: on Windows mount points and junctions are reparse points,
// which it removes without descending into them; elsewhere a mount point
// below the tree is not detected.
//
// The functions keep no state and are safe for concurrent use.
package rmtree

import "errors"

// ErrMountPoint is wrapped by the error for an entry that is a mount point
// (or, on file systems where only device numbers can be compared, on
// another device): nothing below it was changed.
var ErrMountPoint = errors.New("a mount point: nothing below it is removed")
