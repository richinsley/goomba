//go:build linux

package goomba

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// killHelperArg marks the command line of the helper process of
// TestRemovalKilled: "<test binary> -test.run=^TestRemovalKillHelper$ --
// goomba-kill-helper <mode> <prefix>".
const killHelperArg = "goomba-kill-helper"

// fillPrefix gives prefix the layout of an installation that got as far as
// writing its history: many files, conda-meta records, the marker.
func fillPrefix(t testing.TB, prefix string) {
	for d := range 20 {
		dir := filepath.Join(prefix, "lib", fmt.Sprintf("d%02d", d))
		must(t, os.MkdirAll(dir, 0o755))
		for f := range 25 {
			must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", f)), []byte("x"), 0o644))
		}
	}
	for _, top := range []string{"bin", "include", "share", "ssl"} {
		must(t, os.MkdirAll(filepath.Join(prefix, top), 0o755))
		must(t, os.WriteFile(filepath.Join(prefix, top, "x"), []byte("x"), 0o644))
	}
	must(t, os.MkdirAll(filepath.Join(prefix, "conda-meta"), 0o755))
	for i := range 20 {
		must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", fmt.Sprintf("pkg%02d-1-0.json", i)), []byte("{}"), 0o644))
	}
	must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", "history"), []byte("==> x <==\n"), 0o644))
	if _, err := os.Lstat(filepath.Join(prefix, "conda-meta", incompleteMarker)); errors.Is(err, fs.ErrNotExist) {
		must(t, os.WriteFile(filepath.Join(prefix, "conda-meta", incompleteMarker), nil, 0o644))
	}
}

// TestRemovalKillHelper is the helper process of TestRemovalKilled. In mode
// "rollback" it takes the prefix with openPrefix, fills it, and rolls back
// when told to; in mode "remove" it runs RemoveIncomplete on the prefix
// when told to.
func TestRemovalKillHelper(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != killHelperArg {
		t.Skip("helper process of TestRemovalKilled")
	}
	mode, prefix := args[1], args[2]
	var tx *prefixTx
	if mode == "rollback" {
		var err error
		if tx, err = openPrefix(prefix); err != nil {
			fmt.Println("openPrefix:", err)
			os.Exit(2)
		}
		fillPrefix(t, prefix)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	if tx != nil {
		tx.rollback()
	} else if err := RemoveIncomplete(prefix); err != nil {
		fmt.Println("RemoveIncomplete:", err)
		os.Exit(2)
	}
	fmt.Println("done")
	os.Exit(0)
}

// TestRemovalKilled (review of WP8c, S1 and N5) kills a rollback and a
// RemoveIncomplete at many points, watching every directory of the prefix
// with inotify and sending SIGKILL after the k-th removal (or when the
// marker or conda-meta goes). Whatever is left must be a prefix
// CheckComplete reports as ErrIncomplete, which RemoveIncomplete then
// removes, or nothing: no files without the marker.
func TestRemovalKilled(t *testing.T) {
	if len(flag.Args()) > 0 {
		t.Skip("in a helper process")
	}
	ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Skipf("inotify: %v", err)
	}
	unix.Close(ifd)
	for _, mode := range []string{"rollback", "remove"} {
		kills := []string{"1", "2", "3", "10", "100", "300", "-30", "-5", "-3", "-2", "-1", "marker", "conda-meta"}
		if testing.Short() {
			kills = []string{"1", "100", "-2", "marker", "conda-meta"}
		}
		for _, kill := range kills {
			t.Run(mode+"/"+kill, func(t *testing.T) { killRemoval(t, mode, kill) })
		}
	}
}

func killRemoval(t *testing.T, mode, kill string) {
	base := t.TempDir()
	prefix := filepath.Join(base, "env")
	if mode == "remove" {
		must(t, os.Mkdir(prefix, 0o700))
		fillPrefix(t, prefix)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemovalKillHelper$", "--", killHelperArg, mode, prefix)
	stdin, err := cmd.StdinPipe()
	must(t, err)
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() || sc.Text() != "ready" {
		t.Fatalf("helper: %q", sc.Text())
	}

	ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	must(t, err)
	defer unix.Close(ifd)
	total := 0
	must(t, filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != prefix {
			total++
		}
		if d.IsDir() {
			_, err = unix.InotifyAddWatch(ifd, p, unix.IN_DELETE)
		}
		return err
	}))
	at := -1
	if n, err := fmt.Sscan(kill, &at); n == 1 && err == nil && at < 0 {
		at += total + 1
	}
	_, _ = stdin.Write([]byte{'\n'})
	buf := make([]byte, 64<<10)
	seen := 0
	deadline := time.Now().Add(20 * time.Second)
watch:
	for time.Now().Before(deadline) {
		n, err := unix.Read(ifd, buf)
		if err != nil {
			break
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
			name := strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+nameLen]), "\x00")
			off += unix.SizeofInotifyEvent + nameLen
			if mask&unix.IN_DELETE == 0 {
				continue // IN_IGNORED for a removed directory's watch
			}
			seen++
			if seen == at || kill == "marker" && name == incompleteMarker || kill == "conda-meta" && name == "conda-meta" {
				_ = cmd.Process.Kill()
				break watch
			}
		}
	}
	_ = cmd.Wait()

	left := 0
	if _, err := os.Lstat(prefix); err == nil {
		must(t, filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
			if err == nil && p != prefix {
				left++
			}
			return err
		}))
	}
	err = CheckComplete(prefix)
	t.Logf("killed after %d of %d removals: %d entries left, CheckComplete: %v", seen, total, left, err)
	switch {
	case errors.Is(err, ErrIncomplete):
		if err := RemoveIncomplete(prefix); err != nil {
			t.Fatalf("RemoveIncomplete: %v", err)
		}
		if _, err := os.Lstat(prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("prefix still there after RemoveIncomplete: %v", err)
		}
	case left == 0 && errors.Is(err, fs.ErrNotExist):
		// Fully removed: gone, or an empty directory.
	default:
		t.Errorf("%d entries left and CheckComplete says %v: neither interrupted nor removed", left, err)
	}
}
