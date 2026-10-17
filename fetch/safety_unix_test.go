//go:build unix

package fetch

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skipIfRoot skips ownership tests when running as root, who owns
// everything it creates and passes every check by design.
func skipIfRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
}

// TestCacheRefusesSharedDirectories: a cache, content directory, lock
// directory or extracted tree that others could change is refused with
// ErrUnsafeCache, before anything is read from it.
func TestCacheRefusesSharedDirectories(t *testing.T) {
	skipIfRoot(t)
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, c *Cache)
	}{
		{"cache directory 0777", func(t *testing.T, c *Cache) {
			must(t, os.Chmod(c.Dir, 0o777))
		}},
		{"cache directory group-writable", func(t *testing.T, c *Cache) {
			must(t, os.Chmod(c.Dir, 0o775))
			chgrpOther(t, c.Dir)
		}},
		{"parent writable by others without sticky bit", func(t *testing.T, c *Cache) {
			must(t, os.Chmod(filepath.Dir(c.Dir), 0o777))
		}},
		{"content directory 0777", func(t *testing.T, c *Cache) {
			must(t, os.MkdirAll(filepath.Join(c.Dir, wheelSHA), 0o755))
			must(t, os.Chmod(filepath.Join(c.Dir, wheelSHA), 0o777))
		}},
		{"lock directory 0777", func(t *testing.T, c *Cache) {
			must(t, os.MkdirAll(filepath.Join(c.Dir, ".locks"), 0o755))
			must(t, os.Chmod(filepath.Join(c.Dir, ".locks"), 0o777))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			c := &Cache{Dir: filepath.Join(parent, "pkgs"), Client: s.Client(), AllowHTTP: true}
			must(t, os.Mkdir(c.Dir, 0o755))
			tc.setup(t, c)
			hits := s.hits.Load()
			_, err := c.Extracted(context.Background(), wheelRecord(url))
			if !errors.Is(err, ErrUnsafeCache) {
				t.Fatalf("err = %v, want ErrUnsafeCache", err)
			}
			if s.hits.Load() != hits {
				t.Error("downloaded into an unsafe cache")
			}
		})
	}
	t.Run("sticky parent is fine", func(t *testing.T) {
		parent := t.TempDir()
		must(t, os.Chmod(parent, 0o777|fs.ModeSticky))
		c := &Cache{Dir: filepath.Join(parent, "pkgs"), Client: s.Client(), AllowHTTP: true}
		if _, err := c.Extracted(context.Background(), wheelRecord(url)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestCacheRefusesSharedTree: a tree with a valid marker whose directory or
// marker others could change is refused, not used.
func TestCacheRefusesSharedTree(t *testing.T) {
	skipIfRoot(t)
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	for _, which := range []string{"tree", "marker"} {
		t.Run(which, func(t *testing.T) {
			c := newCache(t, s)
			p, err := c.Extracted(context.Background(), wheelRecord(url))
			must(t, err)
			target := p
			if which == "marker" {
				target = filepath.Join(p, markerName)
			}
			fi, err := os.Lstat(target)
			must(t, err)
			must(t, os.Chmod(target, fi.Mode().Perm()|0o022))
			c2 := &Cache{Dir: c.Dir, Client: s.Client(), AllowHTTP: true}
			if _, err := c2.Extracted(context.Background(), wheelRecord(url)); !errors.Is(err, ErrUnsafeCache) {
				t.Fatalf("err = %v, want ErrUnsafeCache", err)
			}
		})
	}
}

// TestCacheFileModes: markers and sidecars are private to the user.
func TestCacheFileModes(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	p, err := c.Extracted(context.Background(), wheelRecord(url))
	must(t, err)
	for _, f := range []string{filepath.Join(p, markerName), archivePath(c, wheelSHA, wheelFn) + ".sha256"} {
		fi, err := os.Lstat(f)
		must(t, err)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", filepath.Base(f), fi.Mode().Perm())
		}
	}
	for _, d := range []string{filepath.Join(c.Dir, wheelSHA), filepath.Join(c.Dir, ".locks"), p} {
		fi, err := os.Lstat(d)
		must(t, err)
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s: mode %v is writable by others", d, fi.Mode().Perm())
		}
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

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestCachePrivateGroup: a group-writable cache directory, or directory
// above it, is accepted when it belongs to the user and to the user's
// private group (gid == uid == egid), and refused with any other group.
func TestCachePrivateGroup(t *testing.T) {
	skipIfRoot(t)
	if os.Getegid() != os.Geteuid() {
		t.Skip("the user's group id differs from its user id")
	}
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	parent := filepath.Join(t.TempDir(), "shared")
	must(t, os.Mkdir(parent, 0o775))
	must(t, os.Chmod(parent, 0o775))
	c := &Cache{Dir: filepath.Join(parent, "pkgs"), Client: s.Client(), AllowHTTP: true}
	must(t, os.Mkdir(c.Dir, 0o775))
	must(t, os.Chmod(c.Dir, 0o775))
	if _, err := c.Extracted(context.Background(), wheelRecord(url)); err != nil {
		t.Fatalf("private group: %v", err)
	}
	chgrpOther(t, parent)
	c2 := &Cache{Dir: c.Dir, Client: s.Client(), AllowHTTP: true}
	if _, err := c2.Extracted(context.Background(), wheelRecord(url)); !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("other group above the cache: err = %v, want ErrUnsafeCache", err)
	}
}

// TestCacheResolvedOnce: a Cache resolves Dir once; re-pointing a symlink
// on the way to it afterwards (here one in a directory others can write)
// changes nothing for that Cache (review round 2, S1).
func TestCacheResolvedOnce(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	root := t.TempDir()
	private := filepath.Join(root, "private")
	must(t, os.Mkdir(private, 0o755))
	shared := filepath.Join(root, "shared")
	must(t, os.Mkdir(shared, 0o777))
	must(t, os.Chmod(shared, 0o777))
	link := filepath.Join(shared, "cache")
	must(t, os.Symlink(private, link))
	c := &Cache{Dir: link, Client: s.Client(), AllowHTTP: true}
	p, err := c.Extracted(context.Background(), wheelRecord(url))
	must(t, err)
	if !strings.HasPrefix(p, private+string(filepath.Separator)) {
		t.Fatalf("tree %s is not below the resolved directory %s", p, private)
	}
	other := filepath.Join(root, "other")
	must(t, os.Mkdir(other, 0o755))
	must(t, os.Remove(link))
	must(t, os.Symlink(other, link))
	p2, err := c.Extracted(context.Background(), wheelRecord(url))
	must(t, err)
	if p2 != p {
		t.Errorf("second use went to %s, not %s", p2, p)
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Errorf("the re-pointed directory was used: %v", ents)
	}
}

// TestEmptyDirRefused: a Cache without Dir is refused and writes nothing
// in the working directory (a key once landed in this package's directory
// that way).
func TestEmptyDirRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := (&Cache{}).markerKey(); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("markerKey with no Dir: %v", err)
	}
	if ents, _ := os.ReadDir("."); len(ents) != 0 {
		t.Errorf("wrote %v", ents)
	}
}
