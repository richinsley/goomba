//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package goomba

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLockDirWaitsForChecks (review of WP8c, S2): a status check's shared
// lock delays lockDir by moments, never makes it fail; an exclusive holder
// does, at once; and lockDir never blocks indefinitely.
func TestLockDirWaitsForChecks(t *testing.T) {
	prefix := t.TempDir()
	shared := func() *os.File {
		d, err := openDirNoFollow(prefix)
		must(t, err)
		must(t, tryLockShared(d))
		return d
	}
	t.Run("brief shared lock", func(t *testing.T) {
		s := shared()
		time.AfterFunc(5*time.Millisecond, func() { unlockDir(s) })
		l, err := lockDir(prefix)
		if err != nil {
			t.Fatalf("lockDir while a check held the lock for 5 ms: %v", err)
		}
		unlockDir(l)
	})
	t.Run("shared lock held throughout", func(t *testing.T) {
		s := shared()
		defer unlockDir(s)
		start := time.Now()
		if l, err := lockDir(prefix); !errors.Is(err, ErrPrefixLocked) {
			t.Errorf("lockDir: %v, want ErrPrefixLocked", err)
			if l != nil {
				unlockDir(l)
			}
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("lockDir took %v", d)
		}
	})
	t.Run("exclusive holder", func(t *testing.T) {
		l, err := lockDir(prefix)
		must(t, err)
		defer unlockDir(l)
		// At once: the fastest of five tries is far below the 50 ms that
		// waiting for checks would take.
		fastest := time.Hour
		for range 5 {
			start := time.Now()
			if l2, err := lockDir(prefix); !errors.Is(err, ErrPrefixLocked) {
				t.Errorf("second lockDir: %v, want ErrPrefixLocked", err)
				if l2 != nil {
					unlockDir(l2)
				}
			}
			fastest = min(fastest, time.Since(start))
		}
		if fastest > 25*time.Millisecond {
			t.Errorf("lockDir against an exclusive holder took %v: it waited", fastest)
		}
		if err := tryLockShared(openUnlocked(t, prefix)); !errors.Is(err, ErrPrefixLocked) {
			t.Errorf("tryLockShared: %v, want ErrPrefixLocked", err)
		}
	})
	t.Run("takeovers against checks", func(t *testing.T) {
		// The reviewer's loop: InstallLock taking over a reused empty
		// directory again and again while a check runs without pause.
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.Mkdir(prefix, 0o700))
		var stop atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = CheckComplete(prefix)
			}
		}()
		blocked := 0
		for range 2000 {
			tx, err := openPrefix(prefix)
			if err != nil {
				if !errors.Is(err, ErrPrefixLocked) {
					t.Fatalf("openPrefix: %v", err)
				}
				blocked++
				continue
			}
			tx.rollback()
		}
		stop.Store(true)
		wg.Wait()
		if blocked > 0 {
			t.Errorf("checks made %d of 2000 takeovers fail", blocked)
		}
	})
}

// openUnlocked opens prefix without locking it.
func openUnlocked(t *testing.T, prefix string) *os.File {
	d, err := openDirNoFollow(prefix)
	must(t, err)
	t.Cleanup(func() { unlockDir(d) })
	return d
}

// TestCommitKeepsMarkerOnFailure (review of WP8c, S1): commit removes the
// marker last, so a commit that fails earlier (here setDirMode, on a
// closed handle) leaves the prefix marked for the rollback.
func TestCommitKeepsMarkerOnFailure(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "env")
	tx, err := openPrefix(prefix)
	must(t, err)
	must(t, tx.dir.Close())
	if err := tx.commit(); err == nil {
		t.Fatal("commit with a closed handle succeeded")
	}
	if _, err := os.Lstat(filepath.Join(prefix, "conda-meta", incompleteMarker)); err != nil {
		t.Errorf("marker gone after a failed commit: %v", err)
	}
	if err := CheckComplete(prefix); !errors.Is(err, ErrIncomplete) {
		t.Errorf("CheckComplete: %v, want ErrIncomplete", err)
	}
	tx.dir = nil
	must(t, RemoveIncomplete(prefix))
}

// TestRmdirSame (review of WP8c round 2, N4): the final removal of a
// prefix uses rmdir semantics and only removes the directory it was given
// the identity of: a path swapped for a file or another directory costs
// nothing.
func TestRmdirSame(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "env")
	must(t, os.Mkdir(p, 0o700))
	id, err := os.Lstat(p)
	must(t, err)
	// The directory moved away (so its inode stays in use) and the path
	// swapped for a file, then for another directory.
	must(t, os.Rename(p, p+".moved"))
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	if err := rmdirSame(p, id); err != nil {
		t.Errorf("swapped for a file: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the file was removed: %v", err)
	}
	must(t, os.Remove(p))
	must(t, os.Mkdir(p, 0o700))
	if err := rmdirSame(p, id); err != nil {
		t.Errorf("swapped for another directory: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the other directory was removed: %v", err)
	}
	must(t, os.Remove(p))
	// Even when the identity matches (inode numbers are reused), rmdir
	// semantics never remove a file.
	must(t, os.Rename(p+".moved", p))
	f := filepath.Join(p, "f")
	must(t, os.WriteFile(f, nil, 0o644))
	if err := rmdirSame(p, id); !isNotEmpty(err) {
		t.Errorf("not empty: %v, want ENOTEMPTY", err)
	}
	must(t, os.Remove(f))
	must(t, rmdirSame(p, id))
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not removed: %v", err)
	}
	must(t, rmdirSame(p, id)) // gone: nothing to do
	// A file with the identity given: rmdir semantics leave it.
	f = filepath.Join(dir, "file")
	must(t, os.WriteFile(f, []byte("x"), 0o644))
	fid, err := os.Lstat(f)
	must(t, err)
	if err := rmdirSame(f, fid); err == nil {
		t.Error("rmdirSame removed a file")
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("file removed: %v", err)
	}
}
