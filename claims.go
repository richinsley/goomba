package goomba

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/richinsley/goomba/link"
)

// claimKind is what a package puts at a path.
type claimKind uint8

const (
	claimDir  claimKind = iota // a directory, possibly shared
	claimFile                  // a file, symlink, .pyc or entry point
)

// dest is one path a package will create in the prefix.
type dest struct {
	path string // prefix-relative, slash-separated
	kind claimKind
}

// claim records the packages that need a path.
type claim struct {
	// file is the package installing a file or symlink at the path, or -1.
	file int
	// dirs are the first two distinct packages needing a directory at the
	// path (listed, or as the parent of one of their paths); -1 if fewer.
	dirs [2]int
}

// get returns the claim on p, or an empty one.
func (c claims) get(p string) claim {
	if cl, ok := c[p]; ok {
		return cl
	}
	return claim{file: -1, dirs: [2]int{-1, -1}}
}

// addDir records that pkg needs a directory at the path.
func (cl *claim) addDir(pkg int) {
	switch {
	case cl.dirs[0] == pkg || cl.dirs[1] == pkg:
	case cl.dirs[0] < 0:
		cl.dirs[0] = pkg
	case cl.dirs[1] < 0:
		cl.dirs[1] = pkg
	}
}

// otherDir returns a package other than pkg that needs a directory at the
// path, or -1.
func (cl claim) otherDir(pkg int) int {
	for _, d := range cl.dirs {
		if d >= 0 && d != pkg {
			return d
		}
	}
	return -1
}

// otherFile returns the package other than pkg that installs a file or
// symlink at the path, or -1.
func (cl claim) otherFile(pkg int) int {
	if cl.file != pkg {
		return cl.file
	}
	return -1
}

// claims maps prefix-relative paths to the packages that install them.
type claims map[string]claim

// plannedDests turns the paths link.PlannedPaths lists for a package into
// claims: directories may be shared, everything else (files, symlinks,
// .pyc files, entry points) belongs to one package.
func plannedDests(planned []link.PlannedPath) []dest {
	out := make([]dest, len(planned))
	for i, p := range planned {
		kind := claimFile
		if p.PathType == link.PathDirectory {
			kind = claimDir
		}
		out[i] = dest{path: p.Path, kind: kind}
	}
	return out
}

// add records pkg's dests and returns a *ClobberError naming the first
// conflict with an earlier package: a file or symlink path that another
// package also installs or needs as a directory, or a directory path (listed
// or implied by a path below it) where another package installs a file or
// symlink. Directories may be shared. Conflicts within one package are left
// to link.
func (c claims) add(pkg int, dests []dest, dists []string) error {
	clobber := func(p string, other int) error {
		return &ClobberError{Path: p, Packages: [2]string{dists[other], dists[pkg]}}
	}
	for _, d := range dests {
		var parents []string
		for dir := path.Dir(d.path); dir != "."; dir = path.Dir(dir) {
			parents = append(parents, dir)
		}
		for i := len(parents) - 1; i >= 0; i-- {
			cl := c.get(parents[i])
			if o := cl.otherFile(pkg); o >= 0 {
				return clobber(parents[i], o)
			}
			cl.addDir(pkg)
			c[parents[i]] = cl
		}
		cl := c.get(d.path)
		if o := cl.otherFile(pkg); o >= 0 {
			return clobber(d.path, o)
		}
		if d.kind == claimDir {
			cl.addDir(pkg)
		} else {
			if o := cl.otherDir(pkg); o >= 0 {
				return clobber(d.path, o)
			}
			cl.file = pkg
		}
		c[d.path] = cl
	}
	return nil
}

// owner finds the package other than pkg that installs a file or symlink at
// the existing prefix path p (relative), for a link.ErrExists that the
// pre-check did not predict: through a directory symlink, or on a
// case-insensitive file system. It returns -1 if no package does.
func (c claims) owner(prefix, p string, pkg int) int {
	if o := c.get(p).otherFile(pkg); o >= 0 {
		return o
	}
	if base, err := filepath.EvalSymlinks(prefix); err == nil {
		if real, err := filepath.EvalSymlinks(filepath.Join(prefix, filepath.FromSlash(p))); err == nil {
			if rel, err := filepath.Rel(base, real); err == nil {
				if o := c.get(filepath.ToSlash(rel)).otherFile(pkg); o >= 0 {
					return o
				}
			}
		}
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if o := c[k].otherFile(pkg); o >= 0 && strings.EqualFold(k, p) {
			return o
		}
	}
	return -1
}

// attributeExistsError turns err, a link.ErrExists from linking package
// pkg, into a *ClobberError naming the package that owns the path: the
// path the *link.ExistsError names, or failing that any existing path of
// pkg's (attributeExists). It returns nil when no other package owns one.
func (c claims) attributeExistsError(prefix string, pkg int, err error, dests []dest, dists []string) *ClobberError {
	var ee *link.ExistsError
	if errors.As(err, &ee) && !ee.SamePackage {
		if other := c.owner(prefix, ee.Path, pkg); other >= 0 {
			return &ClobberError{Path: ee.Path, Packages: [2]string{dists[other], dists[pkg]}}
		}
	}
	return c.attributeExists(prefix, pkg, dests, dists)
}

// attributeExists turns a link.ErrExists from linking package pkg into a
// *ClobberError naming the package that owns the path, when one does.
func (c claims) attributeExists(prefix string, pkg int, dests []dest, dists []string) *ClobberError {
	for _, d := range dests {
		if d.kind == claimDir {
			continue
		}
		if _, err := os.Lstat(filepath.Join(prefix, filepath.FromSlash(d.path))); err != nil {
			continue
		}
		if other := c.owner(prefix, d.path, pkg); other >= 0 {
			return &ClobberError{Path: d.path, Packages: [2]string{dists[other], dists[pkg]}}
		}
	}
	return nil
}
