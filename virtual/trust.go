package virtual

import (
	"io/fs"
	"strings"
	"time"
)

// maxLinkHops bounds the symbolic links followed while resolving one path,
// as the Linux kernel does (MAXSYMLINKS).
const maxLinkHops = 40

// program is a program found by a search: the file to execute, and the
// name to give it as argv[0], the location it was found at. A multi-call
// binary (busybox, toybox) reached through a link dispatches on that name,
// while the file executed is the resolved one that was checked.
type program struct {
	path string
	name string
}

// fileFacts is what the program searches need to know about a file.
type fileFacts struct {
	mode    fs.FileMode
	uid     uint32 // owner on Unix; not root where unknown
	modTime time.Time
}

// searchFS is the file system the program searches look at. osSearchFS is
// the real one; tests use fakes so that the Unix and Windows searches can
// both be tested on any host.
type searchFS interface {
	// stat describes name, following symbolic links.
	stat(name string) (fileFacts, error)
	// lstat describes name itself: a final symbolic link is not followed.
	// The searches pass only paths whose directories hold no links.
	lstat(name string) (fileFacts, error)
	// readlink returns the target of the symbolic link name.
	readlink(name string) (string, error)
	// readDirNames returns at most max entry names of dir, in any order.
	readDirNames(dir string, max int) ([]string, error)
}

// isRegularFile reports whether path is a regular file (after following
// symbolic links): the only requirement on a program path the caller chose.
func isRegularFile(fsys searchFS, path string) bool {
	fi, err := fsys.stat(path)
	return err == nil && fi.mode.IsRegular()
}

// trustedUnixExecutable resolves the absolute path p one component at a
// time and reports the resolved path (without symbolic links) if every
// step is one only root controls:
//   - every directory passed through, including those reached through links
//     and those holding the links, is owned by root and not writable by
//     group or others, unless it is sticky (like /tmp, or /nix/store);
//   - in such a sticky, writable directory every entry used, link or not,
//     is owned by root, since another user may have created it, and
//     stickiness only stops others from renaming root's entries;
//   - the final file is a regular file owned by root and not writable by
//     group or others.
//
// Callers run the resolved path, never p, so the program that runs is the
// one checked: nobody but root can replace it or any directory above it
// between the check and the run. Anything else is treated as absent. A
// missing p costs a single stat.
//
// The check covers the file and its path, not what the program loads (its
// #! interpreter, the ELF loader, shared libraries), which is root's
// configuration; callers run it with an empty environment, so LD_PRELOAD,
// LD_LIBRARY_PATH and PATH play no part. One gap: with the kernel's
// protected_hardlinks off, another user can hard-link a root-owned file
// into a sticky directory, where the entry then looks root's; no built-in
// location has a file or link directly in a sticky directory, and
// directories cannot be hard-linked.
func trustedUnixExecutable(fsys searchFS, p string) (string, bool) {
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	if _, err := fsys.stat(p); err != nil {
		return "", false
	}
	root, err := fsys.lstat("/")
	if err != nil || !trustedDir(root) {
		return "", false
	}
	type dir struct {
		path  string
		facts fileFacts
	}
	stack := []dir{{"/", root}} // the resolved directories from / down
	rest := splitUnixPath(p)
	for hops := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		if name == ".." {
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		parent := stack[len(stack)-1]
		next := parent.path + "/" + name
		if parent.path == "/" {
			next = "/" + name
		}
		fi, err := fsys.lstat(next)
		if err != nil {
			return "", false
		}
		if parent.facts.mode.Perm()&0o022 != 0 && fi.uid != 0 {
			return "", false // in a sticky directory others write to
		}
		switch {
		case fi.mode&fs.ModeSymlink != 0:
			if hops++; hops > maxLinkHops {
				return "", false
			}
			target, err := fsys.readlink(next)
			if err != nil || target == "" {
				return "", false
			}
			if strings.HasPrefix(target, "/") {
				stack = stack[:1]
			}
			rest = append(splitUnixPath(target), rest...)
		case len(rest) > 0:
			if !trustedDir(fi) {
				return "", false
			}
			stack = append(stack, dir{next, fi})
		default:
			if !fi.mode.IsRegular() || fi.uid != 0 || fi.mode.Perm()&0o022 != 0 {
				return "", false
			}
			return next, true
		}
	}
	return "", false // p resolves to a directory
}

// trustedDir reports whether a directory is owned by root and not writable
// by group or others, unless it is sticky.
func trustedDir(fi fileFacts) bool {
	return fi.mode.IsDir() && fi.uid == 0 &&
		(fi.mode.Perm()&0o022 == 0 || fi.mode&fs.ModeSticky != 0)
}

// splitUnixPath returns the components of a slash-separated path, without
// empty and "." components.
func splitUnixPath(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}
