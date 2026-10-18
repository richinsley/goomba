package rmtree

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// mountHelperArg marks the command line of the helper process that runs
// in new user and mount namespaces: "<test binary>
// -test.run=^TestMountHelper$ -- rmtree-mount-helper <mode> <dir>".
const mountHelperArg = "rmtree-mount-helper"

// inNamespace runs the helper in mode on dir as root of new user and mount
// namespaces, where it may bind-mount, and skips the test where the system
// does not allow unprivileged namespaces.
func inNamespace(t *testing.T, mode, dir string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMountHelper$", "--", mountHelperArg, mode, dir)
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
	return text
}

// TestMountHelper is the helper process of the mount tests.
func TestMountHelper(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != mountHelperArg {
		t.Skip("helper process of the mount tests")
	}
	mode, dir := args[1], args[2]
	fail := func(format string, a ...any) {
		fmt.Printf("FAIL: "+format+"\n", a...)
		os.Exit(1)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Println("SKIP: make mounts private:", err)
		os.Exit(0)
	}
	if err := unix.Mount(filepath.Join(dir, "outside"), filepath.Join(dir, "tree", "lib", "mnt"), "", unix.MS_BIND, ""); err != nil {
		fmt.Println("SKIP: bind mount:", err)
		os.Exit(0)
	}
	lib, err := unix.Open(filepath.Join(dir, "tree", "lib"), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		fail("open lib: %v", err)
	}
	switch mode {
	case "open":
		if _, err := OpenDir(lib, "mnt"); !errors.Is(err, ErrMountPoint) {
			fail("OpenDir on a bind mount: %v, want ErrMountPoint", err)
		}
		if fd, err := OpenDir(lib, "plain"); err != nil {
			fail("OpenDir on a plain directory: %v", err)
		} else {
			unix.Close(fd)
		}
		// The fallback without openat2: the mount ID tells a bind mount
		// of the same file system, the device number does not.
		if _, err := openSubdirCompare(lib, "mnt"); !errors.Is(err, ErrMountPoint) {
			fail("openSubdirCompare on a bind mount: %v, want ErrMountPoint", err)
		}
		if fd, err := openSubdirCompare(lib, "plain"); err != nil {
			fail("openSubdirCompare on a plain directory: %v", err)
		} else {
			unix.Close(fd)
		}
		mfd, err := unix.Openat(lib, "mnt", unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			fail("openat mnt: %v", err)
		}
		same, err := sameDevice(lib, mfd)
		fmt.Printf("same device for a bind mount of the same file system: %v (%v)\n", same, err)
	case "remove":
		before, _ := os.Stat(filepath.Join(dir, "outside"))
		err := RemoveAll(filepath.Join(dir, "tree"))
		if !errors.Is(err, ErrMountPoint) || !strings.Contains(err.Error(), "lib/mnt") {
			fail("RemoveAll across a mount point: %v, want ErrMountPoint naming lib/mnt", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "outside", "precious", "data")); err != nil {
			fail("data behind the mount point removed: %v", err)
		}
		if after, _ := os.Stat(filepath.Join(dir, "outside")); after.Mode() != before.Mode() {
			fail("mount point mode changed from %v to %v", before.Mode(), after.Mode())
		}
		if _, err := os.Stat(filepath.Join(dir, "tree", "lib", "plain")); err == nil {
			fail("the plain directory beside the mount point was not removed")
		}
	default:
		fail("unknown mode %q", mode)
	}
	fmt.Println("PASS-HELPER")
	os.Exit(0)
}

// mountTree makes dir/tree/lib/{mnt,plain/f} and dir/outside/precious/data
// (mode 0755), which the helper binds onto dir/tree/lib/mnt.
func mountTree(t *testing.T) string {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "tree", "lib", "mnt"), 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, "tree", "lib", "plain"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "tree", "lib", "plain", "f"), nil, 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "outside", "precious"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "outside", "precious", "data"), []byte("keep"), 0o644))
	must(t, os.Chmod(filepath.Join(dir, "outside"), 0o755))
	return dir
}

// TestOpenDirMountPoint (review of WP8c round 2, B1): OpenDir refuses a
// bind mount of the same file system, with openat2 and with the mount-ID
// fallback.
func TestOpenDirMountPoint(t *testing.T) {
	if len(flag.Args()) > 0 {
		t.Skip("in a helper process")
	}
	t.Log(inNamespace(t, "open", mountTree(t)))
}

// TestRemoveAllMountPoint (review of WP8c round 2, B1): RemoveAll stops at
// a mount point inside the tree, leaving what is mounted there, and its
// mode, untouched.
func TestRemoveAllMountPoint(t *testing.T) {
	if len(flag.Args()) > 0 {
		t.Skip("in a helper process")
	}
	dir := mountTree(t)
	t.Log(inNamespace(t, "remove", dir))
	if data, err := os.ReadFile(filepath.Join(dir, "outside", "precious", "data")); err != nil || string(data) != "keep" {
		t.Errorf("data outside: %q, %v", data, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "outside")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("outside: %v, %v", fi.Mode(), err)
	}
}
