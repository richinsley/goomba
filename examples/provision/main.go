// Command provision makes sure PREFIX holds the environment a lockfile
// describes, the way a program provisions its own Python at runtime: it
// reuses a finished environment, waits while another process installs it,
// removes one a killed installation left behind, and otherwise installs the
// lockfile. PIP_SPECs, if given, are installed with pip inside goomba's
// transaction, so the environment counts as finished only once pip has
// succeeded.
//
// PREFIX must belong to this lockfile: a finished environment is reused
// without comparing it with the lockfile, so give each lockfile its own
// prefix (for example, one named after the lockfile's sha256).
//
// Make the lockfile with the CLI first; it must include pip if PIP_SPECs
// are given:
//
//	goomba lock -o env.lock.json python=3.12 pip
//	go run ./examples/provision env.lock.json ./env six==1.17.0
//
// The pip step assumes a Unix layout (bin/python).
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/richinsley/goomba"
	"github.com/richinsley/goomba/lock"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: provision LOCKFILE PREFIX [PIP_SPEC...]")
	}
	lockFile, prefix, pipSpecs := os.Args[1], os.Args[2], os.Args[3:]

	f, err := os.Open(lockFile)
	if err != nil {
		log.Fatal(err)
	}
	lf, err := lock.Read(f) // strict: validates everything it returns
	f.Close()
	if err != nil {
		log.Fatal(err)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		log.Fatal(err)
	}
	opt := goomba.InstallOptions{
		PkgsDir:    filepath.Join(cache, "goomba", "pkgs"),
		CompilePyc: true,
		// The caller's last step, still inside the transaction.
		BeforeCommit: func(ctx context.Context, prefix string) error {
			return pipInstall(ctx, prefix, pipSpecs)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := ensure(ctx, prefix, lf, opt); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s is ready\n", prefix)
}

// ensure returns once prefix holds a finished environment, installing lf
// there if it holds none.
func ensure(ctx context.Context, prefix string, lf *lock.Lockfile, opt goomba.InstallOptions) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := goomba.CheckComplete(prefix)
		switch {
		case err == nil:
			// Finished, by goomba, micromamba or conda: reuse it.
			return nil

		case errors.Is(err, goomba.ErrPrefixLocked):
			// Another process is installing, rolling back or removing it.
			if err := pause(ctx); err != nil {
				return err
			}

		case errors.Is(err, goomba.ErrIncomplete):
			// A killed installation left it. goomba never resumes one:
			// remove it and install again. Another caller may have got
			// there first: then the prefix is locked, gone, or already
			// reinstalled (ErrNotIncomplete), and we look again.
			err := goomba.RemoveIncomplete(prefix)
			switch {
			case errors.Is(err, goomba.ErrPrefixLocked):
				if err := pause(ctx); err != nil {
					return err
				}
			case err != nil && !errors.Is(err, goomba.ErrNotIncomplete) && !errors.Is(err, fs.ErrNotExist):
				return err // ErrMountPoint, ErrUnsafePrefix, I/O errors
			}

		case errors.Is(err, fs.ErrNotExist):
			// Missing, an empty directory, or a directory that holds no
			// environment, which InstallLock refuses with ErrPrefixNotEmpty.
			err := goomba.InstallLock(ctx, prefix, lf, opt)
			switch {
			case errors.Is(err, goomba.ErrPrefixLocked):
				// Another installer took the prefix first.
				if err := pause(ctx); err != nil {
					return err
				}
			case errors.Is(err, goomba.ErrPrefixNotEmpty):
				// Another installer's prefix, or a directory of something
				// else; only the second still looks like that now.
				if foreign(prefix) {
					return err
				}
			default:
				// nil, or the failure (failures.md says what it leaves).
				return err
			}

		default:
			// ErrUnsafePrefix, a dangling symlink (fs.ErrExist), I/O errors.
			return err
		}
	}
}

// pause waits a moment before ensure looks at the prefix again.
func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}

// foreign reports whether prefix is a directory that is not empty, holds no
// environment and is not being installed into: one InstallLock refuses.
func foreign(prefix string) bool {
	if !errors.Is(goomba.CheckComplete(prefix), fs.ErrNotExist) {
		return false
	}
	ents, err := os.ReadDir(prefix)
	return err == nil && len(ents) > 0
}

// pipInstall installs specs into the environment at prefix with its own pip.
// It runs inside the transaction: if it fails, or ctx ends, goomba rolls the
// whole environment back.
func pipInstall(ctx context.Context, prefix string, specs []string) error {
	if len(specs) == 0 {
		return nil
	}
	// -I and --isolated keep Python and pip from reading PYTHON* and PIP_*
	// variables, the user's site directory and the user's pip configuration;
	// PIP_CONFIG_FILE=/dev/null keeps pip from reading any configuration
	// file, the system's and the environment's too.
	args := append([]string{"-I", "-m", "pip", "install", "--isolated", "--no-input", "--disable-pip-version-check"}, specs...)
	cmd := exec.CommandContext(ctx, filepath.Join(prefix, "bin", "python"), args...)
	cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE="+os.DevNull)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// CommandContext kills pip when ctx ends, but not the processes pip
	// started; this hook must not return while they still run (see
	// failures.md).
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pip install: %w", err)
	}
	return nil
}
