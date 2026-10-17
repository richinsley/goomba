//go:build linux

package virtual

import (
	"context"
	"os/exec"
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
		if v, ok := glibcFromGetconf(context.Background()); !ok || v != want {
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
