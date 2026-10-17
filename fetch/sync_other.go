//go:build !linux && !darwin

package fetch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// syncTree makes everything written below dir durable by syncing every
// regular file and directory in it, checking ctx between files. On Windows
// directories cannot be synced and read-only files cannot be opened for
// flushing; those are skipped there.
func syncTree(ctx context.Context, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil // symlinks: their directory's sync covers them
		}
		if runtime.GOOS == "windows" {
			if d.IsDir() {
				return nil
			}
			f, err := os.OpenFile(p, os.O_RDWR, 0)
			if err != nil {
				return nil
			}
			defer f.Close()
			return f.Sync()
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
}
