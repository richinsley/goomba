//go:build unix

package safedir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// noUnixPerms explains an ownership mismatch that a filesystem without
// Unix ownership produces.
const noUnixPerms = "the directory must be on a filesystem with Unix ownership and permissions (FAT, exFAT, SMB/CIFS and similar mounts cannot protect it)"

// maxLinks bounds the symlinks followed while resolving one path, as the
// Linux kernel does (MAXSYMLINKS).
const maxLinks = 40

// maxSteps bounds the components a resolution looks at, links included,
// so that directories removed as fast as they are created cannot keep it
// going.
const maxSteps = 4096

// sysOps are the file system calls a resolution makes; tests replace them
// (to present a symlink as another user's, say).
type sysOps struct {
	lstat    func(string) (fs.FileInfo, error)
	readlink func(string) (string, error)
	mkdir    func(string, fs.FileMode) error
}

func osOps() sysOps { return sysOps{lstat: os.Lstat, readlink: os.Readlink, mkdir: os.Mkdir} }

// resolve resolves p; with create, it first resolves p without creating
// anything, so that a path refused anywhere leaves nothing behind, then
// again creating the missing directories and checking what it finds, in
// case the file system changed in between.
func resolve(p string, create bool) (string, error) {
	ops := osOps()
	if create {
		if _, err := resolveWith(p, false, ops); err != nil {
			return "", err
		}
	}
	return resolveWith(p, create, ops)
}

// component is one component of a path being resolved; fromLink marks one
// that came from a symlink's target.
type component struct {
	name     string
	fromLink bool
}

// resolveWith resolves p component by component as the kernel does (see
// the package documentation): every directory passed through must pass
// checkTraversed, and a symlink is followed only as checkSymlink allows.
// A trailing slash requires the final component to be a directory. With
// create, missing directories are made. Without, the components from the
// first missing one on are appended as written, if that tail holds no "."
// or ".." (ENOENT, as for the kernel, which cannot look them up in a
// missing directory), and if the missing component is not part of a
// symlink's target (EEXIST for a dangling final symlink, as mkdir(2)
// reports; ENOENT otherwise).
func resolveWith(p string, create bool, ops sysOps) (string, error) {
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + string(filepath.Separator) + p // not cleaned: "link/.." follows link first
	}
	todo := splitPath(p, false)
	cur := "/"
	rootInfo, err := ops.lstat(cur)
	if err != nil {
		return "", err
	}
	if err := checkTraversed(cur, rootInfo, p); err != nil {
		return "", err
	}
	curInfo := rootInfo
	links := 0
	for steps := 0; len(todo) > 0; steps++ {
		if steps > maxSteps {
			return "", &os.PathError{Op: "resolve", Path: p, Err: syscall.ELOOP}
		}
		c := todo[0]
		todo = todo[1:]
		switch c.name {
		case "", ".":
			// "" is a trailing slash: cur, a directory, is what it needs.
			continue
		case "..":
			// cur holds no symlinks, so its parent is its lexical parent,
			// which was checked on the way down.
			if up := filepath.Dir(cur); up != cur {
				cur = up
				if curInfo, err = ops.lstat(cur); err != nil {
					return "", err
				}
			}
			continue
		}
		next := filepath.Join(cur, c.name)
		fi, err := ops.lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			if !create {
				return missingTail(p, next, c, todo)
			}
			if err := ops.mkdir(next, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", err
			}
			todo = append([]component{c}, todo...) // look at what is there now
			continue
		}
		if err != nil {
			return "", err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			if links++; links > maxLinks {
				return "", &os.PathError{Op: "resolve", Path: p, Err: syscall.ELOOP}
			}
			if err := checkSymlink(next, fi, cur, curInfo, p); err != nil {
				return "", err
			}
			target, err := ops.readlink(next)
			if err != nil {
				return "", err
			}
			if target == "" {
				return "", &os.PathError{Op: "resolve", Path: next, Err: syscall.ENOENT}
			}
			if filepath.IsAbs(target) {
				cur, curInfo = "/", rootInfo
			}
			todo = append(splitPath(target, true), todo...)
		case fi.IsDir():
			if err := checkTraversed(next, fi, p); err != nil {
				return "", err
			}
			cur, curInfo = next, fi
		default:
			if len(todo) == 0 {
				return next, nil // the final component: the caller decides
			}
			// Followed by more, if only by a trailing slash.
			return "", &os.PathError{Op: "resolve", Path: next, Err: syscall.ENOTDIR}
		}
	}
	return cur, nil
}

// missingTail is the result of a non-creating resolution of p whose
// component c (at next) is missing, with todo after it.
func missingTail(p, next string, c component, todo []component) (string, error) {
	rest := make([]string, 0, len(todo))
	fromLink := c.fromLink
	original := !c.fromLink // a component of p itself comes after
	for _, t := range todo {
		switch t.name {
		case "":
			continue
		case ".", "..":
			return "", &os.PathError{Op: "resolve", Path: p, Err: syscall.ENOENT}
		}
		fromLink = fromLink || t.fromLink
		original = original || !t.fromLink
		rest = append(rest, t.name)
	}
	if fromLink {
		if !original { // the final component is a dangling symlink
			return "", &os.PathError{Op: "resolve", Path: p, Err: syscall.EEXIST}
		}
		return "", &os.PathError{Op: "resolve", Path: p, Err: syscall.ENOENT}
	}
	return filepath.Join(append([]string{next}, rest...)...), nil
}

// splitPath splits a slash path into its components, dropping empty ones;
// a trailing slash (not the root's) becomes a final empty component.
func splitPath(p string, fromLink bool) []component {
	var out []component
	for _, c := range strings.Split(filepath.ToSlash(p), "/") {
		if c != "" {
			out = append(out, component{name: c, fromLink: fromLink})
		}
	}
	if len(out) > 0 && strings.HasSuffix(p, "/") {
		out = append(out, component{fromLink: fromLink})
	}
	return out
}

// checkTraversed refuses a directory on the way to target that another
// user could change: one that belongs neither to root nor to the effective
// user, or that group or others may write, unless it is sticky or a
// directory of the user's private group.
func checkTraversed(dir string, fi fs.FileInfo, target string) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return unsafef(dir, "%s: no owner information", dir)
	}
	euid := os.Geteuid()
	if st.Uid != 0 && int(st.Uid) != euid {
		return unsafef(dir, "%s, on the way to %s, belongs to uid %d, neither root nor the current user (uid %d)", dir, target, st.Uid, euid)
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 && !privateGroupDir(fi) {
		return unsafef(dir, "%s, on the way to %s, is writable by group or others (mode %v) without the sticky bit", dir, target, fi.Mode().Perm())
	}
	return nil
}

// checkSymlink refuses to follow the symlink link (fi) in dir (dirInfo,
// which passed checkTraversed) when dir is sticky and writable by group or
// others and the link belongs to neither the effective user nor root:
// another user may have put it there.
func checkSymlink(link string, fi fs.FileInfo, dir string, dirInfo fs.FileInfo, target string) error {
	if dirInfo.Mode()&fs.ModeSticky == 0 || dirInfo.Mode().Perm()&0o022 == 0 {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return unsafef(link, "%s: no owner information", link)
	}
	if euid := os.Geteuid(); st.Uid != 0 && int(st.Uid) != euid {
		return unsafef(link, "%s, a symlink on the way to %s in the shared directory %s, belongs to uid %d, neither root nor the current user (uid %d)", link, target, dir, st.Uid, euid)
	}
	return nil
}

// CheckDir refuses a directory that a user other than the current one could
// change (see the package documentation): dir, already resolved (neither it
// nor a directory above it may be a symlink), must belong to the effective
// user and not be writable by group or others, and every directory above
// it must pass CheckAncestors; a directory of the user's private group may
// be group-writable. Otherwise another user could rename an entry of
// theirs into it. Refusals are *Error values; I/O errors are returned as
// they are.
func CheckDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return unsafef(dir, "%s is not a directory", dir)
	}
	if err := CheckOwnDir(dir, fi); err != nil {
		return err
	}
	return CheckAncestors(dir)
}

// CheckOwnDir refuses a directory (fi, from Lstat or the open directory's
// Stat) that does not belong to the effective user or that group or others
// may write, except a directory of the user's private group.
func CheckOwnDir(dir string, fi fs.FileInfo) error {
	if err := CheckEntry(dir, fi); err != nil && !privateGroupDir(fi) {
		return err
	}
	return nil
}

// CheckAncestors refuses p when a directory above it (all of them, up to
// the root; a missing one is skipped) is not a directory, belongs neither
// to root nor to the effective user, or may be written by group or others
// without the sticky bit (a private-group directory may be
// group-writable). p must be resolved: a symlink above it is an error.
func CheckAncestors(p string) error {
	euid := os.Geteuid()
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		fi, err := os.Lstat(d)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case !fi.IsDir():
			return unsafef(d, "%s, above %s, is not a directory (the path changed)", d, p)
		default:
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return unsafef(d, "%s: no owner information", d)
			}
			if st.Uid != 0 && int(st.Uid) != euid {
				return unsafef(d, "%s, above %s, belongs to uid %d, neither root nor the current user (uid %d)", d, p, st.Uid, euid)
			}
			if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 && !privateGroupDir(fi) {
				return unsafef(d, "%s, above %s, is writable by group or others (mode %v) without the sticky bit", d, p, fi.Mode().Perm())
			}
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

// CheckEntry refuses an entry of a checked directory, described by fi (from
// Lstat, or Stat of the open file), that does not belong to the effective
// user or that group or others may write.
func CheckEntry(p string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return unsafef(p, "%s: no owner information", p)
	}
	if euid := os.Geteuid(); int(st.Uid) != euid {
		return unsafef(p, "%s belongs to uid %d, not the current user (uid %d); %s", p, st.Uid, euid, noUnixPerms)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return unsafef(p, "%s is writable by group or others (mode %v)", p, fi.Mode().Perm())
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

// errNotDir is the error of a resolved directory that is not one.
var errNotDir = syscall.ENOTDIR
