// Ported from rattler crates/rattler_virtual_packages/src/{cuda.rs,lib.rs}
// and libmamba src/core/virtual_packages.cpp (BSD-3-Clause, see NOTICE).
// rattler asks NVML through dlopen and falls back to nvidia-smi; libmamba
// runs nvidia-smi. Both find nvidia-smi through PATH. goomba has no cgo and
// never reads PATH, so it runs nvidia-smi only from a caller-chosen path or
// from fixed system locations that pass an ownership check.

package virtual

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// nvidiaSMITimeout bounds each nvidia-smi run. Without persistence mode
	// the driver can take seconds to initialise the GPUs.
	nvidiaSMITimeout = 10 * time.Second
	// maxNvidiaSMIOutput bounds the stdout read from each run. Both queries
	// print a few hundred bytes; longer output is discarded, not parsed.
	maxNvidiaSMIOutput = 64 << 10
	// maxDriverStoreEntries bounds how many names of the Windows driver
	// store are read; a long-lived machine holds a few thousand packages.
	maxDriverStoreEntries = 1 << 14
	// maxDriverStoreCandidates bounds how many nv* driver packages are
	// examined for nvidia-smi.exe.
	maxDriverStoreCandidates = 256
	// maxCudaComponent bounds the major and minor parts of a CUDA version
	// and of a compute capability: rattler accepts CUDA majors 1 to 99 from
	// the driver API (parse_cuda_driver_version), whose encoding also caps
	// the minor at 99.
	maxCudaComponent = 99
)

// cudaVersionArgs are the arguments of the nvidia-smi run that reports the
// CUDA version the driver supports, as in rattler's
// detect_cuda_version_via_nvidia_smi and libmamba's cuda_version.
func cudaVersionArgs() []string { return []string{"--query", "-u", "-x"} }

// cudaArchArgs are the arguments of the nvidia-smi run that reports each
// GPU's compute capability, as in rattler's detect_cuda_arch_via_nvidia_smi.
func cudaArchArgs() []string {
	return []string{"--query-gpu=compute_cap", "--format=csv,noheader"}
}

// probesCUDA reports whether Detect looks for NVIDIA GPUs on platforms of
// os.
func probesCUDA(os string) bool { return os == "linux" || os == "win" }

// detectCUDA adds __cuda and __cuda_arch from nvidia-smi. A package that has
// an override is not probed. __cuda_arch is probed only when there is a
// __cuda to go with it: one detected here, or an override that sets one (an
// empty __cuda override removes __cuda and so the detected __cuda_arch).
// When neither package needs probing, or no nvidia-smi is found, nothing is
// run. Output that cannot be interpreted leaves the package out; the caller
// checks ctx afterwards.
func detectCUDA(ctx context.Context, p probe, explicit string, ov map[string]string, add func(name, version, build string)) {
	cudaOverride, cudaOverridden := ov["__cuda"]
	_, archOverridden := ov["__cuda_arch"]
	wantVersion := !cudaOverridden
	wantArch := !archOverridden && (!cudaOverridden || cudaOverride != "")
	if !wantVersion && !wantArch {
		return
	}
	smi, ok := p.findNvidiaSMI(explicit)
	if !ok {
		return
	}
	haveCuda := !wantVersion // overridden with a version
	if wantVersion {
		if out, ok := p.runNvidiaSMI(ctx, smi, cudaVersionArgs()...); ok {
			if v, ok := parseNvidiaSMICudaVersion(out); ok && checkVersion(v) == nil {
				add("__cuda", v, "0")
				haveCuda = true
			}
		}
	}
	if !wantArch || !haveCuda || ctx.Err() != nil {
		return
	}
	if out, ok := p.runNvidiaSMI(ctx, smi, cudaArchArgs()...); ok {
		if v, ok := parseNvidiaSMIComputeCap(out); ok && checkVersion(v) == nil {
			add("__cuda_arch", v, "0")
		}
	}
}

// parseNvidiaSMICudaVersion returns the CUDA version in the XML printed by
// `nvidia-smi --query -u -x`, the highest CUDA version the installed driver
// supports, normalised by normalizeCudaVersion. rattler and libmamba take
// the text of the first <cuda_version> element. goomba requires every
// <cuda_version> element to hold the same valid version, so that output it
// cannot interpret (two different versions, an unterminated element, "N/A")
// fails closed and yields no __cuda.
func parseNvidiaSMICudaVersion(out []byte) (string, bool) {
	openTag, closeTag := []byte("<cuda_version>"), []byte("</cuda_version>")
	version := ""
	rest := out
	for {
		i := bytes.Index(rest, openTag)
		if i < 0 {
			break
		}
		rest = rest[i+len(openTag):]
		j := bytes.Index(rest, closeTag)
		if j < 0 {
			return "", false
		}
		v, ok := normalizeCudaVersion(strings.Trim(string(rest[:j]), " \t\r\n"))
		if !ok || (version != "" && v != version) {
			return "", false
		}
		version = v
		rest = rest[j+len(closeTag):]
	}
	return version, version != ""
}

// normalizeCudaVersion turns a CUDA version printed by nvidia-smi into
// major.minor, the form the driver API reports and conda's CUDA packages
// use: "12.4" and "12.4.1" become "12.4", "12" becomes "12.0" (the same
// version to conda), and leading zeros are dropped. The major part must be
// 1 to 99 and the minor 0 to 99; anything else is not a CUDA version.
func normalizeCudaVersion(s string) (string, bool) {
	parts := strings.SplitN(s, ".", 4)
	if len(parts) > 3 {
		return "", false
	}
	var nums [3]int
	for i, part := range parts {
		n, ok := parseDecimal(part)
		if !ok {
			return "", false
		}
		nums[i] = n
	}
	major, minor := nums[0], nums[1]
	if major < 1 || major > maxCudaComponent || minor > maxCudaComponent {
		return "", false
	}
	return strconv.Itoa(major) + "." + strconv.Itoa(minor), true
}

// parseNvidiaSMIComputeCap returns the minimum compute capability in the
// CSV printed by `nvidia-smi --query-gpu=compute_cap --format=csv,noheader`,
// one line per GPU, as major.minor. As in rattler's
// parse_nvidia_smi_compute_capabilities, lines that are not a compute
// capability ("[N/A]" for a GPU whose capability is unknown, junk) are
// skipped and the valid ones still count; with no valid line there is no
// result.
func parseNvidiaSMIComputeCap(out []byte) (string, bool) {
	found := false
	minMajor, minMinor := 0, 0
	for len(out) > 0 {
		line := out
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			line, out = out[:i], out[i+1:]
		} else {
			out = nil
		}
		major, minor, ok := parseComputeCap(strings.TrimSpace(string(line)))
		if !ok {
			continue
		}
		if !found || major < minMajor || (major == minMajor && minor < minMinor) {
			found, minMajor, minMinor = true, major, minor
		}
	}
	if !found {
		return "", false
	}
	return strconv.Itoa(minMajor) + "." + strconv.Itoa(minMinor), true
}

// parseComputeCap parses one compute capability such as "8.6". Trailing
// sub-architecture letters are dropped ("9.0a" is 9.0), as rattler's
// CudaArch excludes them: a GPU of capability 9.0a has capability 9.0, and
// skipping the line instead could overstate the minimum. Both parts must be
// in range (major 1 to 99, minor 0 to 99).
func parseComputeCap(s string) (major, minor int, ok bool) {
	s = strings.TrimRightFunc(s, func(r rune) bool { return r < 0x80 && isAlpha(byte(r)) })
	if !isValidCudaVersionFormat(s) {
		return 0, 0, false
	}
	ma, mi, _ := strings.Cut(s, ".")
	major, ok1 := parseDecimal(ma)
	minor, ok2 := parseDecimal(mi)
	if !ok1 || !ok2 || major < 1 || major > maxCudaComponent || minor > maxCudaComponent {
		return 0, 0, false
	}
	return major, minor, true
}

// isValidCudaVersionFormat reports whether s is "major.minor" with both
// parts non-empty ASCII digits, as rattler's is_valid_cuda_version_format.
func isValidCudaVersionFormat(s string) bool {
	major, minor, ok := strings.Cut(s, ".")
	return ok && isDigits(major) && isDigits(minor)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// parseDecimal parses 1 to 9 ASCII digits, so the value fits any int.
func parseDecimal(s string) (int, bool) {
	if len(s) > 9 || !isDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// checkNvidiaSMIPath validates Options.NvidiaSMI for a host whose platform
// has the given OS half. The path must be absolute; on Windows it must also
// name an ".exe" file, so that os/exec runs exactly that file instead of
// trying the extensions in PATHEXT.
func checkNvidiaSMIPath(os, p string) error {
	if strings.IndexByte(p, 0) >= 0 {
		return errors.New("path contains a NUL byte")
	}
	if os != "win" {
		if !strings.HasPrefix(p, "/") {
			return errors.New("path is not absolute")
		}
		return nil
	}
	if !isAbsWindowsPath(p) {
		return errors.New("path is not absolute")
	}
	base := p[strings.LastIndexAny(p, `\/`)+1:]
	if len(base) <= len(".exe") || !strings.EqualFold(base[len(base)-len(".exe"):], ".exe") {
		return errors.New(`path does not name an ".exe" file`)
	}
	return nil
}

// isAbsWindowsPath reports whether p is an absolute Windows path: a drive
// letter followed by a separator ("C:\..."), or a UNC or device path
// ("\\server\share\...", "\\?\C:\...").
func isAbsWindowsPath(p string) bool {
	if len(p) >= 3 && isAlpha(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return len(p) > 2 && (p[0] == '\\' || p[0] == '/') && (p[1] == '\\' || p[1] == '/')
}

// unixNvidiaSMIPaths are the fixed locations Detect tries on Linux, in
// order. goomba never searches PATH, so these cover where nvidia-smi is
// installed in practice:
//   - /usr/bin/nvidia-smi: distribution driver packages (Debian, Ubuntu,
//     Fedora, RHEL, Arch, openSUSE) and NVIDIA's own .run installer;
//   - /usr/lib/wsl/lib/nvidia-smi: WSL2, where the Windows driver provides
//     it and distributions do not link it into /usr/bin (that WSL presents
//     it as root's, as the trust check needs, has not been verified on a
//     WSL2 machine);
//   - /usr/local/nvidia/bin/nvidia-smi: GPU containers, where Kubernetes
//     device plugins and cloud GPU images mount the host driver's tools;
//   - /run/current-system/sw/bin/nvidia-smi: NixOS, which has no /usr/bin
//     programs outside its system profile.
func unixNvidiaSMIPaths() []string {
	return []string{
		"/usr/bin/nvidia-smi",
		"/usr/lib/wsl/lib/nvidia-smi",
		"/usr/local/nvidia/bin/nvidia-smi",
		"/run/current-system/sw/bin/nvidia-smi",
	}
}

// unixNvidiaSMI returns the nvidia-smi to run on a Unix host: explicit when
// it is set (and a regular file), otherwise the first of builtin that
// trustedUnixExecutable accepts, run at its resolved path under its own
// name.
func unixNvidiaSMI(fsys searchFS, explicit string, builtin []string) (program, bool) {
	if explicit != "" {
		return program{path: explicit, name: explicit}, isRegularFile(fsys, explicit)
	}
	for _, p := range builtin {
		if real, ok := trustedUnixExecutable(fsys, p); ok {
			return program{path: real, name: p}, true
		}
	}
	return program{}, false
}

// windowsNvidiaSMI returns the nvidia-smi.exe to run on a Windows host, given
// the system directory (from GetSystemDirectory) and the Program Files
// directory (from the known-folder API), either of which may be "". It
// tries, in order:
//   - <system directory>\nvidia-smi.exe, where current (DCH) drivers put it;
//   - the newest <system directory>\DriverStore\FileRepository\nv*\nvidia-smi.exe,
//     libmamba's fallback, searched only when <system directory>\nvcuda.dll
//     shows an NVIDIA driver is installed (libmamba looks for nvcuda.dll on
//     PATH, which goomba does not read). Older driver packages stay in the
//     store, so the most recently modified nvidia-smi.exe is taken (ties by
//     name), not the first in directory order;
//   - <Program Files>\NVIDIA Corporation\NVSMI\nvidia-smi.exe, where drivers
//     before the DCH model installed it.
//
// These directories are writable only by administrators on a default
// Windows installation.
func windowsNvidiaSMI(fsys searchFS, sysDir, programFiles string) (string, bool) {
	if sysDir != "" {
		if p := winJoin(sysDir, "nvidia-smi.exe"); isRegularFile(fsys, p) {
			return p, true
		}
		if isRegularFile(fsys, winJoin(sysDir, "nvcuda.dll")) {
			if p, ok := driverStoreNvidiaSMI(fsys, winJoin(sysDir, "DriverStore", "FileRepository")); ok {
				return p, true
			}
		}
	}
	if programFiles != "" {
		if p := winJoin(programFiles, "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe"); isRegularFile(fsys, p) {
			return p, true
		}
	}
	return "", false
}

// driverStoreNvidiaSMI returns the most recently modified nvidia-smi.exe in
// the nv* subdirectories of the driver store repo, reading at most
// maxDriverStoreEntries names and examining at most
// maxDriverStoreCandidates directories.
func driverStoreNvidiaSMI(fsys searchFS, repo string) (string, bool) {
	names, err := fsys.readDirNames(repo, maxDriverStoreEntries)
	if err != nil && len(names) == 0 {
		return "", false
	}
	if len(names) > maxDriverStoreEntries {
		names = names[:maxDriverStoreEntries]
	}
	sort.Strings(names)
	best, bestTime, examined := "", time.Time{}, 0
	for _, name := range names {
		if len(name) < 2 || !strings.EqualFold(name[:2], "nv") || strings.ContainsAny(name, `\/`) {
			continue
		}
		if examined++; examined > maxDriverStoreCandidates {
			break
		}
		p := winJoin(repo, name, "nvidia-smi.exe")
		fi, err := fsys.stat(p)
		if err != nil || !fi.mode.IsRegular() {
			continue
		}
		if best == "" || fi.modTime.After(bestTime) {
			best, bestTime = p, fi.modTime
		}
	}
	return best, best != ""
}

// winJoin joins Windows path elements with backslashes, independent of the
// host's separator.
func winJoin(base string, elems ...string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, `\/`))
	for _, e := range elems {
		b.WriteByte('\\')
		b.WriteString(e)
	}
	return b.String()
}
