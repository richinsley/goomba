package goomba

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/richinsley/goomba/internal/safedir"
)

// incompleteMarker is the file conda-meta/.goomba-incomplete. openPrefix
// creates it right after conda-meta, before anything else is written;
// commit removes it as the last change to the prefix, while the lock is
// still held; emptyPrefix makes sure it exists before it removes anything
// and removes it only once nothing else is left. A prefix that a crash
// left behind, at any point, is therefore recognisable as incomplete (see
// prefixState.interrupted).
const incompleteMarker = ".goomba-incomplete"

// prefixTx is a prefix InstallLock is creating. It holds the prefix lock
// from openPrefix until commit or rollback.
type prefixTx struct {
	path    string
	created bool        // this call created the prefix directory
	parents []string    // directories created above path, outermost first
	dir     *os.File    // the open (and on Unix flock-ed) prefix directory
	id      fs.FileInfo // identity of the prefix directory
	// mode is the prefix directory's mode once the transaction ends: 0755
	// for a prefix this call created, the previous mode of an existing one.
	// Until then the directory is 0700, so that nobody else can reach
	// anything in it while it is being filled, whatever the umask made of
	// the directories link and the interpreter create.
	mode fs.FileMode
}

// checkPrefixFree reports whether prefix is free for InstallLock: missing,
// or an empty directory. openPrefix checks again under the lock; this early
// check only avoids downloading for a prefix that cannot be used.
func checkPrefixFree(prefix string) error {
	fi, err := os.Lstat(prefix) // resolved: a symlink here is not a directory of the user's
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("goomba: prefix %s: %w", prefix, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("goomba: prefix %s: %w", prefix, ErrPrefixNotEmpty)
	}
	f, err := os.Open(prefix)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // removed meanwhile (a RemoveIncomplete)
	}
	if err != nil {
		return fmt.Errorf("goomba: prefix %s: %w", prefix, err)
	}
	defer f.Close()
	if names, err := f.Readdirnames(1); len(names) > 0 {
		return fmt.Errorf("goomba: prefix %s: %w", prefix, ErrPrefixNotEmpty)
	} else if err != nil && err != io.EOF && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("goomba: prefix %s: %w", prefix, err)
	}
	return nil
}

// openPrefix creates prefix (and missing parents) unless it is an existing
// empty directory, locks it, checks under the lock that it is still empty,
// and creates conda-meta with the incomplete marker in it. On error nothing
// this call created is left behind, and nothing it did not create is
// touched.
func openPrefix(prefix string) (_ *prefixTx, err error) {
	tx := &prefixTx{path: prefix}
	fail := func(err error) error { return fmt.Errorf("goomba: prefix %s: %w", prefix, err) }
	if err := mkdirAllRecorded(filepath.Dir(prefix), &tx.parents); err != nil {
		tx.removeParents()
		return nil, fail(err)
	}
	// The directories made just now, and those above them, must still be
	// safe: nothing was redirected meanwhile.
	if err := safedir.CheckAncestors(prefix); err != nil {
		tx.removeParents()
		return nil, prefixError(prefix, err)
	}
	switch err := os.Mkdir(prefix, 0o700); {
	case err == nil:
		tx.created = true
	case errors.Is(err, fs.ErrExist):
	default:
		tx.removeParents()
		return nil, fail(err)
	}
	// Until the lock is held the directory may be another process's: on a
	// failure here, remove only an empty directory this call created.
	abandon := func() {
		if tx.dir != nil {
			unlockDir(tx.dir)
			tx.dir = nil
		}
		if tx.created {
			_ = rmdir(prefix)
		}
		tx.removeParents()
	}
	d, err := lockDir(prefix)
	if err != nil {
		if errors.Is(err, ErrPrefixLocked) {
			tx.created = false // another process uses it now
		}
		abandon()
		return nil, fail(err)
	}
	tx.dir = d
	fi, err := d.Stat()
	if err != nil {
		abandon()
		return nil, fail(err)
	}
	if !fi.IsDir() {
		abandon()
		return nil, fail(ErrPrefixNotEmpty)
	}
	if st, err := os.Lstat(prefix); err != nil || !os.SameFile(st, fi) {
		tx.created = false
		abandon()
		return nil, fail(errors.New("prefix was replaced while it was being locked"))
	}
	if err := safedir.CheckOwnDir(prefix, fi); err != nil {
		tx.created = false
		abandon()
		return nil, prefixError(prefix, err)
	}
	if names, err := d.Readdirnames(1); len(names) > 0 || err != nil && err != io.EOF {
		// Not ours to remove, even if this call made the directory.
		tx.created = false
		abandon()
		if err != nil && err != io.EOF {
			return nil, fail(err)
		}
		return nil, fail(ErrPrefixNotEmpty)
	}
	tx.id = fi
	tx.mode = 0o755
	if !tx.created {
		tx.mode = fi.Mode().Perm()
	}

	// From here on the prefix is this call's: roll back on failure.
	defer func() {
		if err != nil {
			if rerr := tx.rollback(); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if err := setDirMode(tx.dir, 0o700); err != nil {
		return nil, fail(err)
	}
	meta := filepath.Join(prefix, "conda-meta")
	if err := os.Mkdir(meta, 0o755); err != nil {
		return nil, fail(err)
	}
	f, err := os.OpenFile(filepath.Join(meta, incompleteMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return nil, fail(err)
	}
	_, werr := io.WriteString(f, "goomba is creating this environment; if no goomba process is running, it is incomplete and may be removed\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fail(werr)
	}
	return tx, nil
}

// commit gives the prefix directory its final mode, removes the incomplete
// marker and releases the lock. If either fails the lock is kept, for
// rollback; the marker goes last, so a failure (or a crash) before it
// leaves the prefix marked.
func (tx *prefixTx) commit() error {
	if err := setDirMode(tx.dir, tx.mode); err != nil {
		return fmt.Errorf("goomba: prefix %s: %w", tx.path, err)
	}
	if err := os.Remove(filepath.Join(tx.path, "conda-meta", incompleteMarker)); err != nil {
		return fmt.Errorf("goomba: prefix %s: remove %s: %w", tx.path, incompleteMarker, err)
	}
	unlockDir(tx.dir)
	tx.dir = nil
	return nil
}

// rollback empties the locked prefix directory (it was empty when this
// call took it over) through its handle, removes the directory itself if
// this call created it and it is still at its path, then releases the lock.
// On Unix the removal works relative to the handle, so a directory renamed
// or swapped for a symlink meanwhile, above or inside the prefix, cannot
// redirect it to other data. The prefix is emptied in emptyPrefix's order,
// so a rollback that is itself killed leaves a prefix CheckComplete
// reports as ErrIncomplete, never files without the marker.
//
// If the prefix cannot be emptied (a mount point inside it, a file that
// cannot be removed), rollback returns why, wrapping ErrMountPoint for a
// mount point; the prefix then keeps its marker, and RemoveIncomplete can
// remove it once the cause is gone.
func (tx *prefixTx) rollback() error {
	defer func() {
		if tx.dir != nil {
			unlockDir(tx.dir)
			tx.dir = nil
		}
	}()
	if tx.dir == nil {
		return nil
	}
	var err error
	for range 3 { // a process killed during the rollback may still be writing
		if err = emptyDir(tx.dir); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("goomba: rollback of prefix %s left it incomplete: %w", tx.path, err)
	}
	if tx.created {
		// Only if it is still the directory this call made, and with
		// rmdir semantics: a path swapped meanwhile costs nothing.
		_ = rmdirSame(tx.path, tx.id)
		tx.removeParents()
		return nil
	}
	// An existing prefix gets back its mode, which passed safedir.CheckOwnDir.
	_ = setDirMode(tx.dir, tx.mode)
	return nil
}

// removeParents removes the parent directories this call created, innermost
// first, as long as they are empty.
func (tx *prefixTx) removeParents() {
	for i := len(tx.parents) - 1; i >= 0; i-- {
		if rmdir(tx.parents[i]) != nil {
			break
		}
	}
	tx.parents = nil
}

// mkdirAllRecorded creates dir and any missing parents, appending each
// directory it creates to *created, outermost first.
func mkdirAllRecorded(dir string, created *[]string) error {
	var missing []string
	for d := filepath.Clean(dir); ; {
		if fi, err := os.Lstat(d); err == nil {
			// dir was resolved: a symlink here appeared since.
			if !fi.IsDir() {
				return fmt.Errorf("%w: %s is not a directory (the path changed)", ErrUnsafePrefix, d)
			}
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		up := filepath.Dir(d)
		if up == d {
			break
		}
		d = up
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return err
		}
		*created = append(*created, missing[i])
	}
	return nil
}
