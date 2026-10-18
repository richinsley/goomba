//go:build !windows && !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package goomba

import "os"

// openDirNoFollow opens the directory dir; this platform offers no way to
// refuse a final symlink.
func openDirNoFollow(dir string) (*os.File, error) {
	return os.Open(dir)
}
