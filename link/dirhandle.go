package link

import (
	"errors"
	"io/fs"
	"os"
)

// errDirChanged reports that a directory resolved inside the prefix was
// replaced (by a symlink, a file or nothing) before it could be opened; the
// caller resolves again.
var errDirChanged = errors.New("directory changed while it was being opened")

// dirHandle is a destination directory opened for the final operation on
// one of its entries. On systems with *at system calls it holds a file
// descriptor obtained by opening every component of the resolved path
// without following symlinks, so the checks made on the resolved path apply
// to the directory actually written to even if the prefix changes
// concurrently. Elsewhere it holds the path.
type dirHandle interface {
	// create creates leaf exclusively for writing with mode 0600.
	create(leaf string) (*os.File, error)
	// link creates leaf as a hard link to the absolute path src.
	link(src, leaf string) error
	// symlink creates leaf as a symlink to target.
	symlink(target, leaf string) error
	// mkdir creates leaf as a directory; an existing directory (not a
	// symlink to one) is accepted and reported with created == false.
	mkdir(leaf string) (created bool, err error)
	close()
}

// retryable reports whether an error from opening or using a dirHandle
// means the directory changed underneath and resolution should be redone.
func retryable(err error) bool {
	return errors.Is(err, errDirChanged) || errors.Is(err, fs.ErrNotExist)
}
