// Ported from libmamba src/core/virtual_packages.cpp and rattler
// crates/rattler_virtual_packages/src/{lib.rs,defaults.rs} (BSD-3-Clause, see
// NOTICE).

package virtual

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/richinsley/goomba/record"
)

// Sentinel errors returned (wrapped) by this package.
var (
	// ErrNotHost reports that Detect was asked for a platform other than the
	// host's. Use Defaults for other platforms.
	ErrNotHost = errors.New("platform is not the host platform")
	// ErrUnknownHost reports that the host has no conda platform.
	ErrUnknownHost = errors.New("host has no conda platform")
	// ErrInvalidOverride reports an override with a malformed name or value,
	// or two keys naming the same package with different values.
	ErrInvalidOverride = errors.New("invalid override")
	// ErrInvalidPlatform reports a platform that is not of the form os-arch.
	ErrInvalidPlatform = errors.New("invalid platform")
	// ErrNotVirtual reports a nil, duplicate or non-virtual record passed to
	// ApplyOverrides.
	ErrNotVirtual = errors.New("not a virtual package record")
	// ErrInvalidOption reports an unusable Options.NvidiaSMI: a relative
	// path, or, on Linux and Windows, a path that is not a regular file.
	ErrInvalidOption = errors.New("invalid option")
)

// Options configures Detect. The zero value detects the host platform with
// no overrides. Detect only reads an Options, so one value may be shared by
// concurrent calls as long as nobody modifies it.
type Options struct {
	// Platform is the target subdir, e.g. "linux-64". "" means the host
	// platform; any other value must equal HostPlatform().
	Platform string
	// Overrides maps a virtual package name to a version and wins over
	// detection, like conda's CONDA_OVERRIDE_* variables. Keys may omit the
	// "__" prefix and are case-insensitive ("glibc", "__glibc" and "GLIBC"
	// are the same key). An empty value removes the package; a value for a
	// package the platform lacks adds it (e.g. "__cuda": "12.4"). The value
	// for "__archspec" is the microarchitecture, i.e. the build string
	// ("x86_64_v3"), as in conda; archspec names are lower-case, so the value
	// is lower-cased ("X86_64_V3" means "x86_64_v3"). The value for
	// "__cuda_arch" is a compute capability, exactly major.minor with digits
	// only ("8.6"), as in rattler; like a detected one, it is dropped when
	// there is no __cuda (detected or overridden), and "__cuda": "" with a
	// non-empty "__cuda_arch" is an error. Other values are used as given.
	// Overrides for packages that cannot exist on the platform (__linux and
	// __glibc off Linux, __osx off macOS, __win off Windows, __unix on
	// Windows) are ignored, as libmamba does, so one override map can serve
	// several platforms.
	Overrides map[string]string
	// NvidiaSMI is the absolute path of the nvidia-smi program Detect runs
	// to find __cuda and __cuda_arch on Linux and Windows, for callers that
	// know where it is (a command-line flag, say). When set, Detect runs
	// only this file, as given, without the ownership checks it applies to
	// the system locations; on Windows its name must end in ".exe". "" means
	// the fixed system locations listed in the package documentation; goomba
	// never searches PATH. A relative path is an ErrInvalidOption error on
	// every platform, and on Linux and Windows so is a path that is not a
	// regular file, even when overrides make the run unnecessary. On other
	// platforms the path is otherwise ignored.
	NvidiaSMI string
}

// probe supplies host facts to detect. hostProbe is the real one; tests use
// fakes.
type probe interface {
	platform() string
	linuxRelease() (string, bool)
	glibcVersion(ctx context.Context) (string, bool)
	osxVersion() (string, bool)
	winVersion() (string, bool)
	archspec() string
	// findNvidiaSMI returns the nvidia-smi to run: explicit when it is set
	// and a regular file, otherwise the first trusted system location.
	findNvidiaSMI(explicit string) (program, bool)
	// runNvidiaSMI runs prog with args and returns its stdout, or false if
	// it could not be run to completion within the limits (a non-zero exit
	// status still returns the output).
	runNvidiaSMI(ctx context.Context, prog program, args ...string) ([]byte, bool)
}

// Detect returns the virtual packages of this machine for opt.Platform
// (which must be the host platform), with Overrides applied, sorted by name.
//
// On Linux it reports __unix, __linux (the numeric part of the kernel
// release, "0" if that cannot be read), __glibc (absent on musl and other
// non-glibc systems) and __archspec; on macOS __unix, __osx and __archspec;
// on Windows __win and __archspec. __archspec has version "1" and the
// microarchitecture as its build string: the x86-64 level (x86_64 to
// x86_64_v4) on amd64 and the platform's architecture (aarch64, arm64,
// ppc64le, ...) elsewhere, as in libmamba.
//
// On Linux and Windows, Detect also reports the NVIDIA GPUs, from nvidia-smi
// as conda and rattler do, both with build "0":
//   - __cuda: the highest CUDA version the installed driver supports
//     (`nvidia-smi --query -u -x`), as major.minor. This is the driver's
//     version, not the toolkit's: micromamba reports the toolkit version
//     from /usr/local/cuda/version.json when that file exists, so on such a
//     machine goomba and micromamba can disagree.
//   - __cuda_arch: the lowest compute capability among the GPUs
//     (`nvidia-smi --query-gpu=compute_cap --format=csv,noheader`), as
//     major.minor without sub-architecture letters. It is reported only
//     together with __cuda.
//
// Missing, unparsable or out-of-range nvidia-smi output leaves the package
// out; it is never an error. An overridden package is not probed, and
// nvidia-smi is not run when neither package needs probing: removing
// __cuda ("__cuda": "") also removes the detected __cuda_arch. On other
// platforms the two appear only through Overrides.
//
// nvidia-smi is Options.NvidiaSMI when that is set, or else the first
// usable one of a few fixed system locations; the package documentation
// lists them, the checks a location must pass, and when to set
// Options.NvidiaSMI instead.
//
// Detect reads files and may run getconf and nvidia-smi; it returns
// ctx.Err() as soon as ctx is cancelled, leaving goroutines to reap a
// program it killed. Each program gets at most 10 seconds (5 for getconf).
// A program that cannot be killed, such as an nvidia-smi in uninterruptible
// sleep on a GPU that fell off the bus, is abandoned about two seconds
// after its timeout, and Detect returns; two goroutines (one waiting for
// the process, and os/exec's context watcher) then stay blocked until the
// process finally exits.
func Detect(ctx context.Context, opt Options) ([]*record.PackageRecord, error) {
	return detect(ctx, opt, hostProbe{})
}

func detect(ctx context.Context, opt Options, p probe) ([]*record.PackageRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host := p.platform()
	if host == "" {
		return nil, fmt.Errorf("virtual: detect: %w", ErrUnknownHost)
	}
	platform := opt.Platform
	if platform == "" {
		platform = host
	}
	if platform != host {
		return nil, fmt.Errorf("virtual: detect %q on host %q: %w", platform, host, ErrNotHost)
	}
	ov, err := normalizeOverrides(opt.Overrides)
	if err != nil {
		return nil, err
	}
	os, _, ok := splitPlatform(platform)
	if !ok {
		return nil, fmt.Errorf("virtual: detect %q: %w", platform, ErrInvalidPlatform)
	}
	if opt.NvidiaSMI != "" {
		if err := checkNvidiaSMIPath(os, opt.NvidiaSMI); err != nil {
			return nil, fmt.Errorf("virtual: detect: NvidiaSMI %q: %w: %v", opt.NvidiaSMI, ErrInvalidOption, err)
		}
		// Checked whether or not nvidia-smi will run, so that a mistyped
		// path never quietly means "no GPU".
		if probesCUDA(os) {
			if _, ok := p.findNvidiaSMI(opt.NvidiaSMI); !ok {
				return nil, fmt.Errorf("virtual: detect: NvidiaSMI %q: %w: not a regular file", opt.NvidiaSMI, ErrInvalidOption)
			}
		}
	}

	var recs []*record.PackageRecord
	add := func(name, version, build string) {
		recs = append(recs, newRecord(platform, name, version, build))
	}
	// An overridden package is not probed: the override decides it.
	shouldProbe := func(name string) bool {
		_, overridden := ov[name]
		return !overridden
	}
	if isUnixOS(os) && shouldProbe("__unix") {
		add("__unix", "0", "0")
	}
	switch os {
	case "linux":
		if shouldProbe("__linux") {
			v := "0"
			if rel, ok := p.linuxRelease(); ok {
				if n := numericPrefix(rel, 2, 4); n != "" {
					v = n
				}
			}
			add("__linux", v, "0")
		}
		if shouldProbe("__glibc") {
			if v, ok := p.glibcVersion(ctx); ok && checkVersion(v) == nil {
				add("__glibc", v, "0")
			}
		}
	case "osx":
		if shouldProbe("__osx") {
			add("__osx", detectedOr(p.osxVersion()), "0")
		}
	case "win":
		if shouldProbe("__win") {
			add("__win", detectedOr(p.winVersion()), "0")
		}
	}
	if shouldProbe("__archspec") {
		build := p.archspec()
		if checkBuild(build) != nil {
			build = "0"
		}
		add("__archspec", "1", build)
	}
	if probesCUDA(os) {
		detectCUDA(ctx, p, opt.NvidiaSMI, ov, add)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return applyOverrides(platform, recs, ov), nil
}

// detectedOr returns a detected version, or "0" (libmamba's fallback) when
// detection failed or produced something that is not a version.
func detectedOr(v string, ok bool) string {
	if !ok || checkVersion(v) != nil {
		return "0"
	}
	return v
}

// Default versions for platforms goomba cannot probe, from rattler's
// defaults.rs (which pixi uses): RHEL 8's kernel and glibc for Linux,
// macOS 13 and Windows 10.
const (
	DefaultLinuxVersion        = "4.18"
	DefaultGlibcVersion        = "2.28"
	DefaultGlibcVersionRiscv64 = "2.39"
	DefaultOSXVersion          = "13.0"
	DefaultWinVersion          = "10.0"
)

// Defaults returns conservative virtual packages for any platform, for
// building lockfiles for platforms other than the host, sorted by name.
//
// The versions follow rattler's baseline (rattler_virtual_packages
// defaults.rs), which pixi uses:
//   - linux-*: __unix 0, __linux 4.18 and __glibc 2.28, the kernel and C
//     library of RHEL 8 / Debian 10 and conda-forge's current baseline;
//     linux-riscv64 gets __glibc 2.39 because conda-forge builds riscv64
//     against it and no older riscv64 glibc exists in practice.
//   - osx-*: __unix 0 and __osx 13.0.
//   - win-*: __win 10.0.
//   - other unix platforms (freebsd, ios, iossimulator, android,
//     emscripten): __unix 0.
//   - every platform: __archspec 1 with the most generic microarchitecture
//     of the platform's architecture, using libmamba's names so that a lock
//     built here matches one built on the target: x86_64 (never a v2-v4
//     level), x86, aarch64, arm64, ppc64le, s390x, wasm32, ...
//
// __cuda and __cuda_arch are never included: no platform is assumed to
// have a GPU. Defaults
// returns nil for "noarch" and for strings that are not of the form os-arch
// with lower-case letters, digits and '_' on each side.
func Defaults(platform string) []*record.PackageRecord {
	os, arch, ok := splitPlatform(platform)
	if !ok || !isPlatformWord(os) || !isPlatformWord(arch) {
		return nil
	}
	var recs []*record.PackageRecord
	add := func(name, version, build string) {
		recs = append(recs, newRecord(platform, name, version, build))
	}
	if isUnixOS(os) {
		add("__unix", "0", "0")
	}
	switch os {
	case "linux":
		add("__linux", DefaultLinuxVersion, "0")
		if arch == "riscv64" {
			add("__glibc", DefaultGlibcVersionRiscv64, "0")
		} else {
			add("__glibc", DefaultGlibcVersion, "0")
		}
	case "osx":
		add("__osx", DefaultOSXVersion, "0")
	case "win":
		add("__win", DefaultWinVersion, "0")
	}
	add("__archspec", "1", archspecForPlatform(arch))
	sortRecords(recs)
	return recs
}

func isPlatformWord(s string) bool {
	if s == "" || len(s) > maxValueLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z') && !isDigit(c) && c != '_' {
			return false
		}
	}
	return true
}

// ApplyOverrides returns copies of recs with overrides applied exactly as
// Detect applies Options.Overrides for platform, sorted by name. recs must
// hold only virtual package records (names starting with "__"), one per
// name; a nil, non-virtual or duplicate record is an ErrNotVirtual error.
// recs is not modified. As in Detect, a __cuda_arch is dropped when the
// result has no __cuda.
//
// Two uses:
//   - ApplyOverrides(p, Defaults(p), userOverrides) locks for another
//     platform with user overrides;
//   - ApplyOverrides(p, nil, m) turns a recorded set m (a lockfile's
//     virtual map, see ToOverrides and CheckRecorded) back into exactly the
//     records it was made from. Do not pass a recorded set to Detect as
//     Options.Overrides: Detect keeps the host packages the set lacks, so a
//     set recorded without __glibc would come back with this host's glibc.
func ApplyOverrides(platform string, recs []*record.PackageRecord, overrides map[string]string) ([]*record.PackageRecord, error) {
	if _, _, ok := splitPlatform(platform); !ok {
		return nil, fmt.Errorf("virtual: apply overrides for %q: %w", platform, ErrInvalidPlatform)
	}
	seen := make(map[string]bool, len(recs))
	for i, r := range recs {
		switch {
		case r == nil:
			return nil, fmt.Errorf("virtual: apply overrides: record %d is nil: %w", i, ErrNotVirtual)
		case !r.IsVirtual():
			return nil, fmt.Errorf("virtual: apply overrides: record %q: %w", r.Name, ErrNotVirtual)
		case seen[r.Name]:
			return nil, fmt.Errorf("virtual: apply overrides: record %q appears twice: %w", r.Name, ErrNotVirtual)
		}
		seen[r.Name] = true
	}
	ov, err := normalizeOverrides(overrides)
	if err != nil {
		return nil, err
	}
	return applyOverrides(platform, recs, ov), nil
}

// CheckRecorded reports whether m is a valid recorded set of virtual
// packages for platform, the form ToOverrides produces and lockfiles store:
// every key a full lower-case name ("__glibc", not "glibc"), every value a
// non-empty version (for "__archspec", a lower-case build string; for
// "__cuda_arch", major.minor), every package one the platform can carry (no
// __glibc on osx-64, no __unix on win-64), and no __cuda_arch without
// __cuda. For such an m, ApplyOverrides(platform, nil, m) reproduces the
// recorded records exactly. Errors wrap ErrInvalidOverride (or
// ErrInvalidPlatform) and name the first bad key in sorted order.
func CheckRecorded(platform string, m map[string]string) error {
	os, _, ok := splitPlatform(platform)
	if !ok {
		return fmt.Errorf("virtual: recorded set for %q: %w", platform, ErrInvalidPlatform)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if err := checkName(k); err != nil {
			return fmt.Errorf("virtual: recorded %q: %w: %v", k, ErrInvalidOverride, err)
		}
		if v == "" {
			return fmt.Errorf("virtual: recorded %q: %w: empty version", k, ErrInvalidOverride)
		}
		if err := checkOverrideValue(k, v); err != nil {
			return fmt.Errorf("virtual: recorded %q=%q: %w: %v", k, v, ErrInvalidOverride, err)
		}
		if k == "__archspec" && strings.ToLower(v) != v {
			return fmt.Errorf("virtual: recorded %q=%q: %w: archspec names are lower-case", k, v, ErrInvalidOverride)
		}
		if !appliesTo(k, os) {
			return fmt.Errorf("virtual: recorded %q: %w: cannot exist on %q", k, ErrInvalidOverride, platform)
		}
	}
	if _, ok := m["__cuda_arch"]; ok {
		if _, ok := m["__cuda"]; !ok {
			return fmt.Errorf("virtual: recorded %q without %q: %w", "__cuda_arch", "__cuda", ErrInvalidOverride)
		}
	}
	return nil
}

// ToOverrides returns the recorded form of the virtual records in recs:
// name to version, except __archspec, which maps to its build string.
// Non-virtual records are skipped. Lockfiles store this map; for records
// from Detect, Defaults or ApplyOverrides for one platform,
// ApplyOverrides(platform, nil, ToOverrides(recs)) returns exactly recs.
func ToOverrides(recs []*record.PackageRecord) map[string]string {
	out := make(map[string]string, len(recs))
	for _, r := range recs {
		if r == nil || !r.IsVirtual() {
			continue
		}
		if r.Name == "__archspec" {
			out[r.Name] = r.Build
		} else {
			out[r.Name] = r.Version
		}
	}
	return out
}

func newRecord(platform, name, version, build string) *record.PackageRecord {
	return &record.PackageRecord{
		Name:    name,
		Version: version,
		Build:   build,
		Subdir:  platform,
		Channel: "@",
	}
}

func sortRecords(recs []*record.PackageRecord) {
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
}

// normalizeOverrides validates overrides and returns them keyed by full,
// lower-case package name. Keys are visited in sorted order so the error for
// a map with several bad keys does not depend on map iteration order.
func normalizeOverrides(ov map[string]string) (map[string]string, error) {
	keys := make([]string, 0, len(ov))
	for k := range ov {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(ov))
	from := make(map[string]string, len(ov))
	for _, k := range keys {
		v := ov[k]
		name := strings.ToLower(k)
		if !strings.HasPrefix(name, "__") {
			name = "__" + name
		}
		if err := checkName(name); err != nil {
			return nil, fmt.Errorf("virtual: override %q: %w: %v", k, ErrInvalidOverride, err)
		}
		if v != "" {
			if err := checkOverrideValue(name, v); err != nil {
				return nil, fmt.Errorf("virtual: override %q=%q: %w: %v", k, v, ErrInvalidOverride, err)
			}
			if name == "__archspec" {
				v = strings.ToLower(v)
			}
		}
		if prev, dup := out[name]; dup && prev != v {
			return nil, fmt.Errorf("virtual: override %q=%q conflicts with %q=%q: %w",
				k, v, from[name], prev, ErrInvalidOverride)
		}
		out[name] = v
		from[name] = k
	}
	if cuda, ok := out["__cuda"]; ok && cuda == "" && out["__cuda_arch"] != "" {
		return nil, fmt.Errorf("virtual: override %q=%q with %q=%q: %w: __cuda_arch needs __cuda",
			from["__cuda_arch"], out["__cuda_arch"], from["__cuda"], cuda, ErrInvalidOverride)
	}
	return out, nil
}

func checkOverrideValue(name, v string) error {
	switch name {
	case "__archspec":
	case "__cuda_arch":
		// rattler's CudaArch::parse_version: a compute capability is exactly
		// major.minor; "86" would satisfy every __cuda_arch >=X.
		if err := checkVersion(v); err != nil {
			return err
		}
		if !isValidCudaVersionFormat(v) {
			return errors.New("a compute capability is major.minor with digits only, e.g. 8.6")
		}
		return nil
	default:
		return checkVersion(v)
	}
	err := checkBuild(v)
	if err != nil && strings.Contains(v, "-") {
		if fixed := strings.ReplaceAll(v, "-", "_"); checkBuild(fixed) == nil {
			return fmt.Errorf("%v; did you mean %q?", err, fixed)
		}
	}
	return err
}

// appliesTo reports whether a virtual package can exist on platforms of os.
func appliesTo(name, os string) bool {
	switch name {
	case "__linux", "__glibc":
		return os == "linux"
	case "__osx":
		return os == "osx"
	case "__win":
		return os == "win"
	case "__unix":
		return isUnixOS(os)
	}
	return true
}

// applyOverrides applies normalized overrides to copies of recs.
func applyOverrides(platform string, recs []*record.PackageRecord, ov map[string]string) []*record.PackageRecord {
	os, _, _ := splitPlatform(platform)
	byName := make(map[string]*record.PackageRecord, len(recs)+len(ov))
	var order []string
	for _, r := range recs {
		c := *r
		order = append(order, c.Name)
		byName[c.Name] = &c
	}
	names := make([]string, 0, len(ov))
	for n := range ov {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !appliesTo(name, os) {
			continue
		}
		v := ov[name]
		if v == "" {
			delete(byName, name)
			continue
		}
		r, ok := byName[name]
		if !ok {
			r = newRecord(platform, name, "0", "0")
			byName[name] = r
			order = append(order, name)
		}
		if name == "__archspec" {
			r.Version, r.Build = "1", v
		} else {
			r.Version = v
		}
	}
	// As in rattler (and the CEP that defines it), __cuda_arch exists only
	// alongside __cuda.
	if _, ok := byName["__cuda"]; !ok {
		delete(byName, "__cuda_arch")
	}
	out := make([]*record.PackageRecord, 0, len(byName))
	for _, n := range order {
		if r, ok := byName[n]; ok {
			out = append(out, r)
		}
	}
	sortRecords(out)
	return out
}
