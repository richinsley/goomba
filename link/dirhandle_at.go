//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package link

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type fdDir struct {
	fd   int
	path string // for messages and returned paths
}

// openDir opens the directory root/comps[0]/... one component at a time
// with O_NOFOLLOW, so that a component replaced by a symlink after
// resolution is detected instead of followed.
func openDir(root string, comps []string) (dirHandle, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	path := root
	for _, c := range comps {
		path = filepath.Join(path, c)
		nfd, err := unix.Openat(fd, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ENOENT) {
				return nil, fmt.Errorf("%s: %w", path, errDirChanged)
			}
			return nil, &os.PathError{Op: "openat", Path: path, Err: err}
		}
		fd = nfd
	}
	return &fdDir{fd: fd, path: path}, nil
}

func (d *fdDir) close() { unix.Close(d.fd) }

func (d *fdDir) create(leaf string) (*os.File, error) {
	name := filepath.Join(d.path, leaf)
	fd, err := unix.Openat(d.fd, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (d *fdDir) link(src, leaf string) error {
	if err := unix.Linkat(unix.AT_FDCWD, src, d.fd, leaf, 0); err != nil {
		return &os.LinkError{Op: "linkat", Old: src, New: filepath.Join(d.path, leaf), Err: err}
	}
	return nil
}

func (d *fdDir) symlink(target, leaf string) error {
	if err := unix.Symlinkat(target, d.fd, leaf); err != nil {
		return &os.LinkError{Op: "symlinkat", Old: target, New: filepath.Join(d.path, leaf), Err: err}
	}
	return nil
}

func (d *fdDir) mkdir(leaf string) (bool, error) {
	err := unix.Mkdirat(d.fd, leaf, 0o777)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EEXIST) {
		var st unix.Stat_t
		if serr := unix.Fstatat(d.fd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return false, nil
		}
	}
	return false, &os.PathError{Op: "mkdirat", Path: filepath.Join(d.path, leaf), Err: err}
}
