package goomba

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/richinsley/goomba/link"
)

// emptySHA256 is the digest of no bytes, which link and libmamba record for
// symlinks that do not resolve to a file.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// rehashSymlinks sets the sha256_in_prefix of every symlink record of insts
// once all packages are linked: the digest of the file the symlink resolves
// to inside the prefix (the digest recorded for that file when a package
// installed it, else its contents' digest), or the empty digest when it
// resolves to no file. link records the target as found while linking the
// symlink's own package, which for a target in a package of the same layer
// depends on which was linked first; this makes it independent of timing.
// A target in a package linked later (a broken dependency cycle) now counts
// too, where micromamba records the empty digest.
func rehashSymlinks(ctx context.Context, prefix string, insts []*link.Installed) error {
	root, err := filepath.EvalSymlinks(prefix)
	if err != nil {
		return fmt.Errorf("goomba: symlink digests: %w", err)
	}
	known := map[string]string{}
	for _, in := range insts {
		if in == nil {
			continue
		}
		for _, p := range in.Paths {
			if p.PathType == link.PathHardlink && p.SHA256InPrefix != "" {
				known[p.Path] = p.SHA256InPrefix
			}
		}
	}
	for _, in := range insts {
		if in == nil {
			continue
		}
		for j := range in.Paths {
			p := &in.Paths[j]
			if p.PathType != link.PathSoftlink {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			final, exists, err := resolveIn(root, p.Path)
			if err != nil {
				return fmt.Errorf("goomba: symlink %s: %w", p.Path, err)
			}
			digest := emptySHA256
			if exists {
				if d, ok := known[final]; ok {
					digest = d
				} else if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(final))); err == nil && fi.Mode().IsRegular() {
					if digest, err = hashPath(filepath.Join(root, filepath.FromSlash(final))); err != nil {
						return fmt.Errorf("goomba: symlink %s: %w", p.Path, err)
					}
					known[final] = digest
				}
			}
			p.SHA256InPrefix = digest
		}
	}
	return nil
}

// verifyInterpreter checks, just before it is run, that the interpreter
// py.Executable in prefix is a file some locked package put there: it
// follows a symlinked interpreter (bin/python3.11 -> python3.11-real) to the
// file it resolves to inside the prefix, finds the package that installed
// that file (dirs and insts are indexed alike), and requires the very inode
// hard linked from that package's verified tree in the cache, or, if it was
// copied (prefix replacement, another file system, a noarch package), a
// file whose sha256 is the one link recorded. A file no package installed
// fails. The prefix and the directories above it are the user's alone
// (openPrefix checks), so only the user's own processes could swap it
// between this check and the start of the interpreter.
func verifyInterpreter(prefix string, py *link.PythonInfo, dirs []string, insts []*link.Installed) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("goomba: compile .pyc: %w: "+format, append([]any{ErrCompilePyc}, a...)...)
	}
	final, exists, err := resolveIn(prefix, py.Executable)
	if err != nil || !exists {
		return fail("interpreter %s not found (%v)", py.Executable, err)
	}
	owner := -1
	var rec *link.PathRecord
	for i, in := range insts {
		if in == nil {
			continue
		}
		for j := range in.Paths {
			if p := &in.Paths[j]; p.Path == final && p.PathType == link.PathHardlink {
				owner, rec = i, p
			}
		}
	}
	if rec == nil {
		return fail("interpreter %s resolves to %s, which no locked package installed", py.Executable, final)
	}
	abs := filepath.Join(prefix, filepath.FromSlash(final))
	lfi, err := os.Lstat(abs)
	if err != nil {
		return fail("%v", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return fail("%v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fail("%v", err)
	}
	if !fi.Mode().IsRegular() || !os.SameFile(fi, lfi) {
		return fail("interpreter %s is not a regular file", final)
	}
	if insts[owner].Noarch == "" {
		// Not relocated: the package path is the prefix path.
		if src, err := os.Lstat(filepath.Join(dirs[owner], filepath.FromSlash(final))); err == nil && os.SameFile(fi, src) {
			return nil
		}
	}
	want := rec.SHA256InPrefix
	if want == "" {
		want = rec.SHA256
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fail("%v", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); want == "" || !strings.EqualFold(got, want) {
		return fail("interpreter %s has sha256 %s, not the %s linked from the verified package", final, got, want)
	}
	return nil
}

// hashPath returns the hex sha256 of the file at p.
func hashPath(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// within reports whether child is parent or lies below it, comparing the
// paths with symlinks resolved as far as they exist, so that an alias of
// the parent is caught too.
func within(child, parent string) (bool, error) {
	c, err := resolvedPath(child)
	if err != nil {
		return false, err
	}
	p, err := resolvedPath(parent)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// resolvedPath returns the absolute form of p with the symlinks of its
// longest existing ancestor resolved and the missing rest appended.
func resolvedPath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
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
