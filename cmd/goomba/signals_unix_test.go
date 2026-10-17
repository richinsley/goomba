//go:build unix

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer safe for the signal goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestSignalsCancel (review S3, N8): SIGTERM and SIGHUP cancel like SIGINT,
// the first one says it rolls back, later ones are ignored.
func TestSignalsCancel(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		var buf lockedBuffer
		ctx, stop := signalContext(context.Background(), &buf)
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%v did not cancel", sig)
		}
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		stop()
		if got := buf.String(); strings.Count(got, "rolling back") != 1 {
			t.Errorf("%v: output %q", sig, got)
		}
	}
}

// TestSIGPIPEIgnored (review S3): with its stderr pipe closed, goomba keeps
// running instead of dying of SIGPIPE.
func TestSIGPIPEIgnored(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSIGPIPEChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GOOMBA_TEST_CHILD=sigpipe")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = w
	r.Close() // nobody reads stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	err = cmd.Wait()
	if err != nil || !strings.Contains(out.String(), "survived") {
		t.Errorf("child: %v\n%s", err, out.String())
	}
}

// TestSIGPIPEChild runs in a child process for TestSIGPIPEIgnored.
func TestSIGPIPEChild(t *testing.T) {
	if os.Getenv("GOOMBA_TEST_CHILD") != "sigpipe" {
		t.Skip("child process only")
	}
	_, stop := signalContext(context.Background(), os.Stderr)
	defer stop()
	for i := 0; i < 100; i++ {
		fmt.Fprintln(os.Stderr, "progress line", i)
	}
	fmt.Println("survived")
}
