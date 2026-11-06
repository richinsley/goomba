package main

import (
	"runtime"
	"strings"
	"testing"
)

// TestVersion: goomba version, --version and -version print one line
// naming a version, the Go toolchain and the platform, and exit 0.
func TestVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-version"} {
		code, stdout, stderr := runCLI(t, &cli{}, arg)
		if code != exitOK {
			t.Fatalf("%s: exit %d (%s)", arg, code, stderr)
		}
		line := strings.TrimSuffix(stdout, "\n")
		if line == "" || strings.Contains(line, "\n") || !strings.HasPrefix(line, "goomba ") {
			t.Errorf("%s: %q", arg, stdout)
		}
		for _, want := range []string{runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: %q lacks %s", arg, stdout, want)
			}
		}
	}
}

// TestVersionFlagAfterCommand: version is a command, not a flag; a
// subcommand that does not know it rejects it as a usage error.
func TestVersionFlagAfterCommand(t *testing.T) {
	if code, _, _ := runCLI(t, &cli{}, "list", "--version"); code != exitUsage {
		t.Errorf("list --version: exit %d, want %d", code, exitUsage)
	}
}
