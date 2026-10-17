package solve

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
)

// Ported from the WP7 review's review_prop_test.go. Second-generation
// random channels: more spec shapes (brackets, build
// globs, build numbers, epochs, pre-releases, precedence, subdir, md5,
// track_features), two channels under strict priority (repodata.Multi),
// favored records the channel does not have, and constraints on absent
// packages.

var (
	rvNames    = []string{"a", "b", "c", "d", "e", "f"}
	rvVersions = []string{"1", "1.0", "1.5", "2", "2.0rc1", "2.0.post1", "2.0.1", "10", "1!0.5", "3a", "1.5.0"}
	rvBuilds   = []string{"b0", "b1", "py39_0", "py310_1", "cpu_0", "cuda_1"}
	rvSpecs    = []string{
		"", " >=2", " <2", " 1.*", " 1.5.*", " ==2", " =2", " 2", " >1,<3", " !=1.5", " 2|3",
		" >=1.5,<2|>=3", " 1.*|>=2,<3", " * b0", " * py*", " * *_1", "[version='>=1.5',build=py*]",
		"[build_number='>=2']", "[build_number=1]", " >=1 b*", "[subdir=noarch]", "[track_features=tf]",
		"[version='!=2.*']", " ~=1.5", " 1.0.*", "[md5=" + dummyMD5 + "]", "[sha256=" + dummySHA256 + "]",
		" 2.0", " >=2.0rc1", " <1!0", " >=1!0", "[version='>=2',build_number='<2']", " 2.0.*", " !=2.0.*",
		" >=2.0.post1", " * cpu*", "[build='*0']", " <=1.5", " >1.5.0", " ==1.5", " 10.*", " >=3a",
	}
	rvChannels = []string{"https://conda.anaconda.org/x", "https://conda.anaconda.org/y"}
	rvVirtual  = []string{"__v", "__w"}
)

type rvGen struct{ rng *rand.Rand }

func (g rvGen) n(k int) int { return g.rng.Intn(k) }

func (g rvGen) spec(withVirtual bool, names []string) string {
	if withVirtual && g.n(8) == 0 {
		return rvVirtual[g.n(len(rvVirtual))] + rvSpecs[g.n(len(rvSpecs))]
	}
	n := names[g.n(len(names))]
	switch g.n(10) {
	case 0, 1, 2, 5, 6:
		return n
	case 3:
		n = "x::" + n
	case 4:
		if g.n(3) == 0 {
			return n + "[channel=y]"
		}
	}
	return n + rvSpecs[g.n(len(rvSpecs))]
}

type rvCase struct {
	repos [][]*record.PackageRecord // highest priority first
	req   Request
}

func (g rvGen) genCase(maxPerName int, deep bool) rvCase {
	names := rvNames[:3+g.n(len(rvNames)-2)]
	var c rvCase
	c.repos = make([][]*record.PackageRecord, 1+g.n(2))
	seen := map[string]bool{}
	for ni, n := range names {
		for i, count := 0, 1+g.n(maxPerName); i < count; i++ {
			ch := g.n(len(c.repos))
			b := pkg(n).version(rvVersions[g.n(len(rvVersions))]).build(rvBuilds[g.n(len(rvBuilds))]).buildNumber(uint64(g.n(3)))
			b.r.Channel = rvChannels[ch]
			if g.n(5) == 0 {
				b.r.Subdir = "noarch"
			}
			if g.n(6) == 0 {
				b = b.tarBz2()
			} else {
				b.r.Fn = b.r.Dist() + ".conda"
			}
			key := b.r.Channel + "/" + b.r.Subdir + "/" + b.r.Dist()
			if seen[key] {
				continue
			}
			seen[key] = true
			depNames := names
			if deep && ni+1 < len(names) {
				depNames = names[ni+1:] // a DAG, deeper chains
			}
			maxDeps := 4
			for d := g.n(maxDeps); d > 0; d-- {
				b.r.Depends = append(b.r.Depends, g.spec(true, depNames))
			}
			for k := g.n(maxDeps - 1); k > 0; k-- {
				// constrains may name packages nothing provides
				b.r.Constrains = append(b.r.Constrains, g.spec(true, append(names, "zz")))
			}
			if g.n(6) == 0 {
				b.r.TrackFeatures = []string{"tf"}
			}
			if g.n(25) == 0 {
				b.r.MD5 = "00000000000000000000000000000000"
			}
			b.r.Timestamp = int64(g.n(4)) * 1000
			c.repos[ch] = append(c.repos[ch], b.rec())
		}
	}
	for i := g.n(3) + 1; i > 0; i-- {
		c.req.Specs = append(c.req.Specs, g.spec(false, names))
	}
	for i := g.n(3); i > 0; i-- {
		c.req.Constraints = append(c.req.Constraints, g.spec(true, append(names, "zz")))
	}
	for _, v := range rvVirtual {
		if g.n(2) == 0 {
			c.req.Virtual = append(c.req.Virtual, virtualPkg(v, rvVersions[g.n(len(rvVersions))], rvBuilds[g.n(len(rvBuilds))]))
		}
	}
	var all []*record.PackageRecord
	for _, r := range c.repos {
		all = append(all, r...)
	}
	if len(all) > 0 {
		switch g.n(6) {
		case 0:
			c.req.Locked = append(c.req.Locked, all[g.n(len(all))])
		case 1:
			l := *all[g.n(len(all))]
			l.Version = "0.1"
			l.Fn = l.Dist() + ".conda"
			l.Depends = nil
			c.req.Locked = append(c.req.Locked, &l)
		}
		switch g.n(5) {
		case 0:
			f := all[g.n(len(all))]
			if len(c.req.Locked) == 0 || c.req.Locked[0].Name != f.Name {
				c.req.Favored = append(c.req.Favored, f)
			}
		case 1:
			// a favored record the channel does not have
			f := *all[g.n(len(all))]
			f.Version = "7"
			f.Fn = f.Dist() + ".conda"
			f.Channel = "https://conda.anaconda.org/z"
			if len(c.req.Locked) == 0 || c.req.Locked[0].Name != f.Name {
				c.req.Favored = append(c.req.Favored, &f)
			}
		}
	}
	return c
}

func (c rvCase) source() repodata.Source {
	srcs := make([]repodata.Source, len(c.repos))
	for i, r := range c.repos {
		srcs[i] = repodata.NewMemory(r)
	}
	if len(srcs) == 1 {
		return srcs[0]
	}
	return repodata.NewMulti(srcs...)
}

// visible returns, per name, the records strict priority lets the solver
// see, plus favored and locked records.
func (c rvCase) visible() map[string][]*record.PackageRecord {
	by := map[string][]*record.PackageRecord{}
	for _, repo := range c.repos {
		local := map[string][]*record.PackageRecord{}
		for _, r := range repo {
			local[r.Name] = append(local[r.Name], r)
		}
		for n, rs := range local {
			if _, done := by[n]; !done {
				by[n] = rs
			}
		}
	}
	for _, r := range append(append([]*record.PackageRecord(nil), c.req.Favored...), c.req.Locked...) {
		by[r.Name] = append(by[r.Name], r)
	}
	return by
}

// bruteForceV returns a valid selection if one exists; ok=false and
// skipped=true when the search space is too large.
func bruteForceV(by map[string][]*record.PackageRecord, req Request, limit int) (sel []*record.PackageRecord, ok, skipped bool) {
	var names []string
	space := 1
	for n, rs := range by {
		names = append(names, n)
		space *= len(rs) + 1
	}
	if space > limit {
		return nil, false, true
	}
	sort.Strings(names)
	choice := make([]int, len(names))
	for {
		sel = sel[:0]
		for i, n := range names {
			if choice[i] > 0 {
				sel = append(sel, by[n][choice[i]-1])
			}
		}
		if Validate(req, sel) == nil {
			return sel, true, false
		}
		i := 0
		for ; i < len(names); i++ {
			choice[i]++
			if choice[i] <= len(by[names[i]]) {
				break
			}
			choice[i] = 0
		}
		if i == len(names) {
			return nil, false, false
		}
	}
}

func dumpCase(c rvCase) string {
	s := fmt.Sprintf("specs=%q constraints=%q virtual=%s locked=%s favored=%s\n", c.req.Specs, c.req.Constraints,
		formatRecords(c.req.Virtual), formatRecords(c.req.Locked), formatRecords(c.req.Favored))
	for i, repo := range c.repos {
		s += fmt.Sprintf(" repo %d:\n", i)
		for _, r := range repo {
			s += fmt.Sprintf("  %s ch=%s sd=%s fn=%s bn=%d depends=%q constrains=%q tf=%v md5=%s\n", displayRecord(r), r.Channel, r.Subdir, r.Fn, r.BuildNumber, r.Depends, r.Constrains, r.TrackFeatures, r.MD5[:4])
		}
	}
	return s
}

func TestPropertyStrictPriority(t *testing.T) {
	iters := 2000
	if testing.Short() {
		iters = 300
	}
	rng := rand.New(rand.NewSource(99))
	g := rvGen{rng}
	var solved, unsat, skipped, invalidReq int
	for i := 0; i < iters; i++ {
		c := g.genCase(2+i%5, i%3 == 0)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		sol, err := Solve(ctx, c.source(), c.req)
		cancel()
		var u *Unsolvable
		switch {
		case err == nil:
			solved++
			if verr := Validate(c.req, sol.Records); verr != nil {
				t.Fatalf("case %d invalid solution %s: %v\n%s", i, formatRecords(sol.Records), verr, dumpCase(c))
			}
			// The solution must only use visible records.
			vis := c.visible()
			for _, r := range sol.Records {
				found := false
				for _, v := range vis[r.Name] {
					if v == r {
						found = true
					}
				}
				if !found {
					t.Fatalf("case %d: solution uses %s (channel %s) which strict priority hides\n%s", i, displayRecord(r), r.Channel, dumpCase(c))
				}
			}
		case errors.As(err, &u):
			unsat++
			sel, ok, skip := bruteForceV(c.visible(), c.req, 200000)
			if skip {
				skipped++
			} else if ok {
				t.Fatalf("case %d: unsolvable but %s is valid\n%s\nexplanation:\n%s", i, formatRecords(sel), dumpCase(c), u.Explanation)
			}
		case errors.Is(err, ErrInvalidRequest):
			invalidReq++
		default:
			if errors.Is(err, cdcl.ErrInternal) {
				t.Fatalf("case %d internal: %v\n%s", i, err, dumpCase(c))
			}
			t.Fatalf("case %d unexpected error: %v\n%s", i, err, dumpCase(c))
		}
	}
	t.Logf("solved %d unsat %d (brute-force skipped %d) invalid request %d", solved, unsat, skipped, invalidReq)
}

// Determinism under source order and across repeated runs, including the
// explanation text.
func TestPropertyStrictPriorityDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	g := rvGen{rng}
	n := 1500
	if testing.Short() {
		n = 200
	}
	for i := 0; i < n; i++ {
		c := g.genCase(1+i%5, i%2 == 0)
		want := outcome(Solve(context.Background(), c.source(), c.req))
		d := c
		d.repos = make([][]*record.PackageRecord, len(c.repos))
		for j, r := range c.repos {
			s := append([]*record.PackageRecord(nil), r...)
			rng.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
			d.repos[j] = s
		}
		if got := outcome(Solve(context.Background(), d.source(), d.req)); got != want {
			t.Fatalf("case %d: order changed the outcome:\n%s\nwant:\n%s\n%s", i, got, want, dumpCase(c))
		}
	}
}
