//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package rmtree

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestRemoveAll removes a tree with read-only directories, deep nesting
// and symlinks to data outside (the links only), and copes with a missing
// path and a symlink at the top.
func TestRemoveAll(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	must(t, os.MkdirAll(outside, 0o755))
	must(t, os.WriteFile(filepath.Join(outside, "data"), []byte("x"), 0o644))
	tree := filepath.Join(base, "tree")
	deep := tree
	for i := range 40 {
		deep = filepath.Join(deep, "d"+strconv.Itoa(i))
	}
	must(t, os.MkdirAll(deep, 0o755))
	must(t, os.WriteFile(filepath.Join(deep, "f"), nil, 0o444))
	ro := filepath.Join(tree, "ro", "deeper")
	must(t, os.MkdirAll(ro, 0o755))
	must(t, os.WriteFile(filepath.Join(ro, "f"), nil, 0o444))
	must(t, os.Chmod(filepath.Join(tree, "ro"), 0o555))
	must(t, os.Symlink(outside, filepath.Join(tree, "out")))
	must(t, os.Symlink(outside, filepath.Join(tree, "ro", "deeper", "out")))
	must(t, RemoveAll(tree))
	if _, err := os.Lstat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tree left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "data")); err != nil {
		t.Errorf("data outside removed: %v", err)
	}
	must(t, RemoveAll(tree)) // missing: fine
	link := filepath.Join(base, "link")
	must(t, os.Symlink(outside, link))
	must(t, RemoveAll(link))
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "data")); err != nil {
		t.Errorf("data behind the symlink removed: %v", err)
	}
	for _, bad := range []string{"/", ".", ".."} {
		if err := RemoveAll(bad); err == nil {
			t.Errorf("RemoveAll(%q) succeeded", bad)
		}
	}
}

// TestRemoveAtNamesEntry: an error names the entry by the path the caller
// gave plus the components below it.
func TestRemoveAtNamesEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "a", "b", "locked")
	must(t, os.MkdirAll(locked, 0o755))
	must(t, os.WriteFile(filepath.Join(locked, "f"), nil, 0o644))
	// A directory that cannot be read even after chmod: owned by us, so
	// chmod works; make the parent unwritable instead, so the final
	// rmdir of "b" fails.
	must(t, os.Chmod(filepath.Join(base, "a"), 0o555))
	defer os.Chmod(filepath.Join(base, "a"), 0o755)
	pfd, err := os.Open(filepath.Join(base, "a"))
	must(t, err)
	defer pfd.Close()
	err = RemoveAt(int(pfd.Fd()), "b", "prefix-rel/b")
	if err == nil || !strings.Contains(err.Error(), "prefix-rel/b") {
		t.Errorf("err = %v, want it to name prefix-rel/b", err)
	}
}
