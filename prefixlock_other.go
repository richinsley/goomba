//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package goomba

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// havePrefixLock reports that this platform has no prefix lock, so
// RemoveIncomplete refuses to work here.
const havePrefixLock = false

// lockOpenDir does nothing: there is no lock on this platform (see
// lockDir).
func lockOpenDir(*os.File) error { return nil }

// lockDir opens the directory dir (see openDirNoFollow). Without flock, the
// exclusive creation of conda-meta in an empty prefix is what keeps two
// installs apart; on Windows, two installs that start into the same empty
// prefix at the same moment can both pass the emptiness check before
// either creates conda-meta, and the loser then fails without removing
// anything.
func lockDir(dir string) (*os.File, error) {
	return openDirNoFollow(dir)
}

// tryLockShared does nothing: there is no lock on this platform (see
// lockDir).
func tryLockShared(*os.File) error { return nil }

// readState reads the state of the prefix directory open as d, by path. A
// conda-meta that is not a directory is an error.
func readState(d *os.File) (prefixState, error) {
	var st prefixState
	n, err := countNames(d.Name(), 2)
	if err != nil {
		return st, err
	}
	st.entries = n
	dir := filepath.Join(d.Name(), "conda-meta")
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return st, err
	case !fi.IsDir():
		return st, fmt.Errorf("conda-meta: %w", errNotADir)
	}
	st.meta = true
	if st.metaEntries, err = countNames(dir, 1); err != nil {
		return st, err
	}
	exists := func(name string) (bool, error) {
		_, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	if st.marker, err = exists(incompleteMarker); err != nil {
		return st, err
	}
	st.history, err = exists("history")
	return st, err
}

// countNames counts the entries of the directory dir, up to max.
func countNames(dir string, max int) (int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	names, err := f.Readdirnames(max)
	if err == io.EOF {
		err = nil
	}
	return len(names), err
}

// unlockDir closes f.
func unlockDir(f *os.File) {
	_ = f.Close()
}

// emptyDir empties the prefix directory open as d with emptyPrefix, by
// path: a directory renamed above the prefix during the removal would
// redirect it, a race the Unix version, which works through the handle,
// does not have. On Windows d is opened with delete sharing, so the user's
// own processes can rename even the prefix itself meanwhile, and the
// removal then follows the name, not the directory. Mount points below the
// prefix: on Windows they are reparse points, which os.RemoveAll removes
// without descending into them; elsewhere they are not detected (only the
// prefix's own rollback empties a prefix on these platforms;
// RemoveIncomplete is not supported).
func emptyDir(d *os.File) error {
	return emptyPrefix(pathPrefixFS(d.Name()))
}

// pathPrefixFS is the prefixFS of the prefix directory at a path.
type pathPrefixFS string

func (p pathPrefixFS) path(rel string) string {
	return filepath.Join(string(p), filepath.FromSlash(rel))
}

func (p pathPrefixFS) names(dir string) ([]string, error) {
	f, err := os.Open(p.path(dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err == io.EOF {
		err = nil
	}
	return names, err
}

func (p pathPrefixFS) removeAll(rel string) error {
	if dir, _, ok := strings.Cut(rel, "/"); ok {
		if fi, err := os.Lstat(p.path(dir)); err == nil && !fi.IsDir() {
			return fmt.Errorf("%s: %w", dir, errNotADir)
		}
	}
	return os.RemoveAll(p.path(rel))
}

func (p pathPrefixFS) ensureMarker() error {
	meta := p.path("conda-meta")
	if err := os.Mkdir(meta, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if fi, err := os.Lstat(meta); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("conda-meta: %w", errNotADir)
	}
	marker := filepath.Join(meta, incompleteMarker)
	if _, err := os.Lstat(marker); err == nil {
		return nil
	}
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// rmdirSame removes the directory path if it is still the one described
// by id (os.SameFile), by path.
func rmdirSame(path string, id fs.FileInfo) error {
	if fi, err := os.Lstat(path); err != nil || !os.SameFile(fi, id) {
		return nil
	}
	return os.Remove(path)
}

// isNotADir reports an error for something that should be a directory and
// is not.
func isNotADir(err error) bool { return errors.Is(err, errNotADir) }

// rmdir removes the empty directory path.
func rmdir(path string) error {
	if fi, err := os.Lstat(path); err == nil && !fi.IsDir() {
		return &os.PathError{Op: "rmdir", Path: path, Err: errNotADir}
	}
	return os.Remove(path)
}

// setDirMode does nothing on this platform, whose permissions are ACLs.
func setDirMode(*os.File, fs.FileMode) error { return nil }

// isBusy reports a mount point; never on this platform.
func isBusy(error) bool { return false }

// isNotEmpty reports a directory that is not empty.
func isNotEmpty(err error) bool { return err != nil && !errors.Is(err, fs.ErrNotExist) }

// markIncomplete makes sure the prefix open as d has conda-meta and the
// incomplete marker in it.
func markIncomplete(d *os.File) error { return pathPrefixFS(d.Name()).ensureMarker() }
