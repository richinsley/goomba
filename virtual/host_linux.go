//go:build linux

// Ported from libmamba src/util/os_unix.cpp and src/core/virtual_packages.cpp
// and rattler crates/rattler_virtual_packages/src/{linux.rs,libc.rs}
// (BSD-3-Clause, see NOTICE).

package virtual

import (
	"context"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// getconfTimeout bounds the getconf fallback.
	getconfTimeout = 5 * time.Second
	// maxGetconfOutput bounds the output read from getconf, which prints one
	// short line.
	maxGetconfOutput = 4096
)

// hostProbe reads facts from the running Linux machine.
type hostProbe struct{}

func (hostProbe) platform() string { return HostPlatform() }

func (hostProbe) linuxRelease() (string, bool) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", false
	}
	return unix.ByteSliceToString(u.Release[:]), true
}

func (hostProbe) glibcVersion(ctx context.Context) (string, bool) {
	if v, ok := glibcFromSystemLibc(ctx, runtime.GOARCH); ok {
		return v, true
	}
	return glibcFromGetconf(ctx, osSearchFS{}, getconfPaths(), runCapped)
}

func (hostProbe) osxVersion() (string, bool) { return "", false }

func (hostProbe) winVersion() (string, bool) { return "", false }

func (hostProbe) archspec() string { return hostArchspec() }

func (hostProbe) findNvidiaSMI(explicit string) (program, bool) {
	return unixNvidiaSMI(osSearchFS{}, explicit, unixNvidiaSMIPaths())
}

// runNvidiaSMI runs nvidia-smi with an empty environment, which also drops
// CUDA_VISIBLE_DEVICES (rattler removes it so every GPU is reported), from
// the root directory.
func (hostProbe) runNvidiaSMI(ctx context.Context, prog program, args ...string) ([]byte, bool) {
	res, ok := runCapped(ctx, runSpec{
		path: prog.path, argv0: prog.name, args: args, env: []string{}, dir: "/",
		timeout: nvidiaSMITimeout, max: maxNvidiaSMIOutput,
	})
	return res.stdout, ok
}

// fileOwner returns the user ID owning fi, or MaxUint32 (never root) when
// the platform does not say.
func fileOwner(fi fs.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return math.MaxUint32
}

// libcCandidates lists the paths where a distribution may keep libc.so.6
// for goarch, most likely first: the Debian multiarch directories, then the
// lib64 and lib directories, then every other /lib*/*/ and /usr/lib*/*/
// directory in sorted order.
func libcCandidates(goarch string) []string {
	var triplets []string
	switch goarch {
	case "amd64":
		triplets = []string{"x86_64-linux-gnu"}
	case "386":
		triplets = []string{"i386-linux-gnu", "i686-linux-gnu"}
	case "arm64":
		triplets = []string{"aarch64-linux-gnu"}
	case "arm":
		triplets = []string{"arm-linux-gnueabihf", "arm-linux-gnueabi"}
	case "ppc64le":
		triplets = []string{"powerpc64le-linux-gnu"}
	case "ppc64":
		triplets = []string{"powerpc64-linux-gnu"}
	case "s390x":
		triplets = []string{"s390x-linux-gnu"}
	case "riscv64":
		triplets = []string{"riscv64-linux-gnu"}
	case "loong64":
		triplets = []string{"loongarch64-linux-gnu"}
	}
	var out []string
	for _, t := range triplets {
		out = append(out, "/lib/"+t+"/libc.so.6", "/usr/lib/"+t+"/libc.so.6")
	}
	out = append(out, "/lib64/libc.so.6", "/usr/lib64/libc.so.6", "/lib/libc.so.6", "/usr/lib/libc.so.6")
	var globbed []string
	for _, pattern := range []string{"/lib*/*/libc.so.6", "/usr/lib*/*/libc.so.6"} {
		m, _ := filepath.Glob(pattern)
		globbed = append(globbed, m...)
	}
	sort.Strings(globbed)
	return append(out, globbed...)
}

// glibcFromSystemLibc reads the version of the first libc.so.6 candidate
// built for goarch.
func glibcFromSystemLibc(ctx context.Context, goarch string) (string, bool) {
	want, ok := elfTargetFor(goarch)
	if !ok {
		return "", false
	}
	seen := map[string]bool{}
	for _, p := range libcCandidates(goarch) {
		if ctx.Err() != nil {
			return "", false
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		if v, err := glibcFromFile(real, want); err == nil {
			return v, true
		}
	}
	return "", false
}

// glibcFromFile opens path and reads its glibc version.
func glibcFromFile(path string, want elfTarget) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() > maxLibcSize {
		return "", errNotGlibc
	}
	return glibcFromELF(io.NewSectionReader(f, 0, st.Size()), want)
}

// getconfPaths are the fixed locations of getconf, in order. goomba never
// reads PATH.
func getconfPaths() []string {
	return []string{
		"/usr/bin/getconf",
		"/bin/getconf",
		"/run/current-system/sw/bin/getconf", // NixOS
	}
}

// glibcFromGetconf runs `getconf GNU_LIBC_VERSION` with an empty environment
// from the first of paths that trustedUnixExecutable accepts in fsys, under
// the same policy as nvidia-smi: the resolved file runs, named after its
// location. Besides nvidia-smi, it is the only program this package runs.
// A resolved file is tried once (with a merged /usr, /bin/getconf is
// /usr/bin/getconf), and only a getconf that could not be started at all
// gives way to the next location: one that started and then failed (timed
// out, was cancelled, printed too much) ends the search, which bounds it
// to a single run.
func glibcFromGetconf(ctx context.Context, fsys searchFS, paths []string, run runFunc) (string, bool) {
	tried := map[string]bool{}
	for _, p := range paths {
		real, ok := trustedUnixExecutable(fsys, p)
		if !ok || tried[real] {
			continue
		}
		tried[real] = true
		res, ok := run(ctx, runSpec{
			path: real, argv0: p, args: []string{"GNU_LIBC_VERSION"}, env: []string{}, dir: "/",
			timeout: getconfTimeout, max: maxGetconfOutput,
		})
		if !ok {
			if res.started {
				return "", false
			}
			continue
		}
		if res.exitCode != 0 {
			// musl's getconf does not know GNU_LIBC_VERSION.
			return "", false
		}
		v := parseLibcVersionOutput(string(res.stdout))
		return v, v != ""
	}
	return "", false
}
