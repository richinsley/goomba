package archive

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// treeFS performs every filesystem operation of an extraction, relative to
// the private temporary directory. Paths are clean, slash-separated and
// relative to that directory; "" is the directory itself. Implementations
// never follow a symlink in the final component, and callers never pass a
// path whose parents are not directories created by the extraction.
type treeFS interface {
	mkdir(rel string) error
	// create makes a new regular file, failing if rel exists.
	create(rel string) (*os.File, error)
	// remove unlinks a file or symlink.
	remove(rel string) error
	symlink(rel, target string) error
	link(src, dst string) error
	setMtime(rel string, t time.Time) error
	readlink(rel string) (string, error)
	// kindOf reports what rel is, kindNone if it does not exist or a
	// parent is missing or not a directory.
	kindOf(rel string) (kind, error)
	// list returns the names in directory rel.
	list(rel string) ([]string, error)
	// chmodRoot sets the mode of the temporary directory itself.
	chmodRoot(perm fs.FileMode) error
	close()
}

// splitRel returns rel's parent ("" for the top level) and base name.
func splitRel(rel string) (string, string) {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}

// isMissing reports errors meaning "no such entry" during a lookup.
func isMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// rootFS implements treeFS with an os.Root for creation and paths inside
// the temporary directory for links and times. It serves the platforms
// where x/sys lacks some directory-relative calls (Windows, Solaris,
// illumos, DragonFly BSD, AIX).
type rootFS struct {
	root *os.Root
	dir  string
}

func (r *rootFS) os(rel string) string   { return filepath.FromSlash(rel) }
func (r *rootFS) full(rel string) string { return filepath.Join(r.dir, filepath.FromSlash(rel)) }

func (r *rootFS) mkdir(rel string) error { return r.root.Mkdir(r.os(rel), 0o755) }

func (r *rootFS) create(rel string) (*os.File, error) {
	return r.root.OpenFile(r.os(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func (r *rootFS) remove(rel string) error { return r.root.Remove(r.os(rel)) }

func (r *rootFS) symlink(rel, target string) error { return os.Symlink(target, r.full(rel)) }

func (r *rootFS) link(src, dst string) error { return os.Link(r.full(src), r.full(dst)) }

func (r *rootFS) setMtime(rel string, t time.Time) error { return os.Chtimes(r.full(rel), t, t) }

func (r *rootFS) readlink(rel string) (string, error) { return os.Readlink(r.full(rel)) }

func (r *rootFS) kindOf(rel string) (kind, error) {
	fi, err := r.root.Lstat(r.os(rel))
	if err != nil {
		if isMissing(err) {
			return kindNone, nil
		}
		return kindNone, err
	}
	return kindOfMode(fi.Mode()), nil
}

func (r *rootFS) list(rel string) ([]string, error) {
	name := "."
	if rel != "" {
		name = r.os(rel)
	}
	f, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

func (r *rootFS) chmodRoot(perm fs.FileMode) error { return os.Chmod(r.dir, perm) }

func (r *rootFS) close() {}

// kindOfMode maps a file mode to the kinds an extraction creates; anything
// else is kindOther.
func kindOfMode(m fs.FileMode) kind {
	switch {
	case m.IsDir():
		return kindDir
	case m.IsRegular():
		return kindFile
	case m&fs.ModeSymlink != 0:
		return kindSymlink
	}
	return kindOther
}
