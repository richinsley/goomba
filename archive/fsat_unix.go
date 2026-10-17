//go:build darwin || freebsd || linux || netbsd || openbsd

package archive

import (
	"fmt"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// haveAt reports that every filesystem operation of an extraction runs
// relative to an open directory handle, so a path swapped during the
// extraction cannot redirect it, and no operation walks from the root.
const haveAt = true

// maxDirHandles bounds the directory handles an extraction keeps open.
const maxDirHandles = 64

// atFS implements treeFS with directory handles: each directory level is
// opened with O_NOFOLLOW|O_DIRECTORY relative to its parent's handle, so
// every operation costs one system call on a cached handle.
type atFS struct {
	root  *os.File // the temporary directory, owned by the caller
	cache map[string]*os.File
}

func newAtFS(root *os.File) treeFS {
	return &atFS{root: root, cache: make(map[string]*os.File)}
}

// dir returns a handle on directory rel. Callers use it before the next
// call to dir, which may close it.
func (a *atFS) dir(rel string) (*os.File, error) {
	if rel == "" {
		return a.root, nil
	}
	if f, ok := a.cache[rel]; ok {
		return f, nil
	}
	parent, base := splitRel(rel)
	pf, err := a.dir(parent)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(pf.Fd()), base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: rel, Err: err}
	}
	if len(a.cache) >= maxDirHandles {
		for k, f := range a.cache { // any entry; only performance depends on which
			f.Close()
			delete(a.cache, k)
			break
		}
	}
	f := os.NewFile(uintptr(fd), rel)
	a.cache[rel] = f
	return f, nil
}

// at returns a handle on rel's parent and rel's base name.
func (a *atFS) at(rel string) (int, string, error) {
	parent, base := splitRel(rel)
	d, err := a.dir(parent)
	if err != nil {
		return -1, "", err
	}
	return int(d.Fd()), base, nil
}

func (a *atFS) mkdir(rel string) error {
	d, base, err := a.at(rel)
	if err != nil {
		return err
	}
	if err := unix.Mkdirat(d, base, 0o755); err != nil {
		return &os.PathError{Op: "mkdirat", Path: rel, Err: err}
	}
	return nil
}

func (a *atFS) create(rel string) (*os.File, error) {
	d, base, err := a.at(rel)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(d, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: rel, Err: err}
	}
	return os.NewFile(uintptr(fd), rel), nil
}

func (a *atFS) remove(rel string) error {
	d, base, err := a.at(rel)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(d, base, 0); err != nil {
		return &os.PathError{Op: "unlinkat", Path: rel, Err: err}
	}
	return nil
}

func (a *atFS) symlink(rel, target string) error {
	d, base, err := a.at(rel)
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(target, d, base); err != nil {
		return &os.LinkError{Op: "symlinkat", Old: target, New: rel, Err: err}
	}
	return nil
}

func (a *atFS) link(src, dst string) error {
	sd, sbase, err := a.at(src)
	if err != nil {
		return err
	}
	// The next dir call may close sd's handle; keep a private copy.
	sfd, err := unix.Dup(sd)
	if err != nil {
		return err
	}
	defer unix.Close(sfd)
	dd, dbase, err := a.at(dst)
	if err != nil {
		return err
	}
	if err := unix.Linkat(sfd, sbase, dd, dbase, 0); err != nil {
		return &os.LinkError{Op: "linkat", Old: src, New: dst, Err: err}
	}
	return nil
}

func (a *atFS) setMtime(rel string, t time.Time) error {
	d, base, err := a.at(rel)
	if err != nil {
		return err
	}
	ts, err := unix.TimeToTimespec(t)
	if err != nil {
		return err
	}
	return unix.UtimesNanoAt(d, base, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}

func (a *atFS) readlink(rel string) (string, error) {
	d, base, err := a.at(rel)
	if err != nil {
		return "", err
	}
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(d, base, buf)
		if err != nil {
			return "", &os.PathError{Op: "readlinkat", Path: rel, Err: err}
		}
		if n < size {
			return string(buf[:n]), nil
		}
		if size >= 1<<20 {
			return "", &os.PathError{Op: "readlinkat", Path: rel, Err: unix.ENAMETOOLONG}
		}
	}
}

func (a *atFS) kindOf(rel string) (kind, error) {
	d, base, err := a.at(rel)
	if err != nil {
		if isMissing(err) {
			return kindNone, nil
		}
		return kindNone, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(d, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if isMissing(err) {
			return kindNone, nil
		}
		return kindNone, &os.PathError{Op: "fstatat", Path: rel, Err: err}
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		return kindDir, nil
	case unix.S_IFREG:
		return kindFile, nil
	case unix.S_IFLNK:
		return kindSymlink, nil
	}
	return kindOther, nil
}

func (a *atFS) list(rel string) ([]string, error) {
	d, err := a.dir(rel)
	if err != nil {
		return nil, err
	}
	// A fresh handle, so the cached one's directory offset is untouched.
	fd, err := unix.Openat(int(d.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: rel, Err: err}
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	return f.Readdirnames(-1)
}

func (a *atFS) chmodRoot(perm fs.FileMode) error { return a.root.Chmod(perm) }

func (a *atFS) close() {
	for k, f := range a.cache {
		f.Close()
		delete(a.cache, k)
	}
}

// openDirNoFollow opens the directory p, refusing a symlink at p.
func openDirNoFollow(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: temporary directory %q: %w", ErrUnsafePath, p, err)
	}
	return f, nil
}
