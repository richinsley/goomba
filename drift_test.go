package goomba

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/richinsley/goomba/archive"
	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
)

// TestPlannedPathsMatchLink cross-checks the clobber check's input: for
// every package in testdata, link.PlannedPaths (what InstallLock compares
// across packages) lists exactly the paths, path types and order that
// link.Link records, and plannedDests claims directories as shareable and
// everything else as one package's.
func TestPlannedPathsMatchLink(t *testing.T) {
	plat := hostPlatform(t)
	var files []string
	for _, dir := range []string{"pkgs", "clobber"} {
		ents, err := os.ReadDir(filepath.Join("testdata", dir))
		must(t, err)
		for _, e := range ents {
			files = append(files, filepath.Join("testdata", dir, e.Name()))
		}
	}
	py, err := link.NewPythonInfo(&record.PackageRecord{Name: "python", Version: "3.11.9", Build: "h0_cpython", Subdir: plat}, plat)
	must(t, err)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			p := archivePkg(t, f)
			if p.rec.Subdir != "noarch" && p.rec.Subdir != plat {
				t.Skipf("package for %s", p.rec.Subdir)
			}
			pkgDir := filepath.Join(t.TempDir(), "pkg")
			must(t, archive.Extract(context.Background(), f, pkgDir, archive.Limits{}))
			prefix := filepath.Join(t.TempDir(), "env")
			opt := link.Options{Prefix: prefix, Platform: plat, Python: py}
			planned, err := link.PlannedPaths(context.Background(), pkgDir, p.rec, opt)
			must(t, err)
			inst, err := link.Link(context.Background(), pkgDir, p.rec, opt)
			must(t, err)
			var want []link.PlannedPath
			for _, r := range inst.Paths {
				want = append(want, link.PlannedPath{Path: r.Path, PathType: r.PathType})
			}
			if !slices.Equal(planned, want) {
				t.Errorf("PlannedPaths %v\nlink         %v", planned, want)
			}
			for i, d := range plannedDests(planned) {
				if d.path != planned[i].Path || (d.kind == claimDir) != (planned[i].PathType == link.PathDirectory) {
					t.Errorf("dest %+v for %+v", d, planned[i])
				}
			}
		})
	}
}

// TestSpecMemory (N7): what the install keeps of the locked specs grows
// with the distinct edges between packages, not with the spec strings.
// It measures the heap retained by scanSpecs and planInstall per byte of
// the lockfile's JSON.
func TestSpecMemory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pkgs     int
		deps     int // depends entries per package
		targets  int // distinct targets among them
		maxRatio float64
	}{
		// The reviewer's probe: a long depends list naming few packages.
		{"one package, many entries", 11, 20000, 10, 0.05},
		// A dense, realistic graph. The plan's graph also keeps the
		// descendents of each node as bit sets (n²/8 bytes, see package
		// dag), so this ratio grows slowly with the number of packages.
		{"2000 packages", 2000, 15, 15, 3.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf, size := syntheticLock(t, tc.pkgs, tc.deps, tc.targets)
			recs := lf.Records()
			slices.SortFunc(recs, func(a, b *record.PackageRecord) int { return strings.Compare(a.Name, b.Name) })
			runtime.GC()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			sc, err := scanSpecs(recs)
			must(t, err)
			p, err := planInstall(recs, sc)
			must(t, err)
			runtime.GC()
			runtime.GC()
			runtime.ReadMemStats(&after)
			retained := float64(int64(after.HeapAlloc) - int64(before.HeapAlloc))
			ratio := retained / float64(size)
			t.Logf("lockfile %d bytes, %d depends entries: %.0f bytes retained, %.3f per lockfile byte", size, tc.pkgs*tc.deps, retained, ratio)
			if retained < 0 {
				t.Fatal("measurement disturbed by garbage collected meanwhile")
			}
			if ratio > tc.maxRatio {
				t.Errorf("%.3f bytes retained per lockfile byte, want at most %.3f", ratio, tc.maxRatio)
			}
			runtime.KeepAlive(sc)
			runtime.KeepAlive(p)
			runtime.KeepAlive(lf)
			runtime.KeepAlive(recs)
		})
	}
}

// syntheticLock builds a valid lockfile of n packages whose depends lists
// have deps entries naming targets distinct earlier packages, and returns
// it with the size of its JSON form.
func syntheticLock(t *testing.T, n, deps, targets int) (*lock.Lockfile, int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(7, 8))
	ch := "https://example.com/ch"
	recs := make([]*record.PackageRecord, n)
	for i := range recs {
		name := fmt.Sprintf("p%05d", i)
		r := &record.PackageRecord{Name: name, Version: "1.0", Build: "h0_0", Subdir: hostPlatform(t),
			SHA256: strings.Repeat("ab", 32), URL: ch + "/" + hostPlatform(t) + "/" + name + "-1.0-h0_0.conda"}
		if i > 0 {
			for j := 0; j < deps; j++ {
				k := rng.IntN(min(i, targets))
				r.Depends = append(r.Depends, fmt.Sprintf("p%05d >=0.%d", k, j%10))
			}
		}
		recs[i] = r
	}
	lf, err := lock.FromRecords(hostPlatform(t), []string{ch}, nil, map[string]string{}, recs)
	must(t, err)
	var buf bytes.Buffer
	must(t, lf.Write(&buf))
	return lf, buf.Len()
}

// TestVirtualSpecMemory (review round 2, N1): repeated specs naming a
// virtual package cost nothing per repetition.
func TestVirtualSpecMemory(t *testing.T) {
	const n = 200000
	recs := []*record.PackageRecord{{Name: "a", Version: "1", Build: "0"}}
	for i := 0; i < n; i++ {
		recs[0].Depends = append(recs[0].Depends, "__a", "__glibc >=2.17,<3.0.a0")
	}
	for i := 0; i < 1000; i++ {
		recs = append(recs, &record.PackageRecord{Name: fmt.Sprintf("p%04d", i), Version: "1", Build: "0",
			Depends: []string{"__glibc >=2.17,<3.0.a0", "__unix"}, Constrains: []string{"__cuda >=12"}})
	}
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	sc, err := scanSpecs(recs)
	must(t, err)
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	entries := 2*n + 3000
	t.Logf("%d virtual entries: %d bytes retained (%.2f per entry), %d distinct specs, %d (package, spec) pairs",
		entries, retained, float64(retained)/float64(entries), len(sc.vspecs), len(sc.virt))
	if len(sc.vspecs) != 4 || len(sc.virt) != 2+3000 {
		t.Errorf("%d specs, %d pairs", len(sc.vspecs), len(sc.virt))
	}
	if float64(retained)/float64(entries) > 1 {
		t.Errorf("%.2f bytes retained per entry", float64(retained)/float64(entries))
	}
	runtime.KeepAlive(sc)
	runtime.KeepAlive(recs)
}
