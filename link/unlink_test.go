package link

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/richinsley/goomba/record"
)

// linkAndRecord links a package and writes its conda-meta record.
func linkAndRecord(t *testing.T, pkgDir string, rec *record.PackageRecord, opt Options) *Installed {
	t.Helper()
	inst, err := Link(context.Background(), pkgDir, rec, opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCondaMeta(opt.Prefix, rec, inst, opt); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestUnlinkSharedDirectories(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	pkgs := filepath.Join(tmp, "pkgs")
	aDir, a := buildPkg(t, pkgs, pkgSpec{name: "a", files: []pkgFile{{path: "lib/a/x", data: "1"}, {path: "lib/shared", data: "a"}, {path: "share/a-link", symlink: "../share/b"}}})
	bDir, b := buildPkg(t, pkgs, pkgSpec{name: "b", files: []pkgFile{{path: "lib/b", data: "2"}, {path: "share/b/file", data: "3"}}})
	prefix := filepath.Join(tmp, "env")
	opt := Options{Prefix: prefix, Platform: "linux-64"}
	linkAndRecord(t, aDir, a, opt)
	linkAndRecord(t, bDir, b, opt)
	// A directory created by the user stays when it is not empty.
	if err := os.WriteFile(filepath.Join(prefix, "lib/a/user-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Unlink(ctx, prefix, a); err != nil {
		t.Fatal(err)
	}
	want := []string{"conda-meta/", "conda-meta/b-1.0-h0_0.json", "lib/", "lib/a/", "lib/a/user-file", "lib/b", "share/", "share/b/", "share/b/file"}
	if got := tree(t, prefix); !reflect.DeepEqual(got, want) {
		t.Errorf("after unlinking a:\n got %q\nwant %q", got, want)
	}
	if err := Unlink(ctx, prefix, b); err != nil {
		t.Fatal(err)
	}
	want = []string{"lib/", "lib/a/", "lib/a/user-file"}
	if got := tree(t, prefix); !reflect.DeepEqual(got, want) {
		t.Errorf("after unlinking b:\n got %q\nwant %q", got, want)
	}
	if err := Unlink(ctx, prefix, b); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("second Unlink: %v, want ErrNotInstalled", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Unlink(canceled, prefix, a); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Unlink: %v", err)
	}
}

// Cases from libmamba tests/src/core/test_transaction_context.cpp,
// TEST_CASE("unlink noarch metadata paths"): records that store
// package-relative site-packages paths, in paths_data or in the legacy files
// list, are resolved against the environment's python.
func TestUnlinkNoarchShortPaths(t *testing.T) {
	for _, doc := range []string{
		`{"name": "httpx", "version": "0.27.2", "build_string": "pyhd8ed1ab_0",
		  "paths_data": {"paths": [{"_path": "site-packages/httpx/__init__.py"}, {"_path": "site-packages/httpx-0.27.2.dist-info/METADATA"}], "paths_version": 1}}`,
		`{"name": "httpx", "version": "0.27.2", "build_string": "pyhd8ed1ab_0",
		  "files": ["site-packages/httpx/__init__.py", "site-packages/httpx-0.27.2.dist-info/METADATA"]}`,
	} {
		prefix := t.TempDir()
		sp := filepath.Join(prefix, "lib/python3.14/site-packages")
		for _, f := range []string{"httpx/__init__.py", "httpx-0.27.2.dist-info/METADATA"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(sp, f)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sp, f), []byte("old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		meta := filepath.Join(prefix, "conda-meta")
		if err := os.MkdirAll(meta, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(meta, "httpx-0.27.2-pyhd8ed1ab_0.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		python := `{"name": "python", "version": "3.14.4", "build": "h0_cp314", "subdir": "linux-64", "files": []}`
		if err := os.WriteFile(filepath.Join(meta, "python-3.14.4-h0_cp314.json"), []byte(python), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := &record.PackageRecord{Name: "httpx", Version: "0.27.2", Build: "pyhd8ed1ab_0"}
		if err := Unlink(context.Background(), prefix, rec); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"httpx/__init__.py", "httpx-0.27.2.dist-info/METADATA"} {
			if _, err := os.Lstat(filepath.Join(sp, f)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s still exists", f)
			}
		}
	}
}

// Unlink confines a (possibly tampered) record to the prefix and removes
// symlinks rather than what they point to.
func TestUnlinkConfined(t *testing.T) {
	tmp := t.TempDir()
	prefix := filepath.Join(tmp, "env")
	outside := filepath.Join(tmp, "outside")
	for _, d := range []string{filepath.Join(prefix, "conda-meta"), filepath.Join(prefix, "lib"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(prefix, "lib/out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(prefix, "dirlink")); err != nil {
		t.Fatal(err)
	}
	rec := &record.PackageRecord{Name: "evil", Version: "1", Build: "0"}
	doc := `{"name": "evil", "version": "1", "build": "0", "paths_data": {"paths": [
		{"_path": "../outside/keep"}, {"_path": "lib/out/keep"}, {"_path": "/etc/passwd"}, {"_path": "conda-meta/history"}]}}`
	metaFile := filepath.Join(prefix, "conda-meta", "evil-1-0.json")
	if err := os.WriteFile(metaFile, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Unlink(context.Background(), prefix, rec)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("file outside the prefix removed: %v", err)
	}
	if _, err := os.Stat(metaFile); err != nil {
		t.Errorf("record removed after a failed unlink: %v", err)
	}

	// A symlink to a directory is removed, its target kept.
	doc = `{"name": "evil", "version": "1", "build": "0", "files": ["dirlink", "lib/out"]}`
	if err := os.WriteFile(metaFile, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(context.Background(), prefix, rec); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("prefix: %q", got)
	}
	if got := tree(t, outside); !reflect.DeepEqual(got, []string{"keep"}) {
		t.Errorf("outside: %q", got)
	}
}
