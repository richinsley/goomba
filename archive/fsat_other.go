//go:build !(darwin || freebsd || linux || netbsd || openbsd)

package archive

import (
	"fmt"
	"io/fs"
	"os"
)

// haveAt is false: these platforms (Windows, Solaris, illumos, DragonFly
// BSD, AIX) lack some of the directory-relative calls in x/sys, so the
// extraction uses an os.Root for creation and paths inside the private
// temporary directory for links, times and lookups, and checks that
// directory's identity before it is renamed into place.
const haveAt = false

func newAtFS(*os.File) treeFS { return nil }

// openDirNoFollow opens the directory p after checking that p is not a
// symlink, and that what was opened is what was checked.
func openDirNoFollow(p string) (*os.File, error) {
	lfi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !lfi.IsDir() {
		return nil, fmt.Errorf("%w: temporary directory %q is not a directory", ErrUnsafePath, p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !os.SameFile(fi, lfi) || fi.Mode()&fs.ModeSymlink != 0 {
		f.Close()
		return nil, fmt.Errorf("%w: temporary directory %q was replaced", ErrUnsafePath, p)
	}
	return f, nil
}
