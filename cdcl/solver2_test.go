// Cases from resolvo tests/solver/main.rs (BSD-3-Clause, see NOTICE),
// continued from solver_test.go.

package cdcl_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

func Test_conditional_requirements(t *testing.T) {
	p := fromPackages([]pk{{"foo", 1, deps("baz; if bar")}, {"bar", 1, nil}, {"baz", 1, nil}})
	reqs := p.Requirements([]string{"foo", "bar"})
	got := solveForSnapshot(t, p, reqs, nil)
	assertInline(t, got, `
		bar=1
		baz=1
		foo=1
	`)
	// The upstream file snapshot predates the inline one; both agree.
	assertSnapshot(t, "conditional_requirements", got)
}

func Test_conditional_unsolvable(t *testing.T) {
	p := fromPackages([]pk{{"foo", 1, deps("baz 2; if bar")}, {"bar", 1, nil}, {"baz", 1, nil}})
	reqs := p.Requirements([]string{"foo", "bar"})
	got := solveForSnapshot(t, p, reqs, nil)
	assertInline(t, got, `
		foo * cannot be installed because there are no viable options:
		└─ foo 1 would require
		   └─ baz >=2, <3, for which no candidates were found.
		The following packages are incompatible
		└─ bar * can be installed with any of the following options:
		   └─ bar 1
	`)
	assertSnapshot(t, "conditional_unsolvable", got)
}

func Test_conditional_unsolvable_without_condition(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, nil},
		// This will not be selected because baz 2 conflicts with the
		// requirement.
		{"foo", 2, deps("baz 2; if bar")},
		{"bar", 1, nil},
		{"baz", 1, nil},
		{"baz", 2, nil},
	})
	reqs := p.Requirements([]string{"foo", "bar", "baz 1"})
	got := solveForSnapshot(t, p, reqs, nil)
	assertInline(t, got, `
		bar=1
		baz=1
		foo=1
	`)
	assertSnapshot(t, "conditional_unsolvable_without_condition", got)
}

func Test_conditional_requirements_version_set(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("baz; if bar 1")},
		{"bar", 1, nil},
		{"bar", 2, nil},
		{"baz", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "bar"})
	got := solveForSnapshot(t, p, reqs, nil)
	assertInline(t, got, `
		bar=2
		foo=1
	`)
	assertSnapshot(t, "conditional_requirements_version_set", got)
}

func Test_conditional_and(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("icon; if bar and baz")},
		{"bar", 1, nil},
		{"bar", 2, nil},
		{"baz", 1, nil},
		{"icon", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "bar", "baz"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bar=2
		baz=1
		foo=1
		icon=1
	`)
}

func Test_conditional_and_mismatch(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("icon; if bar and baz")},
		{"bar", 1, nil},
		{"baz", 1, nil},
		{"icon", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "bar"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bar=1
		foo=1
	`)
}

func Test_conditional_or(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("icon; if bar or baz")},
		{"bar", 1, nil},
		{"baz", 1, nil},
		{"icon", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "bar"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bar=1
		foo=1
		icon=1
	`)
}

func Test_conditional_complex(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("icon; if bar and baz or menu")},
		{"bar", 1, nil},
		{"baz", 1, nil},
		{"icon", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "bar", "baz"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bar=1
		baz=1
		foo=1
		icon=1
	`)
}

func Test_condition_missing_requirement(t *testing.T) {
	p := fromPackages([]pk{{"menu", 1, deps("bla; if intl")}, {"intl", 1, nil}})
	reqs := p.Requirements([]string{"menu"})
	got := solveForSnapshot(t, p, reqs, nil)
	assertInline(t, got, "menu=1")
	assertSnapshot(t, "condition_missing_requirement", got)
}

// Lazy conditional candidate loading: deferred requirements only fetch the
// gated packages once the condition fires, multi-disjunct conditions fire
// per disjunct, deferral survives backtracking, etc.

// The condition never fires, so the gated package bla (and its dependency
// dep) never have their candidates fetched.
func Test_lazy_conditional_skips_unreached_candidates(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("bla; if bar")},
		{"bla", 1, deps("dep")},
		{"dep", 1, nil},
		{"bar", 1, nil},
	})
	// bar is not required, so the condition never fires.
	reqs := p.Requirements([]string{"foo"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), "foo=1")
	fetched := p.RequestedPackageNames()
	if slices.Contains(fetched, "bla") || slices.Contains(fetched, "dep") {
		t.Errorf("expected bla and dep to remain unfetched, got %v", fetched)
	}
}

// When the condition fires the deferred clause is encoded and the gated
// package is fetched.
func Test_lazy_conditional_fires_when_condition_holds(t *testing.T) {
	p := fromPackages([]pk{{"foo", 1, deps("bla; if bar")}, {"bla", 1, nil}, {"bar", 1, nil}})
	reqs := p.Requirements([]string{"foo", "bar"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bar=1
		bla=1
		foo=1
	`)
}

// Firing only one disjunct of "a or b" still produces a correct solution.
func Test_lazy_conditional_multi_disjunct_fires_per_disjunct(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("bla; if a or b")},
		{"bla", 1, nil},
		{"a", 1, nil},
		{"b", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "a"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		a=1
		bla=1
		foo=1
	`)
}

// Several requirements sharing a condition are all encoded when it fires.
func Test_lazy_conditional_shared_condition_fires_all(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("bla; if cond", "ext; if cond")},
		{"bla", 1, nil},
		{"ext", 1, nil},
		{"cond", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "cond"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bla=1
		cond=1
		ext=1
		foo=1
	`)
}

// When the condition fires and the requirement is unsolvable, the conflict
// is reported.
func Test_lazy_conditional_late_arriving_conflict_is_reported(t *testing.T) {
	p := fromPackages([]pk{{"foo", 1, deps("bla 2; if bar")}, {"bar", 1, nil}, {"bla", 1, nil}})
	reqs := p.Requirements([]string{"foo", "bar"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		foo * cannot be installed because there are no viable options:
		└─ foo 1 would require
		   └─ bla >=2, <3, for which no candidates were found.
		The following packages are incompatible
		└─ bar * can be installed with any of the following options:
		   └─ bar 1
	`)
}

func lazyBacktrackingProvider() *bundlebox.Provider {
	return fromPackages([]pk{
		{"foo", 2, deps("bla; if cond", "bla 2")},
		{"foo", 1, deps("bla; if cond")},
		{"bla", 1, nil},
		{"cond", 1, nil},
	})
}

// Backtracking through a deferred conditional requirement must not encode
// the deferred clause twice.
func Test_lazy_conditional_survives_backtracking(t *testing.T) {
	p := lazyBacktrackingProvider()
	reqs := p.Requirements([]string{"foo", "cond"})
	solver, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		bla=1
		cond=1
		foo=1
	`)
	if n := solver.DeferredRequirementsCount(); n != 0 {
		t.Errorf("deferred requirements = %d, want 0: entries must be drained on first fire", n)
	}

	fresh := lazyBacktrackingProvider()
	freshReqs := fresh.Requirements([]string{"foo", "cond"})
	freshSolver, _ := solveOK(t, fresh, cdcl.Problem{Requirements: freshReqs})
	if solver.ClauseCount() != freshSolver.ClauseCount() {
		t.Errorf("clause count %d != fresh re-solve %d: a deferred entry was encoded more than once",
			solver.ClauseCount(), freshSolver.ClauseCount())
	}
}

// A candidate of a deferred requirement can already be false when the
// condition fires; its prefetched clauses must still be added.
func Test_lazy_conditional_prefetches_forbidden_candidate(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, deps("gated; if cond")},
		{"trigger", 1, deps("cond")},
		{"cond", 1, nil},
		{"gated", 2, nil},
		{"dep", 1, nil},
		{"z", 1, nil},
		{"w", 1, nil},
		{"w", 2, nil},
		{"w", 3, nil},
		{"w", 4, nil},
	})
	p.HintDependenciesAvailable = true
	// Installing c forbids gated=1 through a constraint.
	p.AddPackage("c", bundlebox.NewPack(1), nil, deps("gated 2..3"))
	// gated=1 carries a requirement and two constraints: z 2..3 excludes a
	// single candidate (pairwise encoding), w 5..6 excludes four (shared
	// encoding).
	p.AddPackage("gated", bundlebox.NewPack(1), deps("dep"), deps("z 2..3", "w 5..6"))
	reqs := p.Requirements([]string{"c", "a", "trigger"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		c=1
		cond=1
		gated=2
		trigger=1
	`)
}

// Unconditional requirements keep fetching eagerly.
func Test_lazy_conditional_unconditional_unaffected(t *testing.T) {
	p := fromPackages([]pk{{"foo", 1, deps("bla")}, {"bla", 1, nil}})
	reqs := p.Requirements([]string{"foo"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		bla=1
		foo=1
	`)
	if !slices.Contains(p.RequestedPackageNames(), "bla") {
		t.Errorf("unconditional requirements must still fetch their candidates; got %v", p.RequestedPackageNames())
	}
}

// Empty-complement condition: "if bar" is bar *, whose complement is empty.
// The condition holds iff any candidate of bar is selected.
func Test_lazy_conditional_empty_complement_fires(t *testing.T) {
	p := fromPackages([]pk{
		{"foo", 1, deps("bla; if bar")},
		{"bar", 1, nil},
		{"bar", 2, nil},
		{"bla", 1, nil},
	})
	reqs := p.Requirements([]string{"foo", "bar"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bar=2
		bla=1
		foo=1
	`)
}

// Running the same problem ten times produces the same solution.
func Test_lazy_conditional_determinism(t *testing.T) {
	build := func() *bundlebox.Provider {
		return fromPackages([]pk{
			{"foo", 1, deps("bla; if a", "ext; if b")},
			{"foo", 2, deps("bla; if a", "ext; if b")},
			{"bla", 1, nil},
			{"ext", 1, nil},
			{"a", 1, nil},
			{"b", 1, nil},
		})
	}
	run := func() string {
		p := build()
		reqs := p.Requirements([]string{"foo", "a", "b"})
		_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
		return transactionToString(p, solved)
	}
	baseline := run()
	for i := 0; i < 9; i++ {
		if got := run(); got != baseline {
			t.Fatalf("non-deterministic solve: %q vs %q", got, baseline)
		}
	}
}

func multiConjunctProvider() *bundlebox.Provider {
	return fromPackages([]pk{
		{"a", 1, deps("b; if cond_a and cond_b")},
		{"b", 1, nil},
		{"cond_a", 1, nil},
		{"cond_b", 1, nil},
	})
}

// Neither conjunct fires: b is not installed and not fetched.
func Test_test_lazy_conditional_multi_conjunct_requires_all__neither_selected(t *testing.T) {
	p := multiConjunctProvider()
	reqs := p.Requirements([]string{"a"})
	solver, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), "a=1")
	if slices.Contains(p.RequestedPackageNames(), "b") {
		t.Errorf("expected b to remain unfetched, got %v", p.RequestedPackageNames())
	}
	if n := solver.DeferredRequirementsCount(); n != 1 {
		t.Errorf("deferred requirements = %d, want 1", n)
	}
}

// Only the first conjunct fires: b is not installed and not fetched.
func Test_test_lazy_conditional_multi_conjunct_requires_all__only_first_selected(t *testing.T) {
	p := multiConjunctProvider()
	reqs := p.Requirements([]string{"a", "cond_a"})
	solver, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		cond_a=1
	`)
	if slices.Contains(p.RequestedPackageNames(), "b") {
		t.Errorf("expected b to remain unfetched, got %v", p.RequestedPackageNames())
	}
	if n := solver.DeferredRequirementsCount(); n != 1 {
		t.Errorf("deferred requirements = %d, want 1", n)
	}
}

// Both conjuncts fire: b is installed.
func Test_test_lazy_conditional_multi_conjunct_requires_all__both_selected(t *testing.T) {
	p := multiConjunctProvider()
	reqs := p.Requirements([]string{"a", "cond_a", "cond_b"})
	solver, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=1
		cond_a=1
		cond_b=1
	`)
	if n := solver.DeferredRequirementsCount(); n != 0 {
		t.Errorf("deferred requirements = %d, want 0", n)
	}
}

// Upstream runs this on an async runtime to force interleaved provider
// futures; Go providers are synchronous, so it checks determinism of the
// resolution and of the fetched package set across repeated solves.
func Test_lazy_conditional_determinism_async(t *testing.T) {
	build := func() *bundlebox.Provider {
		return fromPackages([]pk{
			{"foo", 1, deps("bla; if a", "ext; if b", "tail; if c")},
			{"foo", 2, deps("bla; if a", "ext; if b", "tail; if c")},
			{"bla", 1, deps("inner; if a")},
			{"ext", 1, nil},
			{"tail", 1, nil},
			{"inner", 1, nil},
			{"a", 1, nil},
			{"b", 1, nil},
			{"c", 1, nil},
		})
	}
	run := func() (string, []string) {
		p := build()
		reqs := p.Requirements([]string{"foo", "a", "b", "c"})
		_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
		fetched := p.RequestedPackageNames()
		slices.Sort(fetched)
		return transactionToString(p, solved), fetched
	}
	baseRes, baseFetched := run()
	for i := 0; i < 9; i++ {
		res, fetched := run()
		if res != baseRes {
			t.Fatalf("non-deterministic resolution at iteration %d", i)
		}
		if !slices.Equal(fetched, baseFetched) {
			t.Fatalf("non-deterministic fetched packages at iteration %d", i)
		}
	}
}

// The lazy path fires "if cond 2..3" only when a matching candidate is
// selected; excluding the complement candidate cond=1 does not fire it.
func Test_lazy_conditional_externally_excluded_complement_does_not_fire(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, deps("b; if cond 2..3")},
		{"b", 1, nil},
		{"cond", 1, nil},
		{"cond", 2, nil},
	})
	p.Exclude("cond", 1, "it is externally excluded")
	reqs := p.Requirements([]string{"a"})
	solver, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), "a=1")
	if n := solver.DeferredRequirementsCount(); n != 1 {
		t.Errorf("deferred requirements = %d, want 1", n)
	}
}

// Constraint edge cases: "if A is installed, B must not be installed" in
// both directions, including backtracking.

func Test_constrains_forward_basic(t *testing.T) {
	p := bundlebox.New()
	// a requires b and constrains b to >= 3 (forbids b=1, b=2)
	p.AddPackage("a", bundlebox.NewPack(1), deps("b"), deps("b 3..100"))
	for v := uint32(1); v <= 4; v++ {
		p.AddPackage("b", bundlebox.NewPack(v), nil, nil)
	}
	reqs := p.Requirements([]string{"a"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=4
	`)
}

func Test_constrains_forward_all_forbidden(t *testing.T) {
	p := bundlebox.New()
	// a requires b but its constraint [100,200) excludes every version.
	p.AddPackage("a", bundlebox.NewPack(1), deps("b"), deps("b 100..200"))
	p.AddPackage("b", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("b", bundlebox.NewPack(2), nil, nil)
	assertSnapshot(t, "constrains_forward_all_forbidden", solveUnsat(t, p, []string{"a"}))
}

func Test_constrains_reverse_direction(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("x", bundlebox.NewPack(1), deps("b 2..3"), nil)
	p.AddPackage("b", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("b", bundlebox.NewPack(2), nil, nil)
	p.AddPackage("b", bundlebox.NewPack(3), nil, nil)
	p.AddPackage("a", bundlebox.NewPack(2), nil, nil)
	// a=3 constrains b to [3,100), so b=1 and b=2 are forbidden
	p.AddPackage("a", bundlebox.NewPack(3), nil, deps("b 3..100"))
	reqs := p.Requirements([]string{"x", "a"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=2
		b=2
		x=1
	`)
}

func Test_constrains_reverse_unsat(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("x", bundlebox.NewPack(1), deps("b 1..2"), nil)
	p.AddPackage("b", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("b", bundlebox.NewPack(2), nil, nil)
	p.AddPackage("a", bundlebox.NewPack(1), nil, deps("b 2..100"))
	assertSnapshot(t, "constrains_reverse_unsat", solveUnsat(t, p, []string{"x", "a"}))
}

func Test_constrains_no_version_selected(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("a", bundlebox.NewPack(1), nil, deps("b 100..200"))
	p.AddPackage("b", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("b", bundlebox.NewPack(2), nil, nil)
	reqs := p.Requirements([]string{"a"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), "a=1")
}

func Test_constrains_matching_version_ok(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("x", bundlebox.NewPack(1), deps("b"), nil)
	for v := uint32(1); v <= 3; v++ {
		p.AddPackage("b", bundlebox.NewPack(v), nil, nil)
	}
	p.AddPackage("a", bundlebox.NewPack(1), nil, deps("b 0..2"))
	reqs := p.Requirements([]string{"x", "a"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=1
		x=1
	`)
}

func Test_constrains_backtracking(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("a", bundlebox.NewPack(1), deps("b"), nil)
	for v := uint32(1); v <= 3; v++ {
		p.AddPackage("b", bundlebox.NewPack(v), nil, nil)
	}
	p.AddPackage("c", bundlebox.NewPack(1), nil, deps("b 0..2"))
	reqs := p.Requirements([]string{"a", "c"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=1
		c=1
	`)
}

func Test_constrains_diamond(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("a", bundlebox.NewPack(1), deps("c"), deps("c 3..100"))
	p.AddPackage("b", bundlebox.NewPack(1), deps("c"), deps("c 0..5"))
	for v := uint32(1); v <= 5; v++ {
		p.AddPackage("c", bundlebox.NewPack(v), nil, nil)
	}
	reqs := p.Requirements([]string{"a", "b"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=1
		c=4
	`)
}

func Test_constrains_many_versions(t *testing.T) {
	p := bundlebox.New()
	for v := uint32(1); v <= 50; v++ {
		p.AddPackage("big", bundlebox.NewPack(v), nil, nil)
	}
	p.AddPackage("a", bundlebox.NewPack(1), deps("big"), deps("big 46..100"))
	reqs := p.Requirements([]string{"a"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		big=50
	`)
}

func Test_constrains_transitive(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("b", bundlebox.NewPack(1), deps("c 1..2"), nil)
	p.AddPackage("b", bundlebox.NewPack(2), deps("c 2..3"), nil)
	p.AddPackage("c", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("c", bundlebox.NewPack(2), nil, nil)
	p.AddPackage("x", bundlebox.NewPack(1), deps("b"), nil)
	p.AddPackage("a", bundlebox.NewPack(1), nil, deps("b 2..100"))
	reqs := p.Requirements([]string{"x", "a"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=2
		c=2
		x=1
	`)
}

func constrainsClauseCount(t *testing.T, pkgVersions, parents int) int {
	p := bundlebox.New()
	for v := 1; v <= pkgVersions; v++ {
		p.AddPackage("pkg", bundlebox.NewPack(uint32(v)), nil, nil)
	}
	var names []string
	for i := 0; i < parents; i++ {
		name := fmt.Sprintf("p%d", i)
		names = append(names, name)
		p.AddPackage(name, bundlebox.NewPack(1), nil, deps("pkg 11..100"))
	}
	reqs := p.Requirements(names)
	solver, _ := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	return solver.ClauseCount()
}

// The shared encoding emits the excluded candidates of a constraint once:
// N parents over M excluded candidates produce M + N clauses, not N * M.
func Test_constrains_shared_encoding_clause_count(t *testing.T) {
	single := constrainsClauseCount(t, 10, 1)
	many := constrainsClauseCount(t, 10, 5)
	// Each additional parent adds one requires clause and one parent
	// clause.
	if many-single != 4*2 {
		t.Errorf("clause delta = %d, want %d", many-single, 4*2)
	}
}

// Constraints that exclude only a few candidates keep the pairwise encoding.
func Test_constrains_pairwise_encoding_clause_count(t *testing.T) {
	single := constrainsClauseCount(t, 2, 1)
	many := constrainsClauseCount(t, 2, 5)
	// Each additional parent adds one requires clause and one pairwise
	// clause per excluded candidate.
	if many-single != 4*3 {
		t.Errorf("clause delta = %d, want %d", many-single, 4*3)
	}
}

func sharedEncodingProvider() *bundlebox.Provider {
	return fromPackages([]pk{
		{"a", 10, deps("b 50..100")},
		{"b", 55, nil},
		{"b", 54, nil},
		{"b", 53, nil},
		{"b", 52, nil},
		{"b", 51, nil},
		{"b", 50, nil},
		{"b", 42, nil},
	})
}

// Like Test_unsat_constrains, through the shared auxiliary-variable
// encoding; the explanation must look as if the constraint were pairwise.
func Test_unsat_constrains_shared_encoding(t *testing.T) {
	p := sharedEncodingProvider()
	p.AddPackage("c", bundlebox.NewPack(10), nil, deps("b 0..50"))
	assertSnapshot(t, "unsat_constrains_shared_encoding", solveUnsat(t, p, []string{"a", "c"}))
}

// Reverse direction through the shared encoding.
func Test_unsat_constrains_shared_encoding_reverse(t *testing.T) {
	p := fromPackages([]pk{
		{"x", 1, deps("b 1..2")},
		{"b", 1, nil},
		{"b", 2, nil},
		{"b", 3, nil},
		{"b", 4, nil},
	})
	p.AddPackage("a", bundlebox.NewPack(1), nil, deps("b 5..100"))
	assertSnapshot(t, "unsat_constrains_shared_encoding_reverse", solveUnsat(t, p, []string{"x", "a"}))
}

// Multiple parents sharing a constraint through the shared encoding.
func Test_unsat_constrains_shared_encoding_multiple_parents(t *testing.T) {
	p := sharedEncodingProvider()
	p.AddPackage("c", bundlebox.NewPack(10), nil, deps("b 0..50"))
	p.AddPackage("d", bundlebox.NewPack(10), nil, deps("b 0..50"))
	assertSnapshot(t, "unsat_constrains_shared_encoding_multiple_parents", solveUnsat(t, p, []string{"a", "c", "d"}))
}

func Test_constrains_multiple_parents(t *testing.T) {
	p := bundlebox.New()
	for v := uint32(1); v <= 8; v++ {
		p.AddPackage("pkg", bundlebox.NewPack(v), nil, nil)
	}
	p.AddPackage("x", bundlebox.NewPack(1), deps("pkg"), nil)
	p.AddPackage("a", bundlebox.NewPack(1), nil, deps("pkg 3..100"))
	p.AddPackage("b", bundlebox.NewPack(1), nil, deps("pkg 0..8"))
	reqs := p.Requirements([]string{"x", "a", "b"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=1
		pkg=7
		x=1
	`)
}

// Decide-queue wake-up scenarios. The debug checks verify the queue's
// bookkeeping invariants on every decision.

func Test_decide_queue_satisfaction_break(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("x", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("x", bundlebox.NewPack(2), nil, nil)
	p.AddPackage("a", bundlebox.NewPack(1), deps("x 1..2"), nil)
	p.AddPackage("a", bundlebox.NewPack(2), deps("x 2..3"), nil)
	p.AddPackage("b", bundlebox.NewPack(1), deps("x 1..2"), nil)
	reqs := p.Requirements([]string{"a", "b"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		a=1
		b=1
		x=1
	`)
}

func Test_decide_queue_backjump_past_parent(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("leaf", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("leaf", bundlebox.NewPack(2), nil, nil)
	p.AddPackage("mid", bundlebox.NewPack(1), deps("leaf 1..2"), nil)
	p.AddPackage("mid", bundlebox.NewPack(2), deps("leaf 2..3"), nil)
	p.AddPackage("top", bundlebox.NewPack(1), deps("mid"), nil)
	reqs := p.Requirements([]string{"top", "leaf 1..2"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		leaf=1
		mid=1
		top=1
	`)
}

func Test_decide_queue_condition_toggles(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("bar", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("bar", bundlebox.NewPack(2), nil, nil)
	p.AddPackage("baz", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("foo", bundlebox.NewPack(1), deps("baz; if bar 2..3"), nil)
	p.AddPackage("qux", bundlebox.NewPack(1), deps("bar 1..2"), nil)
	reqs := p.Requirements([]string{"foo", "bar", "qux"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		bar=1
		foo=1
		qux=1
	`)
}

func Test_decide_queue_reset_on_late_conflict(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("a", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("b", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("b", bundlebox.NewPack(2), nil, deps("a 2..3"))
	reqs := p.Requirements([]string{"a", "b"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		a=1
		b=1
	`)
}

func Test_decide_queue_union_duplicate_name(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("x", bundlebox.NewPack(1), nil, nil)
	p.AddPackage("x", bundlebox.NewPack(3), nil, nil)
	p.AddPackage("a", bundlebox.NewPack(1), deps("x 0..2 | x 3..4"), nil)
	p.AddPackage("b", bundlebox.NewPack(1), deps("x 0..2"), nil)
	reqs := p.Requirements([]string{"a", "b"})
	assertInline(t, solveForSnapshot(t, p, reqs, nil), `
		a=1
		b=1
		x=1
	`)
}

// gateFuzzRng is a tiny xorshift RNG used to generate the shared-requires
// gate fuzz graphs.
type gateFuzzRng uint64

func newGateFuzzRng(seed uint64) gateFuzzRng { return gateFuzzRng(seed*0x9E3779B97F4A7C15 | 1) }

func (r *gateFuzzRng) next() uint64 {
	x := uint64(*r)
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	*r = gateFuzzRng(x)
	return x
}

type gateFuzzPkg struct {
	name    string
	version uint32
	deps    []string
}

// genGateFuzzGraph generates a random dependency graph plus root
// requirements. Multi-version packages produce requirements with enough
// candidates to be encoded behind a shared gate.
func genGateFuzzGraph(seed uint64) ([]gateFuzzPkg, []string) {
	r := newGateFuzzRng(seed)
	n := 5 + int(r.next()%6)
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("p%d", i)
	}
	var pkgs []gateFuzzPkg
	for i, name := range names {
		versions := 1 + uint32(r.next()%5)
		for v := uint32(1); v <= versions; v++ {
			numDeps := int(r.next() % 4)
			var ds []string
			for k := 0; k < numDeps; k++ {
				j := int(r.next() % uint64(n))
				if j == i {
					continue
				}
				if r.next()%3 == 0 {
					lo := 1 + uint32(r.next()%3)
					hi := lo + 1 + uint32(r.next()%3)
					ds = append(ds, fmt.Sprintf("%s %d..%d", names[j], lo, hi))
				} else {
					ds = append(ds, names[j])
				}
			}
			pkgs = append(pkgs, gateFuzzPkg{name, v, ds})
		}
	}
	numRoots := 1 + int(r.next()%3)
	roots := make([]string, numRoots)
	for i := range roots {
		roots[i] = names[int(r.next()%uint64(n))]
	}
	return pkgs, roots
}

// validateComplete checks that sol satisfies every root and every
// dependency of an installed package.
func validateComplete(pkgs []gateFuzzPkg, roots []string, sol map[string]uint32) error {
	satisfied := func(spec string) bool {
		fields := strings.Fields(spec)
		cv, ok := sol[fields[0]]
		if !ok {
			return false
		}
		if len(fields) == 1 {
			return true
		}
		bounds := strings.Split(fields[1], "..")
		lo, _ := strconv.Atoi(bounds[0])
		hi, _ := strconv.Atoi(bounds[1])
		return int(cv) >= lo && int(cv) < hi
	}
	for _, root := range roots {
		if !satisfied(root) {
			return fmt.Errorf("root %q is unsatisfied", root)
		}
	}
	for name, version := range sol {
		for _, p := range pkgs {
			if p.name != name || p.version != version {
				continue
			}
			for _, d := range p.deps {
				if !satisfied(d) {
					return fmt.Errorf("%s==%d requires %q which is absent", name, version, d)
				}
			}
		}
	}
	return nil
}

// Regression for the shared-requires gate: a backtrack could strand the
// gate (unassign it while a requirer stayed installed), silently dropping
// the gated requirement. Upstream reproduces it with seeded async orderings
// of graph 542; the Go encoder is synchronous, so this test checks graph 542
// and a range of other seeds for complete solutions.
func Test_shared_requires_gate_survives_backtrack(t *testing.T) {
	for _, seed := range append([]uint64{542}, seq(1, 300)...) {
		pkgs, roots := genGateFuzzGraph(seed)
		var ps []pk
		for _, p := range pkgs {
			ps = append(ps, pk{p.name, p.version, p.deps})
		}
		provider := fromPackages(ps)
		reqs := provider.Requirements(roots)
		solver := newSolver(provider)
		solved, err := solver.Solve(t.Context(), cdcl.Problem{Requirements: reqs})
		if err != nil {
			continue
		}
		sol := map[string]uint32{}
		for _, s := range solved {
			rec := provider.Pool.ResolveSolvable(s)
			sol[provider.DisplayName(rec.Name)] = rec.Record.Version
		}
		if err := validateComplete(pkgs, roots, sol); err != nil {
			t.Errorf("seed %d produced an incomplete solution: %v", seed, err)
		}
	}
}

func seq(from, to uint64) []uint64 {
	var out []uint64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// allow_multiple_versions

// Non-overlapping version requirements are unsolvable without
// AllowMultiple.
func Test_allow_multiple_versions__test_non_overlapping_requirements_unsat_without_allow_multiple(t *testing.T) {
	p := bundlebox.New()
	for v := uint32(1); v <= 3; v++ {
		p.AddPackage("kernel", bundlebox.NewPack(v), nil, nil)
	}
	p.AddPackage("kmod-a", bundlebox.NewPack(1), deps("kernel 1..2"), nil)
	p.AddPackage("kmod-b", bundlebox.NewPack(1), deps("kernel 3..4"), nil)
	assertSnapshot(t, "allow_multiple_versions__non_overlapping_requirements_unsat_without_allow_multiple",
		solveUnsat(t, p, []string{"kmod-a", "kmod-b"}))
}

// With AllowMultiple, non-overlapping requirements install several
// versions.
func Test_allow_multiple_versions__test_non_overlapping_requirements(t *testing.T) {
	p := bundlebox.New()
	for v := uint32(1); v <= 3; v++ {
		p.AddPackage("kernel", bundlebox.NewPack(v), nil, nil)
	}
	p.SetAllowMultiple("kernel")
	p.AddPackage("kmod-a", bundlebox.NewPack(1), deps("kernel 1..2"), nil)
	p.AddPackage("kmod-b", bundlebox.NewPack(1), deps("kernel 3..4"), nil)
	reqs := p.Requirements([]string{"kmod-a", "kmod-b"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		kernel=1
		kernel=3
		kmod-a=1
		kmod-b=1
	`)
}

// With overlapping requirements each one independently selects its best
// candidate (not ideal, but resolvo's documented behaviour).
func Test_allow_multiple_versions__test_overlapping_requirements(t *testing.T) {
	p := bundlebox.New()
	for v := uint32(1); v <= 3; v++ {
		p.AddPackage("kernel", bundlebox.NewPack(v), nil, nil)
	}
	p.SetAllowMultiple("kernel")
	p.AddPackage("kmod-a", bundlebox.NewPack(1), deps("kernel 1..3"), nil)
	p.AddPackage("kmod-b", bundlebox.NewPack(1), deps("kernel 2..4"), nil)
	reqs := p.Requirements([]string{"kmod-a", "kmod-b"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		kernel=2
		kernel=3
		kmod-a=1
		kmod-b=1
	`)
}

// AllowMultiple works when the package is reached in a later encoder pass.
func Test_allow_multiple_versions__test_allow_multiple_transitive(t *testing.T) {
	p := bundlebox.New()
	for v := uint32(1); v <= 3; v++ {
		p.AddPackage("kernel", bundlebox.NewPack(v), nil, nil)
	}
	p.SetAllowMultiple("kernel")
	p.AddPackage("kmod", bundlebox.NewPack(1), deps("kernel 1..2"), nil)
	p.AddPackage("kmod", bundlebox.NewPack(2), deps("kernel 2..3"), nil)
	p.AddPackage("kmod", bundlebox.NewPack(3), deps("kernel 3..4"), nil)
	p.SetAllowMultiple("kmod")
	p.AddPackage("app-old", bundlebox.NewPack(1), deps("kmod 1..2"), nil)
	p.AddPackage("app-new", bundlebox.NewPack(1), deps("kmod 3..4"), nil)
	reqs := p.Requirements([]string{"app-old", "app-new"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		app-new=1
		app-old=1
		kernel=1
		kernel=3
		kmod=1
		kmod=3
	`)
}

func Test_allow_multiple_versions__test_soft_requirements_with_allow_multiple(t *testing.T) {
	skipSoft(t)
}

// forbid_registration_regression

func solveSpecsToString(t *testing.T, packages []pk, specs []string) string {
	p := fromPackages(packages)
	reqs := p.Requirements(specs)
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	return transactionToString(p, solved)
}

// A union over two non-overlapping version sets of one package installs
// exactly one version.
func Test_forbid_registration_regression__test_union_same_package_single_version(t *testing.T) {
	got := solveSpecsToString(t, []pk{{"x", 1, nil}, {"x", 2, nil}, {"x", 3, nil}}, []string{"x 1..2 | x 3..4"})
	if strings.Count(got, "x=") != 1 {
		t.Errorf("got %q, want exactly one x", got)
	}
	assertInline(t, got, "x=1")
}

// Overlapping same-name version sets share candidate variables; each is
// registered once.
func Test_forbid_registration_regression__test_union_overlapping_same_package_dedup(t *testing.T) {
	got := solveSpecsToString(t, []pk{{"x", 1, nil}, {"x", 2, nil}, {"x", 3, nil}}, []string{"x 1..3 | x 2..4"})
	if strings.Count(got, "x=") != 1 {
		t.Errorf("got %q, want exactly one x", got)
	}
	assertInline(t, got, "x=2")
}

// An empty candidate list in the first version set of a union is skipped.
func Test_forbid_registration_regression__test_union_empty_first_candidate_list(t *testing.T) {
	assertInline(t, solveSpecsToString(t, []pk{{"a", 1, nil}, {"b", 1, nil}}, []string{"a 5..6 | b 1..2"}), "b=1")
}

// An empty candidate list in a later version set of a union is skipped.
func Test_forbid_registration_regression__test_union_empty_second_candidate_list(t *testing.T) {
	assertInline(t, solveSpecsToString(t, []pk{{"x", 1, nil}, {"x", 2, nil}}, []string{"x 1..2 | x 5..6"}), "x=1")
}

// A lock combined with a union resolves to the locked version.
func Test_forbid_registration_regression__test_locked_through_union(t *testing.T) {
	p := fromPackages([]pk{{"x", 1, nil}, {"x", 2, nil}, {"x", 3, nil}})
	p.SetLocked("x", 1)
	reqs := p.Requirements([]string{"x 2..3 | x 1..2"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), "x=1")
}

// An exclusion inside a union branch selects the other branch.
func Test_forbid_registration_regression__test_excluded_through_union(t *testing.T) {
	p := fromPackages([]pk{{"x", 1, nil}, {"x", 2, nil}, {"x", 3, nil}})
	p.Exclude("x", 2, "it is externally excluded")
	reqs := p.Requirements([]string{"x 2..3 | x 1..2"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), "x=1")
}

// Skipping forbid registration for an AllowMultiple union member must not
// leak into a normal sibling member.
func Test_forbid_registration_regression__test_allow_multiple_does_not_disable_forbid_for_sibling_union_member(t *testing.T) {
	p := fromPackages([]pk{
		{"multi", 0, nil},
		{"single", 1, nil},
		{"single", 2, nil},
		{"app-a", 1, deps("multi 0..1 | single 1..2")},
		{"app-b", 1, deps("single 2..3")},
	})
	p.SetAllowMultiple("multi")
	reqs := p.Requirements([]string{"app-a", "app-b"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		app-a=1
		app-b=1
		multi=0
		single=2
	`)
}

// AllowMultiple combined with an empty candidate list and a union, reached
// transitively.
func Test_forbid_registration_regression__test_allow_multiple_union_with_empty_branch_transitive(t *testing.T) {
	p := fromPackages([]pk{
		{"multi", 1, nil},
		{"multi", 2, nil},
		{"multi", 3, nil},
		{"helper", 1, nil},
		{"app-a", 1, deps("multi 1..2 | helper 2..3")},
		{"app-b", 1, deps("multi 3..4")},
	})
	p.SetAllowMultiple("multi")
	reqs := p.Requirements([]string{"app-a", "app-b"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		app-a=1
		app-b=1
		multi=1
		multi=3
	`)
}
