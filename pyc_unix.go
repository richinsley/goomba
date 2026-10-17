//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package goomba

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts cmd in its own process group and makes
// cancellation kill the whole group, so that compile_pyc.py's worker
// processes die with the interpreter.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
