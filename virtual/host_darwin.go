//go:build darwin

// Ported from libmamba src/util/os_osx.cpp and rattler
// crates/rattler_virtual_packages/src/osx.rs (BSD-3-Clause, see NOTICE). Both
// ask the system for the product version (sw_vers, SystemVersion.plist);
// goomba reads the kern.osproductversion sysctl, available since macOS 10.13.4.

package virtual

import (
	"context"
	"strings"

	"golang.org/x/sys/unix"
)

// hostProbe reads facts from the running macOS machine.
type hostProbe struct{}

func (hostProbe) platform() string { return HostPlatform() }

func (hostProbe) linuxRelease() (string, bool) { return "", false }

func (hostProbe) glibcVersion(context.Context) (string, bool) { return "", false }

func (hostProbe) osxVersion() (string, bool) {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

func (hostProbe) winVersion() (string, bool) { return "", false }

func (hostProbe) archspec() string { return hostArchspec() }
