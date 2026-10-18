package goomba

// Regression tests for the findings of review round 1 (B1, S1, S4, S5, S6,
// N4, N6), ported from the reviewer's probes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/richinsley/goomba/fetch"
	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/virtual"
)

func skipIfRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
}

// copyTree copies a directory tree (files, directories, symlinks), as a user
// with read access to a cache could.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	must(t, filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			return os.MkdirAll(out, 0o777)
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, out)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, fi.Mode().Perm()|0o200)
		}
	}))
}

// TestCachePoisonRefused (B1): a package cache another user can write is
// refused before anything is installed from it, so a tree swapped in with
// a copy of the (readable) marker is never installed.
func TestCachePoisonRefused(t *testing.T) {
	skipIfRoot(t)
	c := newChannel(t)
	victim := buildPkg(t, tspec{name: "victim", version: "1", build: "0", files: []tfile{
		{path: "bin/tool", data: "#!/bin/sh\necho genuine\n", mode: 0o755},
	}})
	lf := c.lockfile(t, nil, victim)
	sha := lf.Packages[0].SHA256
	opt := c.options(t)
	opt.CompilePyc = false

	// Installed once from a private cache.
	must(t, InstallLock(context.Background(), filepath.Join(t.TempDir(), "env1"), lf, opt))
	// The cache becomes writable by others, who swap in a tree of their own.
	shaDir := filepath.Join(opt.PkgsDir, sha)
	must(t, os.Chmod(opt.PkgsDir, 0o777))
	must(t, os.Chmod(shaDir, 0o777))
	tree := filepath.Join(shaDir, "pkg")
	evil := filepath.Join(shaDir, "evil")
	copyTree(t, tree, evil)
	must(t, os.WriteFile(filepath.Join(evil, "bin", "tool"), []byte("#!/bin/sh\necho pwned\n"), 0o755))
	must(t, os.Rename(tree, filepath.Join(shaDir, "old")))
	must(t, os.Rename(evil, tree))

	opt2 := c.options(t)
	opt2.PkgsDir, opt2.CompilePyc = opt.PkgsDir, false
	prefix2 := filepath.Join(t.TempDir(), "env2")
	err := InstallLock(context.Background(), prefix2, lf, opt2)
	if !errors.Is(err, fetch.ErrUnsafeCache) {
		t.Fatalf("err = %v, want fetch.ErrUnsafeCache", err)
	}
	assertNoPrefix(t, prefix2)

	// A cache pre-created writable by others is refused on first use.
	opt3 := c.options(t)
	opt3.CompilePyc = false
	must(t, os.MkdirAll(filepath.Join(opt3.PkgsDir, sha), 0o777))
	must(t, os.Chmod(filepath.Join(opt3.PkgsDir, sha), 0o777))
	err = InstallLock(context.Background(), filepath.Join(t.TempDir(), "env3"), lf, opt3)
	if !errors.Is(err, fetch.ErrUnsafeCache) {
		t.Fatalf("pre-created shared content directory: err = %v, want fetch.ErrUnsafeCache", err)
	}
}

// TestPthDoesNotRunAtInstall (S1): byte-compilation runs the real
// interpreter isolated, so a .pth file or sitecustomize.py shipped by a
// locked package does not execute during InstallLock. It needs the real
// python archive from $GOOMBA_REF_ROOT/pkgs.
func TestPthDoesNotRunAtInstall(t *testing.T) {
	root := os.Getenv("GOOMBA_REF_ROOT")
	pyArchive := filepath.Join(root, "pkgs", "python-3.11.15-hd63d673_0_cpython.conda")
	if root == "" || hostPlatform(t) != "linux-64" {
		t.Skip("set GOOMBA_REF_ROOT (linux-64)")
	}
	if _, err := os.Stat(pyArchive); err != nil {
		t.Skipf("no %s", pyArchive)
	}
	python := archivePkg(t, pyArchive)
	ran := filepath.Join(t.TempDir(), "ran")
	payload := "import os; open(" + strconv.Quote(ran) + ", 'a').write('code ran at install time\\n')\n"
	evil := buildPkg(t, tspec{
		name: "evilpth", version: "1", build: "pyh_0", subdir: "noarch", noarch: "python", depends: []string{"python"},
		files: []tfile{
			{path: "site-packages/zz-evil.pth", data: payload},
			{path: "site-packages/sitecustomize.py", data: payload},
			{path: "site-packages/usercustomize.py", data: payload},
			{path: "site-packages/evilpkg/__init__.py", data: "x = 1\n"},
		},
	})
	c := newChannel(t)
	lf := c.lockfile(t, nil, python, evil)
	opt := c.options(t)
	opt.Concurrency = 2
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, InstallLock(context.Background(), prefix, lf, opt))
	if b, err := os.ReadFile(ran); err == nil {
		t.Errorf("package code ran during InstallLock: %q", b)
	}
	if _, err := os.Stat(filepath.Join(prefix, "lib/python3.11/site-packages/evilpkg/__pycache__/__init__.cpython-311.pyc")); err != nil {
		t.Errorf("not byte-compiled: %v", err)
	}
	// Control: the same interpreter run normally does execute the .pth.
	out, err := exec.Command(filepath.Join(prefix, "bin", "python3.11"), "-c", "pass").CombinedOutput()
	if err != nil {
		t.Fatalf("control run: %v\n%s", err, out)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Errorf("control: the .pth did not run without isolation, so the test proves nothing: %v", err)
	}
}

// TestForeignPrefixRefused (S5): an existing empty prefix that others may
// write is refused before anything runs.
func TestForeignPrefixRefused(t *testing.T) {
	skipIfRoot(t)
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	for _, mode := range []fs.FileMode{0o777, 0o757} {
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.Mkdir(prefix, 0o755))
		must(t, os.Chmod(prefix, mode))
		err := InstallLock(context.Background(), prefix, lf, c.options(t))
		if !errors.Is(err, ErrUnsafePrefix) {
			t.Fatalf("mode %v: err = %v, want ErrUnsafePrefix", mode, err)
		}
		if ents, _ := os.ReadDir(prefix); len(ents) != 0 {
			t.Errorf("mode %v: refused prefix touched: %v", mode, ents)
		}
	}
}

// TestUnsafePathError: the refusals of a prefix and of a package cache that
// other users could change carry an *UnsafePathError naming the offending
// directory, for callers that tell the user what to fix.
func TestUnsafePathError(t *testing.T) {
	skipIfRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("ownership is not checked on Windows")
	}
	c := newChannel(t)
	shared := filepath.Join(t.TempDir(), "shared")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Chmod(shared, 0o777)) // writable by others, not sticky
	sharedReal, err := filepath.EvalSymlinks(shared)
	must(t, err)
	opt := c.options(t)
	for name, tc := range map[string]struct {
		err  func() error
		is   error
		path string
	}{
		"prefix below it": {func() error { return ValidateInstall(filepath.Join(shared, "env"), opt) }, ErrUnsafePrefix, sharedReal},
		"prefix status":   {func() error { return CheckComplete(filepath.Join(shared, "env")) }, ErrUnsafePrefix, sharedReal},
		"package cache below it": {func() error {
			o := opt
			o.PkgsDir = filepath.Join(shared, "pkgs")
			return ValidateInstall(filepath.Join(t.TempDir(), "env"), o)
		}, fetch.ErrUnsafeCache, sharedReal},
	} {
		err := tc.err()
		var upe *UnsafePathError
		switch {
		case !errors.Is(err, tc.is):
			t.Errorf("%s: %v, want %v", name, err, tc.is)
		case !errors.As(err, &upe):
			t.Errorf("%s: %v carries no *UnsafePathError", name, err)
		case upe.Path != tc.path:
			t.Errorf("%s: Path = %q, want %q", name, upe.Path, tc.path)
		}
	}
}

// TestPrivateGroupRule (S5, B1): a group-writable directory is the user's
// own when it belongs to the user and to the user's private group (gid ==
// uid == egid); with any other group it is refused, for prefixes and package
// caches alike.
func TestPrivateGroupRule(t *testing.T) {
	skipIfRoot(t)
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	upg := os.Getegid() == os.Geteuid()
	t.Run("private group", func(t *testing.T) {
		if !upg {
			t.Skip("the user's group id differs from its user id")
		}
		parent := filepath.Join(t.TempDir(), "shared")
		must(t, os.Mkdir(parent, 0o775))
		must(t, os.Chmod(parent, 0o775))
		prefix := filepath.Join(parent, "env")
		must(t, os.Mkdir(prefix, 0o775))
		must(t, os.Chmod(prefix, 0o775))
		opt := c.options(t)
		opt.PkgsDir = filepath.Join(parent, "pkgs")
		must(t, os.Mkdir(opt.PkgsDir, 0o775))
		must(t, os.Chmod(opt.PkgsDir, 0o775))
		must(t, InstallLock(context.Background(), prefix, lf, opt))
	})
	t.Run("other group", func(t *testing.T) {
		other := -1
		groups, err := os.Getgroups()
		must(t, err)
		for _, g := range groups {
			if g != os.Getegid() && g != os.Geteuid() {
				other = g
				break
			}
		}
		if other < 0 {
			t.Skip("the user belongs to no other group")
		}
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, os.Mkdir(prefix, 0o775))
		must(t, os.Chmod(prefix, 0o775))
		must(t, os.Chown(prefix, -1, other))
		if err := InstallLock(context.Background(), prefix, lf, c.options(t)); !errors.Is(err, ErrUnsafePrefix) {
			t.Errorf("prefix: err = %v, want ErrUnsafePrefix", err)
		}
		opt := c.options(t)
		parent := filepath.Join(t.TempDir(), "shared")
		must(t, os.Mkdir(parent, 0o775))
		must(t, os.Chmod(parent, 0o775))
		must(t, os.Chown(parent, -1, other))
		opt.PkgsDir = filepath.Join(parent, "pkgs")
		if err := InstallLock(context.Background(), filepath.Join(t.TempDir(), "env2"), lf, opt); !errors.Is(err, fetch.ErrUnsafeCache) {
			t.Errorf("cache below a shared directory: err = %v, want fetch.ErrUnsafeCache", err)
		}
	})
}

// TestInterpreterSwapRefused (B1): an interpreter replaced after linking is
// not run; an identical copy (another inode, the recorded digest) is.
func TestInterpreterSwapRefused(t *testing.T) {
	c := newChannel(t)
	logDir := t.TempDir()
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, logDir, 0))...)
	for _, identical := range []bool{false, true} {
		t.Run(map[bool]string{false: "replaced", true: "identical copy"}[identical], func(t *testing.T) {
			opt := c.options(t)
			prefix := filepath.Join(t.TempDir(), "env")
			ran := filepath.Join(t.TempDir(), "ran")
			opt.Progress = func(e Event) {
				if e.Kind != EventCompiling {
					return
				}
				exe := filepath.Join(prefix, "bin", "python3.11")
				data := []byte("#!/bin/sh\necho unverified > '" + ran + "'\n")
				if identical {
					var err error
					data, err = os.ReadFile(exe)
					must(t, err)
				}
				must(t, os.Remove(exe))
				must(t, os.WriteFile(exe, data, 0o755))
			}
			err := InstallLock(context.Background(), prefix, lf, opt)
			if identical {
				must(t, err)
				return
			}
			if !errors.Is(err, ErrCompilePyc) {
				t.Fatalf("err = %v, want ErrCompilePyc", err)
			}
			if _, err := os.Stat(ran); err == nil {
				t.Error("an unverified interpreter ran")
			}
			assertNoPrefix(t, prefix)
		})
	}
}

// TestRollbackSymlinkPrefix: a prefix that is a symlink to an empty
// directory is emptied on failure; the symlink and its neighbours stay.
func TestRollbackSymlinkPrefix(t *testing.T) {
	cb := newChannel(t)
	bad := cb.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 3))...)
	root := t.TempDir()
	target := filepath.Join(root, "real")
	must(t, os.Mkdir(target, 0o755))
	must(t, os.WriteFile(filepath.Join(root, "keep"), []byte("user data"), 0o644))
	prefix := filepath.Join(root, "env")
	must(t, os.Symlink("real", prefix))
	err := InstallLock(context.Background(), prefix, bad, cb.options(t))
	if !errors.Is(err, ErrCompilePyc) {
		t.Fatalf("err = %v", err)
	}
	if fi, err := os.Lstat(prefix); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink prefix gone: %v", err)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("target not emptied: %v", ents)
	}
	if b, err := os.ReadFile(filepath.Join(root, "keep")); err != nil || string(b) != "user data" {
		t.Errorf("neighbour damaged: %v", err)
	}
}

// TestRollbackParentSwapped (N9): the prefix's parent is replaced by a
// symlink to a directory holding user data under the same name during the
// install; the rollback, working through the locked handle, does not touch
// that data.
func TestRollbackParentSwapped(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "existing"}[existing], func(t *testing.T) {
			cb := newChannel(t)
			bad := cb.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 3))...)
			root := t.TempDir()
			parent := filepath.Join(root, "p")
			must(t, os.Mkdir(parent, 0o755))
			prefix := filepath.Join(parent, "env")
			if existing {
				must(t, os.Mkdir(prefix, 0o755))
			}
			victim := filepath.Join(root, "victim")
			must(t, os.MkdirAll(filepath.Join(victim, "env", "conda-meta"), 0o755))
			must(t, os.WriteFile(filepath.Join(victim, "env", "precious"), []byte("x"), 0o644))
			opt := cb.options(t)
			var once atomic.Bool
			opt.Progress = func(e Event) {
				if e.Kind == EventCompiling && once.CompareAndSwap(false, true) {
					must(t, os.Rename(parent, filepath.Join(root, "p-moved")))
					must(t, os.Symlink("victim", parent))
				}
			}
			err := InstallLock(context.Background(), prefix, bad, opt)
			if err == nil {
				t.Fatal("install succeeded")
			}
			if _, err := os.Stat(filepath.Join(victim, "env", "precious")); err != nil {
				t.Errorf("rollback deleted user data: %v", err)
			}
			// The moved prefix itself was emptied through its handle.
			if ents, _ := os.ReadDir(filepath.Join(root, "p-moved", "env")); len(ents) != 0 {
				t.Errorf("moved prefix not emptied: %v", ents)
			}
		})
	}
}

// TestVerifySymlinksSymlinkedPrefix (S4): a prefix path that is itself a
// symlink is walked.
func TestVerifySymlinksSymlinkedPrefix(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	must(t, os.MkdirAll(filepath.Join(real, "lib"), 0o755))
	must(t, os.Symlink("../../../../../../../../../../etc/passwd", filepath.Join(real, "lib", "escape")))
	alias := filepath.Join(root, "env")
	must(t, os.Symlink("real", alias))
	for _, p := range []string{real, alias} {
		if err := verifySymlinks(context.Background(), p); !errors.Is(err, link.ErrUnsafePath) {
			t.Errorf("verifySymlinks(%s) = %v, want ErrUnsafePath", filepath.Base(p), err)
		}
	}
}

// TestPkgsDirAliasesPrefix (N4): a package cache reaching into the prefix
// through a symlink is refused before anything is created.
func TestPkgsDirAliasesPrefix(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	for _, existing := range []bool{false, true} {
		root := t.TempDir()
		must(t, os.Mkdir(filepath.Join(root, "p"), 0o755))
		must(t, os.Symlink("p", filepath.Join(root, "alias")))
		prefix := filepath.Join(root, "p", "env")
		if existing {
			must(t, os.Mkdir(prefix, 0o755))
		}
		opt := c.options(t)
		opt.PkgsDir = filepath.Join(root, "alias", "env", "pkgs")
		err := InstallLock(context.Background(), prefix, lf, opt)
		if !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("existing=%v: err = %v, want ErrInvalidOptions", existing, err)
		}
		if existing {
			if ents, _ := os.ReadDir(prefix); len(ents) != 0 {
				t.Errorf("prefix touched: %v", ents)
			}
		} else {
			assertNoPrefix(t, prefix)
		}
	}
	for _, tc := range []struct {
		child, parent string
		want          bool
	}{
		{"/a/b", "/a", true}, {"/a", "/a", true}, {"/a/..b", "/a", true}, {"/ab", "/a", false}, {"/", "/a", false},
	} {
		if got, err := within(tc.child, tc.parent); err != nil || got != tc.want {
			t.Errorf("within(%s, %s) = %v, %v", tc.child, tc.parent, got, err)
		}
	}
}

// TestVirtualConstrains (S6): a constraint on a virtual package the host
// has must hold; one on a virtual package the host lacks is satisfied.
func TestVirtualConstrains(t *testing.T) {
	c := newChannel(t)
	cv := buildPkg(t, tspec{name: "cuda-version", version: "12.4", build: "h3060b56_3", subdir: "noarch", noarch: "generic",
		constrains: []string{"cudatoolkit 12.4|12.4.*", "__cuda >=12"}, files: []tfile{{path: "share/cv", data: "x"}}})
	lf := c.lockfile(t, nil, cv)
	for _, tc := range []struct {
		cuda string
		ok   bool
	}{{"11.8", false}, {"12.4", true}, {"", true}} {
		opt := c.options(t)
		virt, err := virtual.ApplyOverrides(hostPlatform(t), testVirtual(t), map[string]string{"__cuda": tc.cuda})
		must(t, err)
		opt.Virtual = virt
		err = InstallLock(context.Background(), filepath.Join(t.TempDir(), "env"), lf, opt)
		var ve *VirtualError
		switch {
		case tc.ok && err != nil:
			t.Errorf("__cuda %q: %v", tc.cuda, err)
		case !tc.ok && (!errors.As(err, &ve) || !ve.Constrains || ve.Spec != "__cuda >=12" || ve.Have != "__cuda 11.8 0"):
			t.Errorf("__cuda %q: err = %v (%+v), want a constrains VirtualError", tc.cuda, err, ve)
		}
	}
}

// symlinkDigest returns the sha256_in_prefix recorded for path in dist's
// conda-meta record.
func symlinkDigest(t *testing.T, prefix, dist, path string) string {
	t.Helper()
	meta := readCondaMetaJSON(t, prefix, dist)
	raw, _ := json.Marshal(meta["paths_data"])
	var pd struct {
		Paths []struct {
			Path   string `json:"_path"`
			Digest string `json:"sha256_in_prefix"`
		} `json:"paths"`
	}
	must(t, json.Unmarshal(raw, &pd))
	for _, e := range pd.Paths {
		if e.Path == path {
			return e.Digest
		}
	}
	t.Fatalf("%s has no %s", dist, path)
	return ""
}

// TestSymlinkDigestDeterministic (N6): a symlink into a package of the same
// layer, linked concurrently, and one into a package linked later (a
// broken dependency cycle) record the target's digest every time.
func TestSymlinkDigestDeterministic(t *testing.T) {
	c := newChannel(t)
	var files []tfile
	for i := 0; i < 300; i++ {
		files = append(files, tfile{path: "share/bulk/f" + strconv.Itoa(i), data: "bulk"})
	}
	target := "real library"
	sum := sha256.Sum256([]byte(target))
	want := hex.EncodeToString(sum[:])
	lf := c.lockfile(t, nil,
		buildPkg(t, tspec{name: "aaa-links", version: "1", build: "0", files: append(files[:150:150], tfile{path: "lib/libx.so", link: "libx.so.1"})}),
		buildPkg(t, tspec{name: "zzz-target", version: "1", build: "0", files: append(files[150:], tfile{path: "lib/libx.so.1", data: target})}),
	)
	for i := 0; i < 20; i++ {
		opt := c.options(t)
		opt.Concurrency = 2
		prefix := filepath.Join(t.TempDir(), "env")
		must(t, InstallLock(context.Background(), prefix, lf, opt))
		if got := symlinkDigest(t, prefix, "aaa-links-1-0", "lib/libx.so"); got != want {
			t.Fatalf("run %d: digest %s, want %s", i, got, want)
		}
	}

	// rattler's python test data cycle: _openmp_mutex's lib/libgomp.so.1
	// points to llvm-openmp's libomp.so, linked after it.
	libomp := "\x7fELF fake libomp\n"
	sum = sha256.Sum256([]byte(libomp))
	c2 := newChannel(t)
	lf2 := c2.lockfile(t, nil,
		buildPkg(t, tspec{name: "_openmp_mutex", version: "4.5", build: "1_llvm", depends: []string{"_libgcc_mutex 0.1 conda_forge", "llvm-openmp >=9.0.1"},
			files: []tfile{{path: "lib/libgomp.so.1", link: "libomp.so"}}}),
		buildPkg(t, tspec{name: "_libgcc_mutex", version: "0.1", build: "conda_forge", files: []tfile{{path: "share/mutex", data: "m"}}}),
		buildPkg(t, tspec{name: "llvm-openmp", version: "15.0.7", build: "h0cdce71_0", depends: []string{"libzlib >=1.2.13,<1.3.0a0"},
			files: []tfile{{path: "lib/libomp.so", data: libomp}}}),
		buildPkg(t, tspec{name: "libzlib", version: "1.2.13", build: "h166bdaf_4", depends: []string{"libgcc-ng >=12"},
			files: []tfile{{path: "lib/libz.so.1", data: "z"}}}),
		buildPkg(t, tspec{name: "libgcc-ng", version: "12.2.0", build: "h65d4601_19", depends: []string{"_libgcc_mutex 0.1 conda_forge", "_openmp_mutex >=4.5"},
			files: []tfile{{path: "lib/libgcc_s.so.1", data: "g"}}}),
	)
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, InstallLock(context.Background(), prefix, lf2, c2.options(t)))
	if got := symlinkDigest(t, prefix, "_openmp_mutex-4.5-1_llvm", "lib/libgomp.so.1"); got != hex.EncodeToString(sum[:]) {
		t.Errorf("symlink into a later package: digest %s", got)
	}
}
