//go:build linux

// Ported from libmamba src/util/os_unix.cpp and src/core/virtual_packages.cpp
// and rattler crates/rattler_virtual_packages/src/{linux.rs,libc.rs}
// (BSD-3-Clause, see NOTICE).

package virtual

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// getconfTimeout bounds the getconf fallback.
const getconfTimeout = 5 * time.Second

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
	return glibcFromGetconf(ctx)
}

func (hostProbe) osxVersion() (string, bool) { return "", false }

func (hostProbe) winVersion() (string, bool) { return "", false }

func (hostProbe) archspec() string { return hostArchspec() }

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

// glibcFromGetconf runs `getconf GNU_LIBC_VERSION`, the only command this
// package executes, with an empty environment.
func glibcFromGetconf(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, getconfTimeout)
	defer cancel()
	// goomba never reads PATH, so getconf is only tried at absolute paths.
	getconfPaths := []string{
		"/usr/bin/getconf",
		"/bin/getconf",
		"/run/current-system/sw/bin/getconf", // NixOS
	}
	for _, p := range getconfPaths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, p, "GNU_LIBC_VERSION")
		cmd.Env = []string{}
		cmd.Stdout = &limitedWriter{w: &out, n: 4096}
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				// musl's getconf does not know GNU_LIBC_VERSION.
				return "", false
			}
			continue
		}
		if v := parseLibcVersionOutput(out.String()); v != "" {
			return v, true
		}
		return "", false
	}
	return "", false
}

// limitedWriter keeps at most n bytes and silently drops the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	k := len(p)
	if len(p) > l.n {
		p = p[:l.n]
	}
	l.n -= len(p)
	if _, err := l.w.Write(p); err != nil {
		return 0, err
	}
	return k, nil
}
