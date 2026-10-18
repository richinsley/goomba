//go:build unix

package safedir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The cases follow fetch's cache tests (fetch/safety_unix_test.go), which
// exercised this check before it moved here.

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// skipIfRoot skips ownership tests when running as root, who owns
// everything it creates and passes every check by design.
func skipIfRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
}

// chgrpOther gives p a group other than the user's private group, or skips
// the test if the user belongs to no other group.
func chgrpOther(t *testing.T, p string) {
	t.Helper()
	groups, err := os.Getgroups()
	must(t, err)
	for _, g := range groups {
		if g != os.Getegid() && g != os.Geteuid() {
			must(t, os.Chown(p, -1, g))
			return
		}
	}
	t.Skip("the user belongs to no group but its own")
}

// TestCheckDirRefusesShared: a directory that others could change, or
// under a directory others could change, is refused with ErrUnsafe.
func TestCheckDirRefusesShared(t *testing.T) {
	skipIfRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"directory 0777", func(t *testing.T, dir string) {
			must(t, os.Chmod(dir, 0o777))
		}},
		{"directory 0757", func(t *testing.T, dir string) {
			must(t, os.Chmod(dir, 0o757))
		}},
		{"directory group-writable, other group", func(t *testing.T, dir string) {
			must(t, os.Chmod(dir, 0o775))
			chgrpOther(t, dir)
		}},
		{"parent writable by others without sticky bit", func(t *testing.T, dir string) {
			must(t, os.Chmod(filepath.Dir(dir), 0o777))
		}},
		{"parent group-writable, other group", func(t *testing.T, dir string) {
			must(t, os.Chmod(filepath.Dir(dir), 0o775))
			chgrpOther(t, filepath.Dir(dir))
		}},
		{"not a directory", func(t *testing.T, dir string) {
			must(t, os.Remove(dir))
			must(t, os.WriteFile(dir, nil, 0o600))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "parent")
			must(t, os.Mkdir(parent, 0o755))
			dir := filepath.Join(parent, "cache")
			must(t, os.Mkdir(dir, 0o755))
			tc.setup(t, dir)
			err := CheckDir(dir)
			if !errors.Is(err, ErrUnsafe) {
				t.Fatalf("CheckDir: %v, want ErrUnsafe", err)
			}
			var e *Error
			if !errors.As(err, &e) || e.Path == "" {
				t.Errorf("not an *Error naming the path: %#v", err)
			}
			if tc.name != "not a directory" {
				if _, err := Resolve(dir); !errors.Is(err, ErrUnsafe) {
					t.Errorf("Resolve: %v, want ErrUnsafe", err)
				}
			}
		})
	}
	t.Run("sticky parent is fine", func(t *testing.T) {
		parent := t.TempDir()
		must(t, os.Chmod(parent, 0o777|fs.ModeSticky))
		if _, err := Resolve(filepath.Join(parent, "cache")); err != nil {
			t.Fatal(err)
		}
	})
}

// TestPrivateGroup: a group-writable directory, or directory above it, is
// accepted when it belongs to the user and to the user's private group
// (gid == uid == egid), and refused with any other group.
func TestPrivateGroup(t *testing.T) {
	skipIfRoot(t)
	if os.Getegid() != os.Geteuid() {
		t.Skip("the user's group id differs from its user id")
	}
	parent := filepath.Join(t.TempDir(), "shared")
	must(t, os.Mkdir(parent, 0o775))
	must(t, os.Chmod(parent, 0o775))
	dir := filepath.Join(parent, "cache")
	must(t, os.Mkdir(dir, 0o775))
	must(t, os.Chmod(dir, 0o775))
	if err := CheckDir(dir); err != nil {
		t.Fatalf("private group: %v", err)
	}
	// Entries get no such exception.
	fi, err := os.Lstat(dir)
	must(t, err)
	if err := CheckEntry(dir, fi); !errors.Is(err, ErrUnsafe) {
		t.Errorf("CheckEntry of a group-writable directory: %v", err)
	}
	// Writable by others is never fine.
	must(t, os.Chmod(dir, 0o777))
	if err := CheckDir(dir); !errors.Is(err, ErrUnsafe) {
		t.Errorf("0777: %v", err)
	}
	must(t, os.Chmod(dir, 0o775))
	chgrpOther(t, parent)
	if err := CheckDir(dir); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("other group above the directory: err = %v, want ErrUnsafe", err)
	}
}

// TestResolveOnce: Resolve returns the directory with its symlinks
// resolved, so the caller can keep using that path after a symlink on the
// way to it is re-pointed.
func TestResolveOnce(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	must(t, os.Mkdir(private, 0o755))
	link := filepath.Join(root, "cache")
	must(t, os.Symlink(private, link))
	got, err := Resolve(link)
	must(t, err)
	if got != private {
		t.Fatalf("Resolve = %s, want %s", got, private)
	}
	other := filepath.Join(root, "other")
	must(t, os.Mkdir(other, 0o755))
	must(t, os.Remove(link))
	must(t, os.Symlink(other, link))
	if err := CheckDir(got); err != nil {
		t.Fatal(err)
	}
	// Relative paths come back absolute.
	t.Chdir(root)
	if got, err := Resolve("private"); err != nil || got != private {
		t.Errorf("Resolve(relative) = %s, %v", got, err)
	}
	if _, err := Resolve(""); err == nil {
		t.Error("Resolve(\"\") succeeded")
	}
}

// TestResolveSymlinkInSharedDir (review round 1, S1): a symlink in a
// directory others can write, without the sticky bit, is never followed:
// another user could re-point it at any time. The real directory under
// the same parent is refused too, as before.
func TestResolveSymlinkInSharedDir(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	must(t, os.Mkdir(target, 0o755))
	shared := filepath.Join(root, "shared")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Chmod(shared, 0o777))
	link := filepath.Join(shared, "cache")
	must(t, os.Symlink(target, link))
	for _, p := range []string{link, filepath.Join(link, "sub"), filepath.Join(shared, "real")} {
		if _, err := Resolve(p); !errors.Is(err, ErrUnsafe) {
			t.Errorf("Resolve(%s) = %v, want ErrUnsafe", p, err)
		}
		if _, err := ResolvePath(p); !errors.Is(err, ErrUnsafe) {
			t.Errorf("ResolvePath(%s) = %v, want ErrUnsafe", p, err)
		}
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("created %v in the symlink's target", ents)
	}
	if _, err := os.Lstat(filepath.Join(shared, "real")); !os.IsNotExist(err) {
		t.Errorf("created a directory under the shared directory: %v", err)
	}
	// A symlink to the shared directory is refused when it is reached.
	link2 := filepath.Join(root, "toshared")
	must(t, os.Symlink(shared, link2))
	if _, err := Resolve(filepath.Join(link2, "x")); !errors.Is(err, ErrUnsafe) {
		t.Errorf("through a symlink into the shared directory: %v", err)
	}
}

// ownedBy returns ops whose lstat reports the entries in uids as owned by
// the given user ids, the real file system otherwise.
func ownedBy(uids map[string]uint32) sysOps {
	ops := osOps()
	real := ops.lstat
	ops.lstat = func(p string) (fs.FileInfo, error) {
		fi, err := real(p)
		if err != nil {
			return fi, err
		}
		if uid, ok := uids[p]; ok {
			st := *fi.Sys().(*syscall.Stat_t)
			st.Uid = uid
			return fakeOwner{fi, &st}, nil
		}
		return fi, nil
	}
	return ops
}

// fakeOwner is a FileInfo with another owner.
type fakeOwner struct {
	fs.FileInfo
	st *syscall.Stat_t
}

func (f fakeOwner) Sys() any { return f.st }

// TestResolveStickySymlink (review round 1, S1): in a sticky directory
// others can write (like /tmp), a symlink is followed only when it belongs
// to the current user or root, as the kernel's protected_symlinks does.
func TestResolveStickySymlink(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	must(t, os.Mkdir(target, 0o755))
	sticky := filepath.Join(root, "tmp")
	must(t, os.Mkdir(sticky, 0o755))
	must(t, os.Chmod(sticky, 0o777|fs.ModeSticky))
	link := filepath.Join(sticky, "cache")
	must(t, os.Symlink(target, link))
	if got, err := Resolve(link); err != nil || got != target {
		t.Errorf("own symlink: %s, %v", got, err)
	}
	other := uint32(os.Geteuid() + 4242)
	if _, err := resolveWith(link, true, ownedBy(map[string]uint32{link: other})); !errors.Is(err, ErrUnsafe) {
		t.Errorf("another user's symlink: %v, want ErrUnsafe", err)
	}
	if got, err := resolveWith(link, true, ownedBy(map[string]uint32{link: 0})); err != nil || got != target {
		t.Errorf("root's symlink: %s, %v", got, err)
	}
	// A directory on the way that belongs to another user is refused too.
	if _, err := resolveWith(filepath.Join(target, "x"), false, ownedBy(map[string]uint32{target: other})); !errors.Is(err, ErrUnsafe) {
		t.Errorf("another user's directory: %v", err)
	}
	// A symlink in a private (not shared) directory may belong to anyone
	// who could put it there: only root or the user.
	link2 := filepath.Join(root, "cache2")
	must(t, os.Symlink(target, link2))
	if got, err := resolveWith(link2, false, ownedBy(map[string]uint32{link2: 0})); err != nil || got != target {
		t.Errorf("root's symlink in a private directory: %s, %v", got, err)
	}
}

// TestResolveChain: chains of relative and absolute symlinks, ".." after a
// symlink (which leaves the symlink's target, as the kernel does), and
// missing components appended by ResolvePath. Review round 2: after a
// missing component, "." and ".." are ENOENT (the kernel cannot look them
// up there), a dangling final symlink is EEXIST (as mkdir(2) says), and a
// missing directory inside a symlink's target is ENOENT; Resolve refuses
// those before creating anything.
func TestResolveChain(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755))
	must(t, os.Mkdir(filepath.Join(root, "a", "d"), 0o755))
	must(t, os.Symlink("a/b", filepath.Join(root, "l1")))                     // relative
	must(t, os.Symlink(filepath.Join(root, "l1"), filepath.Join(root, "l2"))) // absolute, to a link
	must(t, os.Symlink("../../d", filepath.Join(root, "a", "b", "c", "up")))
	must(t, os.Symlink("../../nowhere", filepath.Join(root, "a", "b", "c", "dang")))
	// The inputs are not cleaned: filepath.Join would drop "l2/.." before
	// the symlink is followed.
	for in, want := range map[string]string{
		root + "/l2":                  filepath.Join(root, "a", "b"),
		root + "/l2/":                 filepath.Join(root, "a", "b"),
		root + "/l2/c":                filepath.Join(root, "a", "b", "c"),
		root + "/l2/c/up":             filepath.Join(root, "a", "d"),
		root + "/l2/..":               filepath.Join(root, "a"),
		root + "/l2/c/../c":           filepath.Join(root, "a", "b", "c"),
		root + "/l2/./c//up/../../b":  filepath.Join(root, "b"), // up is a/d; b is missing
		root + "/l2/new":              filepath.Join(root, "a", "b", "new"),
		root + "/l2/new/":             filepath.Join(root, "a", "b", "new"),
		root + "/l2/new/deeper/still": filepath.Join(root, "a", "b", "new", "deeper", "still"),
	} {
		got, err := ResolvePath(in)
		if err != nil || got != want {
			t.Errorf("ResolvePath(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	for in, want := range map[string]error{
		root + "/l2/new/x/../y": syscall.ENOENT,
		root + "/l2/new/..":     syscall.ENOENT,
		root + "/l2/new/.":      syscall.ENOENT,
		root + "/l2/new/./x":    syscall.ENOENT,
		root + "/l2/c/dang":     syscall.EEXIST,
		root + "/l2/c/dang/":    syscall.EEXIST,
		root + "/l2/c/dang/sub": syscall.ENOENT,
	} {
		if got, err := ResolvePath(in); !errors.Is(err, want) {
			t.Errorf("ResolvePath(%s) = %s, %v; want %v", in, got, err, want)
		}
		if _, err := Resolve(in); !errors.Is(err, want) {
			t.Errorf("Resolve(%s) = %v; want %v", in, err, want)
		}
	}
	for _, p := range []string{filepath.Join(root, "a", "b", "new"), filepath.Join(root, "a", "nowhere")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s was created", p)
		}
	}
	got, err := Resolve(filepath.Join(root, "l2", "c", "up", "e"))
	if want := filepath.Join(root, "a", "d", "e"); err != nil || got != want {
		t.Errorf("Resolve = %s, %v; want %s", got, err, want)
	}
}

// TestResolveRefusedCreatesNothing (review round 2, N2): a path refused
// further on leaves no directory behind, also when its missing directories
// come first.
func TestResolveRefusedCreatesNothing(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Chmod(shared, 0o777))
	must(t, os.Mkdir(filepath.Join(shared, "env"), 0o755))
	base := filepath.Join(root, "base")
	must(t, os.Mkdir(base, 0o755))
	must(t, os.Symlink(filepath.Join(shared, "env"), filepath.Join(base, "x")))
	for _, p := range []string{
		base + "/missing/../x",       // ENOENT, as the kernel says
		base + "/x/sub",              // through the shared directory: unsafe
		base + "/new/deeper/../../x", // ENOENT
	} {
		if _, err := Resolve(p); err == nil {
			t.Errorf("Resolve(%s) succeeded", p)
		}
		if _, err := ResolvePath(p); err == nil {
			t.Errorf("ResolvePath(%s) succeeded", p)
		}
	}
	if ents, _ := os.ReadDir(base); len(ents) != 1 {
		t.Errorf("created %v", ents)
	}
	if ents, _ := os.ReadDir(filepath.Join(shared, "env")); len(ents) != 0 {
		t.Errorf("created %v in the shared directory", ents)
	}
	// The same symlink named directly is refused too.
	if _, err := ResolvePath(filepath.Join(base, "x")); !errors.Is(err, ErrUnsafe) {
		t.Errorf("base/x: %v", err)
	}
}

// TestResolveTrailingSlash (review round 2, N5): a trailing slash after a
// file is ENOTDIR, as for the kernel; after a directory or a missing
// component it is fine.
func TestResolveTrailingSlash(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "file")
	must(t, os.WriteFile(f, nil, 0o600))
	must(t, os.Symlink("file", filepath.Join(root, "lf")))
	must(t, os.Symlink("file/", filepath.Join(root, "lfs")))
	for _, p := range []string{f + "/", root + "/lf/", root + "/lfs", f + "/."} {
		if _, err := ResolvePath(p); !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("ResolvePath(%s) = %v, want ENOTDIR", p, err)
		}
	}
	if got, err := ResolvePath(root + "/lf"); err != nil || got != f {
		t.Errorf("ResolvePath(lf) = %s, %v", got, err)
	}
	if got, err := ResolvePath(root + "/"); err != nil || got != root {
		t.Errorf("ResolvePath(root/) = %s, %v", got, err)
	}
}

// TestResolveLoop: a symlink loop, and a chain longer than 40 links, are
// errors (ELOOP), not hangs.
func TestResolveLoop(t *testing.T) {
	root := t.TempDir()
	must(t, os.Symlink("b", filepath.Join(root, "a")))
	must(t, os.Symlink("a", filepath.Join(root, "b")))
	for _, f := range []func(string) (string, error){Resolve, ResolvePath} {
		if _, err := f(filepath.Join(root, "a", "x")); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("loop: %v", err)
		}
	}
	must(t, os.Mkdir(filepath.Join(root, "end"), 0o755))
	prev := "end"
	for i := 0; i < 41; i++ {
		name := fmt.Sprintf("c%02d", i)
		must(t, os.Symlink(prev, filepath.Join(root, name)))
		prev = name
	}
	if _, err := ResolvePath(filepath.Join(root, "c39")); err != nil {
		t.Errorf("40 links: %v", err)
	}
	if _, err := ResolvePath(filepath.Join(root, "c40")); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("41 links: %v", err)
	}
}

// TestResolveNotADirectory: a file on the way is an I/O error (ENOTDIR),
// not a safety refusal, and nothing is removed.
func TestResolveNotADirectory(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "file")
	must(t, os.WriteFile(f, []byte("x"), 0o600))
	for _, fn := range []func(string) (string, error){Resolve, ResolvePath} {
		_, err := fn(filepath.Join(f, "cache"))
		if !errors.Is(err, syscall.ENOTDIR) || errors.Is(err, ErrUnsafe) {
			t.Errorf("err = %v, want ENOTDIR", err)
		}
	}
	if data, err := os.ReadFile(f); err != nil || string(data) != "x" {
		t.Errorf("file changed: %q, %v", data, err)
	}
}

// TestResolveCreates: a missing directory is created with its parents,
// never group or other writable.
func TestResolveCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "cache")
	got, err := Resolve(dir)
	must(t, err)
	fi, err := os.Lstat(got)
	must(t, err)
	if !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("created %v", fi.Mode())
	}
}

// TestCheckEntry: entries must belong to the user and not be writable by
// group or others.
func TestCheckEntry(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		mode fs.FileMode
		ok   bool
	}{{0o600, true}, {0o644, true}, {0o755, true}, {0o664, false}, {0o646, false}, {0o666, false}} {
		p := filepath.Join(dir, tc.mode.String())
		must(t, os.WriteFile(p, nil, 0o600))
		must(t, os.Chmod(p, tc.mode))
		fi, err := os.Lstat(p)
		must(t, err)
		if err := CheckEntry(p, fi); (err == nil) != tc.ok {
			t.Errorf("mode %v: %v", tc.mode, err)
		}
	}
	// A file of another user (root's /etc/passwd stands in).
	if fi, err := os.Lstat("/etc/passwd"); err == nil {
		if err := CheckEntry("/etc/passwd", fi); !errors.Is(err, ErrUnsafe) {
			t.Errorf("root's file: %v", err)
		}
	}
}

// TestEnsureDir: a missing directory is created, an existing private one
// accepted; a file or symlink there is refused and left alone (review
// round 1, B1: goomba deletes nothing it did not create), and so is a
// directory others could change.
func TestEnsureDir(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	target := t.TempDir()
	for _, tc := range []struct {
		name  string
		setup func(p string)
		ok    bool
	}{
		{"missing", func(string) {}, true},
		{"directory", func(p string) { must(t, os.Mkdir(p, 0o755)) }, true},
		{"file", func(p string) { must(t, os.WriteFile(p, []byte("the user's"), 0o600)) }, false},
		{"symlink", func(p string) { must(t, os.Symlink(target, p)) }, false},
		{"shared directory", func(p string) { must(t, os.Mkdir(p, 0o755)); must(t, os.Chmod(p, 0o777)) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name)
			tc.setup(p)
			before, _ := os.Lstat(p)
			err := EnsureDir(p)
			if !tc.ok {
				if !errors.Is(err, ErrUnsafe) {
					t.Fatalf("err = %v, want ErrUnsafe", err)
				}
				after, err := os.Lstat(p)
				if err != nil || !os.SameFile(before, after) {
					t.Errorf("%s was replaced: %v", p, err)
				}
				return
			}
			must(t, err)
			fi, err := os.Lstat(p)
			must(t, err)
			if !fi.IsDir() {
				t.Errorf("%s is %v", p, fi.Mode())
			}
		})
	}
	if data, err := os.ReadFile(filepath.Join(dir, "file")); err != nil || string(data) != "the user's" {
		t.Errorf("the file changed: %q, %v", data, err)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("wrote through the symlink: %v", ents)
	}
}
