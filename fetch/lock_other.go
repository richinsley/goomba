//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris || windows)

package fetch

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

func openLockFile(p string) (*os.File, error) { return os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644) }

// waitLock fails closed where no cross-process file lock is implemented.
func waitLock(context.Context, *os.File, string) error {
	return fmt.Errorf("file locking is not supported on %s", runtime.GOOS)
}

func unlockFile(f *os.File) { _ = f.Close() }
