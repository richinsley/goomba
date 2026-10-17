package dag

import (
	"errors"
	"slices"
	"testing"
)

// pyTest is one pytest function: its class/name, and either the port or
// the reason it is not ported.
type pyTest struct {
	name string
	skip string
	run  func(t *testing.T)
}

func runPyTests(t *testing.T, tests []pyTest) {
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip != "" {
				t.Skip("not ported: " + tc.skip)
			}
			tc.run(t)
		})
	}
}

const (
	skipPins   = "pins are UI machinery; dag connects nodes directly"
	skipSignal = "signals are UI machinery; dag has no observers"
	skipClone  = "auto-cloned pins are UI machinery"
)

// fourNodes is pydagger's setup of n1..n4 added to a fresh graph.
func fourNodes(dims int) *Graph[string] {
	g := New[string](dims)
	for _, k := range []string{"n1", "n2", "n3", "n4"} {
		g.AddNode(k)
	}
	return g
}

func mustConnect(t *testing.T, g *Graph[string], dim int, from, to string) {
	t.Helper()
	if err := g.Connect(dim, from, to); err != nil {
		t.Fatalf("Connect(%d, %s, %s) = %v", dim, from, to, err)
	}
}

func wantKeys(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// wantSet checks set equality, as the pytest assertions on set(...) do.
func wantSet(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("%s = %q, want the set %q", what, got, want)
	}
}

func wantInt(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, want %d", what, got, want)
	}
}

func wantCycle(t *testing.T, err error) {
	t.Helper()
	var ce *CycleError[string]
	if !errors.As(err, &ce) || !errors.Is(err, ErrCycle) {
		t.Fatalf("Connect = %v, want a CycleError", err)
	}
}

// Cases from pydagger tests/test_graph.py.
var graphTests = []pyTest{
	// Node basics
	{name: "TestDaggerNode/test_pin_collections_exist", skip: skipPins},
	{name: "TestDaggerNode/test_default_name", skip: "nodes are their keys; there is no name property"},
	{name: "TestDaggerNode/test_name_change_emits_signal", skip: skipSignal},
	{name: "TestDaggerNode/test_is_true_source_no_input_pins", skip: skipPins},
	{name: "TestDaggerNode/test_is_true_dest_no_output_pins", skip: skipPins},

	// Simple topology (single topology system)
	{name: "TestSimpleTopology/test_initial_subgraph_count", run: func(t *testing.T) {
		g := fourNodes(1)
		wantInt(t, "subgraph count", len(g.Subgraphs(0)), 4)
	}},
	{name: "TestSimpleTopology/test_connect_reduces_subgraph_count", run: func(t *testing.T) {
		g := fourNodes(1)
		mustConnect(t, g, 0, "n1", "n2")
		wantInt(t, "subgraph count", len(g.Subgraphs(0)), 3)
	}},
	{name: "TestSimpleTopology/test_fan_out_reduces_subgraph", run: func(t *testing.T) {
		g := fourNodes(1)
		mustConnect(t, g, 0, "n1", "n2")
		mustConnect(t, g, 0, "n1", "n3")
		wantInt(t, "subgraph count", len(g.Subgraphs(0)), 2)
	}},
	{name: "TestSimpleTopology/test_cycle_prevention", run: func(t *testing.T) {
		g := fourNodes(1)
		mustConnect(t, g, 0, "n1", "n2")
		// n2 -> n1 would create a cycle
		wantCycle(t, g.Connect(0, "n2", "n1"))
	}},
	{name: "TestSimpleTopology/test_disconnect_increases_subgraph", run: func(t *testing.T) {
		g := New[string](1)
		for _, k := range []string{"n1", "n2", "n3"} {
			g.AddNode(k)
		}
		mustConnect(t, g, 0, "n1", "n2")
		mustConnect(t, g, 0, "n1", "n3")
		wantInt(t, "subgraph count", len(g.Subgraphs(0)), 1)
		if !g.Disconnect(0, "n1", "n2") {
			t.Fatal("Disconnect = false")
		}
		wantInt(t, "subgraph count", len(g.Subgraphs(0)), 2)
	}},
	{name: "TestSimpleTopology/test_no_multi_connect", skip: "single-connect output pins are pin machinery; see TestDuplicateEdge for node edges"},
	{name: "TestSimpleTopology/test_remove_node", run: func(t *testing.T) {
		g := fourNodes(1)
		mustConnect(t, g, 0, "n1", "n4")
		if !g.RemoveNode("n4") {
			t.Fatal("RemoveNode = false")
		}
		wantInt(t, "len(nodes)", len(g.Nodes()), 3)
	}},

	// Dual topology
	{name: "TestDualTopology/test_connect_topology_0", run: func(t *testing.T) {
		g := fourNodes(2)
		mustConnect(t, g, 0, "n1", "n2")
	}},
	{name: "TestDualTopology/test_connect_topology_1", run: func(t *testing.T) {
		g := fourNodes(2)
		mustConnect(t, g, 0, "n1", "n2")
		mustConnect(t, g, 1, "n2", "n3")
	}},
	{name: "TestDualTopology/test_ordinals_across_topologies", run: func(t *testing.T) {
		g := fourNodes(2)
		mustConnect(t, g, 0, "n1", "n2")
		mustConnect(t, g, 1, "n2", "n3")
		mustConnect(t, g, 1, "n3", "n4")
		wantInt(t, "subgraph count 0", len(g.Subgraphs(0)), 3)
		wantInt(t, "subgraph count 1", len(g.Subgraphs(1)), 2)
		for _, c := range []struct {
			k        string
			dim, ord int
		}{
			{"n1", 0, 0}, {"n1", 1, 0},
			{"n2", 0, 1}, {"n2", 1, 0},
			{"n3", 0, 0}, {"n3", 1, 1},
			{"n4", 0, 0}, {"n4", 1, 2},
		} {
			wantInt(t, c.k+" ordinal", g.Ordinal(c.dim, c.k), c.ord)
		}
	}},
	{name: "TestDualTopology/test_acyclic_failure_topology_1", run: func(t *testing.T) {
		g := fourNodes(2)
		mustConnect(t, g, 0, "n1", "n2")
		mustConnect(t, g, 1, "n2", "n3")
		mustConnect(t, g, 1, "n3", "n4")
		// n4 -> n2 on topology 1 would create cycle
		wantCycle(t, g.Connect(1, "n4", "n2"))
	}},
	{name: "TestDualTopology/test_cross_topology_non_cycle", run: func(t *testing.T) {
		g := fourNodes(2)
		mustConnect(t, g, 0, "n1", "n2")
		mustConnect(t, g, 1, "n2", "n3")
		mustConnect(t, g, 1, "n3", "n4")
		// n4 -> n1 on topology 1 is fine (different topology)
		mustConnect(t, g, 1, "n4", "n1")
		wantInt(t, "n1 ordinal", g.Ordinal(1, "n1"), 3)
		wantInt(t, "subgraph count 1", len(g.Subgraphs(1)), 1)
	}},
	{name: "TestDualTopology/test_disconnect_topology_1", run: func(t *testing.T) {
		g := fourNodes(2)
		mustConnect(t, g, 0, "n1", "n2")
		mustConnect(t, g, 1, "n2", "n3")
		mustConnect(t, g, 1, "n3", "n4")
		mustConnect(t, g, 1, "n4", "n1")
		if !g.Disconnect(1, "n3", "n4") {
			t.Fatal("Disconnect = false")
		}
		wantInt(t, "n4 ordinal", g.Ordinal(1, "n4"), 0)
		wantInt(t, "subgraph count 1", len(g.Subgraphs(1)), 2)
	}},

	// Autoclone
	{name: "TestAutoclone/test_add_autoclone_pin", skip: skipClone},
	{name: "TestAutoclone/test_clone_on_connect", skip: skipClone},
	{name: "TestAutoclone/test_remove_clone_on_disconnect", skip: skipClone},

	// Graph signals
	{name: "TestGraphSignals/test_node_added_signal", skip: skipSignal},
	{name: "TestGraphSignals/test_pins_connected_signal", skip: skipSignal},
	{name: "TestGraphSignals/test_topology_changed_signal", skip: skipSignal + "; TestLazyTopology covers recomputation"},

	// Graph queries
	{name: "TestGraphQueries/test_top_level_nodes", run: func(t *testing.T) {
		g := New[string](1)
		g.AddNode("n1")
		g.AddNode("n2")
		mustConnect(t, g, 0, "n1", "n2")
		top := g.Roots(0)
		if !slices.Contains(top, "n1") || slices.Contains(top, "n2") {
			t.Fatalf("Roots = %q", top)
		}
	}},
	{name: "TestGraphQueries/test_bottom_level_nodes", run: func(t *testing.T) {
		g := New[string](1)
		g.AddNode("n1")
		g.AddNode("n2")
		mustConnect(t, g, 0, "n1", "n2")
		bottom := g.Leaves(0)
		if !slices.Contains(bottom, "n2") || slices.Contains(bottom, "n1") {
			t.Fatalf("Leaves = %q", bottom)
		}
	}},
	{name: "TestGraphQueries/test_get_nodes_with_name", skip: "nodes are their keys, so two nodes cannot share a name"},
	{name: "TestGraphQueries/test_get_node_with_instance_id", skip: "nodes are their keys; there are no instance IDs"},
	{name: "TestGraphQueries/test_max_ordinal", run: func(t *testing.T) {
		g := chain3()
		wantInt(t, "MaxOrdinal", g.MaxOrdinal(0), 2)
	}},
	{name: "TestGraphQueries/test_descendents", run: func(t *testing.T) {
		g := chain3()
		desc := g.Descendents(0, "n1")
		if !slices.Contains(desc, "n2") || !slices.Contains(desc, "n3") {
			t.Fatalf("Descendents = %q", desc)
		}
	}},
	{name: "TestGraphQueries/test_ascendents", run: func(t *testing.T) {
		g := chain3()
		asc := g.Ascendents(0, "n3")
		if !slices.Contains(asc, "n1") || !slices.Contains(asc, "n2") {
			t.Fatalf("Ascendents = %q", asc)
		}
	}},
	{name: "TestGraphQueries/test_enable_topology_toggle", skip: "acyclicity cannot be switched off: goomba fails closed"},
}

// chain3 is n1 -> n2 -> n3.
func chain3() *Graph[string] {
	g := New[string](1)
	for _, k := range []string{"n1", "n2", "n3"} {
		g.AddNode(k)
	}
	g.Connect(0, "n1", "n2")
	g.Connect(0, "n2", "n3")
	return g
}

func TestPydaggerGraph(t *testing.T) { runPyTests(t, graphTests) }

// graphOf builds a one-dimension graph from nodes and edges given as
// "from>to" pairs.
func graphOf(nodes []string, edges ...[2]string) *Graph[string] {
	g := New[string](1)
	for _, k := range nodes {
		g.AddNode(k)
	}
	for _, e := range edges {
		if err := g.Connect(0, e[0], e[1]); err != nil {
			panic(err)
		}
	}
	return g
}

// A -> B -> C -> D
func linearChainABCD() *Graph[string] {
	return graphOf([]string{"A", "B", "C", "D"}, [2]string{"A", "B"}, [2]string{"B", "C"}, [2]string{"C", "D"})
}

//	  A
//	 / \
//	B   C
//	 \ /
//	  D
func diamondABCD() *Graph[string] {
	return graphOf([]string{"A", "B", "C", "D"},
		[2]string{"A", "B"}, [2]string{"A", "C"}, [2]string{"B", "D"}, [2]string{"C", "D"})
}

//	A   B   C
//	 \  |  /
//	    D
//	    |
//	    E
func wideConvergence() *Graph[string] {
	return graphOf([]string{"A", "B", "C", "D", "E"},
		[2]string{"A", "D"}, [2]string{"B", "D"}, [2]string{"C", "D"}, [2]string{"D", "E"})
}

// A -> B -> C plus A -> C: longest path wins, C.ordinal = 2.
func longestPath() *Graph[string] {
	return graphOf([]string{"A", "B", "C"}, [2]string{"A", "B"}, [2]string{"B", "C"}, [2]string{"A", "C"})
}

// Two separate chains: (A -> B) and (C -> D).
func disconnectedSubgraphs() *Graph[string] {
	return graphOf([]string{"A", "B", "C", "D"}, [2]string{"A", "B"}, [2]string{"C", "D"})
}

//	  A
//	 / \
//	B   C
//	|   |
//	D   E
//	 \ /
//	  F
func deepDiamond() *Graph[string] {
	return graphOf([]string{"A", "B", "C", "D", "E", "F"},
		[2]string{"A", "B"}, [2]string{"A", "C"}, [2]string{"B", "D"},
		[2]string{"C", "E"}, [2]string{"D", "F"}, [2]string{"E", "F"})
}

func wantOrdinals(t *testing.T, g *Graph[string], keys string, ords ...int) {
	t.Helper()
	for i, k := range keys {
		wantInt(t, string(k)+".ordinal", g.Ordinal(0, string(k)), ords[i])
	}
}

func desc(g *Graph[string], k string) []string { return g.Descendents(0, k) }
func asc(g *Graph[string], k string) []string  { return g.Ascendents(0, k) }

// wantSortedByOrdinal is pytest's `ordinals == sorted(ordinals)` check.
func wantSortedByOrdinal(t *testing.T, g *Graph[string], keys []string) {
	t.Helper()
	for i := 1; i < len(keys); i++ {
		if g.Ordinal(0, keys[i-1]) > g.Ordinal(0, keys[i]) {
			t.Fatalf("%q not sorted by ordinal", keys)
		}
	}
}

// Cases from pydagger tests/test_descendents.py. Its tests compare sets
// where pydagger's order is unspecified (descendents within one ordinal) and
// for most ascendent lists; each port keeps that comparison and also checks
// dag's exact order: by ordinal, then insertion (for ascendents a deliberate
// change from pydagger's insertion order, see Graph.Ascendents).
var descendentTests = []pyTest{
	// TestLinearChainDescendents: A -> B -> C -> D
	{name: "TestLinearChainDescendents/test_ordinals", run: func(t *testing.T) {
		wantOrdinals(t, linearChainABCD(), "ABCD", 0, 1, 2, 3)
	}},
	{name: "TestLinearChainDescendents/test_a_descendents", run: func(t *testing.T) {
		wantKeys(t, "A.descendents", desc(linearChainABCD(), "A"), "B", "C", "D")
	}},
	{name: "TestLinearChainDescendents/test_b_descendents", run: func(t *testing.T) {
		wantKeys(t, "B.descendents", desc(linearChainABCD(), "B"), "C", "D")
	}},
	{name: "TestLinearChainDescendents/test_c_descendents", run: func(t *testing.T) {
		wantKeys(t, "C.descendents", desc(linearChainABCD(), "C"), "D")
	}},
	{name: "TestLinearChainDescendents/test_d_descendents", run: func(t *testing.T) {
		wantKeys(t, "D.descendents", desc(linearChainABCD(), "D"))
	}},
	{name: "TestLinearChainDescendents/test_a_ascendents", run: func(t *testing.T) {
		wantKeys(t, "A.ascendents", asc(linearChainABCD(), "A"))
	}},
	{name: "TestLinearChainDescendents/test_b_ascendents", run: func(t *testing.T) {
		got := asc(linearChainABCD(), "B")
		if !slices.Contains(got, "A") || len(got) != 1 {
			t.Fatalf("B.ascendents = %q", got)
		}
	}},
	{name: "TestLinearChainDescendents/test_c_ascendents", run: func(t *testing.T) {
		got := asc(linearChainABCD(), "C")
		wantSet(t, "C.ascendents", got, "A", "B")
		wantKeys(t, "C.ascendents", got, "A", "B")
	}},
	{name: "TestLinearChainDescendents/test_d_ascendents", run: func(t *testing.T) {
		got := asc(linearChainABCD(), "D")
		wantSet(t, "D.ascendents", got, "A", "B", "C")
		wantKeys(t, "D.ascendents", got, "A", "B", "C")
	}},

	// TestDiamondDescendents
	{name: "TestDiamondDescendents/test_ordinals", run: func(t *testing.T) {
		wantOrdinals(t, diamondABCD(), "ABCD", 0, 1, 1, 2)
	}},
	{name: "TestDiamondDescendents/test_a_descendents_set", run: func(t *testing.T) {
		wantSet(t, "A.descendents", desc(diamondABCD(), "A"), "B", "C", "D")
	}},
	{name: "TestDiamondDescendents/test_a_descendents_sorted_by_ordinal", run: func(t *testing.T) {
		g := diamondABCD()
		d := desc(g, "A")
		// B and C are at ordinal 1, D at ordinal 2
		if d[len(d)-1] != "D" {
			t.Fatalf("A.descendents = %q, want D last", d)
		}
		wantSet(t, "A.descendents[:2]", d[:2], "B", "C")
		wantSortedByOrdinal(t, g, d)
		wantKeys(t, "A.descendents", d, "B", "C", "D")
	}},
	{name: "TestDiamondDescendents/test_b_descendents", run: func(t *testing.T) {
		wantKeys(t, "B.descendents", desc(diamondABCD(), "B"), "D")
	}},
	{name: "TestDiamondDescendents/test_c_descendents", run: func(t *testing.T) {
		wantKeys(t, "C.descendents", desc(diamondABCD(), "C"), "D")
	}},
	{name: "TestDiamondDescendents/test_d_descendents", run: func(t *testing.T) {
		wantKeys(t, "D.descendents", desc(diamondABCD(), "D"))
	}},
	{name: "TestDiamondDescendents/test_d_ascendents", run: func(t *testing.T) {
		got := asc(diamondABCD(), "D")
		wantSet(t, "D.ascendents", got, "A", "B", "C")
		wantKeys(t, "D.ascendents", got, "A", "B", "C")
	}},
	{name: "TestDiamondDescendents/test_b_ascendents", run: func(t *testing.T) {
		wantKeys(t, "B.ascendents", asc(diamondABCD(), "B"), "A")
	}},
	{name: "TestDiamondDescendents/test_c_ascendents", run: func(t *testing.T) {
		wantKeys(t, "C.ascendents", asc(diamondABCD(), "C"), "A")
	}},

	// TestWideConvergence
	{name: "TestWideConvergence/test_ordinals", run: func(t *testing.T) {
		wantOrdinals(t, wideConvergence(), "ABCDE", 0, 0, 0, 1, 2)
	}},
	{name: "TestWideConvergence/test_a_descendents", run: func(t *testing.T) {
		got := desc(wideConvergence(), "A")
		wantSet(t, "A.descendents", got, "D", "E")
		wantKeys(t, "A.descendents", got, "D", "E") // sorted by ordinal: d=1, e=2
	}},
	{name: "TestWideConvergence/test_b_descendents", run: func(t *testing.T) {
		wantKeys(t, "B.descendents", desc(wideConvergence(), "B"), "D", "E")
	}},
	{name: "TestWideConvergence/test_c_descendents", run: func(t *testing.T) {
		wantKeys(t, "C.descendents", desc(wideConvergence(), "C"), "D", "E")
	}},
	{name: "TestWideConvergence/test_d_descendents", run: func(t *testing.T) {
		wantKeys(t, "D.descendents", desc(wideConvergence(), "D"), "E")
	}},
	{name: "TestWideConvergence/test_e_ascendents", run: func(t *testing.T) {
		got := asc(wideConvergence(), "E")
		wantSet(t, "E.ascendents", got, "A", "B", "C", "D")
		wantKeys(t, "E.ascendents", got, "A", "B", "C", "D")
	}},

	// TestLongestPathOrdinal
	{name: "TestLongestPathOrdinal/test_ordinals", run: func(t *testing.T) {
		wantOrdinals(t, longestPath(), "ABC", 0, 1, 2) // longest path, not 1
	}},
	{name: "TestLongestPathOrdinal/test_a_descendents", run: func(t *testing.T) {
		wantKeys(t, "A.descendents", desc(longestPath(), "A"), "B", "C")
	}},
	{name: "TestLongestPathOrdinal/test_b_descendents", run: func(t *testing.T) {
		wantKeys(t, "B.descendents", desc(longestPath(), "B"), "C")
	}},
	{name: "TestLongestPathOrdinal/test_c_ascendents", run: func(t *testing.T) {
		got := asc(longestPath(), "C")
		wantSet(t, "C.ascendents", got, "A", "B")
		wantKeys(t, "C.ascendents", got, "A", "B")
	}},

	// TestDisconnectedSubgraphs
	{name: "TestDisconnectedSubgraphs/test_a_descendents_only_b", run: func(t *testing.T) {
		wantKeys(t, "A.descendents", desc(disconnectedSubgraphs(), "A"), "B")
	}},
	{name: "TestDisconnectedSubgraphs/test_c_descendents_only_d", run: func(t *testing.T) {
		wantKeys(t, "C.descendents", desc(disconnectedSubgraphs(), "C"), "D")
	}},
	{name: "TestDisconnectedSubgraphs/test_b_no_descendents", run: func(t *testing.T) {
		wantKeys(t, "B.descendents", desc(disconnectedSubgraphs(), "B"))
	}},
	{name: "TestDisconnectedSubgraphs/test_d_ascendents_only_c", run: func(t *testing.T) {
		wantKeys(t, "D.ascendents", asc(disconnectedSubgraphs(), "D"), "C")
	}},
	{name: "TestDisconnectedSubgraphs/test_b_ascendents_only_a", run: func(t *testing.T) {
		wantKeys(t, "B.ascendents", asc(disconnectedSubgraphs(), "B"), "A")
	}},
	{name: "TestDisconnectedSubgraphs/test_separate_subgraphs", run: func(t *testing.T) {
		g := disconnectedSubgraphs()
		a, b, c, d := g.Subgraph(0, "A"), g.Subgraph(0, "B"), g.Subgraph(0, "C"), g.Subgraph(0, "D")
		if a != b || c != d || a == c {
			t.Fatalf("subgraphs A=%d B=%d C=%d D=%d", a, b, c, d)
		}
	}},

	// TestDeepDiamond
	{name: "TestDeepDiamond/test_ordinals", run: func(t *testing.T) {
		wantOrdinals(t, deepDiamond(), "ABCDEF", 0, 1, 1, 2, 2, 3)
	}},
	{name: "TestDeepDiamond/test_a_descendents", run: func(t *testing.T) {
		g := deepDiamond()
		got := desc(g, "A")
		wantSet(t, "A.descendents", got, "B", "C", "D", "E", "F")
		wantSortedByOrdinal(t, g, got)
		wantKeys(t, "A.descendents", got, "B", "C", "D", "E", "F")
	}},
	{name: "TestDeepDiamond/test_b_descendents", run: func(t *testing.T) {
		got := desc(deepDiamond(), "B")
		wantSet(t, "B.descendents", got, "D", "F")
		wantKeys(t, "B.descendents", got, "D", "F") // d=2, f=3
	}},
	{name: "TestDeepDiamond/test_c_descendents", run: func(t *testing.T) {
		got := desc(deepDiamond(), "C")
		wantSet(t, "C.descendents", got, "E", "F")
		wantKeys(t, "C.descendents", got, "E", "F")
	}},
	{name: "TestDeepDiamond/test_d_descendents", run: func(t *testing.T) {
		wantKeys(t, "D.descendents", desc(deepDiamond(), "D"), "F")
	}},
	{name: "TestDeepDiamond/test_e_descendents", run: func(t *testing.T) {
		wantKeys(t, "E.descendents", desc(deepDiamond(), "E"), "F")
	}},
	{name: "TestDeepDiamond/test_f_descendents", run: func(t *testing.T) {
		wantKeys(t, "F.descendents", desc(deepDiamond(), "F"))
	}},
	{name: "TestDeepDiamond/test_f_ascendents", run: func(t *testing.T) {
		got := asc(deepDiamond(), "F")
		wantSet(t, "F.ascendents", got, "A", "B", "C", "D", "E")
		wantKeys(t, "F.ascendents", got, "A", "B", "C", "D", "E")
	}},
	{name: "TestDeepDiamond/test_d_ascendents", run: func(t *testing.T) {
		got := asc(deepDiamond(), "D")
		wantSet(t, "D.ascendents", got, "A", "B")
		wantKeys(t, "D.ascendents", got, "A", "B")
	}},
	{name: "TestDeepDiamond/test_single_subgraph", run: func(t *testing.T) {
		wantInt(t, "subgraph count", len(deepDiamond().Subgraphs(0)), 1)
	}},
}

func TestPydaggerDescendents(t *testing.T) { runPyTests(t, descendentTests) }

// TestPydaggerCaseCounts pins the number of ported pytest functions, so a
// dropped case is noticed.
func TestPydaggerCaseCounts(t *testing.T) {
	count := func(tests []pyTest) (ported, skipped int) {
		for _, tc := range tests {
			if tc.skip != "" {
				skipped++
			} else {
				ported++
			}
		}
		return
	}
	gp, gs := count(graphTests)
	dp, ds := count(descendentTests)
	if gp+gs != 32 || dp+ds != 44 {
		t.Fatalf("test_graph.py: %d+%d cases (want 32), test_descendents.py: %d+%d (want 44)", gp, gs, dp, ds)
	}
	if gp != 17 || gs != 15 || ds != 0 {
		t.Fatalf("ported/skipped: test_graph.py %d/%d, test_descendents.py %d/%d", gp, gs, dp, ds)
	}
	if n := len(optimizationScenarios); n != 36 { // 32 functions; one loops over 5 seeds
		t.Fatalf("test_optimization.py: %d scenarios, want 36", n)
	}
}
