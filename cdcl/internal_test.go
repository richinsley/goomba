package cdcl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Cases from resolvo src/solver/clause.rs (tests module).

func TestLiteralSatisfyingValue(t *testing.T) {
	if rootVariable.negative().satisfyingValue() {
		t.Error("negative literal must be satisfied by false")
	}
	if !rootVariable.positive().satisfyingValue() {
		t.Error("positive literal must be satisfied by true")
	}
}

func TestLiteralEval(t *testing.T) {
	var m decisionMap
	lit := rootVariable.positive()
	neg := rootVariable.negative()

	// Undecided.
	if _, ok := lit.eval(&m); ok {
		t.Error("undecided literal evaluated")
	}
	if _, ok := neg.eval(&m); ok {
		t.Error("undecided literal evaluated")
	}

	// Decided.
	m.set(rootVariable, true, 1)
	if v, ok := lit.eval(&m); !ok || !v {
		t.Error("positive literal of a true variable must be true")
	}
	if v, ok := neg.eval(&m); !ok || v {
		t.Error("negative literal of a true variable must be false")
	}

	m.set(rootVariable, false, 1)
	if v, ok := lit.eval(&m); !ok || v {
		t.Error("positive literal of a false variable must be false")
	}
	if v, ok := neg.eval(&m); !ok || !v {
		t.Error("negative literal of a false variable must be true")
	}
}

func TestRequiresWithAndWithoutConflict(t *testing.T) {
	var dt decisionTracker
	parent, c1, c2 := variableID(1), variableID(2), variableID(3)
	req := SingleRequirement(0)

	// No conflict, all candidates available.
	_, wl, conflict := requiresClause(parent, req, []variableID{c1, c2}, nil, nil, &dt)
	if conflict || wl.lits[0].variable() != parent || wl.lits[1].variable() != c1 {
		t.Fatalf("all candidates available: watches %v conflict %v", wl.lits, conflict)
	}

	// No conflict, one candidate still available.
	dt.tryAddDecision(decision{variable: c1, value: false, derivedFrom: 0}, 1)
	_, wl, conflict = requiresClause(parent, req, []variableID{c1, c2}, nil, nil, &dt)
	if conflict || wl.lits[0].variable() != parent || wl.lits[1].variable() != c2 {
		t.Fatalf("one candidate available: watches %v conflict %v", wl.lits, conflict)
	}

	// Conflict, no candidates available.
	dt.tryAddDecision(decision{variable: c2, value: false, derivedFrom: installRootClause}, 1)
	_, wl, conflict = requiresClause(parent, req, []variableID{c1, c2}, nil, nil, &dt)
	if !conflict || wl.lits[0].variable() != parent || wl.lits[1].variable() != c1 {
		t.Fatalf("no candidates available: watches %v conflict %v", wl.lits, conflict)
	}

	// No conflict: every candidate is false but so is the parent, which
	// satisfies the clause (the lazy conditional path).
	dt.tryAddDecision(decision{variable: parent, value: false, derivedFrom: installRootClause}, 1)
	_, wl, conflict = requiresClause(parent, req, []variableID{c1, c2}, nil, nil, &dt)
	if conflict || wl.lits[0].variable() != parent || wl.lits[1].variable() != c1 {
		t.Fatalf("parent false: watches %v conflict %v", wl.lits, conflict)
	}
}

func TestConstrainsWithAndWithoutConflict(t *testing.T) {
	var dt decisionTracker
	parent, forbidden := variableID(1), variableID(2)

	// No conflict, forbidden package not installed.
	_, wl, conflict := constrainsClause(parent, forbidden, 0, &dt)
	if conflict || wl.lits[0].variable() != parent || wl.lits[1].variable() != forbidden {
		t.Fatalf("not installed: watches %v conflict %v", wl.lits, conflict)
	}

	// Conflict, forbidden package installed.
	dt.tryAddDecision(decision{variable: forbidden, value: true, derivedFrom: installRootClause}, 1)
	_, wl, conflict = constrainsClause(parent, forbidden, 0, &dt)
	if !conflict || wl.lits[0].variable() != parent || wl.lits[1].variable() != forbidden {
		t.Fatalf("installed: watches %v conflict %v", wl.lits, conflict)
	}

	// No conflict: the forbidden solvable is installed but the parent is
	// false, which satisfies the clause.
	dt.tryAddDecision(decision{variable: parent, value: false, derivedFrom: installRootClause}, 1)
	_, wl, conflict = constrainsClause(parent, forbidden, 0, &dt)
	if conflict || wl.lits[0].variable() != parent || wl.lits[1].variable() != forbidden {
		t.Fatalf("parent false: watches %v conflict %v", wl.lits, conflict)
	}
}

// A solvable that constrains itself away is an assertion; it must not watch
// the same literal twice (resolvo trips a debug assertion there).
func TestConstrainsSelfIsAssertion(t *testing.T) {
	var dt decisionTracker
	_, wl, conflict := constrainsClause(1, 1, 0, &dt)
	if wl.ok || conflict {
		t.Fatalf("self constraint: watches %v conflict %v", wl, conflict)
	}
}

// Cases from resolvo src/internal/id.rs (tests module). The size and niche
// tests are Rust layout properties with no Go equivalent.

func TestVariableIDRootRoundtrip(t *testing.T) {
	if !rootVariable.isRoot() || variableID(1).isRoot() {
		t.Fatal("root variable is index 0")
	}
	if l := variableID(7).positive(); l.variable() != 7 || l.negate() {
		t.Fatalf("literal roundtrip: %v", l)
	}
	if l := variableID(7).negative(); l.variable() != 7 || !l.negate() {
		t.Fatalf("literal roundtrip: %v", l)
	}
}

// Case from resolvo src/solver/decision_tracker.rs (tests module).
func TestTakeSyncFloorTracksMinimumStackLength(t *testing.T) {
	var dt decisionTracker
	push := func(v variableID, level uint32) {
		if added, conflict := dt.tryAddDecision(decision{variable: v, value: true, derivedFrom: installRootClause}, level); !added || conflict {
			t.Fatalf("push %d failed", v)
		}
	}
	check := func(want int) {
		t.Helper()
		if got := dt.takeSyncFloor(); got != want {
			t.Fatalf("takeSyncFloor = %d, want %d", got, want)
		}
	}
	check(0)

	push(0, 1)
	push(1, 2)
	push(2, 2)
	check(0)

	// An assignment made and undone between two observations cancels out:
	// the floor only drops to the minimum length reached.
	dt.undoLast()
	push(3, 2)
	check(2)

	// Two undos followed by new pushes report the deepest undo.
	dt.undoLast()
	dt.undoLast()
	push(4, 2)
	push(5, 2)
	check(1)

	// Pushes alone never lower the floor.
	push(6, 2)
	check(3)

	// Clearing resets the floor to zero.
	dt.clear()
	check(0)
}

// Case from resolvo src/solver/binary_encoding.rs (tests module), compared
// with its insta snapshot.
func TestAtMostOnceTracker(t *testing.T) {
	type v struct {
		concrete bool
		id       int
	}
	str := func(x v) string {
		if x.concrete {
			return fmt.Sprintf("x%d", x.id)
		}
		return fmt.Sprintf("v%d", x.id)
	}
	var clauses []string
	nextConcrete, nextVar := 1, 1
	tracker := newAtMostOnceTracker[v]()
	addNewVar := func() {
		x := v{concrete: true, id: nextConcrete}
		nextConcrete++
		tracker.add(x,
			func(a, b v, positive bool) {
				neg := "¬"
				if positive {
					neg = ""
				}
				clauses = append(clauses, fmt.Sprintf("¬%s ∨ %s%s", str(a), neg, str(b)))
			},
			func() v {
				y := v{id: nextVar}
				nextVar++
				return y
			})
	}
	for i := 0; i < 4; i++ {
		addNewVar()
	}

	data, err := os.ReadFile(filepath.Join("testdata", "snapshots", "resolvo__solver__binary_encoding__test__at_most_once_tracker.snap"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(data), "---\n", 3)
	want := strings.TrimSpace(parts[2])
	if got := strings.Join(clauses, "\n"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Cases from resolvo src/conflict.rs (tests module).

func TestIndenterWithoutTopLevelIndent(t *testing.T) {
	in := newIndenter(false)
	in = in.pushLevelWithOrder(lastChild)
	if got := in.indent(); got != "" {
		t.Errorf("got %q", got)
	}
	in = in.pushLevelWithOrder(lastChild)
	if got := in.indent(); got != "└─ " {
		t.Errorf("got %q", got)
	}
}

func TestIndenterWithMultipleSiblings(t *testing.T) {
	in := newIndenter(true)
	steps := []struct {
		order childOrder
		want  string
	}{
		{lastChild, "└─ "},
		{hasRemainingSiblings, "   ├─ "},
		{lastChild, "   │  └─ "},
		{lastChild, "   │     └─ "},
		{hasRemainingSiblings, "   │        ├─ "},
	}
	for i, s := range steps {
		in = in.pushLevelWithOrder(s.order)
		if got := in.indent(); got != s.want {
			t.Errorf("step %d: got %q, want %q", i, got, s.want)
		}
	}
}

// Cases from resolvo src/utils/indexed_set.rs (tests module), for the
// bitSet that replaces IndexedSet.

func TestBitSetWordBoundaries(t *testing.T) {
	var s bitSet
	for _, idx := range []uint32{0, wordBits - 1, wordBits, wordBits + 1, wordBits*2 - 1, wordBits * 2} {
		if !s.insert(idx) || !s.contains(idx) || s.insert(idx) {
			t.Fatalf("index %d", idx)
		}
	}
	for _, idx := range []uint32{1, wordBits - 2, wordBits + 2, wordBits*2 + 1} {
		if s.contains(idx) {
			t.Fatalf("index %d unexpectedly present", idx)
		}
	}
}

func TestBitSetSparseAndLargeIndices(t *testing.T) {
	var s bitSet
	if s.contains(10_000) || !s.insert(10_000) || !s.contains(10_000) {
		t.Fatal("10000")
	}
	if s.contains(9_999) || s.contains(10_001) {
		t.Fatal("neighbours of 10000")
	}
	if !s.insert(0) || !s.contains(0) || !s.contains(10_000) {
		t.Fatal("0")
	}
	for _, idx := range []uint32{wordBits - 1, wordBits, wordBits + 1} {
		if s.contains(idx) {
			t.Fatalf("index %d unexpectedly present", idx)
		}
	}
}

func TestBitSetDuplicateInsertsAndSequentialFill(t *testing.T) {
	var s bitSet
	for i := uint32(0); i <= wordBits*32; i++ {
		if !s.insert(i) {
			t.Fatalf("insert %d", i)
		}
	}
	for i := uint32(0); i <= wordBits*32; i++ {
		if s.insert(i) || !s.contains(i) {
			t.Fatalf("reinsert %d", i)
		}
	}
	if s.contains(wordBits*32 + 1) {
		t.Fatal("past the end")
	}
	var r bitSet
	for i := int(wordBits * 4); i >= 0; i-- {
		if !r.insert(uint32(i)) {
			t.Fatalf("reverse insert %d", i)
		}
	}
	for i := uint32(0); i <= wordBits*4; i++ {
		if !r.contains(i) {
			t.Fatalf("reverse contains %d", i)
		}
	}
}

func TestPositionSetOrder(t *testing.T) {
	var s positionSet
	ps := []clausePosition{{3, 1}, {0, 5}, {3, 0}, {1, 64}, {0, 0}, {70, 2}}
	for _, p := range ps {
		s.insert(p)
	}
	var got []clausePosition
	for p, ok := s.first(); ok; p, ok = s.after(p) {
		got = append(got, p)
	}
	want := []clausePosition{{0, 0}, {0, 5}, {1, 64}, {3, 0}, {3, 1}, {70, 2}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	s.remove(clausePosition{1, 64})
	s.remove(clausePosition{0, 0})
	if p, _ := s.first(); p != (clausePosition{0, 5}) {
		t.Fatalf("first after removal = %v", p)
	}
	if p, _ := s.after(clausePosition{0, 5}); p != (clausePosition{3, 0}) {
		t.Fatalf("after {0 5} = %v", p)
	}
	if s.contains(clausePosition{1, 64}) || !s.contains(clausePosition{70, 2}) {
		t.Fatal("contains")
	}
}

// The graph helpers mirror petgraph: edges are listed most recent first and
// removing a node swaps the last node into its index.
func TestDigraphPetgraphOrder(t *testing.T) {
	var g digraph
	a := g.addNode(ConflictNode{Kind: ConflictNodeRoot})
	b := g.addNode(ConflictNode{Kind: ConflictNodeUnresolved})
	c := g.addNode(ConflictNode{Kind: ConflictNodeSolvable, Solvable: 1})
	d := g.addNode(ConflictNode{Kind: ConflictNodeSolvable, Solvable: 2})
	e1 := g.addEdge(a, c, requiresEdge(SingleRequirement(1)))
	e2 := g.addEdge(a, d, requiresEdge(SingleRequirement(2)))
	e3 := g.addEdge(d, d, conflictEdge(CauseForbidMultipleInstances))
	if got := g.outgoing(a); fmt.Sprint(got) != fmt.Sprint([]int{e2, e1}) {
		t.Fatalf("outgoing(a) = %v", got)
	}
	if got := g.incoming(d); fmt.Sprint(got) != fmt.Sprint([]int{e3, e2}) {
		t.Fatalf("incoming(d) = %v", got)
	}
	g.removeNode(b)
	// d moved into b's index.
	if g.nodes[b].weight.Solvable != 2 || g.target(e2) != b || g.source(e3) != b || g.target(e3) != b {
		t.Fatalf("swap remove: nodes %v edges %v", g.nodes, g.edges)
	}
	if got := g.dfsPostOrder(a); fmt.Sprint(got) != fmt.Sprint([]int{c, b, a}) {
		t.Fatalf("postorder = %v", got)
	}
}

// Panics recovered at the API boundary fail closed: an out-of-range ID wraps
// ErrInvalidID and every other panic (invariant violations, runtime errors,
// provider and Pool panics) wraps ErrInternal. Only invariant violations are
// re-raised, and only under debug checks.
func TestRecoveredError(t *testing.T) {
	var rerr runtime.Error
	func() {
		defer func() { rerr, _ = recover().(runtime.Error) }()
		var s []int
		_ = s[len(s)+1]
	}()
	if rerr == nil {
		t.Fatal("no runtime error")
	}
	for _, debug := range []bool{false, true} {
		for _, r := range []any{rerr, "provider panic", errors.New("provider error panic")} {
			err := recoveredError(r, debug)
			if !errors.Is(err, ErrInternal) || !strings.Contains(err.Error(), fmt.Sprint(r)) {
				t.Errorf("recoveredError(%v, %v) = %v, want ErrInternal with the panic value", r, debug, err)
			}
		}
		if err := recoveredError(idError{kind: "solvable", id: MaxID}, debug); !errors.Is(err, ErrInvalidID) {
			t.Errorf("recoveredError(idError) = %v, want ErrInvalidID", err)
		}
	}
	if err := recoveredError(internalError("broken"), false); !errors.Is(err, ErrInternal) {
		t.Errorf("recoveredError(internalError) = %v, want ErrInternal", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("an invariant violation under debug checks was not re-raised")
			}
		}()
		_ = recoveredError(internalError("broken"), true)
	}()
}

// TestConflictAddClause checks that a conflict keeps its clauses in the
// order they were found, without duplicates, and that adding is O(1): a
// million additions would take minutes with a linear scan per addition.
func TestConflictAddClause(t *testing.T) {
	var c Conflict
	for _, id := range []clauseID{5, 3, 5, 9, 3, 0, 9, 1} {
		c.addClause(id)
	}
	if got, want := fmt.Sprint(c.clauses), "[5 3 9 0 1]"; got != want {
		t.Fatalf("clauses %s, want %s", got, want)
	}
	var big Conflict
	const n = 1 << 20
	for i := 0; i < n; i++ {
		big.addClause(clauseID(n - 1 - i))
		big.addClause(clauseID(n - 1 - i/2))
	}
	if len(big.clauses) != n || big.clauses[0] != n-1 || big.clauses[n-1] != 0 {
		t.Fatalf("%d clauses, first %d, last %d", len(big.clauses), big.clauses[0], big.clauses[len(big.clauses)-1])
	}
}
