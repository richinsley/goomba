//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package rmtree

import (
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll is os.RemoveAll on this platform (see the package
// documentation); if that fails, it makes the directories of the tree
// writable (filepath.WalkDir follows no symlink or reparse point) and
// tries again.
func RemoveAll(path string) error {
	if os.RemoveAll(path) == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(path)
}
