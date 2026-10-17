package repodata

import (
	"errors"
	"fmt"
)

// ErrNotSharded reports that a channel subdir does not publish a sharded
// index (repodata_shards.msgpack.zst answered 404, 410 or 501). Callers that
// get it from NewShardedChannel may fall back to NewFullIndex.
var ErrNotSharded = errors.New("channel has no sharded index")

// ErrNotFound reports that an index file does not exist on the server
// (HTTP 404, 410 or 501).
var ErrNotFound = errors.New("not found")

// ErrNotCached reports that Options.Offline is set and the requested file is
// not in the cache.
var ErrNotCached = errors.New("not in cache (offline)")

// ErrUnsupportedURL reports a URL that goomba refuses to use: anything other
// than https, except http to a loopback host (127.0.0.1, ::1 or localhost),
// and URLs carrying a query or fragment where a base URL is expected.
var ErrUnsupportedURL = errors.New("unsupported URL")

// ErrTooLarge reports a download or decompressed payload above goomba's size
// limits.
var ErrTooLarge = errors.New("payload too large")

// HashMismatchError reports content whose sha256 differs from the digest the
// index promised. It is never retried. It is an immutable value, safe to
// share between goroutines.
type HashMismatchError struct {
	// URL is where the content came from, or the cache path for a cached copy.
	URL string
	// Expected and Actual are lowercase hex sha256 digests.
	Expected, Actual string
}

// Error implements error.
func (e *HashMismatchError) Error() string {
	return fmt.Sprintf("repodata: sha256 mismatch for %s: expected %s, got %s", e.URL, e.Expected, e.Actual)
}

// HTTPError reports an unexpected HTTP status. 404, 410 and 501 are reported
// as ErrNotFound instead (wrapped together with an HTTPError). It is an
// immutable value, safe to share between goroutines.
type HTTPError struct {
	// URL is the requested URL with any password redacted.
	URL string
	// StatusCode is the HTTP status code of the response.
	StatusCode int
}

// Error implements error.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("repodata: GET %s: HTTP %d", e.URL, e.StatusCode)
}
