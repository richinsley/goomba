package goomba

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// removeUnchanged runs RemoveIncomplete on prefix, expects it to fail, and
// checks that it changed nothing.
func removeUnchanged(t *testing.T, prefix string) error {
	t.Helper()
	parent := filepath.Dir(prefix)
	before := statSnapshot(t, parent)
	err := RemoveIncomplete(prefix)
	if err == nil {
		t.Errorf("RemoveIncomplete(%s) succeeded", prefix)
	}
	if after := statSnapshot(t, parent); after != before {
		t.Errorf("RemoveIncomplete changed the file system:\n before %s\n after  %s", before, after)
	}
	return err
}

func TestRemoveIncomplete(t *testing.T) {
	t.Run("marked prefix", func(t *testing.T) {
		prefix := incompletePrefix(t)
		outside := filepath.Join(t.TempDir(), "keep")
		must(t, os.WriteFile(outside, []byte("k"), 0o644))
		if runtime.GOOS != "windows" {
			must(t, os.Symlink(filepath.Dir(outside), filepath.Join(prefix, "lib", "out")))
			must(t, os.Symlink(filepath.Dir(outside), filepath.Join(prefix, "conda-meta", "out")))
		}
		must(t, os.MkdirAll(filepath.Join(prefix, "share", "ro", "deep"), 0o755))
		must(t, os.WriteFile(filepath.Join(prefix, "share", "ro", "deep", "f"), nil, 0o444))
		must(t, os.Chmod(filepath.Join(prefix, "share", "ro"), 0o555))
		must(t, RemoveIncomplete(prefix))
		if _, err := os.Lstat(prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("prefix left: %v", err)
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "k" {
			t.Errorf("data outside: %q, %v", data, err)
		}
		if err := CheckComplete(prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("CheckComplete afterwards: %v", err)
		}
	})
	t.Run("only an empty conda-meta", func(t *testing.T) {
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
		must(t, RemoveIncomplete(prefix))
		if _, err := os.Lstat(prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("prefix left: %v", err)
		}
	})
	t.Run("relative path", func(t *testing.T) {
		prefix := incompletePrefix(t)
		t.Chdir(filepath.Dir(prefix))
		must(t, RemoveIncomplete("env"))
		if _, err := os.Lstat(prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("prefix left: %v", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		c := newChannel(t)
		lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
		complete := filepath.Join(t.TempDir(), "env")
		must(t, InstallLock(context.Background(), complete, lf, c.options(t)))
		err := removeUnchanged(t, complete)
		if !errors.Is(err, ErrNotIncomplete) || errors.Is(err, ErrIncomplete) || errors.Is(err, ErrPrefixLocked) || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("complete environment: %v, want ErrNotIncomplete alone", err)
		}
		t.Logf("complete environment: %v", err)
		for name, setup := range map[string]func(string){
			"missing":         func(string) {},
			"empty directory": func(p string) { must(t, os.Mkdir(p, 0o700)) },
			"no conda-meta": func(p string) {
				must(t, os.MkdirAll(filepath.Join(p, "lib"), 0o755))
				must(t, os.WriteFile(filepath.Join(p, "lib", "a"), nil, 0o644))
			},
			"empty conda-meta beside files": func(p string) {
				must(t, os.MkdirAll(filepath.Join(p, "conda-meta"), 0o755))
				must(t, os.WriteFile(filepath.Join(p, "user-file"), nil, 0o644))
			},
		} {
			prefix := filepath.Join(t.TempDir(), "env")
			setup(prefix)
			// A missing prefix holds nothing to judge; the others are
			// directories that hold no environment.
			err := removeUnchanged(t, prefix)
			if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrIncomplete) || errors.Is(err, ErrNotIncomplete) != (name != "missing") {
				t.Errorf("%s: %v, want fs.ErrNotExist, and ErrNotIncomplete unless missing", name, err)
			}
		}
		if err := RemoveIncomplete(""); !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("empty prefix: %v", err)
		}
	})
	t.Run("through a symlink", func(t *testing.T) {
		// The directory is emptied and kept, so that the symlink still
		// leads to a directory an install can fill.
		real := incompletePrefix(t)
		link := filepath.Join(filepath.Dir(real), "envlink")
		must(t, os.Symlink("env", link))
		must(t, RemoveIncomplete(link))
		if ents, err := os.ReadDir(real); err != nil || len(ents) != 0 {
			t.Errorf("real directory: %v, %v; want it kept and empty", ents, err)
		}
		if err := CheckComplete(link); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("CheckComplete(link): %v, want fs.ErrNotExist (empty)", err)
		}
		c := newChannel(t)
		lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
		must(t, InstallLock(context.Background(), link, lf, c.options(t)))
		if err := CheckComplete(link); err != nil {
			t.Errorf("CheckComplete after installing through the link: %v", err)
		}
	})
	t.Run("empty directory is not locked", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		// An InstallLock that has just taken an empty prefix is not
		// disturbed: nothing to remove, and no lock taken to find out.
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.Mkdir(prefix, 0o700))
		l, err := lockDir(prefix)
		must(t, err)
		defer unlockDir(l)
		if err := removeUnchanged(t, prefix); !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrNotIncomplete) || errors.Is(err, ErrPrefixLocked) {
			t.Errorf("err = %v, want fs.ErrNotExist and ErrNotIncomplete", err)
		}
	})
	t.Run("unsupported without the prefix lock", func(t *testing.T) {
		if havePrefixLock {
			t.Skip("this platform has the prefix lock")
		}
		prefix := incompletePrefix(t)
		if err := removeUnchanged(t, prefix); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("err = %v, want errors.ErrUnsupported", err)
		}
	})
	t.Run("locked", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		prefix := incompletePrefix(t)
		l, err := lockDir(prefix)
		must(t, err)
		defer unlockDir(l)
		if err := removeUnchanged(t, prefix); !errors.Is(err, ErrPrefixLocked) {
			t.Errorf("err = %v, want ErrPrefixLocked", err)
		}
	})
	t.Run("unsafe prefix", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("ownership is not checked")
		}
		prefix := incompletePrefix(t)
		must(t, os.Chmod(prefix, 0o777))
		defer os.Chmod(prefix, 0o700)
		if err := removeUnchanged(t, prefix); !errors.Is(err, ErrUnsafePrefix) {
			t.Errorf("err = %v, want ErrUnsafePrefix", err)
		}
		must(t, os.Chmod(prefix, 0o777|fs.ModeSticky))
		if err := removeUnchanged(t, prefix); !errors.Is(err, ErrUnsafePrefix) {
			t.Errorf("sticky prefix: %v, want ErrUnsafePrefix", err)
		}
	})
	t.Run("racing a fresh install", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		// One caller removes an interrupted prefix while another keeps
		// trying to install into the same path, as two jumpboot processes
		// that both saw ErrIncomplete might: the removal succeeds, the
		// install succeeds once the path is free, and what it installed is
		// complete.
		c := newChannel(t)
		lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
		opt := c.options(t)
		failures := map[string]int{}
		for range 20 {
			prefix := incompletePrefix(t)
			var wg sync.WaitGroup
			var rerr error
			wg.Add(1)
			go func() { defer wg.Done(); rerr = RemoveIncomplete(prefix) }()
			var ierr error
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
				if ierr = InstallLock(context.Background(), prefix, lf, opt); ierr == nil {
					break
				}
				switch msg := ierr.Error(); {
				case errors.Is(ierr, ErrPrefixNotEmpty):
					failures["not empty"]++
				case errors.Is(ierr, ErrPrefixLocked):
					failures["locked"]++
				case strings.HasSuffix(msg, "prefix was replaced while it was being locked"):
					// It opened the directory the removal then took away.
					failures["replaced"]++
				default:
					t.Fatalf("InstallLock: %v", ierr)
				}
			}
			wg.Wait()
			if rerr != nil {
				t.Fatalf("RemoveIncomplete: %v", rerr)
			}
			if ierr != nil {
				t.Fatalf("InstallLock never succeeded: %v", ierr)
			}
			if err := CheckComplete(prefix); err != nil {
				t.Fatalf("CheckComplete after the install: %v", err)
			}
		}
		t.Logf("install attempts refused meanwhile: %v", failures)
	})
}
