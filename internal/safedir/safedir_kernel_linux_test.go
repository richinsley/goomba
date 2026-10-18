//go:build linux

package safedir

// A differential test of the resolution against the Linux kernel's own:
// open(O_PATH) the same path and read back /proc/self/fd/N. Contributed by
// the WP8b reviewer (round 2) as a probe and made permanent.

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// kernelResolve resolves p as the kernel does, following a final symlink.
func kernelResolve(p string) (string, error) {
	fd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
}

func errnoOf(err error) syscall.Errno {
	var e syscall.Errno
	if errors.As(err, &e) {
		return e
	}
	return 0
}

// TestResolveMatchesKernel builds random trees of directories, files and
// relative and absolute symlinks, and resolves random paths with ".", "..",
// "//" and trailing slashes both ways. Wherever the kernel resolves a path,
// ResolvePath must return the same path; ENOTDIR and ELOOP must match;
// where the kernel says ENOENT, ResolvePath may return ENOENT, EEXIST (a
// dangling final symlink, as mkdir(2) says) or a path that does not exist
// yet (only for a missing tail of plain names, which it guarantees by
// construction). The seeds are fixed; -short checks fewer trees.
func TestResolveMatchesKernel(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	trees := 300
	if testing.Short() {
		trees = 40
	}
	names := []string{"a", "b", "c", "d", "f", "g", "l1", "l2", "l3", "l4", "l5"}
	stats := map[string]int{}
	var mismatch []string
	for tree := 0; tree < trees; tree++ {
		rng := rand.New(rand.NewSource(int64(tree)))
		root := filepath.Join(base, fmt.Sprintf("t%d", tree))
		must(t, os.Mkdir(root, 0o755))
		dirs := []string{root}
		for i := 0; i < 12; i++ {
			parent := dirs[rng.Intn(len(dirs))]
			n := filepath.Join(parent, names[rng.Intn(6)])
			if _, err := os.Lstat(n); err == nil {
				continue
			}
			if rng.Intn(4) == 0 {
				must(t, os.WriteFile(n, nil, 0o644))
			} else if strings.Count(n[len(root):], "/") < 4 {
				must(t, os.Mkdir(n, 0o755))
				dirs = append(dirs, n)
			}
		}
		comp := func() string {
			switch r := rng.Intn(10); {
			case r < 2:
				return ".."
			case r < 3:
				return "."
			case r < 4:
				return ""
			default:
				return names[rng.Intn(len(names))]
			}
		}
		randRel := func(max int) string {
			parts := make([]string, 1+rng.Intn(max))
			for i := range parts {
				parts[i] = comp()
			}
			s := strings.Join(parts, "/")
			if rng.Intn(5) == 0 {
				s += "/"
			}
			return s
		}
		for i := 0; i < 8; i++ {
			parent := dirs[rng.Intn(len(dirs))]
			n := filepath.Join(parent, names[6+rng.Intn(5)])
			if _, err := os.Lstat(n); err == nil {
				continue
			}
			var target string
			switch r := rng.Intn(10); {
			case r < 6:
				target = randRel(4)
			case r < 8:
				target = root + "/" + randRel(3)
			case r < 9:
				target = "/"
			default:
				target = "//" + strings.TrimPrefix(root, "/") + "//" + randRel(2)
			}
			if target == "" {
				target = "."
			}
			must(t, os.Symlink(target, n))
		}
		for q := 0; q < 200; q++ {
			p := root + "/" + randRel(5)
			if rng.Intn(10) == 0 {
				p = "/" + p
			}
			kp, kerr := kernelResolve(p)
			mp, merr := resolveWith(p, false, osOps())
			ke, me := errnoOf(kerr), errnoOf(merr)
			switch {
			case kerr == nil && merr == nil:
				if kp == mp {
					stats["same path"]++
					continue
				}
				mismatch = append(mismatch, fmt.Sprintf("path %s: kernel %s, ResolvePath %s", p, kp, mp))
			case kerr == nil:
				mismatch = append(mismatch, fmt.Sprintf("path %s: kernel %s, ResolvePath %v", p, kp, merr))
			case merr == nil:
				if _, err := os.Lstat(mp); ke == unix.ENOENT && errors.Is(err, fs.ErrNotExist) {
					stats["kernel ENOENT, ResolvePath a missing path"]++
					continue
				}
				mismatch = append(mismatch, fmt.Sprintf("path %s: kernel %v, ResolvePath %s", p, kerr, mp))
			case ke == me || ke == unix.ENOENT && me == unix.EEXIST:
				stats["errno "+me.Error()]++
			default:
				mismatch = append(mismatch, fmt.Sprintf("path %s: kernel %v, ResolvePath %v", p, kerr, merr))
			}
		}
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-45s %d", k, stats[k])
	}
	for i, m := range mismatch {
		if i == 20 {
			t.Errorf("... %d more", len(mismatch)-20)
			break
		}
		t.Error(m)
	}
}
