package goomba

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/richinsley/goomba/fetch"
	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
)

// longPlaceholder is a build prefix longer than any test prefix, so binary
// replacement fits.
var longPlaceholder = "/build/placeholder_" + strings.Repeat("p", 200)

// envPackages returns the packages of the standard test environment: a fake
// python that depends on pip (a python <-> pip cycle, as in conda-forge's
// python 3.12+), a synthetic pip and prefix-rewriting package, and the real
// archives in testdata (the linux-64 ones only on linux-64).
func envPackages(t testing.TB, logDir string) []testPkg {
	t.Helper()
	pkgs := []testPkg{
		fakePython(t, logDir, 0, "pip", "__unix"),
		buildPkg(t, tspec{
			name: "pip", version: "24.0", build: "pyh_0", subdir: "noarch", noarch: "python",
			depends: []string{"python >=3.8", "wheel"},
			files: []tfile{
				{path: "site-packages/pip/__init__.py", data: "def main():\n    return 0\n"},
				{path: "site-packages/pip/_internal/cli.py", data: "x = 1\n"},
				{path: "site-packages/pip-24.0.dist-info/METADATA", data: "Name: pip\n"},
				{path: "python-scripts/pip-helper", data: "#!/bin/sh\n", mode: 0o755},
			},
			entryPoints: []string{"pip = pip:main", "pip3 = pip:main"},
			index:       map[string]any{"track_features": "pipfeature", "license": "MIT"},
		}),
		buildPkg(t, tspec{
			name: "prefixed", version: "1.0", build: "h0_0", buildNumber: 0,
			files: []tfile{
				{path: "etc/prefixed.conf", data: "root=" + longPlaceholder + "/share\n", placeholder: longPlaceholder, fileMode: "text"},
				{path: "lib/libprefixed.so", data: "\x7fELF\x00" + longPlaceholder + "/lib\x00tail\x00", placeholder: longPlaceholder, fileMode: "binary"},
				{path: "share/prefixed/data", dir: true},
				{path: "share/prefixed/README", data: "readme\n"},
			},
			index: map[string]any{"track_features": []string{"feat_a", "feat_b"}, "timestamp": 1690000000},
		}),
		realPkg(t, "wheel-0.46.3-pyhd8ed1ab_0"),
		realPkg(t, "packaging-26.0-pyhcf101f3_0"),
		realPkg(t, "ca-certificates-2026.2.25-hbd8a1cb_0"),
	}
	if hostPlatform(t) == "linux-64" {
		pkgs = append(pkgs,
			realPkg(t, "libzlib-1.3.2-h25fd6f3_2"),
			realPkg(t, "libgomp-15.2.0-he0feb66_18"),
			realPkg(t, "_openmp_mutex-4.5-20_gnu"),
		)
	}
	return pkgs
}

func TestInstallLock(t *testing.T) {
	t.Setenv("GOOMBA_TEST_LEAK", "1")
	logDir := t.TempDir()
	c := newChannel(t)
	lf := c.lockfile(t, []string{"python=3.11", "pip"}, envPackages(t, logDir)...)
	opt := c.options(t)
	var mu sync.Mutex
	var events []Event
	opt.Progress = func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	prefix := filepath.Join(t.TempDir(), "envs", "test")
	if err := InstallLock(context.Background(), prefix, lf, opt); err != nil {
		t.Fatal(err)
	}
	tree := treeOf(t, prefix)

	// Package files, relocated noarch files, entry points, compiled .pyc.
	for _, p := range []string{
		"bin/python3.11", "bin/python", "etc/prefixed.conf", "lib/libprefixed.so", "share/prefixed/data",
		"lib/python3.11/site-packages/pip/__init__.py",
		"lib/python3.11/site-packages/pip/__pycache__/__init__.cpython-311.pyc",
		"lib/python3.11/site-packages/pip/_internal/__pycache__/cli.cpython-311.pyc",
		"lib/python3.11/site-packages/wheel/__init__.py",
		"bin/pip", "bin/pip3", "bin/pip-helper", "bin/wheel",
		"conda-meta/history", "conda-meta/pip-24.0-pyh_0.json",
	} {
		if _, ok := tree[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	for p := range tree {
		if strings.Contains(p, ".goomba") {
			t.Errorf("leftover %s", p)
		}
	}
	if got := tree["bin/pip"]; !strings.HasPrefix(got, "f -rwxr-xr-x") {
		t.Errorf("bin/pip: %s, want mode 0755", got)
	}

	// Prefix replacement.
	conf, err := os.ReadFile(filepath.Join(prefix, "etc/prefixed.conf"))
	must(t, err)
	if string(conf) != "root="+prefix+"/share\n" {
		t.Errorf("prefixed.conf = %q", conf)
	}
	so, err := os.ReadFile(filepath.Join(prefix, "lib/libprefixed.so"))
	must(t, err)
	want := "\x7fELF\x00" + prefix + "/lib" + strings.Repeat("\x00", len(longPlaceholder)-len(prefix)) + "\x00tail\x00"
	if string(so) != want {
		t.Errorf("libprefixed.so = %q", so)
	}

	// The interpreter ran as libmamba runs it.
	info, err := os.ReadFile(filepath.Join(logDir, "info"))
	must(t, err)
	lines := strings.Split(strings.TrimSpace(string(info)), "\n")
	// -I -S -Wi -u <script>, run from the script's private directory.
	if len(lines) != 7 || lines[0] != "argc=5" || lines[1] != "arg=-I" || lines[2] != "arg=-S" ||
		lines[3] != "arg=-Wi" || lines[4] != "arg=-u" || !strings.HasSuffix(lines[5], "/compile_pyc.py") ||
		lines[6] != "cwd="+filepath.Dir(strings.TrimPrefix(lines[5], "arg=")) ||
		!strings.HasPrefix(lines[5], "arg="+filepath.Join(prefix, "conda-meta", ".goomba-pyc-")) {
		t.Errorf("interpreter invocation:\n%s", info)
	}
	// The environment holds MAMBA_EXTRACT_THREADS and what the shell
	// itself exports, nothing inherited.
	env, err := os.ReadFile(filepath.Join(logDir, "env"))
	must(t, err)
	if !strings.Contains(string(env), "MAMBA_EXTRACT_THREADS='4'") && !strings.Contains(string(env), `MAMBA_EXTRACT_THREADS="4"`) {
		t.Errorf("MAMBA_EXTRACT_THREADS not passed:\n%s", env)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(env)), "\n") {
		name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(l, "declare -x "), "export "), "=")
		if !slices.Contains([]string{"MAMBA_EXTRACT_THREADS", "PWD", "OLDPWD", "SHLVL", "_"}, name) {
			t.Errorf("interpreter environment holds %s", l)
		}
	}
	script, err := os.ReadFile(filepath.Join(logDir, "script"))
	must(t, err)
	if string(script) != compilePycScript {
		t.Error("the interpreter did not get compile_pyc.py")
	}
	stdin, err := os.ReadFile(filepath.Join(logDir, "stdin"))
	must(t, err)
	for _, want := range []string{prefix + "/lib/python3.11/site-packages/pip/__init__.py\n", prefix + "/lib/python3.11/site-packages/wheel/__init__.py\n"} {
		if !strings.Contains(string(stdin), want) {
			t.Errorf("stdin lacks %q", want)
		}
	}

	// conda-meta: fields from index.json, requested_spec from the specs.
	pip := readCondaMetaJSON(t, prefix, "pip-24.0-pyh_0")
	for k, want := range map[string]any{
		"license": "MIT", "track_features": "pipfeature", "timestamp": float64(1700000000),
		"requested_spec": "pip", "channel": c.base, "noarch": "python", "subdir": "noarch",
	} {
		if pip[k] != want {
			t.Errorf("pip conda-meta %s = %v, want %v", k, pip[k], want)
		}
	}
	files := fmt.Sprint(pip["files"])
	for _, want := range []string{"lib/python3.11/site-packages/pip/__pycache__/__init__.cpython-311.pyc", "bin/pip3"} {
		if !strings.Contains(files, want) {
			t.Errorf("pip conda-meta files lack %s", want)
		}
	}
	pf := readCondaMetaJSON(t, prefix, "prefixed-1.0-h0_0")
	if pf["track_features"] != "feat_a,feat_b" || pf["timestamp"] != float64(1690000000) || pf["requested_spec"] != "" {
		t.Errorf("prefixed conda-meta: track_features %v timestamp %v requested_spec %v", pf["track_features"], pf["timestamp"], pf["requested_spec"])
	}
	py := readCondaMetaJSON(t, prefix, "python-3.11.9-h0_cpython")
	if py["requested_spec"] != "python=3.11" {
		t.Errorf("python requested_spec = %v", py["requested_spec"])
	}
	recs, err := link.ReadCondaMeta(prefix)
	must(t, err)
	if len(recs) != len(lf.Packages) {
		t.Errorf("%d conda-meta records, want %d", len(recs), len(lf.Packages))
	}
	for _, r := range recs {
		if !strings.HasPrefix(r.PackageDir, opt.PkgsDir) {
			t.Errorf("%s: extracted_package_dir %s is not in the cache", r.FileName, r.PackageDir)
		}
	}

	// History: one revision, packages in link order, the lockfile's specs.
	hist, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "history"))
	must(t, err)
	h := strings.Split(strings.TrimSpace(string(hist)), "\n")
	if !strings.HasPrefix(h[0], "==> ") || h[1] != "# cmd: goomba create -p "+prefix || h[2] != "# conda version: 3.8.0" {
		t.Errorf("history header:\n%s", hist)
	}
	if h[len(h)-1] != `# update specs: ["python=3.11", "pip"]` {
		t.Errorf("history specs line %q", h[len(h)-1])
	}
	var added []string
	for _, l := range h[3 : len(h)-1] {
		_, dist, ok := strings.Cut(l, "::")
		if !strings.HasPrefix(l, "+"+c.base+"/") || !ok {
			t.Errorf("history line %q", l)
		}
		added = append(added, dist)
	}
	idx := func(prefix string) int {
		return slices.IndexFunc(added, func(s string) bool { return strings.HasPrefix(s, prefix+"-") })
	}
	for _, o := range [][2]string{{"python", "pip"}, {"python", "wheel"}, {"packaging", "wheel"}, {"wheel", "pip"}, {"python", "packaging"}} {
		if idx(o[0]) < 0 || idx(o[0]) > idx(o[1]) {
			t.Errorf("history order %v: %s should come before %s", added, o[0], o[1])
		}
	}
	if hostPlatform(t) == "linux-64" && idx("libgomp") > idx("_openmp_mutex") {
		t.Errorf("libgomp must link before _openmp_mutex: %v", added)
	}

	// Events: every package fetched, then linked, then compiled, then done.
	kinds := map[EventKind]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	n := len(lf.Packages)
	if kinds[EventFetched] != n || kinds[EventLinked] != n || kinds[EventCompiling] != 1 || kinds[EventCompiled] != 1 || kinds[EventDone] != 1 {
		t.Errorf("events %v", kinds)
	}
	last := events[len(events)-1]
	if last.Kind != EventDone || last.Done != int64(n) {
		t.Errorf("last event %+v", last)
	}
	for _, e := range events {
		if e.Kind == EventCompiled && (e.Done != e.Total || e.Total == 0) {
			t.Errorf("compiled event %+v", e)
		}
	}
	if hostPlatform(t) == "linux-64" {
		// The symlink into another package resolves (libgomp linked first).
		if _, err := os.Stat(filepath.Join(prefix, "lib/libgomp.so.1")); err != nil {
			t.Error(err)
		}
	}

	// The lockfile round-trips through the prefix.
	back, err := lock.FromPrefix(prefix, lf.Platform)
	must(t, err)
	if !slices.Equal(back.Specs, []string{"pip", "python=3.11"}) {
		t.Errorf("FromPrefix specs %v", back.Specs)
	}
	if len(back.Packages) != n {
		t.Errorf("FromPrefix has %d packages", len(back.Packages))
	}
	for i, p := range back.Packages {
		q := lf.Packages[i]
		if p.URL != q.URL || p.SHA256 != q.SHA256 || !slices.Equal(p.Depends, q.Depends) {
			t.Errorf("FromPrefix package %s differs: %+v vs %+v", p.Name, p, q)
		}
	}
}

// TestInstallSecondTimeUsesCache installs twice from one cache: the second
// install downloads nothing.
func TestInstallSecondTimeUsesCache(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil, envPackages(t, t.TempDir())...)
	opt := c.options(t)
	opt.CompilePyc = false
	must(t, InstallLock(context.Background(), filepath.Join(t.TempDir(), "a"), lf, opt))
	hits := c.totalHits()
	if hits != len(lf.Packages) {
		t.Errorf("%d requests for %d packages", hits, len(lf.Packages))
	}
	b := filepath.Join(t.TempDir(), "b")
	must(t, InstallLock(context.Background(), b, lf, opt))
	if c.totalHits() != hits {
		t.Errorf("second install downloaded %d archives", c.totalHits()-hits)
	}
	// Without byte-compilation the .pyc files are recorded but absent.
	if _, err := os.Stat(filepath.Join(b, "lib/python3.11/site-packages/pip/__pycache__/__init__.cpython-311.pyc")); !os.IsNotExist(err) {
		t.Errorf("pyc exists without CompilePyc: %v", err)
	}
}

// TestInstallConcurrency checks that the result does not depend on the
// concurrency and that downloads respect the bound.
func TestInstallConcurrency(t *testing.T) {
	logDir := t.TempDir()
	c := newChannel(t)
	c.delay = 20 * 1e6 // 20ms per request, so downloads overlap
	lf := c.lockfile(t, []string{"pip"}, envPackages(t, logDir)...)
	var trees []map[string]string
	for _, conc := range []int{1, 3, 64, 1000} {
		opt := c.options(t)
		opt.Concurrency = conc
		c.mu.Lock()
		c.maxInflight = 0
		c.mu.Unlock()
		prefix := filepath.Join(t.TempDir(), "env-same-length")
		must(t, InstallLock(context.Background(), prefix, lf, opt))
		c.mu.Lock()
		got := c.maxInflight
		c.mu.Unlock()
		if got > min(conc, maxConcurrency) {
			t.Errorf("concurrency %d: %d downloads in flight", conc, got)
		}
		tree := treeOf(t, prefix)
		// Files that embed the prefix differ; compare everything else.
		for _, p := range []string{"etc/prefixed.conf", "lib/libprefixed.so", "conda-meta/history", "bin/pip", "bin/pip3", "bin/wheel"} {
			delete(tree, p)
		}
		for p := range tree {
			if strings.HasPrefix(p, "conda-meta/") {
				delete(tree, p)
			}
		}
		trees = append(trees, tree)
	}
	for i := 1; i < len(trees); i++ {
		if fmt.Sprint(trees[i]) != fmt.Sprint(trees[0]) {
			t.Errorf("tree %d differs from tree 0", i)
		}
	}
}

func TestInstallClobber(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []tfile
		aEPs     []string
		path     string
		noarchB  bool
		wantPkgs [2]string
	}{
		{"same file", []tfile{{path: "share/x", data: "a"}}, []tfile{{path: "share/x", data: "b"}}, nil, "share/x", false, [2]string{"aaa-1-0", "bbb-1-0"}},
		{"file vs directory", []tfile{{path: "share/x", data: "a"}}, []tfile{{path: "share/x/y", data: "b"}}, nil, "share/x", false, [2]string{"aaa-1-0", "bbb-1-0"}},
		{"directory vs file", []tfile{{path: "share/x/y", data: "a"}}, []tfile{{path: "share/x", data: "b"}}, nil, "share/x", false, [2]string{"aaa-1-0", "bbb-1-0"}},
		{"symlink vs file below", []tfile{{path: "lib64", link: "lib"}}, []tfile{{path: "lib64/libb.so", data: "b"}}, nil, "lib64", false, [2]string{"aaa-1-0", "bbb-1-0"}},
		{"same symlink", []tfile{{path: "lib/x.so", link: "x.so.1"}}, []tfile{{path: "lib/x.so", link: "x.so.1"}}, nil, "lib/x.so", false, [2]string{"aaa-1-0", "bbb-1-0"}},
		{"entry point vs file", []tfile{{path: "bin/tool", data: "a"}}, []tfile{{path: "site-packages/b.py", data: "b"}}, nil, "bin/tool", true, [2]string{"aaa-1-0", "bbb-1-0"}},
		{"pyc vs file", []tfile{{path: "lib/python3.11/site-packages/__pycache__/b.cpython-311.pyc", data: "a"}}, []tfile{{path: "site-packages/b.py", data: "b"}}, nil, "lib/python3.11/site-packages/__pycache__/b.cpython-311.pyc", true, [2]string{"aaa-1-0", "bbb-1-0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newChannel(t)
			sb := tspec{name: "bbb", version: "1", build: "0", files: tc.b}
			if tc.noarchB {
				sb.subdir, sb.noarch = "noarch", "python"
				sb.entryPoints = []string{"tool = b:main"}
			}
			pkgs := []testPkg{
				buildPkg(t, tspec{name: "aaa", version: "1", build: "0", files: tc.a}),
				buildPkg(t, sb),
			}
			if tc.noarchB {
				pkgs = append(pkgs, fakePython(t, t.TempDir(), 0))
			}
			lf := c.lockfile(t, nil, pkgs...)
			opt := c.options(t)
			prefix := filepath.Join(t.TempDir(), "env")
			err := InstallLock(context.Background(), prefix, lf, opt)
			var ce *ClobberError
			if !errors.As(err, &ce) || !errors.Is(err, ErrClobber) || !errors.Is(err, link.ErrExists) {
				t.Fatalf("err = %v, want a ClobberError", err)
			}
			if ce.Path != tc.path || ce.Packages != tc.wantPkgs {
				t.Errorf("clobber %q %v, want %q %v", ce.Path, ce.Packages, tc.path, tc.wantPkgs)
			}
			assertNoPrefix(t, prefix)
			// The cache is kept.
			entries, err := os.ReadDir(opt.PkgsDir)
			must(t, err)
			if len(entries) < 2 {
				t.Errorf("package cache emptied: %d entries", len(entries))
			}
		})
	}
}

// TestInstallSharedDirectories: packages may share directories, including
// explicit "directory" entries.
func TestInstallSharedDirectories(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil,
		buildPkg(t, tspec{name: "aaa", version: "1", build: "0", files: []tfile{{path: "share/x", dir: true}, {path: "share/x/a", data: "a"}}}),
		buildPkg(t, tspec{name: "bbb", version: "1", build: "0", files: []tfile{{path: "share/x", dir: true}, {path: "share/x/b", data: "b"}}}),
		buildPkg(t, tspec{name: "ccc", version: "1", build: "0", files: []tfile{{path: "share/x/c", data: "c"}}}),
	)
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, InstallLock(context.Background(), prefix, lf, c.options(t)))
	for _, p := range []string{"share/x/a", "share/x/b", "share/x/c"} {
		if _, err := os.Stat(filepath.Join(prefix, p)); err != nil {
			t.Error(err)
		}
	}
}

func TestInstallVirtualRefused(t *testing.T) {
	plat := hostPlatform(t)
	for _, tc := range []struct {
		dep, have string
	}{
		{"__glibc >=99", "__glibc 2.28 0"},
		{"__cuda >=12", ""},
		{"__unix", ""},
	} {
		t.Run(tc.dep, func(t *testing.T) {
			c := newChannel(t)
			lf := c.lockfile(t, nil, buildPkg(t, tspec{name: "needy", version: "1", build: "0", depends: []string{"zzz", tc.dep}}))
			opt := c.options(t)
			opt.Virtual = []*record.PackageRecord{{Name: "__glibc", Version: "2.28", Build: "0", Subdir: plat}}
			prefix := filepath.Join(t.TempDir(), "env")
			err := InstallLock(context.Background(), prefix, lf, opt)
			var ve *VirtualError
			if !errors.As(err, &ve) || !errors.Is(err, ErrVirtual) {
				t.Fatalf("err = %v, want a VirtualError", err)
			}
			if ve.Package != "needy-1-0" || ve.Spec != tc.dep || ve.Have != tc.have {
				t.Errorf("VirtualError %+v", ve)
			}
			if !strings.Contains(err.Error(), "needy-1-0") || !strings.Contains(err.Error(), tc.dep) {
				t.Errorf("message %q does not name the package and spec", err)
			}
			if n := c.totalHits(); n != 0 {
				t.Errorf("%d downloads before the virtual package check failed", n)
			}
			assertNoPrefix(t, prefix)
			if _, err := os.Stat(opt.PkgsDir); !os.IsNotExist(err) {
				t.Errorf("package cache created: %v", err)
			}
		})
	}
	// Satisfied requirements pass.
	c := newChannel(t)
	lf := c.lockfile(t, nil, buildPkg(t, tspec{name: "needy", version: "1", build: "0", depends: []string{"__glibc >=2.17,<3.0.a0"}}))
	opt := c.options(t)
	opt.Virtual = []*record.PackageRecord{{Name: "__glibc", Version: "2.28", Build: "0"}}
	must(t, InstallLock(context.Background(), filepath.Join(t.TempDir(), "env"), lf, opt))
}

func TestInstallInvalidInput(t *testing.T) {
	c := newChannel(t)
	lf := c.lockfile(t, nil, buildPkg(t, tspec{name: "aaa", version: "1", build: "0"}))
	base := c.options(t)
	other := *lf
	if lf.Platform == "linux-64" {
		other.Platform = "osx-arm64"
	} else {
		other.Platform = "linux-64"
	}
	other.Packages = nil
	badDep := *lf
	badDep.Packages = slices.Clone(lf.Packages)
	badDep.Packages[0].Depends = []string{"bad spec ((("}
	for _, tc := range []struct {
		name string
		lf   *lock.Lockfile
		opt  func(*InstallOptions)
		want error
	}{
		{"nil lockfile", nil, nil, lock.ErrInvalid},
		{"invalid lockfile", &lock.Lockfile{Version: 2}, nil, lock.ErrInvalid},
		{"other platform", &other, nil, ErrPlatform},
		{"no pkgs dir", lf, func(o *InstallOptions) { o.PkgsDir = "" }, ErrInvalidOptions},
		{"negative concurrency", lf, func(o *InstallOptions) { o.Concurrency = -1 }, ErrInvalidOptions},
		{"cmd line break", lf, func(o *InstallOptions) { o.Cmd = "a\nb" }, ErrInvalidOptions},
		{"cache inside the prefix", lf, func(o *InstallOptions) { o.PkgsDir = "" }, ErrInvalidOptions},
		{"non-virtual record", lf, func(o *InstallOptions) { o.Virtual = []*record.PackageRecord{{Name: "glibc", Version: "1"}} }, ErrInvalidOptions},
		{"nil virtual record", lf, func(o *InstallOptions) { o.Virtual = []*record.PackageRecord{nil} }, ErrInvalidOptions},
		{"duplicate virtual", lf, func(o *InstallOptions) {
			o.Virtual = []*record.PackageRecord{{Name: "__unix", Version: "0"}, {Name: "__unix", Version: "0"}}
		}, ErrInvalidOptions},
		{"bad virtual version", lf, func(o *InstallOptions) { o.Virtual = []*record.PackageRecord{{Name: "__unix", Version: "a..b"}} }, ErrInvalidOptions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := base
			if tc.opt != nil {
				tc.opt(&opt)
			}
			prefix := filepath.Join(t.TempDir(), "env")
			if tc.name == "cache inside the prefix" {
				opt.PkgsDir = filepath.Join(prefix, "pkgs")
			}
			err := InstallLock(context.Background(), prefix, tc.lf, opt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			assertNoPrefix(t, prefix)
		})
	}
	t.Run("unparsable depends", func(t *testing.T) {
		err := InstallLock(context.Background(), filepath.Join(t.TempDir(), "env"), &badDep, base)
		if err == nil || !strings.Contains(err.Error(), "aaa-1-0") || !strings.Contains(err.Error(), "bad spec") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("noarch python without python", func(t *testing.T) {
		c := newChannel(t)
		lf := c.lockfile(t, nil, buildPkg(t, tspec{name: "nopy", version: "1", build: "0", subdir: "noarch", noarch: "python",
			files: []tfile{{path: "site-packages/x.py", data: "x"}}}))
		prefix := filepath.Join(t.TempDir(), "env")
		err := InstallLock(context.Background(), prefix, lf, c.options(t))
		if !errors.Is(err, link.ErrNoPython) {
			t.Fatalf("err = %v", err)
		}
		assertNoPrefix(t, prefix)
	})
	t.Run("legacy info/files with an absolute path", func(t *testing.T) {
		c := newChannel(t)
		p := buildPkg(t, tspec{name: "legacy", version: "1", build: "0", files: []tfile{{path: "etc/x", data: "x"}},
			info: map[string]string{"info/paths.json": "", "info/files": "etc/x\n/etc/passwd\n"}})
		lf := c.lockfile(t, nil, p)
		prefix := filepath.Join(t.TempDir(), "env")
		err := InstallLock(context.Background(), prefix, lf, c.options(t))
		if !errors.Is(err, link.ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
		assertNoPrefix(t, prefix)
	})
	t.Run("digest mismatch", func(t *testing.T) {
		c := newChannel(t)
		lf := c.lockfile(t, nil, buildPkg(t, tspec{name: "aaa", version: "1", build: "0"}))
		c.files["/test-channel/"+lf.Packages[0].Subdir+"/aaa-1-0.conda"] = buildPkg(t, tspec{name: "aaa", version: "1", build: "0", files: []tfile{{path: "evil", data: "x"}}}).data
		lf.Packages[0].Size = 0
		prefix := filepath.Join(t.TempDir(), "env")
		err := InstallLock(context.Background(), prefix, lf, c.options(t))
		var ie *fetch.IntegrityError
		if !errors.As(err, &ie) {
			t.Fatalf("err = %v, want an IntegrityError", err)
		}
		assertNoPrefix(t, prefix)
	})
}
