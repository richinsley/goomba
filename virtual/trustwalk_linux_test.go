//go:build linux

package virtual

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTrustWalkResolvedPathTooLong pins a way the walk knowingly parts from
// the kernel, which random trees do not reach: links that lead from a short
// path to a file whose own path is longer than PATH_MAX (4096 bytes on
// Linux). The kernel resolves the short path, as it never spells out the
// file's path; the walk names every directory it passes, and the file's
// path is one nothing can open or run, so it refuses, failing closed.
func TestTrustWalkResolvedPathTooLong(t *testing.T) {
	const depth = 17 // directories of 251 bytes each: 4267 bytes in all
	seg := strings.Repeat("d", 250)
	base := trustWalkBase(t)
	// The path is too long to make by name, so make each directory in the
	// one before it. Half way down, a link leads to the file at the bottom.
	fd, err := unix.Open(base, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unix.Close(fd) }()
	for i := range depth {
		if i == depth/2 {
			if err := unix.Symlinkat(strings.Repeat(seg+"/", depth-i)+"f", fd, "rest"); err != nil {
				t.Fatal(err)
			}
		}
		if err := unix.Mkdirat(fd, seg, 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, seg, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		unix.Close(fd)
		fd = next
	}
	f, err := unix.Openat(fd, "f", unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(f)
	start := base + "/start"
	if err := os.Symlink(strings.Repeat(seg+"/", depth/2)+"rest", start); err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Stat(start); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the kernel does not resolve %s to a regular file: %v", start, err)
	}
	// Both links are followed (so the refusal is the long path's lstat
	// failing with ENAMETOOLONG), and the walk refuses.
	k := &kernelFS{}
	if got, ok := trustedUnixExecutable(k, start); ok {
		t.Errorf("trustedUnixExecutable(%q) accepted a %d-byte path", start, len(got))
	} else if len(k.targets) != 2 {
		t.Errorf("the walk refused after %d links, want 2", len(k.targets))
	}
}
