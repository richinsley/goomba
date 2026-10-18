package goomba

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/richinsley/goomba/internal/rmtree"
	"golang.org/x/sys/unix"
)

// mountHelperArg marks the command line of the helper process of the mount
// tests, which runs in new user and mount namespaces: "<test binary>
// -test.run=^TestMountHelper$ -- goomba-mount-helper <mode> <root>". It
// bind-mounts what its mode needs, chroots into root (inside the user
// namespace the host's root-owned directories would belong to nobody, and
// safedir would rightly refuse them) and runs the scenario there.
const mountHelperArg = "goomba-mount-helper"

// inMountNamespace runs the helper in mode on root, skipping the test where
// the system does not allow unprivileged user and mount namespaces.
func inMountNamespace(t *testing.T, mode, root string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMountHelper$", "--", mountHelperArg, mode, root)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	var pe *os.PathError
	if errors.As(err, &pe) || strings.Contains(text, "SKIP:") {
		t.Skipf("no unprivileged user and mount namespaces: %v %s", err, text)
	}
	if err != nil || !strings.Contains(text, "PASS-HELPER") {
		t.Fatalf("helper %s: %v\n%s", mode, err, text)
	}
	t.Log(strings.TrimSpace(text))
}

// mountRoot makes a chroot directory holding an interrupted prefix /env
// with an empty lib/mnt, a file lib/f, and /outside/precious/data (mode
// 0755) and /outfile to mount into it.
func mountRoot(t *testing.T) string {
	root := filepath.Join(t.TempDir(), "root")
	for _, d := range []string{"env/conda-meta", "env/lib/mnt", "env/share", "outside/precious", "vol/conda-meta", "vol/lib", "metavol"} {
		must(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	for f, data := range map[string]string{
		"env/conda-meta/" + incompleteMarker: "", "env/conda-meta/x-1-0.json": "{}", "env/lib/f": "f", "env/share/s": "s",
		"outside/precious/data": "keep", "outfile": "keep",
		"vol/conda-meta/" + incompleteMarker: "", "vol/lib/a": "a",
		"metavol/" + incompleteMarker: "", "metavol/keep": "keep",
	} {
		must(t, os.WriteFile(filepath.Join(root, f), []byte(data), 0o644))
	}
	for _, d := range []string{"", "outside", "env", "vol"} {
		must(t, os.Chmod(filepath.Join(root, d), 0o755))
	}
	return root
}

// TestMountHelper is the helper process of the mount tests.
func TestMountHelper(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != mountHelperArg {
		t.Skip("helper process of the mount tests")
	}
	mode, root := args[1], args[2]
	fail := func(format string, a ...any) {
		fmt.Printf("FAIL: "+format+"\n", a...)
		os.Exit(1)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Println("SKIP: make mounts private:", err)
		os.Exit(0)
	}
	bind := func(src, dst string) {
		if err := unix.Mount(filepath.Join(root, src), filepath.Join(root, dst), "", unix.MS_BIND, ""); err != nil {
			fmt.Println("SKIP: bind mount:", err)
			os.Exit(0)
		}
	}
	chroot := func() {
		if err := unix.Chroot(root); err != nil {
			fmt.Println("SKIP: chroot:", err)
			os.Exit(0)
		}
		if err := os.Chdir("/"); err != nil {
			fail("chdir: %v", err)
		}
	}
	intact := func() {
		if data, err := os.ReadFile("/outside/precious/data"); err != nil || string(data) != "keep" {
			fail("data behind the mount point: %q, %v", data, err)
		}
		if fi, err := os.Stat("/outside"); err != nil || fi.Mode().Perm() != 0o755 {
			fail("the mounted directory: %v, %v (mode was 0755)", fi, err)
		}
	}
	marked := func(prefix string) {
		if err := CheckComplete(prefix); !errors.Is(err, ErrIncomplete) {
			fail("CheckComplete(%s) afterwards: %v, want ErrIncomplete", prefix, err)
		}
	}
	switch mode {
	case "remove-crossing":
		bind("outside", "env/lib/mnt")
		chroot()
		err := RemoveIncomplete("/env")
		fmt.Println("RemoveIncomplete:", err)
		if !errors.Is(err, rmtree.ErrMountPoint) || !strings.Contains(err.Error(), "lib/mnt") {
			fail("RemoveIncomplete across a mount point: %v, want rmtree.ErrMountPoint naming lib/mnt", err)
		}
		intact()
		marked("/env")
	case "file-mount":
		bind("outfile", "env/lib/f")
		chroot()
		err := RemoveIncomplete("/env")
		fmt.Println("RemoveIncomplete:", err)
		if err == nil || !strings.Contains(err.Error(), "lib/f") {
			fail("RemoveIncomplete with a file mounted on lib/f: %v, want an error naming it", err)
		}
		if data, err := os.ReadFile("/outfile"); err != nil || string(data) != "keep" {
			fail("mounted file: %q, %v", data, err)
		}
		marked("/env")
	case "rollback-crossing":
		chroot()
		tx, err := openPrefix("/new")
		if err != nil {
			fail("openPrefix: %v", err)
		}
		for _, d := range []string{"/new/lib/mnt", "/new/share"} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				fail("%v", err)
			}
		}
		if err := os.WriteFile("/new/share/s", nil, 0o644); err != nil {
			fail("%v", err)
		}
		if err := unix.Mount("/outside", "/new/lib/mnt", "", unix.MS_BIND, ""); err != nil {
			fail("bind mount inside the chroot: %v", err)
		}
		tx.rollback()
		intact()
		marked("/new")
	case "meta-mount":
		bind("metavol", "env/conda-meta")
		chroot()
		err := RemoveIncomplete("/env")
		fmt.Println("RemoveIncomplete:", err)
		if !errors.Is(err, rmtree.ErrMountPoint) {
			fail("RemoveIncomplete with conda-meta a mount point: %v, want rmtree.ErrMountPoint", err)
		}
		for _, f := range []string{"/env/lib/f", "/env/share/s", "/metavol/keep"} {
			if _, err := os.Stat(f); err != nil {
				fail("%s removed although conda-meta is a mount point: %v", f, err)
			}
		}
	case "prefix-mount":
		bind("vol", "env")
		chroot()
		if err := RemoveIncomplete("/env"); err != nil {
			fail("RemoveIncomplete of a prefix that is a mount point: %v", err)
		}
		ents, err := os.ReadDir("/env")
		if err != nil || len(ents) != 0 {
			fail("the prefix (a mount point) afterwards: %v, %v; want it kept and empty", ents, err)
		}
		if err := CheckComplete("/env"); !errors.Is(err, fs.ErrNotExist) {
			fail("CheckComplete afterwards: %v, want fs.ErrNotExist (empty)", err)
		}
	default:
		fail("unknown mode %q", mode)
	}
	fmt.Println("PASS-HELPER")
	os.Exit(0)
}

// TestRemoveIncompleteMountPoints (review of WP8c round 2, B1, S2): in user
// and mount namespaces, RemoveIncomplete and the rollback refuse to enter
// a mount point inside the prefix (a bind mount of the same file system, a
// mounted file, conda-meta itself), leave what is mounted there and its
// mode untouched and keep the prefix marked; a prefix that is itself a
// mount point is emptied and kept.
func TestRemoveIncompleteMountPoints(t *testing.T) {
	if len(flag.Args()) > 0 {
		t.Skip("in a helper process")
	}
	for _, mode := range []string{"remove-crossing", "file-mount", "rollback-crossing", "meta-mount", "prefix-mount"} {
		t.Run(mode, func(t *testing.T) {
			root := mountRoot(t)
			inMountNamespace(t, mode, root)
			if data, err := os.ReadFile(filepath.Join(root, "outside", "precious", "data")); err != nil || string(data) != "keep" {
				t.Errorf("data outside after the namespace ended: %q, %v", data, err)
			}
			makeWritable(root)
		})
	}
}
