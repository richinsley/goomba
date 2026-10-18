// Command create builds a conda environment from match specs, as
// "goomba create -p PREFIX SPEC..." does: it resolves the specs against
// conda-forge and installs the result into PREFIX.
//
//	go run ./examples/create ./py312 python=3.12 pip
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/richinsley/goomba"
	"github.com/richinsley/goomba/solve"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: create PREFIX SPEC...")
	}
	prefix, specs := os.Args[1], os.Args[2:]

	// goomba reads no environment variables or config files: the caller
	// chooses the caches. These are the CLI's defaults.
	cache, err := os.UserCacheDir()
	if err != nil {
		log.Fatal(err)
	}

	// Solving is NP-complete and a hostile channel can make it run for very
	// long, so always pass a deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Progress may be called from several goroutines at once.
	var mu sync.Mutex
	progress := func(e goomba.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Kind {
		case goomba.EventSolved:
			fmt.Fprintf(os.Stderr, "solved: %d packages\n", e.Total)
		case goomba.EventLinked:
			fmt.Fprintf(os.Stderr, "linked %s (%d/%d)\n", e.Package, e.Done, e.Total)
		}
	}

	lf, err := goomba.Create(ctx, prefix,
		goomba.LockOptions{
			Specs:       specs,
			Channels:    []string{"conda-forge"},
			RepodataDir: filepath.Join(cache, "goomba", "repodata"),
			Progress:    progress,
		},
		goomba.InstallOptions{
			PkgsDir:    filepath.Join(cache, "goomba", "pkgs"),
			CompilePyc: true,
			Progress:   progress,
		})

	var unsolvable *solve.Unsolvable
	switch {
	case errors.As(err, &unsolvable):
		fmt.Fprintf(os.Stderr, "no solution:\n%s\n", unsolvable.Explanation)
		os.Exit(1)
	case errors.Is(err, goomba.ErrPrefixNotEmpty):
		log.Fatalf("%s already exists; choose a new prefix", prefix)
	case err != nil:
		// The installation was rolled back (see docs/failures.md).
		log.Fatal(err)
	}
	fmt.Printf("created %s with %d packages\n", prefix, len(lf.Packages))
}
