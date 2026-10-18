package goomba

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/record"
)

// cancelEnv is a small environment with a python <-> pip cycle, a noarch
// package to byte-compile and a few platform packages.
func cancelEnv(t testing.TB, python testPkg) []testPkg {
	return []testPkg{
		python,
		buildPkg(t, tspec{name: "pip", version: "1", build: "0", subdir: "noarch", noarch: "python", depends: []string{"python"},
			files: []tfile{{path: "site-packages/pip/__init__.py", data: "x = 1\n"}}, entryPoints: []string{"pip = pip:main"}}),
		buildPkg(t, tspec{name: "aaa", version: "1", build: "0", files: []tfile{{path: "lib/a", data: "a"}, {path: "lib/a.so", link: "a"}}}),
		buildPkg(t, tspec{name: "bbb", version: "1", build: "0", depends: []string{"aaa"}, files: []tfile{{path: "lib/b", data: "b"}}}),
		buildPkg(t, tspec{name: "ccc", version: "1", build: "0", depends: []string{"bbb"}, files: []tfile{{path: "share/c/d/e", data: "c"}}}),
	}
}

// TestInstallCancelAtEveryEvent cancels the context when the k-th progress
// event arrives, for every k: the call must fail with context.Canceled and
// leave no prefix, except when the last event (done) is reached.
func TestInstallCancelAtEveryEvent(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, []string{"pip"}, cancelEnv(t, fakePython(t, t.TempDir(), 0, "pip"))...)
	opt := c.options(t)
	// Count the events of a full run.
	var total atomic.Int64
	opt.Progress = func(Event) { total.Add(1) }
	must(t, InstallLock(context.Background(), filepath.Join(t.TempDir(), "full"), lf, opt))
	n := int(total.Load())
	if n < 2*len(lf.Packages)+3 {
		t.Fatalf("only %d events", n)
	}
	for k := 1; k <= n; k++ {
		for _, conc := range []int{1, 4} {
			t.Run(fmt.Sprintf("event%d-conc%d", k, conc), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var seen atomic.Int64
				var last atomic.Value
				o := opt
				o.Concurrency = conc
				o.PkgsDir = filepath.Join(t.TempDir(), "pkgs") // fresh cache: fetch events too
				o.Progress = func(e Event) {
					if int(seen.Add(1)) == k {
						last.Store(e)
						cancel()
					}
				}
				prefix := filepath.Join(t.TempDir(), "env")
				err := InstallLock(ctx, prefix, lf, o)
				if e, _ := last.Load().(Event); e.Kind == EventDone {
					if err != nil {
						t.Fatalf("cancelled after done: %v", err)
					}
					return
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
				assertNoPrefix(t, prefix)
				if _, err := os.Stat(o.PkgsDir); err != nil {
					t.Errorf("package cache removed: %v", err)
				}
			})
		}
	}
}

// TestInstallCancelDuringDownload cancels when the j-th download starts.
func TestInstallCancelDuringDownload(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	for j := 1; j <= len(lf.Packages); j++ {
		t.Run(fmt.Sprint(j), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var n atomic.Int64
			c.mu.Lock()
			c.onRequest = func(string) {
				if int(n.Add(1)) == j {
					cancel()
				}
			}
			c.mu.Unlock()
			defer func() { c.mu.Lock(); c.onRequest = nil; c.mu.Unlock() }()
			prefix := filepath.Join(t.TempDir(), "env")
			err := InstallLock(ctx, prefix, lf, c.options(t))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			assertNoPrefix(t, prefix)
		})
	}
}

// TestInstallCancelDuringCompile kills a slow interpreter.
func TestInstallCancelDuringCompile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script interpreter")
	}
	slow := buildPkg(t, tspec{name: "python", version: "3.11.9", build: "h0_cpython", files: []tfile{
		{path: "bin/python3.11", data: "#!/bin/sh\n/bin/sleep 60 &\nwait\n", mode: 0o755},
	}})
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, slow)...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := c.options(t)
	opt.Progress = func(e Event) {
		if e.Kind == EventCompiling {
			time.AfterFunc(200*time.Millisecond, cancel)
		}
	}
	prefix := filepath.Join(t.TempDir(), "env")
	start := time.Now()
	err := InstallLock(ctx, prefix, lf, opt)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("cancellation took %v", d)
	}
	assertNoPrefix(t, prefix)
}

// TestInstallPanicRollsBack: a panic in the progress callback, for every
// event kind and whichever goroutine reports it (fetched and linked events
// come from worker goroutines), cancels the transaction, which is rolled
// back, and is re-raised on the goroutine that called InstallLock. After
// EventDone the environment is complete and stays. (Review S2.)
func TestInstallPanicRollsBack(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), 0))...)
	for _, kind := range []EventKind{EventFetched, EventLinked, EventCompiling, EventCompiled, EventDone} {
		t.Run(kind.String(), func(t *testing.T) {
			opt := c.options(t)
			var n atomic.Int64
			opt.Progress = func(e Event) {
				if e.Kind == kind && n.Add(1) == 2 || e.Kind == kind && (kind == EventCompiling || kind == EventCompiled || kind == EventDone) {
					panic("boom in " + kind.String())
				}
			}
			prefix := filepath.Join(t.TempDir(), "env")
			func() {
				defer func() {
					if r := recover(); r != "boom in "+kind.String() {
						t.Errorf("recovered %v", r)
					}
				}()
				_ = InstallLock(context.Background(), prefix, lf, opt)
			}()
			if kind == EventDone {
				if _, err := os.Stat(filepath.Join(prefix, "conda-meta", "history")); err != nil {
					t.Errorf("complete environment removed: %v", err)
				}
				return
			}
			assertNoPrefix(t, prefix)
			// The lock was released: the prefix can be created now.
			opt.Progress = nil
			must(t, InstallLock(context.Background(), prefix, lf, opt))
		})
	}
}

func TestInstallPycOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exit    int
		wantErr bool
	}{
		{"some sources fail", 1, false},
		{"interpreter dies", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newChannel(t)
			lf := c.lockfile(t, nil, cancelEnv(t, fakePython(t, t.TempDir(), tc.exit))...)
			prefix := filepath.Join(t.TempDir(), "env")
			err := InstallLock(context.Background(), prefix, lf, c.options(t))
			if tc.wantErr {
				if !errors.Is(err, ErrCompilePyc) {
					t.Fatalf("err = %v, want ErrCompilePyc", err)
				}
				assertNoPrefix(t, prefix)
				return
			}
			must(t, err)
		})
	}
	t.Run("no interpreter", func(t *testing.T) {
		c := newChannel(t)
		nopy := buildPkg(t, tspec{name: "python", version: "3.11.9", build: "h0_cpython", files: []tfile{{path: "lib/libpython.so", data: "x"}}})
		lf := c.lockfile(t, nil, cancelEnv(t, nopy)...)
		prefix := filepath.Join(t.TempDir(), "env")
		err := InstallLock(context.Background(), prefix, lf, c.options(t))
		if !errors.Is(err, ErrCompilePyc) {
			t.Fatalf("err = %v, want ErrCompilePyc", err)
		}
		assertNoPrefix(t, prefix)
		// Without byte-compilation the same lockfile installs.
		opt := c.options(t)
		opt.CompilePyc = false
		must(t, InstallLock(context.Background(), prefix, lf, opt))
	})
}

func TestInstallPrefixStates(t *testing.T) {
	c, cb := newChannel(t), newChannel(t)
	logDir := t.TempDir()
	good := c.lockfile(t, nil, cancelEnv(t, fakePython(t, logDir, 0))...)
	bad := cb.lockfile(t, nil, cancelEnv(t, fakePython(t, logDir, 3))...) // fails after linking

	t.Run("existing empty directory", func(t *testing.T) {
		prefix := t.TempDir()
		must(t, InstallLock(context.Background(), prefix, good, c.options(t)))
		if _, err := os.Stat(filepath.Join(prefix, "conda-meta", "history")); err != nil {
			t.Error(err)
		}
	})
	t.Run("existing empty directory, failure empties it", func(t *testing.T) {
		prefix := t.TempDir()
		err := InstallLock(context.Background(), prefix, bad, cb.options(t))
		if !errors.Is(err, ErrCompilePyc) {
			t.Fatalf("err = %v", err)
		}
		ents, err := os.ReadDir(prefix)
		must(t, err)
		if len(ents) != 0 {
			t.Errorf("prefix not emptied: %v", ents)
		}
	})
	t.Run("missing parents are removed again", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "a", "b", "env")
		err := InstallLock(context.Background(), prefix, bad, cb.options(t))
		if !errors.Is(err, ErrCompilePyc) {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
			t.Errorf("created parent left behind: %v", err)
		}
	})
	t.Run("relative prefix", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		must(t, InstallLock(context.Background(), "rel-env", good, c.options(t)))
		hist, err := os.ReadFile(filepath.Join(dir, "rel-env", "conda-meta", "history"))
		must(t, err)
		if !strings.Contains(string(hist), "# cmd: goomba create -p "+filepath.Join(dir, "rel-env")+"\n") {
			t.Errorf("history:\n%s", hist)
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, prefix string)
	}{
		{"not empty", func(t *testing.T, prefix string) {
			must(t, os.Mkdir(prefix, 0o755))
			must(t, os.WriteFile(filepath.Join(prefix, "keep"), []byte("mine"), 0o644))
		}},
		{"a file", func(t *testing.T, prefix string) {
			must(t, os.WriteFile(prefix, []byte("mine"), 0o644))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "env")
			tc.setup(t, prefix)
			before, err := os.Lstat(prefix)
			must(t, err)
			hits := c.totalHits()
			err = InstallLock(context.Background(), prefix, good, c.options(t))
			if !errors.Is(err, ErrPrefixNotEmpty) {
				t.Fatalf("err = %v, want ErrPrefixNotEmpty", err)
			}
			if c.totalHits() != hits {
				t.Error("downloaded for an unusable prefix")
			}
			after, err := os.Lstat(prefix)
			must(t, err)
			// Reading a directory may update its access time (notably on
			// macOS). Check the identity and metadata an install could change.
			if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Errorf("prefix changed: %v -> %v", before, after)
			}
			keep := prefix
			if tc.name == "not empty" {
				keep = filepath.Join(prefix, "keep")
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "mine" {
				t.Errorf("original contents changed: %q, %v", data, err)
			}
		})
	}
	t.Run("locked", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no flock")
		}
		prefix := t.TempDir()
		l, err := lockDir(prefix)
		must(t, err)
		defer unlockDir(l)
		err = InstallLock(context.Background(), prefix, good, c.options(t))
		if !errors.Is(err, ErrPrefixLocked) {
			t.Fatalf("err = %v, want ErrPrefixLocked", err)
		}
		ents, err := os.ReadDir(prefix)
		must(t, err)
		if len(ents) != 0 {
			t.Errorf("locked prefix touched: %v", ents)
		}
	})
	t.Run("concurrent installs into one prefix", func(t *testing.T) {
		prefix := filepath.Join(t.TempDir(), "env")
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = InstallLock(context.Background(), prefix, good, c.options(t))
			}()
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrPrefixLocked), errors.Is(err, ErrPrefixNotEmpty):
			default:
				t.Errorf("unexpected error %v", err)
			}
		}
		if ok != 1 {
			t.Errorf("%d installs succeeded, want 1: %v", ok, errs)
		}
		if _, err := os.Stat(filepath.Join(prefix, "conda-meta", incompleteMarker)); !os.IsNotExist(err) {
			t.Errorf("marker left: %v", err)
		}
	})
}

func TestVerifySymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "dir", "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "file"), nil, 0o644))
	links := map[string]string{
		"ok": "file", "dir/up": "../file", "chain": "dir/up", "dirlink": "dir", "dir/sub/deep": "../../dir/../file",
		"dangling": "missing/x", "danglingdir/x": "",
	}
	delete(links, "danglingdir/x")
	for l, target := range links {
		must(t, os.Symlink(target, filepath.Join(root, l)))
	}
	must(t, verifySymlinks(context.Background(), root))
	for _, tc := range []struct{ link, target string }{
		{"esc", "../outside"},
		{"dir/esc", "../../x"},
		{"abs", "/etc/passwd"},
		{"dangling-esc", "missing/../../x"},
		{"viadir", "dirlink/../.."},
		{"loop1", "loop2"},
		{"empty", ""},
	} {
		t.Run(tc.link, func(t *testing.T) {
			p := filepath.Join(root, tc.link)
			if tc.target == "" {
				t.Skip("empty symlink targets cannot be created")
			}
			must(t, os.Symlink(tc.target, p))
			if tc.link == "loop1" {
				must(t, os.Symlink("loop1", filepath.Join(root, "loop2")))
				defer os.Remove(filepath.Join(root, "loop2"))
			}
			defer os.Remove(p)
			err := verifySymlinks(context.Background(), root)
			if !errors.Is(err, link.ErrUnsafePath) {
				t.Fatalf("err = %v, want ErrUnsafePath", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifySymlinks(ctx, root); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled walk: %v", err)
	}
}

func TestRunBounded(t *testing.T) {
	errA, errB := errors.New("a"), errors.New("b")
	var inflight, peak atomic.Int64
	err := runBounded(context.Background(), 20, 3, func(ctx context.Context, i int) error {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		switch i {
		case 2:
			// Fails late, but has the lowest index of the real failures.
			time.Sleep(50 * time.Millisecond)
			return errB
		case 1:
			<-ctx.Done()
			return ctx.Err()
		case 0:
			time.Sleep(10 * time.Millisecond)
			return errA
		}
		return nil
	})
	if !errors.Is(err, errA) {
		t.Errorf("err = %v, want a", err)
	}
	if peak.Load() > 3 {
		t.Errorf("%d in flight", peak.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	if err := runBounded(ctx, 5, 2, func(ctx context.Context, i int) error { ran = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	_ = ran
	if err := runBounded(context.Background(), 0, 0, nil); err != nil {
		t.Error(err)
	}
}

func TestFillFromIndex(t *testing.T) {
	for _, tc := range []struct {
		index   string
		want    record.PackageRecord
		wantErr bool
	}{
		{`{}`, record.PackageRecord{}, false},
		{`{"timestamp": 1700000000123, "license": "MIT", "track_features": "a,b c", "features": "f"}`,
			record.PackageRecord{Timestamp: 1700000000123, License: "MIT", TrackFeatures: []string{"a", "b", "c"}, Features: "f"}, false},
		{`{"timestamp": 1700000000}`, record.PackageRecord{Timestamp: 1700000000000}, false},
		{`{"timestamp": 1.7e12}`, record.PackageRecord{Timestamp: 1700000000000}, false},
		{`{"track_features": ["x", " ", "y"]}`, record.PackageRecord{TrackFeatures: []string{"x", "y"}}, false},
		{`{"track_features": "", "license": null, "timestamp": null}`, record.PackageRecord{}, false},
		{`{"timestamp": "soon"}`, record.PackageRecord{}, true},
		{`{"timestamp": 1.5}`, record.PackageRecord{}, true},
		{`{"license": 3}`, record.PackageRecord{}, true},
		{`{"track_features": {"a": 1}}`, record.PackageRecord{}, true},
		{`{"features": ["x"]}`, record.PackageRecord{}, true},
	} {
		var index map[string]json.RawMessage
		must(t, json.Unmarshal([]byte(tc.index), &index))
		var got record.PackageRecord
		err := fillFromIndex(&got, index)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v", tc.index, err)
			continue
		}
		if err == nil && fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.index, got, tc.want)
		}
		if err != nil && !errors.Is(err, link.ErrInvalidPackage) {
			t.Errorf("%s: err %v does not wrap ErrInvalidPackage", tc.index, err)
		}
	}
}

func TestRequestedSpec(t *testing.T) {
	u := newUserSpecs([]string{"python=3.11", "numpy>=1.2,<2", "py*", "bad spec (((", "python 3.11.*"})
	for name, want := range map[string]string{
		"python": "python=3.11", // the last match wins, in canonical form
		"numpy":  `numpy[version=">=1.2,<2"]`,
		"pyyaml": "py*",
		"bad":    "",
		"zlib":   "",
	} {
		if got := u.requested(name); got != want {
			t.Errorf("requested(%s) = %q, want %q", name, got, want)
		}
	}
	if fmt.Sprint(u.text) != `[python=3.11 numpy[version=">=1.2,<2"] py* bad spec ((( python=3.11]` {
		t.Errorf("history specs %q", u.text)
	}
}

func TestClaimsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	prefix := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(prefix, "lib"), 0o755))
	must(t, os.WriteFile(filepath.Join(prefix, "lib", "x.so"), nil, 0o644))
	must(t, os.Symlink("lib", filepath.Join(prefix, "lib64")))
	c := claims{}
	dists := []string{"a-1-0", "b-1-0"}
	must(t, c.add(0, []dest{{"lib/x.so", claimFile}, {"Share/Doc", claimFile}}, dists))
	if got := c.owner(prefix, "lib/x.so", 1); got != 0 {
		t.Errorf("exact owner %d", got)
	}
	if got := c.owner(prefix, "lib64/x.so", 1); got != 0 {
		t.Errorf("owner through a symlink %d", got)
	}
	if got := c.owner(prefix, "share/doc", 1); got != 0 {
		t.Errorf("case-folded owner %d", got)
	}
	if got := c.owner(prefix, "lib/x.so", 0); got != -1 {
		t.Errorf("own path owned by %d", got)
	}
	if got := c.owner(prefix, "lib", 1); got != -1 {
		t.Errorf("implicit directory owned by %d", got)
	}
	ce := c.attributeExists(prefix, 1, []dest{{"lib", claimDir}, {"none", claimFile}, {"lib64/x.so", claimFile}}, dists)
	if ce == nil || ce.Path != "lib64/x.so" || ce.Packages != [2]string{"a-1-0", "b-1-0"} {
		t.Errorf("attributeExists = %+v", ce)
	}
}

func TestPythonAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"3.11": true, "3.6": true, "3.13t": true, "4.0": true, "3.5": false, "2.7": false, "3": false, "x.y": false} {
		if got := pythonAtLeast(v, 3, 6); got != want {
			t.Errorf("pythonAtLeast(%q, 3, 6) = %v", v, got)
		}
	}
	if !pythonAtLeast("3.4", 3, 4) || pythonAtLeast("3.3", 3, 4) {
		t.Error("pythonAtLeast 3.4")
	}
}
