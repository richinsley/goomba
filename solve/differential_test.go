package solve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/virtual"
)

// The differential test solves a corpus of requests with goomba and with
// micromamba ($GOOMBA_MICROMAMBA) against the same frozen conda-forge
// linux-64 + noarch index: the repodata cached under
// $GOOMBA_REF_ROOT/pkgs/cache, copied into a scratch directory of the
// working tree and served from a loopback HTTP server.
//
// Both solvers are told about the same virtual packages (micromamba
// through CONDA_OVERRIDE_*), micromamba does not add pip to python
// (MAMBA_ADD_PIP_AS_PYTHON_DEPENDENCY=false, conda's
// add_pip_as_python_dependency) and reads no configuration (--no-rc).
//
// Every difference between the two solutions must be listed in the case's
// want table, with the reason; both solutions must pass Validate.

// diffCase is one request of the differential corpus.
type diffCase struct {
	name  string
	specs []string
	// cuda, if set, adds __cuda with this version.
	cuda string
	// unsolvable means both solvers must find no solution.
	unsolvable bool
	// diffs lists the expected differences, one line per package:
	// "name: goomba <version> <build> | micromamba <version> <build>",
	// with "-" for a package one solver does not install.
	diffs []string
	// why explains the differences.
	why string
}

// diffMachine is the target machine of every case.
var diffMachine = map[string]string{
	"__glibc":    "2.39",
	"__linux":    "6.8",
	"__archspec": "x86_64_v3",
}

func diffCorpus() []diffCase {
	return []diffCase{
		{name: "python 3.9", specs: []string{"python=3.9"}},
		{name: "python 3.10", specs: []string{"python=3.10"}},
		{name: "python 3.11", specs: []string{"python=3.11"}},
		{name: "python 3.12", specs: []string{"python=3.12"}},
		{name: "python 3.13", specs: []string{"python=3.13"}},
		{name: "numpy", specs: []string{"numpy", "python=3.11"}},
		{name: "scipy", specs: []string{"scipy", "python=3.11"}},
		{name: "pandas", specs: []string{"pandas", "python=3.11"}},
		{name: "pytorch cpu", specs: []string{"pytorch=*=cpu*"}},
		{name: "jupyterlab", specs: []string{"jupyterlab"}},
		{name: "cupy cuda 12.4", specs: []string{"cupy"}, cuda: "12.4"},
		{name: "free-threaded python", specs: []string{"python=3.13=*_cp313t"}},
		{name: "noarch stack", specs: []string{"sphinx", "requests", "flask", "python=3.12"}},
		{name: "unsolvable", specs: []string{"python=3.12", "numpy=1.21"}, unsolvable: true},
		// Beyond the brief's corpus: larger and older stacks.
		{name: "tensorflow", specs: []string{"tensorflow"}},
		{name: "pytorch cuda 12.4", specs: []string{"pytorch=*=cuda*"}, cuda: "12.4"},
		{name: "ffmpeg", specs: []string{"ffmpeg"}},
		{name: "r-base", specs: []string{"r-base"}},
		{name: "gdal", specs: []string{"gdal", "python=3.12"}},
		{name: "matplotlib old python", specs: []string{"matplotlib", "python=3.8"}},
		{name: "dask xarray", specs: []string{"dask", "xarray", "netcdf4"}},
		{name: "opencv", specs: []string{"opencv"}},
		{name: "nodejs", specs: []string{"nodejs", "pnpm"}},
		{name: "compilers", specs: []string{"gcc", "gxx", "gfortran", "cmake", "make"}},
		{name: "pinned old numpy", specs: []string{"numpy=1.24", "pandas<2"}},
		{name: "openssl 1.1", specs: []string{"openssl=1.1", "python"}},
		// The WP7 review's corpus (review_diff_test.go).
		{name: "py310 numpy<2", specs: []string{"python=3.10", "numpy<2"}},
		{name: "bracket numpy", specs: []string{"numpy[version='>=1.20,<1.25',build=py311*]"}},
		{name: "mkl blas", specs: []string{"libblas=*=*mkl", "numpy", "python=3.11"}},
		{name: "openblas", specs: []string{"libblas=*=*openblas", "scipy", "python=3.12"}},
		{name: "setuptools pin", specs: []string{"pip", "setuptools<70", "python=3.12"}},
		{name: "openssl range", specs: []string{"openssl>=3,<3.2", "curl"}},
		{name: "pyarrow pandas", specs: []string{"pyarrow", "pandas", "python=3.11"}},
		{name: "numba", specs: []string{"numba", "python=3.11"}},
		{name: "two-token python", specs: []string{"python 3.11.*"}},
		{name: "disjunction python", specs: []string{"python >=3.8,<3.9|>=3.12,<3.13", "numpy"}},
		{name: "numpy not", specs: []string{"numpy !=1.26.4", "python=3.11"}},
		{name: "gcc 12", specs: []string{"gcc=12", "gxx=12"}},
		{name: "hdf5 mpi", specs: []string{"hdf5=*=mpi_openmpi*"}},
		{name: "cuda-version 11.8", specs: []string{"cuda-version=11.8", "cupy"}, cuda: "11.8"},
		{name: "scipy old", specs: []string{"python=3.12", "numpy=1.26", "scipy<1.12"}},
		{name: "psycopg2", specs: []string{"postgresql", "psycopg2"}},
		{name: "pyqt", specs: []string{"pyqt", "python=3.11"}},
		{name: "rust maturin", specs: []string{"rust", "maturin"}},
		{name: "build number", specs: []string{"python[version='3.11.*',build_number='<2']"}},
		{name: "pytorch numpy<2", specs: []string{"pytorch>=2.1", "numpy<2"}},
		{name: "sklearn", specs: []string{"scikit-learn", "python=3.10"}},
		{name: "unsat old tf", specs: []string{"python=3.13", "tensorflow<2.10"}, unsolvable: true},
		{name: "python 3.8 pandas", specs: []string{"pandas", "python=3.8"}},
		{name: "jax", specs: []string{"jax", "jaxlib"}},
		{name: "libxml2 old", specs: []string{"libxml2<2.12", "lxml"}},
	}
}

// moduleRoot returns the root of the goomba module (the parent of the
// package directory).
func moduleRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

// cachedRepodata finds the micromamba cache entry of a conda-forge subdir
// under $GOOMBA_REF_ROOT/pkgs/cache.
func cachedRepodata(t testing.TB, refRoot, subdir string) string {
	t.Helper()
	states, err := filepath.Glob(filepath.Join(refRoot, "pkgs", "cache", "*.state.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://conda.anaconda.org/conda-forge/" + subdir + "/repodata.json"
	for _, s := range states {
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
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

// copyIfChanged copies src to dst unless dst already has src's size.
func copyIfChanged(t testing.TB, src, dst string) {
	t.Helper()
	si, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if di, err := os.Stat(dst); err == nil && di.Size() == si.Size() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

// frozenCondaForge serves the frozen index and loads it into a FullIndex.
type frozenCondaForge struct {
	scratch string
	channel string
	index   *repodata.FullIndex
}

func startFrozenCondaForge(t testing.TB) *frozenCondaForge {
	t.Helper()
	refRoot := os.Getenv("GOOMBA_REF_ROOT")
	if refRoot == "" {
		t.Skip("set GOOMBA_REF_ROOT")
	}
	scratch := filepath.Join(moduleRoot(t), "tmp", "solve-differential")
	for _, sd := range []string{"linux-64", "noarch"} {
		copyIfChanged(t, cachedRepodata(t, refRoot, sd), filepath.Join(scratch, "srv", "conda-forge", sd, "repodata.json"))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.FileServer(http.Dir(filepath.Join(scratch, "srv"))))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f := &frozenCondaForge{scratch: scratch, channel: srv.URL + "/conda-forge"}

	cacheDir := filepath.Join(scratch, "goomba-cache")
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(cacheDir) })
	start := time.Now()
	f.index, err = repodata.NewFullIndex(context.Background(), f.channel, repodata.Options{CacheDir: cacheDir, Platform: "linux-64"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("loaded the frozen conda-forge index in %v", time.Since(start))
	return f
}

// machine returns the virtual packages of a case, and the CONDA_OVERRIDE_*
// variables that give micromamba the same ones.
func (c diffCase) machine(t *testing.T) ([]*record.PackageRecord, []string) {
	t.Helper()
	ov := map[string]string{}
	for k, v := range diffMachine {
		ov[k] = v
	}
	if c.cuda != "" {
		ov["__cuda"] = c.cuda
	}
	recs, err := virtual.ApplyOverrides("linux-64", virtual.Defaults("linux-64"), ov)
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for k, v := range ov {
		env = append(env, "CONDA_OVERRIDE_"+strings.ToUpper(strings.TrimPrefix(k, "__"))+"="+v)
	}
	if c.cuda == "" {
		// An empty override stops micromamba from detecting the host's
		// GPU driver.
		env = append(env, "CONDA_OVERRIDE_CUDA=")
	}
	sort.Strings(env)
	return recs, env
}

// mmPackage is a package of micromamba's --json dry-run output.
type mmPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Build   string `json:"build_string"`
	Fn      string `json:"fn"`
	Channel string `json:"channel"`
}

// runMicromamba solves specs with micromamba. It returns the packages to
// link, or ok=false when micromamba finds no solution.
func runMicromamba(t *testing.T, f *frozenCondaForge, specs, overrides []string) ([]mmPackage, bool, string) {
	t.Helper()
	bin := os.Getenv("GOOMBA_MICROMAMBA")
	root := filepath.Join(f.scratch, "mamba-root")
	home := filepath.Join(f.scratch, "home")
	for _, d := range []string{root, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args := append([]string{"create", "-n", "goomba-differential", "--dry-run", "--json", "--yes", "--no-rc",
		"--override-channels", "-c", f.channel, "--platform", "linux-64"}, specs...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append([]string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"MAMBA_ROOT_PREFIX=" + root,
		"MAMBA_ADD_PIP_AS_PYTHON_DEPENDENCY=false",
		"MAMBA_USE_SHARDED_REPODATA=false",
	}, overrides...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var out struct {
		Success bool `json:"success"`
		Actions struct {
			Link []mmPackage `json:"LINK"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("micromamba %q: %v\nstdout:\n%s\nstderr:\n%s", specs, runErr, stdout.String(), stderr.String())
	}
	if runErr != nil || !out.Success {
		return nil, false, stdout.String() + stderr.String()
	}
	return out.Actions.Link, true, ""
}

// TestDifferentialMicromamba compares goomba's solutions with micromamba's.
func TestDifferentialMicromamba(t *testing.T) {
	if os.Getenv("GOOMBA_MICROMAMBA") == "" {
		t.Skip("set GOOMBA_MICROMAMBA")
	}
	f := startFrozenCondaForge(t)
	// The server's port, and so micromamba's cache key, changes on every
	// run: drop the caches of earlier runs.
	mmCache := filepath.Join(f.scratch, "mamba-root", "pkgs", "cache")
	if err := os.RemoveAll(mmCache); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(mmCache) })
	for _, c := range diffCorpus() {
		t.Run(c.name, func(t *testing.T) {
			virt, overrides := c.machine(t)
			req := Request{Specs: c.specs, Virtual: virt}
			start := time.Now()
			sol, gerr := Solve(context.Background(), f.index, req)
			goombaTime := time.Since(start)
			start = time.Now()
			mm, mmOK, mmOut := runMicromamba(t, f, c.specs, overrides)
			t.Logf("goomba %v, micromamba %v", goombaTime, time.Since(start))

			if c.unsolvable {
				var u *Unsolvable
				if !errors.As(gerr, &u) {
					t.Errorf("goomba: expected no solution, got %v", gerr)
				} else {
					t.Logf("goomba explanation:\n%s", u.Explanation)
				}
				if mmOK {
					t.Errorf("micromamba found a solution")
				}
				return
			}
			if gerr != nil {
				t.Fatalf("goomba: %v", gerr)
			}
			if !mmOK {
				t.Fatalf("micromamba found no solution:\n%s", mmOut)
			}

			// micromamba's packages as records of the same index.
			var mmRecs []*record.PackageRecord
			for _, p := range mm {
				recs, err := f.index.Records(context.Background(), p.Name)
				if err != nil {
					t.Fatal(err)
				}
				var found *record.PackageRecord
				for _, r := range recs {
					if r.Fn == p.Fn {
						found = r
					}
				}
				if found == nil {
					t.Fatalf("micromamba chose %s, which goomba's index does not have", p.Fn)
				}
				mmRecs = append(mmRecs, found)
			}
			if err := Validate(req, mmRecs); err != nil {
				t.Errorf("micromamba's solution fails Validate: %v", err)
			}

			t.Logf("goomba %d packages, micromamba %d", len(sol.Records), len(mmRecs))
			got := solutionDiff(sol.Records, mmRecs)
			if strings.Join(got, "\n") != strings.Join(c.diffs, "\n") {
				t.Errorf("differences from micromamba:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.diffs, "\n"))
			}
		})
	}
}

// solutionDiff lists, by package name, where two solutions differ.
func solutionDiff(goomba, mm []*record.PackageRecord) []string {
	byName := func(recs []*record.PackageRecord) map[string]*record.PackageRecord {
		m := map[string]*record.PackageRecord{}
		for _, r := range recs {
			m[r.Name] = r
		}
		return m
	}
	g, m := byName(goomba), byName(mm)
	names := map[string]bool{}
	for n := range g {
		names[n] = true
	}
	for n := range m {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	show := func(r *record.PackageRecord) string {
		if r == nil {
			return "-"
		}
		return r.Version + " " + r.Build
	}
	var out []string
	for _, n := range sorted {
		gr, mr := g[n], m[n]
		if gr != nil && mr != nil && gr.Fn == mr.Fn {
			continue
		}
		out = append(out, fmt.Sprintf("%s: goomba %s | micromamba %s", n, show(gr), show(mr)))
	}
	return out
}
