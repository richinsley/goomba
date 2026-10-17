//go:build linux || windows

package virtual

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

const (
	// runWaitDelay bounds the wait for a program's output pipe after the
	// program exits or is killed, in case something it started keeps the
	// pipe open (exec.Cmd.WaitDelay).
	runWaitDelay = time.Second
	// runGrace is how long, beyond runWaitDelay, a run waits for a killed
	// program to exit before giving up on it.
	runGrace = time.Second
)

// osSearchFS is the real file system for the program searches.
type osSearchFS struct{}

func (osSearchFS) stat(name string) (fileFacts, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return fileFacts{}, err
	}
	return fileFacts{mode: fi.Mode(), uid: fileOwner(fi), modTime: fi.ModTime()}, nil
}

func (osSearchFS) lstat(name string) (fileFacts, error) {
	fi, err := os.Lstat(name)
	if err != nil {
		return fileFacts{}, err
	}
	return fileFacts{mode: fi.Mode(), uid: fileOwner(fi), modTime: fi.ModTime()}, nil
}

func (osSearchFS) readlink(name string) (string, error) { return os.Readlink(name) }

func (osSearchFS) readDirNames(dir string, max int) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(max)
}

// errOutputTooLarge stops reading a program's output once it passes the cap.
var errOutputTooLarge = errors.New("output too large")

// cappedBuffer keeps a program's output up to max bytes and fails the write
// that would pass it, which closes the pipe so the program stops early.
type cappedBuffer struct {
	buf      []byte
	max      int
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > c.max-len(c.buf) {
		c.overflow = true
		return 0, errOutputTooLarge
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// runResult is the outcome of a run. stdout and exitCode are set only when
// the program ran to completion.
type runResult struct {
	stdout   []byte
	exitCode int  // 0 for success, -1 when killed by a signal
	started  bool // the program was started, whether or not it completed
}

// runFunc runs a program as runCapped does; tests substitute fakes.
type runFunc func(context.Context, runSpec) (runResult, bool)

// runSpec describes one run of a program.
type runSpec struct {
	path    string        // absolute path of the file to execute
	argv0   string        // argv[0] for the program; "" means path
	args    []string      // the arguments after argv[0]
	env     []string      // the whole environment; nil means none
	dir     string        // the working directory
	timeout time.Duration // the most the run may take
	max     int           // the most stdout kept; more fails the run
}

// runCapped runs the program s describes, with exactly the environment
// s.env (never the caller's), and returns its stdout and exit status. stdin
// is empty and stderr is discarded. It reports false when the program could
// not be started, did not finish within s.timeout or before ctx was done,
// wrote more than s.max bytes, or left its output pipe open after exiting;
// the caller checks ctx. A non-zero exit status is still a result:
// nvidia-smi exits non-zero on degraded multi-GPU systems while printing
// usable output (rattler parses such output too).
//
// When ctx is done, runCapped returns at once: os/exec kills the program,
// and a goroutine waits for it and ends when it has exited. When s.timeout
// passes, the program is killed and runCapped waits for it up to
// runWaitDelay plus runGrace (two seconds), so normally nothing is left
// behind. A program that cannot be killed, as a process in uninterruptible
// sleep (a GPU that has fallen off the bus can do this to nvidia-smi), is
// abandoned then: until the process finally exits, two goroutines stay
// blocked, the one waiting in exec.Cmd.Wait and os/exec's own context
// watcher.
func runCapped(ctx context.Context, s runSpec) (runResult, bool) {
	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.path, s.args...)
	if s.argv0 != "" {
		cmd.Args[0] = s.argv0
	}
	cmd.Env = s.env
	if cmd.Env == nil {
		cmd.Env = []string{} // nil would mean the caller's environment
	}
	cmd.Dir = s.dir
	out := &cappedBuffer{max: s.max}
	cmd.Stdout = out
	cmd.WaitDelay = runWaitDelay
	if err := cmd.Start(); err != nil {
		return runResult{}, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished, err := awaitExit(ctx, runCtx, done, runWaitDelay+runGrace)
	// When Wait has returned, its output goroutines have too, so out is
	// safe to read; when it has not, out is never read.
	if !finished || runCtx.Err() != nil || out.overflow {
		return runResult{started: true}, false
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return runResult{started: true}, false
		}
		return runResult{stdout: out.buf, exitCode: ee.ExitCode(), started: true}, true
	}
	return runResult{stdout: out.buf, started: true}, true
}

// awaitExit returns the error sent on done, the result of exec.Cmd.Wait,
// with finished = true. When the caller's ctx is done it returns at once
// with finished = false and leaves the wait to the goroutine sending on
// done. When only run (the run's timeout, derived from ctx) is done,
// exec.CommandContext has killed the program and Wait returns within its
// WaitDelay unless the program cannot die, so awaitExit waits at most grace
// longer before reporting finished = false.
func awaitExit(ctx, run context.Context, done <-chan error, grace time.Duration) (finished bool, err error) {
	select {
	case err := <-done:
		return true, err
	case <-ctx.Done():
		return false, nil
	case <-run.Done():
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case err := <-done:
		return true, err
	case <-ctx.Done():
		return false, nil
	case <-t.C:
		return false, nil
	}
}
