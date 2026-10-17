//go:build unix

package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openRegular opens path for reading without following a final symbolic
// link (O_NOFOLLOW) and without blocking if it is a FIFO (O_NONBLOCK, which
// has no effect on regular files). The caller checks that the result is a
// regular file.
func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		// Systems report O_NOFOLLOW on a link differently (ELOOP on Linux
		// and macOS, EMLINK on FreeBSD, EFTYPE on NetBSD), so ask Lstat
		// whether a link was in the way rather than matching errnos.
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
			return nil, invalidf("conda-meta %q: is a symbolic link", filepath.Base(path))
		}
		if lst, lerr := os.Lstat(path); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
			return nil, invalidf("conda-meta %q: is a symbolic link", filepath.Base(path))
		}
		return nil, fmt.Errorf("lock: %w", &os.PathError{Op: "open", Path: path, Err: err})
	}
	return os.NewFile(uintptr(fd), path), nil
}
