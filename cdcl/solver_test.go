// Cases from resolvo tests/solver/main.rs (BSD-3-Clause, see NOTICE). Each
// TestX_y ports the upstream #[test] fn x_y with the same name; tests in an
// upstream module are named Test_<module>__<name>.
//
// Two upstream snapshot files have no test upstream and are not ported:
// solver__condition_is_disabled.snap and
// solver__merge_installable_non_continuous_range.snap (the latter also
// needs a range-merging display the current BundleBox provider lacks).
//
// cdcl does not support soft requirements (see the package documentation),
// so the four upstream tests that exercise them are skipped:
// test_solve_with_additional,
// test_solve_with_soft_requirement_forbid_clause_conflict,
// test_solve_with_additional_with_constrains and
// allow_multiple_versions::test_soft_requirements_with_allow_multiple.

package cdcl_test

import (
	"context"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

// Test whether we can select a version, the most basic operation.
func Test_unit_propagation_1(t *testing.T) {
	p := fromPackages([]pk{{"asdf", 1, nil}})
	reqs := p.Requirements([]string{"asdf"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if len(solved) != 1 {
		t.Fatalf("got %d solvables, want 1", len(solved))
	}
	s := p.Pool.ResolveSolvable(solved[0])
	if p.Pool.ResolvePackageName(s.Name) != "asdf" || s.Record.Version != 1 {
		t.Fatalf("got %s", p.DisplaySolvable(solved[0]))
	}
}

// Test if we can also select a nested version.
func Test_unit_propagation_nested(t *testing.T) {
	p := fromPackages([]pk{
		{"asdf", 1, deps("efgh")},
		{"efgh", 4, nil},
		{"dummy", 6, nil},
	})
	reqs := p.Requirements([]string{"asdf"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if len(solved) != 2 {
		t.Fatalf("got %d solvables, want 2", len(solved))
	}
	if got := p.DisplaySolvable(solved[0]); got != "asdf=1" {
		t.Errorf("solved[0] = %s, want asdf=1", got)
	}
	if got := p.DisplaySolvable(solved[1]); got != "efgh=4" {
		t.Errorf("solved[1] = %s, want efgh=4", got)
	}
}

// Test if we can resolve multiple versions at once.
func Test_resolve_multiple(t *testing.T) {
	p := fromPackages([]pk{
		{"asdf", 1, nil},
		{"asdf", 2, nil},
		{"efgh", 4, nil},
		{"efgh", 5, nil},
	})
	reqs := p.Requirements([]string{"asdf", "efgh"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		asdf=2
		efgh=5
	`)
	if len(solved) != 2 {
		t.Fatalf("got %d solvables, want 2", len(solved))
	}
}

func Test_resolve_with_concurrent_metadata_fetching(t *testing.T) {
	p := fromPackages([]pk{
		{"parent", 4, deps("child1", "child2")},
		{"child1", 3, nil},
		{"child2", 2, nil},
	})
	assertSnapshot(t, "resolve_with_concurrent_metadata_fetching", solveSnapshot(t, p, []string{"parent"}))
	// resolvo fetches child1 and child2 concurrently (2 requests in
	// flight). Go providers are synchronous, so there is never more than
	// one request in flight.
	if p.ConcurrentRequestsMax != 1 {
		t.Errorf("max concurrent requests = %d, want 1", p.ConcurrentRequestsMax)
	}
}

// In case of a conflict the version should not be selected with the
// conflict.
func Test_resolve_with_conflict(t *testing.T) {
	p := fromPackages([]pk{
		{"asdf", 4, deps("conflicting 1")},
		{"asdf", 3, deps("conflicting 0")},
		{"efgh", 7, deps("conflicting 0")},
		{"efgh", 6, deps("conflicting 0")},
		{"conflicting", 1, nil},
		{"conflicting", 0, nil},
	})
	assertSnapshot(t, "resolve_with_conflict", solveSnapshot(t, p, []string{"asdf", "efgh"}))
}

// The non-existing package should not be selected.
func Test_resolve_with_nonexisting(t *testing.T) {
	p := fromPackages([]pk{
		{"asdf", 4, deps("b")},
		{"asdf", 3, nil},
		{"b", 1, deps("idontexist")},
	})
	reqs := p.Requirements([]string{"asdf"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if len(solved) != 1 || p.DisplaySolvable(solved[0]) != "asdf=3" {
		t.Fatalf("got %q, want asdf=3", transactionToString(p, solved))
	}
}

func Test_resolve_with_nested_deps(t *testing.T) {
	p := fromPackages([]pk{
		{"apache-airflow", 3, deps("opentelemetry-api 2..4", "opentelemetry-exporter-otlp")},
		{"apache-airflow", 2, deps("opentelemetry-api 2..4", "opentelemetry-exporter-otlp")},
		{"apache-airflow", 1, nil},
		{"opentelemetry-api", 3, deps("opentelemetry-sdk")},
		{"opentelemetry-api", 2, nil},
		{"opentelemetry-api", 1, nil},
		{"opentelemetry-exporter-otlp", 1, deps("opentelemetry-grpc")},
		{"opentelemetry-grpc", 1, deps("opentelemetry-api 1")},
	})
	reqs := p.Requirements([]string{"apache-airflow"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if len(solved) != 1 || p.DisplaySolvable(solved[0]) != "apache-airflow=1" {
		t.Fatalf("got %q, want apache-airflow=1", transactionToString(p, solved))
	}
}

func Test_resolve_with_unknown_deps(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("opentelemetry-api", bundlebox.NewPack(3).WithUnknownDeps(), nil, nil)
	p.AddPackage("opentelemetry-api", bundlebox.NewPack(2), nil, nil)
	reqs := p.Requirements([]string{"opentelemetry-api"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if len(solved) != 1 || p.DisplaySolvable(solved[0]) != "opentelemetry-api=2" {
		t.Fatalf("got %q, want opentelemetry-api=2", transactionToString(p, solved))
	}
}

func Test_resolve_and_cancel(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("opentelemetry-api", bundlebox.NewPack(3).WithUnknownDeps(), nil, nil)
	p.AddPackage("opentelemetry-api", bundlebox.NewPack(2).WithCancelDuringGetDependencies(), nil, nil)
	assertSnapshot(t, "resolve_and_cancel", solveUnsat(t, p, []string{"opentelemetry-api"}))
}

// Locking a specific package version, in this case a lower version namely
// 3, should result in the higher package not being considered.
func Test_resolve_locked_top_level(t *testing.T) {
	p := fromPackages([]pk{{"asdf", 4, nil}, {"asdf", 3, nil}})
	p.SetLocked("asdf", 3)
	reqs := p.Requirements([]string{"asdf"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if len(solved) != 1 || p.Pool.ResolveSolvable(solved[0]).Record.Version != 3 {
		t.Fatalf("got %q, want asdf=3", transactionToString(p, solved))
	}
}

// Should ignore the lock when it is not a top level package and a newer
// version exists without it.
func Test_resolve_ignored_locked_top_level(t *testing.T) {
	p := fromPackages([]pk{
		{"asdf", 4, nil},
		{"asdf", 3, deps("fgh")},
		{"fgh", 1, nil},
	})
	p.SetLocked("fgh", 1)
	reqs := p.Requirements([]string{"asdf"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	if len(solved) != 1 || p.DisplaySolvable(solved[0]) != "asdf=4" {
		t.Fatalf("got %q, want asdf=4", transactionToString(p, solved))
	}
}

// Test checks if favoring without a conflict results in a package upgrade.
func Test_resolve_favor_without_conflict(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, nil},
		{"a", 2, nil},
		{"b", 1, nil},
		{"b", 2, nil},
	})
	p.SetFavored("a", 1)
	p.SetFavored("b", 1)
	// Already installed: A=1; B=1
	assertInline(t, solveSnapshot(t, p, []string{"a", "b 2"}), `
		a=1
		b=2
	`)
}

func Test_resolve_favor_with_conflict(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, deps("c 1")},
		{"a", 2, nil},
		{"b", 1, deps("c 1")},
		{"b", 2, deps("c 2")},
		{"c", 1, nil},
		{"c", 2, nil},
	})
	p.SetFavored("a", 1)
	p.SetFavored("b", 1)
	p.SetFavored("c", 1)
	assertInline(t, solveSnapshot(t, p, []string{"a", "b 2"}), `
		a=2
		b=2
		c=2
	`)
}

func Test_resolve_cyclic(t *testing.T) {
	p := fromPackages([]pk{{"a", 2, deps("b 0..10")}, {"b", 5, deps("a 2..4")}})
	reqs := p.Requirements([]string{"a 0..100"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=2
		b=5
	`)
}

func Test_resolve_union_requirements(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, nil},
		{"b", 1, nil},
		{"c", 1, deps("a")},
		{"d", 1, deps("b")},
		{"e", 1, deps("a | b")},
	})
	// Make d conflict with a=1
	p.AddPackage("f", bundlebox.NewPack(1), deps("b"), deps("a 2"))
	assertInline(t, solveSnapshot(t, p, []string{"c | d", "e", "f"}), `
		b=1
		d=1
		e=1
		f=1
	`)
}

func Test_unsat_locked_and_excluded(t *testing.T) {
	p := fromPackages([]pk{
		{"asdf", 1, deps("c 2")},
		{"c", 2, nil},
		{"c", 1, nil},
	})
	p.SetLocked("c", 1)
	assertSnapshot(t, "unsat_locked_and_excluded", solveSnapshot(t, p, []string{"asdf"}))
}

// A locked solvable that is itself excluded rules out the whole package:
// the lock forbids every other candidate and the exclusion the locked one.
// The conflict must surface the exclusion reason.
func Test_unsat_locked_solvable_is_excluded(t *testing.T) {
	p := fromPackages([]pk{{"c", 1, nil}, {"c", 2, nil}, {"c", 3, nil}, {"c", 4, nil}})
	p.SetLocked("c", 1)
	p.Exclude("c", 1, "it is not available locally")
	assertInline(t, solveSnapshot(t, p, []string{"c"}), `
		The following packages are incompatible
		└─ c * can be installed with any of the following options:
		   └─ c 2 | 3 | 4
		└─ c 1 is locked, but it is excluded because it is not available locally
	`)
}

func Test_unsat_no_candidates_for_child_1(t *testing.T) {
	p := fromPackages([]pk{{"asdf", 1, deps("c 2")}, {"c", 1, nil}})
	assertSnapshot(t, "unsat_no_candidates_for_child_1", solveUnsat(t, p, []string{"asdf"}))
}

func Test_unsat_no_candidates_for_child_2(t *testing.T) {
	p := fromPackages([]pk{{"a", 41, deps("B 0..20")}})
	assertSnapshot(t, "unsat_no_candidates_for_child_2", solveUnsat(t, p, []string{"a 0..1000"}))
}

// Versions requiring different versions of a missing dependency are each
// reported (conda/rattler#2476).
func Test_unsat_no_candidates_distinct_requirements(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, deps("b 41..42")},
		{"a", 2, deps("b 41..42")},
		{"a", 3, deps("b 42..43")},
		{"a", 4, deps("b 43..44")},
	})
	assertSnapshot(t, "unsat_no_candidates_distinct_requirements", solveUnsat(t, p, []string{"a"}))
}

func Test_unsat_missing_top_level_dep_1(t *testing.T) {
	p := fromPackages([]pk{{"asdf", 1, nil}})
	assertSnapshot(t, "unsat_missing_top_level_dep_1", solveUnsat(t, p, []string{"fghj"}))
}

func Test_unsat_missing_top_level_dep_2(t *testing.T) {
	p := fromPackages([]pk{{"a", 41, deps("b 15")}, {"b", 15, nil}})
	assertSnapshot(t, "unsat_missing_top_level_dep_2", solveUnsat(t, p, []string{"a 41", "b 14"}))
}

func Test_unsat_after_backtracking(t *testing.T) {
	p := fromPackages([]pk{
		{"b", 7, deps("d 1")},
		{"b", 6, deps("d 1")},
		{"c", 1, deps("d 2")},
		{"c", 2, deps("d 2")},
		{"d", 2, nil},
		{"d", 1, nil},
		{"e", 1, nil},
		{"e", 2, nil},
	})
	assertSnapshot(t, "unsat_after_backtracking", solveUnsat(t, p, []string{"b", "c", "e"}))
}

func Test_unsat_incompatible_root_requirements(t *testing.T) {
	p := fromPackages([]pk{{"a", 2, nil}, {"a", 5, nil}})
	assertSnapshot(t, "unsat_incompatible_root_requirements", solveUnsat(t, p, []string{"a 0..4", "a 5..10"}))
}

func Test_unsat_bluesky_conflict(t *testing.T) {
	p := fromPackages([]pk{
		{"suitcase-utils", 54, nil},
		{"suitcase-utils", 53, nil},
		{"bluesky-widgets", 42, deps("bluesky-live 0..10", "numpy 0..10", "python 0..10", "suitcase-utils 0..54")},
		{"bluesky-live", 1, nil},
		{"numpy", 1, nil},
		{"python", 1, nil},
	})
	assertSnapshot(t, "unsat_bluesky_conflict", solveUnsat(t, p, []string{"bluesky-widgets 0..100", "suitcase-utils 54..100"}))
}

func Test_unsat_pubgrub_article(t *testing.T) {
	// Taken from the pubgrub article: https://nex3.medium.com/pubgrub-2fb6470504f
	p := fromPackages([]pk{
		{"menu", 15, deps("dropdown 2..3")},
		{"menu", 10, deps("dropdown 1..2")},
		{"dropdown", 2, deps("icons 2")},
		{"dropdown", 1, deps("intl 3")},
		{"icons", 2, nil},
		{"icons", 1, nil},
		{"intl", 5, nil},
		{"intl", 3, nil},
	})
	assertSnapshot(t, "unsat_pubgrub_article", solveUnsat(t, p, []string{"menu", "icons 1", "intl 5"}))
}

func Test_unsat_applies_graph_compression(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 10, deps("b")},
		{"a", 9, deps("b")},
		{"b", 100, deps("c 0..100")},
		{"b", 42, deps("c 0..100")},
		{"c", 103, nil},
		{"c", 101, nil},
		{"c", 100, nil},
		{"c", 99, nil},
	})
	assertSnapshot(t, "unsat_applies_graph_compression", solveUnsat(t, p, []string{"a", "c 101..104"}))
}

func Test_unsat_constrains(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 10, deps("b 50..100")},
		{"a", 9, deps("b 50..100")},
		{"b", 50, nil},
		{"b", 42, nil},
	})
	p.AddPackage("c", bundlebox.NewPack(10), nil, deps("b 0..50"))
	p.AddPackage("c", bundlebox.NewPack(8), nil, deps("b 0..50"))
	assertSnapshot(t, "unsat_constrains", solveUnsat(t, p, []string{"a", "c"}))
}

func Test_unsat_constrains_2(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, deps("b")},
		{"a", 2, deps("b")},
		{"b", 1, deps("c 1")},
		{"b", 2, deps("c 2")},
	})
	p.AddPackage("c", bundlebox.NewPack(1), nil, deps("a 3"))
	p.AddPackage("c", bundlebox.NewPack(2), nil, deps("a 3"))
	assertSnapshot(t, "unsat_constrains_2", solveUnsat(t, p, []string{"a"}))
}

func Test_missing_dep(t *testing.T) {
	p := fromPackages([]pk{{"a", 2, deps("missing")}, {"a", 1, nil}})
	assertSnapshot(t, "missing_dep", solveSnapshot(t, p, []string{"a"}))
}

func Test_no_backtracking(t *testing.T) {
	var packages []pk
	packages = append(packages, pk{"quetz-server", 2, deps("pydantic 10..20")}, pk{"quetz-server", 1, deps("pydantic 0..10")})
	for v := uint32(1); v <= 14; v++ {
		packages = append(packages, pk{"pydantic", v, nil})
	}
	p := fromPackages(packages)
	assertSnapshot(t, "no_backtracking", solveSnapshot(t, p, []string{"quetz-server", "pydantic 0..10"}))
}

func Test_incremental_crash(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 3, deps("missing")},
		{"a", 2, deps("missing")},
		{"a", 1, deps("b")},
		{"b", 2, deps("a 2..4")},
		{"b", 1, nil},
	})
	assertSnapshot(t, "incremental_crash", solveSnapshot(t, p, []string{"a"}))
}

func Test_excluded(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 2, deps("b")},
		{"a", 1, deps("c")},
		{"b", 1, nil},
		{"c", 1, nil},
	})
	p.Exclude("b", 1, "it is externally excluded")
	p.Exclude("c", 1, "it is externally excluded")
	assertSnapshot(t, "excluded", solveSnapshot(t, p, []string{"a"}))
}

func Test_merge_excluded(t *testing.T) {
	p := fromPackages([]pk{{"a", 1, nil}, {"a", 2, nil}})
	p.Exclude("a", 1, "it is externally excluded")
	p.Exclude("a", 2, "it is externally excluded")
	assertSnapshot(t, "merge_excluded", solveSnapshot(t, p, []string{"a"}))
}

func Test_merge_installable(t *testing.T) {
	p := fromPackages([]pk{{"a", 1, nil}, {"a", 2, nil}, {"a", 3, nil}, {"a", 4, nil}})
	assertSnapshot(t, "merge_installable", solveSnapshot(t, p, []string{"a 0..3", "a 3..5"}))
}

func Test_root_excluded(t *testing.T) {
	p := fromPackages([]pk{{"a", 1, nil}})
	p.Exclude("a", 1, "it is externally excluded")
	assertSnapshot(t, "root_excluded", solveSnapshot(t, p, []string{"a"}))
}

func Test_constraints(t *testing.T) {
	p := fromPackages([]pk{
		{"a", 1, deps("b 0..10")},
		{"b", 1, nil},
		{"b", 2, nil},
		{"c", 1, nil},
	})
	reqs := p.Requirements([]string{"a 0..10"})
	cons := p.VersionSets([]string{"b 1..2", "c"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs, Constraints: cons})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=1
	`)
}

// skipSoft skips an upstream test of soft requirements.
func skipSoft(t *testing.T) {
	t.Skip("cdcl does not support soft requirements: resolvo's are unsound (see the package documentation); prefer installed packages with Candidates.Favored or Locked")
}

func Test_solve_with_additional(t *testing.T) { skipSoft(t) }

func Test_solve_with_soft_requirement_forbid_clause_conflict(t *testing.T) { skipSoft(t) }

func Test_solve_with_additional_with_constrains(t *testing.T) { skipSoft(t) }

func Test_snapshot(t *testing.T) {
	p := fromPackages([]pk{
		{"menu", 15, deps("dropdown 2..3")},
		{"menu", 10, deps("dropdown 1..2")},
		{"dropdown", 2, deps("icons 2")},
		{"dropdown", 1, deps("intl 3; if menu")},
		{"icons", 2, nil},
		{"icons", 1, nil},
		{"intl", 5, nil},
		{"intl", 3, nil},
	})
	menu := p.PackageName("menu")
	snapshot, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sp := snapshot.Provider()
	menuReq, err := sp.AddPackageRequirement(menu, "*")
	if err != nil {
		t.Fatal(err)
	}
	reqs := []cdcl.ConditionalRequirement{cdcl.SingleRequirement(menuReq).Unconditional()}
	assertSnapshot(t, "snapshot", solveForSnapshot(t, sp, reqs, nil))
}

// The snapshot's highest-index version set must remain resolvable through a
// SnapshotProvider: it must not be shadowed by the first version set added
// with AddPackageRequirement.
func Test_snapshot_highest_version_set_not_shadowed(t *testing.T) {
	p := fromPackages([]pk{{"a", 1, deps("b 1")}, {"b", 1, nil}})
	a := p.PackageName("a")
	snapshot, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sp := snapshot.Provider()
	aReq, err := sp.AddPackageRequirement(a, "*")
	if err != nil {
		t.Fatal(err)
	}
	reqs := []cdcl.ConditionalRequirement{cdcl.SingleRequirement(aReq).Unconditional()}
	// Solving must resolve `b 1`, the highest-index version set in the
	// snapshot, to pull in b.
	assertInline(t, solveForSnapshot(t, sp, reqs, nil), `
		a=1
		b=1
	`)
}

func Test_snapshot_union_requirements(t *testing.T) {
	p := fromPackages([]pk{
		{"icons", 2, nil},
		{"icons", 1, nil},
		{"intl", 5, nil},
		{"intl", 3, nil},
		{"union", 1, deps("icons 2 | intl")},
	})
	reqs := p.Requirements([]string{"intl", "union"})
	assertSnapshot(t, "snapshot_union_requirements", solveForSnapshot(t, p, reqs, nil))
}

func Test_union_empty_requirements(t *testing.T) {
	p := fromPackages([]pk{{"a", 1, deps("b 1 | c")}, {"b", 1, nil}})
	assertInline(t, solveSnapshot(t, p, []string{"a"}), `
		a=1
		b=1
	`)
}

func Test_root_constraints(t *testing.T) {
	p := fromPackages([]pk{{"icons", 1, nil}, {"union", 1, deps("icons")}})
	reqs := p.Requirements([]string{"union"})
	cons := p.VersionSets([]string{"union 5"})
	assertSnapshot(t, "root_constraints", solveForSnapshot(t, p, reqs, cons))
}

func Test_explicit_root_requirements(t *testing.T) {
	p := fromPackages([]pk{
		// a depends transitively on b
		{"a", 1, deps("b")},
		// b depends on c, but the highest version of b constrains c to <2.
		{"b", 1, deps("c")},
		{"b", 2, deps("c 1..2")},
		// c has more versions than b, so the heuristic will most likely
		// pick b first.
		{"c", 1, nil},
		{"c", 2, nil},
		{"c", 3, nil},
		{"c", 4, nil},
		{"c", 5, nil},
	})
	// We require both a and c explicitly. The expected outcome is the
	// highest versions of a and c and a lower version of b.
	reqs := p.Requirements([]string{"a", "c"})
	_, solved := solveOK(t, p, cdcl.Problem{Requirements: reqs})
	assertInline(t, transactionToString(p, solved), `
		a=1
		b=1
		c=5
	`)
}
