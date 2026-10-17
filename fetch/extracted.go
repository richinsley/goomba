// Extraction flow follows libmamba src/core/package_fetcher.cpp
// (PackageFetcher::extract: unpack into a fresh directory, then record it)
// (BSD-3-Clause, see NOTICE).

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/richinsley/goomba/archive"
	"github.com/richinsley/goomba/record"
)

const (
	// markerName is written last into an extracted package directory.
	markerName = ".goomba-extracted"
	// keyName holds the cache's MAC key, 32 random bytes in hex.
	keyName = ".goomba-key"
)

// markerContent is the exact content of a valid marker for the tree
// extracted from the archive with digest sha. The MAC binds it to this
// cache's key, so a marker shipped inside a package, or planted by a
// process that cannot read the key, is never trusted.
func markerContent(key []byte, sha string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("goomba-extracted 2\x00" + treeName + "\x00" + sha))
	return []byte("goomba-extracted 2\nsha256 " + sha + "\nmac " + hex.EncodeToString(m.Sum(nil)) + "\n")
}

// Extracted fetches and unpacks rec, returning <Dir>/<rec.SHA256>/pkg. The
// archive is extracted from the very file whose digest was verified, under
// the lock for rec.SHA256, so no other bytes can be substituted. An
// existing tree is used only if its .goomba-extracted marker matches
// rec.SHA256 and this cache's key; any other tree there is replaced.
// Extracted data is synced to disk before the marker is written (on
// Windows, directories and read-only files cannot be flushed and are
// skipped). rec.Fn, if set, and the URL's file name must both be
// rec.Dist() plus ".conda" or ".tar.bz2".
func (c *Cache) Extracted(ctx context.Context, rec *record.PackageRecord) (string, error) {
	u, fn, err := c.checkRecord(rec)
	if err != nil {
		return "", err
	}
	var tree string
	err = c.withLock(ctx, rec.SHA256, func(dir string, key []byte) error {
		tree = filepath.Join(dir, treeName)
		removeStaleExtractions(dir)
		if trusted(tree, key, rec.SHA256) {
			return nil
		}
		// Another process may have replaced the key; look once.
		if k, ok := c.reloadKey(key); ok {
			if key = k; trusted(tree, key, rec.SHA256) {
				return nil
			}
		}
		f, err := c.openArchive(ctx, u, dir, fn, rec.SHA256, rec.Size, key)
		if err != nil {
			return err
		}
		defer f.Close()
		return c.extract(ctx, f, fn, dir, key, rec.SHA256)
	})
	if err != nil {
		return "", fmt.Errorf("fetch: extract %s: %w", rec.Dist(), err)
	}
	return tree, nil
}

// checkRecord validates the fields of rec that Extracted uses and returns
// its parsed URL and archive file name.
func (c *Cache) checkRecord(rec *record.PackageRecord) (*url.URL, string, error) {
	if rec == nil {
		return nil, "", fmt.Errorf("fetch: %w: nil record", ErrInvalidInput)
	}
	dist := rec.Dist()
	if rec.Name == "" || rec.Version == "" || rec.Build == "" || !safeName(dist) {
		return nil, "", fmt.Errorf("fetch: %w: record %s does not name a safe package", ErrInvalidInput, quote(dist))
	}
	u, fn, err := c.checkFetch(rec.URL, rec.SHA256, rec.Size)
	if err != nil {
		return nil, "", err
	}
	if fn != dist+".conda" && fn != dist+".tar.bz2" {
		return nil, "", fmt.Errorf("fetch: %w: url file name %s does not match record %s", ErrInvalidInput, quote(fn), quote(dist))
	}
	if rec.Fn != "" && rec.Fn != fn {
		return nil, "", fmt.Errorf("fetch: %w: record fn %s does not match url file name %s", ErrInvalidInput, quote(rec.Fn), quote(fn))
	}
	return u, fn, nil
}

// extract unpacks the verified archive open as f into <dir>/pkg. The
// archive is read only through f, which must be in the state it was
// verified in both before and after extraction.
func (c *Cache) extract(ctx context.Context, f *archiveFile, fn, dir string, key []byte, sha string) error {
	before, err := stampOf(f.File)
	if err != nil {
		return err
	}
	if before != f.verified {
		return fmt.Errorf("%w: archive %s changed after it was verified", ErrUnsafeCache, fn)
	}
	r, err := randomHex()
	if err != nil {
		return err
	}
	staging := filepath.Join(dir, "."+treeName+".staging-"+r)
	if err := archive.ExtractReaderAt(ctx, f, before.size, fn, staging, c.Limits); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			forceRemoveAll(staging)
		}
	}()
	after, err := stampOf(f.File)
	if err != nil {
		return err
	}
	if after != f.verified {
		return fmt.Errorf("%w: archive %s changed during extraction", ErrUnsafeCache, fn)
	}
	if err := writeMarker(ctx, staging, key, sha); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tree := filepath.Join(dir, treeName)
	if _, err := os.Lstat(tree); err == nil {
		// An untrusted tree (corrupt, tampered or planted): nothing was
		// ever returned for it, so it can be moved aside and removed.
		r, err := randomHex()
		if err != nil {
			return err
		}
		old := filepath.Join(dir, "."+treeName+".old-"+r)
		if err := os.Rename(tree, old); err != nil {
			return err
		}
		forceRemoveAll(old)
	}
	if err := renameNoReplace(staging, tree); err != nil {
		return err
	}
	done = true
	syncDir(dir)
	return nil
}

// trusted reports whether tree is a real directory holding a valid marker
// for sha.
func trusted(tree string, key []byte, sha string) bool {
	fi, err := os.Lstat(tree)
	if err != nil || !fi.IsDir() {
		return false
	}
	b, err := readSmall(filepath.Join(tree, markerName), 512)
	return err == nil && hmac.Equal(b, markerContent(key, sha))
}

// writeMarker syncs the extracted tree, then records it as complete.
func writeMarker(ctx context.Context, dir string, key []byte, sha string) error {
	p := filepath.Join(dir, markerName)
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("%w: package contains the reserved path %s", archive.ErrFormat, markerName)
	}
	if err := syncTree(ctx, dir); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(markerContent(key, sha))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// removeStaleExtractions removes what a crashed Extracted left in a
// content directory: its staging directory, the temporary directory
// archive.Extract made beside it, and a tree moved aside but not yet
// removed. The caller holds the lock, so none of them is in use.
func removeStaleExtractions(dir string) {
	removeStale(dir, "."+treeName+".staging-", "")
	removeStale(dir, "."+treeName+".old-", "")
	matches, _ := filepath.Glob(filepath.Join(dir, ".."+treeName+".staging-*"))
	for _, m := range matches {
		rest := strings.TrimPrefix(filepath.Base(m), ".."+treeName+".staging-")
		a, b, ok := strings.Cut(rest, ".tmp-")
		if ok && isRandomSuffix(a) && isRandomSuffix(b) {
			forceRemoveAll(m)
		}
	}
}

// forceRemoveAll removes a tree, making its directories writable first if
// a plain removal fails (a tree may hold read-only directories). Errors
// are ignored: a leftover is removed by a later stale-entry sweep.
func forceRemoveAll(p string) {
	if os.RemoveAll(p) == nil {
		return
	}
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(q, 0o755)
		}
		return nil
	})
	_ = os.RemoveAll(p)
}

// markerKey returns the cache's MAC key, creating Dir/.goomba-key with 32
// random bytes on first use. Creation links a complete temporary file into
// place, so concurrent processes agree on one key. On Unix the key file
// must belong to the current user and grant no access to others, so the
// cache must live on a filesystem with Unix permissions. Stop every goomba
// process using a cache before deleting its key: a process holding the old
// key in memory rereads it only when a MAC fails to verify.
func (c *Cache) markerKey() ([]byte, error) {
	c.mu.Lock()
	k := c.markKey
	c.mu.Unlock()
	if k != nil {
		return k, nil
	}
	p := filepath.Join(c.Dir, keyName)
	k, err := readKey(p)
	if errors.Is(err, fs.ErrNotExist) {
		k, err = createKey(c.Dir, p)
	}
	if err != nil {
		return nil, fmt.Errorf("key %s: %w", p, err)
	}
	c.mu.Lock()
	c.markKey = k
	c.mu.Unlock()
	return k, nil
}

// reloadKey rereads the key file and adopts it if it differs from old,
// reporting whether it did.
func (c *Cache) reloadKey(old []byte) ([]byte, bool) {
	k, err := readKey(filepath.Join(c.Dir, keyName))
	if err != nil || hmac.Equal(k, old) {
		return old, false
	}
	c.mu.Lock()
	c.markKey = k
	c.mu.Unlock()
	return k, true
}

// readKey reads the key through one descriptor, opened without following a
// symlink, whose owner and mode are checked before it is read.
func readKey(p string) ([]byte, error) {
	f, err := openNoFollow(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkKeyFile(fi); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil {
		return nil, err
	}
	s, ok := strings.CutSuffix(string(b), "\n")
	if !ok || !isHexDigest(s) {
		return nil, errors.New("malformed key file")
	}
	return hex.DecodeString(s)
}

func createKey(dir, p string) ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	f, tmp, err := createUnique(dir, keyName+".tmp-", 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	_, err = f.WriteString(hex.EncodeToString(k) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Link(tmp, p); errors.Is(err, fs.ErrExist) {
		return readKey(p) // another process won the race
	} else if err != nil {
		return nil, err
	}
	syncDir(dir)
	// Read it back: a filesystem without Unix permissions shows a key it
	// cannot protect, and that is refused now rather than on next use.
	if _, err := readKey(p); err != nil {
		return nil, err
	}
	return k, nil
}
