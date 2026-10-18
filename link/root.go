package link

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxSymlinkHops bounds symlink expansion during path resolution, like the
// kernel's ELOOP limit.
const maxSymlinkHops = 255

// root confines filesystem operations to the tree below dir. Paths are
// resolved one component at a time; symlinks are followed only while the
// result stays below dir (or below dir's real path, for absolute targets),
// so a package cannot write or read outside its tree through a symlink it
// or another package planted. Final path components are never followed by
// the operations that create or remove entries.
//
// Resolution by path is not atomic with the operation that follows it. Link
// therefore performs its writes in a directory reopened along the resolved
// components without following symlinks (see dirHandle), and checks again
// whenever the tree changed in between.
type root struct {
	dir  string // absolute and clean
	real string // dir with symlinks evaluated, or dir if that fails
	// mkdirCheck, if set, vets every directory resolveCreate is about to
	// create (or to plan, with an overlay), given its resolved parent.
	mkdirCheck func(parent []string, name string) error
}

func newRoot(dir string) *root {
	r := &root{dir: filepath.Clean(dir), real: filepath.Clean(dir)}
	if real, err := filepath.EvalSymlinks(r.dir); err == nil {
		r.real = real
	}
	return r
}

// abs returns the OS path for the resolved components.
func (r *root) abs(components []string) string {
	if len(components) == 0 {
		return r.dir
	}
	return filepath.Join(r.dir, filepath.FromSlash(strings.Join(components, "/")))
}

// within reports the components of the absolute path p below the root, if
// p lies inside it.
func (r *root) within(p string) ([]string, bool) {
	p = filepath.Clean(p)
	for _, base := range []string{r.dir, r.real} {
		if p == base {
			return nil, true
		}
		rel, err := filepath.Rel(base, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		return splitComponents(filepath.ToSlash(rel)), true
	}
	return nil, false
}

func splitComponents(p string) []string {
	var out []string
	for c := range strings.SplitSeq(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// splitResolve splits p into the components resolve walks: like
// splitComponents, but "." and empty components stay, as ".", so that one
// after a file ("file/", "file/.") fails as the kernel's ENOTDIR does,
// rather than resolving to the file.
func splitResolve(p string) []string {
	var out []string
	for c := range strings.SplitSeq(p, "/") {
		if c == "" {
			c = "."
		}
		out = append(out, c)
	}
	return out
}

type resolveMode int

const (
	// resolveExisting resolves a directory that must already exist.
	resolveExisting resolveMode = iota
	// resolveCreate resolves a directory, creating missing components (in
	// the overlay instead, when one is given).
	resolveCreate
	// resolveFollow resolves any existing entry, following a final symlink.
	resolveFollow
	// resolveLenient resolves like resolveFollow, but takes missing
	// components literally instead of failing: it tells where a symlink
	// target would land without creating anything.
	resolveLenient
)

// overlay holds the entries a Link plans to create, keyed by their resolved
// slash path below the root. Resolving through an overlay treats them as if
// they existed, so a package's later entries are validated against its
// earlier ones before anything is written.
type overlay map[string]overlayEntry

type overlayEntry struct {
	kind   entryKind
	target string // symlink target
}

type entryKind int

const (
	kindFile entryKind = iota
	kindDir
	kindSymlink
)

func (o overlay) get(comps []string) (overlayEntry, bool) {
	if o == nil {
		return overlayEntry{}, false
	}
	e, ok := o[strings.Join(comps, "/")]
	return e, ok
}

func (o overlay) put(comps []string, e overlayEntry) { o[strings.Join(comps, "/")] = e }

// resolve walks rel (slash-separated, relative to the root) and returns the
// absolute OS path it designates with all symlinks expanded, plus the
// components of that path below the root. Created directories are appended
// to *created in creation order. Entries of ov take precedence over the
// file system.
func (r *root) resolve(rel string, mode resolveMode, created *[]string, ov overlay) (string, []string, error) {
	queue := splitResolve(rel)
	var stack []string
	hops := 0
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == "." {
			// The component before it was checked to be a directory
			// (this one was still queued), as the kernel requires.
			continue
		}
		if c == ".." {
			if len(stack) == 0 {
				return "", nil, fmt.Errorf("%w: %q resolves outside %s", ErrUnsafePath, rel, r.dir)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		next := append(stack[:len(stack):len(stack)], c)
		var kind entryKind
		var target string
		if e, ok := ov.get(next); ok {
			kind, target = e.kind, e.target
		} else {
			cur := r.abs(next)
			fi, err := os.Lstat(cur)
			if errors.Is(err, fs.ErrNotExist) && mode == resolveLenient {
				stack = next
				continue
			}
			if errors.Is(err, fs.ErrNotExist) && mode == resolveCreate {
				if r.mkdirCheck != nil {
					if err := r.mkdirCheck(stack, c); err != nil {
						return "", nil, err
					}
				}
				if ov != nil {
					ov.put(next, overlayEntry{kind: kindDir})
					stack = next
					continue
				}
				// Create the directory inside its parent opened without
				// following symlinks, so a parent swapped since it was
				// resolved is noticed rather than written through.
				d, derr := openDir(r.dir, stack)
				if derr != nil {
					return "", nil, derr
				}
				made, merr := d.mkdir(c)
				d.close()
				if merr != nil {
					return "", nil, merr
				}
				if made && created != nil {
					*created = append(*created, cur)
				}
				fi, err = os.Lstat(cur)
			}
			if err != nil {
				return "", nil, err
			}
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				kind = kindSymlink
				if target, err = os.Readlink(cur); err != nil {
					return "", nil, err
				}
			case fi.IsDir():
				kind = kindDir
			default:
				kind = kindFile
			}
		}
		if kind == kindSymlink {
			hops++
			if hops > maxSymlinkHops {
				return "", nil, fmt.Errorf("%w: too many symlinks resolving %q", ErrUnsafePath, rel)
			}
			target = filepath.ToSlash(target)
			if path.IsAbs(target) || filepath.IsAbs(filepath.FromSlash(target)) {
				comps, ok := r.within(filepath.FromSlash(target))
				if !ok {
					return "", nil, fmt.Errorf("%w: %q resolves through a symlink to %q outside %s", ErrUnsafePath, rel, target, r.dir)
				}
				stack = nil
				queue = append(comps, queue...)
			} else {
				queue = append(splitResolve(target), queue...)
			}
			continue
		}
		if kind != kindDir && (len(queue) > 0 || mode != resolveFollow && mode != resolveLenient) {
			return "", nil, fmt.Errorf("%q: %s is not a directory", rel, r.abs(next))
		}
		stack = next
	}
	return r.abs(stack), stack, nil
}

// parentResolved resolves the directory containing rel and returns its OS
// path, its components below the root and rel's final component.
func (r *root) parentResolved(rel string, mode resolveMode, created *[]string, ov overlay) (string, []string, string, error) {
	dir, leaf := path.Split(rel)
	d, comps, err := r.resolve(strings.TrimSuffix(dir, "/"), mode, created, ov)
	if err != nil {
		return "", nil, "", err
	}
	return d, comps, leaf, nil
}

// parent resolves the directory containing rel and returns it with rel's
// final component. With create set, missing directories are created.
func (r *root) parent(rel string, create bool, created *[]string) (string, string, error) {
	mode := resolveExisting
	if create {
		mode = resolveCreate
	}
	d, _, leaf, err := r.parentResolved(rel, mode, created, nil)
	return d, leaf, err
}

// path returns the OS path of rel with its directory resolved inside the
// root and its final component left unresolved.
func (r *root) path(rel string) (string, error) {
	d, leaf, err := r.parent(rel, false, nil)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, leaf), nil
}

// lstat stats rel without following a final symlink.
func (r *root) lstat(rel string) (fs.FileInfo, error) {
	p, err := r.path(rel)
	if err != nil {
		return nil, err
	}
	return os.Lstat(p)
}

// readFile reads the regular file rel, refusing symlinks and anything
// larger than limit.
func (r *root) readFile(rel string, limit int64) ([]byte, error) {
	p, err := r.path(rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalidPackage, rel)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrInvalidPackage, rel, limit)
	}
	return os.ReadFile(p)
}
