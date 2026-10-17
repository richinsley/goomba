// Byte-compilation of noarch: python sources, ported from libmamba
// src/core/transaction_context.cpp (TransactionContext::
// start_pyc_compilation_process, try_pyc_compilation,
// wait_for_pyc_compilation) (BSD-3-Clause, see NOTICE).

package goomba

import (
	"bytes"
	"context"
	_ "embed" // compile_pyc.py
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/richinsley/goomba/link"
)

// compilePycScript is libmamba's data/compile_pyc.py: it reads source paths
// from stdin, one per line, and byte-compiles them with compileall in a
// process pool of MAMBA_EXTRACT_THREADS workers. It exits 1 if any file
// failed to compile.
//
//go:embed compile_pyc.py
var compilePycScript string

// maxPycOutput bounds the interpreter output kept for error messages.
const maxPycOutput = 64 << 10

// pycResult describes a finished byte-compilation.
type pycResult struct {
	compiled int // .pyc files present afterwards
	total    int // sources passed to the interpreter
}

// compilePyc byte-compiles sources (prefix-relative .py paths) by running
// the environment's own interpreter as libmamba does, but isolated so that
// no package code runs: from a fresh private directory inside conda-meta as
// working directory, never the prefix, with no environment variables but
// MAMBA_EXTRACT_THREADS (libmamba's reproc::env::empty plus that one), as
//
//	<prefix>/<python> -I -S -Wi -u <dir>/compile_pyc.py
//
// with the sources' absolute paths on stdin. -I ignores PYTHON* variables
// and the user's site-packages and keeps the script's directory off
// sys.path; -S skips the site module, so no .pth file or sitecustomize of a
// locked package is executed. Before Python 3.6 libmamba's fallback runs
// instead, "<python> <flags> -Wi -m compileall -q -l -i -", with the same
// flags from 3.4 and "-E -s -S" before (no -I). The version is the
// lockfile's python record.
// Unlike libmamba, the command is not wrapped in a shell script that
// activates the prefix, and the script lives in the private directory
// rather than the shared temporary directory. The directory is removed
// afterwards.
//
// This is the only process goomba runs. The interpreter is the one just
// linked from an archive verified against the lockfile, checked again by
// verifyInterpreter just before. If ctx is cancelled the interpreter and its
// worker processes are killed.
//
// A run that exits with status 1 had some files fail to compile (or the
// script fail); libmamba logs this and carries on, and so does goomba: the
// result counts the .pyc files that exist. Any other failure to run the
// interpreter is an ErrCompilePyc error.
func compilePyc(ctx context.Context, prefix string, py *link.PythonInfo, sources []string, threads int) (pycResult, error) {
	res := pycResult{total: len(sources)}
	if len(sources) == 0 {
		return res, nil
	}
	fail := func(err error) error { return fmt.Errorf("goomba: compile .pyc: %w", err) }
	exe := filepath.Join(prefix, filepath.FromSlash(py.Executable))
	if fi, err := os.Stat(exe); err != nil {
		return res, fail(fmt.Errorf("%w: %v", ErrCompilePyc, err))
	} else if !fi.Mode().IsRegular() {
		return res, fail(fmt.Errorf("%w: %s is not a regular file", ErrCompilePyc, py.Executable))
	}
	dir, err := os.MkdirTemp(filepath.Join(prefix, "conda-meta"), ".goomba-pyc-")
	if err != nil {
		return res, fail(err)
	}
	defer os.RemoveAll(dir)
	isolation := []string{"-I", "-S"}
	if !pythonAtLeast(py.Version, 3, 4) {
		isolation = []string{"-E", "-s", "-S"}
	}
	args := append(isolation, "-Wi", "-m", "compileall", "-q", "-l", "-i", "-")
	if pythonAtLeast(py.Version, 3, 6) {
		script := filepath.Join(dir, "compile_pyc.py")
		if err := os.WriteFile(script, []byte(compilePycScript), 0o600); err != nil {
			return res, fail(err)
		}
		args = append(isolation, "-Wi", "-u", script)
	}
	var stdin strings.Builder
	for _, s := range sources {
		stdin.WriteString(filepath.Join(prefix, filepath.FromSlash(s)))
		stdin.WriteByte('\n')
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Env = []string{"MAMBA_EXTRACT_THREADS=" + strconv.Itoa(threads)}
	cmd.Stdin = strings.NewReader(stdin.String())
	var out outputBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 10 * time.Second
	setProcessGroup(cmd)

	err = cmd.Run()
	if cerr := ctx.Err(); cerr != nil {
		return res, fail(cerr)
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		// Some sources did not compile; counted below.
	case errors.As(err, &ee):
		return res, fail(fmt.Errorf("%w: %s: %v: %s", ErrCompilePyc, py.Executable, err, out.text()))
	default:
		return res, fail(fmt.Errorf("%w: %s: %v", ErrCompilePyc, py.Executable, err))
	}
	seen := map[string]bool{}
	for _, s := range sources {
		pyc := filepath.Join(prefix, filepath.FromSlash(link.PycPath(s, py.Version)))
		if _, err := os.Lstat(pyc); err == nil {
			res.compiled++
		}
		// The interpreter creates __pycache__ directories as the umask
		// allows; like the rest of the prefix they lose group and other
		// write.
		if dir := filepath.Dir(pyc); !seen[dir] {
			seen[dir] = true
			if fi, err := os.Lstat(dir); err == nil && fi.IsDir() && fi.Mode().Perm()&0o022 != 0 {
				if err := os.Chmod(dir, fi.Mode().Perm()&^0o022); err != nil {
					return res, fail(err)
				}
			}
		}
	}
	return res, nil
}

// pythonAtLeast reports whether the short version ("3.11", "3.13t") is at
// least major.minor.
func pythonAtLeast(short string, major, minor int) bool {
	maj, mnr, ok := strings.Cut(strings.TrimSuffix(short, "t"), ".")
	if !ok {
		return false
	}
	a, err1 := strconv.Atoi(maj)
	b, err2 := strconv.Atoi(mnr)
	return err1 == nil && err2 == nil && (a > major || a == major && b >= minor)
}

// outputBuffer keeps the first maxPycOutput bytes written to it.
type outputBuffer struct {
	buf bytes.Buffer
}

func (t *outputBuffer) Write(p []byte) (int, error) {
	if room := maxPycOutput - t.buf.Len(); room > 0 {
		t.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// text returns the end of the kept output, at most 1 KiB, trimmed and with
// control characters other than newline and tab replaced, for an error
// message.
func (t *outputBuffer) text() string {
	s := strings.TrimSpace(t.buf.String())
	if len(s) > 1024 {
		s = s[len(s)-1024:]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
