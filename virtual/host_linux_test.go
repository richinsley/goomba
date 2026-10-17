//go:build linux

package virtual

import (
	"context"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestDetectHost runs the real detection on this Linux machine and compares
// __glibc with `getconf GNU_LIBC_VERSION`. On a musl host getconf fails and
// no __glibc may be reported.
func TestDetectHost(t *testing.T) {
	recs, err := Detect(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("detected %q", triples(recs))
	host := HostPlatform()
	for _, r := range recs {
		if r.Channel != "@" || r.Subdir != host || r.URL != "" || r.SHA256 != "" {
			t.Errorf("record %+v", *r)
		}
	}
	unix := find(recs, "__unix")
	if unix == nil || unix.Version != "0" || unix.Build != "0" {
		t.Errorf("__unix = %v", unix)
	}
	// Like libmamba's test_os_linux: the kernel version is numeric.
	linux := find(recs, "__linux")
	if linux == nil || !regexp.MustCompile(`^\d+\.\d+(\.\d+){0,2}$`).MatchString(linux.Version) || linux.Version == "0" {
		t.Errorf("__linux = %v", linux)
	}
	arch := find(recs, "__archspec")
	if arch == nil || arch.Version != "1" {
		t.Fatalf("__archspec = %v", arch)
	}
	switch runtime.GOARCH {
	case "amd64":
		if !strings.HasPrefix(arch.Build, "x86_64") {
			t.Errorf("__archspec build = %q, want x86_64*", arch.Build)
		}
	case "arm64":
		if arch.Build != "aarch64" {
			t.Errorf("__archspec build = %q, want aarch64", arch.Build)
		}
	}

	out, gerr := exec.Command("getconf", "GNU_LIBC_VERSION").Output()
	want := ""
	if gerr == nil {
		want = strings.TrimPrefix(strings.TrimSpace(string(out)), "glibc ")
	}
	glibc := find(recs, "__glibc")
	switch {
	case want == "" && glibc != nil:
		t.Errorf("__glibc = %v on a host without glibc", glibc)
	case want != "" && (glibc == nil || glibc.Version != want):
		t.Errorf("__glibc = %v, want version %q from getconf", glibc, want)
	}

	// The ELF reader alone agrees with getconf too.
	if want != "" {
		if v, ok := glibcFromSystemLibc(context.Background(), runtime.GOARCH); ok && v != want {
			t.Errorf("glibcFromSystemLibc = %q, want %q", v, want)
		}
		if v, ok := glibcFromGetconf(context.Background(), osSearchFS{}, getconfPaths(), runCapped); !ok || v != want {
			t.Errorf("glibcFromGetconf = %q, %v; want %q", v, ok, want)
		}
	}

	// Overrides apply on the real path too.
	recs, err = Detect(context.Background(), Options{Platform: host, Overrides: map[string]string{"glibc": "2.17", "cuda": "12.4", "linux": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if g := find(recs, "__glibc"); g == nil || g.Version != "2.17" {
		t.Errorf("overridden __glibc = %v", g)
	}
	if c := find(recs, "__cuda"); c == nil || c.Version != "12.4" {
		t.Errorf("overridden __cuda = %v", c)
	}
	if l := find(recs, "__linux"); l != nil {
		t.Errorf("removed __linux = %v", l)
	}
}

func TestLibcCandidates(t *testing.T) {
	c := libcCandidates("amd64")
	if len(c) < 6 || c[0] != "/lib/x86_64-linux-gnu/libc.so.6" {
		t.Errorf("libcCandidates(amd64) = %q", c)
	}
	if _, ok := glibcFromSystemLibc(context.Background(), "mips"); ok {
		t.Error("glibcFromSystemLibc(mips) found a libc")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := glibcFromSystemLibc(ctx, runtime.GOARCH); ok {
		t.Error("glibcFromSystemLibc ignored a cancelled context")
	}
}

// getconf is found only at fixed locations and run only when the same trust
// policy as nvidia-smi accepts it.
func TestGetconfPaths(t *testing.T) {
	want := []string{"/usr/bin/getconf", "/bin/getconf", "/run/current-system/sw/bin/getconf"}
	if got := getconfPaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("getconfPaths = %q, want %q", got, want)
	}
	if _, ok := glibcFromGetconf(context.Background(), osSearchFS{}, getconfPaths(), runCapped); !ok {
		for _, p := range getconfPaths() {
			if real, ok := trustedUnixExecutable(osSearchFS{}, p); ok {
				t.Logf("trusted getconf %s -> %s gave no glibc version (musl?)", p, real)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v, ok := glibcFromGetconf(ctx, osSearchFS{}, getconfPaths(), runCapped); ok {
		t.Errorf("glibcFromGetconf ran with a cancelled context: %q", v)
	}
}

// fakeRunner answers runs by the path executed and records each spec.
type fakeRunner struct {
	results map[string]runResult // path -> result; missing = not started
	ok      map[string]bool
	specs   []runSpec
}

func (r *fakeRunner) run(_ context.Context, s runSpec) (runResult, bool) {
	r.specs = append(r.specs, s)
	return r.results[s.path], r.ok[s.path]
}

func TestGlibcFromGetconfSearch(t *testing.T) {
	paths := getconfPaths()
	done := func(out string, code int) runResult {
		return runResult{stdout: []byte(out), exitCode: code, started: true}
	}
	cases := []struct {
		name      string
		setup     func(f *fakeFS)
		results   map[string]runResult
		ok        map[string]bool
		want      string // "" = none
		wantRuns  []string
		wantNames []string
	}{
		{"distribution getconf", func(f *fakeFS) { f.addUnix("/usr/bin/getconf", rootExe()) },
			map[string]runResult{"/usr/bin/getconf": done("glibc 2.35\n", 0)}, map[string]bool{"/usr/bin/getconf": true},
			"2.35", []string{"/usr/bin/getconf"}, []string{"/usr/bin/getconf"}},
		{"merged /usr runs one binary once", func(f *fakeFS) {
			f.addUnix("/usr/bin/getconf", rootExe())
			f.links["/bin"] = "usr/bin"
		}, nil, nil, "", []string{"/usr/bin/getconf"}, []string{"/usr/bin/getconf"}},
		{"a getconf that could not start gives way", func(f *fakeFS) {
			f.addUnix("/usr/bin/getconf", rootExe())
			f.addUnix("/bin/getconf", rootExe())
		}, map[string]runResult{"/bin/getconf": done("glibc 2.28", 0)}, map[string]bool{"/bin/getconf": true},
			"2.28", []string{"/usr/bin/getconf", "/bin/getconf"}, []string{"/usr/bin/getconf", "/bin/getconf"}},
		{"a getconf that started and failed ends the search", func(f *fakeFS) {
			f.addUnix("/usr/bin/getconf", rootExe())
			f.addUnix("/bin/getconf", rootExe())
		}, map[string]runResult{"/usr/bin/getconf": {started: true}, "/bin/getconf": done("glibc 2.28", 0)},
			map[string]bool{"/bin/getconf": true}, "", []string{"/usr/bin/getconf"}, []string{"/usr/bin/getconf"}},
		{"musl", func(f *fakeFS) { f.addUnix("/usr/bin/getconf", rootExe()) },
			map[string]runResult{"/usr/bin/getconf": done("", 1)}, map[string]bool{"/usr/bin/getconf": true},
			"", []string{"/usr/bin/getconf"}, []string{"/usr/bin/getconf"}},
		{"unparsable output", func(f *fakeFS) { f.addUnix("/usr/bin/getconf", rootExe()) },
			map[string]runResult{"/usr/bin/getconf": done("weird\n", 0)}, map[string]bool{"/usr/bin/getconf": true},
			"", []string{"/usr/bin/getconf"}, []string{"/usr/bin/getconf"}},
		{"an untrusted getconf is never run", func(f *fakeFS) {
			f.addUnix("/usr/bin/getconf", userFile(1000))
			f.addUnix("/bin/getconf", rootExe())
		}, map[string]runResult{"/usr/bin/getconf": done("glibc 9.9", 0), "/bin/getconf": done("glibc 2.28", 0)},
			map[string]bool{"/usr/bin/getconf": true, "/bin/getconf": true}, "2.28", []string{"/bin/getconf"}, []string{"/bin/getconf"}},
		{"NixOS multi-call binary keeps its name", func(f *fakeFS) {
			f.addUnix("/nix/store/aaa-glibc-bin/bin/getconf", fileFacts{mode: 0o555})
			f.addUnix("/run/current-system/sw/bin/placeholder", rootExe())
			f.links["/run/current-system/sw/bin/getconf"] = "/nix/store/aaa-glibc-bin/bin/getconf"
		}, map[string]runResult{"/nix/store/aaa-glibc-bin/bin/getconf": done("glibc 2.40", 0)},
			map[string]bool{"/nix/store/aaa-glibc-bin/bin/getconf": true},
			"2.40", []string{"/nix/store/aaa-glibc-bin/bin/getconf"}, []string{"/run/current-system/sw/bin/getconf"}},
		{"nothing installed", func(f *fakeFS) { f.addUnix("/usr/bin/env", rootExe()) }, nil, nil, "", nil, nil},
	}
	for _, c := range cases {
		f := newFakeFS()
		c.setup(f)
		r := &fakeRunner{results: c.results, ok: c.ok}
		v, ok := glibcFromGetconf(context.Background(), f, paths, r.run)
		if v != c.want || ok != (c.want != "") {
			t.Errorf("%s: %q, %v; want %q", c.name, v, ok, c.want)
		}
		var runs, names []string
		for _, s := range r.specs {
			runs, names = append(runs, s.path), append(names, s.argv0)
			if !reflect.DeepEqual(s.args, []string{"GNU_LIBC_VERSION"}) || s.env == nil || len(s.env) != 0 ||
				s.dir != "/" || s.timeout != getconfTimeout || s.max != maxGetconfOutput {
				t.Errorf("%s: spec %+v", c.name, s)
			}
		}
		if !reflect.DeepEqual(runs, c.wantRuns) || !reflect.DeepEqual(names, c.wantNames) {
			t.Errorf("%s: ran %q named %q; want %q named %q", c.name, runs, names, c.wantRuns, c.wantNames)
		}
	}
}

// glibcFromGetconf with the real runner: the trusted location (here a fake
// file system pointing it at a real script) is what runs.
func TestGlibcFromGetconfRuns(t *testing.T) {
	dir := scriptDir(t)
	good := script(t, dir, "getconf", `[ "$1" = GNU_LIBC_VERSION ] && printf 'glibc 2.99\n'`)
	f := newFakeFS()
	f.addUnix(good, rootExe())
	f.addUnix("/usr/bin/placeholder", rootExe())
	f.links["/usr/bin/getconf"] = good
	if v, ok := glibcFromGetconf(context.Background(), f, getconfPaths(), runCapped); !ok || v != "2.99" {
		t.Errorf("glibcFromGetconf = %q, %v; want 2.99", v, ok)
	}
	// The same script is not run when the fake says it is not root's.
	f.files[good] = userFile(1000)
	if v, ok := glibcFromGetconf(context.Background(), f, getconfPaths(), runCapped); ok {
		t.Errorf("ran an untrusted getconf: %q", v)
	}
}
