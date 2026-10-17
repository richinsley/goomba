//go:build linux

package fetch

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames src to dst, failing if dst exists.
func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return renameChecked(src, dst)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

// syncTree makes everything written below dir durable with one syncfs on
// its filesystem, far cheaper than an fsync per extracted file. syncfs
// cannot be interrupted, so it runs on its own goroutine and syncTree
// returns as soon as ctx is done; the marker is then never written.
func syncTree(ctx context.Context, dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		defer f.Close()
		done <- unix.Syncfs(int(f.Fd()))
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("syncfs %q: %w", dir, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
