//go:build unix

package repodata

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != os.Getegid() && g != os.Geteuid() {
			if err := os.Chown(p, -1, g); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Skip("the user belongs to no group but its own")
}

// cacheTestServer serves a sharded channel /c (linux-64 and noarch, with
// package foo) and the same packages as a full index under /f.
func cacheTestServer(t *testing.T) *testServer {
	ts := newTestServer(t)
	ts.putShards("/c", "linux-64", "", "", "", map[string][]byte{"foo": simpleShard(t, "foo", "1.0")})
	ts.putShards("/c", "noarch", "", "", "", map[string][]byte{"bar": simpleShard(t, "bar", "2.0")})
	ts.put("/f/noarch/repodata.json", []byte(noarchDoc))
	return ts
}

// openBoth opens /c as a ShardedChannel (loading foo's shard) and /f as a
// FullIndex with opt, returning the first error.
func openBoth(t *testing.T, ts *testServer, opt Options) error {
	ctx := ctxT(t)
	c, err := NewShardedChannel(ctx, ts.url("/c"), opt)
	if err != nil {
		return err
	}
	if _, err := c.Records(ctx, "foo"); err != nil {
		return err
	}
	_, err = NewFullIndex(ctx, ts.url("/f"), opt)
	return err
}

// TestUnsafeCacheDirRefused: a cache directory that others could change,
// or under a directory others could change, is refused with ErrUnsafeCache
// before anything is downloaded, online and offline.
func TestUnsafeCacheDirRefused(t *testing.T) {
	skipIfRoot(t)
	ts := cacheTestServer(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"cache directory 0777", func(t *testing.T, dir string) {
			must(t, os.Chmod(dir, 0o777))
		}},
		{"cache directory group-writable, other group", func(t *testing.T, dir string) {
			must(t, os.Chmod(dir, 0o775))
			chgrpOther(t, dir)
		}},
		{"parent writable by others without sticky bit", func(t *testing.T, dir string) {
			must(t, os.Chmod(filepath.Dir(dir), 0o777))
		}},
		{"index directory 0777", func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, "index"), 0o755))
			must(t, os.Chmod(filepath.Join(dir, "index"), 0o777))
		}},
		{"shards directory 0777", func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, "shards"), 0o755))
			must(t, os.Chmod(filepath.Join(dir, "shards"), 0o777))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "parent")
			must(t, os.Mkdir(parent, 0o755))
			dir := filepath.Join(parent, "cache")
			must(t, os.Mkdir(dir, 0o755))
			tc.setup(t, dir)
			before := ts.totalRequests("/")
			for _, offline := range []bool{false, true} {
				err := openBoth(t, ts, Options{CacheDir: dir, Platform: "linux-64", Offline: offline})
				if !errors.Is(err, ErrUnsafeCache) {
					t.Errorf("offline=%v: err = %v, want ErrUnsafeCache", offline, err)
				}
				if _, err := NewChannel(ctxT(t), ts.url("/c"), Options{CacheDir: dir, Platform: "linux-64", Offline: offline}); !errors.Is(err, ErrUnsafeCache) {
					t.Errorf("NewChannel offline=%v: err = %v, want ErrUnsafeCache", offline, err)
				}
			}
			if n := ts.totalRequests("/") - before; n != 0 {
				t.Errorf("%d requests with an unsafe cache", n)
			}
		})
	}
	t.Run("sticky parent is fine", func(t *testing.T) {
		parent := t.TempDir()
		must(t, os.Chmod(parent, 0o777|fs.ModeSticky))
		if err := openBoth(t, ts, Options{CacheDir: filepath.Join(parent, "cache"), Platform: "linux-64"}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestUnsafeCacheFileRefused: an index entry, freshness sidecar or shard
// that group or others may write is refused (ErrUnsafeCache), not used or
// silently replaced: it can only be there if the cache was not private.
func TestUnsafeCacheFileRefused(t *testing.T) {
	skipIfRoot(t)
	ts := cacheTestServer(t)
	for _, which := range []string{"index entry", "shard", "freshness sidecar"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			opt := Options{CacheDir: dir, Platform: "linux-64"}
			must(t, openBoth(t, ts, opt))
			if which == "freshness sidecar" {
				// A revalidation (MaxAge 0, the server answers 304)
				// writes the sidecar next to each entry.
				must(t, openBoth(t, ts, opt))
			}
			var victim string
			switch which {
			case "index entry":
				victim = onlyFile(t, filepath.Join(dir, "index", "*-repodata_shards.msgpack.zst"), "")
			case "shard":
				victim = onlyFile(t, filepath.Join(dir, "shards", "*.msgpack.zst"), "")
			case "freshness sidecar":
				victim = onlyFile(t, filepath.Join(dir, "index", "*-repodata_shards.msgpack.zst.fresh"), "")
			}
			must(t, os.Chmod(victim, 0o666))
			for _, offline := range []bool{false, true} {
				o := opt
				o.Offline = offline
				if err := openBoth(t, ts, o); !errors.Is(err, ErrUnsafeCache) {
					t.Errorf("offline=%v: err = %v, want ErrUnsafeCache", offline, err)
				}
			}
			fi, err := os.Lstat(victim)
			must(t, err)
			if fi.Mode().Perm() != 0o666 {
				t.Errorf("the unsafe file was replaced (mode %v)", fi.Mode().Perm())
			}
		})
	}
}

// onlyFile returns the one file matching glob whose name lacks skip
// (unless skip is ""), failing otherwise.
func onlyFile(t *testing.T, glob, skip string) string {
	t.Helper()
	m, err := filepath.Glob(glob)
	must(t, err)
	var out []string
	for _, p := range m {
		if skip == "" || !strings.Contains(filepath.Base(p), skip) {
			out = append(out, p)
		}
	}
	// Two subdirs: take the first in name order, deterministically.
	if len(out) == 0 {
		t.Fatalf("no file matches %s", glob)
	}
	return out[0]
}

// TestCacheFileModes: cache files are 0600 and the directories the cache
// makes are not writable by group or others, whatever the umask.
func TestCacheFileModes(t *testing.T) {
	ts := cacheTestServer(t)
	dir := t.TempDir()
	must(t, openBoth(t, ts, Options{CacheDir: dir, Platform: "linux-64"}))
	must(t, openBoth(t, ts, Options{CacheDir: dir, Platform: "linux-64"})) // sidecars
	files := 0
	must(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case p == dir: // made by the test (umask 002 makes it 0775)
		case fi.IsDir():
			if fi.Mode().Perm()&0o022 != 0 {
				t.Errorf("%s: mode %v", p, fi.Mode().Perm())
			}
		case fi.Mode().IsRegular():
			files++
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v, want 0600", p, fi.Mode().Perm())
			}
		default:
			t.Errorf("%s: %v", p, fi.Mode())
		}
		return nil
	}))
	if files < 5 {
		t.Errorf("only %d cache files", files)
	}
}

// TestCacheResolvedOnce: a source resolves CacheDir once; re-pointing a
// symlink on the way to it afterwards changes nothing for that source:
// shards loaded later still go to the resolved directory. A symlink in a
// directory others can write is not followed at all (WP8b review round 1,
// S1).
func TestCacheResolvedOnce(t *testing.T) {
	ts := cacheTestServer(t)
	root := t.TempDir()
	private := filepath.Join(root, "private")
	must(t, os.Mkdir(private, 0o755))
	ctx := ctxT(t)
	if os.Geteuid() != 0 {
		shared := filepath.Join(root, "shared")
		must(t, os.Mkdir(shared, 0o777))
		must(t, os.Chmod(shared, 0o777))
		bad := filepath.Join(shared, "cache")
		must(t, os.Symlink(private, bad))
		for _, offline := range []bool{false, true} {
			if err := openBoth(t, ts, Options{CacheDir: bad, Platform: "linux-64", Offline: offline}); !errors.Is(err, ErrUnsafeCache) {
				t.Errorf("symlink in a world-writable directory, offline=%v: %v", offline, err)
			}
		}
		if ents, _ := os.ReadDir(private); len(ents) != 0 {
			t.Fatalf("the symlink was followed: %v", ents)
		}
	}
	link := filepath.Join(root, "cache")
	must(t, os.Symlink(private, link))
	c, err := NewShardedChannel(ctx, ts.url("/c"), Options{CacheDir: link, Platform: "linux-64"})
	must(t, err)
	other := filepath.Join(root, "other")
	must(t, os.Mkdir(other, 0o755))
	must(t, os.Remove(link))
	must(t, os.Symlink(other, link))
	if recs, err := c.Records(ctx, "foo"); err != nil || len(recs) != 1 {
		t.Fatalf("Records: %v, %v", recs, err)
	}
	if m, _ := filepath.Glob(filepath.Join(private, "shards", "*.msgpack.zst")); len(m) != 1 {
		t.Errorf("shard not cached in the resolved directory: %v", m)
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Errorf("the re-pointed directory was used: %v", ents)
	}
}

// TestCachePlantedEntries (WP8b review round 1, B1): a file or symlink
// where the index or shards directory belongs, for example a user's own
// file in a CacheDir that is a working directory, is refused
// (ErrUnsafeCache) and left alone, online and offline; nothing is written
// through a planted symlink. A symlink planted in place of a cache file is
// not followed to another user's file: the source downloads again.
func TestCachePlantedEntries(t *testing.T) {
	ts := cacheTestServer(t)
	elsewhere := t.TempDir()
	for _, tc := range []struct {
		name  string
		plant func(p string)
	}{
		{"user file index", func(p string) { must(t, os.WriteFile(filepath.Join(p, "index"), []byte("the user's notes"), 0o644)) }},
		{"user file shards", func(p string) { must(t, os.WriteFile(filepath.Join(p, "shards"), []byte("the user's notes"), 0o644)) }},
		{"symlink index", func(p string) { must(t, os.Symlink(elsewhere, filepath.Join(p, "index"))) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.plant(dir)
			before := snapshot(t, dir)
			for _, offline := range []bool{true, false} {
				err := openBoth(t, ts, Options{CacheDir: dir, Platform: "linux-64", Offline: offline})
				if !errors.Is(err, ErrUnsafeCache) {
					t.Errorf("offline=%v: err = %v, want ErrUnsafeCache", offline, err)
				}
			}
			if after := snapshot(t, dir); after != before {
				t.Errorf("the cache directory changed:\n%s\nwas\n%s", after, before)
			}
		})
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("wrote through the planted symlink: %v", ents)
	}
	// A cache file replaced by a symlink to a file of root's is not read
	// (it is not a regular file); the source downloads again.
	dir := t.TempDir()
	must(t, openBoth(t, ts, Options{CacheDir: dir, Platform: "linux-64"}))
	entry := onlyFile(t, filepath.Join(dir, "index", "*-repodata_shards.msgpack.zst"), "")
	must(t, os.Remove(entry))
	must(t, os.Symlink("/etc/passwd", entry))
	before := ts.totalRequests("/c/")
	must(t, openBoth(t, ts, Options{CacheDir: dir, Platform: "linux-64"}))
	if ts.totalRequests("/c/") == before {
		t.Error("no download after the planted symlink")
	}
}

// snapshot describes the entries directly in dir: name, type and, for
// files, contents.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	must(t, err)
	var b strings.Builder
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		must(t, err)
		b.WriteString(e.Name() + " " + fi.Mode().String())
		if fi.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			must(t, err)
			b.WriteString(" " + string(data))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TestUncreatableCacheHint (WP8b review round 1, N11): a CacheDir that
// cannot be created leaves the source without a disk cache; online it
// still works, offline ErrNotCached says why.
func TestUncreatableCacheHint(t *testing.T) {
	ts := cacheTestServer(t)
	f := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(f, nil, 0o600))
	opt := Options{CacheDir: filepath.Join(f, "cache"), Platform: "linux-64", Offline: true}
	_, err := NewChannel(ctxT(t), ts.url("/c"), opt)
	if !errors.Is(err, ErrNotCached) || !strings.Contains(err.Error(), "disk cache disabled") || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("offline: %v", err)
	}
	opt.Offline = false
	if _, err := NewChannel(ctxT(t), ts.url("/c"), opt); err != nil {
		t.Errorf("online: %v", err)
	}
}
