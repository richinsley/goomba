//go:build linux

package link

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// kernelResolve resolves p as the Linux kernel does, following a final
// symlink: open(O_PATH) and read back /proc/self/fd/N.
func kernelResolve(p string) (string, error) {
	fd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
}

// kernelCheckPrefix resolves every symlink below prefix with the kernel and
// fails the test for one that lands outside the prefix or in its
// conda-meta. A symlink the kernel cannot resolve (dangling, ENOTDIR for
// "file/.", a loop) leads nowhere and passes. It returns how many resolved.
func kernelCheckPrefix(t *testing.T, prefix string) (resolved int) {
	t.Helper()
	real, err := filepath.EvalSymlinks(prefix)
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(real, "conda-meta")
	err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		got, err := kernelResolve(p)
		var errno syscall.Errno
		switch {
		case errors.As(err, &errno) && (errno == unix.ENOENT || errno == unix.ENOTDIR || errno == unix.ELOOP):
			return nil
		case err != nil:
			return err
		}
		resolved++
		target, _ := os.Readlink(p)
		if got != real && !strings.HasPrefix(got, real+"/") {
			t.Errorf("%s -> %q resolves to %s, outside %s", p, target, got, real)
		}
		if got == meta || strings.HasPrefix(got, meta+"/") {
			t.Errorf("%s -> %q resolves into conda-meta (%s)", p, target, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Every accepted target of the shape table, linked at the depths around
// its limit, resolves inside the prefix as the kernel sees it, to the
// entry its normalised form names.
func TestSymlinkTargetsKernel(t *testing.T) {
	targets := []string{
		".", "./", ".//./", "f", "./f", "d/", "d/.", "d//f", "d/./f/", "..", "./..", "../",
		"..//f", ".././d/f", ".././../f", "./.././../d/./f//", "../../",
	}
	for depth := 0; depth <= 3; depth++ {
		dir := strings.Repeat("s/", depth)
		var files []pkgFile
		// d/f and f at every level, so most targets resolve.
		for i := 0; i <= depth; i++ {
			lvl := strings.Repeat("s/", i)
			files = append(files, pkgFile{path: lvl + "f", data: "f" + strconv.Itoa(i)}, pkgFile{path: lvl + "d/f", data: "df" + strconv.Itoa(i)})
		}
		var accepted []string
		for i, target := range targets {
			if checkSymlinkTarget(dir+"l"+strconv.Itoa(i), target) != nil {
				continue
			}
			accepted = append(accepted, target)
			files = append(files, pkgFile{path: dir + "l" + strconv.Itoa(i), symlink: target})
		}
		t.Run(fmt.Sprint("depth", depth), func(t *testing.T) {
			tmp := t.TempDir()
			pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "shapes", files: files})
			prefix := filepath.Join(tmp, "env")
			if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"}); err != nil {
				t.Fatal(err)
			}
			real, err := filepath.EvalSymlinks(prefix)
			if err != nil {
				t.Fatal(err)
			}
			n := kernelCheckPrefix(t, prefix)
			notDir := 0
			for i, target := range targets {
				l := filepath.Join(real, dir+"l"+strconv.Itoa(i))
				if _, err := os.Lstat(l); err != nil {
					continue // refused for this depth
				}
				ups, names, _ := symlinkTargetShape(target)
				want := filepath.Join(append([]string{real, strings.Repeat("s/", depth-ups)}, names...)...)
				got, err := kernelResolve(l)
				// A trailing "/" or "/." after a file: ENOTDIR, so the
				// symlink leads nowhere.
				if fi, serr := os.Lstat(want); serr == nil && !fi.IsDir() && (strings.HasSuffix(target, "/") || strings.HasSuffix(target, "/.")) {
					notDir++
					if !errors.Is(err, unix.ENOTDIR) {
						t.Errorf("%s -> %q: kernel %q, %v; want ENOTDIR", dir+"l"+strconv.Itoa(i), target, got, err)
					}
					continue
				}
				if err != nil || got != want {
					t.Errorf("%s -> %q: kernel %q, %v; normalised form names %q", dir+"l"+strconv.Itoa(i), target, got, err, want)
				}
			}
			if n+notDir != len(accepted) || notDir == 0 && depth > 0 {
				t.Errorf("%d of %d symlinks resolved, %d ENOTDIR", n, len(accepted), notDir)
			}
		})
	}
}

// TestSymlinkGuardKernelRandom links random packages, several into each
// prefix, over a small name space so that they interact: their symlinks
// have targets made of ".", empty components, ".." and names, and a later
// package may turn a component an earlier target goes through into a
// symlink. After every Link that succeeds, every symlink in the prefix must
// resolve inside it (and outside conda-meta) as the kernel sees it; a Link
// that fails must leave the prefix as it was. -short checks fewer prefixes.
func TestSymlinkGuardKernelRandom(t *testing.T) {
	rounds := 400
	if testing.Short() {
		rounds = 50
	}
	names := []string{"a", "l"}
	anything := []string{"..", "..", ".", "", "a", "l"}
	stats := map[string]int{}
	for round := range rounds {
		rng := rand.New(rand.NewSource(int64(round)))
		pick := func(s []string) string { return s[rng.Intn(len(s))] }
		tmp := t.TempDir()
		prefix := filepath.Join(tmp, "env")
		for pkg := range 4 {
			seen := map[string]bool{}
			var files []pkgFile
			for range 3 {
				depth := rng.Intn(3)
				var p []string
				for range depth + 1 {
					p = append(p, pick(names))
				}
				rel := strings.Join(p, "/")
				clash := false
				for k := range seen {
					if k == rel || strings.HasPrefix(k, rel+"/") || strings.HasPrefix(rel, k+"/") {
						clash = true
					}
				}
				if clash {
					continue
				}
				seen[rel] = true
				if rng.Intn(4) == 0 {
					files = append(files, pkgFile{path: rel, data: rel})
					continue
				}
				var tc []string
				noise := func() {
					if rng.Intn(3) == 0 {
						tc = append(tc, pick([]string{".", ""}))
					}
				}
				switch rng.Intn(6) {
				case 0: // anything: mostly refused
					for range 1 + rng.Intn(5) {
						tc = append(tc, pick(anything))
					}
				case 1: // ".." after a name: always refused
					tc = append(tc, pick(names))
					noise()
					for range 1 + rng.Intn(depth+1) {
						tc = append(tc, "..")
						noise()
					}
				default: // leading ".." (now and then one too many), then names
					noise()
					ups := rng.Intn(depth + 1)
					if rng.Intn(8) == 0 {
						ups = depth + 1
					}
					for range ups {
						tc = append(tc, "..")
						noise()
					}
					for range rng.Intn(2) + rng.Intn(2) {
						tc = append(tc, pick(names))
						noise()
					}
				}
				target := strings.Join(tc, "/")
				if target == "" || strings.HasPrefix(target, "/") {
					target = "." + target
				}
				files = append(files, pkgFile{path: rel, symlink: target})
			}
			if len(files) == 0 {
				continue
			}
			pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: fmt.Sprint("p", pkg), files: files})
			before := tree(t, prefix)
			_, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"})
			switch {
			case err == nil:
				stats["linked"]++
				stats["resolved"] += kernelCheckPrefix(t, prefix)
			default:
				switch {
				case errors.Is(err, ErrUnsafePath):
					stats["unsafe"]++
				case errors.Is(err, ErrExists):
					stats["exists"]++
				default: // a path through a file, ...
					stats["other"]++
				}
				if after := tree(t, prefix); fmt.Sprint(after) != fmt.Sprint(before) {
					t.Errorf("round %d: failed package changed the prefix: %v\n before %q\n after  %q", round, err, before, after)
				}
			}
			if t.Failed() {
				t.Fatalf("round %d, package %d: %+v", round, pkg, files)
			}
		}
		attackSymlinks(t, tmp, prefix, stats)
		if t.Failed() {
			t.Fatalf("round %d: attack succeeded", round)
		}
	}
	t.Logf("%v", stats)
	if stats["linked"] < rounds/2 || stats["resolved"] < rounds/2 {
		t.Errorf("too few links exercised: %v", stats)
	}
}

// attackSymlinks plays the next package of an attacker: for each symlink in
// prefix, it takes the first directory the symlink's target names that does
// not exist yet and tries to link a package that makes it a symlink to the
// prefix root, the strongest way to push a later ".." of the target above
// the prefix. Whatever Link accepts must keep every symlink inside.
func attackSymlinks(t *testing.T, tmp, prefix string, stats map[string]int) {
	t.Helper()
	type link struct{ rel, target string }
	var links []link
	err := filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		rel, _ := filepath.Rel(prefix, p)
		target, err := os.Readlink(p)
		links = append(links, link{filepath.ToSlash(rel), target})
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for i, l := range links {
		var cur []string
		if dir := path.Dir(l.rel); dir != "." {
			cur = strings.Split(dir, "/")
		}
		var at string
	walk:
		for c := range strings.SplitSeq(l.target, "/") {
			switch c {
			case "", ".":
			case "..":
				if len(cur) == 0 {
					break walk
				}
				cur = cur[:len(cur)-1]
			default:
				cur = append(cur, c)
				if _, err := os.Lstat(filepath.Join(prefix, filepath.FromSlash(strings.Join(cur, "/")))); errors.Is(err, fs.ErrNotExist) {
					at = strings.Join(cur, "/")
					break walk
				}
			}
		}
		if at == "" {
			continue
		}
		up := "."
		if n := strings.Count(at, "/"); n > 0 {
			up = strings.TrimSuffix(strings.Repeat("../", n), "/")
		}
		pkgDir, rec := buildPkg(t, filepath.Join(tmp, "attack"), pkgSpec{name: fmt.Sprint("attack", i), files: []pkgFile{{path: at, symlink: up}}})
		if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"}); err == nil {
			stats["attack linked"]++
			kernelCheckPrefix(t, prefix)
		} else {
			stats["attack refused"]++
		}
	}
}
