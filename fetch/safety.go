package fetch

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/richinsley/goomba/internal/safedir"
)

// The cache-directory rule (owner, StrictModes-style ancestors, the
// private-group exception) lives in internal/safedir, shared with the
// index cache of package repodata.

// unsafeCache marks a safedir refusal as an ErrUnsafeCache error; other
// errors are returned unchanged.
func unsafeCache(err error) error {
	if errors.Is(err, safedir.ErrUnsafe) {
		return fmt.Errorf("%w: %w", ErrUnsafeCache, err)
	}
	return err
}

// checkCacheDir refuses a resolved cache directory that a user other than
// the current one could change (safedir.CheckDir). Otherwise another user
// could rename a content directory or extracted tree of theirs into the
// cache, holding a copy of a marker it can read, and have unverified bytes
// installed.
func checkCacheDir(dir string) error { return unsafeCache(safedir.CheckDir(dir)) }

// checkPrivate refuses a cache entry (described by fi, from Lstat) that does
// not belong to the effective user or that group or others may write
// (safedir.CheckEntry).
func checkPrivate(p string, fi fs.FileInfo) error { return unsafeCache(safedir.CheckEntry(p, fi)) }
