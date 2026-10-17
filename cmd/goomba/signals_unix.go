//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// signalContext returns a context cancelled by the first SIGINT, SIGTERM or
// SIGHUP, which also prints that the command is rolling back; later signals
// are ignored until stop is called. SIGPIPE is ignored for the life of the
// process, so that a closed stdout or stderr pipe makes writes fail instead
// of killing goomba in the middle of an installation.
func signalContext(parent context.Context, stderr io.Writer) (ctx context.Context, stop func()) {
	signal.Ignore(syscall.SIGPIPE)
	return notifyCancel(parent, stderr, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}

// notifyCancel cancels the returned context on the first of sigs.
func notifyCancel(parent context.Context, stderr io.Writer, sigs ...os.Signal) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	go func() {
		first := true
		for {
			select {
			case s := <-ch:
				if first {
					first = false
					fmt.Fprintf(stderr, "goomba: %v: stopping, rolling back…\n", s)
					cancel()
				}
			case <-done:
				return
			}
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		close(done)
		cancel()
	}
}
