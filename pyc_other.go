//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package goomba

import "os/exec"

// setProcessGroup leaves cmd unchanged: cancellation kills the interpreter
// only.
func setProcessGroup(cmd *exec.Cmd) {}
