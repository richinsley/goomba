package goomba

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/richinsley/goomba/internal/safedir"
)

// CheckComplete reports whether prefix holds a finished environment, for a
// caller that reuses existing environments and must tell one that another
// process is still installing (or removing) from one that a killed process
// left behind; both hold conda-meta/.goomba-incomplete. It returns:
//
//   - nil: the prefix holds conda-meta with a history and no incomplete
//     marker, and no goomba process holds its lock. Environments micromamba
//     or conda made count too (their own locks are not consulted).
//   - ErrPrefixLocked: an InstallLock or a RemoveIncomplete, in this
//     process or another, holds the prefix lock: the prefix is being
//     installed, rolled back or removed.
//   - ErrIncomplete: a goomba transaction was interrupted and nobody holds
//     the lock: the marker is present, or the prefix holds nothing but an
//     empty conda-meta (what a process killed just after creating
//     conda-meta, or just before removing it, leaves). The prefix is
//     unusable; RemoveIncomplete removes it.
//   - an error wrapping fs.ErrNotExist for a prefix that is not an
//     environment: it is missing or an empty directory (InstallLock can
//     create an environment there), or it holds no conda-meta, or a
//     conda-meta with neither the marker nor a history.
//   - an error wrapping fs.ErrExist for a prefix that is a dangling
//     symlink (as mkdir(2) would report).
//   - ErrUnsafePrefix when the prefix or a directory on the way to it could
//     be changed by another user, by the rules InstallLock applies (see
//     package internal/safedir); ErrInvalidOptions for an empty prefix;
//     other errors (ENOTDIR, a conda-meta that is not a directory) as
//     they are.
//
// The prefix is resolved as InstallLock resolves it. CheckComplete creates,
// removes and modifies nothing. Unless the prefix directory is empty, it
// takes the prefix lock, shared and without blocking, then reads the prefix
// again and decides from that second reading: no InstallLock or
// RemoveIncomplete can hold the lock or take it meanwhile, so the marker it
// sees is not one a commit is about to remove. The lock is shared, so that
// concurrent checks do not see each other, and an InstallLock or
// RemoveIncomplete that meets a check holding it waits for it (about 50 ms
// at most) instead of failing. An empty directory, the only kind
// InstallLock takes over, is never locked, so a check cannot delay an
// installation that is starting; an InstallLock that has just taken an
// empty prefix, and not yet created conda-meta, is therefore reported as
// not an environment. CheckComplete is safe for concurrent use.
//
// The answer describes one instant and can change as soon as it is
// returned: a running installation can finish or fail, and another process
// can remove the prefix. An installation downloads its packages before it
// takes the prefix, so while another process is still downloading, a prefix
// it is about to create is reported missing; two callers that both decide
// to create it are kept apart by InstallLock (one gets ErrPrefixLocked or
// ErrPrefixNotEmpty). ErrIncomplete stays true until the prefix is removed,
// since InstallLock refuses a prefix that is not empty; RemoveIncomplete
// removes it under the prefix lock, so callers need not agree among
// themselves who does.
//
// A goomba process killed while it byte-compiles leaves the interpreter it
// started running for a moment: it may still write .pyc files into the
// prefix after CheckComplete reports ErrIncomplete (RemoveIncomplete lists
// the prefix again until it stays empty).
//
// Without flock (Windows) there is no prefix lock: a prefix with the marker
// is ErrIncomplete even while an installation is running, so a caller there
// must not remove it on that answer alone (RemoveIncomplete refuses to,
// with errors.ErrUnsupported).
func CheckComplete(prefix string) error {
	return checkComplete(prefix, nil)
}

// checkComplete is CheckComplete; beforeLock, if set, runs between the
// first reading and the locked one (tests use it to change the prefix
// there).
func checkComplete(prefix string, beforeLock func()) error {
	if prefix == "" {
		return fmt.Errorf("goomba: %w: empty prefix", ErrInvalidOptions)
	}
	resolved, err := safedir.ResolvePath(prefix)
	if err != nil {
		return prefixError(prefix, err)
	}
	fail := func(err error) error { return fmt.Errorf("goomba: prefix %s: %w", resolved, err) }
	d, _, err := openPrefixDir(resolved)
	if err != nil {
		return err
	}
	defer unlockDir(d)
	st, err := readState(d)
	if err != nil {
		return fail(err)
	}
	if st.entries == 0 {
		return fail(st.verdict())
	}
	if beforeLock != nil {
		beforeLock()
	}
	if err := tryLockShared(d); err != nil {
		return fail(err)
	}
	if st, err = readState(d); err != nil {
		return fail(err)
	}
	if err := st.verdict(); err != nil {
		return fail(err)
	}
	return nil
}

// openPrefixDir opens the resolved prefix directory, without following a
// symlink there and without locking it, and checks that it is a directory
// of the user's own (safedir.CheckOwnDir). Errors are wrapped for the
// caller to return.
func openPrefixDir(resolved string) (*os.File, os.FileInfo, error) {
	fail := func(err error) error { return fmt.Errorf("goomba: prefix %s: %w", resolved, err) }
	d, err := openDirNoFollow(resolved)
	if err != nil {
		return nil, nil, fail(err)
	}
	fi, err := d.Stat()
	switch {
	case err != nil:
		err = fail(err)
	case !fi.IsDir():
		err = fail(errNotADir)
	default:
		if err = safedir.CheckOwnDir(resolved, fi); err != nil {
			err = prefixError(resolved, err)
		}
	}
	if err != nil {
		unlockDir(d)
		return nil, nil, err
	}
	return d, fi, nil
}

// RemoveIncomplete removes a prefix that holds an interrupted goomba
// transaction, one CheckComplete reports as ErrIncomplete. It needs the
// prefix lock (flock), so on platforms without it (Windows) it returns an
// error wrapping errors.ErrUnsupported before it looks at anything: there
// it could not tell an interrupted installation from a running one.
//
// It resolves prefix as InstallLock does, reads it, and removes nothing
// from an empty directory (which it does not lock, so that it cannot
// delay an InstallLock taking it). Otherwise it takes the prefix lock
// exclusively, as InstallLock does: ErrPrefixLocked while another
// InstallLock or RemoveIncomplete holds it; status checks are waited for
// briefly, but under a stream of them it may still get ErrPrefixLocked,
// and may simply be called again. Under the lock it checks that the prefix
// still holds an interrupted transaction; if not, it removes nothing and
// returns an error that wraps neither ErrIncomplete nor ErrPrefixLocked (it
// wraps fs.ErrNotExist for a prefix that is not an environment).
//
// Then it empties the prefix through the locked directory handle with
// package internal/rmtree: it never follows a symlink and never enters a
// mount point inside the prefix (a volume mounted there is neither
// emptied nor chmod-ed: the removal stops with an error wrapping
// rmtree.ErrMountPoint that names it, and leaves the prefix marked); and
// it keeps the marker until nothing else is left, so that a removal that
// is itself killed still leaves a prefix CheckComplete reports as
// ErrIncomplete. Last it removes the prefix directory, with rmdir
// semantics and only if it is still the directory it locked, except when
// the prefix is itself a mount point (EBUSY: emptied, kept) or the given
// path reached it through a symlink (emptied, kept, so that the symlink
// still leads to a directory InstallLock can fill). A missing prefix is an
// error wrapping fs.ErrNotExist; a prefix that is not the user's own
// directory is ErrUnsafePrefix.
//
// RemoveIncomplete is safe for concurrent use: calls for the same prefix,
// and InstallLock, exclude each other through the prefix lock, so of two
// callers that both saw ErrIncomplete one removes the prefix and the other
// gets ErrPrefixLocked or, once the prefix is gone, an error wrapping
// fs.ErrNotExist.
func RemoveIncomplete(prefix string) error {
	if !havePrefixLock {
		return fmt.Errorf("goomba: remove interrupted prefix %s: %w: no prefix lock on this platform", prefix, errors.ErrUnsupported)
	}
	if prefix == "" {
		return fmt.Errorf("goomba: %w: empty prefix", ErrInvalidOptions)
	}
	abs, err := filepath.Abs(prefix)
	if err != nil {
		return fmt.Errorf("goomba: prefix %s: %w", prefix, err)
	}
	resolved, err := safedir.ResolvePath(prefix)
	if err != nil {
		return prefixError(prefix, err)
	}
	return removeIncomplete(resolved, resolved != abs)
}

// removeIncomplete is RemoveIncomplete for the resolved prefix path; with
// keepDir set it leaves the emptied prefix directory in place.
func removeIncomplete(resolved string, keepDir bool) error {
	fail := func(err error) error { return fmt.Errorf("goomba: prefix %s: %w", resolved, err) }
	d, fi, err := openPrefixDir(resolved)
	if err != nil {
		return err
	}
	defer unlockDir(d)
	st, err := readState(d)
	if err != nil {
		return fail(err)
	}
	if st.entries == 0 {
		return fail(fmt.Errorf("not removed: it holds no interrupted installation: %w", st.verdict()))
	}
	if err := lockOpenDir(d); err != nil {
		return fail(err)
	}
	if now, err := os.Lstat(resolved); err != nil || !os.SameFile(now, fi) {
		return fail(errors.New("prefix was replaced while it was being locked"))
	}
	if st, err = readState(d); err != nil {
		return fail(err)
	}
	if !st.interrupted() {
		if v := st.verdict(); v != nil {
			return fail(fmt.Errorf("not removed: it holds no interrupted installation: %w", v))
		}
		return fail(errors.New("not removed: it holds a finished environment"))
	}
	for range 3 {
		if err := emptyDir(d); err != nil {
			return fail(err) // the marker stays
		}
		if keepDir {
			return nil
		}
		switch err = rmdirSame(resolved, fi); {
		case err == nil:
			return nil
		case isBusy(err):
			return nil // the prefix is a mount point: emptied, kept
		case !isNotEmpty(err):
			return fail(err)
		}
		// Something appeared since it was emptied: empty it again.
	}
	_ = markIncomplete(d)
	return fail(err)
}
