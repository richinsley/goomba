//go:build !unix

package lock

import (
	"fmt"
	"os"
	"path/filepath"
)

// openRegular opens path for reading after checking with Lstat that it is a
// regular file. The caller re-checks with os.SameFile after opening, which
// catches a link swapped in between.
func openRegular(path string) (*os.File, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if !lst.Mode().IsRegular() {
		return nil, invalidf("conda-meta %q: not a regular file", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	return f, nil
}
