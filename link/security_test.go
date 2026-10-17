package link

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/richinsley/goomba/record"
)

// secEnv is a prefix with an "outside" sibling directory that nothing may
// touch.
type secEnv struct {
	t                     *testing.T
	tmp, prefix, outside  string
	pkgs                  string
	py                    *PythonInfo
	allow                 []string
	beforeOutside, before []string
}

func newSecEnv(t *testing.T) *secEnv {
	tmp := t.TempDir()
	e := &secEnv{t: t, tmp: tmp, prefix: filepath.Join(tmp, "env"), outside: filepath.Join(tmp, "outside"), pkgs: filepath.Join(tmp, "pkgs"), py: pythonInfo()}
	for _, d := range []string{e.prefix, e.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.outside, "keep"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *secEnv) symlink(target, rel string) {
	e.t.Helper()
	p := filepath.Join(e.prefix, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		e.t.Fatal(err)
	}
}

// link links spec and records it; it returns Link's error.
func (e *secEnv) link(spec pkgSpec) error {
	e.t.Helper()
	dir, rec := buildPkg(e.t, e.pkgs, spec)
	opt := Options{Prefix: e.prefix, Platform: "linux-64", Python: e.py, AllowScripts: e.allow}
	inst, err := Link(context.Background(), dir, rec, opt)
	if err == nil {
		if err := WriteCondaMeta(e.prefix, rec, inst, opt); err != nil {
			e.t.Fatal(err)
		}
	}
	return err
}

// mustFail links spec, expects want, and checks that neither the prefix nor
// the outside directory changed.
func (e *secEnv) mustFail(spec pkgSpec, want error) {
	e.t.Helper()
	before, outside := tree(e.t, e.prefix), tree(e.t, e.outside)
	err := e.link(spec)
	if !errors.Is(err, want) {
		e.t.Fatalf("err = %v, want %v", err, want)
	}
	e.t.Log(err)
	if got := tree(e.t, e.prefix); !reflect.DeepEqual(got, before) {
		e.t.Errorf("prefix changed:\n got %q\nwant %q", got, before)
	}
	if got := tree(e.t, e.outside); !reflect.DeepEqual(got, outside) {
		e.t.Errorf("outside changed: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(e.tmp, "x")); err == nil {
		e.t.Error("wrote next to the prefix")
	}
}

func (e *secEnv) mustLink(spec pkgSpec) {
	e.t.Helper()
	if err := e.link(spec); err != nil {
		e.t.Fatal(err)
	}
}

// B1: a link script cannot be planted through a symlinked directory.
func TestLinkScriptThroughSymlink(t *testing.T) {
	t.Run("same package", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "bin/.evil-pre-unlink.sh", data: "rm -rf ~", unlisted: true},
			{path: "b", symlink: "bin"},
			{path: "b/.evil-pre-unlink.sh", data: "rm -rf ~"},
		}}, ErrLinkScript)
	})
	t.Run("same package, existing bin", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "base", files: []pkgFile{{path: "bin/tool", data: "x"}}})
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "bin/.evil-post-link.sh", data: "x", unlisted: true},
			{path: "b", symlink: "bin"},
			{path: "b/.evil-post-link.sh", data: "x"},
		}}, ErrLinkScript)
	})
	t.Run("across packages", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "a", files: []pkgFile{{path: "bin/tool", data: "x"}, {path: "b", symlink: "bin"}}})
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "b/.evil-post-link.sh", data: "x"}}}, ErrLinkScript)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "b/.a-pre-unlink.sh", data: "x"}}}, ErrLinkScript)
	})
	t.Run("planted Scripts alias", func(t *testing.T) {
		e := newSecEnv(t)
		if err := os.MkdirAll(filepath.Join(e.prefix, "Scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.symlink("Scripts", "tools")
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "tools/.evil-pre-link.bat", data: "x"}}}, ErrLinkScript)
	})
	t.Run("noarch python-scripts through symlink", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", noarch: "python", files: []pkgFile{
			{path: "bin/.evil-post-link.sh", data: "x", unlisted: true},
			{path: "share/b", symlink: "../bin"},
			{path: "share/b/.evil-post-link.sh", data: "x"},
		}}, ErrLinkScript)
	})
	t.Run("ignorable characters", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "bin/.evil‌-post-link.sh", data: "x"}}}, ErrLinkScript)
	})
	t.Run("allowed own script only at bin", func(t *testing.T) {
		e := newSecEnv(t)
		e.allow = []string{"good"}
		e.mustFail(pkgSpec{name: "good", files: []pkgFile{{path: "bin/.good-post-link.sh", data: "x", unlisted: true}, {path: "b", symlink: "bin"}, {path: "b/.good-post-link.sh", data: "x"}}}, ErrLinkScript)
		e.mustFail(pkgSpec{name: "good", version: "2", files: []pkgFile{{path: "bin/.good-post-link.sh", symlink: "x"}}}, ErrLinkScript)
		e.mustLink(pkgSpec{name: "good", version: "3", files: []pkgFile{{path: "bin/.good-post-link.sh", data: "x"}}})
	})
}

// B2: nothing reaches conda-meta through an alias, and Unlink does not
// trust records that point into it.
func TestCondaMetaThroughSymlink(t *testing.T) {
	t.Run("package symlink to conda-meta", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "good", files: []pkgFile{{path: "bin/important", data: "x"}}})
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "conda-meta/victim-1.0-0.json", data: `{"name": "victim", "version": "1.0", "build": "0", "files": ["bin/important"]}`, unlisted: true},
			{path: "m", symlink: "conda-meta"},
			{path: "m/victim-1.0-0.json", data: `{"name": "victim", "version": "1.0", "build": "0", "files": ["bin/important"]}`},
		}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil2", files: []pkgFile{{path: "lib/m", symlink: "../conda-meta"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil3", files: []pkgFile{{path: "lib/m", symlink: "../Conda-Meta/history"}}}, ErrUnsafePath)
	})
	t.Run("planted symlink to conda-meta", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "good", files: []pkgFile{{path: "bin/important", data: "x"}}})
		e.symlink("conda-meta", "m")
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "m/victim-1.0-0.json", data: `{"name": "victim", "version": "1.0", "build": "0", "files": ["bin/important"]}`},
		}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil2", noarch: "python", files: []pkgFile{{path: "m/x", data: "x"}}}, ErrUnsafePath)
		recs, err := ReadCondaMeta(e.prefix)
		if err != nil || len(recs) != 1 || recs[0].Record.Name != "good" {
			t.Fatalf("records %v %v", recs, err)
		}

		// A tampered record that reaches conda-meta through m is refused
		// and changes nothing.
		history := filepath.Join(e.prefix, "conda-meta", "history")
		if err := os.WriteFile(history, []byte("h"), 0o644); err != nil {
			t.Fatal(err)
		}
		doc := `{"name": "evil", "version": "1", "build": "0", "files": ["m/history", "m/good-1.0-h0_0.json", "M/history"]}`
		if err := os.WriteFile(filepath.Join(e.prefix, "conda-meta", "evil-1-0.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Unlink(context.Background(), e.prefix, &record.PackageRecord{Name: "evil", Version: "1", Build: "0"}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Unlink: %v, want ErrUnsafePath", err)
		}
		for _, f := range []string{"history", "good-1.0-h0_0.json", "evil-1-0.json"} {
			if _, err := os.Lstat(filepath.Join(e.prefix, "conda-meta", f)); err != nil {
				t.Errorf("conda-meta/%s: %v", f, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(e.prefix, "bin/important")); err != nil {
			t.Error(err)
		}
	})
	t.Run("conda-meta alias by identity", func(t *testing.T) {
		// conda-meta is a symlink to a directory inside the prefix: writes
		// to that directory under its other name are refused.
		e := newSecEnv(t)
		if err := os.MkdirAll(filepath.Join(e.prefix, "var/meta"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.symlink("var/meta", "conda-meta")
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "var/meta/x-1-0.json", data: "{}"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil2", files: []pkgFile{{path: "var/meta", dir: true}}}, ErrUnsafePath)
		doc := `{"name": "evil", "version": "1", "build": "0", "files": ["var/meta/history"]}`
		if err := os.WriteFile(filepath.Join(e.prefix, "var/meta/history"), []byte("h"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.prefix, "var/meta/evil-1-0.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Unlink(context.Background(), e.prefix, &record.PackageRecord{Name: "evil", Version: "1", Build: "0"}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Unlink: %v, want ErrUnsafePath", err)
		}
		if _, err := os.Lstat(filepath.Join(e.prefix, "var/meta/history")); err != nil {
			t.Error(err)
		}
	})
	t.Run("entry point through planted bin", func(t *testing.T) {
		e := newSecEnv(t)
		e.symlink("conda-meta", "bin")
		e.mustFail(pkgSpec{name: "evil", noarch: "python", entryPoints: []string{"victim-1.0-0.json = a:b"}}, ErrUnsafePath)
	})
}

// S1: no symlink that leaves the prefix is ever created, whatever the
// order of packages. Cases from the review of WP4.
func TestSymlinkEscapes(t *testing.T) {
	t.Run("A lib/x -> .. then B lib/x/y -> ../..", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "a", files: []pkgFile{{path: "lib/x", symlink: ".."}}})
		e.mustFail(pkgSpec{name: "b", files: []pkgFile{{path: "lib/x/y", symlink: "../.."}}}, ErrUnsafePath)
	})
	t.Run("A then B lib/z -> x/../..", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "a", files: []pkgFile{{path: "lib/x", symlink: ".."}}})
		e.mustFail(pkgSpec{name: "b", files: []pkgFile{{path: "lib/z", symlink: "x/../.."}}}, ErrUnsafePath)
	})
	t.Run("B first then A", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "b", files: []pkgFile{{path: "lib/x/y", symlink: "../.."}}})
		e.mustFail(pkgSpec{name: "a", files: []pkgFile{{path: "lib/x", symlink: ".."}}}, ErrExists)
		// y resolves to the prefix itself.
		if p, err := filepath.EvalSymlinks(filepath.Join(e.prefix, "lib/x/y")); err != nil || p != mustEval(t, e.prefix) {
			t.Errorf("lib/x/y -> %s, %v", p, err)
		}
	})
	t.Run("missing intermediate then filled in", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "b", files: []pkgFile{{path: "lib/x", symlink: "../d/e/../.."}}}, ErrUnsafePath)
		e.mustLink(pkgSpec{name: "b2", files: []pkgFile{{path: "lib/x", symlink: "../d/e"}}})
		e.mustFail(pkgSpec{name: "c", files: []pkgFile{{path: "d/e", symlink: "../.."}}}, ErrUnsafePath)
		e.mustLink(pkgSpec{name: "c2", files: []pkgFile{{path: "d/e", symlink: ".."}}})
	})
	t.Run("A a/l -> n/../../x then B a/n -> ..", func(t *testing.T) {
		// From the fetch/archive review: a missing component followed by
		// "..". Lexically a/l stays at the prefix root, but once B makes
		// a/n a symlink to the prefix, a/l would point above it. The
		// normal-form rule refuses A's link when it is created.
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "a", files: []pkgFile{{path: "a/l", symlink: "n/../../x"}}}, ErrUnsafePath)
		e.mustLink(pkgSpec{name: "b", files: []pkgFile{{path: "a/n", symlink: ".."}}})
		if p, err := filepath.EvalSymlinks(filepath.Join(e.prefix, "a/n")); err != nil || p != mustEval(t, e.prefix) {
			t.Errorf("a/n -> %s, %v", p, err)
		}
		// With B in place first, A is refused the same way.
		e.mustFail(pkgSpec{name: "a2", files: []pkgFile{{path: "a/l", symlink: "n/../../x"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "a3", files: []pkgFile{{path: "a/l", symlink: "n/../x"}}}, ErrUnsafePath)
	})
	t.Run("same package q/r -> .. and q/r/x -> ../outside", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "q", files: []pkgFile{
			{path: "q/r", symlink: ".."},
			{path: "q/r/x", symlink: "../outside"},
		}}, ErrUnsafePath)
	})
	t.Run("same package chain", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "q", files: []pkgFile{
			{path: "lib/sub/deep/c", symlink: "../../.."},
			{path: "lib/s1", symlink: "sub/deep/c/.."},
		}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "q2", files: []pkgFile{
			{path: "lib/sub/deep/c", symlink: "../../.."},
			{path: "lib/s2", symlink: "sub/deep/c"},
			{path: "lib/s2/up", symlink: ".."},
		}}, ErrUnsafePath)
	})
	t.Run("noarch site-packages", func(t *testing.T) {
		e := newSecEnv(t)
		// lib/python3.11/site-packages/a: three levels deep.
		e.mustLink(pkgSpec{name: "ok", noarch: "python", files: []pkgFile{
			{path: "site-packages/a", symlink: "../../.."},
			{path: "site-packages/b", symlink: "../../../bin"},
		}})
		e.mustFail(pkgSpec{name: "up", noarch: "python", files: []pkgFile{{path: "site-packages/c", symlink: "../../../.."}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "meta", noarch: "python", files: []pkgFile{{path: "site-packages/d", symlink: "../../../conda-meta"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "via", noarch: "python", files: []pkgFile{{path: "site-packages/a/e", symlink: "../x"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "script", noarch: "python", files: []pkgFile{{path: "site-packages/b/.script-post-link.sh", data: "x"}}}, ErrLinkScript)
	})
	t.Run("entry point through planted bin outside", func(t *testing.T) {
		e := newSecEnv(t)
		e.symlink("../outside", "bin")
		e.mustFail(pkgSpec{name: "ep", noarch: "python", entryPoints: []string{"tool = a:b"}}, ErrUnsafePath)
		e2 := newSecEnv(t)
		e2.symlink("lib/python3.11/site-packages/pkg", "bin")
		e2.mustLink(pkgSpec{name: "ep", noarch: "python", entryPoints: []string{"tool = a:b"}})
		if _, err := os.Lstat(filepath.Join(e2.prefix, "lib/python3.11/site-packages/pkg/tool")); err != nil {
			t.Error(err)
		}
	})
	t.Run("normal form", func(t *testing.T) {
		for _, target := range []string{"./x", "x/", "x//y", "x/.", "..//x", "/abs", `x\y`, "x/../y"} {
			e := newSecEnv(t)
			e.mustFail(pkgSpec{name: "n", files: []pkgFile{{path: "lib/deep/l", symlink: target}}}, ErrUnsafePath)
		}
	})
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Every symlink of the reference package cache passes the target rules (the
// review counted 1,186).
func TestOracleSymlinkTargets(t *testing.T) {
	_, pkgs := oracleDirs(t)
	n := 0
	err := filepath.WalkDir(pkgs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		rel, _ := filepath.Rel(pkgs, p)
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if len(parts) < 2 || strings.HasPrefix(parts[1], "info/") {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		n++
		if err := checkSymlinkTarget(parts[1], target); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 1000 {
		t.Errorf("only %d symlinks checked", n)
	}
}

// S2: files with setuid, setgid or sticky bits are copied without them,
// never hard linked.
func TestLinkSpecialModes(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "suid", files: []pkgFile{
		{path: "bin/suid", data: "x", mode: 0o755},
		{path: "bin/sgid", data: "y", mode: 0o755},
		{path: "share/sticky", data: "z", mode: 0o644},
		{path: "bin/plain", data: "p", mode: 0o755},
	}})
	for f, m := range map[string]fs.FileMode{"bin/suid": 0o4755, "bin/sgid": 0o2755, "share/sticky": 0o1644} {
		// os.Chmod takes the Go mode bits.
		gm := m.Perm()
		if m&0o4000 != 0 {
			gm |= fs.ModeSetuid
		}
		if m&0o2000 != 0 {
			gm |= fs.ModeSetgid
		}
		if m&0o1000 != 0 {
			gm |= fs.ModeSticky
		}
		if err := os.Chmod(filepath.Join(pkgDir, f), gm); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(filepath.Join(pkgDir, f)); err != nil || fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) == 0 {
			t.Skipf("cannot set special mode bits here: %v %v", fi.Mode(), err)
		}
	}
	prefix := filepath.Join(tmp, "env")
	if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"bin/suid", "bin/sgid", "share/sticky", "bin/plain"} {
		src, _ := os.Stat(filepath.Join(pkgDir, f))
		dst, err := os.Stat(filepath.Join(prefix, f))
		if err != nil {
			t.Fatal(err)
		}
		special := dst.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0
		if f == "bin/plain" {
			if !os.SameFile(src, dst) {
				t.Errorf("%s was not hard linked", f)
			}
			continue
		}
		if os.SameFile(src, dst) || special || dst.Mode().Perm() != src.Mode().Perm() {
			t.Errorf("%s: mode %v, hard linked %v", f, dst.Mode(), os.SameFile(src, dst))
		}
	}
}

// N1: a failed Link removes everything it created except the prefix and
// its conda-meta directory, which stay (empty) so that a concurrent Link or
// WriteCondaMeta never loses conda-meta from under it.
func TestLinkRollbackPrefixParents(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), basicSpec(testPlaceholder()))
	prefix := filepath.Join(tmp, "a", "b", "c")
	boom := errors.New("boom")
	_, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "osx-arm64", Signer: func(string) error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// Only the prefix and its (kept) empty conda-meta remain.
	if got := tree(t, filepath.Join(tmp, "a")); !reflect.DeepEqual(got, []string{"b/", "b/c/", "b/c/conda-meta/"}) {
		t.Errorf("left behind: %q", got)
	}
}

// ReadCondaMeta fails closed on records it cannot trust: unparsable files
// and files whose name does not match their content.
func TestReadCondaMetaFailsClosed(t *testing.T) {
	for name, doc := range map[string]string{
		"junk.json":      "not json",
		"other-1-0.json": `{"name": "victim", "version": "1", "build": "0"}`,
	} {
		prefix := t.TempDir()
		if err := os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "conda-meta", name), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadCondaMeta(prefix); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A directory swapped for a symlink between the checks and the write is
// detected: the destination is resolved and checked again, never reached
// through the swapped path.
func TestLinkRaceSwappedDirectory(t *testing.T) {
	for _, c := range []struct {
		name   string
		target string // symlink replacing x; "" removes x
		want   error
	}{
		{"symlink to conda-meta", "conda-meta", ErrUnsafePath},
		{"symlink outside", "../outside", ErrUnsafePath},
		{"directory removed (concurrent rollback)", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newSecEnv(t)
			dir, rec := buildPkg(t, e.pkgs, pkgSpec{name: "evil", files: []pkgFile{{path: "x/victim-1.0-0.json", data: "{}"}}})
			swapped := false
			opt := Options{Prefix: e.prefix, Platform: "linux-64", beforeOpen: func(dst string) {
				if swapped || dst != "x/victim-1.0-0.json" {
					return
				}
				swapped = true
				if err := os.Remove(filepath.Join(e.prefix, "x")); err != nil {
					t.Fatal(err)
				}
				if c.target != "" {
					if err := os.Symlink(c.target, filepath.Join(e.prefix, "x")); err != nil {
						t.Fatal(err)
					}
				}
			}}
			_, err := Link(context.Background(), dir, rec, opt)
			if !swapped {
				t.Fatal("hook did not run")
			}
			if !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.want == nil {
				if _, err := os.Lstat(filepath.Join(e.prefix, "x/victim-1.0-0.json")); err != nil {
					t.Error(err)
				}
				return
			}
			t.Log(err)
			for _, p := range []string{filepath.Join(e.prefix, "conda-meta/victim-1.0-0.json"), filepath.Join(e.outside, "victim-1.0-0.json")} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s was written", p)
				}
			}
		})
	}
}

// A signer that replaces the file (as codesign may) does not defeat the
// rollback.
func TestLinkRollbackAfterReplacingSigner(t *testing.T) {
	tmp := t.TempDir()
	spec := basicSpec(testPlaceholder())
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), spec)
	prefix := filepath.Join(tmp, "env")
	calls := 0
	_, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "osx-arm64", Signer: func(p string) error {
		calls++
		// Replace the file with a new inode, then fail.
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o755); err != nil {
			return err
		}
		return errors.New("signing failed")
	}})
	if err == nil || calls != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("left behind: %q", got)
	}
}

// N-B1 (second review): a directory cannot become bin or Scripts after a
// script is inside it. Link-script names are refused anywhere, and bin and
// Scripts may never be symlinks.
func TestScriptDirectoryCannotBecomeBin(t *testing.T) {
	t.Run("same package", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "share/evil/.evil-pre-unlink.sh", data: "rm -rf ~"},
			{path: "bin", symlink: "share/evil"},
		}}, ErrLinkScript)
	})
	t.Run("bin symlink alone", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "bin", symlink: "share/evil"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil2", files: []pkgFile{{path: "Scripts", symlink: "lib"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil3", files: []pkgFile{{path: "SCRIPTS", symlink: "lib"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil4", files: []pkgFile{{path: "bın", symlink: "lib"}}}, ErrUnsafePath)
	})
	t.Run("across packages", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "apkg", files: []pkgFile{{path: "share/evil/.apkg-pre-unlink.sh", data: "x"}}}, ErrLinkScript)
		e.mustLink(pkgSpec{name: "apkg2", files: []pkgFile{{path: "share/evil/README", data: "x"}}})
		e.mustFail(pkgSpec{name: "bpkg", files: []pkgFile{{path: "bin", symlink: "share/evil"}}}, ErrUnsafePath)
	})
	t.Run("x -> . plus x/bin", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "x", symlink: "."},
			{path: "x/bin", symlink: "share/evil"},
		}}, ErrUnsafePath)
		e.mustLink(pkgSpec{name: "dot", files: []pkgFile{{path: "x", symlink: "."}}})
		e.mustFail(pkgSpec{name: "evil2", files: []pkgFile{{path: "x/bin", symlink: "share/evil"}}}, ErrUnsafePath)
	})
	t.Run("script-named symlink", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "share/evil/x", data: "x"},
			{path: "share/evil/.evil-pre-unlink.sh", symlink: "x"},
		}}, ErrLinkScript)
	})
	t.Run("script-named directory", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "share/.evil-post-link.sh", dir: true}}}, ErrLinkScript)
	})
}

// N-S1 (second review): a conda-meta alias built from the package's own
// earlier entries is caught, in a fresh environment too.
func TestCondaMetaAliasFromOwnEntries(t *testing.T) {
	t.Run("same package", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "r", symlink: "."},
			{path: "cm", symlink: "r/conda-meta"},
		}}, ErrUnsafePath)
	})
	t.Run("across packages", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustLink(pkgSpec{name: "dot", files: []pkgFile{{path: "r", symlink: "."}}})
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: "cm", symlink: "r/conda-meta"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "evil2", files: []pkgFile{{path: "lib/cm", symlink: "../r/r/conda-meta/history"}}}, ErrUnsafePath)
	})
	t.Run("through a directory symlink", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "a/b", dir: true},
			{path: "a/up", symlink: ".."},
			{path: "a/b/cm", symlink: "../up/conda-meta"},
		}}, ErrUnsafePath)
	})
	t.Run("conda-meta exists before the dry run", func(t *testing.T) {
		e := newSecEnv(t)
		dir, rec := buildPkg(t, e.pkgs, pkgSpec{name: "ok", files: []pkgFile{{path: "x", data: "x"}}})
		if _, err := Link(context.Background(), dir, rec, Options{Prefix: e.prefix, Platform: "linux-64"}); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(filepath.Join(e.prefix, "conda-meta")); err != nil || !fi.IsDir() {
			t.Errorf("conda-meta not created: %v", err)
		}
	})
}

// Folding: case-insensitive file systems fold per Unicode, and HFS+
// ignores invisible code points.
func TestUnicodeFolding(t *testing.T) {
	cases := []struct {
		name string
		path string
		want error
	}{
		{"long s and Kelvin sign", "bin/.x-poſt-linK.sh", ErrLinkScript},
		{"folded script name elsewhere", "share/.x-POſT-link.SH", ErrLinkScript},
		{"zero-width joiner", "share/.x-post‍-link.sh", ErrLinkScript},
		{"non-ASCII dot name in bin", "bin/.été", ErrLinkScript},
		{"non-ASCII dot name in Scripts", "Scripts/.сfg", ErrLinkScript},
		{"conda-meta with zero-width space", "conda​-meta/x.json", ErrUnsafePath},
		{"conda-meta in capitals", "CONDA-META/x.json", ErrUnsafePath},
		{"non-ASCII dot name elsewhere", "share/.été", nil},
		{"non-ASCII name in bin", "bin/café", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSecEnv(t)
			spec := pkgSpec{name: "x", files: []pkgFile{{path: c.path, data: "x"}}}
			if c.want == nil {
				e.mustLink(spec)
				return
			}
			e.mustFail(spec, c.want)
		})
	}
	if got := foldName("Conda‌-Meta️"); got != "conda-meta" {
		t.Errorf("foldName = %q", got)
	}
	if got := foldName(".x-poſt-linK.sh"); got != ".x-post-link.sh" {
		t.Errorf("foldName = %q", got)
	}
}

// Windows (target win-* or a Windows host): names Win32 would read as
// another name are refused.
func TestWindowsAliases(t *testing.T) {
	cases := []struct {
		path string
		want error
	}{
		{"Scripts./x.py", ErrUnsafePath},
		{"Lib/file.txt ", ErrUnsafePath},
		{"Lib/file.txt:stream", ErrUnsafePath},
		{"CONDA-~1/x.json", ErrUnsafePath},
		{"SCRIPT~1/tool.exe", ErrUnsafePath},
		{"Lib/PROGRA~1/x", nil},
		{"Lib/site-packages/a.py", nil},
	}
	for _, c := range cases {
		e := newSecEnv(t)
		dir, rec := buildPkg(t, e.pkgs, pkgSpec{name: "w", files: []pkgFile{{path: c.path, data: "x"}}})
		_, err := Link(context.Background(), dir, rec, Options{Prefix: e.prefix, Platform: "win-64"})
		if !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("%q: err = %v, want %v", c.path, err, c.want)
		}
	}
	// The same names are ordinary on Unix targets (perl man pages use "::").
	e := newSecEnv(t)
	e.mustLink(pkgSpec{name: "perl", files: []pkgFile{{path: "share/man/man3/Foo::Bar.3pm", data: "x"}, {path: "share/x.", data: "y"}}})
}

// R3-1 (third review): concurrent Links into a fresh prefix, half of them
// failing after writing, never disturb each other: conda-meta is never
// rolled back and never identified from a stale FileInfo.
func TestLinkConcurrentFreshPrefix(t *testing.T) {
	ph := testPlaceholder()
	tmp := t.TempDir()
	pkgs := filepath.Join(tmp, "pkgs")
	type pkg struct {
		dir  string
		rec  *record.PackageRecord
		fail bool
	}
	var all []pkg
	for i := 0; i < 12; i++ {
		fail := i%2 == 1
		name := fmt.Sprintf("ms%d", i)
		files := []pkgFile{{path: "lib/" + name + ".txt", data: name}, {path: "share/" + name + "/data", data: name}}
		if fail {
			name = fmt.Sprintf("mf%d", i)
			files = []pkgFile{
				{path: "lib/" + name + ".txt", data: name},
				{path: "lib/" + name + "/deep/x", data: name},
				{path: "lib/" + name + ".bin", data: "\x7fELF" + ph + "/lib\x00", placeholder: ph, fileMode: FileModeBinary},
			}
		}
		dir, rec := buildPkg(t, pkgs, pkgSpec{name: name, files: files})
		all = append(all, pkg{dir, rec, fail})
	}
	boom := errors.New("signing failed")
	for round := 0; round < 25; round++ {
		prefix := filepath.Join(tmp, fmt.Sprintf("env%d", round))
		errs := make([]error, len(all))
		var wg sync.WaitGroup
		// Start in a different order each round, so that a failing Link is
		// often the one that creates conda-meta.
		for k := range all {
			i := (k + round) % len(all)
			p := all[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				opt := Options{Prefix: prefix, Platform: "osx-arm64", Signer: func(string) error { return boom }}
				inst, err := Link(context.Background(), p.dir, p.rec, opt)
				if err == nil {
					err = WriteCondaMeta(prefix, p.rec, inst, opt)
				}
				errs[i] = err
			}()
		}
		wg.Wait()
		var want []string
		for i, p := range all {
			switch {
			case p.fail && !errors.Is(errs[i], boom):
				t.Fatalf("round %d: %s: err = %v, want the signer's error", round, p.rec.Name, errs[i])
			case !p.fail && errs[i] != nil:
				t.Fatalf("round %d: %s: %v", round, p.rec.Name, errs[i])
			case !p.fail:
				want = append(want, p.rec.Name)
			}
		}
		recs, err := ReadCondaMeta(prefix)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range recs {
			got = append(got, r.Record.Name)
		}
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: records %q, want %q", round, got, want)
		}
		for _, f := range tree(t, prefix) {
			if strings.Contains(f, "mf") {
				t.Fatalf("round %d: failed package left %s", round, f)
			}
		}
	}
}

// R3-2 (third review): symlink targets naming conda-meta are refused
// whatever the order in which the symlinks they go through appear.
func TestSymlinkTargetNamingCondaMeta(t *testing.T) {
	t.Run("same package cm -> d/conda-meta, d -> .", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "evil", files: []pkgFile{
			{path: "cm", symlink: "d/conda-meta"},
			{path: "d", symlink: "."},
		}}, ErrUnsafePath)
	})
	t.Run("across packages cm -> d/conda-meta, then d -> .", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "a", files: []pkgFile{{path: "cm", symlink: "d/conda-meta"}}}, ErrUnsafePath)
		e.mustLink(pkgSpec{name: "b", files: []pkgFile{{path: "d", symlink: "."}}})
		e.mustFail(pkgSpec{name: "a2", files: []pkgFile{{path: "cm", symlink: "d/conda-meta"}}}, ErrUnsafePath)
	})
	t.Run("cm -> d/e/conda-meta, then d/e -> ..", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "a", files: []pkgFile{{path: "cm", symlink: "d/e/conda-meta"}}}, ErrUnsafePath)
		e.mustLink(pkgSpec{name: "b", files: []pkgFile{{path: "d/e", symlink: ".."}}})
	})
	t.Run("lib/h -> ../d/conda-meta/history, then d -> .", func(t *testing.T) {
		e := newSecEnv(t)
		e.mustFail(pkgSpec{name: "a", files: []pkgFile{{path: "lib/h", symlink: "../d/conda-meta/history"}}}, ErrUnsafePath)
		e.mustFail(pkgSpec{name: "a2", files: []pkgFile{{path: "lib/h", symlink: "../d/CONDA-META"}}}, ErrUnsafePath)
		e.mustLink(pkgSpec{name: "b", files: []pkgFile{{path: "d", symlink: "."}}})
	})
}

// R3-3 (third review): directories created on the way to an entry are
// checked like entries, so a script-named or suspicious directory cannot
// appear in bin as a side effect.
func TestImplicitDirectoriesChecked(t *testing.T) {
	cases := []struct {
		path string
		want error
	}{
		{"bin/.evil-pre-unlink.sh/x", ErrLinkScript},
		{"bin/.e\u00e9/y", ErrLinkScript},
		{"share/.evil-post-link.sh/x", ErrLinkScript},
		{"Scripts/.evil-pre-link.bat/deep/z", ErrLinkScript},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			e := newSecEnv(t)
			e.mustFail(pkgSpec{name: "evil", files: []pkgFile{{path: c.path, data: "x"}}}, c.want)
		})
	}
	// Ordinary nested directories are fine.
	e := newSecEnv(t)
	e.mustLink(pkgSpec{name: "ok", files: []pkgFile{{path: "bin/.config/tool/x", data: "x"}, {path: "share/a/b/c", data: "y"}}})
}
