//go:build windows

// Ported from libmamba src/util/os_win.cpp and rattler
// crates/rattler_virtual_packages/src/win.rs (BSD-3-Clause, see NOTICE).
// libmamba parses the output of `cmd /c ver`; goomba asks RtlGetVersion for
// the same major.minor.build triple without running a command.

package virtual

import (
	"context"
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
