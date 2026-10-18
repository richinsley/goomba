package goomba

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/richinsley/goomba/link"
)

// TestInstallNonNormalSymlinkTargets installs, through archive and link,
// packages shaped like conda-forge's ncurses-6.6-hdb14827_1, whose terminfo
// symlinks have targets such as ".././61/adm1178" (WP8c): the targets are
// written as shipped, the clobber check and the final symlink check accept
// them, and their digests are those of the files they reach, also from
// another package.
func TestInstallNonNormalSymlinkTargets(t *testing.T) {
	c := newChannel(t)
	ncurses := buildPkg(t, tspec{name: "ncurses", version: "6.6", build: "hdb14827_1", files: []tfile{
		{path: "share/terminfo/61/adm1178", data: "adm1178\n"},
		{path: "share/terminfo/31/1178", link: ".././61/adm1178"},
		{path: "share/terminfo/45/Eterm", link: ".././/61/./adm1178"},
		{path: "share/terminfo/E", link: "./45/"},
	}})
	user := buildPkg(t, tspec{name: "user", version: "1", build: "0", depends: []string{"ncurses"}, files: []tfile{
		{path: "lib/terminfo-1178", link: "./../share/terminfo/./31/1178"},
		{path: "lib/terminfo", link: "../share//terminfo/"},
	}})
	lf := c.lockfile(t, []string{"user"}, ncurses, user)
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, InstallLock(context.Background(), prefix, lf, c.options(t)))

	want := sha256.Sum256([]byte("adm1178\n"))
	digest := hex.EncodeToString(want[:])
	for dist, links := range map[string]map[string]string{
		"ncurses-6.6-hdb14827_1": {"share/terminfo/31/1178": ".././61/adm1178", "share/terminfo/45/Eterm": ".././/61/./adm1178", "share/terminfo/E": "./45/"},
		"user-1-0":               {"lib/terminfo-1178": "./../share/terminfo/./31/1178", "lib/terminfo": "../share//terminfo/"},
	} {
		rec := readCondaMetaJSON(t, prefix, dist)
		paths := rec["paths_data"].(map[string]any)["paths"].([]any)
		for _, p := range paths {
			p := p.(map[string]any)
			rel := p["_path"].(string)
			target, ok := links[rel]
			if !ok {
				continue
			}
			if got, err := os.Readlink(filepath.Join(prefix, rel)); err != nil || got != target {
				t.Errorf("%s -> %q, %v; want %q", rel, got, err, target)
			}
			if p["path_type"] != "softlink" {
				t.Errorf("%s: path_type %v", rel, p["path_type"])
			}
			if rel == "share/terminfo/E" || rel == "lib/terminfo" {
				continue // directories: the empty digest
			}
			if p["sha256_in_prefix"] != digest {
				t.Errorf("%s: sha256_in_prefix %v, want %s", rel, p["sha256_in_prefix"], digest)
			}
		}
	}
	for _, rel := range []string{"lib/terminfo/E/Eterm", "lib/terminfo-1178"} {
		data, err := os.ReadFile(filepath.Join(prefix, rel))
		if err != nil || string(data) != "adm1178\n" {
			t.Errorf("%s: %q, %v", rel, data, err)
		}
	}
	if err := CheckComplete(prefix); err != nil {
		t.Errorf("CheckComplete: %v", err)
	}

	// ".." after a name stays refused, and nothing is left behind.
	bad := buildPkg(t, tspec{name: "bad", version: "1", build: "0", files: []tfile{
		{path: "share/x/l", link: "y/../../.."},
	}})
	prefix = filepath.Join(t.TempDir(), "bad")
	err := InstallLock(context.Background(), prefix, c.lockfile(t, nil, bad), c.options(t))
	if !errors.Is(err, link.ErrUnsafePath) {
		t.Fatalf("err = %v, want link.ErrUnsafePath", err)
	}
	assertNoPrefix(t, prefix)
}

// TestSymlinkDigestTrailingSlash (review of WP8c, N1): a symlink whose
// target ends in "/" or "/." after a file leads nowhere (ENOTDIR), so its
// sha256_in_prefix is the empty digest, as libmamba records; "./f" is the
// file's.
func TestSymlinkDigestTrailingSlash(t *testing.T) {
	c := newChannel(t)
	p := buildPkg(t, tspec{name: "p", version: "1", build: "0", files: []tfile{
		{path: "share/f", data: "data\n"},
		{path: "share/l1", link: "f/"},
		{path: "share/l2", link: "f/."},
		{path: "share/l3", link: "./f"},
		{path: "share/l4", link: "../share//f"},
		{path: "share/l5", link: "l3/"},
	}})
	prefix := filepath.Join(t.TempDir(), "env")
	must(t, InstallLock(context.Background(), prefix, c.lockfile(t, []string{"p"}, p), c.options(t)))
	sum := sha256.Sum256([]byte("data\n"))
	file := hex.EncodeToString(sum[:])
	want := map[string]string{"share/f": file, "share/l1": emptySHA256, "share/l2": emptySHA256, "share/l3": file, "share/l4": file, "share/l5": emptySHA256}
	rec := readCondaMetaJSON(t, prefix, "p-1-0")
	for _, x := range rec["paths_data"].(map[string]any)["paths"].([]any) {
		m := x.(map[string]any)
		rel := m["_path"].(string)
		if m["sha256_in_prefix"] != want[rel] {
			t.Errorf("%s: sha256_in_prefix %v, want %s", rel, m["sha256_in_prefix"], want[rel])
		}
		_, statErr := os.Stat(filepath.Join(prefix, rel))
		if (statErr == nil) != (want[rel] == file) {
			t.Errorf("%s: stat %v", rel, statErr)
		}
	}
}
