package goomba

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/record"
)

// maxMetadataSize bounds a package metadata file read into memory, as in
// package link.
const maxMetadataSize = 64 << 20

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

// packageDests lists the paths the extracted package in pkgDir will create
// when linked with python (nil when the environment has no python): every
// entry of info/paths.json (or the legacy info/files), relocated for noarch:
// python, the .pyc files of its site-packages sources and its entry points.
// It reads the same metadata link.Link reads; link validates it in full.
func packageDests(pkgDir string, rec *record.PackageRecord, py *link.PythonInfo) ([]dest, error) {
	noarch := rec.Noarch
	if noarch == "" {
		var err error
		if noarch, err = indexNoarch(pkgDir); err != nil {
			return nil, err
		}
	}
	if noarch == "python" && py == nil {
		return nil, fmt.Errorf("goomba: %s: %w: python is not in the lockfile", rec.Dist(), link.ErrNoPython)
	}
	entries, err := packagePaths(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("goomba: %s: %w", rec.Dist(), err)
	}
	out := make([]dest, 0, len(entries))
	var pyc []dest
	for _, e := range entries {
		p, kind := e.Path, claimFile
		if e.PathType == link.PathDirectory {
			kind = claimDir
		}
		if noarch == "python" {
			p = link.NoarchTargetPath(p, py.SitePackagesPath, py.BinDir)
			if isPycCandidate(e.Path) {
				pyc = append(pyc, dest{path: link.PycPath(p, py.Version), kind: claimFile})
			}
		}
		out = append(out, dest{path: p, kind: kind})
	}
	out = append(out, pyc...)
	for _, d := range out {
		if !cleanRelPath(d.path) {
			return nil, fmt.Errorf("goomba: %s: %w: %q is not a clean relative path", rec.Dist(), link.ErrUnsafePath, d.path)
		}
	}
	if noarch == "python" {
		eps, err := entryPoints(pkgDir)
		if err != nil {
			return nil, fmt.Errorf("goomba: %s: %w", rec.Dist(), err)
		}
		for _, def := range eps {
			ep, err := link.ParseEntryPoint(def)
			if err != nil {
				return nil, fmt.Errorf("goomba: %s: %w", rec.Dist(), err)
			}
			out = append(out, dest{path: py.BinDir + "/" + ep.Command, kind: claimFile})
		}
	}
	return out, nil
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

// cleanRelPath reports whether p is a non-empty relative slash path without
// empty, "." or ".." components. link refuses such paths too (and more);
// the check here keeps the claim bookkeeping well defined.
func cleanRelPath(p string) bool {
	if p == "" || p[0] == '/' {
		return false
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}

// isPycCandidate is libmamba's "^site-packages[/\\][^\t\n\r\f\v]+\.py$",
// the rule link uses to record .pyc files.
func isPycCandidate(p string) bool {
	rest, ok := strings.CutPrefix(p, "site-packages/")
	if !ok {
		rest, ok = strings.CutPrefix(p, `site-packages\`)
	}
	if !ok || !strings.HasSuffix(rest, ".py") || len(rest) < len(".py")+1 {
		return false
	}
	return !strings.ContainsAny(rest, "\t\n\r\f\v")
}

// readPkgFile reads info/<name> from an extracted package, refusing
// symlinks and files over maxMetadataSize. A missing file wraps
// fs.ErrNotExist.
func readPkgFile(pkgDir, name string) ([]byte, error) {
	info := filepath.Join(pkgDir, "info")
	fi, err := os.Lstat(info)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: info is not a directory", link.ErrInvalidPackage)
	}
	p := filepath.Join(info, name)
	if fi, err = os.Lstat(p); err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: info/%s is not a regular file", link.ErrInvalidPackage, name)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMetadataSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadataSize {
		return nil, fmt.Errorf("%w: info/%s is larger than %d bytes", link.ErrInvalidPackage, name, maxMetadataSize)
	}
	return data, nil
}

// indexNoarch returns the noarch kind from info/index.json ("" if absent),
// mapping the legacy noarch: true to "generic" as link does.
func indexNoarch(pkgDir string) (string, error) {
	data, err := readPkgFile(pkgDir, "index.json")
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var idx struct {
		Noarch json.RawMessage `json:"noarch"`
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return "", fmt.Errorf("%w: index.json: %v", link.ErrInvalidPackage, err)
	}
	var s string
	var b bool
	switch {
	case len(idx.Noarch) == 0:
	case json.Unmarshal(idx.Noarch, &s) == nil:
		return s, nil
	case json.Unmarshal(idx.Noarch, &b) == nil:
		if b {
			return "generic", nil
		}
	}
	return "", nil
}

// packagePaths reads info/paths.json, or the legacy info/files list.
func packagePaths(pkgDir string) ([]link.PathEntry, error) {
	data, err := readPkgFile(pkgDir, "paths.json")
	if err == nil {
		return link.ParsePathsJSON(data)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read paths.json: %w", err)
	}
	if data, err = readPkgFile(pkgDir, "files"); err != nil {
		return nil, fmt.Errorf("read info/files: %w", err)
	}
	// The same line rules as link's legacy reader.
	var out []link.PathEntry
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if l := strings.Trim(string(line), " \t\n\v\f\r"); l != "" {
			out = append(out, link.PathEntry{Path: l, PathType: link.PathHardlink})
		}
	}
	return out, nil
}

// entryPoints returns noarch.entry_points from info/link.json.
func entryPoints(pkgDir string) ([]string, error) {
	data, err := readPkgFile(pkgDir, "link.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lj struct {
		Noarch *struct {
			EntryPoints []string `json:"entry_points"`
		} `json:"noarch"`
	}
	if err := json.Unmarshal(data, &lj); err != nil {
		return nil, fmt.Errorf("%w: link.json: %v", link.ErrInvalidPackage, err)
	}
	if lj.Noarch == nil {
		return nil, nil
	}
	return lj.Noarch.EntryPoints, nil
}
