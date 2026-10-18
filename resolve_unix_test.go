//go:build unix

package goomba

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/richinsley/goomba/fetch"
	"github.com/richinsley/goomba/repodata"
)

// TestLockUnsafeRepodataDir (WP8b review round 1, S2): an index cache that
// another user could write is refused by Lock and Create with
// repodata.ErrUnsafeCache, before any request and without creating the
// prefix.
func TestLockUnsafeRepodataDir(t *testing.T) {
	skipIfRoot(t)
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.publishSharded(t, plat)
	for _, tc := range []struct {
		name  string
		setup func(opt *LockOptions)
	}{
		{"directory 0777", func(opt *LockOptions) { must(t, os.Chmod(opt.RepodataDir, 0o777)) }},
		{"parent 0777", func(opt *LockOptions) {
			parent := filepath.Join(t.TempDir(), "shared")
			must(t, os.Mkdir(parent, 0o755))
			must(t, os.Chmod(parent, 0o777))
			opt.RepodataDir = filepath.Join(parent, "cache")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := s.lockOptions(t, []string{"foo"}, ch)
			tc.setup(&opt)
			before := s.requests()
			if _, err := Lock(ctxT(t), opt); !errors.Is(err, repodata.ErrUnsafeCache) {
				t.Errorf("Lock: %v, want ErrUnsafeCache", err)
			}
			prefix := filepath.Join(t.TempDir(), "env")
			_, err := Create(ctxT(t), prefix, opt, InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()})
			if !errors.Is(err, repodata.ErrUnsafeCache) {
				t.Errorf("Create: %v, want ErrUnsafeCache", err)
			}
			assertNoPrefix(t, prefix)
			if n := s.requests() - before; n != 0 {
				t.Errorf("%d requests", n)
			}
		})
	}
}

// TestLockCacheThroughSharedSymlink (WP8b review round 1, S1): a
// RepodataDir reached through a symlink in a directory others can write
// (not sticky) is refused, online and offline: another user could point it
// at an index of theirs (crafted sha256 and base_url) or at a directory of
// the victim's. Nothing in the symlink's target is used or changed.
func TestLockCacheThroughSharedSymlink(t *testing.T) {
	skipIfRoot(t)
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.publishSharded(t, plat)
	// A warm cache for the channel, and a victim's directory holding a
	// file named like a cache subdirectory.
	warm := filepath.Join(t.TempDir(), "cache")
	opt := s.lockOptions(t, []string{"foo"}, ch)
	opt.RepodataDir = warm
	_, err := Lock(ctxT(t), opt)
	must(t, err)
	victim := filepath.Join(t.TempDir(), "project")
	must(t, os.Mkdir(victim, 0o755))
	must(t, os.WriteFile(filepath.Join(victim, "shards"), []byte("precious"), 0o644))

	shared := filepath.Join(t.TempDir(), "shared")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Chmod(shared, 0o777))
	link := filepath.Join(shared, "goomba-cache")
	for _, target := range []string{warm, victim} {
		_ = os.Remove(link)
		must(t, os.Symlink(target, link))
		before := s.requests()
		for _, offline := range []bool{true, false} {
			o := opt
			o.RepodataDir, o.Offline = link, offline
			if _, err := Lock(ctxT(t), o); !errors.Is(err, repodata.ErrUnsafeCache) {
				t.Errorf("through the symlink to %s, offline=%v: %v, want ErrUnsafeCache", filepath.Base(target), offline, err)
			}
		}
		if n := s.requests() - before; n != 0 {
			t.Errorf("%d requests", n)
		}
	}
	if data, err := os.ReadFile(filepath.Join(victim, "shards")); err != nil || string(data) != "precious" {
		t.Errorf("the victim's file: %q, %v", data, err)
	}
	if ents, _ := os.ReadDir(victim); len(ents) != 1 {
		t.Errorf("the victim's directory changed: %v", ents)
	}
}

// TestCreateUnsafePrefixFirst (review round 1, N4): a prefix under a
// directory others can write is refused by Create before any request.
func TestCreateUnsafePrefixFirst(t *testing.T) {
	skipIfRoot(t)
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.publishSharded(t, plat)
	shared := filepath.Join(t.TempDir(), "shared")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Chmod(shared, 0o777))
	prefix := filepath.Join(shared, "env")
	inst := InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()}
	if err := ValidateInstall(prefix, inst); !errors.Is(err, ErrUnsafePrefix) {
		t.Errorf("ValidateInstall: %v", err)
	}
	if _, err := Create(ctxT(t), prefix, s.lockOptions(t, []string{"foo"}, ch), inst); !errors.Is(err, ErrUnsafePrefix) {
		t.Errorf("Create: %v", err)
	}
	if n := s.requests(); n != 0 {
		t.Errorf("%d requests", n)
	}
	if ents, _ := os.ReadDir(shared); len(ents) != 0 {
		t.Errorf("created %v", ents)
	}
}

// fooChannel serves one small package, foo, for install tests.
func fooChannel(t *testing.T) (*specServer, *specChannel) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.add(t, buildPkg(t, tspec{name: "foo", version: "1.0", build: "0", files: []tfile{{path: "share/foo", data: "foo"}}}))
	ch.publishSharded(t, plat)
	return s, ch
}

// TestPrefixDotDotAfterMissing (review round 2, S1): "base/missing/../x",
// where x is the user's symlink to a directory under a world-writable
// parent, is refused like "base/x": the kernel cannot resolve ".." in a
// missing directory, and neither does goomba. Nothing is installed or
// created, and nothing is requested.
func TestPrefixDotDotAfterMissing(t *testing.T) {
	skipIfRoot(t)
	s, ch := fooChannel(t)
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Chmod(shared, 0o777))
	target := filepath.Join(shared, "env")
	must(t, os.Mkdir(target, 0o755))
	base := filepath.Join(root, "base")
	must(t, os.Mkdir(base, 0o755))
	must(t, os.Symlink(target, filepath.Join(base, "x")))
	inst := InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()}
	for _, p := range []string{filepath.Join(base, "x"), base + "/missing/../x", base + "/x/../x"} {
		if err := ValidateInstall(p, inst); err == nil {
			t.Errorf("ValidateInstall(%s) passed", p)
		}
		if _, err := Create(ctxT(t), p, s.lockOptions(t, []string{"foo"}, ch), inst); err == nil {
			t.Errorf("Create(%s) succeeded", p)
		}
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("installed into the shared directory: %v", ents)
	}
	if ents, _ := os.ReadDir(base); len(ents) != 1 {
		t.Errorf("created %v", ents)
	}
	if n := s.requests(); n != 0 {
		t.Errorf("%d requests", n)
	}
}

// TestValidateInstallOwnDir (review round 2, S2): an existing empty prefix
// that passes the rule for directories on the way (sticky, or root's) but
// is not the user's own private directory is refused by ValidateInstall,
// so Create refuses it before resolving, as InstallLock would after.
func TestValidateInstallOwnDir(t *testing.T) {
	skipIfRoot(t)
	s, ch := fooChannel(t)
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, os.Mkdir(prefix, 0o755))
	must(t, os.Chmod(prefix, 0o777|fs.ModeSticky))
	inst := InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()}
	if err := ValidateInstall(prefix, inst); !errors.Is(err, ErrUnsafePrefix) {
		t.Errorf("ValidateInstall: %v, want ErrUnsafePrefix", err)
	}
	if _, err := Create(ctxT(t), prefix, s.lockOptions(t, []string{"foo"}, ch), inst); !errors.Is(err, ErrUnsafePrefix) {
		t.Errorf("Create: %v, want ErrUnsafePrefix", err)
	}
	if n := s.requests(); n != 0 {
		t.Errorf("%d requests", n)
	}
	// The same directory with something in it: not empty comes first, as
	// in InstallLock.
	must(t, os.WriteFile(filepath.Join(prefix, "x"), nil, 0o644))
	if err := ValidateInstall(prefix, inst); !errors.Is(err, ErrPrefixNotEmpty) {
		t.Errorf("non-empty: %v, want ErrPrefixNotEmpty", err)
	}
}

// TestPrefixDanglingSymlink (review round 2, N1): a prefix that is a
// dangling symlink fails (EEXIST, as mkdir(2) says) instead of creating
// the environment at the link's missing target; a symlink to an existing
// empty directory of the user's still works.
func TestPrefixDanglingSymlink(t *testing.T) {
	s, ch := fooChannel(t)
	root := t.TempDir()
	missing := filepath.Join(root, "elsewhere", "env")
	link := filepath.Join(root, "dl")
	must(t, os.Symlink(missing, link))
	inst := InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()}
	if err := ValidateInstall(link, inst); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("ValidateInstall: %v, want EEXIST", err)
	}
	if _, err := Create(ctxT(t), link, s.lockOptions(t, []string{"foo"}, ch), inst); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("Create: %v, want EEXIST", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "elsewhere")); !os.IsNotExist(err) {
		t.Errorf("the link's target was created: %v", err)
	}
	real := filepath.Join(root, "real")
	must(t, os.Mkdir(real, 0o755))
	good := filepath.Join(root, "good")
	must(t, os.Symlink(real, good))
	_, err := Create(ctxT(t), good, s.lockOptions(t, []string{"foo"}, ch), inst)
	must(t, err)
	if data, err := os.ReadFile(filepath.Join(real, "share", "foo")); err != nil || string(data) != "foo" {
		t.Errorf("installed: %q, %v", data, err)
	}
}

// TestOpenPrefixRefusesSymlink (review round 2, S1): a symlink planted at
// the resolved prefix after it was resolved (the race of a sticky
// directory without protected_symlinks) is not followed: the prefix is
// opened with O_NOFOLLOW and compared with Lstat.
func TestOpenPrefixRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	must(t, os.Mkdir(victim, 0o755))
	prefix := filepath.Join(root, "env")
	must(t, os.Symlink(victim, prefix))
	if tx, err := openPrefix(prefix); err == nil {
		tx.rollback()
		t.Fatal("openPrefix followed a symlink")
	}
	if ents, _ := os.ReadDir(victim); len(ents) != 0 {
		t.Errorf("wrote into the symlink's target: %v", ents)
	}
	if err := checkPrefixFree(prefix); !errors.Is(err, ErrPrefixNotEmpty) {
		t.Errorf("checkPrefixFree(symlink): %v", err)
	}
}

// TestValidateInstallPkgsDir (review round 2, N4): an unsafe package cache
// is refused by ValidateInstall, and so by Create before resolving, with
// fetch.ErrUnsafeCache; a missing one is fine.
func TestValidateInstallPkgsDir(t *testing.T) {
	skipIfRoot(t)
	s, ch := fooChannel(t)
	shared := filepath.Join(t.TempDir(), "shared")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Chmod(shared, 0o777))
	own := filepath.Join(t.TempDir(), "pkgs")
	must(t, os.Mkdir(own, 0o755))
	must(t, os.Chmod(own, 0o777))
	for _, pkgs := range []string{filepath.Join(shared, "pkgs"), own} {
		inst := InstallOptions{PkgsDir: pkgs, Client: s.client()}
		prefix := filepath.Join(t.TempDir(), "env")
		if err := ValidateInstall(prefix, inst); !errors.Is(err, fetch.ErrUnsafeCache) {
			t.Errorf("ValidateInstall(PkgsDir %s): %v, want fetch.ErrUnsafeCache", pkgs, err)
		}
		if _, err := Create(ctxT(t), prefix, s.lockOptions(t, []string{"foo"}, ch), inst); !errors.Is(err, fetch.ErrUnsafeCache) {
			t.Errorf("Create(PkgsDir %s): %v, want fetch.ErrUnsafeCache", pkgs, err)
		}
	}
	if n := s.requests(); n != 0 {
		t.Errorf("%d requests", n)
	}
	if ents, _ := os.ReadDir(shared); len(ents) != 0 {
		t.Errorf("created %v", ents)
	}
	if err := ValidateInstall(filepath.Join(t.TempDir(), "env"), InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "new", "pkgs")}); err != nil {
		t.Errorf("a missing package cache: %v", err)
	}
}
