package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/virtual"
)

// fakeDetect stands in for virtual.Detect: the platform's defaults with
// the overrides applied as Detect applies them.
func fakeDetect(_ context.Context, o virtual.Options) ([]*record.PackageRecord, error) {
	return virtual.ApplyOverrides(o.Platform, virtual.Defaults(o.Platform), o.Overrides)
}

func TestDefaultCacheRequired(t *testing.T) {
	lockFile, srv, _ := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	c := &cli{
		client:   func() (*http.Client, error) { return srv.Client(), nil },
		cacheDir: func() (string, error) { return "", errors.New("neither $XDG_CACHE_HOME nor $HOME are defined") },
		virtual:  fakeDetect,
	}
	code, _, stderr := runCLI(t, c, "create", "--lock", lockFile, "-p", filepath.Join(t.TempDir(), "env"))
	if code != exitUsage || !strings.Contains(stderr, "--pkgs-dir is required") {
		t.Errorf("exit %d: %s", code, stderr)
	}
	code, _, stderr = runCLI(t, c, "create", "--lock", lockFile, "-p", filepath.Join(t.TempDir(), "env"), "--pkgs-dir", filepath.Join(t.TempDir(), "pkgs"))
	if code != exitOK {
		t.Errorf("with --pkgs-dir: exit %d: %s", code, stderr)
	}
}

// TestVirtualOverrides: CONDA_OVERRIDE_* and --virtual-override adjust the
// virtual packages checked (flags win), --virtual-from-lock checks the
// lockfile's recorded set, and a lockfile recorded for other virtual
// packages draws a warning.
func TestVirtualOverrides(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test package needs __unix")
	}
	lockFile, srv, lf := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	// The same lockfile, recording the virtual packages it was solved for.
	recorded := *lf
	recorded.Virtual = virtual.ToOverrides(virtual.Defaults(lf.Platform))
	// __unix exists on both Linux and macOS. Make the recorded set differ
	// from Defaults on either host; changing __glibc alone misses macOS.
	recorded.Virtual["__unix"] = "1"
	for k := range recorded.Virtual {
		if k == "__glibc" {
			recorded.Virtual[k] = "2.99"
		}
	}
	var buf bytes.Buffer
	if err := recorded.Write(&buf); err != nil {
		t.Fatal(err)
	}
	recordedFile := filepath.Join(t.TempDir(), "recorded.lock.json")
	if err := os.WriteFile(recordedFile, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// Solved for the platform's conservative defaults, as for any machine.
	defaults := *lf
	defaults.Virtual = virtual.ToOverrides(virtual.Defaults(lf.Platform))
	buf.Reset()
	if err := defaults.Write(&buf); err != nil {
		t.Fatal(err)
	}
	defaultsFile := filepath.Join(t.TempDir(), "defaults.lock.json")
	if err := os.WriteFile(defaultsFile, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		env     []string
		args    []string
		lock    string
		code    int
		stderr  string
		noMatch string
	}{
		{"detected", nil, nil, lockFile, exitOK, "", "warning"},
		{"flag removes __unix", nil, []string{"--virtual-override", "unix="}, lockFile, exitFailure, `requires "__unix", host has none`, ""},
		{"flag with prefix", nil, []string{"--virtual-override", "__UNIX="}, lockFile, exitFailure, `requires "__unix"`, ""},
		{"unknown environment override", []string{"CONDA_OVERRIDE_UNIX="}, nil, lockFile, exitOK, "ignoring CONDA_OVERRIDE_UNIX", ""},
		{"malformed flag", nil, []string{"--virtual-override", "unix"}, lockFile, exitUsage, "is not NAME=VALUE", ""},
		{"from lock", nil, []string{"--virtual-from-lock"}, recordedFile, exitOK, "", "warning"},
		{"from lock with override", nil, []string{"--virtual-from-lock", "--virtual-override", "unix="}, recordedFile, exitFailure, `requires "__unix"`, ""},
		{"recorded set differs", nil, nil, recordedFile, exitOK, "warning: the lockfile was solved for other virtual packages", ""},
		{"recorded defaults", nil, nil, defaultsFile, exitOK, "", "warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &cli{
				client:   func() (*http.Client, error) { return srv.Client(), nil },
				cacheDir: func() (string, error) { return t.TempDir(), nil },
				environ:  func() []string { return tc.env },
				virtual:  fakeDetect,
			}
			args := append([]string{"create", "--lock", tc.lock, "-p", filepath.Join(t.TempDir(), "env"), "--no-pyc"}, tc.args...)
			code, _, stderr := runCLI(t, c, args...)
			if code != tc.code || !strings.Contains(stderr, tc.stderr) || tc.noMatch != "" && strings.Contains(stderr, tc.noMatch) {
				t.Errorf("exit %d (want %d): %s", code, tc.code, stderr)
			}
			if strings.Contains(stderr, "goomba: goomba:") {
				t.Errorf("doubled prefix: %s", stderr)
			}
		})
	}
}

// TestLockRefusesIncomplete (review S7).
func TestLockRefusesIncomplete(t *testing.T) {
	lockFile, srv, _ := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	c := &cli{
		client:   func() (*http.Client, error) { return srv.Client(), nil },
		cacheDir: func() (string, error) { return t.TempDir(), nil },
		virtual:  fakeDetect,
	}
	prefix := filepath.Join(t.TempDir(), "env")
	if code, _, stderr := runCLI(t, c, "create", "--lock", lockFile, "-p", prefix); code != exitOK {
		t.Fatalf("create: %s", stderr)
	}
	if err := os.WriteFile(filepath.Join(prefix, "conda-meta", ".goomba-incomplete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.json")
	code, _, stderr := runCLI(t, c, "lock", "--from-prefix", prefix, "-o", out)
	if code != exitFailure || !strings.Contains(stderr, "is incomplete") {
		t.Errorf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("lockfile written: %v", err)
	}
}

// TestMessagePrefix (review N2): errors carry one "goomba: " prefix.
func TestMessagePrefix(t *testing.T) {
	for _, args := range [][]string{
		{"list", "-p", "/nonexistent/prefix"},
		{"export", "--explicit", "--lock", "/nonexistent/lock.json"},
		{"bogus"},
	} {
		_, _, stderr := runCLI(t, &cli{}, args...)
		if !strings.HasPrefix(stderr, "goomba: ") || strings.Contains(stderr, "goomba: goomba:") {
			t.Errorf("%q: %s", args, stderr)
		}
	}
}

// TestOverrideSpellings (review round 2, N2, N3): environment and flag
// spell names alike, the flag wins, values are trimmed, and unknown
// CONDA_OVERRIDE_* names are ignored with a warning.
func TestOverrideSpellings(t *testing.T) {
	var buf bytes.Buffer
	c := &cli{stderr: &buf, environ: func() []string {
		return []string{"CONDA_OVERRIDE___CUDA=12.4", "CONDA_OVERRIDE_GLIBC= 2.17 ", "CONDA_OVERRIDE_FOO=1", "CONDA_OVERRIDE_Osx=13.1", "PATH=/bin"}
	}}
	got, err := c.overrides([]string{"cuda=12.5", "__Linux = 6.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"cuda": "12.5", "glibc": "2.17", "osx": "13.1", "linux": "6.1"}
	if len(got) != len(want) {
		t.Errorf("overrides %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("overrides[%s] = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if !strings.Contains(buf.String(), "ignoring CONDA_OVERRIDE_FOO") || strings.Count(buf.String(), "warning") != 1 {
		t.Errorf("warnings: %q", buf.String())
	}
	// The merged map is one virtual.Options.Overrides: no conflicting keys.
	plat := virtual.HostPlatform()
	if plat == "" {
		t.Skip("no host platform")
	}
	if _, err := virtual.ApplyOverrides(plat, virtual.Defaults(plat), got); err != nil {
		t.Errorf("ApplyOverrides: %v", err)
	}
}

// TestEnvironmentOverrides: CONDA_OVERRIDE_GLIBC changes the __glibc the
// lockfile is checked against; the flag wins over it.
func TestEnvironmentOverrides(t *testing.T) {
	if virtual.HostPlatform() != "linux-64" {
		t.Skip("libzlib is a linux-64 package")
	}
	lockFile, srv, _ := testLockfile(t, "libzlib-1.3.2-h25fd6f3_2") // depends on __glibc >=2.17,<3.0.a0
	for _, tc := range []struct {
		env  []string
		args []string
		code int
	}{
		{nil, nil, exitOK},
		{[]string{"CONDA_OVERRIDE_GLIBC=2.12"}, nil, exitFailure},
		{[]string{"CONDA_OVERRIDE___GLIBC=2.12"}, nil, exitFailure},
		{[]string{"CONDA_OVERRIDE_GLIBC=2.12"}, []string{"--virtual-override", "__GLIBC=2.28"}, exitOK},
		{[]string{"CONDA_OVERRIDE_GLIBC=2.28"}, []string{"--virtual-override", "glibc="}, exitFailure},
	} {
		c := &cli{
			client:   func() (*http.Client, error) { return srv.Client(), nil },
			cacheDir: func() (string, error) { return t.TempDir(), nil },
			environ:  func() []string { return tc.env },
			virtual:  fakeDetect,
		}
		args := append([]string{"create", "--lock", lockFile, "-p", filepath.Join(t.TempDir(), "env")}, tc.args...)
		if code, _, stderr := runCLI(t, c, args...); code != tc.code {
			t.Errorf("%v %v: exit %d, want %d: %s", tc.env, tc.args, code, tc.code, stderr)
		}
	}
}

// TestNvidiaSMIFlag: --nvidia-smi reaches virtual.Options.NvidiaSMI; a
// relative or missing path is a usage error (virtual.ErrInvalidOption), as
// is combining it with --virtual-from-lock.
func TestNvidiaSMIFlag(t *testing.T) {
	lockFile, srv, _ := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	var got string
	capture := func(ctx context.Context, o virtual.Options) ([]*record.PackageRecord, error) {
		got = o.NvidiaSMI
		return fakeDetect(ctx, o)
	}
	newCLI := func(detect func(context.Context, virtual.Options) ([]*record.PackageRecord, error)) *cli {
		return &cli{
			client:   func() (*http.Client, error) { return srv.Client(), nil },
			cacheDir: func() (string, error) { return t.TempDir(), nil },
			virtual:  detect,
		}
	}
	smi := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(smi, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, newCLI(capture), "create", "--lock", lockFile, "-p", filepath.Join(t.TempDir(), "env"), "--nvidia-smi", smi); code != exitOK || got != smi {
		t.Errorf("passed %q: exit %d: %s", got, code, stderr)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--nvidia-smi", "nvidia-smi"}, "--nvidia-smi"},
		{[]string{"--nvidia-smi", filepath.Join(t.TempDir(), "missing", "nvidia-smi")}, "--nvidia-smi"},
		{[]string{"--nvidia-smi", smi, "--virtual-from-lock"}, "--virtual-from-lock"},
	} {
		if (runtime.GOOS != "linux" && runtime.GOOS != "windows") && strings.Contains(tc.args[1], "missing") {
			continue // only Linux and Windows check that the file exists
		}
		args := append([]string{"create", "--lock", lockFile, "-p", filepath.Join(t.TempDir(), "env")}, tc.args...)
		code, _, stderr := runCLI(t, newCLI(nil), args...)
		if code != exitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d: %s", tc.args, code, stderr)
		}
	}
}
