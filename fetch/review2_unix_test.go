//go:build unix

package fetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO planted as a lock file, key file or sidecar is refused without
// blocking.
func TestPlantedFIFOsDoNotBlock(t *testing.T) {
	s := newServer(t, false)
	data := buildConda(t, map[string]string{"x": "x"})
	url := s.put("/c/foo-1.0-0.conda", data)
	sha := sha256Hex(data)
	run := func(c *Cache) error {
		done := make(chan error, 1)
		go func() { _, err := c.Fetch(context.Background(), url, sha, 0); done <- err }()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Fetch blocked on a FIFO")
			return nil
		}
	}

	c := newCache(t, s)
	os.MkdirAll(filepath.Join(c.Dir, ".locks"), 0o755)
	if err := unix.Mkfifo(filepath.Join(c.Dir, ".locks", sha+".lock"), 0o644); err != nil {
		t.Skip(err)
	}
	if err := run(c); !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("FIFO lock file: %v", err)
	}

	c = newCache(t, s)
	unix.Mkfifo(filepath.Join(c.Dir, keyName), 0o600)
	if err := run(c); err == nil {
		t.Fatal("FIFO key accepted")
	}

	c = newCache(t, s)
	if err := run(c); err != nil {
		t.Fatal(err)
	}
	side := archivePath(c, sha, "foo-1.0-0.conda") + ".sha256"
	os.Remove(side)
	unix.Mkfifo(side, 0o644)
	if err := run(c); err != nil {
		t.Fatalf("FIFO sidecar: %v", err)
	}
}

func TestKeyErrorNamesFilesystemCause(t *testing.T) {
	err := checkKeyFile(fakeInfo{0o777, uint32(os.Getuid())})
	if !errors.Is(err, ErrUnsafeCache) || !strings.Contains(err.Error(), "Unix ownership and permissions") {
		t.Fatalf("error %v does not name the cause", err)
	}
}
