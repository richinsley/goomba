// Ported from rattler crates/rattler_conda_types/src/subdir.rs and libmamba
// src/core/virtual_packages.cpp (BSD-3-Clause, see NOTICE).

package virtual

import (
	"runtime"
	"strings"
)

// HostPlatform returns the conda subdir of the machine goomba runs on, e.g.
// "linux-64" or "osx-arm64", derived from the Go build target. It returns ""
// when the target has no conda subdir.
func HostPlatform() string {
	return platformFor(runtime.GOOS, runtime.GOARCH)
}

// platformFor maps a Go GOOS/GOARCH pair to a conda subdir, or "" if there
// is none. 32-bit ARM maps to armv7l, the only 32-bit ARM subdir conda-forge
// still builds.
func platformFor(goos, goarch string) string {
	var arch string
	switch goarch {
	case "amd64":
		arch = "64"
	case "386":
		arch = "32"
	case "arm64":
		arch = "arm64"
		if goos == "linux" || goos == "android" {
			arch = "aarch64"
		}
	case "arm":
		arch = "armv7l"
		if goos == "android" {
			arch = "armv7a"
		}
	case "ppc64le":
		arch = "ppc64le"
	case "ppc64":
		arch = "ppc64"
	case "s390x":
		arch = "s390x"
	case "riscv64":
		arch = "riscv64"
	case "loong64":
		arch = "loongarch64"
	case "wasm":
		arch = "wasm32"
	default:
		return ""
	}
	var os string
	switch goos {
	case "linux":
		os = "linux"
	case "darwin":
		os = "osx"
	case "windows":
		os = "win"
	case "freebsd":
		os = "freebsd"
	case "android":
		os = "android"
	case "ios":
		os = "ios"
	case "wasip1":
		os = "wasi"
	default:
		return ""
	}
	p := os + "-" + arch
	if !KnownPlatform(p) {
		return ""
	}
	return p
}

// KnownPlatform reports whether p is a conda platform subdir: one of
// rattler's Subdir values other than "noarch" (linux-64, osx-arm64, win-64,
// emscripten-wasm32, ...). Package lock uses it too, so there is one list.
func KnownPlatform(p string) bool {
	switch p {
	case "linux-32", "linux-64", "linux-aarch64", "linux-armv6l", "linux-armv7l",
		"linux-loongarch64", "linux-ppc64le", "linux-ppc64", "linux-ppc",
		"linux-s390x", "linux-riscv32", "linux-riscv64",
		"freebsd-32", "freebsd-64", "freebsd-arm64", "freebsd-ppc64le", "freebsd-ppc64",
		"osx-64", "osx-arm64",
		"ios-arm64", "iossimulator-arm64", "iossimulator-64",
		"android-aarch64", "android-armv7a", "android-64", "android-32",
		"win-32", "win-64", "win-arm64",
		"emscripten-wasm32", "emscripten-wasm64", "wasi-wasm32", "zos-z":
		return true
	}
	return false
}

// splitPlatform splits "os-arch" at the first dash, as libmamba does. It
// reports false for strings without a dash or with an empty half.
func splitPlatform(p string) (os, arch string, ok bool) {
	os, arch, ok = strings.Cut(p, "-")
	if !ok || os == "" || arch == "" {
		return "", "", false
	}
	return os, arch, true
}

// isUnixOS reports whether platforms of this OS carry __unix. This follows
// rattler's Subdir::is_unix, a superset of libmamba (which only knows linux,
// osx and emscripten).
func isUnixOS(os string) bool {
	switch os {
	case "linux", "osx", "freebsd", "ios", "iossimulator", "android", "emscripten":
		return true
	}
	return false
}

// archspecForPlatform returns libmamba's __archspec build string for a
// platform's arch half: "64" is x86_64, "32" is x86, and any other arch is
// used verbatim (aarch64, arm64, ppc64le, s390x, wasm32, ...).
func archspecForPlatform(arch string) string {
	switch arch {
	case "64":
		return "x86_64"
	case "32":
		return "x86"
	}
	return arch
}
