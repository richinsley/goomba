//go:build !unix

package safedir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// resolve makes p absolute and resolves the symlinks of its longest
// existing part with filepath.EvalSymlinks, creating missing directories
// first when create is set; ownership is not examined on this platform.
func resolve(p string, create bool) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return "", err
		}
	}
	var rest []string
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		up := filepath.Dir(p)
		if up == p {
			return p, nil
		}
		rest = append(rest, filepath.Base(p))
		p = up
	}
}

// CheckDir requires dir to be a directory; ownership and modes are not
// examined on this platform (see the package documentation).
func CheckDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return unsafef(dir, "%s is not a directory", dir)
	}
	return nil
}

// CheckOwnDir checks nothing on this platform; see CheckDir.
func CheckOwnDir(string, fs.FileInfo) error { return nil }

// CheckAncestors checks nothing on this platform; see CheckDir.
func CheckAncestors(string) error { return nil }

// CheckEntry checks nothing on this platform; see CheckDir.
func CheckEntry(string, fs.FileInfo) error { return nil }

// errNotDir is the error of a resolved directory that is not one.
var errNotDir = errors.New("not a directory")
