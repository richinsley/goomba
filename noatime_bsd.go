//go:build darwin || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package goomba

// oNoatime is 0: this platform has no O_NOATIME.
const oNoatime = 0
