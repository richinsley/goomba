// Ported from pydagger tests/graph_generators.py (MIT, see NOTICE).

package dag

import (
	"fmt"
	"testing"
)

// pyRandom is MT19937 (Matsumoto and Nishimura, 1998) seeded the way
// CPython's random.Random(seed) seeds it for a seed below 2³², so that
// buildRandomDAG draws exactly the edges pydagger's build_random_dag draws.
type pyRandom struct {
	mt  [624]uint32
	idx int
}

func newPyRandom(seed uint32) *pyRandom {
	r := &pyRandom{}
	// init_genrand(19650218), then init_by_array([seed]).
	r.mt[0] = 19650218
	for i := 1; i < 624; i++ {
		r.mt[i] = 1812433253*(r.mt[i-1]^(r.mt[i-1]>>30)) + uint32(i)
	}
	i, j := 1, 0
	key := []uint32{seed}
	for k := max(624, len(key)); k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= 624 {
			r.mt[0] = r.mt[623]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k := 623; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= 624 {
			r.mt[0] = r.mt[623]
			i = 1
		}
	}
	r.mt[0] = 0x80000000
	r.idx = 624
	return r
}

func (r *pyRandom) uint32() uint32 {
	if r.idx >= 624 {
		for k := 0; k < 624; k++ {
			y := r.mt[k]&0x80000000 | r.mt[(k+1)%624]&0x7fffffff
			v := r.mt[(k+397)%624] ^ y>>1
			if y&1 != 0 {
				v ^= 0x9908b0df
			}
			r.mt[k] = v
		}
		r.idx = 0
	}
	y := r.mt[r.idx]
	r.idx++
	y ^= y >> 11
	y ^= y << 7 & 0x9d2c5680
	y ^= y << 15 & 0xefc60000
	y ^= y >> 18
	return y
}

// random is CPython's random.random(): 53 bits from two draws.
func (r *pyRandom) random() float64 {
	a, b := r.uint32()>>5, r.uint32()>>6
	return (float64(a)*67108864 + float64(b)) / 9007199254740992
}

// genGraph is a generated graph and the edges it was built from, as
// (from, to, dim) in the order they were connected.
type genGraph struct {
	g     *Graph[string]
	edges []genEdge
}

type genEdge struct {
	from, to string
	dim      int
}

func (gg *genGraph) add(t testing.TB, name string) string {
	t.Helper()
	if !gg.g.AddNode(name) {
		t.Fatalf("generator: AddNode(%q) = false", name)
	}
	return name
}

func (gg *genGraph) connect(t testing.TB, from, to string) {
	t.Helper()
	for d := 0; d < gg.g.Dims(); d++ {
		if err := gg.g.Connect(d, from, to); err != nil {
			t.Fatalf("generator: %v", err)
		}
		gg.edges = append(gg.edges, genEdge{from, to, d})
	}
}

// buildLinearChain is A -> B -> C -> ... -> N: max ordinal n-1, one
// subgraph.
func buildLinearChain(t testing.TB, n, dims int) *genGraph {
	gg := &genGraph{g: New[string](dims)}
	nodes := make([]string, n)
	for i := range nodes {
		nodes[i] = gg.add(t, fmt.Sprintf("chain_%d", i))
	}
	for i := 0; i < n-1; i++ {
		gg.connect(t, nodes[i], nodes[i+1])
	}
	return gg
}

// buildWideFanOut has one root fanning out to width children, each of
// which fans out again, depth levels in all.
func buildWideFanOut(t testing.TB, width, depth, dims int) *genGraph {
	gg := &genGraph{g: New[string](dims)}
	current := []string{gg.add(t, "root")}
	for d := 1; d < depth; d++ {
		var next []string
		for _, parent := range current {
			for range width {
				child := gg.add(t, fmt.Sprintf("d%d_w%d", d, len(next)))
				gg.connect(t, parent, child)
				next = append(next, child)
			}
		}
		current = next
	}
	return gg
}

// buildDiamond has layers of width nodes, each connected to every node of
// the next layer.
func buildDiamond(t testing.TB, layers, width, dims int) *genGraph {
	gg := &genGraph{g: New[string](dims)}
	var prev []string
	for layer := range layers {
		var cur []string
		for w := range width {
			cur = append(cur, gg.add(t, fmt.Sprintf("L%d_W%d", layer, w)))
		}
		for _, parent := range prev {
			for _, child := range cur {
				gg.connect(t, parent, child)
			}
		}
		prev = cur
	}
	return gg
}

// buildRandomDAG adds edge i -> j, for every i < j, with probability p,
// drawing from CPython's generator seeded with seed.
func buildRandomDAG(t testing.TB, n int, p float64, seed uint32, dims int) *genGraph {
	rng := newPyRandom(seed)
	var planned [][2]int
	for i := range n {
		for j := i + 1; j < n; j++ {
			if rng.random() < p {
				planned = append(planned, [2]int{i, j})
			}
		}
	}
	gg := &genGraph{g: New[string](dims)}
	nodes := make([]string, n)
	for i := range nodes {
		nodes[i] = gg.add(t, fmt.Sprintf("rand_%d", i))
	}
	for _, e := range planned {
		gg.connect(t, nodes[e[0]], nodes[e[1]])
	}
	return gg
}

// buildParallelChains is nChains independent chains.
func buildParallelChains(t testing.TB, nChains, length, dims int) *genGraph {
	gg := &genGraph{g: New[string](dims)}
	for c := range nChains {
		var chain []string
		for i := range length {
			chain = append(chain, gg.add(t, fmt.Sprintf("c%d_n%d", c, i)))
		}
		for i := 0; i < length-1; i++ {
			gg.connect(t, chain[i], chain[i+1])
		}
	}
	return gg
}

// buildHourglass is a wide top, a narrow neck and a wide bottom, fully
// connected layer to layer.
func buildHourglass(t testing.TB, top, bottom, neck, dims int) *genGraph {
	gg := &genGraph{g: New[string](dims)}
	var tops, necks, bots []string
	for i := range top {
		tops = append(tops, gg.add(t, fmt.Sprintf("top_%d", i)))
	}
	for i := range neck {
		necks = append(necks, gg.add(t, fmt.Sprintf("neck_%d", i)))
	}
	for i := range bottom {
		bots = append(bots, gg.add(t, fmt.Sprintf("bot_%d", i)))
	}
	for _, a := range tops {
		for _, b := range necks {
			gg.connect(t, a, b)
		}
	}
	for _, a := range necks {
		for _, b := range bots {
			gg.connect(t, a, b)
		}
	}
	return gg
}

// buildNodes is n nodes and no edges (pydagger's single-node, two-node and
// empty-graph edge cases).
func buildNodes(t testing.TB, n int) *genGraph {
	gg := &genGraph{g: New[string](1)}
	for i := range n {
		gg.add(t, fmt.Sprintf("n%d", i))
	}
	return gg
}

func TestPyRandomMatchesCPython(t *testing.T) {
	golden := loadGolden(t)
	r := newPyRandom(42)
	for i, want := range golden.PythonRandom42 {
		if got := r.random(); got != want {
			t.Fatalf("random.Random(42) draw %d = %v, CPython %v", i, got, want)
		}
	}
}
