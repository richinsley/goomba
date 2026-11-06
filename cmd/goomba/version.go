package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// versionLine returns the one line goomba version prints: the module
// version the binary was built with (the tag under go install, "(devel)"
// for a local build), the git revision and modified flag when the build
// was stamped from a git tree, and the Go toolchain, OS and architecture.
// It takes no input and its output is deterministic for a given binary.
func versionLine() string {
	version := ""
	var revision string
	modified := false
	if info, ok := debug.ReadBuildInfo(); ok {
		version = info.Main.Version
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	if version == "" {
		// A test binary, or a build with the main module's version
		// stripped: nothing to name but the tree it came from.
		version = "(devel)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "goomba %s (", version)
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		b.WriteString(revision)
		if modified {
			b.WriteString(", modified")
		}
		b.WriteString("; ")
	}
	fmt.Fprintf(&b, "%s %s/%s)", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return b.String()
}
