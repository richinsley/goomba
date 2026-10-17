// Ported from libmamba src/core/link.cpp (UnlinkPackage::execute,
// UnlinkPackage::unlink_path) (BSD-3-Clause, see NOTICE).

package link

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/richinsley/goomba/record"
)

// Unlink removes an installed package from prefix: every path listed in its
// conda-meta record (paths_data, or the legacy files list), then each
// directory left empty on the way up to the prefix, then the record itself.
// Paths from the record are confined to the prefix like Link's; symlinks
// are removed, never followed. Missing files are ignored. If any path cannot
// be removed, Unlink reports all failures and keeps the conda-meta record so
// that the operation can be retried. Package scripts are never run.
func Unlink(ctx context.Context, prefix string, rec *record.PackageRecord) error {
	if rec == nil {
		return errors.New("link: unlink: nil package record")
	}
	dist := rec.Dist()
	fail := func(err error) error { return fmt.Errorf("link: unlink %s: %w", dist, err) }
	if err := checkRecord(rec); err != nil {
		return fail(err)
	}
	prefix, err := checkPrefix(prefix)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	pr := newRoot(prefix)
	metaPath, err := pr.path("conda-meta/" + dist + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return fail(ErrNotInstalled)
	}
	if err != nil {
		return fail(err)
	}
	cm, err := readCondaMetaFile(metaPath)
	if errors.Is(err, fs.ErrNotExist) {
		return fail(ErrNotInstalled)
	}
	if err != nil {
		return fail(err)
	}
	paths := cm.Files
	if len(cm.Paths) > 0 {
		paths = make([]string, len(cm.Paths))
		for i, p := range cm.Paths {
			paths[i] = p.Path
		}
	}

	if cm.Record.Dist() != dist {
		return fail(fmt.Errorf("%w: conda-meta record describes %s", ErrInvalidPackage, cm.Record.Dist()))
	}
	u := &unlinker{root: pr, guard: newGuard(pr), prefix: prefix}
	var errs []error
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := u.unlinkPath(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	if len(errs) > 0 {
		return fail(errors.Join(errs...))
	}
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	return nil
}

type unlinker struct {
	root   *root
	guard  *guard
	prefix string
	python *PythonInfo // loaded lazily for short noarch paths
	pyErr  error
}

func (u *unlinker) unlinkPath(p string) error {
	if err := validRelPath(p); err != nil {
		return err
	}
	if first, _, _ := strings.Cut(p, "/"); strings.EqualFold(first, "conda-meta") {
		return fmt.Errorf("%w: %q is inside conda-meta", ErrUnsafePath, p)
	}
	_, err := u.root.lstat(p)
	if errors.Is(err, fs.ErrNotExist) && (strings.HasPrefix(p, "site-packages/") || strings.HasPrefix(p, "python-scripts/")) {
		// Older conda-meta records of noarch: python packages store the
		// package-relative path; resolve it like libmamba does.
		if py := u.pythonInfo(); py != nil {
			p = NoarchTargetPath(p, py.SitePackagesPath, py.BinDir)
		}
	}
	dir, parent, leaf, err := u.root.parentResolved(p, resolveExisting, nil, nil)
	if errors.Is(err, fs.ErrNotExist) {
		u.pruneParents(p)
		return nil
	}
	if err != nil {
		return err
	}
	if full := append(parent, leaf); u.guard.inCondaMeta(full) {
		return fmt.Errorf("%w: resolves into conda-meta (%s)", ErrUnsafePath, strings.Join(full, "/"))
	}
	abs := filepath.Join(dir, leaf)
	fi, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		u.pruneParents(p)
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if !fi.IsDir() {
			return err
		}
		// A directory entry that still has content stays.
	}
	u.pruneParents(p)
	return nil
}

// pruneParents removes the now-empty directories above the prefix-relative
// path p, like libmamba's unlink_path: missing levels are skipped, and the
// walk stops at the first directory that is not empty or not a real
// directory, or at the prefix.
func (u *unlinker) pruneParents(p string) {
	for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
		abs, err := u.root.path(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return
		}
		fi, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !fi.IsDir() || os.Remove(abs) != nil {
			return
		}
	}
}

// pythonInfo derives the environment's python layout from its conda-meta.
func (u *unlinker) pythonInfo() *PythonInfo {
	if u.python != nil || u.pyErr != nil {
		return u.python
	}
	recs, err := ReadCondaMeta(u.prefix)
	if err != nil {
		u.pyErr = err
		return nil
	}
	for _, r := range recs {
		if r.Record.Name == "python" {
			u.python, u.pyErr = NewPythonInfo(r.Record, r.Record.Subdir)
			return u.python
		}
	}
	u.pyErr = errors.New("no python package installed")
	return nil
}
