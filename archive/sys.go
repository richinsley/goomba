package archive

import (
	"errors"
	"io/fs"
	"os"
)

// renameChecked renames src to dst after checking that dst does not exist.
// It is the fallback where an atomic no-replace rename is unavailable.
func renameChecked(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}

// syncDir flushes a directory's entries to disk. It is best effort: some
// platforms (Windows) cannot sync a directory handle.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}
