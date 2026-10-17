// Ported from pydagger tests/topology_snapshot.py (MIT, see NOTICE).

package dag

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
)

// nodeSnapshot is the topology of one node in one dimension.
type nodeSnapshot struct {
	ordinal, subgraph int
	descendents       []string
	top, bottom       bool
}

type snapshotKey struct {
	node string
	dim  int
}

// topologySnapshot freezes every computed piece of a graph's topology so
// two computations can be compared field by field.
type topologySnapshot struct {
	nodeCount, dims int
	maxOrdinals     []int
	subgraphCounts  []int
	top, bottom     [][]string
	nodes           map[snapshotKey]nodeSnapshot
}

func captureSnapshot(g *Graph[string]) *topologySnapshot {
	s := &topologySnapshot{nodeCount: g.Len(), dims: g.Dims(), nodes: map[snapshotKey]nodeSnapshot{}}
	for d := 0; d < g.Dims(); d++ {
		s.maxOrdinals = append(s.maxOrdinals, g.MaxOrdinal(d))
		s.subgraphCounts = append(s.subgraphCounts, len(g.Subgraphs(d)))
		roots, leaves := g.Roots(d), g.Leaves(d)
		s.top = append(s.top, roots)
		s.bottom = append(s.bottom, leaves)
		for _, k := range g.Nodes() {
			s.nodes[snapshotKey{k, d}] = nodeSnapshot{
				ordinal:     g.Ordinal(d, k),
				subgraph:    g.Subgraph(d, k),
				descendents: g.Descendents(d, k),
				top:         slices.Contains(roots, k),
				bottom:      slices.Contains(leaves, k),
			}
		}
	}
	return s
}

// diff returns human-readable differences; none means identical.
func (s *topologySnapshot) diff(o *topologySnapshot) []string {
	var diffs []string
	if s.nodeCount != o.nodeCount {
		diffs = append(diffs, fmt.Sprintf("node_count: %d vs %d", s.nodeCount, o.nodeCount))
	}
	if s.dims != o.dims {
		diffs = append(diffs, fmt.Sprintf("topology_count: %d vs %d", s.dims, o.dims))
	}
	for d := range min(s.dims, o.dims) {
		if s.maxOrdinals[d] != o.maxOrdinals[d] {
			diffs = append(diffs, fmt.Sprintf("topology %d: max_ordinal %d vs %d", d, s.maxOrdinals[d], o.maxOrdinals[d]))
		}
		if s.subgraphCounts[d] != o.subgraphCounts[d] {
			diffs = append(diffs, fmt.Sprintf("topology %d: sub_graph_count %d vs %d", d, s.subgraphCounts[d], o.subgraphCounts[d]))
		}
		if !slices.Equal(s.top[d], o.top[d]) {
			diffs = append(diffs, fmt.Sprintf("topology %d: top_level_nodes differ (len %d vs %d)", d, len(s.top[d]), len(o.top[d])))
		}
		if !slices.Equal(s.bottom[d], o.bottom[d]) {
			diffs = append(diffs, fmt.Sprintf("topology %d: bottom_level_nodes differ (len %d vs %d)", d, len(s.bottom[d]), len(o.bottom[d])))
		}
	}
	keys := make([]snapshotKey, 0, len(s.nodes)+len(o.nodes))
	for k := range s.nodes {
		keys = append(keys, k)
	}
	for k := range o.nodes {
		if _, ok := s.nodes[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b snapshotKey) int {
		return cmp.Or(cmp.Compare(a.node, b.node), cmp.Compare(a.dim, b.dim))
	})
	for _, k := range keys {
		a, aok := s.nodes[k]
		b, bok := o.nodes[k]
		label := fmt.Sprintf("node %q topo %d", k.node, k.dim)
		switch {
		case !aok:
			diffs = append(diffs, label+": missing in first snapshot")
			continue
		case !bok:
			diffs = append(diffs, label+": missing in second snapshot")
			continue
		}
		if a.ordinal != b.ordinal {
			diffs = append(diffs, fmt.Sprintf("%s: ordinal %d vs %d", label, a.ordinal, b.ordinal))
		}
		if a.subgraph != b.subgraph {
			diffs = append(diffs, fmt.Sprintf("%s: subgraph_affiliation %d vs %d", label, a.subgraph, b.subgraph))
		}
		if !slices.Equal(a.descendents, b.descendents) {
			as, bs := slices.Sorted(slices.Values(a.descendents)), slices.Sorted(slices.Values(b.descendents))
			if slices.Equal(as, bs) {
				diffs = append(diffs, label+": descendent order differs")
			} else {
				diffs = append(diffs, label+": descendent sets differ")
			}
		}
		if a.top != b.top {
			diffs = append(diffs, fmt.Sprintf("%s: is_top_level %v vs %v", label, a.top, b.top))
		}
		if a.bottom != b.bottom {
			diffs = append(diffs, fmt.Sprintf("%s: is_bottom_level %v vs %v", label, a.bottom, b.bottom))
		}
	}
	return diffs
}

// golden is testdata/pydagger_golden.json, written by
// testdata/pydagger_golden.py from pydagger itself. Node lists are indexes
// in insertion order.
//
// Descendents are sorted by (ordinal, index). pydagger sorts them by ordinal
// only, stably, starting from a Python set of id() values, so its order
// within one ordinal follows memory addresses and is not reproducible; dag
// orders them by insertion there, and the golden comparison is exact under
// that normalization.
//
// Ascendents are exactly as pydagger returns them: in insertion order
// (pydagger/node.py ascendents). dag deliberately sorts them by ordinal
// first, so checkGolden re-sorts dag's into insertion order and then
// compares them exactly; dag's own order is checked against the reference
// model.
type golden struct {
	Scenarios      map[string]goldenGraph
	PythonRandom42 []float64
}

type goldenGraph struct {
	Nodes int      `json:"nodes"`
	Edges [][2]int `json:"edges"` // dimension 0, in connection order
	Dims  []struct {
		MaxOrdinal    int     `json:"max_ordinal"`
		SubgraphCount int     `json:"subgraph_count"`
		Ordinal       []int   `json:"ordinal"`
		Subgraph      []int   `json:"subgraph"`
		Descendents   [][]int `json:"descendents"`
		Ascendents    [][]int `json:"ascendents"`
		Top           []int   `json:"top"`
		Bottom        []int   `json:"bottom"`
	} `json:"dims"`
}

func loadGolden(t testing.TB) *golden {
	t.Helper()
	data, err := os.ReadFile("testdata/pydagger_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("golden: %v", err)
	}
	gd := &golden{Scenarios: map[string]goldenGraph{}}
	for name, msg := range raw {
		if name == "_python_random_42" {
			if err := json.Unmarshal(msg, &gd.PythonRandom42); err != nil {
				t.Fatalf("golden %s: %v", name, err)
			}
			continue
		}
		var gg goldenGraph
		if err := json.Unmarshal(msg, &gg); err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		gd.Scenarios[name] = gg
	}
	return gd
}

// checkGolden compares gg with what pydagger computed for the same graph.
func checkGolden(t testing.TB, gg *genGraph, want goldenGraph) {
	t.Helper()
	g := gg.g
	nodes := g.Nodes()
	if len(nodes) != want.Nodes {
		t.Fatalf("golden: %d nodes, pydagger %d", len(nodes), want.Nodes)
	}
	pos := make(map[string]int, len(nodes))
	for i, k := range nodes {
		pos[k] = i
	}
	idx := func(keys []string) []int {
		out := make([]int, len(keys))
		for i, k := range keys {
			out[i] = pos[k]
		}
		return out
	}
	var edges [][2]int
	for _, e := range gg.edges {
		if e.dim == 0 {
			edges = append(edges, [2]int{pos[e.from], pos[e.to]})
		}
	}
	if !slices.Equal(edges, want.Edges) {
		t.Fatalf("golden: generated edges differ from pydagger's:\n got %v\nwant %v", edges, want.Edges)
	}
	if len(want.Dims) != g.Dims() {
		t.Fatalf("golden: %d dims, pydagger %d", g.Dims(), len(want.Dims))
	}
	for d, w := range want.Dims {
		if got := g.MaxOrdinal(d); got != w.MaxOrdinal {
			t.Errorf("dim %d: MaxOrdinal %d, pydagger %d", d, got, w.MaxOrdinal)
		}
		if got := len(g.Subgraphs(d)); got != w.SubgraphCount {
			t.Errorf("dim %d: %d subgraphs, pydagger %d", d, got, w.SubgraphCount)
		}
		if got := idx(g.Roots(d)); !slices.Equal(got, w.Top) {
			t.Errorf("dim %d: Roots %v, pydagger %v", d, got, w.Top)
		}
		if got := idx(g.Leaves(d)); !slices.Equal(got, w.Bottom) {
			t.Errorf("dim %d: Leaves %v, pydagger %v", d, got, w.Bottom)
		}
		for i, k := range nodes {
			if got := g.Ordinal(d, k); got != w.Ordinal[i] {
				t.Errorf("dim %d: Ordinal(%s) %d, pydagger %d", d, k, got, w.Ordinal[i])
			}
			if got := g.Subgraph(d, k); got != w.Subgraph[i] {
				t.Errorf("dim %d: Subgraph(%s) %d, pydagger %d", d, k, got, w.Subgraph[i])
			}
			if got := idx(g.Descendents(d, k)); !slices.Equal(got, w.Descendents[i]) {
				t.Errorf("dim %d: Descendents(%s) %v, pydagger %v", d, k, got, w.Descendents[i])
			}
			if got := slices.Sorted(slices.Values(idx(g.Ascendents(d, k)))); !slices.Equal(got, w.Ascendents[i]) {
				t.Errorf("dim %d: Ascendents(%s) in insertion order %v, pydagger %v", d, k, got, w.Ascendents[i])
			}
		}
	}
}
