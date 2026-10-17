// Helpers ported from resolvo tests/solver/main.rs (BSD-3-Clause, see
// NOTICE).

package cdcl_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

// pk is a package version: name, version and dependency specs.
type pk struct {
	name    string
	version uint32
	deps    []string
}

// fromPackages builds a BundleBox provider from package versions.
func fromPackages(ps []pk) *bundlebox.Provider {
	specs := make([]bundlebox.PackageSpec, len(ps))
	for i, p := range ps {
		specs[i] = bundlebox.PackageSpec{Name: p.name, Version: p.version, Deps: p.deps}
	}
	return bundlebox.FromPackages(specs)
}

// deps is shorthand for a dependency list.
func deps(d ...string) []string { return d }

// newSolver returns a solver with the debug invariant checks enabled, like
// resolvo's tests running in debug builds.
func newSolver(p cdcl.DependencyProvider) *cdcl.Solver {
	s := cdcl.NewSolver(p)
	cdcl.EnableDebugChecks(s)
	return s
}

// transactionToString formats a solution as sorted lines.
func transactionToString(in cdcl.Interner, solvables []cdcl.SolvableID) string {
	lines := make([]string, len(solvables))
	for i, s := range solvables {
		lines[i] = in.DisplaySolvable(s)
	}
	sort.Strings(lines)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// solveToString solves and returns the solution, the conflict explanation
// or the cancellation cause.
func solveToString(t *testing.T, p cdcl.DependencyProvider, problem cdcl.Problem, ctx context.Context, wantUnsat bool) string {
	t.Helper()
	solver := newSolver(p)
	sol, err := solver.Solve(ctx, problem)
	if err == nil {
		if wantUnsat {
			t.Fatalf("expected unsat, but a solution was found:\n%s", transactionToString(p, sol))
		}
		return transactionToString(p, sol)
	}
	var conflict *cdcl.Conflict
	switch {
	case errors.As(err, &conflict):
		if !errors.Is(err, cdcl.ErrUnsolvable) {
			t.Errorf("conflict does not match ErrUnsolvable")
		}
		var dot strings.Builder
		graph, gerr := conflict.Graph()
		if gerr != nil {
			t.Fatalf("graph: %v", gerr)
		}
		if gerr := graph.Graphviz(&dot, p, true); gerr != nil {
			t.Fatalf("graphviz: %v", gerr)
		}
		t.Logf("UNSOLVABLE:\n%s", dot.String())
		return conflict.Display(p)
	case errors.Is(err, context.Canceled):
		return context.Cause(ctx).Error()
	}
	t.Fatalf("solve: %v", err)
	return ""
}

// solveUnsat solves specs, expecting no solution, and returns the conflict
// explanation (or the cancellation cause).
func solveUnsat(t *testing.T, p *bundlebox.Provider, specs []string) string {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p.Cancel = cancel
	return solveToString(t, p, cdcl.Problem{Requirements: p.Requirements(specs)}, ctx, true)
}

// solveSnapshot solves specs and returns the solution or the conflict
// explanation. resolvo runs these tests on an async runtime; Go providers
// are synchronous (resolvo produces identical results in its synchronous
// mode for every one of these tests).
func solveSnapshot(t *testing.T, p *bundlebox.Provider, specs []string) string {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p.Cancel = cancel
	return solveToString(t, p, cdcl.Problem{Requirements: p.Requirements(specs)}, ctx, false)
}

// solveForSnapshot solves the given requirements and constraints.
func solveForSnapshot(t *testing.T, p cdcl.DependencyProvider, reqs []cdcl.ConditionalRequirement, cons []cdcl.VersionSetID) string {
	t.Helper()
	return solveToString(t, p, cdcl.Problem{Requirements: reqs, Constraints: cons}, context.Background(), false)
}

// solveOK solves and fails the test on any error.
func solveOK(t *testing.T, p cdcl.DependencyProvider, problem cdcl.Problem) (*cdcl.Solver, []cdcl.SolvableID) {
	t.Helper()
	solver := newSolver(p)
	sol, err := solver.Solve(context.Background(), problem)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	return solver, sol
}

// readSnapshot returns the body of testdata/snapshots/<name>.snap.
func readSnapshot(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "snapshots", name+".snap"))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	s := string(data)
	// The body follows the second "---" line.
	first := strings.Index(s, "---\n")
	if first < 0 {
		t.Fatalf("snapshot %s: no header", name)
	}
	rest := s[first+4:]
	second := strings.Index(rest, "\n---\n")
	if second < 0 {
		t.Fatalf("snapshot %s: unterminated header", name)
	}
	return rest[second+5:]
}

// normalize trims surrounding blank lines and trailing whitespace, like
// insta's snapshot comparison.
func normalize(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// dedent removes the common leading indentation of an inline snapshot.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	minIndent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	for i, l := range lines {
		if len(l) >= minIndent && minIndent > 0 {
			lines[i] = l[minIndent:]
		}
	}
	return strings.Join(lines, "\n")
}

// assertSnapshot compares got with the upstream file snapshot
// solver__<name>.
func assertSnapshot(t *testing.T, name, got string) {
	t.Helper()
	want := readSnapshot(t, "solver__"+name)
	if normalize(got) != normalize(want) {
		t.Errorf("snapshot %s mismatch\n--- got:\n%s\n--- want:\n%s", name, got, want)
	}
}

// assertInline compares got with an inline snapshot.
func assertInline(t *testing.T, got, want string) {
	t.Helper()
	if normalize(got) != normalize(dedent(want)) {
		t.Errorf("inline snapshot mismatch\n--- got:\n%s\n--- want:\n%s", got, dedent(want))
	}
}
