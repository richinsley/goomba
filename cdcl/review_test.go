package cdcl_test

// Regression tests for the issues found in review.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

// A conditional requirement must only apply while its condition holds.
// resolvo encodes "b if c 2" through the versions of c that do not match
// (here c 1), so once the condition has held the clause keeps demanding b or
// c 1 after backtracking: with c 1 excluded it reports these solvable
// problems unsat (or panics), without the exclusion it installs c 1, which
// nothing requires. {a=1, x=1} satisfies every requirement.
func TestConditionDoesNotApplyAfterBacktracking(t *testing.T) {
	build := func(exclude bool, xVersions ...uint32) *bundlebox.Provider {
		var ps []pk
		ps = append(ps, pk{"a", 1, deps("b; if c 2")})
		for _, v := range xVersions {
			if v == 2 {
				ps = append(ps, pk{"x", 2, deps("c 2")})
			} else {
				ps = append(ps, pk{"x", v, nil})
			}
		}
		ps = append(ps, pk{"c", 1, nil}, pk{"c", 2, nil})
		p := fromPackages(ps)
		if exclude {
			p.Exclude("c", 1, "it is retracted")
		}
		return p
	}
	cases := []struct {
		name  string
		p     *bundlebox.Provider
		roots []string
	}{
		{"excluded complement", build(true, 2, 1), []string{"a", "x"}},
		{"excluded complement, x first", build(true, 2, 1), []string{"x", "a"}},
		{"only x 1", build(true, 1), []string{"a", "x"}},
		{"no exclusion", build(false, 2, 1), []string{"a", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertInline(t, solveSnapshot(t, c.p, c.roots), `
				a=1
				x=1
			`)
		})
	}
}

// A condition on a package that is not installed does not hold, also when a
// constraint rules out the versions that do not match it (the review's
// repros, minus their soft requirements; resolvo agrees here because it
// defers the requirement, and the exact encoding must not change that).
func TestConditionOnUninstalledPackage(t *testing.T) {
	build := func() *bundlebox.Provider {
		return fromPackages([]pk{{"a", 1, deps("b; if c 2")}, {"c", 1, nil}, {"c", 2, nil}})
	}
	assertInline(t, solveSnapshot(t, build(), []string{"a"}), `
		a=1
	`)

	p := build()
	_, solved := solveOK(t, p, cdcl.Problem{
		Requirements: p.Requirements([]string{"a"}),
		Constraints:  p.VersionSets([]string{"c 2..9"}),
	})
	assertInline(t, transactionToString(p, solved), `
		a=1
	`)
}

// A condition on a package that allows several versions is enforced like
// any other (resolvo never enforces it).
func TestConditionOnMultiVersionPackage(t *testing.T) {
	p := fromPackages([]pk{{"a", 1, deps("b; if c 2")}, {"c", 1, nil}, {"c", 2, nil}})
	p.SetAllowMultiple("c")
	assertInline(t, solveUnsat(t, p, []string{"a", "c 2"}), `
		a * cannot be installed because there are no viable options:
		└─ a 1 would require
		   └─ b *, for which no candidates were found.
		The following packages are incompatible
		└─ c >=2, <3 can be installed with any of the following options:
		   └─ c 2
	`)

	q := fromPackages([]pk{{"a", 1, deps("b; if c")}, {"c", 2, nil}})
	q.SetAllowMultiple("c")
	assertInline(t, solveUnsat(t, q, []string{"a", "c"}), `
		a * cannot be installed because there are no viable options:
		└─ a 1 would require
		   └─ b *, for which no candidates were found.
		The following packages are incompatible
		└─ c * can be installed with any of the following options:
		   └─ c 2
	`)

	// The condition does not hold without c 2, so a 1 is installable.
	r := fromPackages([]pk{{"a", 1, deps("b; if c 2")}, {"c", 1, nil}, {"c", 2, nil}})
	r.SetAllowMultiple("c")
	assertInline(t, solveSnapshot(t, r, []string{"a", "c 1"}), `
		a=1
		c=1
	`)
}

// Solve never panics: panics in the provider or in the Pool it uses come
// back as errors wrapping ErrInternal and carrying the panic value.
func TestProviderPanicsAreErrors(t *testing.T) {
	t.Run("unknown solvable id", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		h.candidates = func(ctx context.Context, n cdcl.NameID) (*cdcl.Candidates, error) {
			c, err := h.Provider.GetCandidates(ctx, n)
			if c != nil {
				// An id the pool never issued: the pool panics on it.
				c.Candidates = append(c.Candidates, cdcl.SolvableID(h.Pool.SolvableCount()+7))
			}
			return c, err
		}
		_, err := solveHostile(t, h, []string{"a"})
		if !errors.Is(err, cdcl.ErrInternal) || !strings.Contains(err.Error(), "unknown solvable id") {
			t.Fatalf("got %v, want ErrInternal naming the unknown solvable id", err)
		}
	})
	t.Run("get dependencies panics", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		h.deps = func(context.Context, cdcl.SolvableID) (cdcl.Dependencies, error) {
			panic("provider is broken")
		}
		_, err := solveHostile(t, h, []string{"a"})
		if !errors.Is(err, cdcl.ErrInternal) || !strings.Contains(err.Error(), "provider is broken") {
			t.Fatalf("got %v, want ErrInternal carrying the panic value", err)
		}
	})
	t.Run("interner panics", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		h.union = func(cdcl.VersionSetUnionID) []cdcl.VersionSetID { panic(errors.New("no unions here")) }
		_, err := solveHostile(t, h, []string{"a | b"})
		if !errors.Is(err, cdcl.ErrInternal) || !strings.Contains(err.Error(), "no unions here") {
			t.Fatalf("got %v, want ErrInternal carrying the panic value", err)
		}
	})
}

// Provider IDs at or above MaxID fail the solve with ErrInvalidID instead of
// making the solver allocate in proportion to the ID.
func TestProviderIDsAreBounded(t *testing.T) {
	for _, id := range []cdcl.SolvableID{cdcl.MaxID, 1 << 26, math.MaxUint32} {
		h := &hostileProvider{Provider: hostileBase()}
		h.filter = func(c []cdcl.SolvableID, vs cdcl.VersionSetID, inv bool) []cdcl.SolvableID {
			out := h.Provider.FilterCandidates(c, vs, inv)
			if len(out) > 0 && !inv {
				out = append(out, id)
			}
			return out
		}
		// Answer for the bogus id instead of panicking like the pool.
		h.name = func(s cdcl.SolvableID) cdcl.NameID {
			if int(s) >= h.Pool.SolvableCount() {
				return 0
			}
			return h.Provider.SolvableName(s)
		}
		h.sort = func(context.Context, *cdcl.SolverCache, []cdcl.SolvableID) error { return nil }
		var before, after runtimeMem
		before.read()
		_, err := solveHostile(t, h, []string{"a"})
		after.read()
		if !errors.Is(err, cdcl.ErrInvalidID) {
			t.Fatalf("id %d: got %v, want ErrInvalidID", id, err)
		}
		if grew := after.total - before.total; grew > 64<<20 {
			t.Errorf("id %d: the solve allocated %d bytes", id, grew)
		}
	}
	// A name id from the interner.
	h := &hostileProvider{Provider: hostileBase()}
	h.candidates = func(ctx context.Context, n cdcl.NameID) (*cdcl.Candidates, error) {
		c, err := h.Provider.GetCandidates(ctx, n)
		if c != nil {
			c.HintDependencies, c.HintSolvables = cdcl.HintSome, []cdcl.SolvableID{1 << 30}
		}
		return c, err
	}
	if _, err := solveHostile(t, h, []string{"a"}); !errors.Is(err, cdcl.ErrInvalidID) {
		t.Fatalf("hinted id: got %v, want ErrInvalidID", err)
	}
}

// An overly complex condition is reported with the condition and the
// requirement that carries it.
func TestConditionTooComplexNamesTheInput(t *testing.T) {
	h := &hostileProvider{Provider: hostileBase()}
	h.condition = func(c cdcl.ConditionID) cdcl.Condition { return cdcl.BinaryCondition(cdcl.Or, c, c) }
	_, err := solveHostile(t, h, []string{"a 2"})
	if !errors.Is(err, cdcl.ErrConditionTooComplex) {
		t.Fatalf("got %v, want ErrConditionTooComplex", err)
	}
	for _, want := range []string{"condition 0", `"c *"`, "a=2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// condaShaped builds a solvable conda-like ecosystem after the review's
// generator: a "python" package with six versions, long-tailed version
// lists, builds pinned to a python version, dependencies on lower-level
// packages (any version or a recent one), and two conditional dependencies
// per solvable on optional feature packages that nothing requires. It
// returns the provider and its root specs.
func condaShaped(names int, vscale float64, seed uint64) (*bundlebox.Provider, []string) {
	r := rand.New(rand.NewPCG(seed, 99))
	p := bundlebox.New()
	for m := uint32(0); m < 6; m++ {
		p.AddPackage("p0", bundlebox.NewPack(m), nil, nil)
	}
	nver := map[int]int{0: 6}
	noarch := map[int]bool{}
	for i := 1; i < names; i++ {
		v := int(math.Pow(1/(1-r.Float64()), 1/1.2) * 8 * vscale)
		nver[i] = min(max(2, v), int(400*vscale)+2)
		noarch[i] = r.Float64() < 0.3
	}
	for i := 1; i < names; i++ {
		var targets []int
		if lower := names - i - 1; lower > 0 {
			for k := min(lower, r.IntN(9)); k > 0; k-- {
				targets = append(targets, i+1+r.IntN(lower))
			}
		}
		// Conditional dependencies point at noarch packages, which accept
		// any python from their lower bound up, so the problem stays
		// solvable.
		pick := func() int {
			for {
				if c := 1 + r.IntN(names-1); noarch[c] {
					return c
				}
			}
		}
		for v := 0; v < nver[i]; v++ {
			frac := float64(v) / float64(max(1, nver[i]-1))
			lo := min(5, int(frac*4))
			hi := min(5, lo+1+r.IntN(3))
			var ds []string
			for _, tg := range targets {
				tv := nver[tg]
				if r.Float64() < 0.6 {
					ds = append(ds, fmt.Sprintf("p%d", tg))
				} else {
					// A recent version: the newest builds support the
					// newest python, so the problem stays solvable.
					lov := tv/2 + r.IntN(max(1, tv-tv/2))
					ds = append(ds, fmt.Sprintf("p%d %d..%d", tg, lov*8, tv*8))
				}
			}
			// Two conditional dependencies, like optional extras: they
			// apply when a feature package is installed, which nothing
			// requires, so they stay deferred.
			ds = append(ds, fmt.Sprintf("p%d; if f%d", pick(), r.IntN(50)), fmt.Sprintf("p%d; if f%d", pick(), r.IntN(50)))
			if noarch[i] {
				p.AddPackage(fmt.Sprintf("p%d", i), bundlebox.NewPack(uint32(v*8)), append(ds, fmt.Sprintf("p0 %d..6", lo)), nil)
				continue
			}
			for b, m := 0, lo; m <= hi && b < 8; b, m = b+1, m+1 {
				p.AddPackage(fmt.Sprintf("p%d", i), bundlebox.NewPack(uint32(v*8+b)), append(append([]string(nil), ds...), fmt.Sprintf("p0 %d", m)), nil)
			}
		}
	}
	for f := 0; f < 50; f++ {
		p.AddPackage(fmt.Sprintf("f%d", f), bundlebox.NewPack(0), nil, nil)
	}
	roots := []string{"p0"}
	for k := 0; k < 25 && k < names/4; k++ {
		roots = append(roots, fmt.Sprintf("p%d", 1+r.IntN(max(1, names/4-1))))
	}
	return p, roots
}

// The check for deferred conditions that started to hold is driven by new
// assignments instead of rescanning every deferred requirement each round
// (resolvo's rescan dominated large conda-shaped solves). This solves a
// conda-shaped problem with conditional dependencies and verifies the
// answer; BenchmarkSolveCondaConditional measures it.
func TestCondaShapedConditionalSolve(t *testing.T) {
	names := 1000
	if testing.Short() {
		names = 200
	}
	p, roots := condaShaped(names, 1, 7)
	reqs := p.Requirements(roots)
	start := time.Now()
	// A production solver: the debug checks are O(clauses) per decision.
	solver := cdcl.NewSolver(p)
	sol, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs})
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	t.Logf("%d solvables interned, %d selected, %d clauses, %d deferred, %v",
		p.Pool.SolvableCount(), len(sol), solver.ClauseCount(), solver.DeferredRequirementsCount(), time.Since(start))
	if len(sol) == 0 {
		t.Fatal("empty solution")
	}
}

func BenchmarkSolveCondaConditional(b *testing.B) {
	type job struct {
		solver *cdcl.Solver
		reqs   []cdcl.ConditionalRequirement
	}
	jobs := make([]job, b.N)
	for i := range jobs {
		p, roots := condaShaped(2000, 1, 7)
		jobs[i] = job{cdcl.NewSolver(p), p.Requirements(roots)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := jobs[i].solver.Solve(context.Background(), cdcl.Problem{Requirements: jobs[i].reqs}); err != nil {
			b.Fatalf("solve: %v", err)
		}
	}
}

// runtimeMem samples the total bytes allocated by the process.
type runtimeMem struct{ total uint64 }

func (m *runtimeMem) read() {
	var s runtime.MemStats
	runtime.ReadMemStats(&s)
	m.total = s.TotalAlloc
}

// solveConflict solves op, which must be unsolvable, and returns its
// provider and conflict.
func solveConflict(t *testing.T, op oracleProblem) (*bundlebox.Provider, *cdcl.Conflict) {
	t.Helper()
	p, problem := buildProblem(op)
	_, err := newSolver(p).Solve(context.Background(), problem)
	var conflict *cdcl.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v, want a conflict", err)
	}
	return p, conflict
}

// An explanation of a cyclic conflict graph terminates. resolvo (and cdcl
// before) expanded a candidate again each time the tree reached it, so
// rendering this error never finished and ran out of memory: a 2 requires
// b, b 4 requires a and c, and c 4 rules a 2 out.
func TestExplanationOfCyclicConflictGraph(t *testing.T) {
	packages := func(bDeps ...string) []oraclePackage {
		return []oraclePackage{
			{Name: "a", Version: 2, Deps: []string{"b"}},
			{Name: "b", Version: 4, Deps: bDeps},
			{Name: "c", Version: 4, Constrains: []string{"a 0..1"}},
		}
	}
	cases := []struct {
		name string
		op   oracleProblem
		want string
	}{
		{"root a or b", oracleProblem{Packages: packages("c", "a"), Requirements: []string{"a | b"}}, `
			cdcl: no solution satisfies the requirements:
			The following packages are incompatible
			└─ a * | b * cannot be installed because there are no viable options:
			   ├─ a 2 would require
			   │  └─ b *, which cannot be installed because there are no viable options:
			   │     └─ b 4 would require
			   │        ├─ a *, which cannot be installed because there are no viable options:
			   │        │  └─ a 2, as reported above.
			   │        └─ c *, which cannot be installed because there are no viable options:
			   │           └─ c 4 would constrain
			   │              └─ a >=0, <1, which conflicts with any installable versions previously reported
			   └─ b 4, as reported above.
		`},
		{"root a", oracleProblem{Packages: packages("c", "a"), Requirements: []string{"a"}}, `
			cdcl: no solution satisfies the requirements:
			The following packages are incompatible
			└─ a * cannot be installed because there are no viable options:
			   └─ a 2 would require
			      └─ b *, which cannot be installed because there are no viable options:
			         └─ b 4 would require
			            └─ c *, which cannot be installed because there are no viable options:
			               └─ c 4 would constrain
			                  └─ a >=0, <1, which conflicts with any installable versions previously reported
		`},
		{"root b", oracleProblem{Packages: packages("a", "c"), Requirements: []string{"b"}}, `
			cdcl: no solution satisfies the requirements:
			The following packages are incompatible
			└─ b * cannot be installed because there are no viable options:
			   └─ b 4 would require
			      ├─ a *, which can be installed with any of the following options:
			      │  └─ a 2
			      └─ c *, which cannot be installed because there are no viable options:
			         └─ c 4 would constrain
			            └─ a >=0, <1, which conflicts with any installable versions previously reported
		`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, conflict := solveConflict(t, c.op)
			assertInline(t, conflict.Error()+"\n", c.want)
		})
	}
}

// layered is a conflict whose graph is a DAG of n layers of two packages
// each, x_i and y_i, both requiring "x_{i+1} | y_{i+1}"; the last layer
// requires a package that does not exist. Every candidate is reached from
// both candidates of the layer above, so expanding each visit (as resolvo
// does) doubles the explanation per layer.
func layered(n int, pad string) oracleProblem {
	name := func(p string, i int) string { return fmt.Sprintf("%s%s%d", p, pad, i) }
	var op oracleProblem
	for i := 0; i < n; i++ {
		dep := name("x", i+1) + " | " + name("y", i+1)
		if i == n-1 {
			dep = "missing"
		}
		op.Packages = append(op.Packages,
			oraclePackage{Name: name("x", i), Version: 1, Deps: []string{dep}},
			oraclePackage{Name: name("y", i), Version: 1, Deps: []string{dep}})
	}
	op.Requirements = []string{name("x", 0) + " | " + name("y", 0)}
	return op
}

// The explanation of a layered DAG has a number of lines linear in the
// number of layers (resolvo's doubles per layer: 16 layers render 39 MB, 24
// about 10 GB). Its size in bytes grows faster only through the tree
// indentation, which is proportional to the depth.
func TestExplanationOfLayeredGraphIsLinear(t *testing.T) {
	lines := map[int]int{}
	for _, n := range []int{4, 8, 16, 32} {
		_, conflict := solveConflict(t, layered(n, ""))
		text := conflict.Error()
		if strings.Contains(text, "truncated") {
			t.Fatalf("%d layers: explanation truncated", n)
		}
		lines[n] = strings.Count(text, "\n")
		t.Logf("%d layers: %d lines, %d bytes", n, lines[n], len(text))
	}
	for _, n := range []int{16, 32} {
		// Linear: doubling the layers adds what the previous doubling
		// added, twice over.
		if got, want := lines[n]-lines[n/2], 2*(lines[n/2]-lines[n/4]); got != want {
			t.Errorf("%d layers: %d more lines than %d layers, want %d", n, got, n/2, want)
		}
	}
}

// Explanations and Graphviz renderings are capped, so printing the error of
// any conflict is cheap.
func TestExplanationIsCapped(t *testing.T) {
	p, problem := buildProblem(layered(1000, strings.Repeat("_", 100)))
	// A production solver: the debug checks are slow on 2000 packages.
	_, err := cdcl.NewSolver(p).Solve(context.Background(), problem)
	var conflict *cdcl.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v, want a conflict", err)
	}
	text := conflict.Display(p)
	if !strings.HasSuffix(text, "… explanation truncated\n") {
		t.Errorf("explanation does not end with the truncation marker:\n%s", text[max(0, len(text)-500):])
	}
	if n := strings.Count(text, "\n"); len(text) > 64<<10 || n > 2000 {
		t.Errorf("explanation has %d bytes and %d lines, want at most 64 KiB and 2000", len(text), n)
	}

	graph, err := conflict.Graph()
	if err != nil {
		t.Fatal(err)
	}
	var dot strings.Builder
	if err := graph.Graphviz(&dot, p, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dot.String(), "/* graph truncated */}") || dot.Len() > 1<<20 {
		t.Errorf("graphviz has %d bytes and ends %q, want at most 1 MiB, truncated", dot.Len(), dot.String()[max(0, dot.Len()-40):])
	}
}

// wideCondition returns a provider whose n solvables s0..s{n-1}, all of
// them root requirements, each require t under the condition
// (x0 or y0) and (x1 or y1) and … and (x{terms-1} or y{terms-1}): its DNF
// has 2^terms disjuncts of terms version sets each. Nothing installs an x
// or a y, so every disjunct of every solvable is deferred.
func wideCondition(terms, n int) (*hostileProvider, []string) {
	ps := []pk{{"t", 1, nil}}
	for j := 0; j < terms; j++ {
		ps = append(ps, pk{fmt.Sprintf("x%d", j), 1, nil}, pk{fmt.Sprintf("y%d", j), 1, nil})
	}
	roots := make([]string, n)
	for i := range roots {
		roots[i] = fmt.Sprintf("s%d", i)
		ps = append(ps, pk{roots[i], 1, deps("t; if x0")})
	}
	h := &hostileProvider{Provider: fromPackages(ps)}
	// The bundlebox condition "x0" of every s stands for the wide one,
	// built from condition ids the bundlebox provider never issues.
	const base = 1 << 20
	conds := map[cdcl.ConditionID]cdcl.Condition{}
	add := func(c cdcl.Condition) cdcl.ConditionID {
		id := cdcl.ConditionID(base + len(conds))
		conds[id] = c
		return id
	}
	leaf := func(spec string) cdcl.ConditionID {
		return add(cdcl.RequirementCondition(h.VersionSets([]string{spec})[0]))
	}
	var wide cdcl.ConditionID
	for j := 0; j < terms; j++ {
		or := add(cdcl.BinaryCondition(cdcl.Or, leaf(fmt.Sprintf("x%d", j)), leaf(fmt.Sprintf("y%d", j))))
		if j == 0 {
			wide = or
		} else {
			wide = add(cdcl.BinaryCondition(cdcl.And, wide, or))
		}
	}
	h.condition = func(c cdcl.ConditionID) cdcl.Condition {
		if c < base {
			return conds[wide]
		}
		return conds[c]
	}
	return h, roots
}

// A condition is expanded and resolved once and shared by every solvable
// that carries it, and conditions above 1024 version sets are rejected.
// Twelve ANDed (x or y) terms on 800 solvables used to keep 1.38 GB live;
// seven on 800 kept 30 MB live and allocated 109 MB.
func TestWideConditionMemory(t *testing.T) {
	h, roots := wideCondition(7, 800) // 128 disjuncts of 7 version sets
	reqs := h.Requirements(roots)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	solver := cdcl.NewSolver(h)
	sol, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs})
	runtime.GC()
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if len(sol) != 800 || solver.DeferredRequirementsCount() != 800*128 {
		t.Fatalf("got %d solvables and %d deferred disjuncts, want 800 and %d", len(sol), solver.DeferredRequirementsCount(), 800*128)
	}
	live := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("live %d KiB, allocated %d KiB", live>>10, allocated>>10)
	if live > 8<<20 || allocated > 32<<20 {
		t.Errorf("solve keeps %d KiB live and allocates %d KiB, want at most 8 MiB and 32 MiB", live>>10, allocated>>10)
	}
	runtime.KeepAlive(solver)

	h, roots = wideCondition(12, 800)
	_, err = cdcl.NewSolver(h).Solve(context.Background(), cdcl.Problem{Requirements: h.Requirements(roots)})
	if !errors.Is(err, cdcl.ErrConditionTooComplex) || !strings.Contains(err.Error(), "more than 1024 version sets") {
		t.Fatalf("got %v, want ErrConditionTooComplex", err)
	}
}

// A SortCandidates panic does not leave the version set marked as being
// sorted: solving again with the same Solver works instead of failing with
// a spurious ErrRecursiveSort.
func TestSortPanicDoesNotPoisonSolver(t *testing.T) {
	h := &hostileProvider{Provider: hostileBase()}
	panicked := false
	h.sort = func(ctx context.Context, cache *cdcl.SolverCache, s []cdcl.SolvableID) error {
		if !panicked {
			panicked = true
			panic("sort failed")
		}
		return h.Provider.SortCandidates(ctx, cache, s)
	}
	reqs := h.Requirements([]string{"a"})
	solver := cdcl.NewSolver(h)
	if _, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs}); !errors.Is(err, cdcl.ErrInternal) {
		t.Fatalf("first solve: got %v, want ErrInternal", err)
	}
	sol, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs})
	if err != nil {
		t.Fatalf("second solve: %v", err)
	}
	assertInline(t, transactionToString(h, sol), `
		a=2
		b=2
		c=2
	`)
}

// A random problem (union-heavy generator) whose conflict graph has the
// cycle p2 2 -> p1 5 -> p2 2: resolvo's rendering of it never finishes.
func TestExplanationOfRandomCyclicConflict(t *testing.T) {
	const problem = `{"packages": [{"name": "p0", "version": 2, "deps": ["p2 | p3 3..5", "p3; if e0 or p3 0 and e0"]}, {"name": "p2", "version": 1, "deps": ["p4 0..2"]}, {"name": "p3", "version": 0, "deps": ["p2 1..3"]}, {"name": "p2", "version": 2, "deps": ["p1 5..8 | p3 2..4", "p2; if e1 and e2 and e2"], "constrains": ["p3 5..6"]}, {"name": "p1", "version": 4, "constrains": ["p2"], "unknown": true}, {"name": "e1", "version": 0, "deps": ["p4 4..8"]}, {"name": "p0", "version": 0, "deps": ["p4", "p0 2..4"]}, {"name": "e2", "version": 0, "deps": ["p1 0..2"]}, {"name": "p4", "version": 1, "deps": ["p1 0..4"]}, {"name": "p0", "version": 4, "deps": ["p4 2..3; if e0 or e1", "p1 | p1 | p2"], "constrains": ["p1 4"]}, {"name": "e0", "version": 0, "deps": ["p1 0..4"]}, {"name": "p1", "version": 5, "deps": ["p2 1..2; if e0 or p2 2..5 or e1", "p2 0..3", "p2 | p4 5..8 | p4; if e0 or e2"], "constrains": ["p4"]}, {"name": "p0", "version": 3, "deps": ["p1 1; if e1 or p4 2 or p1 4..8"]}, {"name": "p4", "version": 2, "deps": ["p0", "p3 4", "p2 5..8 | p2 4; if p0 2..6 and p3 0..2 and e1"]}, {"name": "p2", "version": 5, "deps": ["p2 1"]}],
		"favored": [["p0", 2]], "locked": [["p2", 2]], "excluded": [["p0", 2, "it is retracted"], ["e2", 0, "it is retracted"]],
		"requirements": ["p2 | p1", "p2 5..9; if p3 2 or e1"]}`
	var op oracleProblem
	if err := json.Unmarshal([]byte(problem), &op); err != nil {
		t.Fatal(err)
	}
	p, conflict := solveConflict(t, op)
	assertInline(t, conflict.Display(p), `
		The following packages are incompatible
		└─ p2 * | p1 * can be installed with any of the following options:
		   ├─ p2 5
		   ├─ p2 2 would require
		   │  └─ p1 >=5, <8 | p3 >=2, <4, which can be installed with any of the following options:
		   │     └─ p1 5 would require
		   │        ├─ p2 >=0, <3, which can be installed with any of the following options:
		   │        │  ├─ p2 2, as reported above.
		   │        │  └─ p2 1
		   │        └─ p2 >=1, <2, which can be installed with any of the following options:
		   │           └─ p2 1
		   ├─ p2 1
		   └─ p1 5, as reported above.
		└─ p2 2 is locked, but another version is required as reported above
	`)
}
