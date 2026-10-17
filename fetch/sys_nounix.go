//go:build !unix

package fetch

import (
	"fmt"
	"io/fs"
	"os"
)

// openNoFollow opens p for reading after checking that p is not a symlink.
func openNoFollow(p string) (*os.File, error) {
	if fi, err := os.Lstat(p); err != nil {
		return nil, err
	} else if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink", p)
	}
	return os.Open(p)
}

// fileStamp returns zeros: no inode number or change time is available.
func fileStamp(*os.File) (uint64, int64) { return 0, 0 }

// checkKeyFile requires a regular file; ownership and modes are not checked
// on this platform.
func checkKeyFile(fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", ErrUnsafeCache)
	}
	return nil
}
