//go:build linux

package archive

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames src to dst, failing if dst exists.
func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		// The filesystem does not support RENAME_NOREPLACE.
		return renameChecked(src, dst)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
