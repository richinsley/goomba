// Cross-process locking modelled on libmamba src/core/util.cpp (LockFile:
// one lock file per resource, taken before the resource is touched)
// (BSD-3-Clause, see NOTICE).

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// keyLock is an in-process mutex that waiting can abandon on cancellation.
// refs counts holders and waiters; the entry is dropped when it reaches 0.
type keyLock struct {
	ch   chan struct{}
	refs int
}

// lock takes the in-process lock for key, then the cross-process lock file
// Dir/.locks/<key>.lock. The returned function releases both.
func (c *Cache) lock(ctx context.Context, key string) (func(), error) {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = make(map[string]*keyLock)
	}
	kl := c.locks[key]
	if kl == nil {
		kl = &keyLock{ch: make(chan struct{}, 1)}
		c.locks[key] = kl
	}
	kl.refs++
	c.mu.Unlock()
	release := func() {
		c.mu.Lock()
		if kl.refs--; kl.refs == 0 {
			delete(c.locks, key)
		}
		c.mu.Unlock()
	}
	select {
	case kl.ch <- struct{}{}:
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
	fl, err := c.lockFile(ctx, key+".lock")
	if err != nil {
		<-kl.ch
		release()
		return nil, err
	}
	return func() {
		unlockFile(fl)
		<-kl.ch
		release()
	}, nil
}

// lockFile takes the lock file name in Dir/.locks, which must be a real
// directory; the lock file itself must be a regular file and is never
// reached through a symlink.
func (c *Cache) lockFile(ctx context.Context, name string) (*os.File, error) {
	root, err := c.root()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, ".locks")
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafeCache, dir)
	}
	if err := checkPrivate(dir, fi); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
	f, err := openLockFile(p)
	if err != nil {
		return nil, fmt.Errorf("%w: lock file %s: %w", ErrUnsafeCache, p, err)
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: lock file %s is not a regular file", ErrUnsafeCache, p)
	}
	if err := waitLock(ctx, f, p); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// pollLock calls try until it reports the lock taken, an error, or ctx is
// done, backing off from 5 ms to 100 ms between attempts.
func pollLock(ctx context.Context, try func() (bool, error)) error {
	delay := 5 * time.Millisecond
	for {
		ok, err := try()
		if err != nil || ok {
			return err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		delay = min(2*delay, 100*time.Millisecond)
	}
}
