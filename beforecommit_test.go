package goomba

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richinsley/goomba/lock"
)

// hookError is a typed error a BeforeCommit hook returns, for errors.As.
type hookError struct{ step string }

func (e *hookError) Error() string { return "pip " + e.step + " failed" }

// kindLog records the kinds of the events it receives.
type kindLog struct {
	mu    sync.Mutex
	kinds []EventKind
}

func (l *kindLog) add(e Event) {
	l.mu.Lock()
	l.kinds = append(l.kinds, e.Kind)
	l.mu.Unlock()
}

func (l *kindLog) snapshot() []EventKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.kinds)
}

// TestBeforeCommit (WP8d): the caller's hook runs once, last, inside the
// transaction: under the prefix lock, with the marker present and the
// prefix still 0700, after the history entry; its error, a cancellation or
// a panic rolls everything back, including what it wrote.
func TestBeforeCommit(t *testing.T) {
	if len(flag.Args()) > 0 {
		t.Skip("in a helper process")
	}
	c := newChannel(t)
	lf := c.lockfile(t, []string{"pip"}, cancelEnv(t, fakePython(t, t.TempDir(), 0, "pip"))...)

	t.Run("runs once, inside the transaction", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		real, err := filepath.EvalSymlinks(dir)
		must(t, err)
		want := filepath.Join(real, "env")
		opt := c.options(t)
		var log kindLog
		opt.Progress = log.add
		calls := 0
		opt.BeforeCommit = func(ctx context.Context, prefix string) error {
			calls++
			if prefix != want {
				t.Errorf("hook got prefix %q, want the resolved %q", prefix, want)
			}
			// The marker is present, the history entry written, every
			// package recorded, symlink digests taken from the
			// finished tree, the .pyc files compiled.
			for _, p := range []string{
				"conda-meta/" + incompleteMarker, "conda-meta/history", "conda-meta/pip-1-0.json", "lib/a.so",
				"lib/python3.11/site-packages/pip/__pycache__/__init__.cpython-311.pyc",
			} {
				if _, err := os.Lstat(filepath.Join(prefix, p)); err != nil {
					t.Errorf("in the hook: %v", err)
				}
			}
			hist, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "history"))
			if err != nil || !strings.Contains(string(hist), "# update specs: [\"pip\"]") {
				t.Errorf("history in the hook: %q, %v", hist, err)
			}
			aaa := readCondaMetaJSON(t, prefix, "aaa-1-0")
			for _, x := range aaa["paths_data"].(map[string]any)["paths"].([]any) {
				m := x.(map[string]any)
				if sum := sha256.Sum256([]byte("a")); m["_path"] == "lib/a.so" && m["sha256_in_prefix"] != hex.EncodeToString(sum[:]) {
					t.Errorf("symlink digest in the hook: %v", m["sha256_in_prefix"])
				}
			}
			// Everyone else sees an installation in progress.
			if runtime.GOOS != "windows" {
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := CheckComplete(prefix); !errors.Is(err, ErrPrefixLocked) {
						t.Errorf("CheckComplete from another goroutine: %v, want ErrPrefixLocked", err)
					}
					if err := RemoveIncomplete(prefix); !errors.Is(err, ErrPrefixLocked) {
						t.Errorf("RemoveIncomplete from another goroutine: %v, want ErrPrefixLocked", err)
					}
				}()
				wg.Wait()
				if fi, err := os.Stat(prefix); err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("prefix mode in the hook: %v, %v; want 0700", fi.Mode(), err)
				}
			}
			kinds := log.snapshot()
			if len(kinds) == 0 || kinds[len(kinds)-1] != EventBeforeCommit || !slices.Contains(kinds, EventCompiled) || slices.Contains(kinds, EventDone) {
				t.Errorf("events before the hook: %v", kinds)
			}
			return nil
		}
		must(t, InstallLock(context.Background(), "env", lf, opt))
		if calls != 1 {
			t.Errorf("hook called %d times", calls)
		}
		if err := CheckComplete(want); err != nil {
			t.Errorf("CheckComplete after the commit: %v", err)
		}
		if runtime.GOOS != "windows" {
			if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o755 {
				t.Errorf("prefix mode after the commit: %v, %v", fi.Mode(), err)
			}
		}
		if kinds := log.snapshot(); kinds[len(kinds)-1] != EventDone || kinds[len(kinds)-2] != EventBeforeCommit {
			t.Errorf("events: %v", kinds)
		}
	})

	t.Run("an error rolls back", func(t *testing.T) {
		errPip := errors.New("pip install failed")
		for _, existing := range []bool{false, true} {
			prefix := filepath.Join(t.TempDir(), "env")
			if existing {
				must(t, os.Mkdir(prefix, 0o750))
				must(t, os.Chmod(prefix, 0o750))
			}
			opt := c.options(t)
			opt.BeforeCommit = func(context.Context, string) error { return fmt.Errorf("restore: %w", errPip) }
			err := InstallLock(context.Background(), prefix, lf, opt)
			if !errors.Is(err, errPip) || !strings.Contains(err.Error(), "before commit") {
				t.Errorf("err = %v, want it to wrap the hook's error", err)
			}
			if existing {
				fi, serr := os.Stat(prefix)
				ents, _ := os.ReadDir(prefix)
				if serr != nil || len(ents) != 0 || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o750 {
					t.Errorf("existing prefix after the rollback: %v %v %v; want it empty, mode 0750", fi, ents, serr)
				}
			} else {
				assertNoPrefix(t, prefix)
			}
		}
		// errors.As reaches a typed error.
		prefix := filepath.Join(t.TempDir(), "env")
		opt := c.options(t)
		opt.BeforeCommit = func(context.Context, string) error { return &hookError{step: "check"} }
		var he *hookError
		if err := InstallLock(context.Background(), prefix, lf, opt); !errors.As(err, &he) || he.step != "check" {
			t.Errorf("err = %v, want a *hookError", err)
		}
		assertNoPrefix(t, prefix)
	})

	t.Run("what the hook wrote is removed", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "keep")
		must(t, os.WriteFile(outside, []byte("k"), 0o644))
		prefix := filepath.Join(t.TempDir(), "env")
		opt := c.options(t)
		opt.BeforeCommit = func(_ context.Context, prefix string) error {
			deep := filepath.Join(prefix, "lib", "python3.11", "site-packages", "extra", "sub")
			must(t, os.MkdirAll(deep, 0o755))
			must(t, os.WriteFile(filepath.Join(deep, "mod.py"), []byte("x"), 0o444))
			must(t, os.Chmod(filepath.Dir(deep), 0o555))
			must(t, os.WriteFile(filepath.Join(prefix, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755))
			if runtime.GOOS != "windows" {
				must(t, os.Symlink(filepath.Dir(outside), filepath.Join(prefix, "out")))
			}
			return errors.New("check failed")
		}
		if err := InstallLock(context.Background(), prefix, lf, opt); err == nil {
			t.Fatal("no error")
		}
		assertNoPrefix(t, prefix)
		if data, err := os.ReadFile(outside); err != nil || string(data) != "k" {
			t.Errorf("data outside: %q, %v", data, err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		// Cancelled while the hook waits on ctx.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		prefix := filepath.Join(t.TempDir(), "env")
		opt := c.options(t)
		started := make(chan struct{})
		opt.BeforeCommit = func(ctx context.Context, _ string) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		go func() {
			<-started
			cancel()
		}()
		if err := InstallLock(ctx, prefix, lf, opt); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		assertNoPrefix(t, prefix)
		// Cancelled while the hook runs, which then succeeds anyway.
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		prefix = filepath.Join(t.TempDir(), "env")
		opt.BeforeCommit = func(context.Context, string) error {
			cancel()
			return nil
		}
		if err := InstallLock(ctx, prefix, lf, opt); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		assertNoPrefix(t, prefix)
		// A Progress panic at EventBeforeCommit cancels before the hook.
		prefix = filepath.Join(t.TempDir(), "env")
		opt.Progress = func(e Event) {
			if e.Kind == EventBeforeCommit {
				panic("progress")
			}
		}
		called := false
		opt.BeforeCommit = func(context.Context, string) error { called = true; return nil }
		func() {
			defer func() {
				if r := recover(); r != "progress" {
					t.Errorf("recovered %v, want the Progress panic", r)
				}
			}()
			_ = InstallLock(context.Background(), prefix, lf, opt)
		}()
		if called {
			t.Error("hook called after Progress panicked")
		}
		assertNoPrefix(t, prefix)
	})

	t.Run("panic", func(t *testing.T) {
		prefix := filepath.Join(t.TempDir(), "env")
		opt := c.options(t)
		opt.BeforeCommit = func(context.Context, string) error { panic("hook panicked") }
		func() {
			defer func() {
				if r := recover(); r != "hook panicked" {
					t.Errorf("recovered %v, want the hook's panic", r)
				}
			}()
			_ = InstallLock(context.Background(), prefix, lf, opt)
			t.Error("InstallLock returned")
		}()
		assertNoPrefix(t, prefix)
	})

	t.Run("other processes", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no prefix lock")
		}
		// While the hook runs, another process gets ErrPrefixLocked from
		// CheckComplete and RemoveIncomplete, and InstallLock for the same
		// prefix fails (ErrPrefixNotEmpty: it checks for an empty prefix
		// before it downloads anything, so before it would meet the lock).
		var lfFile bytes.Buffer
		must(t, lf.Write(&lfFile))
		lfPath := filepath.Join(t.TempDir(), "lock.json")
		must(t, os.WriteFile(lfPath, lfFile.Bytes(), 0o644))
		prefix := filepath.Join(t.TempDir(), "env")
		opt := c.options(t)
		var out []byte
		opt.BeforeCommit = func(ctx context.Context, prefix string) error {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBeforeCommitHelper$", "--", commitHelperArg, prefix, lfPath, t.TempDir())
			var err error
			out, err = cmd.CombinedOutput()
			return err
		}
		must(t, InstallLock(context.Background(), prefix, lf, opt))
		t.Logf("%s", out)
		for _, want := range []string{"CheckComplete: locked", "RemoveIncomplete: locked", "InstallLock: refused"} {
			if !strings.Contains(string(out), want+"\n") {
				t.Errorf("helper output lacks %q:\n%s", want, out)
			}
		}
		if err := CheckComplete(prefix); err != nil {
			t.Errorf("after the commit: %v", err)
		}
	})
}

// commitHelperArg marks the command line of TestBeforeCommitHelper:
// "<test binary> -test.run=^TestBeforeCommitHelper$ -- goomba-commit-helper
// <prefix> <lockfile> <pkgs dir>".
const commitHelperArg = "goomba-commit-helper"

// TestBeforeCommitHelper is the helper process of TestBeforeCommit's
// "other processes": it reports what CheckComplete, RemoveIncomplete and
// InstallLock say about a prefix whose BeforeCommit hook is running.
func TestBeforeCommitHelper(t *testing.T) {
	args := flag.Args()
	if len(args) != 4 || args[0] != commitHelperArg {
		t.Skip("helper process of TestBeforeCommit")
	}
	prefix, lfPath, pkgs := args[1], args[2], args[3]
	kind := func(err error) string {
		switch {
		case err == nil:
			return "nil"
		case errors.Is(err, ErrPrefixLocked):
			return "locked"
		case errors.Is(err, ErrPrefixNotEmpty):
			return "refused"
		case errors.Is(err, fs.ErrNotExist):
			return "notexist"
		}
		return "other " + err.Error()
	}
	fmt.Printf("CheckComplete: %s\n", kind(CheckComplete(prefix)))
	fmt.Printf("RemoveIncomplete: %s\n", kind(RemoveIncomplete(prefix)))
	f, err := os.Open(lfPath)
	if err != nil {
		fmt.Println("open lockfile:", err)
		os.Exit(1)
	}
	lf, err := lock.Read(f)
	f.Close()
	if err != nil {
		fmt.Println("read lockfile:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err = InstallLock(ctx, prefix, lf, InstallOptions{PkgsDir: pkgs, Virtual: testVirtual(t)})
	fmt.Printf("InstallLock: %s\n", kind(err))
	os.Exit(0)
}

// TestCreateBeforeCommit (WP8d): Create passes the hook to InstallLock.
func TestCreateBeforeCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake interpreter is a shell script")
	}
	s := newSpecServer(t)
	ch := s.channel("cf")
	ch.add(t, fakePython(t, t.TempDir(), 0))
	ch.publishSharded(t, hostPlatform(t))
	ctx := ctxT(t)
	opt := s.lockOptions(t, []string{"python=3.11"}, ch)
	errCheck := errors.New("python --version failed")
	for _, fail := range []bool{true, false} {
		prefix := filepath.Join(t.TempDir(), "env")
		calls := 0
		inst := InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client(), BeforeCommit: func(_ context.Context, p string) error {
			calls++
			if _, err := os.Stat(filepath.Join(p, "bin", "python3.11")); err != nil {
				return err
			}
			if fail {
				return errCheck
			}
			return nil
		}}
		_, err := Create(ctx, prefix, opt, inst)
		switch {
		case calls != 1:
			t.Errorf("fail=%v: hook called %d times", fail, calls)
		case fail && !errors.Is(err, errCheck):
			t.Errorf("Create: %v, want the hook's error", err)
		case fail:
			assertNoPrefix(t, prefix)
		case err != nil:
			t.Errorf("Create: %v", err)
		}
	}
}
