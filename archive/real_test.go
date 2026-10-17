package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false,
	"rewrite testdata/golden from micromamba's extraction (needs GOOMBA_MICROMAMBA)")

// Real packages in testdata. wheel and libzlib come from the micromamba
// package cache; the bzip2 pair and ca-certificates (whose zip uses data
// descriptors) are fixtures of rattler's tests/extract.rs.
var realFixtures = []struct {
	file, dist string
}{
	{"wheel-0.46.3-pyhd8ed1ab_0.conda", "wheel-0.46.3-pyhd8ed1ab_0"},
	{"libzlib-1.3.2-h25fd6f3_2.conda", "libzlib-1.3.2-h25fd6f3_2"},
	{"bzip2-1.0.8-h7f98852_4.tar.bz2", "bzip2-1.0.8-h7f98852_4"},
	{"bzip2-1.0.8-hd590300_5.conda", "bzip2-1.0.8-hd590300_5"},
	{"ca-certificates-2024.7.4-hbcca054_0.conda", "ca-certificates-2024.7.4-hbcca054_0"},
}

func extractFixture(t *testing.T, file string) string {
	t.Helper()
	_, dest := sandbox(t)
	if err := Extract(context.Background(), filepath.Join("testdata", file), dest, Limits{}); err != nil {
		t.Fatal(err)
	}
	return dest
}

type pathsJSON struct {
	Paths []struct {
		Path        string `json:"_path"`
		PathType    string `json:"path_type"`
		SHA256      string `json:"sha256"`
		SizeInBytes *int64 `json:"size_in_bytes"`
	} `json:"paths"`
	PathsVersion int `json:"paths_version"`
}

func fileSHA256(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// TestRealFixturesMatchPathsJSON checks that the extracted tree outside
// info/ is exactly what info/paths.json lists, with matching digests.
func TestRealFixturesMatchPathsJSON(t *testing.T) {
	for _, fx := range realFixtures {
		t.Run(fx.dist, func(t *testing.T) {
			dest := extractFixture(t, fx.file)
			raw, err := os.ReadFile(filepath.Join(dest, "info", "paths.json"))
			if err != nil {
				t.Fatal(err)
			}
			var pj pathsJSON
			if err := json.Unmarshal(raw, &pj); err != nil {
				t.Fatal(err)
			}
			listed := map[string]bool{}
			for _, p := range pj.Paths {
				listed[p.Path] = true
				full := filepath.Join(dest, filepath.FromSlash(p.Path))
				fi, err := os.Lstat(full)
				if err != nil {
					t.Errorf("%s: %v", p.Path, err)
					continue
				}
				switch p.PathType {
				case "hardlink":
					if !fi.Mode().IsRegular() {
						t.Errorf("%s: mode %v, want a regular file", p.Path, fi.Mode())
						continue
					}
					if p.SHA256 != "" && fileSHA256(t, full) != p.SHA256 {
						t.Errorf("%s: sha256 differs from paths.json", p.Path)
					}
					if p.SizeInBytes != nil && fi.Size() != *p.SizeInBytes {
						t.Errorf("%s: size %d, paths.json says %d", p.Path, fi.Size(), *p.SizeInBytes)
					}
				case "softlink":
					if fi.Mode()&os.ModeSymlink == 0 {
						t.Errorf("%s: mode %v, want a symlink", p.Path, fi.Mode())
						continue
					}
					// paths.json hashes the link's target; check it when
					// the target is inside this package.
					if _, err := os.Stat(full); err == nil && p.SHA256 != "" && fileSHA256(t, full) != p.SHA256 {
						t.Errorf("%s: target sha256 differs from paths.json", p.Path)
					}
				case "directory":
					if !fi.IsDir() {
						t.Errorf("%s: mode %v, want a directory", p.Path, fi.Mode())
					}
				default:
					t.Errorf("%s: unknown path_type %q", p.Path, p.PathType)
				}
			}
			// Every file and symlink outside info/ must be listed.
			for rel, n := range snapshot(t, dest) {
				if strings.HasPrefix(rel, "info/") || n.Type == "dir" {
					continue
				}
				if !listed[rel] {
					t.Errorf("%s: extracted but not in paths.json", rel)
				}
			}
			if len(pj.Paths) == 0 {
				t.Fatal("paths.json lists nothing")
			}
		})
	}
}

func goldenPath(dist string) string { return filepath.Join("testdata", "golden", dist+".json") }

// comparable drops what micromamba adds (info/repodata_record.json) and
// what depends on the umask or on extraction order (directory modes).
func comparable(snap map[string]node) map[string]node {
	out := map[string]node{}
	for k, v := range snap {
		if k == "info/repodata_record.json" {
			continue
		}
		out[k] = v
	}
	return out
}

func diffSnapshots(t *testing.T, label string, got, want map[string]node) {
	t.Helper()
	keys := slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
		for k := range got {
			if !yield(k, true) {
				return
			}
		}
		for k := range want {
			if !yield(k, true) {
				return
			}
		}
	})))
	bad := 0
	for _, k := range keys {
		g, gok := got[k]
		w, wok := want[k]
		switch {
		case !gok:
			t.Errorf("%s: %s missing (want %+v)", label, k, w)
		case !wok:
			t.Errorf("%s: %s unexpected (%+v)", label, k, g)
		case g != w:
			t.Errorf("%s: %s = %+v, want %+v", label, k, g, w)
		default:
			continue
		}
		if bad++; bad > 20 {
			t.Fatalf("%s: too many differences", label)
		}
	}
}

// TestRealFixturesMatchMicromambaGolden compares each extraction with
// micromamba's (same files, modes, digests, mtimes and symlink targets),
// recorded in testdata/golden by -update-golden.
func TestRealFixturesMatchMicromambaGolden(t *testing.T) {
	for _, fx := range realFixtures {
		t.Run(fx.dist, func(t *testing.T) {
			raw, err := os.ReadFile(goldenPath(fx.dist))
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]node
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			got := comparable(snapshot(t, extractFixture(t, fx.file)))
			diffSnapshots(t, fx.dist, got, want)
		})
	}
}

// micromambaExtract runs "micromamba package extract" with a scratch root
// prefix inside the worktree's ignored tmp/ directory.
func micromambaExtract(t *testing.T, mm, archive, dest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join("..", "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	rootPrefix, err := os.MkdirTemp(filepath.Join("..", "tmp"), "mmroot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(rootPrefix) })
	absRoot, _ := filepath.Abs(rootPrefix)
	absArchive, _ := filepath.Abs(archive)
	cmd := exec.Command(mm, "package", "extract", absArchive, dest)
	cmd.Env = append(os.Environ(), "MAMBA_ROOT_PREFIX="+absRoot)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("micromamba package extract: %v\n%s", err, out)
	}
}

// TestRealFixturesMatchMicromamba is the live oracle: it extracts every
// fixture with micromamba ($GOOMBA_MICROMAMBA names the binary) and
// compares. With -update-golden it rewrites testdata/golden from
// micromamba's output.
func TestRealFixturesMatchMicromamba(t *testing.T) {
	mm := os.Getenv("GOOMBA_MICROMAMBA")
	if mm == "" {
		t.Skip("set GOOMBA_MICROMAMBA to a micromamba binary")
	}
	for _, fx := range realFixtures {
		t.Run(fx.dist, func(t *testing.T) {
			got := comparable(snapshot(t, extractFixture(t, fx.file)))
			ref := filepath.Join(t.TempDir(), fx.dist)
			micromambaExtract(t, mm, filepath.Join("testdata", fx.file), ref)
			want := comparable(snapshot(t, ref))
			diffSnapshots(t, "micromamba package extract", got, want)
			if !*updateGolden {
				return
			}
			data, err := json.MarshalIndent(want, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(goldenPath(fx.dist)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath(fx.dist), append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRealFixturesMatchPackageCache compares each extraction with the copy
// micromamba extracted into $GOOMBA_REF_ROOT/pkgs/<dist>/, when present.
func TestRealFixturesMatchPackageCache(t *testing.T) {
	root := os.Getenv("GOOMBA_REF_ROOT")
	if root == "" {
		t.Skip("set GOOMBA_REF_ROOT to a micromamba root prefix")
	}
	for _, fx := range realFixtures {
		t.Run(fx.dist, func(t *testing.T) {
			cached := filepath.Join(root, "pkgs", fx.dist)
			if _, err := os.Stat(cached); err != nil {
				t.Skipf("no package cache copy: %v", err)
			}
			got := comparable(snapshot(t, extractFixture(t, fx.file)))
			diffSnapshots(t, "micromamba package cache", got, comparable(snapshot(t, cached)))
		})
	}
}

// Case from rattler tests/extract.rs packages_with_symlinks_extract:
// linux-64 bzip2 ships six symlinks in both archive formats.
func TestPackagesWithSymlinksExtract(t *testing.T) {
	for _, file := range []string{"bzip2-1.0.8-h7f98852_4.tar.bz2", "bzip2-1.0.8-hd590300_5.conda"} {
		t.Run(file, func(t *testing.T) {
			dest := extractFixture(t, file)
			for _, f := range []string{"info/index.json", "bin/bzgrep", "bin/bzmore", "bin/bzdiff", "lib/libbz2.so.1.0.8"} {
				fi, err := os.Lstat(filepath.Join(dest, f))
				if err != nil || !fi.Mode().IsRegular() {
					t.Errorf("%s is not a regular file: %v", f, err)
				}
			}
			for _, l := range [][2]string{
				{"bin/bzfgrep", "bzgrep"},
				{"bin/bzegrep", "bzgrep"},
				{"bin/bzless", "bzmore"},
				{"bin/bzcmp", "bzdiff"},
				{"lib/libbz2.so", "libbz2.so.1.0.8"},
				{"lib/libbz2.so.1.0", "libbz2.so.1.0.8"},
			} {
				got, err := os.Readlink(filepath.Join(dest, l[0]))
				if err != nil || got != l[1] {
					t.Errorf("%s -> %q (%v), want %q", l[0], got, err, l[1])
				}
			}
			// The executable bit survives.
			fi, err := os.Stat(filepath.Join(dest, "bin", "bzip2"))
			if err != nil || fi.Mode().Perm()&0o100 == 0 {
				t.Errorf("bin/bzip2 lost its executable bit: %v %v", fi.Mode(), err)
			}
		})
	}
}

// Case from rattler tests/extract.rs
// test_extract_data_descriptor_package_fails_streaming_and_uses_buffering:
// rattler's streaming reader cannot handle zip data descriptors and falls
// back to buffering; archive/zip reads the central directory, so the package
// extracts directly. The fixture is rattler's, re-zipped (same members,
// Deflate, data descriptors, nested in a directory) so that no user's home
// path from the original member names is committed; hence its own sha256.
func TestExtractDataDescriptorPackage(t *testing.T) {
	const file = "ca-certificates-2024.7.4-hbcca054_0.conda"
	if got := fileSHA256(t, filepath.Join("testdata", file)); got != "7c2904744a9912e7808fe411ecf5c068920d47a55846364c728cbe876ebbb8db" {
		t.Fatalf("fixture sha256 %s", got)
	}
	dest := extractFixture(t, file)
	for _, f := range []string{"info/index.json", "ssl/cacert.pem"} {
		if fi, err := os.Lstat(filepath.Join(dest, f)); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: not a regular file (%v)", f, err)
		}
	}
	if got, err := os.Readlink(filepath.Join(dest, "ssl", "cert.pem")); err != nil || got != "cacert.pem" {
		t.Errorf("ssl/cert.pem -> %q (%v), want cacert.pem", got, err)
	}
}

// TestReferenceCacheMatchesMicromamba extracts every archive in
// $GOOMBA_REF_ROOT/pkgs that micromamba also extracted there, and compares
// the trees (files, modes, digests, mtimes, symlink targets).
func TestReferenceCacheMatchesMicromamba(t *testing.T) {
	root := os.Getenv("GOOMBA_REF_ROOT")
	if root == "" {
		t.Skip("set GOOMBA_REF_ROOT to a micromamba root prefix")
	}
	pkgs := filepath.Join(root, "pkgs")
	archives, err := filepath.Glob(filepath.Join(pkgs, "*.conda"))
	if err != nil {
		t.Fatal(err)
	}
	bz2s, _ := filepath.Glob(filepath.Join(pkgs, "*.tar.bz2"))
	archives = append(archives, bz2s...)
	slices.Sort(archives)
	n := 0
	for _, a := range archives {
		dist := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(a), ".conda"), ".tar.bz2")
		cached := filepath.Join(pkgs, dist)
		if _, err := os.Stat(filepath.Join(cached, "info", "index.json")); err != nil {
			continue
		}
		n++
		t.Run(dist, func(t *testing.T) {
			_, dest := sandbox(t)
			if err := Extract(context.Background(), a, dest, Limits{}); err != nil {
				t.Fatal(err)
			}
			diffSnapshots(t, dist, comparable(snapshot(t, dest)), comparable(snapshot(t, cached)))
		})
	}
	t.Logf("compared %d packages", n)
}
