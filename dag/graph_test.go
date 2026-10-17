package dag

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMultiDimension(t *testing.T) {
	g := New[string](3)
	for _, k := range []string{"python", "pip", "setuptools", "python_abi"} {
		g.AddNode(k)
	}
	mustConnect(t, g, 0, "python", "setuptools")
	mustConnect(t, g, 0, "python", "pip")
	mustConnect(t, g, 0, "setuptools", "pip")

	// pip -> python closes a cycle in dimension 0 but not in dimension 1.
	err := g.Connect(0, "pip", "python")
	var ce *CycleError[string]
	if !errors.As(err, &ce) {
		t.Fatalf("Connect(0, pip, python) = %v, want CycleError", err)
	}
	// Shortest existing path, not python -> setuptools -> pip.
	wantKeys(t, "Path", ce.Path, "python", "pip")
	mustConnect(t, g, 1, "pip", "python")
	mustConnect(t, g, 2, "python", "python_abi")

	wantKeys(t, "Layers(0)[2]", g.Layers(0)[2], "pip")
	wantInt(t, "Ordinal(1, python)", g.Ordinal(1, "python"), 1)
	wantInt(t, "Ordinal(0, python)", g.Ordinal(0, "python"), 0)
	wantKeys(t, "Descendents(0, pip)", g.Descendents(0, "pip"))
	wantKeys(t, "Descendents(1, pip)", g.Descendents(1, "pip"), "python")

	// Across dimensions 0 and 1 the cycle is visible; pip is not its own
	// descendent. Sorted by ordinal in the first dimension listed.
	wantKeys(t, "DescendentsAcross(0,1 pip)", g.DescendentsAcross([]int{0, 1}, "pip"), "python", "setuptools")
	wantKeys(t, "DescendentsAcross(1,0 pip)", g.DescendentsAcross([]int{1, 0}, "pip"), "setuptools", "python")
	wantKeys(t, "DescendentsAcross(0,2 python)", g.DescendentsAcross([]int{0, 2}, "python"), "python_abi", "setuptools", "pip")
	wantKeys(t, "DescendentsAcross(2 pip)", g.DescendentsAcross([]int{2}, "pip"))
	wantKeys(t, "DescendentsAcross(none)", g.DescendentsAcross(nil, "pip"))
	wantKeys(t, "DescendentsAcross(all, absent)", g.DescendentsAcross([]int{0, 1, 2}, "absent"))

	// A dimension-1 change leaves dimension 0's cached topology alone.
	g.Layers(0)
	before := cached(g, 0)
	mustConnect(t, g, 1, "setuptools", "python_abi")
	if cached(g, 0) != before || cached(g, 1) != nil {
		t.Fatal("Connect in dimension 1 invalidated the wrong topology")
	}
	checkGraph(t, g, refFrom(g), "absent")
}

func TestConnectErrors(t *testing.T) {
	g := New[string](2)
	g.AddNode("a")
	g.AddNode("b\x1b[2J")
	for _, c := range []struct {
		dim      int
		from, to string
		is       error
		msg      string
	}{
		{0, "a", "zz", ErrUnknownNode, `dag: connect "a" -> "zz" in dimension 0: unknown node "zz"`},
		{1, "zz", "a", ErrUnknownNode, `dag: connect "zz" -> "a" in dimension 1: unknown node "zz"`},
		{2, "a", "b\x1b[2J", ErrDimension, `dag: connect "a" -> "b\x1b[2J" in dimension 2: dimension out of range [0, 2)`},
		{-1, "a", "a", ErrDimension, `dag: connect "a" -> "a" in dimension -1: dimension out of range [0, 2)`},
		{0, "a", "a", ErrCycle, `dag: connect "a" -> "a" in dimension 0: edge would close a cycle: existing path "a"`},
	} {
		err := g.Connect(c.dim, c.from, c.to)
		if !errors.Is(err, c.is) || err.Error() != c.msg {
			t.Errorf("Connect(%d, %q, %q) = %v\nwant %s", c.dim, c.from, c.to, err, c.msg)
		}
	}
	if len(g.edges) != 0 {
		t.Fatalf("failed Connects added edges: %d", len(g.edges))
	}

	// Integer keys are not quoted.
	h := New[int](1)
	h.AddNode(1)
	h.AddNode(2)
	h.Connect(0, 1, 2)
	if err := h.Connect(0, 2, 1); err == nil || err.Error() != "dag: connect 2 -> 1 in dimension 0: edge would close a cycle: existing path 1 -> 2" {
		t.Fatalf("Connect(0, 2, 1) = %v", err)
	}
}

func TestKeyString(t *testing.T) {
	type pkgName string
	type num int
	type pair struct{ a, b string }
	for i, c := range []struct{ got, want string }{
		{keyString("x y"), `"x y"`},
		{keyString("\n"), `"\n"`},
		{keyString("plain"), `"plain"`},
		{keyString(pkgName("a -> b")), `"a -> b"`},
		{keyString(pkgName("c")), `"c"`},
		{keyString[any]("s"), `"s"`},
		{keyString(7), "7"},
		{keyString(num(-3)), "-3"},
		{keyString(2.5), "2.5"},
		{keyString(pair{"a", "b"}), `"{a b}"`},
		{keyString(pair{"a->b", "\x00"}), `"{a->b \x00}"`},
		{keyString([2]int{1, 2}), `"[1 2]"`},
		{keyString(struct{ s string }{`x"y`}), `"{x\"y}"`},
		{keyString(struct{ s string }{"x->y"}), `"{x->y}"`},
	} {
		if c.got != c.want {
			t.Errorf("case %d: keyString = %s, want %s", i, c.got, c.want)
		}
	}
}

// TestNamedStringKeysCannotForgeErrors is the regression test for keys of a
// named string type, which used to print unquoted.
func TestNamedStringKeysCannotForgeErrors(t *testing.T) {
	type pkgName string
	g := New[pkgName](1)
	g.AddNode("a -> b")
	g.AddNode("c")
	if err := g.Connect(0, "a -> b", "c"); err != nil {
		t.Fatal(err)
	}
	err := g.Connect(0, "c", "a -> b")
	want := `dag: connect "c" -> "a -> b" in dimension 0: edge would close a cycle: existing path "a -> b" -> "c"`
	if err == nil || err.Error() != want {
		t.Fatalf("Connect = %v\nwant %s", err, want)
	}
	err = g.Connect(0, "c", "d -> e")
	want = `dag: connect "c" -> "d -> e" in dimension 0: unknown node "d -> e"`
	if err == nil || err.Error() != want {
		t.Fatalf("Connect = %v\nwant %s", err, want)
	}
}

func TestNaNKeys(t *testing.T) {
	g := New[float64](1)
	nan := math.NaN()
	if g.AddNode(nan) || g.Len() != 0 || g.Has(nan) || g.RemoveNode(nan) {
		t.Fatal("a NaN key was accepted")
	}
	g.AddNode(1)
	if err := g.Connect(0, 1, nan); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("Connect(1, NaN) = %v", err)
	}
	type rec struct{ x float64 }
	h := New[rec](1)
	if h.AddNode(rec{nan}) || !h.AddNode(rec{1}) {
		t.Fatal("struct keys")
	}
}

// TestDuplicateEdge stands in for pydagger's test_no_multi_connect: there
// are no pins, and a repeated edge is a no-op.
func TestDuplicateEdge(t *testing.T) {
	g := fourNodes(1)
	mustConnect(t, g, 0, "n1", "n4")
	g.Layers(0)
	before := cached(g, 0)
	mustConnect(t, g, 0, "n1", "n4")
	if len(g.edges) != 1 || len(g.index["n1"].out[0]) != 1 || cached(g, 0) != before {
		t.Fatal("duplicate Connect changed the graph")
	}
}

func TestCyclePathIsShortest(t *testing.T) {
	// a -> b -> c -> d -> e and a -> x -> e, b -> e: the shortest path from
	// a to e is a -> b -> e (found before a -> x -> e, b's edge came first).
	g := graphOf([]string{"a", "b", "c", "d", "e", "x"},
		[2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"c", "d"}, [2]string{"d", "e"},
		[2]string{"a", "x"}, [2]string{"x", "e"}, [2]string{"b", "e"})
	var ce *CycleError[string]
	if err := g.Connect(0, "e", "a"); !errors.As(err, &ce) {
		t.Fatalf("Connect = %v", err)
	}
	wantKeys(t, "Path", ce.Path, "a", "b", "e")
	if err := g.Connect(0, "d", "b"); !errors.As(err, &ce) {
		t.Fatalf("Connect = %v", err)
	}
	wantKeys(t, "Path", ce.Path, "b", "c", "d")
}

func TestRemoveNode(t *testing.T) {
	g := New[string](2)
	for _, k := range []string{"a", "b", "c", "d"} {
		g.AddNode(k)
	}
	mustConnect(t, g, 0, "a", "b")
	mustConnect(t, g, 0, "b", "c")
	mustConnect(t, g, 1, "c", "b")
	mustConnect(t, g, 1, "d", "b")
	wantInt(t, "Ordinal(0, c)", g.Ordinal(0, "c"), 2)

	if g.RemoveNode("zz") {
		t.Fatal("RemoveNode(absent) = true")
	}
	if !g.RemoveNode("b") || g.Has("b") {
		t.Fatal("RemoveNode(b) failed")
	}
	wantKeys(t, "Nodes", g.Nodes(), "a", "c", "d")
	wantInt(t, "Ordinal(0, c)", g.Ordinal(0, "c"), 0)
	wantInt(t, "Ordinal(0, b)", g.Ordinal(0, "b"), -1)
	wantKeys(t, "Leaves(1)", g.Leaves(1), "a", "c", "d")
	if len(g.edges) != 0 {
		t.Fatalf("%d edges left", len(g.edges))
	}
	if g.Disconnect(0, "a", "b") {
		t.Fatal("Disconnect of a removed node's edge = true")
	}

	// Re-adding puts the node last; its old edges are gone, and edges that
	// were cycles through it no longer are.
	if !g.AddNode("b") || g.AddNode("b") {
		t.Fatal("AddNode")
	}
	wantKeys(t, "Nodes", g.Nodes(), "a", "c", "d", "b")
	mustConnect(t, g, 0, "c", "a")
	mustConnect(t, g, 0, "b", "c")
	wantKeys(t, "Layers(0)", slices.Concat(g.Layers(0)...), "d", "b", "c", "a")
	checkGraph(t, g, refFrom(g), "absent")
}

func TestDisconnect(t *testing.T) {
	g := chain3()
	for _, c := range []struct{ from, to string }{{"n1", "n3"}, {"n2", "n1"}, {"zz", "n1"}, {"n1", "zz"}} {
		if g.Disconnect(0, c.from, c.to) {
			t.Errorf("Disconnect(%s, %s) = true", c.from, c.to)
		}
	}
	if !g.Disconnect(0, "n2", "n3") || g.Disconnect(0, "n2", "n3") {
		t.Fatal("Disconnect(n2, n3) not exactly once")
	}
	wantInt(t, "MaxOrdinal", g.MaxOrdinal(0), 1)
	// The edge that closed a cycle before is fine now.
	mustConnect(t, g, 0, "n3", "n1")
	wantKeys(t, "Layers", slices.Concat(g.Layers(0)...), "n3", "n1", "n2")
}

func TestEmptyGraph(t *testing.T) {
	g := New[string](1)
	if g.Layers(0) != nil || g.Subgraphs(0) != nil || g.Roots(0) != nil || g.Leaves(0) != nil || g.Nodes() == nil {
		t.Fatal("empty graph queries")
	}
	wantInt(t, "MaxOrdinal", g.MaxOrdinal(0), 0)
	wantInt(t, "Len", g.Len(), 0)
	wantInt(t, "Dims", g.Dims(), 1)
	wantInt(t, "Ordinal", g.Ordinal(0, "a"), -1)
}

func TestPanics(t *testing.T) {
	g := chain3()
	for _, c := range []struct {
		name string
		f    func()
	}{
		{"New(0)", func() { New[int](0) }},
		{"Ordinal", func() { g.Ordinal(1, "n1") }},
		{"MaxOrdinal", func() { g.MaxOrdinal(-1) }},
		{"Layers", func() { g.Layers(1) }},
		{"Descendents", func() { g.Descendents(1, "n1") }},
		{"Ascendents", func() { g.Ascendents(1, "n1") }},
		{"DescendentsAcross", func() { g.DescendentsAcross([]int{0, 1}, "n1") }},
		{"Subgraphs", func() { g.Subgraphs(1) }},
		{"Subgraph", func() { g.Subgraph(1, "n1") }},
		{"Roots", func() { g.Roots(1) }},
		{"Leaves", func() { g.Leaves(1) }},
		{"Disconnect", func() { g.Disconnect(1, "n1", "n2") }},
	} {
		name, f := c.name, c.f
		func() {
			defer func() {
				r := recover()
				if s, _ := r.(string); !strings.HasPrefix(s, "dag: ") {
					t.Errorf("%s: recovered %v, want a dag panic", name, r)
				}
			}()
			f()
		}()
	}
}

// TestLazyTopology covers recomputation after each kind of change, which
// pydagger signals with topology_changed.
func TestLazyTopology(t *testing.T) {
	g := New[string](2)
	if cached(g, 0) != nil {
		t.Fatal("topology computed eagerly")
	}
	g.AddNode("a")
	g.AddNode("b")
	wantInt(t, "MaxOrdinal", g.MaxOrdinal(0), 0)
	first := cached(g, 0)
	wantInt(t, "MaxOrdinal", g.MaxOrdinal(0), 0)
	if cached(g, 0) != first {
		t.Fatal("topology recomputed without a change")
	}
	mustConnect(t, g, 0, "a", "b")
	wantInt(t, "MaxOrdinal", g.MaxOrdinal(0), 1)
	g.MaxOrdinal(1)
	g.AddNode("c")
	if cached(g, 0) != nil || cached(g, 1) != nil {
		t.Fatal("AddNode left a topology cached")
	}
	wantKeys(t, "Layers", g.Layers(0)[0], "a", "c")
}

// TestConcurrentQueries runs every query from many goroutines on a stale
// topology; run under -race.
func TestConcurrentQueries(t *testing.T) {
	gg := buildRandomDAG(t, 60, 0.1, 5, 3)
	g := gg.g
	for _, limit := range []int{defaultClosureLimit, 0} {
		g.closureLimit = limit
		g.invalidateAll()
		want := captureSnapshot(g)
		g.invalidateAll()
		var wg sync.WaitGroup
		errs := make(chan string, 16)
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d := i % 3
				for _, k := range g.Nodes() {
					g.Ordinal(d, k)
					g.Descendents(d, k)
					g.Ascendents(d, k)
					g.DescendentsAcross([]int{2, 1, 0}, k)
					g.Subgraph(d, k)
				}
				g.Layers(d)
				g.Subgraphs(d)
				if diffs := want.diff(captureSnapshot(g)); len(diffs) > 0 {
					errs <- strings.Join(diffs, "\n")
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
	}
}

// TestRandomOperations drives Graph and the reference model with the same
// long random sequences of operations, checking them against each other:
// small graphs checked often, and graphs of more than 64 nodes (several
// words of descendent bits, long reorders) checked less often.
func TestRandomOperations(t *testing.T) {
	for _, c := range []struct {
		name            string
		keys, ops, step int
		seeds           uint64
	}{
		{"small", 18, 2000, 50, 8},
		{"large", 150, 6000, 1500, 6},
	} {
		for seed := range c.seeds {
			t.Run(fmt.Sprintf("%s/%d", c.name, seed), func(t *testing.T) {
				t.Parallel()
				randomOperations(t, seed, c.keys, c.ops, c.step)
			})
		}
	}
}

func randomOperations(t *testing.T, seed uint64, keys, ops, step int) {
	rng := rand.New(rand.NewPCG(seed, 99))
	dims := 1 + int(seed%3)
	x := newDiffer(t, dims, keys/2)
	if seed%2 == 1 {
		x.g.closureLimit = 0
	}
	key := func() int { return rng.IntN(keys + 2) } // a few keys never added
	for i := range ops {
		d := rng.IntN(dims)
		switch op := rng.IntN(40); {
		case op < 24:
			x.connect(d, key(), key())
		case op < 27:
			batch := make([][2]int, rng.IntN(12))
			for j := range batch {
				batch[j] = [2]int{key(), key()}
			}
			x.connectAll(d, batch)
		case op < 32:
			x.disconnect(d, key(), key())
		case op < 37:
			x.add(key())
		default:
			x.remove(key())
		}
		if i%step == 0 {
			x.check()
		}
	}
	x.check()
	if x.cycles < ops/200 || x.added < ops/15 || x.g.Len() < keys/2 {
		t.Fatalf("weak run: %d cycles refused, %d edge additions, %d nodes", x.cycles, x.added, x.g.Len())
	}
}

// cached returns dimension d's cached topology, or nil if it is stale.
func cached[K comparable](g *Graph[K], d int) *topology { return g.state[d].topo.Load() }

func TestConnectAll(t *testing.T) {
	g := New[string](2)
	for _, k := range []string{"a", "b", "c", "d"} {
		g.AddNode(k)
	}
	mustConnect(t, g, 0, "c", "d")

	// A chain given back to front, with duplicates of a new and an
	// existing edge.
	if err := g.ConnectAll(0, [][2]string{{"b", "c"}, {"a", "b"}, {"a", "b"}, {"c", "d"}}); err != nil {
		t.Fatal(err)
	}
	wantKeys(t, "Layers", slices.Concat(g.Layers(0)...), "a", "b", "c", "d")
	wantInt(t, "edges", len(g.edges), 3)
	checkInternals(t, g)

	// All or nothing: the cycle closed by the third edge is reported as
	// Connect would report it, after the first two, and nothing is added.
	err := g.ConnectAll(1, [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}, {"a", "zz"}})
	var ce *CycleError[string]
	if !errors.As(err, &ce) || ce.Dim != 1 || ce.From != "c" || ce.To != "a" || !slices.Equal(ce.Path, []string{"a", "b", "c"}) {
		t.Fatalf("ConnectAll = %v", err)
	}
	if len(g.Descendents(1, "a")) != 0 || len(g.edges) != 3 {
		t.Fatal("failed ConnectAll added edges")
	}
	// An unknown node before any cycle is reported first.
	if err := g.ConnectAll(1, [][2]string{{"a", "b"}, {"a", "zz"}, {"b", "a"}}); !errors.Is(err, ErrUnknownNode) ||
		err.Error() != `dag: connect "a" -> "zz" in dimension 1: unknown node "zz"` {
		t.Fatalf("ConnectAll = %v", err)
	}
	if err := g.ConnectAll(1, [][2]string{{"d", "d"}}); !errors.Is(err, ErrCycle) {
		t.Fatalf("ConnectAll self-edge = %v", err)
	}
	if err := g.ConnectAll(2, nil); !errors.Is(err, ErrDimension) ||
		err.Error() != "dag: connect 0 edges in dimension 2: dimension out of range [0, 2)" {
		t.Fatalf("ConnectAll bad dim = %v", err)
	}
	if err := g.ConnectAll(1, nil); err != nil {
		t.Fatal(err)
	}
	// The rebuilt order still accepts and refuses single edges correctly.
	if err := g.Connect(0, "d", "a"); !errors.As(err, &ce) || !slices.Equal(ce.Path, []string{"a", "b", "c", "d"}) {
		t.Fatalf("Connect after ConnectAll = %v", err)
	}
	checkGraph(t, g, refFrom(g), "absent")
}

// TestConnectAllPathPrefersExistingEdges pins the search order behind a
// refusal's Path: like Connect, ConnectAll follows existing edges before the
// batch's own, so the shortest path a→b→d is reported, not a→c→d.
func TestConnectAllPathPrefersExistingEdges(t *testing.T) {
	g := New[string](1)
	for _, k := range []string{"a", "b", "c", "d"} {
		g.AddNode(k)
	}
	mustConnect(t, g, 0, "a", "b")
	mustConnect(t, g, 0, "b", "d")
	err := g.ConnectAll(0, [][2]string{{"a", "c"}, {"c", "d"}, {"d", "a"}})
	var ce *CycleError[string]
	if !errors.As(err, &ce) || !slices.Equal(ce.Path, []string{"a", "b", "d"}) {
		t.Fatalf("ConnectAll = %v, want existing path a -> b -> d", err)
	}
}

// TestConnectAllReportsCallerKeys checks that a refusal names the keys the
// caller passed, as Connect does, even when they print differently from the
// stored keys they compare equal to.
func TestConnectAllReportsCallerKeys(t *testing.T) {
	negZero := math.Copysign(0, -1)
	g := New[float64](1)
	g.AddNode(0)
	g.AddNode(1)
	if err := g.Connect(0, 0, 1); err != nil {
		t.Fatal(err)
	}
	single := g.Connect(0, 1, negZero)
	batch := g.ConnectAll(0, [][2]float64{{1, negZero}})
	if single == nil || batch == nil || single.Error() != batch.Error() {
		t.Fatalf("Connect = %v, ConnectAll = %v; want the same error", single, batch)
	}
}

// TestChainBackToFront builds a long chain in the order that is worst for
// Connect's incremental ordering, with ConnectAll, and checks the result.
func TestChainBackToFront(t *testing.T) {
	const n = 20000
	g, edges := backToFront(n)
	if err := g.ConnectAll(0, edges); err != nil {
		t.Fatal(err)
	}
	wantInt(t, "MaxOrdinal", g.MaxOrdinal(0), n-1)
	wantInt(t, "Ordinal(0)", g.Ordinal(0, 0), n-1)
	var ce *CycleError[int]
	if err := g.ConnectAll(0, [][2]int{{0, n - 1}}); !errors.As(err, &ce) || len(ce.Path) != n {
		t.Fatalf("closing the chain: %v", err)
	}
	// The order ConnectAll rebuilt lets Connect add a chain edge at once.
	g.AddNode(n)
	if err := g.Connect(0, n, n-1); err != nil {
		t.Fatal(err)
	}
	checkInternals(t, g)
}

// TestCachedQueriesTakeNoLock checks that a query on a current topology
// does not wait for a dimension being computed, its own or another.
func TestCachedQueriesTakeNoLock(t *testing.T) {
	g := chain3()
	h := New[string](2)
	for _, k := range []string{"a", "b"} {
		h.AddNode(k)
	}
	h.Connect(1, "a", "b")
	g.Layers(0)
	h.Layers(1)
	h.invalidate(0)

	g.state[0].mu.Lock() // as if a reader were computing dimension 0
	h.state[0].mu.Lock()
	done := make(chan struct{})
	go func() {
		g.Layers(0)
		h.Layers(1)
		h.Descendents(1, "a")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a query on a current topology waited for a lock")
	}
	g.state[0].mu.Unlock()
	h.state[0].mu.Unlock()
	wantInt(t, "MaxOrdinal", h.MaxOrdinal(0), 0)
}
