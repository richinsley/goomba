//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package goomba

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// lockDir opens the directory dir, refusing a symlink there, and takes an
// exclusive, non-blocking flock on it. The lock belongs to the open file, so it conflicts with any
// other open of the directory that locks it, in this process or another,
// and it leaves nothing behind in the prefix.
func lockDir(dir string) (*os.File, error) {
	// O_NOFOLLOW: dir is resolved; a symlink there now was planted since
	// and must not be followed (its target's parents were never checked).
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrPrefixLocked
		}
		return nil, fmt.Errorf("lock: %w", &os.PathError{Op: "flock", Path: dir, Err: err})
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// unlockDir releases the lock and closes f.
func unlockDir(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}

// emptyDir removes everything inside the open directory d, working relative
// to its handle (openat, unlinkat) and never following a symlink, so that a
// directory renamed or replaced meanwhile, above or inside the prefix,
// cannot redirect the removal elsewhere. Read-only directories are made
// writable first. It returns the first error; it removes what it can.
func emptyDir(d *os.File) error {
	return emptyDirFd(int(d.Fd()), 0)
}

// maxRemoveDepth bounds the recursion of emptyDirFd; link creates nothing
// deeper (archive limits entries to 256 components).
const maxRemoveDepth = 300

func emptyDirFd(fd, depth int) error {
	if depth > maxRemoveDepth {
		return errors.New("rollback: directory tree too deep")
	}
	// Names are read in full before any is removed (removing entries while
	// a directory is being read may skip some), and the directory is read
	// again until it is empty or a pass removes nothing.
	var first error
	for pass := 0; ; pass++ {
		names, err := readNamesAt(fd)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return nil
		}
		if pass >= 10 {
			return errors.New("rollback: the directory keeps getting new entries")
		}
		var passErr error
		for _, n := range names {
			if err := removeAt(fd, n, depth); err != nil && passErr == nil {
				passErr = err
			}
		}
		if passErr != nil {
			if first == nil {
				first = passErr
			}
			if pass >= 2 {
				return first
			}
		}
	}
}

// readNamesAt lists the directory fd through a fresh handle, so that fd's
// own offset does not matter.
func readNamesAt(fd int) ([]string, error) {
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

// removeAt removes the entry name of the directory fd: a file or symlink
// directly, a directory after emptying it through a handle opened without
// following symlinks.
func removeAt(fd int, name string, depth int) error {
	err := unix.Unlinkat(fd, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		return err
	}
	cfd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	_ = unix.Fchmod(cfd, 0o700)
	err = emptyDirFd(cfd, depth+1)
	unix.Close(cfd)
	if rerr := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err == nil {
		err = rerr
	}
	return err
}

// setDirMode sets the mode of the open directory d (fchmod), whatever the
// umask.
func setDirMode(d *os.File, mode fs.FileMode) error { return d.Chmod(mode) }
