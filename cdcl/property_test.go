package cdcl_test

// Property test: random small problems solved by cdcl and by exhaustive
// enumeration. A solution must satisfy every requirement, constraint, lock,
// exclusion and the one-version-per-package rule; cdcl must report unsat
// exactly when no assignment works.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

// propSpec is a name with the version range [lo, hi), or all versions when
// full is set.
type propSpec struct {
	name   int
	full   bool
	lo, hi uint32
}

func (s propSpec) matches(name int, v uint32) bool {
	return s.name == name && (s.full || (v >= s.lo && v < s.hi))
}

func (s propSpec) String() string {
	if s.full {
		return fmt.Sprintf("p%d", s.name)
	}
	return fmt.Sprintf("p%d %d..%d", s.name, s.lo, s.hi)
}

// propReq is a union of specs, optionally gated by a condition.
type propReq struct {
	specs []propSpec
	// cond is a condition "c0 op1 c1 op2 c2 ...", parsed left-associative
	// with equal precedence like the BundleBox parser; ops[i] joins cond[i]
	// and cond[i+1] and is true for "and".
	cond []propSpec
	ands []bool
}

func (r propReq) String() string {
	parts := make([]string, len(r.specs))
	for i, s := range r.specs {
		parts[i] = s.String()
	}
	out := strings.Join(parts, " | ")
	if len(r.cond) > 0 {
		out += "; if " + r.cond[0].String()
		for i, c := range r.cond[1:] {
			op := " or "
			if r.ands[i] {
				op = " and "
			}
			out += op + c.String()
		}
	}
	return out
}

// conditionDNF returns the condition as a disjunction of conjunctions.
func (r propReq) conditionDNF() [][]propSpec {
	dnf := [][]propSpec{{r.cond[0]}}
	for i, c := range r.cond[1:] {
		if !r.ands[i] {
			dnf = append(dnf, []propSpec{c})
			continue
		}
		for j := range dnf {
			dnf[j] = append(append([]propSpec(nil), dnf[j]...), c)
		}
	}
	return dnf
}

type propVersion struct {
	version    uint32
	deps       []propReq
	constrains []propSpec
	unknown    bool
	excluded   bool
}

type propPackage struct {
	versions      []propVersion
	locked        int // index into versions, or -1
	allowMultiple bool
}

type propProblem struct {
	pkgs        []propPackage
	roots       []propReq
	constraints []propSpec
}

func genPropProblem(r *rand.Rand) propProblem {
	n := 1 + r.IntN(8)
	spec := func() propSpec {
		s := propSpec{name: r.IntN(n)}
		if r.IntN(3) == 0 {
			s.full = true
			return s
		}
		s.lo = uint32(r.IntN(5))
		s.hi = s.lo + 1 + uint32(r.IntN(3))
		return s
	}
	req := func() propReq {
		q := propReq{specs: []propSpec{spec()}}
		if r.IntN(6) == 0 {
			q.specs = append(q.specs, spec())
		}
		if r.IntN(4) == 0 {
			q.cond = []propSpec{spec()}
			for k := r.IntN(3); k > 0; k-- {
				q.ands = append(q.ands, r.IntN(2) == 0)
				q.cond = append(q.cond, spec())
			}
		}
		return q
	}
	p := propProblem{pkgs: make([]propPackage, n)}
	for i := range p.pkgs {
		perm := r.Perm(5)
		pkg := propPackage{locked: -1, allowMultiple: r.IntN(10) == 0}
		for _, v := range perm[:1+r.IntN(4)] {
			pv := propVersion{version: uint32(v), unknown: r.IntN(20) == 0, excluded: r.IntN(15) == 0}
			for d := r.IntN(3); d > 0; d-- {
				pv.deps = append(pv.deps, req())
			}
			for c := r.IntN(2); c > 0; c-- {
				if s := spec(); s.name != i {
					pv.constrains = append(pv.constrains, s)
				}
			}
			pkg.versions = append(pkg.versions, pv)
		}
		if r.IntN(8) == 0 {
			pkg.locked = r.IntN(len(pkg.versions))
		}
		p.pkgs[i] = pkg
	}
	for k := 1 + r.IntN(3); k > 0; k-- {
		p.roots = append(p.roots, req())
	}
	if r.IntN(5) == 0 {
		p.constraints = append(p.constraints, spec())
	}
	return p
}

// provider renders the problem as a BundleBox provider.
func (p propProblem) provider() (*bundlebox.Provider, cdcl.Problem) {
	bp := bundlebox.New()
	for i, pkg := range p.pkgs {
		name := fmt.Sprintf("p%d", i)
		for _, v := range pkg.versions {
			var ds, cs []string
			for _, d := range v.deps {
				ds = append(ds, d.String())
			}
			for _, c := range v.constrains {
				cs = append(cs, c.String())
			}
			pack := bundlebox.NewPack(v.version)
			if v.unknown {
				pack = pack.WithUnknownDeps()
			}
			bp.AddPackage(name, pack, ds, cs)
			if v.excluded {
				bp.Exclude(name, v.version, "excluded")
			}
		}
		if pkg.locked >= 0 {
			bp.SetLocked(name, pkg.versions[pkg.locked].version)
		}
		if pkg.allowMultiple {
			bp.SetAllowMultiple(name)
		}
	}
	var roots, cons []string
	for _, r := range p.roots {
		roots = append(roots, r.String())
	}
	for _, c := range p.constraints {
		cons = append(cons, c.String())
	}
	problem := cdcl.Problem{Requirements: bp.Requirements(roots), Constraints: bp.VersionSets(cons)}
	return bp, problem
}

// selection holds, per package, a bitmask of selected version indices.
type selection []uint32

func (p propProblem) selectable(name, idx int) bool {
	pkg := p.pkgs[name]
	v := pkg.versions[idx]
	// Exclusions and locks are keyed by plain packs; a pack with unknown
	// dependencies is unusable anyway.
	if v.unknown || v.excluded {
		return false
	}
	if pkg.locked >= 0 && !pkg.versions[pkg.locked].unknown && pkg.locked != idx {
		return false
	}
	return true
}

func (p propProblem) satisfied(sel selection, r propReq) bool {
	if len(r.cond) > 0 && !p.conditionForces(sel, r) {
		return true
	}
	for _, s := range r.specs {
		if s.name >= len(sel) {
			continue
		}
		for idx, v := range p.pkgs[s.name].versions {
			if sel[s.name]&(1<<idx) != 0 && s.matches(s.name, v.version) {
				return true
			}
		}
	}
	return false
}

// conditionForces reports whether a conditional requirement must hold in
// sel: whether every conjunct of some disjunct of its condition has a
// selected matching version.
func (p propProblem) conditionForces(sel selection, r propReq) bool {
	for _, conj := range r.conditionDNF() {
		holds := true
		for _, c := range conj {
			if !p.anySelectedMatches(sel, c) {
				holds = false
				break
			}
		}
		if holds {
			return true
		}
	}
	return false
}

func (p propProblem) anySelectedMatches(sel selection, c propSpec) bool {
	for idx, v := range p.pkgs[c.name].versions {
		if sel[c.name]&(1<<idx) != 0 && c.matches(c.name, v.version) {
			return true
		}
	}
	return false
}

func (p propProblem) respects(sel selection, c propSpec) bool {
	for idx, v := range p.pkgs[c.name].versions {
		if sel[c.name]&(1<<idx) != 0 && !c.matches(c.name, v.version) {
			return false
		}
	}
	return true
}

// valid checks a selection against every rule of the problem.
func (p propProblem) valid(sel selection) error {
	for name, pkg := range p.pkgs {
		mask := sel[name]
		if mask&(mask-1) != 0 && !pkg.allowMultiple {
			return fmt.Errorf("p%d: several versions selected", name)
		}
		for idx, v := range pkg.versions {
			if mask&(1<<idx) == 0 {
				continue
			}
			if !p.selectable(name, idx) {
				return fmt.Errorf("p%d=%d is excluded, locked out or has unknown dependencies", name, v.version)
			}
			for _, d := range v.deps {
				if !p.satisfied(sel, d) {
					return fmt.Errorf("p%d=%d: requirement %s unsatisfied", name, v.version, d)
				}
			}
			for _, c := range v.constrains {
				if !p.respects(sel, c) {
					return fmt.Errorf("p%d=%d: constraint %s violated", name, v.version, c)
				}
			}
		}
	}
	for _, r := range p.roots {
		if !p.satisfied(sel, r) {
			return fmt.Errorf("root requirement %s unsatisfied", r)
		}
	}
	for _, c := range p.constraints {
		if !p.respects(sel, c) {
			return fmt.Errorf("root constraint %s violated", c)
		}
	}
	return nil
}

// decided reports whether every name of r is at most upto.
func decided(r propReq, upto int) bool {
	for _, s := range r.cond {
		if s.name > upto {
			return false
		}
	}
	for _, s := range r.specs {
		if s.name > upto {
			return false
		}
	}
	return true
}

// partiallyValid checks the rules that only involve packages 0..upto, whose
// selection is final. It never rejects a selection that valid accepts.
func (p propProblem) partiallyValid(sel selection, upto int) bool {
	for name := 0; name <= upto; name++ {
		for idx, v := range p.pkgs[name].versions {
			if sel[name]&(1<<idx) == 0 {
				continue
			}
			if !p.selectable(name, idx) {
				return false
			}
			for _, d := range v.deps {
				if decided(d, upto) && !p.satisfied(sel, d) {
					return false
				}
			}
			for _, c := range v.constrains {
				if c.name <= upto && !p.respects(sel, c) {
					return false
				}
			}
		}
	}
	for _, r := range p.roots {
		if decided(r, upto) && !p.satisfied(sel, r) {
			return false
		}
	}
	for _, c := range p.constraints {
		if c.name <= upto && !p.respects(sel, c) {
			return false
		}
	}
	return true
}

// bruteForce reports whether any selection is valid, enumerating every
// selection (pruning partial selections that already break a rule).
func (p propProblem) bruteForce() bool {
	sel := make(selection, len(p.pkgs))
	var rec func(name int) bool
	rec = func(name int) bool {
		if name > 0 && !p.partiallyValid(sel, name-1) {
			return false
		}
		if name == len(p.pkgs) {
			return p.valid(sel) == nil
		}
		pkg := p.pkgs[name]
		limit := uint32(1) << len(pkg.versions)
		for mask := uint32(0); mask < limit; mask++ {
			if mask&(mask-1) != 0 && !pkg.allowMultiple {
				continue
			}
			sel[name] = mask
			if rec(name + 1) {
				return true
			}
		}
		sel[name] = 0
		return false
	}
	return rec(0)
}

// checkExplanation renders the explanation of an unsat answer as printing
// the error does, and its Graphviz form, and checks that the explanation
// is linear in the size of the conflict graph: every candidate is expanded
// at most once per section, so each edge accounts for a bounded number of
// lines.
func checkExplanation(t testing.TB, in cdcl.Interner, err error) {
	t.Helper()
	var conflict *cdcl.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("unsat answer is not a conflict: %v", err)
	}
	graph, gerr := conflict.Graph()
	if gerr != nil {
		t.Fatalf("graph: %v", gerr)
	}
	var dot strings.Builder
	if gerr := graph.Graphviz(&dot, in, true); gerr != nil {
		t.Fatalf("graphviz: %v", gerr)
	}
	text := err.Error()
	limit := 8*graph.EdgeCount() + 4
	if lines := strings.Count(text, "\n"); lines > limit || strings.Contains(text, "explanation truncated") {
		t.Fatalf("explanation has %d lines for %d conflict graph edges (limit %d):\n%s", lines, graph.EdgeCount(), limit, text)
	}
}

// checkProblem solves p with cdcl and compares with brute force.
func checkProblem(t testing.TB, p propProblem) {
	checkProblemWith(t, p, nil)
}

// checkProblemWith is checkProblem with a hook to configure the solver.
func checkProblemWith(t testing.TB, p propProblem, configure func(*cdcl.Solver)) {
	bp, problem := p.provider()
	solver := newSolver(bp)
	if configure != nil {
		configure(solver)
	}
	solved, err := solver.Solve(context.Background(), problem)
	want := p.bruteForce()
	if err != nil {
		if !errors.Is(err, cdcl.ErrUnsolvable) {
			t.Fatalf("solve: %v", err)
		}
		if want {
			t.Fatalf("cdcl reports unsat but a solution exists:\n%s", err)
		}
		checkExplanation(t, bp, err)
		return
	}
	if !want {
		t.Fatalf("cdcl found a solution but brute force found none:\n%s", transactionToString(bp, solved))
	}
	sel := make(selection, len(p.pkgs))
	for _, s := range solved {
		rec := bp.Pool.ResolveSolvable(s)
		var name int
		fmt.Sscanf(bp.DisplayName(rec.Name), "p%d", &name)
		found := false
		for idx, v := range p.pkgs[name].versions {
			if v.version == rec.Record.Version && v.unknown == rec.Record.UnknownDeps {
				sel[name] |= 1 << idx
				found = true
			}
		}
		if !found {
			t.Fatalf("solution contains unknown solvable %s", bp.DisplaySolvable(s))
		}
	}
	if err := p.valid(sel); err != nil {
		t.Fatalf("invalid solution %q: %v", transactionToString(bp, solved), err)
	}
}

func TestPropertyBruteForce(t *testing.T) {
	cases := 3000
	if testing.Short() {
		cases = 300
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < cases; i++ {
		p := genPropProblem(r)
		t.Run(fmt.Sprint(i), func(t *testing.T) { checkProblem(t, p) })
	}
}

// Non-standard activity parameters disable the decide queue's activity
// shortcuts (every eligible clause is visited); answers must stay correct.
func TestPropertyNonStandardActivity(t *testing.T) {
	params := [][2]float32{{0, 0.95}, {1, -0.5}, {-1, 0.5}, {2, 1}}
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 1200; i++ {
		p := genPropProblem(r)
		ap := params[i%len(params)]
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			checkProblemWith(t, p, func(s *cdcl.Solver) { s.SetActivityParams(ap[0], ap[1]) })
		})
	}
}
