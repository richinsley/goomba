//go:build windows

// Cross-process locking modelled on libmamba src/core/util.cpp (LockFile)
// (BSD-3-Clause, see NOTICE), with LockFileEx.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// openLockFile opens or creates the lock file p, refusing a symlink or
// junction at p.
func openLockFile(p string) (*os.File, error) {
	if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	return os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
}

// waitLock takes an exclusive LockFileEx lock on the first byte of f,
// polling so that waiting stops when ctx is done.
func waitLock(ctx context.Context, f *os.File, path string) error {
	h := windows.Handle(f.Fd())
	return pollLock(ctx, func() (bool, error) {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
			return false, nil
		}
		return false, &os.PathError{Op: "LockFileEx", Path: path, Err: err}
	})
}

func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
	_ = f.Close()
}
