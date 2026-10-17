//go:build windows

// Ported from libmamba src/util/os_win.cpp and src/core/virtual_packages.cpp
// and rattler crates/rattler_virtual_packages/src/win.rs (BSD-3-Clause, see
// NOTICE). libmamba parses the output of `cmd /c ver`; goomba asks
// RtlGetVersion for the same major.minor.build triple without running a
// command.

package virtual

import (
	"context"
	"io/fs"
	"math"
	"strconv"

	"golang.org/x/sys/windows"
)

// hostProbe reads facts from the running Windows machine.
type hostProbe struct{}

func (hostProbe) platform() string { return HostPlatform() }

func (hostProbe) linuxRelease() (string, bool) { return "", false }

func (hostProbe) glibcVersion(context.Context) (string, bool) { return "", false }

func (hostProbe) osxVersion() (string, bool) { return "", false }

func (hostProbe) winVersion() (string, bool) {
	v := windows.RtlGetVersion()
	if v == nil || v.MajorVersion == 0 {
		return "", false
	}
	return strconv.FormatUint(uint64(v.MajorVersion), 10) + "." +
		strconv.FormatUint(uint64(v.MinorVersion), 10) + "." +
		strconv.FormatUint(uint64(v.BuildNumber), 10), true
}

func (hostProbe) archspec() string { return hostArchspec() }

func (hostProbe) findNvidiaSMI(explicit string) (program, bool) {
	fsys := osSearchFS{}
	if explicit != "" {
		return program{path: explicit, name: explicit}, isRegularFile(fsys, explicit)
	}
	sysDir, err := windows.GetSystemDirectory()
	if err != nil {
		sysDir = ""
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		programFiles = ""
	}
	p, ok := windowsNvidiaSMI(fsys, sysDir, programFiles)
	return program{path: p, name: p}, ok
}

// runNvidiaSMI runs nvidia-smi with only SystemRoot in its environment, set
// from GetSystemWindowsDirectory (many Windows programs fail without it, and
// os/exec would otherwise copy it from goomba's environment), in the system
// directory, so that DLLs are not loaded from goomba's working directory.
func (hostProbe) runNvidiaSMI(ctx context.Context, prog program, args ...string) ([]byte, bool) {
	winDir, err := windows.GetSystemWindowsDirectory()
	if err != nil || winDir == "" {
		return nil, false
	}
	sysDir, err := windows.GetSystemDirectory()
	if err != nil || sysDir == "" {
		return nil, false
	}
	res, ok := runCapped(ctx, runSpec{
		path: prog.path, argv0: prog.name, args: args, env: []string{"SystemRoot=" + winDir}, dir: sysDir,
		timeout: nvidiaSMITimeout, max: maxNvidiaSMIOutput,
	})
	return res.stdout, ok
}

// fileOwner is unused on Windows, where the nvidia-smi locations are
// protected by the system's ACLs; it reports no owner (never root).
func fileOwner(fs.FileInfo) uint32 { return math.MaxUint32 }
