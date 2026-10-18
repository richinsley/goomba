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

// symlinkTargetShape checks the shape of a symlink target and returns its
// normalised form: the number of leading ".." components and the names
// after them. A target must be zero or more ".." followed by zero or more
// names, once its "." and empty components ("./a", "a//b", "a/", "a/.",
// ".././../a", as conda-forge's ncurses ships) are dropped: path resolution
// skips them, or, after a name that is not a directory, fails with ENOTDIR,
// so they never move a target anywhere. Every check runs on the normalised
// form; the symlink is still created with the target exactly as written,
// as micromamba does.
//
// Refused: an empty target, a NUL byte or a backslash, an absolute or drive
// target (also once "." components are dropped, as in "./C:/x"), ".." after
// a name ("a/../b": a may be or become a symlink, so where it leads cannot
// be known from the text), and any name that folds to "conda-meta".
func symlinkTargetShape(target string) (int, []string, error) {
	bad := func(why string) (int, []string, error) {
		return 0, nil, fmt.Errorf("%w: symlink target %q %s", ErrUnsafePath, target, why)
	}
	switch {
	case target == "":
		return bad("is empty")
	case strings.ContainsAny(target, "\x00\\"):
		return bad("contains a NUL byte or backslash")
	case strings.HasPrefix(target, "/") || filepath.IsAbs(target) || hasDriveLetter(target):
		return bad("is absolute")
	}
	ups := 0
	var names []string
	for p := range strings.SplitSeq(target, "/") {
		switch {
		case p == "" || p == ".":
			continue
		case p == "..":
			if len(names) > 0 {
				return bad(`has ".." after a name`)
			}
			ups++
		default:
			// Whether such a component would reach conda-meta depends
			// on symlinks created later; refuse it whatever the order.
			if foldName(p) == "conda-meta" {
				return bad("names conda-meta")
			}
			names = append(names, p)
		}
	}
	if ups == 0 && len(names) > 0 && hasDriveLetter(names[0]) {
		return bad("is a drive path once its \".\" components are dropped")
	}
	return ups, names, nil
}
