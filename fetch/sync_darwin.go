//go:build darwin

package fetch

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// syncTree makes everything written below dir durable. os.File.Sync on
// macOS issues F_FULLFSYNC, a drive cache flush, for every call; instead
// each file and directory gets a plain fsync and the drive cache is
// flushed once at the end.
func syncTree(ctx context.Context, dir string) error {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil // symlinks: their directory's sync covers them
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return unix.Fsync(int(f.Fd()))
	})
	if err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return fmt.Errorf("F_FULLFSYNC %q: %w", dir, err)
	}
	return nil
}
