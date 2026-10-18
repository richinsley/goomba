// Command lock resolves match specs into one lockfile per platform, the
// way a project locks its environments at development time for review.
// Each platform is solved against conservative virtual packages
// (virtual.Defaults), not the machine running the solve, so the result does
// not depend on who ran it.
//
//	go run ./examples/lock ./locks python=3.12 pip
//
// writes ./locks/linux-64.lock.json, ./locks/linux-aarch64.lock.json,
// ./locks/osx-64.lock.json and ./locks/osx-arm64.lock.json.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/richinsley/goomba"
	"github.com/richinsley/goomba/virtual"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: lock OUTDIR SPEC...")
	}
	outDir, specs := os.Args[1], os.Args[2:]
	cache, err := os.UserCacheDir()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	for _, platform := range []string{"linux-64", "linux-aarch64", "osx-64", "osx-arm64"} {
		lf, err := goomba.Lock(ctx, goomba.LockOptions{
			Specs:       specs,
			Channels:    []string{"conda-forge"},
			Platform:    platform,
			Virtual:     virtual.Defaults(platform),
			RepodataDir: filepath.Join(cache, "goomba", "repodata"),
		})
		if err != nil {
			log.Fatalf("%s: %v", platform, err)
		}
		name := filepath.Join(outDir, platform+".lock.json")
		f, err := os.Create(name)
		if err != nil {
			log.Fatal(err)
		}
		// Write produces canonical JSON: the same lockfile, the same bytes.
		if err := lf.Write(f); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %d packages\n", name, len(lf.Packages))
	}
}
