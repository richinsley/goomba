//go:build unix

package fetch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// checkCacheDir refuses a cache directory that a user other than the
// current one could change, in the manner of OpenSSH's StrictModes: dir
// (already resolved: neither it nor a directory above it may be a symlink)
// must belong to the effective user and not be writable
// by group or others, and every directory above it, up to the root, must
// belong to root or the effective user and not be writable by group or
// others unless its sticky bit is set (as on /tmp). Otherwise another user
// could rename a content directory or extracted tree of theirs into the
// cache, holding a copy of a marker it can read, and have unverified bytes
// installed. ACLs are not examined.
//
// One relaxation applies to dir and its ancestors: a directory that belongs
// to the effective user and to the user's private group (a group whose id
// equals the user's id and the process's effective group id) may be
// group-writable, as directories are under the umask 002 that
// user-private-group systems use. Whether another account also has that
// group as its primary group, or as a supplementary one, would need the
// password and group databases, which this package does not read (no cgo,
// no files under /etc); on a system where a user's private group is shared,
// keep such directories mode g-w.
func checkCacheDir(dir string) error {
	real := dir
	fi, err := os.Lstat(real)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafeCache, real)
	}
	if err := checkPrivate(real, fi); err != nil && !privateGroupDir(fi) {
		return err
	}
	euid := os.Geteuid()
	for p := filepath.Dir(real); ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s, above the package cache, is not a directory (the cache path changed)", ErrUnsafeCache, p)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%w: %s: no owner information", ErrUnsafeCache, p)
		}
		if st.Uid != 0 && int(st.Uid) != euid {
			return fmt.Errorf("%w: %s, above the package cache, belongs to uid %d, neither root nor the current user (uid %d)", ErrUnsafeCache, p, st.Uid, euid)
		}
		if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 && !privateGroupDir(fi) {
			return fmt.Errorf("%w: %s, above the package cache, is writable by group or others (mode %v) without the sticky bit", ErrUnsafeCache, p, fi.Mode().Perm())
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

// checkPrivate refuses a cache entry (described by fi, from Lstat) that does
// not belong to the effective user or that group or others may write.
func checkPrivate(p string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: %s: no owner information", ErrUnsafeCache, p)
	}
	if euid := os.Geteuid(); int(st.Uid) != euid {
		return fmt.Errorf("%w: %s belongs to uid %d, not the current user (uid %d); %s", ErrUnsafeCache, p, st.Uid, euid, noUnixPerms)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is writable by group or others (mode %v)", ErrUnsafeCache, p, fi.Mode().Perm())
	}
	return nil
}

// privateGroupDir reports whether fi is a directory that belongs to the
// effective user and to that user's private group (gid equal to the uid
// and to the effective gid) and is writable by that group at most: a
// directory made under umask 002 on a user-private-group system.
func privateGroupDir(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || fi.Mode().Perm()&0o002 != 0 {
		return false
	}
	euid, egid := os.Geteuid(), os.Getegid()
	return int(st.Uid) == euid && int(st.Gid) == egid && egid == euid
}
