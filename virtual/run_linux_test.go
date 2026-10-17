//go:build linux

package virtual

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// script writes an executable /bin/sh script that uses only shell builtins
// (the scripts run with an empty environment).
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// scriptDir returns a directory whose scripts can be run, or skips.
func scriptDir(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	probe := script(t, dir, "probe", "printf ok")
	if res, ok := runCapped(context.Background(), runSpec{path: probe, dir: "/", timeout: 10 * time.Second, max: 16}); !ok || string(res.stdout) != "ok" {
		t.Skipf("cannot run scripts from %s", dir)
	}
	return dir
}

func TestRunCapped(t *testing.T) {
	dir := scriptDir(t)
	ctx := context.Background()
	cases := []struct {
		name string
		body string
		args []string
		max  int
		want string // "-" = not ok
		code int
	}{
		{"stdout only", "printf '<cuda_version>12.4</cuda_version>'\necho noise >&2", nil, 1024, "<cuda_version>12.4</cuda_version>", 0},
		{"arguments", `printf '%s|' "$@"`, []string{"--query", "-u", "-x"}, 1024, "--query|-u|-x|", 0},
		{"non-zero exit keeps output", "printf '8.6\\n7.5\\n'\nexit 3", nil, 1024, "8.6\n7.5\n", 3},
		{"non-zero exit without output", "exit 9", nil, 1024, "", 9},
		{"killed by a signal keeps output", "printf x\nkill -9 $$", nil, 1024, "x", -1},
		{"exactly the cap", "printf 12345678", nil, 8, "12345678", 0},
		{"one byte over the cap", "printf 123456789", nil, 8, "-", 0},
		{"stdin is empty", "read line\nprintf '[%s]' \"$line\"", nil, 1024, "[]", 0},
	}
	for _, c := range cases {
		p := script(t, dir, strings.ReplaceAll(c.name, " ", "-"), c.body)
		res, ok := runCapped(ctx, runSpec{path: p, args: c.args, dir: "/", timeout: 10 * time.Second, max: c.max})
		if c.want == "-" {
			if ok {
				t.Errorf("%s: ok with %q, want failure", c.name, res.stdout)
			}
			continue
		}
		if !ok || !res.started || string(res.stdout) != c.want || res.exitCode != c.code {
			t.Errorf("%s: %q, exit %d, %v; want %q, exit %d", c.name, res.stdout, res.exitCode, ok, c.want, c.code)
		}
	}

	// Endless output is cut off at the cap and stops the program early.
	flood := script(t, dir, "flood", "while :; do printf xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; done")
	start := time.Now()
	if res, ok := runCapped(ctx, runSpec{path: flood, dir: "/", timeout: 10 * time.Second, max: maxNvidiaSMIOutput}); ok {
		t.Errorf("flood: ok with %d bytes", len(res.stdout))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("flood took %v", d)
	}

	// A program that never finishes is killed at the timeout.
	hang := script(t, dir, "hang", "printf partial\nwhile :; do :; done")
	start = time.Now()
	if res, ok := runCapped(ctx, runSpec{path: hang, dir: "/", timeout: 300 * time.Millisecond, max: 1024}); ok || !res.started {
		t.Errorf("hang: %+v, %v; want a started run without a result", res, ok)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("hang took %v", d)
	}

	// A cancelled context runs nothing to completion.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if res, ok := runCapped(cctx, runSpec{path: script(t, dir, "fine", "printf 8.6"), dir: "/", timeout: 10 * time.Second, max: 1024}); ok {
		t.Errorf("cancelled: ok with %q", res.stdout)
	}

	// Programs that cannot be started.
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nprintf 8.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{plain, filepath.Join(dir, "missing"), dir} {
		if res, ok := runCapped(ctx, runSpec{path: p, dir: "/", timeout: 10 * time.Second, max: 1024}); ok || res.started {
			t.Errorf("%s: %+v, %v; want not started", filepath.Base(p), res, ok)
		}
	}
}

// TestHostRunNvidiaSMIEnvironment checks that the real probe runs nvidia-smi
// with nothing from goomba's environment (CUDA_VISIBLE_DEVICES included)
// and from the root directory.
func TestHostRunNvidiaSMIEnvironment(t *testing.T) {
	dir := scriptDir(t)
	t.Setenv("GOOMBA_TEST_LEAK", "secret")
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	p := script(t, dir, "env", `printf '%s|%s|%s|%s' "${GOOMBA_TEST_LEAK-unset}" "${CUDA_VISIBLE_DEVICES-unset}" "${HOME-unset}" "$(pwd)"`)
	out, ok := hostProbe{}.runNvidiaSMI(context.Background(), program{path: p, name: p})
	if want := "unset|unset|unset|/"; !ok || string(out) != want {
		t.Errorf("environment = %q, %v; want %q", out, ok, want)
	}
}

func TestHostFindNvidiaSMI(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nvidia-smi")
	if err := os.WriteFile(p, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	// A caller-chosen path need only be a regular file.
	if got, ok := (hostProbe{}).findNvidiaSMI(p); !ok || got != (program{path: p, name: p}) {
		t.Errorf("findNvidiaSMI(%q) = %q, %v", p, got, ok)
	}
	for _, bad := range []string{dir, filepath.Join(dir, "missing")} {
		if got, ok := (hostProbe{}).findNvidiaSMI(bad); ok {
			t.Errorf("findNvidiaSMI(%q) = %q", bad, got)
		}
	}
	// The real file system reports owners, so a file of ours is never
	// trusted as a system location.
	fi, err := osSearchFS{}.stat(p)
	if err != nil || !fi.mode.IsRegular() || fi.uid != uint32(os.Getuid()) {
		t.Errorf("stat(%q) = %+v, %v", p, fi, err)
	}
	if real, ok := trustedUnixExecutable(osSearchFS{}, p); ok && os.Getuid() != 0 {
		t.Errorf("trusted %q, a file owned by uid %d", real, os.Getuid())
	}
	if real, ok := trustedUnixExecutable(osSearchFS{}, "/"); ok {
		t.Errorf("trusted the root directory as %q", real)
	}
	// A link in our own directory to a system program is not trusted either:
	// we could retarget it after the check.
	if sh, ok := trustedUnixExecutable(osSearchFS{}, "/bin/sh"); ok && os.Getuid() != 0 {
		link := filepath.Join(dir, "sh")
		if err := os.Symlink(sh, link); err != nil {
			t.Fatal(err)
		}
		if real, ok := trustedUnixExecutable(osSearchFS{}, link); ok {
			t.Errorf("trusted %q through a link in a directory of uid %d", real, os.Getuid())
		}
		if !strings.HasPrefix(sh, "/") || strings.Contains(sh[1:], "//") {
			t.Errorf("resolved /bin/sh to %q", sh)
		}
		if real, err := filepath.EvalSymlinks("/bin/sh"); err != nil || real != sh {
			t.Errorf("resolved /bin/sh to %q, the kernel to %q (%v)", sh, real, err)
		}
	}
	if root, err := (osSearchFS{}).stat("/"); err != nil || !root.mode.IsDir() {
		t.Errorf("stat(/) = %+v, %v", root, err)
	}
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if names, err := (osSearchFS{}).readDirNames(dir, 3); err != nil || len(names) != 3 {
		t.Errorf("readDirNames(max 3) = %q, %v", names, err)
	}
	if _, err := (osSearchFS{}).readDirNames(filepath.Join(dir, "missing"), 3); err == nil {
		t.Error("readDirNames of a missing directory succeeded")
	}
}

// sleepPath returns a sleep program for scripts that need a child process,
// or skips.
func sleepPath(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/sleep", "/usr/bin/sleep"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no sleep program")
	return ""
}

// A program that exits but leaves a child holding its stdout: WaitDelay
// bounds the wait, and the output, which may be incomplete, is discarded.
func TestRunCappedChildHoldsPipe(t *testing.T) {
	dir := scriptDir(t)
	p := script(t, dir, "bg", sleepPath(t)+" 6 &\nprintf '<cuda_version>12.4</cuda_version>'")
	start := time.Now()
	res, ok := runCapped(context.Background(), runSpec{path: p, dir: "/", timeout: 10 * time.Second, max: 1024})
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("waited %v for a child holding the pipe", d)
	}
	if ok {
		t.Errorf("ok with %q although the pipe was still open", res.stdout)
	}
}

// Cancelling Detect while nvidia-smi runs (and its child holds the pipe)
// returns ctx.Err() promptly, through the real probe.
func TestDetectCancelDuringRun(t *testing.T) {
	dir := scriptDir(t)
	p := script(t, dir, "slow", sleepPath(t)+" 6")
	for _, mk := range []func() (context.Context, context.CancelFunc){
		func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(300*time.Millisecond, cancel)
			return ctx, cancel
		},
		func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		},
	} {
		ctx, cancel := mk()
		start := time.Now()
		recs, err := Detect(ctx, Options{NvidiaSMI: p})
		d := time.Since(start)
		cancel()
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Detect = %q, %v; want ctx.Err()", triples(recs), err)
		}
		// Prompt: no wait for the killed program (WaitDelay would be 1 s).
		if d > time.Second {
			t.Errorf("Detect took %v, cancelled after 300ms", d)
		}
	}
}

// awaitExit returns at once when the caller's ctx is done, and gives up on
// a program that cannot be killed (a process in uninterruptible sleep never
// lets Wait return) shortly after the run's timeout.
func TestAwaitExit(t *testing.T) {
	never := make(chan error)
	live := context.Background()
	cancelled, cancel := context.WithCancel(live)
	cancel()

	// The caller cancels: no grace period, even an hour long.
	start := time.Now()
	if finished, err := awaitExit(cancelled, cancelled, never, time.Hour); finished || err != nil {
		t.Errorf("cancelled: finished %v, %v", finished, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cancelled: returned after %v", d)
	}

	// The run times out and the program never exits: give up after grace.
	start = time.Now()
	if finished, err := awaitExit(live, cancelled, never, 50*time.Millisecond); finished || err != nil {
		t.Errorf("unkillable: finished %v, %v", finished, err)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 3*time.Second {
		t.Errorf("unkillable: gave up after %v, want about 50ms", d)
	}

	// The caller cancels during the grace period: return at once.
	parent, cancelParent := context.WithCancel(live)
	time.AfterFunc(50*time.Millisecond, cancelParent)
	start = time.Now()
	if finished, _ := awaitExit(parent, cancelled, never, time.Hour); finished {
		t.Error("cancelled during grace: finished")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("cancelled during grace: returned after %v", d)
	}

	// A program that exits before anything is done, or within grace.
	done := make(chan error, 1)
	done <- errors.New("exit status 3")
	if finished, err := awaitExit(live, live, done, time.Hour); !finished || err == nil {
		t.Errorf("exited: finished %v, %v", finished, err)
	}
	late := make(chan error, 1)
	time.AfterFunc(20*time.Millisecond, func() { late <- nil })
	if finished, err := awaitExit(live, cancelled, late, 5*time.Second); !finished || err != nil {
		t.Errorf("exited within grace: finished %v, %v", finished, err)
	}
}

// Through the real probe: nvidia-smi exiting non-zero with usable output
// still counts, for both queries.
func TestDetectNonZeroExit(t *testing.T) {
	dir := scriptDir(t)
	p := script(t, dir, "smi", `case "$1" in
--query) printf '<nvidia_smi_log><cuda_version>12.8</cuda_version></nvidia_smi_log>'; exit 15;;
*) printf '8.9\n[Unknown Error]\n'; exit 15;;
esac`)
	recs, err := Detect(context.Background(), Options{NvidiaSMI: p})
	if err != nil {
		t.Fatal(err)
	}
	if r := find(recs, "__cuda"); r == nil || r.Version != "12.8" {
		t.Errorf("__cuda = %v", r)
	}
	if r := find(recs, "__cuda_arch"); r == nil || r.Version != "8.9" {
		t.Errorf("__cuda_arch = %v", r)
	}
}

// Concurrent Detect calls through the real probe (gate.sh runs -race).
func TestDetectConcurrentRuns(t *testing.T) {
	dir := scriptDir(t)
	p := script(t, dir, "smi", `case "$1" in
--query) printf '<cuda_version>12.8</cuda_version>';;
*) printf '8.9\n7.5\n';;
esac`)
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs, err := Detect(context.Background(), Options{NvidiaSMI: p})
			if r := find(recs, "__cuda_arch"); err != nil || r == nil || r.Version != "7.5" {
				errs <- fmt.Sprintf("Detect = %q, %v", triples(recs), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestHelperPrintArgv0 is not a test. Run as a child with the last argument
// "print-argv0", it prints its argv[0] and exits; it lets tests see the
// name a program was given, which a #! script cannot (the kernel replaces
// argv[0] with the script's path).
func TestHelperPrintArgv0(t *testing.T) {
	if args := flag.Args(); len(args) == 0 || args[len(args)-1] != "print-argv0" {
		t.Skip("helper process only")
	}
	os.Stdout.WriteString(os.Args[0])
	os.Exit(0)
}

// The runner executes the resolved file but gives the program the name of
// the location it was found at, so a multi-call binary (busybox, toybox)
// reached through a link dispatches correctly.
func TestRunCappedArgv0(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	helper := []string{"-test.run=^TestHelperPrintArgv0$", "--", "print-argv0"}
	for _, c := range []struct{ argv0, want string }{
		{"/usr/bin/nvidia-smi", "/usr/bin/nvidia-smi"},
		{"", exe},
	} {
		res, ok := runCapped(context.Background(), runSpec{
			path: exe, argv0: c.argv0, args: helper, dir: "/", timeout: 30 * time.Second, max: 4096,
		})
		if !ok || string(res.stdout) != c.want {
			t.Errorf("argv0 %q: %q, %v; want %q", c.argv0, res.stdout, ok, c.want)
		}
	}
	out, ok := hostProbe{}.runNvidiaSMI(context.Background(), program{path: exe, name: "/usr/local/nvidia/bin/nvidia-smi"}, helper...)
	if want := "/usr/local/nvidia/bin/nvidia-smi"; !ok || string(out) != want {
		t.Errorf("runNvidiaSMI argv[0] = %q, %v; want %q", out, ok, want)
	}
}
