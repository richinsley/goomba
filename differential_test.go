package goomba

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
)

// emptyDigest is the sha256 of no bytes, recorded for symlinks that do not
// resolve to a file.
const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// TestDifferentialMicromamba installs the reference environment
// ($GOOMBA_REF_ROOT/envs/base-3.11, and base-3.12 if present) twice from one
// lockfile: with goomba,
// and with `micromamba create -f` from the lockfile's @EXPLICIT export, and
// compares the trees. Allowed differences, all documented in the package
// doc:
//   - files never have group or other write in goomba (packages' 0664
//     files are 0644, entry points 0755 against micromamba's 0775); only
//     those bits may differ;
//   - sha256_in_prefix of files whose prefix was rewritten, and of symlinks
//     to them, is the real digest in goomba where micromamba 2.2.0 writes
//     one stale constant;
//   - micromamba 2.2.0's explicit install leaves build_number, depends,
//     constrains, license, timestamp and sha256 empty and records one exact
//     requested_spec per package; goomba's values must equal those of the
//     reference environment, which micromamba created by solving;
//   - the history lists the same packages, in goomba's link order, with the
//     lockfile's specs.
//
// Both prefixes have the same length, so that binary files whose prefix was
// rewritten compare equal once the prefix is replaced. micromamba runs with
// a scratch root prefix inside the module's tmp directory (git-ignored),
// whose package cache is kept between runs.
func TestDifferentialMicromamba(t *testing.T) {
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	mm, refRoot := os.Getenv("GOOMBA_MICROMAMBA"), os.Getenv("GOOMBA_REF_ROOT")
	if mm == "" || refRoot == "" {
		t.Skip("set GOOMBA_MICROMAMBA and GOOMBA_REF_ROOT")
	}
	// base-3.11 is the documented reference environment; base-3.12, whose
	// python depends on pip (a python <-> pip cycle), is used when present.
	for _, env := range []string{"base-3.11", "base-3.12"} {
		t.Run(env, func(t *testing.T) {
			ref := filepath.Join(refRoot, "envs", env)
			if _, err := os.Stat(filepath.Join(ref, "conda-meta")); err != nil {
				t.Skipf("no reference environment %s", ref)
			}
			differential(t, mm, ref)
		})
	}
}

func differential(t *testing.T, mm, ref string) {
	lf, err := lock.FromPrefix(ref, "")
	must(t, err)
	if lf.Platform != hostPlatform(t) {
		t.Skipf("reference environment is for %s", lf.Platform)
	}

	scratch, err := filepath.Abs(filepath.Join("tmp", "differential"))
	must(t, err)
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	run := filepath.Join(scratch, "run-"+hex.EncodeToString(rnd[:]))
	must(t, os.MkdirAll(run, 0o755))
	defer os.RemoveAll(run)
	gPrefix, mPrefix := filepath.Join(run, "gmb"), filepath.Join(run, "mmb")

	explicit := filepath.Join(run, "explicit.txt")
	var buf bytes.Buffer
	must(t, lf.WriteExplicit(&buf))
	must(t, os.WriteFile(explicit, buf.Bytes(), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	start := time.Now()
	must(t, InstallLock(ctx, gPrefix, lf, InstallOptions{
		PkgsDir:    filepath.Join(scratch, "goomba-pkgs"),
		CompilePyc: true,
	}))
	t.Logf("goomba: %v", time.Since(start))

	home := filepath.Join(run, "home")
	must(t, os.MkdirAll(home, 0o755))
	cmd := exec.CommandContext(ctx, mm, "create", "--no-rc", "-y", "-p", mPrefix, "-f", explicit)
	cmd.Env = []string{"MAMBA_ROOT_PREFIX=" + filepath.Join(scratch, "mmroot"), "HOME=" + home}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	start = time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("micromamba: %v\n%s", err, out)
	}
	t.Logf("micromamba: %v", time.Since(start))

	g, m := scanTree(t, gPrefix), scanTree(t, mPrefix)
	compareTrees(t, gPrefix, mPrefix, g, m, true)
	compareCondaMeta(t, gPrefix, mPrefix, ref, lf)

	py := exec.CommandContext(ctx, filepath.Join(gPrefix, "bin", "python"), "-c", "import ssl, sqlite3, ctypes, lzma, bz2, zlib, json; print('ok')")
	py.Env = []string{}
	if out, err := py.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("goomba environment's python: %v\n%s", err, out)
	}
}

// node is one path of a scanned tree.
type node struct {
	mode   fs.FileMode
	target string // symlink target
	data   []byte // regular file contents
}

func scanTree(t *testing.T, root string) map[string]node {
	t.Helper()
	out := map[string]node{}
	must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		n := node{mode: fi.Mode()}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			n.target, err = os.Readlink(p)
		case fi.Mode().IsRegular():
			n.data, err = os.ReadFile(p)
		}
		out[filepath.ToSlash(rel)] = n
		return err
	}))
	return out
}

func compareTrees(t *testing.T, gPrefix, mPrefix string, g, m map[string]node, python bool) {
	t.Helper()
	entryPoints := map[string]bool{}
	for p := range g {
		if strings.HasPrefix(p, "conda-meta/") && strings.HasSuffix(p, ".json") {
			var rec struct {
				PathsData struct {
					Paths []struct {
						Path     string `json:"_path"`
						PathType string `json:"path_type"`
					} `json:"paths"`
				} `json:"paths_data"`
			}
			must(t, json.Unmarshal(g[p].data, &rec))
			for _, e := range rec.PathsData.Paths {
				if e.PathType == "unix_python_entry_point" {
					entryPoints[e.Path] = true
				}
			}
		}
	}
	var only []string
	for _, p := range sortedKeys(g) {
		if _, ok := m[p]; !ok {
			only = append(only, "goomba only: "+p)
		}
	}
	for _, p := range sortedKeys(m) {
		if _, ok := g[p]; !ok {
			only = append(only, "micromamba only: "+p)
		}
	}
	if len(only) > 0 {
		t.Errorf("path sets differ (%d):\n%s", len(only), strings.Join(only[:min(len(only), 40)], "\n"))
	}
	pycs, files, links, modeDiffs := 0, 0, 0, 0
	for _, p := range sortedKeys(g) {
		a, ok := g[p]
		b, ok2 := m[p]
		if !ok || !ok2 {
			continue
		}
		if a.mode.Type() != b.mode.Type() {
			t.Errorf("%s: type %v vs %v", p, a.mode.Type(), b.mode.Type())
			continue
		}
		switch {
		case a.mode&fs.ModeSymlink != 0:
			links++
			if a.target != b.target {
				t.Errorf("%s: symlink to %q vs %q", p, a.target, b.target)
			}
		case a.mode.IsRegular():
			// goomba never gives group or others write permission
			// (package files: archive masks 0755; entry points: 0755
			// against micromamba's 0775); those bits alone may differ.
			if a.mode.Perm() != b.mode.Perm()&^0o022 {
				t.Errorf("%s: mode %v vs %v", p, a.mode.Perm(), b.mode.Perm())
			}
			if a.mode.Perm() != b.mode.Perm() {
				modeDiffs++
			}
			switch {
			case strings.HasSuffix(p, ".pyc"):
				pycs++ // the .pyc set is compared; contents embed paths and mtimes
			case strings.HasPrefix(p, "conda-meta/"):
			default:
				files++
				if !bytes.Equal(bytes.ReplaceAll(a.data, []byte(gPrefix), []byte(mPrefix)), b.data) {
					t.Errorf("%s: contents differ", p)
				}
			}
		}
	}
	t.Logf("compared %d paths: %d files, %d symlinks, %d .pyc, %d entry points; %d files without micromamba's group or other write", len(g), files, links, pycs, len(entryPoints), modeDiffs)
	if python && (pycs == 0 || len(entryPoints) == 0) {
		t.Error("no .pyc files or entry points compared")
	}
}

// compareCondaMeta compares goomba's conda-meta records with micromamba's
// (paths and files) and with the reference environment's (repodata
// fields), and the history files.
func compareCondaMeta(t *testing.T, gPrefix, mPrefix, ref string, lf *lock.Lockfile) {
	t.Helper()
	gPkgs := filepath.Dir(filepath.Dir(gPrefix)) // tmp/differential
	// Fields micromamba 2.2.0's explicit install leaves empty or sets per
	// package; goomba's must equal the solved reference's instead.
	fromRef := []string{"build_number", "constrains", "depends", "license", "timestamp", "sha256", "requested_spec", "track_features"}
	cacheKeys := []string{"extracted_package_dir", "link", "package_tarball_full_path"}
	stale := ""
	for _, p := range lf.Packages {
		dist := p.Name + "-" + p.Version + "-" + p.Build
		gr := readCondaMetaJSON(t, gPrefix, dist)
		mr := readCondaMetaJSON(t, mPrefix, dist)
		rr := readCondaMetaJSON(t, ref, dist)
		for _, k := range sortedKeys(mergeKeys(gr, mr, rr)) {
			switch {
			case k == "paths_data":
				stale = comparePaths(t, gPrefix, dist, gr[k], mr[k], stale)
			case slices.Contains(cacheKeys, k):
				if !strings.Contains(fmt.Sprint(gr[k]), gPkgs) {
					t.Errorf("%s: %s = %v is not in goomba's cache", dist, k, gr[k])
				}
			case slices.Contains(fromRef, k):
				if fmt.Sprint(gr[k]) != fmt.Sprint(rr[k]) {
					t.Errorf("%s: %s = %v, reference %v", dist, k, gr[k], rr[k])
				}
			default:
				if fmt.Sprint(gr[k]) != fmt.Sprint(mr[k]) {
					t.Errorf("%s: %s = %v, micromamba %v", dist, k, gr[k], mr[k])
				}
			}
		}
	}
	gh, err := os.ReadFile(filepath.Join(gPrefix, "conda-meta", "history"))
	must(t, err)
	mh, err := os.ReadFile(filepath.Join(mPrefix, "conda-meta", "history"))
	must(t, err)
	added := func(h []byte) []string {
		var out []string
		for _, l := range strings.Split(string(h), "\n") {
			if strings.HasPrefix(l, "+") {
				out = append(out, l)
			}
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(added(gh), added(mh)) {
		t.Errorf("history packages differ:\n%s\nvs\n%s", gh, mh)
	}
	quoted := make([]string, len(lf.Specs))
	for i, s := range lf.Specs {
		quoted[i] = `"` + s + `"`
	}
	if want := "# update specs: [" + strings.Join(quoted, ", ") + "]\n"; !strings.HasSuffix(string(gh), want) {
		t.Errorf("history does not end with %q:\n%s", want, gh)
	}
}

func mergeKeys(ms ...map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			out[k] = true
		}
	}
	return out
}

// comparePaths compares paths_data entries. Where sha256_in_prefix differs,
// goomba's must be the real digest of what the path resolves to in its
// prefix and micromamba's the one stale value it writes for every rewritten
// file; stale is that value once seen.
func comparePaths(t *testing.T, gPrefix, dist string, g, m any, stale string) string {
	t.Helper()
	type entry = map[string]any
	list := func(v any) []entry {
		var out []entry
		raw, _ := json.Marshal(v.(map[string]any)["paths"])
		must(t, json.Unmarshal(raw, &out))
		return out
	}
	ge, me := list(g), list(m)
	if len(ge) != len(me) {
		t.Errorf("%s: %d paths vs %d", dist, len(ge), len(me))
		return stale
	}
	for i := range ge {
		a, b := ge[i], me[i]
		da, db := a["sha256_in_prefix"], b["sha256_in_prefix"]
		delete(a, "sha256_in_prefix")
		delete(b, "sha256_in_prefix")
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("%s: paths_data[%d] %v vs %v", dist, i, a, b)
			continue
		}
		if da == db {
			continue
		}
		p := filepath.Join(gPrefix, filepath.FromSlash(a["_path"].(string)))
		real := emptyDigest
		if data, err := os.ReadFile(p); err == nil {
			s := sha256.Sum256(data)
			real = hex.EncodeToString(s[:])
		}
		if da != real {
			t.Errorf("%s: %s sha256_in_prefix %v, the file's digest is %s", dist, a["_path"], da, real)
		}
		if a["path_type"] == "hardlink" && a["sha256"] == real {
			t.Errorf("%s: %s: digest differs from micromamba's for an unchanged file", dist, a["_path"])
		}
		if stale == "" {
			stale, _ = db.(string)
		}
		if db != stale {
			t.Errorf("%s: %s: micromamba digest %v is not the stale constant %s", dist, a["_path"], db, stale)
		}
	}
	return stale
}

// TestDifferentialOpenmpCycle installs the five packages of rattler's
// python test data that form the cycle libzlib -> libgcc-ng ->
// _openmp_mutex -> llvm-openmp -> libzlib, where _openmp_mutex's
// lib/libgomp.so.1 points to llvm-openmp's libomp.so, with goomba and with
// micromamba. goomba records the digest of libomp.so for the symlink,
// whatever the link order; micromamba records what it found when it linked
// _openmp_mutex. The trees must otherwise agree.
func TestDifferentialOpenmpCycle(t *testing.T) {
	mm := os.Getenv("GOOMBA_MICROMAMBA")
	if os.Getenv("GOOMBA_LIVE") == "" || mm == "" {
		t.Skip("set GOOMBA_LIVE=1 and GOOMBA_MICROMAMBA")
	}
	if hostPlatform(t) != "linux-64" {
		t.Skip("linux-64 packages")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "topological-sort", "python.json"))
	must(t, err)
	var raw []struct {
		Name, Version, Build, Subdir, URL, SHA256, MD5 string
		BuildNumber                                    uint64 `json:"build_number"`
		Depends                                        []string
		Size                                           int64
	}
	must(t, json.Unmarshal(data, &raw))
	want := []string{"_libgcc_mutex", "_openmp_mutex", "libgcc-ng", "libzlib", "llvm-openmp"}
	var recs []*record.PackageRecord
	explicit := "@EXPLICIT\n"
	for _, r := range raw {
		if !slices.Contains(want, r.Name) {
			continue
		}
		recs = append(recs, &record.PackageRecord{Name: r.Name, Version: r.Version, Build: r.Build, BuildNumber: r.BuildNumber,
			Subdir: r.Subdir, URL: r.URL, SHA256: r.SHA256, MD5: r.MD5, Size: r.Size, Depends: r.Depends})
		explicit += r.URL + "#" + r.MD5 + "\n"
	}
	lf, err := lock.FromRecords("linux-64", []string{"https://conda.anaconda.org/conda-forge"}, nil, map[string]string{}, recs)
	must(t, err)
	scratch, err := filepath.Abs(filepath.Join("tmp", "differential"))
	must(t, err)
	must(t, os.MkdirAll(scratch, 0o755))
	run, err := os.MkdirTemp(scratch, "cycle-")
	must(t, err)
	defer os.RemoveAll(run)
	g, m := filepath.Join(run, "gmb"), filepath.Join(run, "mmb")
	must(t, InstallLock(context.Background(), g, lf, InstallOptions{PkgsDir: filepath.Join(scratch, "goomba-pkgs")}))
	ex := filepath.Join(run, "explicit.txt")
	must(t, os.WriteFile(ex, []byte(explicit), 0o644))
	home := filepath.Join(run, "home")
	must(t, os.MkdirAll(home, 0o755))
	cmd := exec.Command(mm, "create", "--no-rc", "-y", "-p", m, "-f", ex)
	cmd.Env = []string{"MAMBA_ROOT_PREFIX=" + filepath.Join(scratch, "mmroot"), "HOME=" + home}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("micromamba: %v\n%s", err, out)
	}
	compareTrees(t, g, m, scanTree(t, g), scanTree(t, m), false)
	lib, err := os.ReadFile(filepath.Join(g, "lib", "libomp.so"))
	must(t, err)
	sum := sha256.Sum256(lib)
	gd := symlinkDigest(t, g, "_openmp_mutex-4.5-1_llvm", "lib/libgomp.so.1")
	md := symlinkDigest(t, m, "_openmp_mutex-4.5-1_llvm", "lib/libgomp.so.1")
	t.Logf("lib/libgomp.so.1 sha256_in_prefix: goomba %s, micromamba %s", gd, md)
	if gd != hex.EncodeToString(sum[:]) {
		t.Errorf("goomba recorded %s, libomp.so is %s", gd, hex.EncodeToString(sum[:]))
	}
}
