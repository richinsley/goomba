package goomba

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
)

// prefixFS is what emptyPrefix needs from an open prefix directory. Names
// are relative to the prefix: "" is the prefix itself, "conda-meta" and
// "conda-meta/<name>" the entries below it. Nothing follows a symlink. The
// Unix implementation works relative to the open directory handle; others
// work by path.
type prefixFS interface {
	// names lists the entries of the directory dir ("" or "conda-meta").
	names(dir string) ([]string, error)
	// removeAll removes the entry rel and everything below it; a missing
	// entry is not an error.
	removeAll(rel string) error
	// ensureMarker creates conda-meta, unless it is a directory already,
	// and the incomplete marker in it, unless it is there already.
	ensureMarker() error
}

// maxEmptyPasses bounds how often emptyPrefix lists the prefix again when
// entries keep appearing (a killed installation's interpreter may still be
// writing).
const maxEmptyPasses = 10

// emptyPrefix removes everything inside a prefix in an order that leaves
// it marked as incomplete wherever the removal is interrupted (a kill, a
// power cut): it first makes sure conda-meta/.goomba-incomplete exists,
// then removes everything but conda-meta, then everything in conda-meta
// but the marker, then the marker and last conda-meta itself. The marker
// goes only once nothing else is left; between it and conda-meta, the
// prefix holds only an empty conda-meta, which prefixState.interrupted
// also counts as incomplete. A prefix without conda-meta gets one first:
// only an empty one lacks it here (openPrefix creates conda-meta before
// anything else, and RemoveIncomplete removes only interrupted prefixes).
// If the marker cannot be made because conda-meta is not a directory (a
// file, a symlink), everything is removed in any order; any other failure
// to make it (EMFILE, EIO, ENOSPC, conda-meta a mount point) is returned
// before anything is removed. It returns the first error; the marker then
// stays. A mount point inside the prefix is an error (see package rmtree)
// that leaves it, and the marker, in place.
func emptyPrefix(p prefixFS) error {
	marked := true
	if err := p.ensureMarker(); err != nil {
		if !isNotADir(err) {
			return err
		}
		marked = false
	}
	removeExcept := func(dir, keep string) error {
		for pass := 0; ; pass++ {
			names, err := p.names(dir)
			if err != nil {
				return err
			}
			slices.Sort(names)
			names = slices.DeleteFunc(names, func(n string) bool { return n == keep })
			if len(names) == 0 {
				return nil
			}
			if pass >= maxEmptyPasses {
				return fmt.Errorf("emptying %s: it keeps getting new entries", orDefault(dir, "the prefix"))
			}
			var first error
			for _, n := range names {
				rel := n
				if dir != "" {
					rel = dir + "/" + n
				}
				if err := p.removeAll(rel); err != nil && first == nil {
					first = err
				}
			}
			if first != nil && pass >= 2 {
				return first
			}
		}
	}
	if !marked {
		return removeExcept("", "")
	}
	if err := removeExcept("", "conda-meta"); err != nil {
		return err
	}
	if err := removeExcept("conda-meta", incompleteMarker); err != nil {
		return err
	}
	// Something may have appeared beside conda-meta meanwhile.
	if err := removeExcept("", "conda-meta"); err != nil {
		return err
	}
	if err := p.removeAll("conda-meta/" + incompleteMarker); err != nil {
		return err
	}
	if err := p.removeAll("conda-meta"); err != nil {
		_ = p.ensureMarker() // something appeared in it: mark it again
		return err
	}
	return nil
}

// orDefault returns s, or alt when s is empty.
func orDefault(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// prefixState is what CheckComplete and RemoveIncomplete read from a prefix
// directory.
type prefixState struct {
	entries     int  // entries of the prefix directory, counted up to 2
	meta        bool // conda-meta is a directory
	metaEntries int  // entries of conda-meta, counted up to 1
	marker      bool // conda-meta holds the incomplete marker
	history     bool // conda-meta holds the history
}

// interrupted reports a goomba transaction that never finished: the marker
// is present, or the prefix holds nothing but an empty conda-meta. Only a
// goomba process killed between creating conda-meta and the marker (in
// InstallLock), or between removing the marker and conda-meta (when
// emptying a prefix), leaves that; micromamba and conda write
// conda-meta/history when they create conda-meta.
func (s prefixState) interrupted() bool {
	return s.marker || s.meta && s.entries == 1 && s.metaEntries == 0
}

// verdict is CheckComplete's answer for a prefix in state s, read while
// nothing held the prefix lock.
func (s prefixState) verdict() error {
	switch {
	case s.entries == 0:
		return fmt.Errorf("not an environment: empty directory: %w", fs.ErrNotExist)
	case s.interrupted():
		return ErrIncomplete
	case !s.meta:
		return fmt.Errorf("not an environment: no conda-meta directory: %w", fs.ErrNotExist)
	case !s.history:
		return fmt.Errorf("not a finished environment: conda-meta has no history: %w", fs.ErrNotExist)
	}
	return nil
}

// errNotADir is the error for a prefix or conda-meta that is not a
// directory.
var errNotADir = errors.New("not a directory")
