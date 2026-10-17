//go:build unix

package lock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestFromPrefixFIFO is a regression test: a FIFO (or a link to one) named
// like a record used to block FromPrefixContext forever in open(2),
// ignoring the context.
func TestFromPrefixFIFO(t *testing.T) {
	for _, viaLink := range []bool{false, true} {
		prefix := copyPrefix(t)
		target := filepath.Join(prefix, "conda-meta", "bar-1-0.json")
		fifo := target
		if viaLink {
			fifo = filepath.Join(prefix, "fifo")
		}
		if err := unix.Mkfifo(fifo, 0o644); err != nil {
			t.Skip("mkfifo unavailable:", err)
		}
		if viaLink {
			if err := os.Symlink(fifo, target); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		done := make(chan error, 1)
		go func() {
			_, err := FromPrefixContext(ctx, prefix, "linux-64")
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("link=%v: FromPrefixContext = %v, want ErrInvalid", viaLink, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("link=%v: FromPrefixContext blocked on a FIFO", viaLink)
		}
		cancel()

		// The opener itself never blocks either, whatever the directory
		// listing said.
		go func() {
			_, err := readPrefixRecordFile(target)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("link=%v: readPrefixRecordFile = %v, want ErrInvalid", viaLink, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("link=%v: readPrefixRecordFile blocked on a FIFO", viaLink)
		}
	}
}
