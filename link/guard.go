package link

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// guard checks resolved locations in a prefix against the places a package
// must not reach: conda-meta (by any name or alias of the directory) and
// bin or Scripts. Checks run on resolved components, so a symlinked
// directory cannot hide the real location. Identity checks are never
// cached, because the tree changes while a package is linked. A guard
// belongs to one Link or Unlink call and is not safe for concurrent use.
type guard struct {
	root *root
}

func newGuard(r *root) *guard { return &guard{root: r} }

// metaInfo stats <prefix>/conda-meta afresh on every call: a cached
// FileInfo could outlive the directory and match an unrelated directory
// that reuses its inode.
func (g *guard) metaInfo() fs.FileInfo {
	if fi, err := os.Stat(filepath.Join(g.root.dir, "conda-meta")); err == nil && fi.IsDir() {
		return fi
	}
	return nil
}

// inCondaMeta reports whether the resolved path comps (below the prefix) is
// conda-meta or lies inside it: its first component folds to "conda-meta",
// or one of its directories is the same file as <prefix>/conda-meta (which
// catches case folding, normalisation and other aliases).
func (g *guard) inCondaMeta(comps []string) bool {
	if len(comps) == 0 {
		return false
	}
	if foldName(comps[0]) == "conda-meta" {
		return true
	}
	meta := g.metaInfo()
	if meta == nil {
		return false
	}
	for i := 1; i <= len(comps); i++ {
		if fi, err := os.Stat(g.root.abs(comps[:i])); err == nil && os.SameFile(fi, meta) {
			return true
		}
	}
	return false
}

// isScriptDir reports whether the resolved directory comps is the prefix's
// bin or Scripts directory, by name or by identity.
func (g *guard) isScriptDir(comps []string) bool {
	if len(comps) == 1 && isScriptDirName(comps[0]) {
		return true
	}
	if len(comps) == 0 {
		return false
	}
	fi, err := os.Stat(g.root.abs(comps))
	if err != nil {
		return false
	}
	for _, name := range [...]string{"bin", "Scripts"} {
		if bi, err := os.Stat(filepath.Join(g.root.dir, name)); err == nil && os.SameFile(fi, bi) {
			return true
		}
	}
	return false
}

// symlinkTargetShape checks that a symlink target is in normal form:
// "." or zero or more ".." followed by zero or more names, never "." or ".."
// after a name, nothing absolute, no empty components or backslashes, and
// no name that folds to "conda-meta". It returns the number of leading ".."
// and the names.
func symlinkTargetShape(target string) (int, []string, error) {
	bad := func(why string) (int, []string, error) {
		return 0, nil, fmt.Errorf("%w: symlink target %q %s", ErrUnsafePath, target, why)
	}
	switch {
	case target == "":
		return bad("is empty")
	case target == ".":
		return 0, nil, nil
	case strings.ContainsAny(target, "\x00\\"):
		return bad("contains a NUL byte or backslash")
	case strings.HasPrefix(target, "/") || filepath.IsAbs(target) || hasDriveLetter(target):
		return bad("is absolute")
	}
	parts := strings.Split(target, "/")
	ups := 0
	for ups < len(parts) && parts[ups] == ".." {
		ups++
	}
	for _, p := range parts[ups:] {
		if p == "" || p == "." || p == ".." {
			return bad("is not in normal form")
		}
		// Whether such a component would reach conda-meta depends on
		// symlinks created later; refuse it whatever the order.
		if foldName(p) == "conda-meta" {
			return bad("names conda-meta")
		}
	}
	return ups, parts[ups:], nil
}
