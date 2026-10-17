//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

// Cross-process locking modelled on libmamba src/core/util.cpp (LockFile)
// (BSD-3-Clause, see NOTICE), with flock instead of fcntl locks.

package fetch

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// openLockFile opens or creates the lock file p, refusing a symlink.
func openLockFile(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o644)
}

// waitLock takes an exclusive flock on f, polling so that waiting stops
// when ctx is done. flock locks belong to the open file, so separate opens
// conflict even within one process.
func waitLock(ctx context.Context, f *os.File, path string) error {
	fd := int(f.Fd())
	return pollLock(ctx, func() (bool, error) {
		for {
			err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
			switch {
			case err == nil:
				return true, nil
			case errors.Is(err, unix.EINTR):
				continue
			case errors.Is(err, unix.EWOULDBLOCK):
				return false, nil
			}
			return false, &os.PathError{Op: "flock", Path: path, Err: err}
		}
	})
}

func unlockFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}
