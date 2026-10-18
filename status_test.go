package goomba

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// statSnapshot describes everything below and including root: path, mode,
// size and modification time, so that a check can prove it changed nothing.
func statSnapshot(t *testing.T, root string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == root {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %v %d %d", p, fi.Mode(), fi.Size(), fi.ModTime().UnixNano()))
		return nil
	})
	must(t, err)
	return fmt.Sprint(out)
}

// checkUnchanged runs CheckComplete on prefix and fails if anything below
// the prefix's parent changed.
func checkUnchanged(t *testing.T, prefix string) error {
	t.Helper()
	parent := filepath.Dir(prefix)
	before := statSnapshot(t, parent)
	err := CheckComplete(prefix)
	if after := statSnapshot(t, parent); after != before {
		t.Errorf("CheckComplete changed the file system:\n before %s\n after  %s", before, after)
	}
	return err
}

// incompletePrefix makes what an InstallLock killed after taking the
// prefix leaves: a private prefix with conda-meta and the marker, and
// optionally some installed files.
func incompletePrefix(t *testing.T) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, os.Mkdir(prefix, 0o700))
	must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
	must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", incompleteMarker), []byte("goomba is creating this environment\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(prefix, "lib"), 0o755))
	must(t, os.WriteFile(filepath.Join(prefix, "lib", "a"), []byte("a"), 0o644))
	return prefix
}

func TestCheckComplete(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, []string{"pip"}, cancelEnv(t, fakePython(t, t.TempDir(), 0, "pip"))...)

	t.Run("missing prefix", func(t *testing.T) {
		prefix := filepath.Join(t.TempDir(), "a", "env")
		if err := checkUnchanged(t, prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want fs.ErrNotExist", err)
		}
		if err := checkUnchanged(t, filepath.Dir(prefix)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("complete prefix", func(t *testing.T) {
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, InstallLock(context.Background(), prefix, lf, c.options(t)))
		if err := checkUnchanged(t, prefix); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		// Also through a relative path and a trailing slash.
		t.Chdir(filepath.Dir(prefix))
		if err := checkUnchanged(t, "env"+string(filepath.Separator)); err != nil {
			t.Errorf("relative: err = %v, want nil", err)
		}
	})
	t.Run("environment of another tool", func(t *testing.T) {
		// micromamba and conda write conda-meta/history and no marker.
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
		must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", "history"), nil, 0o644))
		if err := checkUnchanged(t, prefix); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("marker, no lock", func(t *testing.T) {
		prefix := incompletePrefix(t)
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrIncomplete) {
			t.Errorf("err = %v, want ErrIncomplete", err)
		}
		// With a history too (a commit that never removed the marker).
		must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", "history"), nil, 0o644))
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrIncomplete) {
			t.Errorf("err = %v, want ErrIncomplete", err)
		}
		// Concurrent checks hold the lock shared and do not see each other.
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					if err := CheckComplete(prefix); !errors.Is(err, ErrIncomplete) {
						errs[i] = err
						return
					}
				}
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Errorf("concurrent check: %v, want ErrIncomplete", err)
			}
		}
	})
	t.Run("marker, locked", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		prefix := incompletePrefix(t)
		// flock locks belong to the open file description: a second open
		// in this process conflicts as another process's would.
		l, err := lockDir(prefix)
		must(t, err)
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrPrefixLocked) {
			t.Errorf("err = %v, want ErrPrefixLocked", err)
		}
		// Even once the marker is gone (InstallLock removes it just
		// before it releases the lock).
		must(t, os.Remove(filepath.Join(prefix, "conda-meta", incompleteMarker)))
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrPrefixLocked) {
			t.Errorf("err = %v, want ErrPrefixLocked", err)
		}
		unlockDir(l)
		if err := checkUnchanged(t, prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("unlocked, no history: err = %v, want fs.ErrNotExist", err)
		}
		// A shared lock (another check) does not count.
		must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", incompleteMarker), nil, 0o644))
		s, err := openDirNoFollow(prefix)
		must(t, err)
		defer unlockDir(s)
		must(t, tryLockShared(s))
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrIncomplete) {
			t.Errorf("shared lock held: err = %v, want ErrIncomplete", err)
		}
		// A shared lock held longer than lockDir waits keeps an install out.
		if l, err := lockDir(prefix); !errors.Is(err, ErrPrefixLocked) {
			t.Errorf("lockDir under a shared lock: %v", err)
			if l != nil {
				unlockDir(l)
			}
		}
	})
	t.Run("lock consulted unless empty", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		// An empty directory is never locked, so a check cannot delay an
		// InstallLock taking it; one that has just taken it is reported
		// as not an environment.
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.Mkdir(prefix, 0o700))
		l, err := lockDir(prefix)
		must(t, err)
		if err := checkUnchanged(t, prefix); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrPrefixLocked) {
			t.Errorf("locked empty prefix: %v, want fs.ErrNotExist", err)
		}
		// Anything else is: a rollback or removal that has taken away
		// conda-meta, or emptied it, is still ErrPrefixLocked.
		for name, entries := range map[string][]string{
			"no conda-meta":    {"lib/a"},
			"empty conda-meta": {"conda-meta/"},
			"both":             {"conda-meta/", "lib/a"},
		} {
			for _, e := range entries {
				p := filepath.Join(prefix, filepath.FromSlash(e))
				if strings.HasSuffix(e, "/") {
					must(t, os.MkdirAll(p, 0o755))
				} else {
					must(t, os.MkdirAll(filepath.Dir(p), 0o755))
					must(t, os.WriteFile(p, nil, 0o644))
				}
			}
			if err := checkUnchanged(t, prefix); !errors.Is(err, ErrPrefixLocked) {
				t.Errorf("%s, locked: %v, want ErrPrefixLocked", name, err)
			}
			unlockDir(l)
			want := fs.ErrNotExist
			if name == "empty conda-meta" {
				want = ErrIncomplete // what a kill just before removing it leaves
			}
			if err := checkUnchanged(t, prefix); !errors.Is(err, want) {
				t.Errorf("%s, unlocked: %v, want %v", name, err, want)
			}
			must(t, os.RemoveAll(prefix))
			must(t, os.Mkdir(prefix, 0o700))
			l, err = lockDir(prefix)
			must(t, err)
		}
		unlockDir(l)
	})
	t.Run("decides on the locked reading", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		// The first, unlocked reading sees the marker of an install that
		// commits before the check gets the lock: the answer must come
		// from the second reading, not ErrIncomplete for a finished
		// environment (which a caller would delete).
		prefix := incompletePrefix(t)
		l, err := lockDir(prefix)
		must(t, err)
		err = checkComplete(prefix, func() {
			must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", "history"), nil, 0o644))
			must(t, os.Remove(filepath.Join(prefix, "conda-meta", incompleteMarker)))
			unlockDir(l)
		})
		if err != nil {
			t.Errorf("a commit between the readings: %v, want nil", err)
		}
		// And the other way round: a finished-looking first reading,
		// then an install that holds the lock.
		prefix = filepath.Join(t.TempDir(), "env")
		must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
		must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", "history"), nil, 0o644))
		var l2 *os.File
		err = checkComplete(prefix, func() {
			var err error
			l2, err = lockDir(prefix)
			must(t, err)
		})
		if !errors.Is(err, ErrPrefixLocked) {
			t.Errorf("locked between the readings: %v, want ErrPrefixLocked", err)
		}
		unlockDir(l2)
	})
	t.Run("never misreports a commit", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		// A writer that has the marker only while it holds the exclusive
		// lock, as InstallLock does, against several checks: a finished
		// environment is never reported as anything but nil or
		// ErrPrefixLocked.
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
		must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", "history"), nil, 0o644))
		marker := filepath.Join(prefix, "conda-meta", incompleteMarker)
		var stop atomic.Bool
		var wg sync.WaitGroup
		var bad atomic.Int64
		var checks atomic.Int64
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					time.Sleep(20 * time.Microsecond) // checks, not a lock held without a break
					checks.Add(1)
					if err := CheckComplete(prefix); err != nil && !errors.Is(err, ErrPrefixLocked) {
						if bad.Add(1) == 1 {
							t.Errorf("check: %v", err)
						}
					}
				}
			}()
		}
		for range 300 {
			l, err := lockDir(prefix)
			if err != nil {
				t.Errorf("lockDir against checks only: %v", err)
				continue
			}
			must(t, os.WriteFile(marker, nil, 0o644))
			must(t, os.Remove(marker))
			unlockDir(l)
		}
		stop.Store(true)
		wg.Wait()
		if bad.Load() > 0 {
			t.Errorf("%d of %d checks misreported", bad.Load(), checks.Load())
		}
	})
	t.Run("leftover empty conda-meta", func(t *testing.T) {
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrIncomplete) {
			t.Errorf("only an empty conda-meta: %v, want ErrIncomplete", err)
		}
	})
	t.Run("during an install", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		prefix := filepath.Join(t.TempDir(), "env")
		opt := c.options(t)
		type check struct {
			kind EventKind
			err  error
		}
		var mu sync.Mutex
		var checks []check
		opt.Progress = func(e Event) {
			if e.Kind == EventFetched {
				return // the prefix is not taken yet
			}
			err := CheckComplete(prefix)
			mu.Lock()
			checks = append(checks, check{e.Kind, err})
			mu.Unlock()
		}
		must(t, InstallLock(context.Background(), prefix, lf, opt))
		for i, c := range checks {
			switch {
			case c.kind == EventDone: // reported once the lock is released
				if c.err != nil {
					t.Errorf("at %v: %v, want nil", c.kind, c.err)
				}
			case !errors.Is(c.err, ErrPrefixLocked):
				t.Errorf("at event %d (%v): %v, want ErrPrefixLocked", i, c.kind, c.err)
			}
		}
		if len(checks) < len(lf.Packages)+1 {
			t.Errorf("only %d checks during the install", len(checks))
		}
		if err := CheckComplete(prefix); err != nil {
			t.Errorf("after the install: %v", err)
		}
	})
	t.Run("not an environment", func(t *testing.T) {
		for name, setup := range map[string]func(prefix string){
			"empty directory": func(prefix string) { must(t, os.Mkdir(prefix, 0o755)) },
			"no conda-meta": func(prefix string) {
				must(t, os.MkdirAll(filepath.Join(prefix, "lib"), 0o755))
			},
			"conda-meta without history or marker": func(prefix string) {
				must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
				must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", "x-1-0.json"), []byte("{}"), 0o644))
			},
		} {
			prefix := filepath.Join(t.TempDir(), "env")
			setup(prefix)
			if err := checkUnchanged(t, prefix); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrIncomplete) {
				t.Errorf("%s: err = %v, want fs.ErrNotExist", name, err)
			}
		}
		for name, setup := range map[string]func(prefix string){
			"a file": func(prefix string) { must(t, os.WriteFile(prefix, nil, 0o644)) },
			"conda-meta is a file": func(prefix string) {
				must(t, os.Mkdir(prefix, 0o755))
				must(t, os.WriteFile(filepath.Join(prefix, "conda-meta"), nil, 0o644))
			},
			"conda-meta is a symlink": func(prefix string) {
				if runtime.GOOS == "windows" {
					t.Skip("symlinks")
				}
				must(t, os.MkdirAll(filepath.Join(prefix, "real-meta"), 0o755))
				must(t, os.WriteFile(filepath.Join(prefix, "real-meta", "history"), nil, 0o644))
				must(t, os.Symlink("real-meta", filepath.Join(prefix, "conda-meta")))
			},
		} {
			prefix := filepath.Join(t.TempDir(), "env")
			setup(prefix)
			if err := checkUnchanged(t, prefix); err == nil || errors.Is(err, ErrIncomplete) || errors.Is(err, ErrPrefixLocked) {
				t.Errorf("%s: err = %v, want an error", name, err)
			} else {
				t.Logf("%s: %v", name, err)
			}
		}
		if err := CheckComplete(""); !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("empty prefix: %v", err)
		}
	})
	t.Run("unsafe prefix", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("ownership is not checked")
		}
		prefix := incompletePrefix(t)
		must(t, os.Chmod(prefix, 0o777))
		defer os.Chmod(prefix, 0o700)
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrUnsafePrefix) {
			t.Errorf("prefix 0777: %v, want ErrUnsafePrefix", err)
		}
		// Sticky: fine on the way to a prefix, not as the prefix.
		must(t, os.Chmod(prefix, 0o777|fs.ModeSticky))
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrUnsafePrefix) {
			t.Errorf("prefix 1777: %v, want ErrUnsafePrefix", err)
		}
		must(t, os.Chmod(prefix, 0o700))
		parent := filepath.Dir(prefix)
		must(t, os.Chmod(parent, 0o777))
		defer os.Chmod(parent, 0o700)
		if err := checkUnchanged(t, prefix); !errors.Is(err, ErrUnsafePrefix) {
			t.Errorf("parent 0777: %v, want ErrUnsafePrefix", err)
		}
	})
}
