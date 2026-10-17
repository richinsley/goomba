// Ported from libmamba src/specs/platform.cpp (BSD-3-Clause, see NOTICE).

package spec

import "runtime"

// Platform names (conda "subdirs").
const (
	PlatformNoarch       = "noarch"        // architecture-independent packages
	PlatformLinux32      = "linux-32"      // Linux on x86
	PlatformLinux64      = "linux-64"      // Linux on x86-64
	PlatformLinuxArmv6l  = "linux-armv6l"  // Linux on ARMv6
	PlatformLinuxArmv7l  = "linux-armv7l"  // Linux on ARMv7
	PlatformLinuxAarch64 = "linux-aarch64" // Linux on 64-bit ARM
	PlatformLinuxPpc64le = "linux-ppc64le" // Linux on little-endian POWER
	PlatformLinuxPpc64   = "linux-ppc64"   // Linux on big-endian POWER
	PlatformLinuxS390x   = "linux-s390x"   // Linux on IBM Z
	PlatformLinuxRiscv32 = "linux-riscv32" // Linux on 32-bit RISC-V
	PlatformLinuxRiscv64 = "linux-riscv64" // Linux on 64-bit RISC-V
	PlatformOSX64        = "osx-64"        // macOS on x86-64
	PlatformOSXArm64     = "osx-arm64"     // macOS on Apple silicon
	PlatformWin32        = "win-32"        // Windows on x86
	PlatformWin64        = "win-64"        // Windows on x86-64
	PlatformWinArm64     = "win-arm64"     // Windows on 64-bit ARM
	PlatformZosZ         = "zos-z"         // z/OS
)

// KnownPlatforms returns the platforms known to conda, in libmamba's order.
// The caller owns the returned slice.
func KnownPlatforms() []string {
	return []string{
		PlatformNoarch,
		PlatformLinux32,
		PlatformLinux64,
		PlatformLinuxArmv6l,
		PlatformLinuxArmv7l,
		PlatformLinuxAarch64,
		PlatformLinuxPpc64le,
		PlatformLinuxPpc64,
		PlatformLinuxS390x,
		PlatformLinuxRiscv32,
		PlatformLinuxRiscv64,
		PlatformOSX64,
		PlatformOSXArm64,
		PlatformWin32,
		PlatformWin64,
		PlatformWinArm64,
		PlatformZosZ,
	}
}

// IsKnownPlatform reports whether p is exactly one of KnownPlatforms.
func IsKnownPlatform(p string) bool {
	switch p {
	case PlatformNoarch, PlatformLinux32, PlatformLinux64, PlatformLinuxArmv6l,
		PlatformLinuxArmv7l, PlatformLinuxAarch64, PlatformLinuxPpc64le,
		PlatformLinuxPpc64, PlatformLinuxS390x, PlatformLinuxRiscv32,
		PlatformLinuxRiscv64, PlatformOSX64, PlatformOSXArm64, PlatformWin32,
		PlatformWin64, PlatformWinArm64, PlatformZosZ:
		return true
	}
	return false
}

// ParsePlatform normalizes s (trimming spaces, lower-casing) and reports
// whether it names a known platform, e.g. " OSX-64" gives "osx-64", true.
func ParsePlatform(s string) (string, bool) {
	p := asciiLower(strip(s))
	if IsKnownPlatform(p) {
		return p, true
	}
	return "", false
}

// PlatformFor maps a Go GOOS/GOARCH pair to its conda platform, reporting
// false when conda has none.
func PlatformFor(goos, goarch string) (string, bool) {
	var p string
	switch goos {
	case "linux":
		switch goarch {
		case "386":
			p = PlatformLinux32
		case "amd64":
			p = PlatformLinux64
		case "arm":
			p = PlatformLinuxArmv7l
		case "arm64":
			p = PlatformLinuxAarch64
		case "ppc64le":
			p = PlatformLinuxPpc64le
		case "ppc64":
			p = PlatformLinuxPpc64
		case "s390x":
			p = PlatformLinuxS390x
		case "riscv64":
			p = PlatformLinuxRiscv64
		}
	case "darwin":
		switch goarch {
		case "amd64":
			p = PlatformOSX64
		case "arm64":
			p = PlatformOSXArm64
		}
	case "windows":
		switch goarch {
		case "386":
			p = PlatformWin32
		case "amd64":
			p = PlatformWin64
		case "arm64":
			p = PlatformWinArm64
		}
	}
	return p, p != ""
}

// HostPlatform returns the conda platform of the running program, from
// runtime.GOOS and runtime.GOARCH, or "" if conda has no such platform.
// 32-bit ARM Linux is reported as linux-armv7l.
func HostPlatform() string {
	p, _ := PlatformFor(runtime.GOOS, runtime.GOARCH)
	return p
}

// IsLinux reports whether platform p is a Linux platform ("linux-...").
// Like libmamba it accepts unknown platforms by prefix.
func IsLinux(p string) bool { return hasPrefixFold(p, "linux") }

// IsOSX reports whether platform p is a macOS platform ("osx-...").
func IsOSX(p string) bool { return hasPrefixFold(p, "osx") }

// IsWin reports whether platform p is a Windows platform ("win-...").
func IsWin(p string) bool { return hasPrefixFold(p, "win") }

// IsUnix reports whether platform p is Linux or macOS.
func IsUnix(p string) bool { return IsLinux(p) || IsOSX(p) }

// IsNoarch reports whether p is the "noarch" platform.
func IsNoarch(p string) bool { return hasPrefixFold(p, "noarch") }

// hasPrefixFold reports whether s starts with the lower-case ASCII prefix,
// ignoring ASCII case only.
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[i]
		if isUpper(c) {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}
