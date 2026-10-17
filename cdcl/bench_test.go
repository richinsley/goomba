package cdcl_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

// syntheticProblem builds a layered ecosystem of packages*versions
// solvables. Each version depends on a few packages of the next layers
// with version ranges, and some versions constrain others, so the solver
// has to backtrack.
func syntheticProblem(packages, versions int, seed uint64) (*bundlebox.Provider, []string) {
	r := rand.New(rand.NewPCG(seed, seed))
	p := bundlebox.New()
	for i := 0; i < packages; i++ {
		for v := 0; v < versions; v++ {
			var ds, cs []string
			for d := r.IntN(4); d > 0 && i+1 < packages; d-- {
				j := i + 1 + r.IntN(min(25, packages-i-1))
				lo := r.IntN(versions)
				ds = append(ds, fmt.Sprintf("p%d %d..%d", j, lo, lo+1+r.IntN(versions)))
			}
			if r.IntN(8) == 0 && i+1 < packages {
				j := i + 1 + r.IntN(min(25, packages-i-1))
				cs = append(cs, fmt.Sprintf("p%d %d..%d", j, r.IntN(versions/2), versions))
			}
			p.AddPackage(fmt.Sprintf("p%d", i), bundlebox.NewPack(uint32(v)), ds, cs)
		}
	}
	var roots []string
	for i := 0; i < 20; i++ {
		roots = append(roots, fmt.Sprintf("p%d", r.IntN(packages/4)))
	}
	return p, roots
}

// BenchmarkSolveSynthetic solves a problem with 5,000 solvables (500
// packages of 10 versions). Provider setup is excluded from the timing.
func BenchmarkSolveSynthetic(b *testing.B) {
	type job struct {
		solver *cdcl.Solver
		reqs   []cdcl.ConditionalRequirement
	}
	jobs := make([]job, b.N)
	for i := range jobs {
		p, roots := syntheticProblem(500, 10, 42)
		jobs[i] = job{cdcl.NewSolver(p), p.Requirements(roots)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	var solutionSize, clauses int
	for i := 0; i < b.N; i++ {
		solver := jobs[i].solver
		sol, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: jobs[i].reqs})
		if err != nil {
			b.Fatalf("solve: %v", err)
		}
		solutionSize, clauses = len(sol), solver.ClauseCount()
	}
	b.ReportMetric(float64(solutionSize), "solvables-selected")
	b.ReportMetric(float64(clauses), "clauses")
}

func TestSyntheticProblemSolves(t *testing.T) {
	p, roots := syntheticProblem(500, 10, 42)
	if n := p.Pool.SolvableCount(); n != 0 {
		t.Fatalf("solvables are interned lazily, got %d before solving", n)
	}
	reqs := p.Requirements(roots)
	solver, sol := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if p.Pool.SolvableCount() > 5000 {
		t.Fatalf("got %d solvables, want at most 5000", p.Pool.SolvableCount())
	}
	t.Logf("%d solvables interned, %d selected, %d clauses", p.Pool.SolvableCount(), len(sol), solver.ClauseCount())
}

// BenchmarkUnsatManyCandidates explains a conflict involving one clause
// per candidate: n versions of a package, each depending on a package
// that does not exist, with dependencies hinted as cheap (as package solve
// does), so every candidate is ruled out up front and the conflict
// collects n clauses. The time grows about linearly with n.
func BenchmarkUnsatManyCandidates(b *testing.B) {
	for _, n := range []int{25000, 100000, 400000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				p := bundlebox.New()
				p.HintDependenciesAvailable = true
				for v := 0; v < n; v++ {
					p.AddPackage("a", bundlebox.NewPack(uint32(v)), []string{"missing"}, nil)
				}
				reqs := p.Requirements([]string{"a"})
				solver := cdcl.NewSolver(p)
				b.StartTimer()
				if _, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs}); err == nil {
					b.Fatal("solved")
				}
			}
		})
	}
}
