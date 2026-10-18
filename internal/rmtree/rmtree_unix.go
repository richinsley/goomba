//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package rmtree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// MaxDepth bounds the recursion of RemoveAt; goomba creates nothing deeper
// (archive limits entries to 256 components).
const MaxDepth = 300

// maxPasses bounds how often a directory is listed again while entries
// keep appearing in it.
const maxPasses = 10

// OpenDir opens the directory name of the directory fd for reading,
// refusing a symlink (ELOOP or ENOTDIR) and a mount point (an error
// wrapping ErrMountPoint).
func OpenDir(fd int, name string) (int, error) {
	return openSubdir(fd, name)
}

// Names lists the directory fd through a fresh handle, so that fd's own
// offset does not matter.
func Names(fd int) ([]string, error) {
	rfd, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	rd := os.NewFile(uintptr(rfd), ".")
	defer rd.Close()
	names, err := rd.Readdirnames(-1)
	if err == io.EOF {
		err = nil
	}
	return names, err
}

// RemoveAt removes the entry name of the directory fd: a file, symlink or
// other non-directory directly, a directory after emptying it through a
// handle opened by OpenDir (made writable by its owner first, once it is
// known not to be a mount point). A missing entry is not an error. rel
// names the entry in errors, e.g. its path below the prefix.
func RemoveAt(fd int, name, rel string) error {
	return removeAt(fd, name, rel, 0)
}

func removeAt(fd int, name, rel string, depth int) error {
	err := unix.Unlinkat(fd, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	// Linux reports a directory as EISDIR, the BSDs and macOS as EPERM.
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		return &os.PathError{Op: "remove", Path: rel, Err: err}
	}
	if depth >= MaxDepth {
		return &os.PathError{Op: "remove", Path: rel, Err: errors.New("directory tree too deep")}
	}
	cfd, err := openSubdir(fd, name)
	if errors.Is(err, ErrMountPoint) {
		return fmt.Errorf("%s: %w", rel, err)
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: rel, Err: err}
	}
	_ = unix.Fchmod(cfd, 0o700)
	err = emptyAt(cfd, rel, depth+1)
	unix.Close(cfd)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "remove", Path: rel, Err: err}
	}
	return nil
}

// emptyAt removes everything inside the directory fd (rel in errors).
// Names are read in full before any is removed (removing entries while a
// directory is being read may skip some), and the directory is read again
// until it is empty.
func emptyAt(fd int, rel string, depth int) error {
	var first error
	for pass := 0; ; pass++ {
		names, err := Names(fd)
		if err != nil {
			return &os.PathError{Op: "read", Path: rel, Err: err}
		}
		if len(names) == 0 {
			return nil
		}
		if pass >= maxPasses {
			return &os.PathError{Op: "remove", Path: rel, Err: errors.New("the directory keeps getting new entries")}
		}
		var passErr error
		for _, n := range names {
			if err := removeAt(fd, n, rel+"/"+n, depth); err != nil && passErr == nil {
				passErr = err
			}
		}
		if passErr != nil {
			if first == nil {
				first = passErr
			}
			// A mount point stays, and so does everything above it.
			if pass >= 2 || errors.Is(passErr, ErrMountPoint) {
				return first
			}
		}
	}
}

// Empty removes everything inside the open directory fd, which may itself
// be a mount point, as RemoveAt removes each entry; rel names fd's
// directory in errors.
func Empty(fd int, rel string) error {
	return emptyAt(fd, rel, 0)
}

// RemoveAll removes path and everything below it, like os.RemoveAll, but
// without crossing a mount point at or below path. The directories leading
// to path are resolved as os.RemoveAll resolves them. A missing path is
// not an error.
func RemoveAll(path string) error {
	path = filepath.Clean(path)
	dir, base := filepath.Split(path)
	if base == "" || base == "." || base == ".." {
		return &os.PathError{Op: "remove", Path: path, Err: errors.New("not a removable name")}
	}
	if dir == "" {
		dir = "."
	}
	pfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(pfd)
	return RemoveAt(pfd, base, path)
}
