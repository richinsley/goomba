// Cache semantics follow libmamba src/core/subdir_index.cpp (ETag /
// Last-Modified revalidation, offline use of any cached copy) and
// src/core/shards.cpp (content-addressed shard cache) (BSD-3-Clause, see
// NOTICE). Unlike libmamba, cached shards are re-hashed on every read.

package repodata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cacheMagic starts every index cache entry. An entry is the magic line, a
// JSON header line (cacheMeta) and the response body. One file per entry
// and an atomic rename keep header and body consistent across processes.
const cacheMagic = "goomba-index-cache 1\n"

// cacheMeta describes a cached index response. The URL is stored as a
// digest: channel URLs may carry tokens (anaconda.org /t/<token>/).
type cacheMeta struct {
	URLHash      string    `json:"url_sha256"`
	Status       int       `json:"status"` // http.StatusOK, or http.StatusNotFound for a cached absence
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Fetched      time.Time `json:"fetched"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
}

// cacheEntry is a verified cache entry.
type cacheEntry struct {
	meta cacheMeta
	body []byte
}

// parseCacheEntry splits and verifies a cache file. Cache files are
// untrusted: any inconsistency is an error and the entry is ignored.
func parseCacheEntry(data []byte) (*cacheEntry, error) {
	if !bytes.HasPrefix(data, []byte(cacheMagic)) {
		return nil, errors.New("cache entry: bad magic")
	}
	data = data[len(cacheMagic):]
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 || nl > maxCacheHeaderBytes {
		return nil, errors.New("cache entry: missing header")
	}
	var meta cacheMeta
	dec := json.NewDecoder(bytes.NewReader(data[:nl]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		return nil, fmt.Errorf("cache entry: header: %w", err)
	}
	if dec.More() {
		return nil, errors.New("cache entry: header: trailing data")
	}
	body := data[nl+1:]
	if meta.Status != http.StatusOK && meta.Status != http.StatusNotFound {
		return nil, fmt.Errorf("cache entry: bad status %d", meta.Status)
	}
	if meta.Status == http.StatusNotFound && len(body) != 0 {
		return nil, errors.New("cache entry: body on a cached absence")
	}
	if int64(len(body)) != meta.Size {
		return nil, fmt.Errorf("cache entry: size %d, header says %d", len(body), meta.Size)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != meta.SHA256 {
		return nil, &HashMismatchError{URL: "cache entry", Expected: meta.SHA256, Actual: got}
	}
	return &cacheEntry{meta: meta, body: body}, nil
}

func encodeCacheHeader(meta cacheMeta) ([]byte, error) {
	h, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(cacheMagic)+len(h)+1)
	out = append(out, cacheMagic...)
	out = append(out, h...)
	return append(out, '\n'), nil
}

// fetcher downloads index files and shards through the on-disk cache.
// It is safe for concurrent use.
type fetcher struct {
	s      *settings
	client *http.Client
	pol    urlPolicy
}

func newFetcher(s *settings, client *http.Client, pol urlPolicy) *fetcher {
	return &fetcher{s: s, client: safeClient(client, pol, s.lookup), pol: pol}
}

// indexCachePath maps a URL to its cache file.
func (f *fetcher) indexCachePath(u string) string {
	sum := sha256.Sum256([]byte(u))
	base := "index"
	if pu, err := url.Parse(u); err == nil {
		base = sanitizeFileName(path.Base(pu.Path))
	}
	return filepath.Join(f.s.cacheDir, "index", hex.EncodeToString(sum[:16])+"-"+base)
}

func sanitizeFileName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			b[i] = '_'
		}
	}
	if len(b) > 64 {
		b = b[len(b)-64:]
	}
	return string(b)
}

// freshness is the sidecar a 304 writes next to a cache entry instead of
// rewriting the body: it moves the entry's fetch time and ETag forward. It
// applies only to the entry whose body hash it names.
type freshness struct {
	SHA256  string    `json:"sha256"`
	Fetched time.Time `json:"fetched"`
	ETag    string    `json:"etag,omitempty"`
}

func freshnessPath(entryPath string) string { return entryPath + ".fresh" }

// loadEntry reads and verifies the cache entry for u, or returns nil. A
// valid freshness sidecar updates its fetch time and ETag.
func (f *fetcher) loadEntry(u string) *cacheEntry {
	p := f.indexCachePath(u)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	e, err := parseCacheEntry(data)
	if err != nil || e.meta.URLHash != urlHash(u) {
		return nil
	}
	if side, err := os.ReadFile(freshnessPath(p)); err == nil && len(side) <= maxCacheHeaderBytes {
		var fr freshness
		dec := json.NewDecoder(bytes.NewReader(side))
		dec.DisallowUnknownFields()
		if dec.Decode(&fr) == nil && fr.SHA256 == e.meta.SHA256 && fr.Fetched.After(e.meta.Fetched) {
			e.meta.Fetched = fr.Fetched
			if fr.ETag != "" {
				e.meta.ETag = fr.ETag
			}
		}
	}
	return e
}

func urlHash(u string) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:])
}

// storeEntry writes a cache entry. Callers treat failures as non-fatal:
// the data in hand is already verified, the cache only saves a download.
func (f *fetcher) storeEntry(u string, meta cacheMeta, body []byte) error {
	meta.URLHash = urlHash(u)
	meta.Size = int64(len(body))
	sum := sha256.Sum256(body)
	meta.SHA256 = hex.EncodeToString(sum[:])
	header, err := encodeCacheHeader(meta)
	if err != nil {
		return err
	}
	return writeFileAtomic(f.indexCachePath(u), header, body)
}

// storeFreshness records a successful revalidation without rewriting the
// body. Failures are non-fatal (the entry is merely revalidated sooner).
func (f *fetcher) storeFreshness(u string, e *cacheEntry) error {
	b, err := json.Marshal(freshness{SHA256: e.meta.SHA256, Fetched: e.meta.Fetched, ETag: e.meta.ETag})
	if err != nil {
		return err
	}
	return writeFileAtomic(freshnessPath(f.indexCachePath(u)), b)
}

// fresh reports whether a cache entry may be used without asking the server.
func (f *fetcher) fresh(e *cacheEntry) bool {
	age := f.s.now().Sub(e.meta.Fetched)
	return age >= 0 && age < f.s.maxAge
}

// fetchIndex returns the body of an index file (repodata.json*,
// repodata_shards.msgpack.zst), using and refreshing the cache. A missing
// file (HTTP 404, 410, 501) is ErrNotFound and is cached like a body.
// Cache writes are best effort: a read-only or full cache directory does
// not fail a download that succeeded.
func (f *fetcher) fetchIndex(ctx context.Context, u string, limit int64) ([]byte, error) {
	entry := f.loadEntry(u)
	result := func(e *cacheEntry) ([]byte, error) {
		if e.meta.Status == http.StatusNotFound {
			return nil, fmt.Errorf("repodata: %s: %w", redactString(u), ErrNotFound)
		}
		return e.body, nil
	}
	if f.s.offline {
		if entry == nil {
			return nil, fmt.Errorf("repodata: %s: %w", redactString(u), ErrNotCached)
		}
		return result(entry)
	}
	if entry != nil && f.fresh(entry) {
		return result(entry)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("repodata: %s: %w", redactString(u), err)
	}
	if entry != nil && entry.meta.Status == http.StatusOK {
		if entry.meta.ETag != "" {
			req.Header.Set("If-None-Match", entry.meta.ETag)
		}
		if entry.meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", entry.meta.LastModified)
		}
	}
	resp, body, err := f.do(req, limit)
	if err != nil {
		return nil, err
	}
	now := f.s.now().UTC()
	switch {
	case resp.StatusCode == http.StatusNotModified && entry != nil && entry.meta.Status == http.StatusOK:
		entry.meta.Fetched = now
		if et := resp.Header.Get("ETag"); et != "" {
			entry.meta.ETag = et
		}
		_ = f.storeFreshness(u, entry)
		return entry.body, nil
	case resp.StatusCode == http.StatusOK:
		meta := cacheMeta{
			Status:       http.StatusOK,
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
			Fetched:      now,
		}
		_ = f.storeEntry(u, meta, body)
		return body, nil
	case isMissingStatus(resp.StatusCode):
		_ = f.storeEntry(u, cacheMeta{Status: http.StatusNotFound, Fetched: now}, nil)
		return nil, fmt.Errorf("repodata: %s: %w (%w)", redactString(u), ErrNotFound,
			&HTTPError{URL: redactString(u), StatusCode: resp.StatusCode})
	default:
		return nil, &HTTPError{URL: redactString(u), StatusCode: resp.StatusCode}
	}
}

// isMissingStatus reports statuses that mean "this file does not exist"
// (rattler treats 404 and 501 so; 410 Gone is the same claim).
func isMissingStatus(code int) bool {
	return code == http.StatusNotFound || code == http.StatusGone || code == http.StatusNotImplemented
}

// fetchShard returns the compressed bytes of a content-addressed shard,
// verified against sum, from the cache or the network. Writing the verified
// shard to the cache is best effort.
func (f *fetcher) fetchShard(ctx context.Context, u string, sum [32]byte) (data []byte, fromNetwork bool, err error) {
	want := hex.EncodeToString(sum[:])
	p := filepath.Join(f.s.cacheDir, "shards", want+".msgpack.zst")
	if data, err := readFileLimited(p, maxShardDownload); err == nil {
		if got := sha256.Sum256(data); got == sum {
			return data, false, nil
		}
		// A corrupt or tampered cache file is ignored and replaced.
	}
	if f.s.offline {
		return nil, false, fmt.Errorf("repodata: shard %s: %w", want, ErrNotCached)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, fmt.Errorf("repodata: %s: %w", redactString(u), err)
	}
	resp, body, err := f.do(req, maxShardDownload)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, &HTTPError{URL: redactString(u), StatusCode: resp.StatusCode}
	}
	if got := sha256.Sum256(body); got != sum {
		return nil, false, &HashMismatchError{URL: redactString(u), Expected: want, Actual: hex.EncodeToString(got[:])}
	}
	_ = writeFileAtomic(p, body)
	return body, true, nil
}

// readFileLimited reads a cache file of at most limit bytes.
func readFileLimited(p string, limit int64) ([]byte, error) {
	fh, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = ErrTooLarge
	}
	return data, err
}

// maxRetryAfter bounds how long a 429 Retry-After may make a request wait.
const maxRetryAfter = 10 * time.Second

// retryAfter parses a Retry-After header (seconds or an HTTP date).
func retryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), maxRetryAfter)
}

// drain discards (a bounded amount of) a response body so the connection
// can be reused, then closes it.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// do performs req with retries for transient failures (network errors,
// HTTP 429 and 5xx other than 501) and reads at most limit body bytes of a
// 200 response. A 429 waits for its Retry-After (at most maxRetryAfter).
func (f *fetcher) do(req *http.Request, limit int64) (*http.Response, []byte, error) {
	if err := f.pol.check(req.URL); err != nil {
		return nil, nil, fmt.Errorf("repodata: %w", err)
	}
	req.Header.Set("User-Agent", "goomba")
	const attempts = 3
	var lastErr error
	var wait time.Duration
	for i := 0; i < attempts; i++ {
		if i > 0 {
			t := time.NewTimer(max(wait, time.Duration(250<<(i-1))*time.Millisecond))
			select {
			case <-req.Context().Done():
				t.Stop()
				return nil, nil, req.Context().Err()
			case <-t.C:
			}
		}
		wait = 0
		resp, err := f.client.Do(req.Clone(req.Context()))
		if err != nil {
			if req.Context().Err() != nil {
				return nil, nil, req.Context().Err()
			}
			// net/http's *url.Error repeats the URL; report its cause
			// under the redacted URL instead.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			lastErr = fmt.Errorf("repodata: GET %s: %w", redact(req.URL), err)
			if errors.Is(err, ErrUnsupportedURL) {
				return nil, nil, lastErr // refused redirect: not transient
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented {
			if resp.StatusCode == http.StatusTooManyRequests {
				wait = retryAfter(resp.Header.Get("Retry-After"), f.s.now())
			}
			drain(resp)
			lastErr = &HTTPError{URL: redact(req.URL), StatusCode: resp.StatusCode}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			drain(resp)
			return resp, nil, nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err == nil && int64(len(body)) > limit {
			err = fmt.Errorf("%w: more than %d bytes", ErrTooLarge, limit)
		}
		resp.Body.Close()
		if err != nil {
			if req.Context().Err() != nil {
				return nil, nil, req.Context().Err()
			}
			if errors.Is(err, ErrTooLarge) {
				return nil, nil, fmt.Errorf("repodata: GET %s: %w", redact(req.URL), err)
			}
			lastErr = fmt.Errorf("repodata: GET %s: %w", redact(req.URL), err)
			continue
		}
		return resp, body, nil
	}
	return nil, nil, lastErr
}

// writeFileAtomic writes the concatenation of parts to path through a
// temporary file in the same directory, fsync and rename, so readers in any
// process see the old file or the new one, never a partial one. A failed
// rename is retried once (on Windows a reader holding the old file open
// makes it fail briefly).
func writeFileAtomic(path string, parts ...[]byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+strings.TrimPrefix(filepath.Base(path), ".")+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	for _, p := range parts {
		if _, err := tmp.Write(p); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		time.Sleep(50 * time.Millisecond)
		return os.Rename(tmp.Name(), path)
	}
	return nil
}
