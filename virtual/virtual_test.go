package virtual

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// fakeProbe returns fixed host facts.
type fakeProbe struct {
	plat    string
	release string // "" = uname failed
	glibc   string // "" = no glibc
	osx     string
	win     string
	arch    string // "" = derive from plat like libmamba
}

func (f fakeProbe) platform() string { return f.plat }
func (f fakeProbe) linuxRelease() (string, bool) {
	return f.release, f.release != ""
}
func (f fakeProbe) glibcVersion(context.Context) (string, bool) { return f.glibc, f.glibc != "" }
func (f fakeProbe) osxVersion() (string, bool)                  { return f.osx, f.osx != "" }
func (f fakeProbe) winVersion() (string, bool)                  { return f.win, f.win != "" }
func (f fakeProbe) archspec() string {
	if f.arch != "" {
		return f.arch
	}
	_, arch, _ := splitPlatform(f.plat)
	return archspecForPlatform(arch)
}

// triples renders records as sorted "name version build" strings.
func triples(recs []*record.PackageRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Name + " " + r.Version + " " + r.Build
	}
	sort.Strings(out)
	return out
}

func find(recs []*record.PackageRecord, name string) *record.PackageRecord {
	for _, r := range recs {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func mustDetect(t *testing.T, p probe, ov map[string]string) []*record.PackageRecord {
	t.Helper()
	recs, err := detect(context.Background(), Options{Overrides: ov}, p)
	if err != nil {
		t.Fatalf("detect(%q, %v): %v", p.platform(), ov, err)
	}
	return recs
}

// Cases from libmamba tests/src/core/test_virtual_packages.cpp
// (TEST_CASE "make_virtual_package"). libmamba also sets md5 to a dummy value
// and filename to the name; goomba's virtual records carry no hashes and no
// file name (see the package documentation).
func TestNewRecord(t *testing.T) {
	r := newRecord("linux-64", "test", "0.1.5", "abcd")
	want := record.PackageRecord{Name: "test", Version: "0.1.5", Build: "abcd", Channel: "@", Subdir: "linux-64"}
	if !reflect.DeepEqual(*r, want) {
		t.Fatalf("newRecord = %+v, want %+v", *r, want)
	}
	if r.BuildNumber != 0 || r.URL != "" || r.SHA256 != "" || r.MD5 != "" {
		t.Fatalf("virtual record has build number, URL or hashes: %+v", *r)
	}
}

// Cases from libmamba tests/src/core/test_virtual_packages.cpp
// (TEST_CASE "dist_packages"), with CONDA_OVERRIDE_* variables turned into
// Overrides because goomba never reads the environment. goomba sorts by
// name; libmamba's order is __unix, OS package, __glibc, __archspec.
func TestDistPackages(t *testing.T) {
	cases := []struct {
		probe fakeProbe
		ov    map[string]string
		want  []string
	}{
		{fakeProbe{plat: "osx-arm"}, map[string]string{"osx": "12.1"},
			[]string{"__archspec 1 arm", "__osx 12.1 0", "__unix 0 0"}},
		{fakeProbe{plat: "linux-32"}, map[string]string{"linux": "5.7", "glibc": "2.15"},
			[]string{"__archspec 1 x86", "__glibc 2.15 0", "__linux 5.7 0", "__unix 0 0"}},
		{fakeProbe{plat: "lin-850"}, nil,
			[]string{"__archspec 1 850"}},
		{fakeProbe{plat: "emscripten-wasm32"}, nil,
			[]string{"__archspec 1 wasm32", "__unix 0 0"}},
	}
	for _, c := range cases {
		if got := triples(mustDetect(t, c.probe, c.ov)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("detect(%q, %v) = %q, want %q", c.probe.plat, c.ov, got, c.want)
		}
	}
	// libmamba's dist_packages("linux") returns no packages; goomba's
	// Defaults does the same and Detect refuses the malformed platform.
	if got := Defaults("linux"); got != nil {
		t.Errorf("Defaults(%q) = %q, want nil", "linux", triples(got))
	}
	if _, err := detect(context.Background(), Options{}, fakeProbe{plat: "linux"}); !errors.Is(err, ErrInvalidPlatform) {
		t.Errorf("detect(%q) error = %v, want ErrInvalidPlatform", "linux", err)
	}
	if got := triples(Defaults("lin-850")); !reflect.DeepEqual(got, []string{"__archspec 1 850"}) {
		t.Errorf("Defaults(%q) = %q", "lin-850", got)
	}
}

// Cases from libmamba tests/src/core/test_virtual_packages.cpp (TEST_CASEs
// "get_virtual_packages", "override_virtual_packages_from_config",
// "override_virtual_packages_dunder_prefix" and the conda_parity_* cases
// that use only the config map). The env-variable cases
// (override_virtual_packages_env_over_config,
// conda_parity_env_override_takes_precedence_over_config,
// conda_parity_empty_env_overrides_config) are skipped: goomba never reads
// the environment. conda_parity_linux_empty_override_keeps_package is
// inverted: in goomba an empty override removes any package, __linux
// included (see TestEmptyOverrideRemoves).
func TestGetVirtualPackages(t *testing.T) {
	linux := fakeProbe{plat: "linux-64", release: "6.8.0-45-generic", glibc: "2.39", arch: "x86_64_v2"}
	osx := fakeProbe{plat: "osx-64", osx: "14.5", arch: "x86_64_v2"}

	base := map[string]string{"linux": "5.15", "glibc": "2.35", "archspec": "x86_64_v3", "cuda": ""}
	if got := mustDetect(t, linux, base); len(got) != 4 {
		t.Fatalf("baseline = %q, want 4 packages", triples(got))
	}
	withCuda := map[string]string{"linux": "5.15", "glibc": "2.35", "archspec": "x86_64_v3", "cuda": "9.0"}
	got := mustDetect(t, linux, withCuda)
	if len(got) != 5 || find(got, "__cuda") == nil || find(got, "__cuda").Version != "9.0" {
		t.Fatalf("with cuda = %q, want 5 packages with __cuda 9.0", triples(got))
	}

	config := map[string]string{"cuda": "13.1", "glibc": "2.15", "linux": "5.7", "osx": "12.1", "archspec": "x86_64_v4"}
	if got, want := triples(mustDetect(t, linux, config)), []string{
		"__archspec 1 x86_64_v4", "__cuda 13.1 0", "__glibc 2.15 0", "__linux 5.7 0", "__unix 0 0",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("linux-64 config overrides = %q, want %q", got, want)
	}
	if got, want := triples(mustDetect(t, osx, config)), []string{
		"__archspec 1 x86_64_v4", "__cuda 13.1 0", "__osx 12.1 0", "__unix 0 0",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("osx-64 config overrides = %q, want %q", got, want)
	}

	dunder := mustDetect(t, linux, map[string]string{"__cuda": "11.8", "__archspec": "x86_64_v2"})
	if r := find(dunder, "__cuda"); r == nil || r.Version != "11.8" {
		t.Errorf("dunder __cuda = %v", r)
	}
	if r := find(dunder, "__archspec"); r == nil || r.Build != "x86_64_v2" || r.Version != "1" {
		t.Errorf("dunder __archspec = %v", r)
	}

	if r := find(mustDetect(t, linux, map[string]string{"cuda": "overridden"}), "__cuda"); r == nil || r.Version != "overridden" {
		t.Errorf("conda parity cuda=overridden: %v", r)
	}
	for _, v := range []string{"4.5", ""} {
		r := find(mustDetect(t, linux, map[string]string{"cuda": v}), "__cuda")
		if (r != nil) != (v != "") || (r != nil && r.Version != v) {
			t.Errorf("conda parity cuda=%q: %v", v, r)
		}
	}
	for _, v := range []string{"1.0", ""} {
		r := find(mustDetect(t, linux, map[string]string{"glibc": v}), "__glibc")
		if (r != nil) != (v != "") || (r != nil && r.Version != v) {
			t.Errorf("conda parity glibc=%q: %v", v, r)
		}
	}
	if r := find(mustDetect(t, linux, map[string]string{"__cuda": "11.8"}), "__cuda"); r == nil || r.Version != "11.8" {
		t.Errorf("conda parity dunder cuda: %v", r)
	}
	if r := find(mustDetect(t, linux, map[string]string{"cuda": ""}), "__cuda"); r != nil {
		t.Errorf("conda parity empty cuda: %v", r)
	}
}

// Override cases from rattler crates/rattler_virtual_packages/src/lib.rs
// (parse_libc, parse_cuda, parse_osx with Override::String, and
// archspec_override_rejects_unknown_names). rattler's archspec database is
// not ported, so unknown but well-formed names such as "nonsense" are
// accepted; the dashed spelling is still rejected with rattler's hint.
func TestRattlerOverrides(t *testing.T) {
	linux := fakeProbe{plat: "linux-64", release: "6.8.0", glibc: "2.39", arch: "x86_64"}
	if r := find(mustDetect(t, linux, map[string]string{"glibc": "1.23"}), "__glibc"); r == nil || r.Version != "1.23" || r.Build != "0" {
		t.Errorf("libc 1.23: %v", r)
	}
	if r := find(mustDetect(t, linux, map[string]string{"glibc": ""}), "__glibc"); r != nil {
		t.Errorf("libc empty: %v", r)
	}
	if r := find(mustDetect(t, linux, map[string]string{"cuda": "1.234"}), "__cuda"); r == nil || r.Version != "1.234" {
		t.Errorf("cuda 1.234: %v", r)
	}
	osx := fakeProbe{plat: "osx-arm64", osx: "14.5"}
	if r := find(mustDetect(t, osx, map[string]string{"osx": "2.345"}), "__osx"); r == nil || r.Version != "2.345" {
		t.Errorf("osx 2.345: %v", r)
	}
	if r := find(mustDetect(t, linux, map[string]string{"archspec": "skylake"}), "__archspec"); r == nil || r.Build != "skylake" {
		t.Errorf("archspec skylake: %v", r)
	}
	if r := find(mustDetect(t, linux, map[string]string{"archspec": "0"}), "__archspec"); r == nil || r.Build != "0" {
		t.Errorf("archspec 0: %v", r)
	}
	_, err := detect(context.Background(), Options{Overrides: map[string]string{"archspec": "x86-64-v3"}}, linux)
	if !errors.Is(err, ErrInvalidOverride) || !strings.Contains(err.Error(), `did you mean "x86_64_v3"`) {
		t.Errorf("archspec x86-64-v3: err = %v", err)
	}
}

func TestOverridesAddChangeRemove(t *testing.T) {
	linux := fakeProbe{plat: "linux-64", release: "6.8.0-45-generic", glibc: "2.35", arch: "x86_64_v3"}
	plain := triples(mustDetect(t, linux, nil))
	if want := []string{"__archspec 1 x86_64_v3", "__glibc 2.35 0", "__linux 6.8.0 0", "__unix 0 0"}; !reflect.DeepEqual(plain, want) {
		t.Fatalf("no overrides = %q, want %q", plain, want)
	}
	cases := []struct {
		name string
		ov   map[string]string
		want []string
	}{
		{"add cuda", map[string]string{"cuda": "12.4"},
			[]string{"__archspec 1 x86_64_v3", "__cuda 12.4 0", "__glibc 2.35 0", "__linux 6.8.0 0", "__unix 0 0"}},
		{"add other", map[string]string{"__cuda_arch": "8.6"},
			[]string{"__archspec 1 x86_64_v3", "__cuda_arch 8.6 0", "__glibc 2.35 0", "__linux 6.8.0 0", "__unix 0 0"}},
		{"change glibc", map[string]string{"__glibc": "2.17"},
			[]string{"__archspec 1 x86_64_v3", "__glibc 2.17 0", "__linux 6.8.0 0", "__unix 0 0"}},
		{"change archspec", map[string]string{"ARCHSPEC": "x86_64"},
			[]string{"__archspec 1 x86_64", "__glibc 2.35 0", "__linux 6.8.0 0", "__unix 0 0"}},
		{"remove glibc and unix", map[string]string{"glibc": "", "__unix": ""},
			[]string{"__archspec 1 x86_64_v3", "__linux 6.8.0 0"}},
		{"remove absent", map[string]string{"cuda": ""}, plain},
		{"ignore other os", map[string]string{"osx": "12.0", "win": "10.0"}, plain},
		{"same value twice", map[string]string{"glibc": "2.17", "__GLIBC": "2.17"},
			[]string{"__archspec 1 x86_64_v3", "__glibc 2.17 0", "__linux 6.8.0 0", "__unix 0 0"}},
	}
	for _, c := range cases {
		if got := triples(mustDetect(t, linux, c.ov)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// A glibc override adds __glibc on a musl host.
	musl := fakeProbe{plat: "linux-aarch64", release: "6.1.0"}
	if got, want := triples(mustDetect(t, musl, nil)), []string{"__archspec 1 aarch64", "__linux 6.1.0 0", "__unix 0 0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("musl = %q, want %q", got, want)
	}
	if r := find(mustDetect(t, musl, map[string]string{"glibc": "2.17"}), "__glibc"); r == nil || r.Version != "2.17" {
		t.Errorf("musl with glibc override: %v", r)
	}
	// Windows ignores __unix overrides and keeps __win.
	win := fakeProbe{plat: "win-64", win: "10.0.22631", arch: "x86_64"}
	if got, want := triples(mustDetect(t, win, map[string]string{"unix": "0", "linux": "1"})), []string{"__archspec 1 x86_64", "__win 10.0.22631 0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("win = %q, want %q", got, want)
	}
}

// TestEmptyOverrideRemoves pins the brief's rule that "" removes a package,
// which differs from conda for __linux (conda keeps __linux 0).
func TestEmptyOverrideRemoves(t *testing.T) {
	linux := fakeProbe{plat: "linux-64", release: "6.8.0", glibc: "2.35"}
	for _, name := range []string{"__linux", "__glibc", "__unix", "__archspec"} {
		if r := find(mustDetect(t, linux, map[string]string{name: ""}), name); r != nil {
			t.Errorf("override %s=\"\" kept %v", name, r)
		}
	}
}

func TestInvalidOverrides(t *testing.T) {
	linux := fakeProbe{plat: "linux-64", release: "6.8.0", glibc: "2.35"}
	bad := []map[string]string{
		{"glibc": "2..17"},
		{"glibc": "2.17 "},
		{"glibc": ">=2.17"},
		{"glibc": "2.17-1"},
		{"glibc": "1!2!3"},
		{"glibc": "a!2"},
		{"glibc": "1+2+3"},
		{"glibc": "2."},
		{"glibc": ".2"},
		{"glibc": strings.Repeat("1", 65)},
		{"": "1"},
		{"__": "1"},
		{"_glibc": "1"},
		{"gl-ibc": "1"},
		{"gl ibc": "1"},
		{"___glibc": "1"},
		{"archspec": "x86 64"},
		{"archspec": "x86*"},
		{"glibc": "2.17", "__glibc": "2.28"},
		{"GLIBC": "2.17", "glibc": ""},
	}
	for _, ov := range bad {
		_, err := detect(context.Background(), Options{Overrides: ov}, linux)
		if !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("overrides %q: err = %v, want ErrInvalidOverride", ov, err)
		}
		if _, err := ApplyOverrides("linux-64", nil, ov); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("ApplyOverrides %q: err = %v, want ErrInvalidOverride", ov, err)
		}
	}
	// The error does not depend on map iteration order.
	ov := map[string]string{"a b": "1", "c d": "1", "e f": "1", "g h": "1"}
	_, first := detect(context.Background(), Options{Overrides: ov}, linux)
	for i := 0; i < 20; i++ {
		if _, err := detect(context.Background(), Options{Overrides: ov}, linux); err.Error() != first.Error() {
			t.Fatalf("error changed between runs: %v vs %v", err, first)
		}
	}
}

func TestDetectErrors(t *testing.T) {
	linux := fakeProbe{plat: "linux-64", release: "6.8.0", glibc: "2.35"}
	if _, err := detect(context.Background(), Options{Platform: "osx-arm64"}, linux); !errors.Is(err, ErrNotHost) {
		t.Errorf("other platform: err = %v, want ErrNotHost", err)
	}
	if _, err := detect(context.Background(), Options{}, fakeProbe{}); !errors.Is(err, ErrUnknownHost) {
		t.Errorf("unknown host: err = %v, want ErrUnknownHost", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := detect(ctx, Options{}, linux); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v, want context.Canceled", err)
	}
	if _, err := Detect(ctx, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Detect cancelled: err = %v, want context.Canceled", err)
	}
	if _, err := Detect(context.Background(), Options{Platform: "zos-z"}); !errors.Is(err, ErrNotHost) && !errors.Is(err, ErrUnknownHost) {
		t.Errorf("Detect zos-z: err = %v, want ErrNotHost", err)
	}
}

func TestDetectFallbacks(t *testing.T) {
	cases := []struct {
		probe fakeProbe
		want  []string
	}{
		// uname failed or unparsable: __linux 0, as libmamba does.
		{fakeProbe{plat: "linux-64", arch: "x86_64"}, []string{"__archspec 1 x86_64", "__linux 0 0", "__unix 0 0"}},
		{fakeProbe{plat: "linux-64", release: "weird", arch: "x86_64"}, []string{"__archspec 1 x86_64", "__linux 0 0", "__unix 0 0"}},
		// A glibc version that is not a version is dropped.
		{fakeProbe{plat: "linux-64", release: "5.4", glibc: "2.x y", arch: "x86_64"}, []string{"__archspec 1 x86_64", "__linux 5.4 0", "__unix 0 0"}},
		// macOS and Windows versions default to 0.
		{fakeProbe{plat: "osx-arm64"}, []string{"__archspec 1 arm64", "__osx 0 0", "__unix 0 0"}},
		{fakeProbe{plat: "osx-arm64", osx: "14.5"}, []string{"__archspec 1 arm64", "__osx 14.5 0", "__unix 0 0"}},
		{fakeProbe{plat: "win-arm64"}, []string{"__archspec 1 arm64", "__win 0 0"}},
		{fakeProbe{plat: "win-64", win: "10.0.26100", arch: "bad arch"}, []string{"__archspec 1 0", "__win 10.0.26100 0"}},
	}
	for _, c := range cases {
		if got := triples(mustDetect(t, c.probe, nil)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.probe, got, c.want)
		}
	}
}

func TestDefaults(t *testing.T) {
	cases := []struct {
		platform string
		want     []string
	}{
		{"linux-64", []string{"__archspec 1 x86_64", "__glibc 2.28 0", "__linux 4.18 0", "__unix 0 0"}},
		{"linux-32", []string{"__archspec 1 x86", "__glibc 2.28 0", "__linux 4.18 0", "__unix 0 0"}},
		{"linux-aarch64", []string{"__archspec 1 aarch64", "__glibc 2.28 0", "__linux 4.18 0", "__unix 0 0"}},
		{"linux-ppc64le", []string{"__archspec 1 ppc64le", "__glibc 2.28 0", "__linux 4.18 0", "__unix 0 0"}},
		{"linux-riscv64", []string{"__archspec 1 riscv64", "__glibc 2.39 0", "__linux 4.18 0", "__unix 0 0"}},
		{"osx-64", []string{"__archspec 1 x86_64", "__osx 13.0 0", "__unix 0 0"}},
		{"osx-arm64", []string{"__archspec 1 arm64", "__osx 13.0 0", "__unix 0 0"}},
		{"win-64", []string{"__archspec 1 x86_64", "__win 10.0 0"}},
		{"win-32", []string{"__archspec 1 x86", "__win 10.0 0"}},
		{"win-arm64", []string{"__archspec 1 arm64", "__win 10.0 0"}},
		{"freebsd-64", []string{"__archspec 1 x86_64", "__unix 0 0"}},
		{"emscripten-wasm32", []string{"__archspec 1 wasm32", "__unix 0 0"}},
		{"wasi-wasm32", []string{"__archspec 1 wasm32"}},
		{"zos-z", []string{"__archspec 1 z"}},
		{"noarch", nil},
		{"", nil},
		{"linux", nil},
		{"-64", nil},
		{"linux-", nil},
		{"Linux-64", nil},
		{"linux-6 4", nil},
		{"linux-64-", nil},
	}
	for _, c := range cases {
		recs := Defaults(c.platform)
		got := triples(recs)
		if c.want == nil {
			if recs != nil {
				t.Errorf("Defaults(%q) = %q, want nil", c.platform, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Defaults(%q) = %q, want %q", c.platform, got, c.want)
		}
		for i, r := range recs {
			if r.Channel != "@" || r.Subdir != c.platform || r.URL != "" || r.SHA256 != "" || r.MD5 != "" || r.BuildNumber != 0 {
				t.Errorf("Defaults(%q)[%d] = %+v", c.platform, i, *r)
			}
			if i > 0 && recs[i-1].Name >= r.Name {
				t.Errorf("Defaults(%q) not sorted by name", c.platform)
			}
		}
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/lib.rs
// (test_cross_platform_virtual_packages, baseline_only_fills_slots_the_platform_carries,
// baseline_assumes_no_gpu, baseline_uses_the_documented_default_versions,
// baseline_assumes_a_newer_glibc_on_riscv64), applied to Defaults.
func TestRattlerBaseline(t *testing.T) {
	has := func(platform, name string) bool { return find(Defaults(platform), name) != nil }
	for _, n := range []string{"__linux", "__glibc", "__archspec", "__unix"} {
		if !has("linux-64", n) {
			t.Errorf("linux-64 lacks %s", n)
		}
	}
	for _, n := range []string{"__osx", "__archspec", "__unix"} {
		if !has("osx-arm64", n) {
			t.Errorf("osx-arm64 lacks %s", n)
		}
	}
	if !has("win-64", "__win") || has("win-64", "__unix") || !has("win-64", "__archspec") {
		t.Errorf("win-64 = %q", triples(Defaults("win-64")))
	}
	for _, n := range []string{"__win", "__osx"} {
		if has("linux-64", n) {
			t.Errorf("linux-64 has %s", n)
		}
	}
	for _, n := range []string{"__linux", "__glibc", "__osx"} {
		if has("win-64", n) {
			t.Errorf("win-64 has %s", n)
		}
	}
	for _, n := range []string{"__linux", "__glibc", "__win"} {
		if has("osx-arm64", n) {
			t.Errorf("osx-arm64 has %s", n)
		}
	}
	for _, p := range []string{"linux-64", "win-64", "osx-arm64"} {
		for _, r := range Defaults(p) {
			if strings.HasPrefix(r.Name, "__cuda") || strings.HasPrefix(r.Name, "__amdgpu") {
				t.Errorf("%s baseline has GPU package %s", p, r.Name)
			}
		}
	}
	if v := find(Defaults("linux-64"), "__linux").Version; v != "4.18" {
		t.Errorf("linux-64 __linux = %s", v)
	}
	if v := find(Defaults("linux-64"), "__glibc").Version; v != "2.28" {
		t.Errorf("linux-64 __glibc = %s", v)
	}
	if v := find(Defaults("linux-riscv64"), "__glibc").Version; v != "2.39" {
		t.Errorf("linux-riscv64 __glibc = %s", v)
	}
	if v := find(Defaults("osx-arm64"), "__osx").Version; v != "13.0" {
		t.Errorf("osx-arm64 __osx = %s", v)
	}
	if v := find(Defaults("win-64"), "__win").Version; v != "10.0" {
		t.Errorf("win-64 __win = %s", v)
	}
}

func TestApplyOverridesAndToOverrides(t *testing.T) {
	for _, p := range []string{"linux-64", "osx-arm64", "win-64", "emscripten-wasm32"} {
		d := Defaults(p)
		again, err := ApplyOverrides(p, nil, ToOverrides(d))
		if err != nil {
			t.Fatalf("ApplyOverrides(%q): %v", p, err)
		}
		if !reflect.DeepEqual(again, d) {
			t.Errorf("%s: ApplyOverrides(ToOverrides(Defaults)) = %q, want %q", p, triples(again), triples(d))
		}
	}
	d := Defaults("linux-64")
	before := triples(d)
	out, err := ApplyOverrides("linux-64", d, map[string]string{"glibc": "2.17", "cuda": "12.0", "unix": ""})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := triples(out), []string{"__archspec 1 x86_64", "__cuda 12.0 0", "__glibc 2.17 0", "__linux 4.18 0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyOverrides = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(triples(d), before) {
		t.Errorf("ApplyOverrides modified its input")
	}
	if _, err := ApplyOverrides("linux", d, nil); !errors.Is(err, ErrInvalidPlatform) {
		t.Errorf("ApplyOverrides(linux) err = %v", err)
	}
	got := ToOverrides([]*record.PackageRecord{
		{Name: "__archspec", Version: "1", Build: "x86_64_v3"},
		{Name: "__glibc", Version: "2.35", Build: "0"},
		{Name: "python", Version: "3.11"},
		nil,
	})
	if want := map[string]string{"__archspec": "x86_64_v3", "__glibc": "2.35"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ToOverrides = %v, want %v", got, want)
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/linux.rs
// (test_extract_linux_version_part).
func TestExtractLinuxVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"5.10.102.1-microsoft-standard-WSL2", "5.10.102.1"},
		{"2.6.32-220.17.1.el6.i686", "2.6.32"},
		{"5.4.72-microsoft-standard-WSL2", "5.4.72"},
		{"4.9.43-1-MANJARO", "4.9.43"},
		{"3.16.0-31-generic", "3.16.0"},
		// goomba additions
		{"6.8.0", "6.8.0"},
		{"6.8", "6.8"},
		{"6.8.0.1.2", "6.8.0.1"},
		{"6.8.", "6.8"},
		{"6", ""},
		{"", ""},
		{"v6.8", ""},
		{"7.1.1-76070101-generic", "7.1.1"},
	}
	for _, c := range cases {
		if got := numericPrefix(c.in, 2, 4); got != c.want {
			t.Errorf("numericPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	good := []string{"0", "2.17", "10.0.22631", "1!2.0", "1.0+local", "2025c", "1_2", "overridden", "9.0"}
	bad := []string{"", "2..17", ".1", "1.", "1!2!3", "a!1", "!1", "1+", "1++2", "1-2", "1 2", ">=1", "1*", "é", "1.\n"}
	for _, v := range good {
		if err := checkVersion(v); err != nil {
			t.Errorf("checkVersion(%q) = %v", v, err)
		}
	}
	for _, v := range bad {
		if err := checkVersion(v); err == nil {
			t.Errorf("checkVersion(%q) = nil, want error", v)
		}
	}
}

func TestPlatformFor(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"linux", "amd64", "linux-64"},
		{"linux", "386", "linux-32"},
		{"linux", "arm64", "linux-aarch64"},
		{"linux", "arm", "linux-armv7l"},
		{"linux", "ppc64le", "linux-ppc64le"},
		{"linux", "ppc64", "linux-ppc64"},
		{"linux", "s390x", "linux-s390x"},
		{"linux", "riscv64", "linux-riscv64"},
		{"linux", "loong64", "linux-loongarch64"},
		{"darwin", "amd64", "osx-64"},
		{"darwin", "arm64", "osx-arm64"},
		{"windows", "amd64", "win-64"},
		{"windows", "386", "win-32"},
		{"windows", "arm64", "win-arm64"},
		{"freebsd", "amd64", "freebsd-64"},
		{"freebsd", "arm64", "freebsd-arm64"},
		{"android", "arm64", "android-aarch64"},
		{"android", "arm", "android-armv7a"},
		{"ios", "arm64", "ios-arm64"},
		{"wasip1", "wasm", "wasi-wasm32"},
		{"darwin", "ppc64", ""},
		{"plan9", "amd64", ""},
		{"js", "wasm", ""},
		{"linux", "mips", ""},
	}
	for _, c := range cases {
		if got := platformFor(c.goos, c.goarch); got != c.want {
			t.Errorf("platformFor(%s, %s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// Feature combinations follow libmamba's get_archspec_x86_64.
func TestX86Level(t *testing.T) {
	v2 := x86Features{POPCNT: true, SSE3: true, SSSE3: true, SSE41: true, SSE42: true}
	v3 := v2
	v3.AVX, v3.AVX2, v3.BMI1, v3.BMI2, v3.FMA = true, true, true, true, true
	v4 := v3
	v4.AVX512F, v4.AVX512BW, v4.AVX512CD, v4.AVX512DQ, v4.AVX512VL = true, true, true, true, true
	almostV4 := v3
	almostV4.AVX512F, almostV4.AVX512BW, almostV4.AVX512CD, almostV4.AVX512DQ = true, true, true, true
	almostV2 := v2
	almostV2.SSE42 = false
	cases := []struct {
		f    x86Features
		want string
	}{
		{x86Features{}, "x86_64"},
		{almostV2, "x86_64"},
		{v2, "x86_64_v2"},
		{v3, "x86_64_v3"},
		{almostV4, "x86_64_v3"},
		{v4, "x86_64_v4"},
	}
	for _, c := range cases {
		if got := x86Level(c.f); got != c.want {
			t.Errorf("x86Level(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
	if got := hostArchspecFor("arm64", "linux-aarch64", nil); got != "aarch64" {
		t.Errorf("linux arm64 archspec = %q", got)
	}
	if got := hostArchspecFor("arm64", "osx-arm64", nil); got != "arm64" {
		t.Errorf("osx arm64 archspec = %q", got)
	}
	if got := hostArchspecFor("386", "linux-32", nil); got != "x86" {
		t.Errorf("linux 386 archspec = %q", got)
	}
	if got := hostArchspecFor("amd64", "linux-64", func() x86Features { return v3 }); got != "x86_64_v3" {
		t.Errorf("amd64 archspec = %q", got)
	}
	if got := hostArchspecFor("mips", "", nil); got != "0" {
		t.Errorf("unknown archspec = %q", got)
	}
}

// TestRecordedReplayIsExact pins how a lockfile's virtual map turns back
// into records: ApplyOverrides(platform, nil, m) reproduces the recorded set
// exactly, while Detect with m as Overrides would add host packages the set
// lacks.
func TestRecordedReplayIsExact(t *testing.T) {
	host := fakeProbe{plat: "linux-64", release: "6.8.0", glibc: "2.35", arch: "x86_64_v3"}
	recorded := mustDetect(t, host, map[string]string{"glibc": ""})
	m := ToOverrides(recorded)
	if want := map[string]string{"__archspec": "x86_64_v3", "__linux": "6.8.0", "__unix": "0"}; !reflect.DeepEqual(m, want) {
		t.Fatalf("ToOverrides = %v, want %v", m, want)
	}
	if err := CheckRecorded("linux-64", m); err != nil {
		t.Fatal(err)
	}
	replayed, err := ApplyOverrides("linux-64", nil, m)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, recorded) {
		t.Errorf("replay = %q, want exactly %q", triples(replayed), triples(recorded))
	}
	// The wrong way: Detect keeps this host's __glibc.
	if find(mustDetect(t, host, m), "__glibc") == nil {
		t.Error("Detect with a recorded set as overrides no longer adds host packages; update the documentation")
	}

	sets := [][]*record.PackageRecord{
		mustDetect(t, fakeProbe{plat: "osx-arm64", osx: "14.5"}, map[string]string{"cuda": "12.4"}),
		mustDetect(t, fakeProbe{plat: "win-64", win: "10.0.26100", arch: "x86_64_v2"}, nil),
		mustDetect(t, fakeProbe{plat: "linux-aarch64", release: "6.1"}, map[string]string{"cuda_arch": "8.6"}),
	}
	for _, p := range []string{
		"linux-32", "linux-64", "linux-aarch64", "linux-armv6l", "linux-armv7l", "linux-loongarch64",
		"linux-ppc64le", "linux-ppc64", "linux-ppc", "linux-s390x", "linux-riscv32", "linux-riscv64",
		"freebsd-64", "osx-64", "osx-arm64", "ios-arm64", "android-aarch64", "win-32", "win-64",
		"win-arm64", "emscripten-wasm32", "wasi-wasm32", "zos-z",
	} {
		if !KnownPlatform(p) {
			t.Fatalf("KnownPlatform(%q) = false", p)
		}
		sets = append(sets, Defaults(p))
	}
	for _, recs := range sets {
		platform := recs[0].Subdir
		m := ToOverrides(recs)
		if err := CheckRecorded(platform, m); err != nil {
			t.Errorf("%s: CheckRecorded(%v) = %v", platform, m, err)
			continue
		}
		got, err := ApplyOverrides(platform, nil, m)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, recs) {
			t.Errorf("%s: replay = %q, want %q", platform, triples(got), triples(recs))
		}
	}
	for _, p := range []string{"noarch", "", "linux", "Linux-64", "linux-65"} {
		if KnownPlatform(p) {
			t.Errorf("KnownPlatform(%q) = true", p)
		}
	}
}

func TestCheckRecorded(t *testing.T) {
	good := map[string]string{"__archspec": "x86_64_v3", "__glibc": "2.17", "__linux": "4.18", "__unix": "0", "__cuda": "12.4"}
	if err := CheckRecorded("linux-64", good); err != nil {
		t.Fatal(err)
	}
	if err := CheckRecorded("linux-64", nil); err != nil {
		t.Errorf("empty set: %v", err)
	}
	bad := []struct {
		platform string
		m        map[string]string
	}{
		{"linux-64", map[string]string{"glibc": "2.17"}},
		{"linux-64", map[string]string{"__GLIBC": "2.17"}},
		{"linux-64", map[string]string{"__glibc": ""}},
		{"linux-64", map[string]string{"__glibc": "2..17"}},
		{"linux-64", map[string]string{"__glibc": strings.Repeat("1", 65)}},
		{"linux-64", map[string]string{"__" + strings.Repeat("a", 63): "1"}},
		{"linux-64", map[string]string{"__archspec": "X86_64"}},
		{"linux-64", map[string]string{"__archspec": "x86-64"}},
		{"linux-64", map[string]string{"__osx": "13.0"}},
		{"linux-64", map[string]string{"__win": "10.0"}},
		{"osx-64", map[string]string{"__glibc": "2.17"}},
		{"osx-64", map[string]string{"__linux": "4.18"}},
		{"win-64", map[string]string{"__unix": "0"}},
	}
	for _, c := range bad {
		if err := CheckRecorded(c.platform, c.m); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("CheckRecorded(%q, %v) = %v, want ErrInvalidOverride", c.platform, c.m, err)
		}
	}
	if err := CheckRecorded("linux", good); !errors.Is(err, ErrInvalidPlatform) {
		t.Errorf("CheckRecorded(linux) = %v", err)
	}
}

func TestApplyOverridesRejectsNonVirtual(t *testing.T) {
	glibc := &record.PackageRecord{Name: "__glibc", Version: "2.17", Build: "0", Channel: "@", Subdir: "linux-64"}
	bad := [][]*record.PackageRecord{
		{nil},
		{{Name: "python", Version: "3.11", Build: "0"}},
		{glibc, glibc},
		{glibc, {Name: "_openmp_mutex", Version: "4.5", Build: "0"}},
	}
	for _, recs := range bad {
		if _, err := ApplyOverrides("linux-64", recs, nil); !errors.Is(err, ErrNotVirtual) {
			t.Errorf("ApplyOverrides(%v) = %v, want ErrNotVirtual", recs, err)
		}
	}
}

func TestArchspecOverrideLowerCased(t *testing.T) {
	linux := fakeProbe{plat: "linux-64", release: "6.8.0", glibc: "2.35", arch: "x86_64"}
	if r := find(mustDetect(t, linux, map[string]string{"ARCHSPEC": "X86_64_V3"}), "__archspec"); r == nil || r.Build != "x86_64_v3" {
		t.Errorf("archspec X86_64_V3 = %v", r)
	}
	// Spellings that differ only in case are the same value.
	if r := find(mustDetect(t, linux, map[string]string{"archspec": "Skylake", "__archspec": "skylake"}), "__archspec"); r == nil || r.Build != "skylake" {
		t.Errorf("archspec Skylake = %v", r)
	}
	// Versions keep their case.
	if r := find(mustDetect(t, linux, map[string]string{"cuda": "12.4RC"}), "__cuda"); r == nil || r.Version != "12.4RC" {
		t.Errorf("cuda 12.4RC = %v", r)
	}
}
