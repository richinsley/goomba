package goomba

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/richinsley/goomba/link"
)

// maxSymlinkHops bounds the symlinks followed while resolving one path, as
// the kernel's MAXSYMLINKS does.
const maxSymlinkHops = 40

// verifySymlinks walks prefix, without following symlinks, and checks that
// every symlink in it resolves to a location inside prefix: absolute
// targets, ".." climbing above the prefix (also through other symlinks or
// after a missing component) and chains of more than maxSymlinkHops links
// are refused with link.ErrUnsafePath. Dangling symlinks that stay inside
// are accepted, as packages ship them. link already refuses escaping
// targets; this is a second line of defence over the finished tree.
//
// The walk starts from the prefix with its own symlinks resolved, so a
// prefix path that is itself a symlink is walked too.
//
// The walk also takes group and other write permission off every directory
// in the prefix (link creates them as the umask allows, so 0775 under umask
// 002 and 0777 under umask 0): before the environment's interpreter runs,
// nobody but the user may add, rename or replace a file in it.
func verifySymlinks(ctx context.Context, prefix string) error {
	prefix, err := filepath.EvalSymlinks(prefix)
	if err != nil {
		return fmt.Errorf("goomba: verify symlinks: %w", err)
	}
	return filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("goomba: verify symlinks: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			fi, err := d.Info()
			if err != nil {
				return fmt.Errorf("goomba: verify symlinks: %w", err)
			}
			if perm := fi.Mode().Perm(); perm&0o022 != 0 {
				if err := os.Chmod(p, perm&^0o022); err != nil {
					return fmt.Errorf("goomba: %w", err)
				}
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(prefix, p)
		if err != nil {
			return fmt.Errorf("goomba: verify symlinks: %w", err)
		}
		rel = filepath.ToSlash(rel)
		if err := resolveInside(prefix, rel); err != nil {
			return fmt.Errorf("goomba: symlink %s: %w", rel, err)
		}
		return nil
	})
}

// resolveInside resolves the slash-separated path rel below root, following
// symlinks, and fails if any step leaves root. Once a component is missing
// the rest of the path is resolved lexically.
func resolveInside(root, rel string) error {
	_, _, err := resolveIn(root, rel)
	return err
}

// resolveIn resolves the slash-separated path rel below root (a directory
// without symlinks of its own) as resolveInside does, and returns where it
// ends, slash-separated below root, and whether every component exists.
// A ".", ".." or empty component after a file ("file/", "file/."), which
// the kernel refuses with ENOTDIR, makes the path missing, so that such a
// symlink gets the empty digest, as libmamba records for it.
func resolveIn(root, rel string) (final string, exists bool, err error) {
	var cur []string // resolved components below root
	todo := strings.Split(rel, "/")
	hops, missing, file := 0, false, false
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			if file {
				missing = true
			}
			continue
		case "..":
			if len(cur) == 0 {
				return "", false, fmt.Errorf("%w: resolves outside the prefix", link.ErrUnsafePath)
			}
			if file {
				missing = true // ENOTDIR too
			}
			cur, file = cur[:len(cur)-1], false
			continue
		}
		cur = append(cur, c)
		if missing {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(strings.Join(cur, "/")))
		fi, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			missing = true
			continue
		}
		if err != nil {
			return "", false, err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			file = !fi.IsDir()
			continue
		}
		if hops++; hops > maxSymlinkHops {
			return "", false, fmt.Errorf("%w: more than %d levels of symbolic links", link.ErrUnsafePath, maxSymlinkHops)
		}
		target, err := os.Readlink(abs)
		if err != nil {
			return "", false, err
		}
		if target == "" || strings.HasPrefix(target, "/") || filepath.IsAbs(target) || filepath.VolumeName(target) != "" ||
			runtime.GOOS == "windows" && strings.Contains(target, `\`) {
			return "", false, fmt.Errorf("%w: %s points to %q", link.ErrUnsafePath, strings.Join(cur, "/"), target)
		}
		cur = cur[:len(cur)-1]
		todo = append(strings.Split(target, "/"), todo...)
	}
	return strings.Join(cur, "/"), !missing, nil
}
