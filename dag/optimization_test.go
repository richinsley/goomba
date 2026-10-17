package dag

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type scenario struct {
	name   string // pytest function (and seed, for the seeded loop)
	golden string // key in testdata/pydagger_golden.json
	build  func(t testing.TB) *genGraph
}

// Cases from pydagger tests/test_optimization.py (TestOptimizationCorrectness).
var optimizationScenarios = []scenario{
	// linear chains
	{"test_linear_5", "linear_5", func(t testing.TB) *genGraph { return buildLinearChain(t, 5, 1) }},
	{"test_linear_10", "linear_10", func(t testing.TB) *genGraph { return buildLinearChain(t, 10, 1) }},
	{"test_linear_50", "linear_50", func(t testing.TB) *genGraph { return buildLinearChain(t, 50, 1) }},
	{"test_linear_100", "linear_100", func(t testing.TB) *genGraph { return buildLinearChain(t, 100, 1) }},
	{"test_linear_200", "linear_200", func(t testing.TB) *genGraph { return buildLinearChain(t, 200, 1) }},
	// fan-out
	{"test_fan_out_3x3", "fan_out_3x3", func(t testing.TB) *genGraph { return buildWideFanOut(t, 3, 3, 1) }},
	{"test_fan_out_4x3", "fan_out_4x3", func(t testing.TB) *genGraph { return buildWideFanOut(t, 4, 3, 1) }},
	{"test_fan_out_3x4", "fan_out_3x4", func(t testing.TB) *genGraph { return buildWideFanOut(t, 3, 4, 1) }},
	// diamonds — the hardest case
	{"test_diamond_3x2", "diamond_3x2", func(t testing.TB) *genGraph { return buildDiamond(t, 3, 2, 1) }},
	{"test_diamond_4x3", "diamond_4x3", func(t testing.TB) *genGraph { return buildDiamond(t, 4, 3, 1) }},
	{"test_diamond_5x4", "diamond_5x4", func(t testing.TB) *genGraph { return buildDiamond(t, 5, 4, 1) }},
	{"test_diamond_5x5", "diamond_5x5", func(t testing.TB) *genGraph { return buildDiamond(t, 5, 5, 1) }},
	{"test_diamond_6x3", "diamond_6x3", func(t testing.TB) *genGraph { return buildDiamond(t, 6, 3, 1) }},
	// random DAGs with various densities
	{"test_random_20_p30", "random_20_p30_s1", func(t testing.TB) *genGraph { return buildRandomDAG(t, 20, 0.3, 1, 1) }},
	{"test_random_30_p20", "random_30_p20_s42", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 42, 1) }},
	{"test_random_50_p15", "random_50_p15_s42", func(t testing.TB) *genGraph { return buildRandomDAG(t, 50, 0.15, 42, 1) }},
	{"test_random_50_p10", "random_50_p10_s42", func(t testing.TB) *genGraph { return buildRandomDAG(t, 50, 0.10, 42, 1) }},
	{"test_random_100_p05", "random_100_p05_s42", func(t testing.TB) *genGraph { return buildRandomDAG(t, 100, 0.05, 42, 1) }},
	{"test_random_different_seeds/7", "random_30_p20_s7", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 7, 1) }},
	{"test_random_different_seeds/13", "random_30_p20_s13", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 13, 1) }},
	{"test_random_different_seeds/99", "random_30_p20_s99", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 99, 1) }},
	{"test_random_different_seeds/256", "random_30_p20_s256", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 256, 1) }},
	{"test_random_different_seeds/1024", "random_30_p20_s1024", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 1024, 1) }},
	// parallel chains
	{"test_parallel_3x5", "parallel_3x5", func(t testing.TB) *genGraph { return buildParallelChains(t, 3, 5, 1) }},
	{"test_parallel_5x20", "parallel_5x20", func(t testing.TB) *genGraph { return buildParallelChains(t, 5, 20, 1) }},
	{"test_parallel_10x10", "parallel_10x10", func(t testing.TB) *genGraph { return buildParallelChains(t, 10, 10, 1) }},
	// hourglass
	{"test_hourglass_5_5_1", "hourglass_5_5_1", func(t testing.TB) *genGraph { return buildHourglass(t, 5, 5, 1, 1) }},
	{"test_hourglass_10_10_2", "hourglass_10_10_2", func(t testing.TB) *genGraph { return buildHourglass(t, 10, 10, 2, 1) }},
	{"test_hourglass_15_15_2", "hourglass_15_15_2", func(t testing.TB) *genGraph { return buildHourglass(t, 15, 15, 2, 1) }},
	// dual topology
	{"test_diamond_4x3_dual", "diamond_4x3_dual", func(t testing.TB) *genGraph { return buildDiamond(t, 4, 3, 2) }},
	{"test_diamond_5x4_dual", "diamond_5x4_dual", func(t testing.TB) *genGraph { return buildDiamond(t, 5, 4, 2) }},
	{"test_linear_50_dual", "linear_50_dual", func(t testing.TB) *genGraph { return buildLinearChain(t, 50, 2) }},
	{"test_random_30_p20_dual", "random_30_p20_s42_dual", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 42, 2) }},
	// edge cases
	{"test_single_node", "single_node", func(t testing.TB) *genGraph { return buildNodes(t, 1) }},
	{"test_two_disconnected_nodes", "two_disconnected", func(t testing.TB) *genGraph { return buildNodes(t, 2) }},
	{"test_empty_graph", "empty", func(t testing.TB) *genGraph { return buildNodes(t, 0) }},
}

// TestPydaggerOptimization ports test_optimization.py: a recomputed
// topology must equal the first one. It also compares each graph with the
// brute-force reference model, with pydagger's own results for the same
// graph, and with the walking query path used for graphs too large for
// descendent bit sets.
func TestPydaggerOptimization(t *testing.T) {
	gd := loadGolden(t)
	for _, sc := range optimizationScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			checkScenario(t, sc, gd)
		})
	}
}

func checkScenario(t *testing.T, sc scenario, gd *golden) {
	gg := sc.build(t)
	g := gg.g

	// _assert_identical
	before := captureSnapshot(g)
	g.invalidateAll()
	after := captureSnapshot(g)
	if diffs := before.diff(after); len(diffs) > 0 {
		t.Fatalf("topology diverged:\n%s", strings.Join(diffs, "\n"))
	}

	// Same results from the walking query path.
	g.closureLimit = 0
	g.invalidateAll()
	walked := captureSnapshot(g)
	if diffs := before.diff(walked); len(diffs) > 0 {
		t.Fatalf("walking queries diverged:\n%s", strings.Join(diffs, "\n"))
	}
	g.closureLimit = defaultClosureLimit
	g.invalidateAll()

	want, ok := gd.Scenarios[sc.golden]
	if !ok {
		t.Fatalf("no golden %q", sc.golden)
	}
	checkGolden(t, gg, want)
	checkGraph(t, g, refFrom(g), "absent")
}

// Cases from pydagger tests/test_benchmark.py (TestTopologyConsistency and
// TestTopologyInvariants): self-consistency and structural invariants. Each
// case runs the consistency, golden and reference checks of
// test_optimization.py too, and then _check_invariants.
var benchmarkScenarios = []struct {
	scenario
	extra func(t *testing.T, g *Graph[string]) // the test's own assertions
}{
	{scenario{"TestTopologyConsistency/test_linear_chain_10", "linear_10", func(t testing.TB) *genGraph { return buildLinearChain(t, 10, 1) }}, nil},
	{scenario{"TestTopologyConsistency/test_linear_chain_100", "linear_100", func(t testing.TB) *genGraph { return buildLinearChain(t, 100, 1) }}, nil},
	{scenario{"TestTopologyConsistency/test_wide_fan_out_5x3", "wide_fan_out_5x3", func(t testing.TB) *genGraph { return buildWideFanOut(t, 5, 3, 1) }}, nil},
	{scenario{"TestTopologyConsistency/test_diamond_5x4", "diamond_5x4", func(t testing.TB) *genGraph { return buildDiamond(t, 5, 4, 1) }}, nil},
	{scenario{"TestTopologyConsistency/test_diamond_4x3_dual_topology", "diamond_4x3_dual", func(t testing.TB) *genGraph { return buildDiamond(t, 4, 3, 2) }}, nil},
	{scenario{"TestTopologyConsistency/test_random_dag_50", "random_50_p20_s42", func(t testing.TB) *genGraph { return buildRandomDAG(t, 50, 0.2, 42, 1) }}, nil},
	{scenario{"TestTopologyConsistency/test_random_dag_100", "random_100_p15_s123", func(t testing.TB) *genGraph { return buildRandomDAG(t, 100, 0.15, 123, 1) }}, nil},
	{scenario{"TestTopologyConsistency/test_parallel_chains_10x10", "parallel_10x10", func(t testing.TB) *genGraph { return buildParallelChains(t, 10, 10, 1) }}, nil},
	{scenario{"TestTopologyConsistency/test_hourglass_10_10_2", "hourglass_10_10_2", func(t testing.TB) *genGraph { return buildHourglass(t, 10, 10, 2, 1) }}, nil},
	{scenario{"TestTopologyInvariants/test_linear_chain", "linear_20", func(t testing.TB) *genGraph { return buildLinearChain(t, 20, 1) }},
		func(t *testing.T, g *Graph[string]) {
			// linear chain: ordinals should be 0, 1, 2, ... n-1
			for i, k := range g.Nodes() {
				if g.Ordinal(0, k) != i {
					t.Errorf("Ordinal(%s) = %d, want %d", k, g.Ordinal(0, k), i)
				}
			}
		}},
	{scenario{"TestTopologyInvariants/test_wide_fan_out", "fan_out_4x3", func(t testing.TB) *genGraph { return buildWideFanOut(t, 4, 3, 1) }}, nil},
	{scenario{"TestTopologyInvariants/test_diamond", "diamond_4x3", func(t testing.TB) *genGraph { return buildDiamond(t, 4, 3, 1) }},
		func(t *testing.T, g *Graph[string]) {
			// all nodes in same layer should have same ordinal
			for _, k := range g.Nodes() {
				layer, err := strconv.Atoi(strings.TrimPrefix(strings.Split(k, "_")[0], "L"))
				if err != nil {
					t.Fatal(err)
				}
				if g.Ordinal(0, k) != layer {
					t.Errorf("Ordinal(%s) = %d, want %d", k, g.Ordinal(0, k), layer)
				}
			}
		}},
	{scenario{"TestTopologyInvariants/test_random_dag", "random_80_p20_s99", func(t testing.TB) *genGraph { return buildRandomDAG(t, 80, 0.2, 99, 1) }}, nil},
	{scenario{"TestTopologyInvariants/test_parallel_chains", "parallel_5x10", func(t testing.TB) *genGraph { return buildParallelChains(t, 5, 10, 1) }},
		func(t *testing.T, g *Graph[string]) {
			// should have exactly 5 subgraphs
			if n := len(g.Subgraphs(0)); n != 5 {
				t.Errorf("%d subgraphs, want 5", n)
			}
		}},
	{scenario{"TestTopologyInvariants/test_hourglass", "hourglass_8_8_2", func(t testing.TB) *genGraph { return buildHourglass(t, 8, 8, 2, 1) }}, nil},
	{scenario{"TestTopologyInvariants/test_dual_topology", "diamond_3x3_dual", func(t testing.TB) *genGraph { return buildDiamond(t, 3, 3, 2) }}, nil},
}

func TestPydaggerBenchmarkChecks(t *testing.T) {
	gd := loadGolden(t)
	for _, sc := range benchmarkScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			checkScenario(t, sc.scenario, gd)
			g := sc.build(t).g
			checkInvariants(t, g)
			if sc.extra != nil {
				sc.extra(t, g)
			}
		})
	}
}

// checkInvariants is _check_invariants from test_benchmark.py.
func checkInvariants(t *testing.T, g *Graph[string]) {
	t.Helper()
	for d := 0; d < g.Dims(); d++ {
		roots := g.Roots(d)
		for _, k := range g.Nodes() {
			ord := g.Ordinal(d, k)
			if ord < 0 {
				t.Errorf("node %s has negative ordinal %d in topology %d", k, ord, d)
			}
			if slices.Contains(roots, k) && ord != 0 {
				t.Errorf("top-level node %s has ordinal %d != 0 in topology %d", k, ord, d)
			}
			desc := g.Descendents(d, k)
			for i, c := range desc {
				if g.Ordinal(d, c) <= ord {
					t.Errorf("node %s (ord %d) has descendent %s with ordinal %d in topology %d", k, ord, c, g.Ordinal(d, c), d)
				}
				if i > 0 && g.Ordinal(d, desc[i-1]) > g.Ordinal(d, c) {
					t.Errorf("descendents of %s not sorted by ordinal in topology %d", k, d)
				}
			}
		}
		for i, sub := range g.Subgraphs(d) {
			if len(sub) == 0 {
				t.Errorf("empty subgraph %d in topology %d", i, d)
			}
		}
	}
	// connected output targets must be in descendents
	for d := 0; d < g.Dims(); d++ {
		for _, n := range g.nodes {
			desc := g.Descendents(d, n.key)
			for _, c := range n.out[d] {
				if !slices.Contains(desc, c.key) {
					t.Errorf("connected child %s not in descendents of %s", c.key, n.key)
				}
			}
		}
	}
}

// TestGeneratorsMatchPydagger is a quick cross-check that the generated
// graphs' edges are pydagger's, independent of the topology checks.
func TestGeneratorsMatchPydagger(t *testing.T) {
	gd := loadGolden(t)
	for _, sc := range optimizationScenarios {
		gg := sc.build(t)
		want := gd.Scenarios[sc.golden]
		var n int
		for _, e := range gg.edges {
			if e.dim == 0 {
				n++
			}
		}
		if n != len(want.Edges) || gg.g.Len() != want.Nodes {
			t.Errorf("%s: %d nodes %d edges, pydagger %d nodes %d edges", sc.name, gg.g.Len(), n, want.Nodes, len(want.Edges))
		}
	}
	if got := fmt.Sprint(len(gd.Scenarios)); got != "44" {
		t.Errorf("golden has %s scenarios, want 44", got)
	}
}
