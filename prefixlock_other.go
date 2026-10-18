//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package goomba

import (
	"io/fs"
	"os"
	"path/filepath"
)

// lockDir opens the directory dir. Without flock, the exclusive creation of
// conda-meta in an empty prefix is what keeps two installs apart; on
// Windows, two installs that start into the same empty prefix at the same
// moment can both pass the emptiness check before either creates
// conda-meta, and the loser then fails without removing anything.
func lockDir(dir string) (*os.File, error) {
	return os.Open(dir)
}

// unlockDir closes f.
func unlockDir(f *os.File) {
	_ = f.Close()
}

// emptyDir removes everything inside the directory open as d, by path: a
// directory renamed above the prefix during the rollback would redirect
// it, a race the Unix version, which works through the handle, does not
// have.
func emptyDir(d *os.File) error {
	f, err := os.Open(d.Name())
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	for _, n := range names {
		if rerr := os.RemoveAll(filepath.Join(d.Name(), n)); err == nil {
			err = rerr
		}
	}
	return err
}

// setDirMode does nothing on this platform, whose permissions are ACLs.
func setDirMode(*os.File, fs.FileMode) error { return nil }
