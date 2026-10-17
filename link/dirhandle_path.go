//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package link

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// pathDir is the dirHandle for systems without *at system calls: the
// directory is re-checked to be a real directory, then used by path.
type pathDir struct{ path string }

func openDir(root string, comps []string) (dirHandle, error) {
	path := root
	for _, c := range comps {
		path = filepath.Join(path, c)
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s: %w", path, errDirChanged)
		}
	}
	return &pathDir{path: path}, nil
}

func (d *pathDir) close() {}

func (d *pathDir) create(leaf string) (*os.File, error) {
	return os.OpenFile(filepath.Join(d.path, leaf), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func (d *pathDir) link(src, leaf string) error { return os.Link(src, filepath.Join(d.path, leaf)) }

func (d *pathDir) symlink(target, leaf string) error {
	return os.Symlink(target, filepath.Join(d.path, leaf))
}

func (d *pathDir) mkdir(leaf string) (bool, error) {
	p := filepath.Join(d.path, leaf)
	err := os.Mkdir(p, 0o777)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrExist) {
		if fi, serr := os.Lstat(p); serr == nil && fi.IsDir() {
			return false, nil
		}
	}
	return false, err
}
