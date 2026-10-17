// Behaviour ported from libmamba src/core/package_fetcher.cpp and
// src/core/package_cache.cpp, and rattler crates/rattler_cache
// (BSD-3-Clause, see NOTICE), with a content-addressed layout.

package fetch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/richinsley/goomba/archive"
)

// Sentinel errors; failures wrap one of these, an *IntegrityError, a
// *SizeError, an *HTTPError, a context error or an I/O error.
var (
	// ErrInvalidInput reports an argument refused before any I/O: a sha256
	// that is not 64 lowercase hex digits, a URL or record field that does
	// not name a safe cache entry, a negative size.
	ErrInvalidInput = errors.New("invalid input")
	// ErrInsecureURL reports a URL, or a redirect target, that is not
	// https (or loopback http with AllowHTTP).
	ErrInsecureURL = errors.New("insecure url")
	// ErrUnsafeCache reports cache state goomba refuses to use: a marker
	// key owned by another user or readable or writable by others, a
	// symlinked lock directory or lock file, or an archive that changed
	// while it was being extracted.
	ErrUnsafeCache = errors.New("unsafe cache state")
)

// IntegrityError reports downloaded bytes whose sha256 differs from the
// expected digest. It is never retried. Like the other error types here it
// is immutable once returned and safe to share between goroutines.
type IntegrityError struct {
	URL      string // download URL, password redacted, truncated
	Expected string // lowercase hex sha256 asked for
	Actual   string // lowercase hex sha256 of the bytes received
}

// Error describes the mismatch, naming the URL and both digests.
func (e *IntegrityError) Error() string {
	return fmt.Sprintf("%s: sha256 mismatch: expected %s, got %s", e.URL, e.Expected, e.Actual)
}

// SizeError reports a download whose length differs from the expected size,
// or exceeds the cap applied when the size is unknown. It is not retried.
// It is immutable once returned.
type SizeError struct {
	URL      string // download URL, password redacted, truncated
	Expected int64  // expected size, or the cap when the size is unknown
	Actual   int64  // bytes announced or received; more than Expected when Over
	Over     bool   // the download was stopped for exceeding Expected
}

// Error describes the mismatch, naming the URL and both sizes.
func (e *SizeError) Error() string {
	if e.Over {
		return fmt.Sprintf("%s: size mismatch: expected %d bytes, got more", e.URL, e.Expected)
	}
	return fmt.Sprintf("%s: size mismatch: expected %d bytes, got %d", e.URL, e.Expected, e.Actual)
}

// HTTPError reports a response status other than 200 OK. It is immutable
// once returned.
type HTTPError struct {
	URL        string // request URL, password redacted, truncated
	StatusCode int
	Status     string
}

// Error names the URL and the response status.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: http status %s", e.URL, e.Status)
}

// Temporary reports whether retrying may help: 408, 413, 429 and 5xx
// (libmamba retries 413, 429 and 5xx).
func (e *HTTPError) Temporary() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		return true
	}
	return e.StatusCode >= 500
}

// maxNameLen leaves room within a 255-byte file name for the suffixes the
// cache appends (".partial-" plus 16 hex digits, ".sha256").
const maxNameLen = 200

// maxUnknownSize caps downloads whose size the caller does not know.
const maxUnknownSize = archive.DefaultMaxTotalSize

// treeName is the extracted package directory inside a content directory.
const treeName = "pkg"

// Cache is a content-addressed package cache directory. Set its fields
// before first use and do not copy it afterwards. A Cache is safe for
// concurrent use, and any number of processes may share one Dir: each
// archive is downloaded and each package extracted once, under an
// in-process mutex per sha256 plus a cross-process lock file.
//
// Layout, for an archive with sha256 S and file name fn:
//
//	<Dir>/<S>/<fn>           the verified archive (returned by Fetch)
//	<Dir>/<S>/<fn>.sha256    sidecar: digest, size, mtime, inode, change time, MAC
//	<Dir>/<S>/pkg/           the extracted package (returned by Extracted),
//	                         holding .goomba-extracted, written last
//	<Dir>/.goomba-key        32-byte key for sidecar and marker MACs (mode 0600)
//	<Dir>/.locks/<S>.lock    lock files
//
// A returned path never holds other bytes later. Dir must be writable only
// by its owner, on a filesystem with Unix permissions: a process able to
// write it can replace any file in it, and a process of the same user can
// also read the key. The marker vouches for a tree as extracted; it does not
// protect files edited afterwards. Stop every process using Dir before
// deleting .goomba-key.
type Cache struct {
	// Dir is the package cache directory. It is created if missing.
	Dir string
	// Client performs downloads. nil selects a client with connect, TLS
	// and response-header timeouts and no proxy (proxies come from the
	// caller's Client, never from the environment). Its CheckRedirect, if
	// any, runs after the cache's own https-only check.
	Client *http.Client
	// Retries is how many times a transient failure (network error,
	// stall, HTTP 408, 413, 429 or 5xx) is retried with exponential
	// backoff. 0 selects 3; a negative value disables retries.
	Retries int
	// AllowHTTP permits plain http to loopback addresses, for tests.
	AllowHTTP bool
	// Limits bounds extraction in Extracted; zero fields select the
	// archive package defaults.
	Limits archive.Limits

	mu       sync.Mutex
	locks    map[string]*keyLock
	client   *http.Client // default client, built on first use
	markKey  []byte       // MAC key, loaded on first use
	backoff0 int64        // first retry delay in ns; 0 selects defaultBackoff (tests)
}

// maxMessageLen bounds untrusted strings (URLs, record fields) quoted in
// error messages.
const maxMessageLen = 256

// quote formats an untrusted string for an error message, truncated.
func quote(s string) string {
	if len(s) > maxMessageLen {
		return strconv.Quote(s[:maxMessageLen]) + "..."
	}
	return strconv.Quote(s)
}

// redact formats u for an error message without its password, truncated.
func redact(u *url.URL) string {
	s := u.Redacted()
	if len(s) > maxMessageLen {
		return s[:maxMessageLen] + "..."
	}
	return s
}

// isHexDigest reports whether s is 64 lowercase hex digits.
func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// safeName reports whether s may be used as a cache entry name: 1 to
// maxNameLen bytes of [A-Za-z0-9._+!-], not starting with '.', '-' or '+'.
// Conda package names, versions and build strings use no other characters.
func safeName(s string) bool {
	if s == "" || len(s) > maxNameLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_':
		case c == '.' || c == '-' || c == '+' || c == '!':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isArchiveName(fn string) bool {
	return strings.HasSuffix(fn, ".conda") || strings.HasSuffix(fn, ".tar.bz2")
}

// allowedURL reports whether u may be fetched: https with a host, or with
// AllowHTTP plain http to a loopback IP address.
func (c *Cache) allowedURL(u *url.URL) error {
	switch {
	case u.Scheme == "https" && u.Host != "":
		return nil
	case c.AllowHTTP && u.Scheme == "http":
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%w: %s: only https is allowed", ErrInsecureURL, redact(u))
}

// parseURL validates rawURL and returns it with the archive file name its
// path ends in.
func (c *Cache) parseURL(rawURL string) (*url.URL, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("%w: url: %w", ErrInvalidInput, err)
	}
	if err := c.allowedURL(u); err != nil {
		return nil, "", err
	}
	if u.Opaque != "" || strings.HasSuffix(u.Path, "/") {
		return nil, "", fmt.Errorf("%w: %s: url does not end in a file name", ErrInvalidInput, redact(u))
	}
	for seg := range strings.SplitSeq(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, "", fmt.Errorf("%w: %s: dot segment in url path", ErrInvalidInput, redact(u))
		}
	}
	fn := path.Base(u.Path)
	if !safeName(fn) || !isArchiveName(fn) {
		return nil, "", fmt.Errorf("%w: %s: file name %s is not a .conda or .tar.bz2 cache name", ErrInvalidInput, redact(u), quote(fn))
	}
	return u, fn, nil
}

// Fetch returns the path of a verified archive for url in the cache,
// downloading it if needed. sha256 is required (64 lowercase hex); size is
// checked when > 0. The path, <Dir>/<sha256>/<fn>, is content-addressed:
// it holds these bytes or nothing. A download streams through sha256 into
// <fn>.partial-<random>, is verified, synced and renamed into place; no
// partial file survives an error or cancellation. An archive already in the
// cache is re-hashed unless its authenticated sidecar shows it unchanged
// since it was last verified.
func (c *Cache) Fetch(ctx context.Context, url, sha256 string, size int64) (string, error) {
	u, fn, err := c.checkFetch(url, sha256, size)
	if err != nil {
		return "", err
	}
	var p string
	err = c.withLock(ctx, sha256, func(dir string, key []byte) error {
		f, err := c.openArchive(ctx, u, dir, fn, sha256, size, key)
		if err != nil {
			return err
		}
		p = filepath.Join(dir, fn)
		return f.Close()
	})
	if err != nil {
		return "", fmt.Errorf("fetch: %s: %w", fn, err)
	}
	return p, nil
}

func (c *Cache) checkFetch(rawURL, sha string, size int64) (*url.URL, string, error) {
	if c.Dir == "" {
		return nil, "", fmt.Errorf("fetch: %w: Cache.Dir is empty", ErrInvalidInput)
	}
	if !isHexDigest(sha) {
		return nil, "", fmt.Errorf("fetch: %w: sha256 %s is not 64 lowercase hex digits", ErrInvalidInput, quote(sha))
	}
	if size < 0 {
		return nil, "", fmt.Errorf("fetch: %w: negative size %d", ErrInvalidInput, size)
	}
	u, fn, err := c.parseURL(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: %w", err)
	}
	return u, fn, nil
}

// withLock runs fn holding the locks for sha, with the content directory
// <Dir>/<sha> created and the MAC key loaded.
func (c *Cache) withLock(ctx context.Context, sha string, fn func(dir string, key []byte) error) error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	unlock, err := c.lock(ctx, sha)
	if err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	defer unlock()
	key, err := c.markerKey()
	if err != nil {
		return err
	}
	dir, err := ensureDir(filepath.Join(c.Dir, sha))
	if err != nil {
		return err
	}
	return fn(dir, key)
}

// ensureDir makes p a real directory, replacing a file or symlink planted
// there.
func ensureDir(p string) (string, error) {
	fi, err := os.Lstat(p)
	switch {
	case err == nil && fi.IsDir():
		return p, nil
	case err == nil:
		if err := os.Remove(p); err != nil {
			return "", err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// archiveFile is an open archive and the state it had when its digest was
// verified; any later difference means its bytes may have changed.
type archiveFile struct {
	*os.File
	verified stamp
}

// openArchive returns an open, verified archive <dir>/<fn> with digest sha
// (and size, when known), downloading it if the cache has no valid copy.
// The caller holds the lock for sha and reads the archive only through the
// returned file, never by path.
func (c *Cache) openArchive(ctx context.Context, u *url.URL, dir, fn, sha string, size int64, key []byte) (*archiveFile, error) {
	removeStale(dir, fn+".partial-", "")
	f, err := c.openCached(ctx, dir, fn, sha, size, key)
	if err != nil || f != nil {
		return f, err
	}
	return c.download(ctx, u, dir, fn, sha, size, key)
}

// stamp is what a sidecar records about the archive file it vouches for.
type stamp struct {
	size  int64
	mtime int64 // ns since the Unix epoch
	ino   uint64
	ctime int64 // ns since the Unix epoch
}

// stampOf describes the open file f.
func stampOf(f *os.File) (stamp, error) {
	fi, err := f.Stat()
	if err != nil {
		return stamp{}, err
	}
	ino, ctime := fileStamp(f)
	return stamp{size: fi.Size(), mtime: fi.ModTime().UnixNano(), ino: ino, ctime: ctime}, nil
}

// sidecarBody is the authenticated part of a sidecar line.
func sidecarBody(sha string, st stamp) string {
	return fmt.Sprintf("%s %d %d %d %d", sha, st.size, st.mtime, st.ino, st.ctime)
}

// sidecarMAC binds a sidecar body to the archive's file name and the key.
func sidecarMAC(key []byte, fn, body string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("goomba-sidecar 1\x00" + fn + "\x00" + body))
	return hex.EncodeToString(m.Sum(nil))
}

// sidecarContent is the exact content of the sidecar for fn.
func sidecarContent(key []byte, fn, sha string, st stamp) string {
	body := sidecarBody(sha, st)
	return body + " " + sidecarMAC(key, fn, body) + "\n"
}

// parseSidecar parses a sidecar strictly and checks its MAC; anything
// unexpected fails.
func parseSidecar(b []byte, key []byte, fn string) (string, stamp, error) {
	bad := func() (string, stamp, error) {
		return "", stamp{}, fmt.Errorf("%w: malformed or unauthenticated sidecar", ErrInvalidInput)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' || bytes.Count(b, []byte("\n")) != 1 {
		return bad()
	}
	f := strings.Split(string(b[:len(b)-1]), " ")
	if len(f) != 6 || !isHexDigest(f[0]) || !isHexDigest(f[5]) {
		return bad()
	}
	size, err1 := strconv.ParseInt(f[1], 10, 64)
	mtime, err2 := strconv.ParseInt(f[2], 10, 64)
	ino, err3 := strconv.ParseUint(f[3], 10, 64)
	ctime, err4 := strconv.ParseInt(f[4], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || size < 0 {
		return bad()
	}
	st := stamp{size: size, mtime: mtime, ino: ino, ctime: ctime}
	body := sidecarBody(f[0], st)
	if body+" "+f[5]+"\n" != string(b) {
		return bad() // non-canonical numbers such as "+1" or "01"
	}
	if !hmac.Equal([]byte(f[5]), []byte(sidecarMAC(key, fn, body))) {
		return bad()
	}
	return f[0], st, nil
}

// readSmall reads a regular file of at most max bytes without following a
// symlink at path.
func readSmall(p string, max int64) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, fmt.Errorf("%s: not a small regular file", p)
	}
	f, err := openNoFollow(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max+1))
}

// openCached opens <dir>/<fn> if it holds the archive with digest sha (and
// size, when known), returning nil if it does not. Anything else found at
// the path (a stale, corrupt or planted file, a symlink, a directory) is
// removed so the archive can be downloaded again.
func (c *Cache) openCached(ctx context.Context, dir, fn, sha string, size int64, key []byte) (*archiveFile, error) {
	final := filepath.Join(dir, fn)
	side := final + ".sha256"
	lfi, err := os.Lstat(final)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !lfi.Mode().IsRegular() {
		forceRemoveAll(final)
		_ = os.Remove(side)
		return nil, nil
	}
	f, err := openNoFollow(final)
	if err != nil {
		discard(final, side)
		return nil, nil
	}
	keep := false
	defer func() {
		if !keep {
			f.Close()
		}
	}()
	// drop closes f first: Windows cannot remove an open file.
	drop := func() (*archiveFile, error) {
		keep = true
		f.Close()
		discard(final, side)
		return nil, nil
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, lfi) || !fi.Mode().IsRegular() || (size > 0 && fi.Size() != size) {
		return drop()
	}
	st, err := stampOf(f)
	if err != nil {
		return nil, err
	}
	if b, err := readSmall(side, 512); err == nil {
		s, sst, err := parseSidecar(b, key, fn)
		if err != nil {
			// Another process may have replaced the key; look once.
			if k, ok := c.reloadKey(key); ok {
				key = k
				s, sst, err = parseSidecar(b, key, fn)
			}
		}
		if err == nil && s == sha && sst == st {
			keep = true
			return &archiveFile{File: f, verified: st}, nil
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: io.NewSectionReader(f, 0, st.size)}); err != nil {
		return nil, err
	}
	after, err := stampOf(f)
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha || after != st {
		return drop()
	}
	if err := writeFileAtomic(side, []byte(sidecarContent(key, fn, sha, st)), 0o644); err != nil {
		return nil, err
	}
	keep = true
	return &archiveFile{File: f, verified: st}, nil
}

// discard removes a cached archive and its sidecar, best effort: the
// archive is replaced by rename anyway.
func discard(final, side string) {
	_ = os.Remove(side)
	_ = os.Remove(final)
}

// writeFileAtomic writes data to a fresh temporary sibling of p, syncs it
// and renames it over p.
func writeFileAtomic(p string, data []byte, perm fs.FileMode) error {
	f, tmp, err := createUnique(filepath.Dir(p), filepath.Base(p)+".tmp-", perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func randomHex() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// createUnique creates dir/<prefix><16 random hex digits> exclusively, open
// for reading and writing.
func createUnique(dir, prefix string, perm fs.FileMode) (*os.File, string, error) {
	for range 16 {
		r, err := randomHex()
		if err != nil {
			return nil, "", err
		}
		p := filepath.Join(dir, prefix+r)
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("create temporary file in %q: too many collisions", dir)
}

// isRandomSuffix reports whether s is 16 lowercase hex digits.
func isRandomSuffix(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// removeStale removes entries of dir named prefix + 16 random hex digits +
// suffix: leftovers of a process that crashed while holding the lock the
// caller now holds. The exact match keeps it away from any other entry.
func removeStale(dir, prefix, suffix string) {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		name := filepath.Base(m)
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		rest, ok = strings.CutSuffix(rest, suffix)
		if ok && isRandomSuffix(rest) {
			forceRemoveAll(m)
		}
	}
}

// ctxReader checks ctx before every read.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
