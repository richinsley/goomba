package goomba

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/richinsley/goomba/lock"
)

// rattlerClobberPkgs loads packages from testdata/clobber, rattler's
// test-data/clobber archives.
func rattlerClobberPkgs(t *testing.T, files ...string) []testPkg {
	var out []testPkg
	for _, f := range files {
		out = append(out, archivePkg(t, filepath.Join("testdata", "clobber", f)))
	}
	return out
}

// Cases from rattler crates/rattler/src/install/clobber_registry.rs
// (test_transaction_with_clobber, test_all_possible_orders_clobber,
// test_all_possible_orders_clobber_nested, test_clobber_python_noarch,
// test_direct_file_and_directory_clobber,
// test_file_and_directory_clobber_with_merging,
// test_clobber_with_symlinks_and_reprioritization, test_clobber_mode_error),
// with rattler's test-data/clobber archives. rattler's default mode keeps
// the first package's file and moves the others' to __clobbers__/<name>/;
// goomba behaves like rattler's ClobberMode::Error and refuses the
// transaction, naming the path and the two packages, whatever the order of
// the lockfile. The update and removal cases (test_clobber_update,
// test_self_clobber_update, test_clobber_update_and_remove,
// test_transaction_with_clobber_remove_all, test_dir_to_file_upgrade,
// test_dependency_clobber) and test_clobber_mode_rename_is_default do not
// apply: InstallLock only creates new environments.
func TestRattlerClobberCases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    []string
		python   bool
		path     string
		packages [2]string
	}{
		{
			name:     "transaction with clobber",
			files:    []string{"clobber-1-0.1.0-h4616a5c_0.tar.bz2", "clobber-2-0.1.0-h4616a5c_0.tar.bz2", "clobber-3-0.1.0-h4616a5c_0.tar.bz2"},
			path:     "another-clobber.txt",
			packages: [2]string{"clobber-1-0.1.0-h4616a5c_0", "clobber-2-0.1.0-h4616a5c_0"},
		},
		{
			name:     "nested",
			files:    []string{"clobber-nested-1-0.1.0-h4616a5c_0.tar.bz2", "clobber-nested-2-0.1.0-h4616a5c_0.tar.bz2", "clobber-nested-3-0.1.0-h4616a5c_0.tar.bz2"},
			path:     "clobber/bobber/clobber.txt",
			packages: [2]string{"clobber-nested-1-0.1.0-h4616a5c_0", "clobber-nested-2-0.1.0-h4616a5c_0"},
		},
		{
			name:     "python noarch",
			files:    []string{"clobber-pynoarch-1-0.1.0-pyh4616a5c_0.tar.bz2", "clobber-pynoarch-2-0.1.0-pyh4616a5c_0.tar.bz2"},
			python:   true,
			path:     "lib/python3.11/site-packages/clobber/clobber.py",
			packages: [2]string{"clobber-pynoarch-1-0.1.0-pyh4616a5c_0", "clobber-pynoarch-2-0.1.0-pyh4616a5c_0"},
		},
		{
			name:     "direct file and directory",
			files:    []string{"clobber-with-dir-0.1.0-h4616a5c_0.conda", "clobber-without-dir-0.1.0-h4616a5c_0.conda"},
			path:     "dir",
			packages: [2]string{"clobber-with-dir-0.1.0-h4616a5c_0", "clobber-without-dir-0.1.0-h4616a5c_0"},
		},
		{
			name:     "file and directory with merging",
			files:    []string{"clobber-fd-rev-3-0.1.0-h4616a5c_0.conda", "clobber-fd-rev-2-0.1.0-h4616a5c_0.conda", "clobber-fd-rev-1-0.1.0-h4616a5c_0.conda"},
			path:     "clobber",
			packages: [2]string{"clobber-fd-rev-1-0.1.0-h4616a5c_0", "clobber-fd-rev-3-0.1.0-h4616a5c_0"},
		},
		{
			name:     "symlinks",
			files:    []string{"clobber-with-symlink-a-0.1.0-h4616a5c_0.conda", "clobber-with-symlink-b-0.1.0-h4616a5c_0.conda"},
			path:     "lib/clobber.so",
			packages: [2]string{"clobber-with-symlink-a-0.1.0-h4616a5c_0", "clobber-with-symlink-b-0.1.0-h4616a5c_0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newChannel(t)
			pkgs := rattlerClobberPkgs(t, tc.files...)
			if tc.python {
				pkgs = append(pkgs, fakePython(t, t.TempDir(), 0))
			}
			lf := c.lockfile(t, nil, pkgs...)
			opt := c.options(t)
			// Every order of the lockfile's packages gives the same error
			// (test_all_possible_orders_clobber).
			for _, perm := range permutations(len(lf.Packages)) {
				l := *lf
				l.Packages = make([]lock.Package, len(perm))
				for i, j := range perm {
					l.Packages[i] = lf.Packages[j]
				}
				prefix := filepath.Join(t.TempDir(), "env")
				err := InstallLock(context.Background(), prefix, &l, opt)
				var ce *ClobberError
				if !errors.As(err, &ce) {
					t.Fatalf("order %v: err = %v, want a ClobberError", perm, err)
				}
				if ce.Path != tc.path || ce.Packages != tc.packages {
					t.Fatalf("order %v: clobber %q %v, want %q %v", perm, ce.Path, ce.Packages, tc.path, tc.packages)
				}
				if _, err := os.Lstat(prefix); !os.IsNotExist(err) {
					t.Fatalf("prefix left behind: %v", err)
				}
			}
		})
	}
}

// permutations returns every ordering of 0..n-1.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := slices.Insert(slices.Clone(p), i, n-1)
			out = append(out, q)
		}
	}
	return out
}
