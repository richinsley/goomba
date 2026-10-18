// Package safedir decides whether a directory can be changed only by the
// current user, in the manner of OpenSSH's StrictModes. goomba's caches use
// it: the package cache (package fetch) and the index cache (package
// repodata) supply the bytes and the digests every installation trusts, so
// a cache that another user could write would defeat verification. The
// installer uses it for environment prefixes too.
//
// A directory is resolved once (Resolve, ResolvePath), one component at a
// time, and callers use only the resolved path from then on, so a symlink
// on the way to it that someone re-points later changes nothing. On Unix
// every directory the resolution passes through, before and after any
// symlink, must belong to root or the effective user and not be writable
// by group or others unless its sticky bit is set (as on /tmp); so a
// symlink is followed only from a directory nobody else can change. In a
// sticky directory that group or others may write, a symlink is followed
// only if it belongs to the effective user or root (the rule of the Linux
// kernel's protected_symlinks). At most 40 symlinks are followed. The
// resolution otherwise follows the kernel's: ".." after a symlink leaves
// the symlink's target, a trailing slash requires a directory, and "." or
// ".." after a missing component is ENOENT. A differential test checks it
// against the Linux kernel's own resolution.
//
// On Unix a directory passes CheckDir when it belongs to the effective user
// and is not writable by group or others, and every directory above it, up
// to the root, belongs to root or the effective user and is not writable by
// group or others unless its sticky bit is set. One relaxation applies to
// the directory and to every directory on the way to it: a directory that
// belongs to the effective user and to the user's private group (a group
// whose id equals the user's id and the process's effective group id) may
// be group-writable, as directories are under the umask 002 that
// user-private-group systems use. Whether another account also has that
// group as its primary group, or as a supplementary one, would need the
// password and group databases, which goomba does not read (no cgo, no
// files under /etc); on a system where a user's private group is shared,
// keep such directories mode g-w. Entries inside a directory (CheckEntry)
// must belong to the effective user and not be writable by group or others,
// with no exception. ACLs are not examined.
//
// Nothing here removes anything: a non-directory where a directory is
// expected is an error, never replaced.
//
// On other platforms ownership and modes are not examined (on Windows, a
// directory under the user's profile is protected by its default ACLs; one
// elsewhere must be protected by the caller); paths are resolved with
// filepath.EvalSymlinks.
//
// The functions keep no state and are safe for concurrent use.
package safedir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrUnsafe is wrapped by every refusal of this package: a path that a user
// other than the current one could change. The error is an *Error.
var ErrUnsafe = errors.New("not private to the current user")

// Error reports a path that another user could change, or that is not what
// it must be (a directory replaced by a file). It unwraps to ErrUnsafe. An
// Error is immutable once returned.
type Error struct {
	// Path is the offending path: the directory, one above it, or an entry.
	Path string
	msg  string
}

// Error describes the path and what is wrong with it.
func (e *Error) Error() string { return e.msg }

// Unwrap returns ErrUnsafe.
func (e *Error) Unwrap() error { return ErrUnsafe }

func unsafef(p, format string, args ...any) error {
	return &Error{Path: p, msg: fmt.Sprintf(format, args...)}
}

// Resolve resolves dir (made absolute against the working directory) one
// component at a time as the package documentation describes, creating
// missing directories on the way (mode 0755, less the umask) inside
// directories that passed the checks, and checks the result with CheckDir.
// The whole path is checked as ResolvePath checks it before anything is
// created, so a path refused anywhere leaves nothing behind.
// It returns the resolved path, which is the only one the caller may use
// afterwards. An empty dir is an error. Refusals are *Error values; a
// component that is not a directory is an I/O error (ENOTDIR).
func Resolve(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("safedir: empty directory name")
	}
	real, err := resolve(dir, true)
	if err != nil {
		return "", err
	}
	if fi, err := os.Lstat(real); err != nil {
		return "", err
	} else if !fi.IsDir() {
		return "", &fs.PathError{Op: "resolve", Path: real, Err: errNotDir}
	}
	if err := CheckDir(real); err != nil {
		return "", err
	}
	return real, nil
}

// ResolvePath resolves p like Resolve but creates nothing: the components
// from the first missing one on are appended to the resolved part as
// written, provided they are plain names (a "." or ".." among them is
// ENOENT, as the kernel cannot look them up in a missing directory) and
// the missing component is not part of a symlink's target (a dangling
// final symlink is EEXIST, as mkdir(2) reports; a missing directory inside
// a symlink's target is ENOENT). The final component may be anything that
// is not a symlink (a symlink is followed); ResolvePath does not check it,
// callers check what they find or create there.
func ResolvePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("safedir: empty path")
	}
	return resolve(p, false)
}

// EnsureDir makes p, an entry of a checked directory, a directory: an
// existing directory must pass CheckEntry, a missing one is created (mode
// 0755, less the umask), and anything else there (a file, a symlink) is an
// *Error, never removed: goomba deletes nothing it did not create.
func EnsureDir(p string) error {
	fi, err := os.Lstat(p)
	switch {
	case err == nil && fi.IsDir():
		return CheckEntry(p, fi)
	case err == nil:
		return unsafef(p, "%s exists and is not a directory (it is left alone)", p)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) { // made concurrently: check it
			if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
				return CheckEntry(p, fi)
			}
			return unsafef(p, "%s exists and is not a directory (it is left alone)", p)
		}
		return err
	}
	return nil
}
