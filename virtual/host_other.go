//go:build !linux && !darwin && !windows

package virtual

import "context"

// hostProbe reports no OS facts on platforms goomba has no detection for;
// Detect still yields __unix and __archspec where the platform carries them.
type hostProbe struct{}

func (hostProbe) platform() string { return HostPlatform() }

func (hostProbe) linuxRelease() (string, bool) { return "", false }

func (hostProbe) glibcVersion(context.Context) (string, bool) { return "", false }

func (hostProbe) osxVersion() (string, bool) { return "", false }

func (hostProbe) winVersion() (string, bool) { return "", false }

func (hostProbe) archspec() string { return hostArchspec() }
