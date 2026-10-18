package goomba

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richinsley/goomba/fetch"
	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/virtual"
)

// TestLiveCreateSweep creates a few typical environments from current
// conda-forge with Create, sharing one package cache and one index cache,
// to find packages goomba refuses that micromamba installs (WP8c: ncurses
// 6.6's terminfo symlinks were the first). Each environment must install
// and its interpreter, run with an empty environment and the prefix's bin
// on PATH, must import ssl, sqlite3, ctypes and the environment's main
// packages. When a Create fails, every package of the solve is planned
// with link.PlannedPaths to list all refusals, not only the first. It
// logs, per environment, the packages, the archive bytes first downloaded
// for it, the bytes received (indexes and TLS included) and the time;
// everything lives in the test's temporary directory.
func TestLiveCreateSweep(t *testing.T) {
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	plat := virtual.HostPlatform()
	if plat == "" || runtime.GOOS == "windows" {
		t.Skip("no host platform, or Windows")
	}
	scratch := removableTempDir(t)
	pkgs, repodata, cwd := filepath.Join(scratch, "pkgs"), filepath.Join(scratch, "repodata"), filepath.Join(scratch, "cwd")
	must(t, os.Mkdir(cwd, 0o700))

	var received atomic.Int64
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return countingConn{c, &received}, nil
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 16,
		TLSHandshakeTimeout: 30 * time.Second,
	}}

	envs := []struct {
		name    string
		specs   []string
		imports string
	}{
		{"py3.9", []string{"python=3.9"}, ""},
		{"py3.10", []string{"python=3.10"}, ""},
		{"py3.11", []string{"python=3.11"}, ""},
		{"py3.12", []string{"python=3.12"}, ""},
		{"py3.13", []string{"python=3.13"}, ""},
		{"scientific", []string{"python=3.12", "numpy", "pandas", "scipy", "matplotlib-base"}, ", numpy, pandas, scipy, matplotlib"},
		{"web-docs", []string{"python=3.12", "sphinx", "requests", "flask"}, ", sphinx, requests, flask"},
	}
	cached := map[string]bool{} // archives in the package cache, by sha256
	var summary []string
	var total int64
	for _, env := range envs {
		t.Run(env.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			prefix := filepath.Join(scratch, "envs", env.name)
			lopt := LockOptions{
				Specs:       env.specs,
				Channels:    []string{"conda-forge"},
				RepodataDir: repodata,
				Client:      client,
				MaxAge:      2 * time.Hour,
			}
			iopt := InstallOptions{PkgsDir: pkgs, Client: client, Concurrency: 8, CompilePyc: true}
			before, start := received.Load(), time.Now()
			lf, err := Create(ctx, prefix, lopt, iopt)
			elapsed := time.Since(start)
			if err != nil {
				t.Errorf("Create %q: %v", env.specs, err)
				for _, r := range plannedRefusals(ctx, t, lopt, pkgs, client, prefix) {
					t.Errorf("refused: %s", r)
				}
				summary = append(summary, fmt.Sprintf("%-10s FAILED after %v: %v", env.name, elapsed.Round(time.Second), err))
				return
			}
			var fresh int64
			for _, p := range lf.Packages {
				if !cached[p.SHA256] {
					cached[p.SHA256] = true
					fresh += p.Size
				}
			}
			total += fresh
			python := ""
			dists := make([]string, 0, len(lf.Packages))
			for _, p := range lf.Packages {
				if p.Name == "python" {
					python = p.Version + "-" + p.Build
				}
				dists = append(dists, p.Name+"-"+p.Version+"-"+p.Build)
			}
			slices.Sort(dists)
			t.Logf("%s: %s", env.name, strings.Join(dists, " "))
			cmd := exec.CommandContext(ctx, filepath.Join(prefix, "bin", "python"), "-c",
				"import ssl, sqlite3, ctypes, sys"+env.imports+"; print(sys.version.split()[0], ssl.OPENSSL_VERSION, 'sqlite', sqlite3.sqlite_version)")
			cmd.Env = []string{"PATH=" + filepath.Join(prefix, "bin")}
			cmd.Dir = cwd
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("python: %v\n%s", err, out)
			}
			if err := CheckComplete(prefix); err != nil {
				t.Errorf("CheckComplete: %v", err)
			}
			line := fmt.Sprintf("%-10s %3d packages, python %s, %6.1f MB new archives, %6.1f MB received, %v; %s",
				env.name, len(lf.Packages), python, float64(fresh)/1e6, float64(received.Load()-before)/1e6,
				elapsed.Round(100*time.Millisecond), strings.TrimSpace(string(out)))
			t.Log(line)
			summary = append(summary, line)
			// The environment is checked; only the cache is shared.
			makeWritable(prefix)
			must(t, os.RemoveAll(prefix))
		})
	}
	t.Logf("sweep, %.1f MB of archives in all, %.1f MB received:\n%s", float64(total)/1e6, float64(received.Load())/1e6, strings.Join(summary, "\n"))
}

// plannedRefusals solves lopt again from the warm index cache and plans
// every package of the result with link.PlannedPaths, as InstallLock's
// clobber check does, returning each package that fails with the reason.
func plannedRefusals(ctx context.Context, t *testing.T, lopt LockOptions, pkgs string, client *http.Client, prefix string) []string {
	t.Helper()
	lf, err := Lock(ctx, lopt)
	if err != nil {
		return []string{fmt.Sprintf("lock: %v", err)}
	}
	return planAll(ctx, lf, pkgs, client, prefix)
}

// planAll extracts every package of lf into the package cache pkgs and
// plans it for prefix, returning the failures.
func planAll(ctx context.Context, lf *lock.Lockfile, pkgs string, client *http.Client, prefix string) []string {
	cache := &fetch.Cache{Dir: pkgs, Client: client}
	recs := lf.Records()
	var py *link.PythonInfo
	for _, r := range recs {
		if r.Name == "python" {
			py, _ = link.NewPythonInfo(r, lf.Platform)
		}
	}
	var out []string
	for _, r := range recs {
		dir, err := cache.Extracted(ctx, r)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: fetch: %v", r.Dist(), err))
			continue
		}
		if _, err := link.PlannedPaths(ctx, dir, r, link.Options{Prefix: prefix, Platform: lf.Platform, Python: py}); err != nil {
			out = append(out, fmt.Sprintf("%s: %v", r.Dist(), err))
		}
	}
	return out
}

// removableTempDir returns t.TempDir(), made writable again before it is
// removed.
func removableTempDir(t *testing.T) string {
	dir := t.TempDir()
	t.Cleanup(func() { makeWritable(dir) })
	return dir
}

// makeWritable gives the owner write permission on every directory below
// dir, so that it can be removed.
func makeWritable(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

// TestLiveDifferentialCurrent installs python=3.12 and pip, locked from
// current conda-forge, with goomba and with micromamba from the same explicit list,
// and compares the trees (symlink targets included) and the paths_data
// records as TestDifferentialMicromamba does. Unlike the differentials on
// the frozen reference index, the solve includes ncurses-6.6-hdb14827_1,
// whose terminfo symlinks have targets such as ".././61/adm1178" (WP8c):
// both must write them as the package has them.
func TestLiveDifferentialCurrent(t *testing.T) {
	mm := os.Getenv("GOOMBA_MICROMAMBA")
	if os.Getenv("GOOMBA_LIVE") == "" || mm == "" {
		t.Skip("set GOOMBA_LIVE=1 and GOOMBA_MICROMAMBA")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows")
	}
	scratch := removableTempDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	lf, err := Lock(ctx, LockOptions{Specs: []string{"python=3.12", "pip"}, Channels: []string{"conda-forge"}, RepodataDir: filepath.Join(scratch, "repodata")})
	must(t, err)
	g, m := filepath.Join(scratch, "gmb"), filepath.Join(scratch, "mmb")
	must(t, InstallLock(ctx, g, lf, InstallOptions{PkgsDir: filepath.Join(scratch, "pkgs"), CompilePyc: true}))
	var buf bytes.Buffer
	must(t, lf.WriteExplicit(&buf))
	explicit := filepath.Join(scratch, "explicit.txt")
	must(t, os.WriteFile(explicit, buf.Bytes(), 0o644))
	home := filepath.Join(scratch, "home")
	must(t, os.Mkdir(home, 0o700))
	cmd := exec.CommandContext(ctx, mm, "create", "--no-rc", "-y", "-p", m, "-f", explicit)
	cmd.Env = []string{"MAMBA_ROOT_PREFIX=" + filepath.Join(scratch, "mmroot"), "HOME=" + home}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("micromamba: %v\n%s", err, out)
	}
	gt, mt := scanTree(t, g), scanTree(t, m)
	compareTrees(t, g, m, gt, mt, true)
	stale := ""
	for _, p := range lf.Packages {
		dist := p.Name + "-" + p.Version + "-" + p.Build
		stale = comparePaths(t, g, dist, readCondaMetaJSON(t, g, dist)["paths_data"], readCondaMetaJSON(t, m, dist)["paths_data"], stale)
	}
	// The case WP8c is about must have been compared.
	dotted := 0
	for _, p := range sortedKeys(gt) {
		if n := gt[p]; n.mode&fs.ModeSymlink != 0 && slices.Contains(strings.Split(n.target, "/"), ".") {
			dotted++
		}
	}
	t.Logf("%d packages; %d symlinks with \".\" components in their targets", len(lf.Packages), dotted)
	if dotted == 0 {
		t.Error("no symlink target with \".\" components was compared")
	}
}
