package goomba

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/virtual"
)

// TestDifferentialCreate creates environments from match specs twice, with
// goomba (Create) and with `micromamba create -p PREFIX -c URL SPEC...`,
// against the same frozen conda-forge index: the linux-64 and noarch
// repodata.json cached under $GOOMBA_REF_ROOT/pkgs/cache (as package
// solve's differential uses), copied into the module's git-ignored tmp
// directory and served over TLS from a loopback server, which serves the
// package archives too (from $GOOMBA_REF_ROOT/pkgs when there, downloaded
// once from conda.anaconda.org into tmp otherwise, which needs
// GOOMBA_LIVE=1). Both get the same virtual packages (micromamba through
// CONDA_OVERRIDE_*), micromamba does not add pip to python
// (MAMBA_ADD_PIP_AS_PYTHON_DEPENDENCY=false: goomba never does) and reads
// no configuration (--no-rc).
//
// Both must install the same packages and the same files. Allowed
// differences, all documented in the package doc: files and directories
// never have group or other write in goomba; sha256_in_prefix of files
// whose prefix was rewritten (and of symlinks to them) is the real digest
// in goomba where micromamba 2.2.0 writes one stale constant; the history
// lists packages in goomba's link order; depends and constrains are
// recorded as the index lists them, where micromamba records constrains as
// libsolv renders them ("libgcc-ng ==15.2.0=*_18" becomes "libgcc-ng
// ==15.2.0 *_18", "blas 2.306   openblas" becomes "blas 2.306 openblas")
// and, once it has written its libsolv cache of the index (from the second
// case on), lists depends and constrains in that cache's order. Every
// other conda-meta field must be equal.
func TestDifferentialCreate(t *testing.T) {
	mm, refRoot := os.Getenv("GOOMBA_MICROMAMBA"), os.Getenv("GOOMBA_REF_ROOT")
	if mm == "" || refRoot == "" {
		t.Skip("set GOOMBA_MICROMAMBA and GOOMBA_REF_ROOT")
	}
	if hostPlatform(t) != "linux-64" {
		t.Skip("the frozen index is linux-64")
	}
	scratch, err := filepath.Abs(filepath.Join("tmp", "lock-differential"))
	must(t, err)
	srv := startFrozenChannel(t, scratch, refRoot)
	// The server's port, and so both index caches' keys, change on every
	// run: start both empty and drop them afterwards (each holds a copy of
	// the 570 MB index). The package caches are kept between runs.
	mmCache := filepath.Join(scratch, "mmroot", "pkgs", "cache")
	repodataDir := filepath.Join(scratch, "goomba-repodata")
	for _, d := range []string{mmCache, repodataDir} {
		must(t, os.RemoveAll(d))
		t.Cleanup(func() { os.RemoveAll(d) })
	}

	// One machine for both: this host's virtual packages, without a GPU.
	virt, err := virtual.Detect(context.Background(), virtual.Options{Overrides: map[string]string{"cuda": ""}})
	must(t, err)
	env := []string{"CONDA_OVERRIDE_CUDA="}
	for k, v := range virtual.ToOverrides(virt) {
		if k != "__unix" {
			env = append(env, "CONDA_OVERRIDE_"+strings.ToUpper(strings.TrimPrefix(k, "__"))+"="+v)
		}
	}
	sort.Strings(env)
	t.Logf("machine: %v", env)

	for _, tc := range []struct {
		name  string
		specs []string
		// entryPoints: the environment has noarch: python entry points.
		entryPoints bool
	}{
		{"python 3.11", []string{"python=3.11"}, false},
		{"numpy", []string{"numpy", "python=3.11"}, false},
		{"pandas", []string{"pandas", "python=3.11"}, false},
		{"noarch stack", []string{"sphinx", "requests", "flask", "python=3.12"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, err := os.MkdirTemp(scratch, "run-")
			must(t, err)
			defer os.RemoveAll(run)
			gPrefix, mPrefix := filepath.Join(run, "gmb"), filepath.Join(run, "mmb")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			start := time.Now()
			lf, err := Create(ctx, gPrefix, LockOptions{
				Specs:       tc.specs,
				Channels:    []string{srv.channel},
				Virtual:     virt,
				RepodataDir: repodataDir,
				Client:      srv.srv.Client(),
			}, InstallOptions{
				PkgsDir:    filepath.Join(scratch, "goomba-pkgs"),
				Client:     srv.srv.Client(),
				CompilePyc: true,
			})
			if err != nil {
				if srv.missing() != "" {
					t.Skipf("%v (%s)", err, srv.missing())
				}
				t.Fatal(err)
			}
			t.Logf("goomba: %d packages in %v", len(lf.Packages), time.Since(start))

			home := filepath.Join(run, "home")
			must(t, os.MkdirAll(home, 0o755))
			args := append([]string{"create", "--no-rc", "-y", "-p", mPrefix, "--override-channels", "-c", srv.channel,
				"--ssl-verify", srv.certFile}, tc.specs...)
			cmd := exec.CommandContext(ctx, mm, args...)
			cmd.Env = append([]string{
				"PATH=/usr/bin:/bin",
				"HOME=" + home,
				"MAMBA_ROOT_PREFIX=" + filepath.Join(scratch, "mmroot"),
				"MAMBA_ADD_PIP_AS_PYTHON_DEPENDENCY=false",
				"MAMBA_USE_SHARDED_REPODATA=false",
			}, env...)
			start = time.Now()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("micromamba: %v\n%s", err, out)
			}
			t.Logf("micromamba: %v", time.Since(start))

			gPkgs, mPkgs := condaMetaRecords(t, gPrefix), condaMetaRecords(t, mPrefix)
			if !slices.Equal(gPkgs, mPkgs) {
				t.Fatalf("package sets differ:\ngoomba     %q\nmicromamba %q", gPkgs, mPkgs)
			}
			t.Logf("same %d packages", len(gPkgs))
			g, m := scanTree(t, gPrefix), scanTree(t, mPrefix)
			compareTrees(t, gPrefix, mPrefix, g, m, tc.entryPoints)
			compareSolvedCondaMeta(t, gPrefix, mPrefix, filepath.Join(scratch, "goomba-pkgs"), lf)
		})
	}
}

// condaMetaRecords lists the package records in prefix/conda-meta.
func condaMetaRecords(t *testing.T, prefix string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(prefix, "conda-meta"))
	must(t, err)
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

// compareSolvedCondaMeta compares the conda-meta records of two solved
// installations field by field (paths_data through comparePaths, which
// allows micromamba's stale digests) and their histories.
func compareSolvedCondaMeta(t *testing.T, gPrefix, mPrefix, gPkgs string, lf *lock.Lockfile) {
	t.Helper()
	cacheKeys := []string{"extracted_package_dir", "link", "package_tarball_full_path"}
	stale := ""
	fields, rendered := 0, 0
	for _, p := range lf.Packages {
		dist := p.Name + "-" + p.Version + "-" + p.Build
		gr := readCondaMetaJSON(t, gPrefix, dist)
		mr := readCondaMetaJSON(t, mPrefix, dist)
		for _, k := range sortedKeys(mergeKeys(gr, mr)) {
			fields++
			switch {
			case k == "paths_data":
				stale = comparePaths(t, gPrefix, dist, gr[k], mr[k], stale)
			case slices.Contains(cacheKeys, k):
				if !strings.Contains(fmt.Sprint(gr[k]), gPkgs) {
					t.Errorf("%s: %s = %v is not in goomba's cache", dist, k, gr[k])
				}
			case k == "depends" || k == "constrains":
				a, b := jsonText(gr[k]), jsonText(mr[k])
				ga, mb := gr[k], mr[k]
				if k == "constrains" {
					ga = libsolvConstrains(ga)
				}
				if a != b {
					rendered++
				}
				if jsonText(sortedList(ga)) != jsonText(sortedList(mb)) {
					t.Errorf("%s: %s = %s, micromamba %s", dist, k, a, b)
				}
			default:
				if a, b := jsonText(gr[k]), jsonText(mr[k]); a != b {
					t.Errorf("%s: %s = %s, micromamba %s", dist, k, a, b)
				}
			}
		}
	}
	t.Logf("compared %d conda-meta fields; %d depends or constrains lists differ only in libsolv's rendering or order", fields, rendered)
	history := func(prefix string) (added []string, specs string) {
		h, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "history"))
		must(t, err)
		for _, l := range strings.Split(string(h), "\n") {
			switch {
			case strings.HasPrefix(l, "+"):
				added = append(added, l)
			case strings.HasPrefix(l, "# update specs:"):
				specs = l
			}
		}
		slices.Sort(added)
		return added, specs
	}
	ga, gs := history(gPrefix)
	ma, ms := history(mPrefix)
	if !slices.Equal(ga, ma) {
		t.Errorf("history packages differ:\n%q\nvs\n%q", ga, ma)
	}
	if gs != ms {
		t.Errorf("history specs differ:\n%s\nvs\n%s", gs, ms)
	}
}

// libsolvConstrains renders a decoded constrains list as micromamba 2.2.0
// records it after a solve, through libsolv's conda match spec parser:
// "name OPVERSION=BUILD" becomes "name OPVERSION BUILD", and runs of white
// space become one space.
func libsolvConstrains(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, len(list))
	for i, e := range list {
		out[i] = e
		s, ok := e.(string)
		if !ok {
			continue
		}
		f := strings.Fields(s)
		if len(f) == 2 {
			op := len(f[1]) - len(strings.TrimLeft(f[1], "=<>!~"))
			if j := strings.IndexByte(f[1][op:], '='); j > 0 {
				f = []string{f[0], f[1][:op+j], f[1][op+j+1:]}
			}
		}
		out[i] = strings.Join(f, " ")
	}
	return out
}

// sortedList returns a decoded JSON list's elements, rendered and sorted.
func sortedList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = jsonText(e)
	}
	sort.Strings(out)
	return out
}

// jsonText renders a decoded JSON value canonically.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// frozenChannel serves the frozen conda-forge index and package archives
// over TLS.
type frozenChannel struct {
	srv      *httptest.Server
	channel  string // <server>/conda-forge
	certFile string // the server's certificate, PEM, for micromamba

	mu      sync.Mutex
	fetchMu sync.Mutex // one download at a time
	miss    string     // an archive that could not be had
}

func (f *frozenChannel) missing() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.miss
}

// startFrozenChannel copies the frozen index into scratch and serves it.
func startFrozenChannel(t *testing.T, scratch, refRoot string) *frozenChannel {
	t.Helper()
	srvDir := filepath.Join(scratch, "srv")
	for _, sd := range []string{"linux-64", "noarch"} {
		copyFileIfChanged(t, findCachedRepodata(t, refRoot, sd), filepath.Join(srvDir, sd, "repodata.json"))
	}
	store := filepath.Join(scratch, "pkgstore")
	live := os.Getenv("GOOMBA_LIVE") != ""
	f := &frozenChannel{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/conda-forge/")
		subdir, fn, ok2 := strings.Cut(rest, "/")
		if !ok || !ok2 || (subdir != "linux-64" && subdir != "noarch") || strings.ContainsAny(fn, `/\`) || fn == "" || fn[0] == '.' {
			http.NotFound(w, r)
			return
		}
		if fn == "repodata.json" {
			http.ServeFile(w, r, filepath.Join(srvDir, subdir, fn))
			return
		}
		if !strings.HasSuffix(fn, ".conda") && !strings.HasSuffix(fn, ".tar.bz2") {
			http.NotFound(w, r) // shard index, .zst and .bz2 variants
			return
		}
		p, err := f.archive(store, refRoot, subdir, fn, live)
		if err != nil {
			f.mu.Lock()
			if f.miss == "" {
				f.miss = err.Error()
			}
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, p)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	f.srv.Listener.Close()
	f.srv.Listener = ln
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	f.channel = f.srv.URL + "/conda-forge"
	f.certFile = filepath.Join(scratch, "server.pem")
	must(t, os.WriteFile(f.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o644))
	return f
}

// archive returns the path of a package archive in the store, copying it
// from the reference package cache or downloading it first.
func (f *frozenChannel) archive(store, refRoot, subdir, fn string, live bool) (string, error) {
	f.fetchMu.Lock()
	defer f.fetchMu.Unlock()
	p := filepath.Join(store, subdir, fn)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if src := filepath.Join(refRoot, "pkgs", fn); fileExists(src) {
		return p, copyFile(src, p)
	}
	if !live {
		return "", fmt.Errorf("%s is not in $GOOMBA_REF_ROOT/pkgs; set GOOMBA_LIVE=1 to download it", fn)
	}
	resp, err := http.Get("https://conda.anaconda.org/conda-forge/" + path.Join(subdir, fn))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", fn, resp.Status)
	}
	tmp := p + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// copyFileIfChanged copies src to dst unless dst already has src's size.
func copyFileIfChanged(t *testing.T, src, dst string) {
	t.Helper()
	si, err := os.Stat(src)
	must(t, err)
	if di, err := os.Stat(dst); err == nil && di.Size() == si.Size() {
		return
	}
	must(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	must(t, copyFile(src, dst))
}

// findCachedRepodata finds micromamba's cache entry of a conda-forge subdir
// under $GOOMBA_REF_ROOT/pkgs/cache.
func findCachedRepodata(t *testing.T, refRoot, subdir string) string {
	t.Helper()
	states, err := filepath.Glob(filepath.Join(refRoot, "pkgs", "cache", "*.state.json"))
	must(t, err)
	want := "https://conda.anaconda.org/conda-forge/" + subdir + "/repodata.json"
	for _, s := range states {
		data, err := os.ReadFile(s)
		must(t, err)
		var st struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(data, &st) == nil && st.URL == want {
			return strings.TrimSuffix(s, ".state.json") + ".json"
		}
	}
	t.Skipf("no cached repodata for %s under $GOOMBA_REF_ROOT/pkgs/cache", want)
	return ""
}
