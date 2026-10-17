// Retry classification ported from libmamba src/download/downloader.cpp
// (DownloadAttempt::Impl::can_retry) (BSD-3-Clause, see NOTICE).

package fetch

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	// defaultBackoff is the delay before the first retry; it doubles for
	// each further retry up to maxBackoff.
	defaultBackoff = 250 * time.Millisecond
	maxBackoff     = 8 * time.Second
	// stallTimeout aborts a download that delivers no bytes for this long.
	stallTimeout = 60 * time.Second
	maxRedirects = 10
)

// errStalled is the cause of a download cancelled for making no progress.
var errStalled = errors.New("download stalled")

// transientError marks a failure worth retrying: a network error or stall.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func isTransient(err error) bool {
	var te *transientError
	if errors.As(err, &te) {
		return true
	}
	var he *HTTPError
	return errors.As(err, &he) && he.Temporary()
}

func newDefaultClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: nil, // never from the environment
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}}
}

// httpClient returns a client that refuses redirects to URLs the cache
// would not fetch directly and never sends a Referer, which could carry a
// channel token, to the redirect target. The caller's Client is copied, not
// modified.
func (c *Cache) httpClient() *http.Client {
	base := c.Client
	if base == nil {
		c.mu.Lock()
		if c.client == nil {
			c.client = newDefaultClient()
		}
		base = c.client
		c.mu.Unlock()
	}
	cl := *base
	next := base.CheckRedirect
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Referer")
		if len(via) >= maxRedirects {
			return fmt.Errorf("%w: stopped after %d redirects", ErrInsecureURL, maxRedirects)
		}
		if err := c.allowedURL(req.URL); err != nil {
			return fmt.Errorf("redirect: %w", err)
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
	return &cl
}

func (c *Cache) retries() int {
	switch {
	case c.Retries == 0:
		return 3
	case c.Retries < 0:
		return 0
	}
	return c.Retries
}

func (c *Cache) backoff(attempt int) time.Duration {
	d := defaultBackoff
	if c.backoff0 > 0 {
		d = time.Duration(c.backoff0)
	}
	for range attempt {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

// download fetches u into <dir>/<fn>, retrying transient failures, and
// returns the verified file, still open.
func (c *Cache) download(ctx context.Context, u *url.URL, dir, fn, sha string, size int64, key []byte) (*archiveFile, error) {
	client := c.httpClient()
	for attempt := 0; ; attempt++ {
		f, err := c.downloadOnce(ctx, client, u, dir, fn, sha, size, key)
		if err == nil {
			return f, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= c.retries() || !isTransient(err) {
			return nil, err
		}
		t := time.NewTimer(c.backoff(attempt))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// progressReader resets a stall timer whenever bytes arrive and marks read
// errors as transient.
type progressReader struct {
	r     io.Reader
	timer *time.Timer
	ctx   context.Context
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.timer.Reset(stallTimeout)
	}
	if err != nil && err != io.EOF {
		if cause := context.Cause(p.ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = cause
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = &transientError{err}
		}
	}
	return n, err
}

// hashWriter writes to a file and a hash; file errors are not transient.
type hashWriter struct {
	f *os.File
	h hash.Hash
}

func (w hashWriter) Write(b []byte) (int, error) {
	n, err := w.f.Write(b)
	w.h.Write(b[:n])
	return n, err
}

// downloadOnce streams u through sha256 into a fresh partial file, verifies
// it, syncs it and renames it to <dir>/<fn>. It returns that file, open, so
// the caller reads exactly the bytes that were verified.
func (c *Cache) downloadOnce(ctx context.Context, client *http.Client, u *url.URL, dir, fn, sha string, size int64, key []byte) (_ *archiveFile, err error) {
	redacted := redact(u)
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	defer stall.Stop()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// Ask for the archive's own bytes: no transparent decompression.
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "goomba")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrInsecureURL) || ctx.Err() != nil {
			return nil, err
		}
		if cause := context.Cause(rctx); errors.Is(cause, errStalled) {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		return nil, &transientError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{URL: redacted, StatusCode: resp.StatusCode, Status: resp.Status}
	}
	limit := size
	if limit <= 0 {
		limit = maxUnknownSize
	}
	switch {
	case size > 0 && resp.ContentLength >= 0 && resp.ContentLength != size:
		return nil, &SizeError{URL: redacted, Expected: size, Actual: resp.ContentLength}
	case resp.ContentLength > limit:
		return nil, &SizeError{URL: redacted, Expected: limit, Actual: resp.ContentLength, Over: true}
	}

	f, tmp, err := createUnique(dir, fn+".partial-", 0o644)
	if err != nil {
		return nil, err
	}
	renamed := false
	defer func() {
		if err != nil {
			_ = f.Close()
			if !renamed {
				_ = os.Remove(tmp)
			}
			f = nil
		}
	}()
	h := sha256.New()
	body := &progressReader{r: resp.Body, timer: stall, ctx: rctx}
	n, err := io.Copy(hashWriter{f, h}, io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, &SizeError{URL: redacted, Expected: limit, Actual: n, Over: true}
	}
	if size > 0 && n != size {
		return nil, &SizeError{URL: redacted, Expected: size, Actual: n}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return nil, &IntegrityError{URL: redacted, Expected: sha, Actual: got}
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	final := filepath.Join(dir, fn)
	side := final + ".sha256"
	_ = os.Remove(side) // never let an old sidecar vouch for the new file
	if f, err = renameOpen(f, tmp, final); err != nil {
		if f == nil {
			renamed = true // nothing left open; the partial was renamed or removed
		}
		return nil, err
	}
	renamed = true
	syncDir(dir)
	st, err := stampOf(f)
	if err != nil {
		return nil, err
	}
	if err = writeFileAtomic(side, []byte(sidecarContent(key, fn, sha, st)), 0o644); err != nil {
		return nil, err
	}
	return &archiveFile{File: f, verified: st}, nil
}
