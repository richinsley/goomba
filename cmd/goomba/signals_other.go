//go:build !unix

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

// signalContext returns a context cancelled by the first interrupt, which
// also prints that the command is rolling back; later interrupts are
// ignored until stop is called.
func signalContext(parent context.Context, stderr io.Writer) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt)
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
