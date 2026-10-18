//go:build unix

package goomba

// Regression tests for review round 2, ported from the reviewer's probes.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/richinsley/goomba/fetch"
)

// TestCacheViaForeignSymlink (S1): the package cache is reached through a
// symlink in a shared sticky directory (as /tmp); re-pointing it after the
// cache was checked does not change what is installed. A symlink in a
// directory others can write without the sticky bit is not followed at all
// (WP8b review round 1, S1): another user could have put it there.
func TestCacheViaForeignSymlink(t *testing.T) {
	c := newChannel(t)
	victim := buildPkg(t, tspec{name: "victim", version: "1", build: "0", files: []tfile{
		{path: "bin/tool", data: "#!/bin/sh\necho genuine\n", mode: 0o755},
	}})
	lf := c.lockfile(t, nil, victim)
	root := t.TempDir()
	private := filepath.Join(root, "private")
	must(t, os.Mkdir(private, 0o755))
	shared := filepath.Join(root, "shared")
	must(t, os.Mkdir(shared, 0o777))
	must(t, os.Chmod(shared, 0o777))
	must(t, os.Symlink(private, filepath.Join(shared, "cache")))
	opt := c.options(t)
	opt.CompilePyc = false
	opt.PkgsDir = filepath.Join(shared, "cache")
	if os.Geteuid() != 0 {
		hits := c.totalHits()
		prefix := filepath.Join(t.TempDir(), "env")
		if err := InstallLock(context.Background(), prefix, lf, opt); !errors.Is(err, fetch.ErrUnsafeCache) {
			t.Fatalf("cache through a symlink in a world-writable directory: %v, want ErrUnsafeCache", err)
		}
		if c.totalHits() != hits {
			t.Error("downloaded into the redirected cache")
		}
		if ents, _ := os.ReadDir(private); len(ents) != 0 {
			t.Fatalf("the symlink was followed: %v", ents)
		}
		assertNoPrefix(t, prefix)
	}
	must(t, os.Chmod(shared, 0o777|fs.ModeSticky))
	evil := filepath.Join(root, "evil")
	opt.Progress = func(e Event) {
		if e.Kind != EventFetched {
			return
		}
		copyTree(t, private, evil)
		matches, _ := filepath.Glob(filepath.Join(evil, "*", "pkg", "bin", "tool"))
		for _, m := range matches {
			must(t, os.WriteFile(m, []byte("#!/bin/sh\necho pwned\n"), 0o755))
		}
		tmp := filepath.Join(shared, "cache.new")
		must(t, os.Symlink(evil, tmp))
		must(t, os.Rename(tmp, filepath.Join(shared, "cache")))
	}
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, InstallLock(context.Background(), prefix, lf, opt))
	got, err := os.ReadFile(filepath.Join(prefix, "bin", "tool"))
	must(t, err)
	if strings.Contains(string(got), "pwned") {
		t.Errorf("installed bytes from the re-pointed cache:\n%s", got)
	}
}

// TestPrefixAncestors (N4): a directory above the prefix that others could
// change is refused before anything is created; a sticky one (as /tmp) is
// fine; missing parents are created 0755.
func TestPrefixAncestors(t *testing.T) {
	skipIfRoot(t)
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	t.Run("writable parent", func(t *testing.T) {
		shared := filepath.Join(t.TempDir(), "shared")
		must(t, os.Mkdir(shared, 0o777))
		must(t, os.Chmod(shared, 0o777))
		prefix := filepath.Join(shared, "a", "env")
		hits := c.totalHits()
		err := InstallLock(context.Background(), prefix, lf, c.options(t))
		if !errors.Is(err, ErrUnsafePrefix) {
			t.Fatalf("err = %v, want ErrUnsafePrefix", err)
		}
		if c.totalHits() != hits {
			t.Error("downloaded for an unsafe prefix")
		}
		if ents, _ := os.ReadDir(shared); len(ents) != 0 {
			t.Errorf("created %v", ents)
		}
	})
	t.Run("sticky parent", func(t *testing.T) {
		shared := filepath.Join(t.TempDir(), "sticky")
		must(t, os.Mkdir(shared, 0o777))
		must(t, os.Chmod(shared, 0o777|fs.ModeSticky))
		must(t, InstallLock(context.Background(), filepath.Join(shared, "env"), lf, c.options(t)))
	})
	t.Run("new parents", func(t *testing.T) {
		root := t.TempDir()
		opt := c.options(t)
		old := syscall.Umask(0)
		defer syscall.Umask(old)
		prefix := filepath.Join(root, "a", "b", "env")
		must(t, InstallLock(context.Background(), prefix, lf, opt))
		for _, d := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), prefix} {
			fi, err := os.Stat(d)
			must(t, err)
			if fi.Mode().Perm() != 0o755 {
				t.Errorf("%s: mode %v, want 0755", d, fi.Mode().Perm())
			}
		}
		// Directories link made under umask 0 lose group and other write.
		must(t, filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				if fi, _ := d.Info(); fi.Mode().Perm()&0o022 != 0 {
					t.Errorf("%s: mode %v", p, fi.Mode().Perm())
				}
			}
			return err
		}))
	})
}

// TestPrefixViaForeignSymlink (S1): the prefix is named through a symlink
// in a shared sticky directory. goomba resolves the prefix once, checks the
// resolved path and installs there, so re-pointing the symlink during the
// install changes nothing. A symlink in a directory others can write
// without the sticky bit is not followed at all (WP8b review round 1, S1).
func TestPrefixViaForeignSymlink(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	must(t, os.Mkdir(real, 0o755))
	other := filepath.Join(root, "other")
	must(t, os.Mkdir(other, 0o755))
	shared := filepath.Join(root, "shared")
	must(t, os.Mkdir(shared, 0o777))
	must(t, os.Chmod(shared, 0o777))
	link := filepath.Join(shared, "env")
	must(t, os.Symlink(real, link))
	if os.Geteuid() != 0 {
		hits := c.totalHits()
		if err := InstallLock(context.Background(), link, lf, c.options(t)); !errors.Is(err, ErrUnsafePrefix) {
			t.Fatalf("prefix through a symlink in a world-writable directory: %v, want ErrUnsafePrefix", err)
		}
		if c.totalHits() != hits {
			t.Error("downloaded for an unsafe prefix")
		}
		if ents, _ := os.ReadDir(real); len(ents) != 0 {
			t.Fatalf("the symlink was followed: %v", ents)
		}
	}
	must(t, os.Chmod(shared, 0o777|fs.ModeSticky))
	opt := c.options(t)
	opt.Progress = func(e Event) {
		if e.Kind == EventLinked {
			_ = os.Remove(link)
			_ = os.Symlink(other, link)
		}
	}
	must(t, InstallLock(context.Background(), link, lf, opt))
	if _, err := os.Stat(filepath.Join(real, "conda-meta", "history")); err != nil {
		t.Errorf("not installed in the resolved prefix: %v", err)
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Errorf("the re-pointed directory was used: %v", ents)
	}
	hist, err := os.ReadFile(filepath.Join(real, "conda-meta", "history"))
	must(t, err)
	if !strings.Contains(string(hist), "# cmd: goomba create -p "+real+"\n") {
		t.Errorf("history does not name the resolved prefix:\n%s", hist)
	}
}

// symlinkedPython builds a "python" package whose bin/python3.11 is a
// symlink: to another file of the same package, or to
// libexec/python3.11-real installed by a second package "python-bin".
func symlinkedPython(t *testing.T, logDir string, samePackage bool) []testPkg {
	t.Helper()
	script := fakePythonScript(logDir, 0)
	if samePackage {
		return []testPkg{buildPkg(t, tspec{name: "python", version: "3.11.9", build: "h0_cpython", files: []tfile{
			{path: "libexec/python3.11-real", data: script, mode: 0o755},
			{path: "bin/python3.11", link: "../libexec/python3.11-real"},
			{path: "bin/python", link: "python3.11"},
		}})}
	}
	return []testPkg{
		buildPkg(t, tspec{name: "python", version: "3.11.9", build: "h0_cpython", depends: []string{"python-bin"}, files: []tfile{
			{path: "bin/python3.11", link: "../libexec/python3.11-real"},
		}}),
		buildPkg(t, tspec{name: "python-bin", version: "3.11.9", build: "h0", files: []tfile{
			{path: "libexec/python3.11-real", data: script, mode: 0o755},
		}}),
	}
}

// TestSymlinkedInterpreter (N5): an interpreter that is a symlink into
// another file of the environment is followed to the package that owns
// the file and checked against that package's cache file; a replaced file
// is refused, and a file no package installed is too.
func TestSymlinkedInterpreter(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same package", false: "other package"}[same], func(t *testing.T) {
			logDir := t.TempDir()
			c := newChannel(t)
			pkgs := append(symlinkedPython(t, logDir, same), cancelEnv(t, testPkg{})[1:]...)
			lf := c.lockfile(t, nil, pkgs...)
			prefix := filepath.Join(t.TempDir(), "env")
			must(t, InstallLock(context.Background(), prefix, lf, c.options(t)))
			if _, err := os.Stat(filepath.Join(logDir, "info")); err != nil {
				t.Errorf("the interpreter did not run: %v", err)
			}
			for _, how := range []string{"replaced", "unowned"} {
				opt := c.options(t)
				prefix := filepath.Join(t.TempDir(), "env")
				ran := filepath.Join(t.TempDir(), "ran")
				opt.Progress = func(e Event) {
					if e.Kind != EventCompiling {
						return
					}
					bad := []byte("#!/bin/sh\necho unverified > '" + ran + "'\n")
					if how == "replaced" {
						real := filepath.Join(prefix, "libexec", "python3.11-real")
						must(t, os.Remove(real))
						must(t, os.WriteFile(real, bad, 0o755))
						return
					}
					must(t, os.WriteFile(filepath.Join(prefix, "libexec", "planted"), bad, 0o755))
					must(t, os.Remove(filepath.Join(prefix, "bin", "python3.11")))
					must(t, os.Symlink("../libexec/planted", filepath.Join(prefix, "bin", "python3.11")))
				}
				err := InstallLock(context.Background(), prefix, lf, opt)
				if !errors.Is(err, ErrCompilePyc) {
					t.Errorf("%s: err = %v, want ErrCompilePyc", how, err)
				}
				if _, err := os.Stat(ran); err == nil {
					t.Errorf("%s: an unverified interpreter ran", how)
				}
			}
		})
	}
}

// TestUnsafeCacheIsRefused keeps the round-1 guarantee visible here too: a
// cache below a directory others can write is refused.
func TestUnsafeCacheIsRefused(t *testing.T) {
	skipIfRoot(t)
	c := newChannel(t)
	lf := c.lockfile(t, nil, buildPkg(t, tspec{name: "a", version: "1", build: "0"}))
	shared := filepath.Join(t.TempDir(), "shared")
	must(t, os.Mkdir(shared, 0o777))
	must(t, os.Chmod(shared, 0o777))
	opt := c.options(t)
	opt.PkgsDir = filepath.Join(shared, "pkgs")
	if err := InstallLock(context.Background(), filepath.Join(t.TempDir(), "env"), lf, opt); !errors.Is(err, fetch.ErrUnsafeCache) {
		t.Errorf("err = %v, want fetch.ErrUnsafeCache", err)
	}
}

// TestNoGroupWritableFiles (S2): a file an archive ships 0664 is cached and
// installed without group write, whatever the umask, so a member of a
// shared group cannot rewrite it in place.
func TestNoGroupWritableFiles(t *testing.T) {
	c := newChannel(t)
	pkg := buildPkg(t, tspec{name: "gw", version: "1", build: "0", files: []tfile{
		{path: "lib/libgw.so", data: "genuine", mode: 0o664},
		{path: "bin/gw", data: "#!/bin/sh\n", mode: 0o777},
	}})
	lf := c.lockfile(t, nil, pkg)
	opt := c.options(t)
	opt.CompilePyc = false
	prefix := filepath.Join(t.TempDir(), "env")
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	must(t, InstallLock(context.Background(), prefix, lf, opt))
	fi, err := os.Stat(filepath.Join(opt.PkgsDir, lf.Packages[0].SHA256, "pkg", "lib", "libgw.so"))
	must(t, err)
	if fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("cached file: mode %v", fi.Mode().Perm())
	}
	// Nothing in the environment, package files, conda-meta records and
	// history included, is writable by group or others.
	n := 0
	must(t, filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		n++
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s: mode %v", p, fi.Mode().Perm())
		}
		return nil
	}))
	if n < 6 {
		t.Errorf("walked only %d entries", n)
	}
}

// TestEmptyDirLarge (round 2 probe): the rollback's emptyDir removes
// everything, including a directory with thousands of entries (removed
// while being read), read-only directories, and symlinks to data outside
// (the links only).
func TestEmptyDirLarge(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "env")
	outside := filepath.Join(root, "outside")
	must(t, os.MkdirAll(filepath.Join(outside, "keep"), 0o755))
	must(t, os.WriteFile(filepath.Join(outside, "keep", "data"), []byte("x"), 0o644))
	big := filepath.Join(prefix, "lib", "big")
	must(t, os.MkdirAll(big, 0o755))
	for i := 0; i < 20000; i++ {
		must(t, os.WriteFile(filepath.Join(big, "f"+strconv.Itoa(i)+"-with-a-longer-name-to-fill-getdents-buffers"), nil, 0o644))
	}
	ro := filepath.Join(prefix, "share", "ro", "deeper")
	must(t, os.MkdirAll(ro, 0o755))
	must(t, os.WriteFile(filepath.Join(ro, "f"), nil, 0o444))
	must(t, os.Chmod(filepath.Join(prefix, "share", "ro"), 0o555))
	must(t, os.Symlink(filepath.Join(outside, "keep"), filepath.Join(prefix, "lib", "out")))
	d, err := lockDir(prefix)
	must(t, err)
	defer unlockDir(d)
	if err := emptyDir(d); err != nil {
		t.Errorf("emptyDir: %v", err)
	}
	if ents, _ := os.ReadDir(prefix); len(ents) != 0 {
		var n []string
		for _, e := range ents {
			n = append(n, e.Name())
		}
		t.Errorf("left behind %v", n)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep", "data")); err != nil {
		t.Errorf("data outside removed: %v", err)
	}
}

// TestPrefixPrivateDuringInstall (review round 3, S1): while the
// transaction runs, the prefix directory is 0700, so no other user can
// reach the directories link and the interpreter create (0777 under umask
// 0); at the end it is 0755, or the previous mode of an existing prefix,
// also after a rollback.
func TestPrefixPrivateDuringInstall(t *testing.T) {
	c, cb := newChannel(t), newChannel(t)
	good := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	bad := cb.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 3))...)
	for _, tc := range []struct {
		name     string
		existing fs.FileMode // 0: the install creates the prefix
		lf       *channel
		want     fs.FileMode
	}{
		{"created", 0, c, 0o755},
		{"existing 0750", 0o750, c, 0o750},
		{"existing 0700, rolled back", 0o700, cb, 0o700},
		{"existing 0755, rolled back", 0o755, cb, 0o755},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := good
			if tc.lf == cb {
				lf = bad
			}
			opt := tc.lf.options(t)
			prefix := filepath.Join(t.TempDir(), "env")
			if tc.existing != 0 {
				must(t, os.Mkdir(prefix, 0o700))
				must(t, os.Chmod(prefix, tc.existing))
			}
			old := syscall.Umask(0)
			defer syscall.Umask(old)
			var seen []string
			opt.Progress = func(e Event) {
				if e.Kind == EventLinked || e.Kind == EventCompiling || e.Kind == EventCompiled {
					fi, err := os.Stat(prefix)
					if err == nil && fi.Mode().Perm() != 0o700 {
						seen = append(seen, e.Kind.String()+" "+fi.Mode().Perm().String())
					}
				}
			}
			err := InstallLock(context.Background(), prefix, lf, opt)
			if (err != nil) != (tc.lf == cb) {
				t.Fatalf("err = %v", err)
			}
			if len(seen) > 0 {
				t.Errorf("prefix not private during the install: %v", seen)
			}
			fi, err := os.Stat(prefix)
			if tc.existing == 0 && tc.lf == cb {
				if err == nil {
					t.Error("prefix left after rollback")
				}
				return
			}
			must(t, err)
			if fi.Mode().Perm() != tc.want {
				t.Errorf("final mode %v, want %v", fi.Mode().Perm(), tc.want)
			}
		})
	}
}

// TestLinkWindowUmask is the reviewer's round-3 probe: under umask 0 the
// directories link creates are 0777 until the hardening walk. Other users
// must not be able to reach them meanwhile: the prefix directory above
// them grants nothing to group or others during the link window, and the
// finished tree has no group- or other-writable directory.
func TestLinkWindowUmask(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	opt := c.options(t)
	opt.Concurrency = 1
	parent := t.TempDir()
	must(t, os.Chmod(parent, 0o755))
	must(t, os.MkdirAll(opt.PkgsDir, 0o755))
	prefix := filepath.Join(parent, "env")
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	var n atomic.Int64
	var rootMode, dirMode fs.FileMode
	opt.Progress = func(e Event) {
		if e.Kind != EventLinked || n.Add(1) != int64(len(lf.Packages)) {
			return
		}
		if fi, err := os.Stat(prefix); err == nil {
			rootMode = fi.Mode().Perm()
		}
		if fi, err := os.Stat(filepath.Join(prefix, "share", "c", "d")); err == nil {
			dirMode = fi.Mode().Perm()
		}
	}
	must(t, InstallLock(context.Background(), prefix, lf, opt))
	if dirMode&0o002 != 0 && rootMode&0o077 != 0 {
		t.Errorf("a world-writable directory (%v) was reachable through the prefix (%v) during linking", dirMode, rootMode)
	}
	fi, err := os.Stat(filepath.Join(prefix, "share", "c", "d"))
	must(t, err)
	if fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("final mode %v", fi.Mode().Perm())
	}
}
