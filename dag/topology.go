// Ported from pydagger pydagger/graph.py (DaggerGraph.calculate_topology and
// _UnionFind) and pydagger/node.py (ordinal, descendents, ascendents) (MIT,
// see NOTICE).

package dag

import (
	"fmt"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
)

// dimState is one dimension's cached topology. A topology is published
// through topo only once it is complete, so a reader never sees a half-built
// one and a reader of a current topology takes no lock; mu makes concurrent
// readers of a stale dimension compute it once, without holding up readers
// of other dimensions.
type dimState struct {
	mu   sync.Mutex
	topo atomic.Pointer[topology]
}

// topology is one dimension's computed state. It is immutable once built,
// so queries may share it without locking.
type topology struct {
	ordinal    []int // by node position: longest path from a root
	maxOrdinal int
	// Rank orders nodes by (ordinal, position). Since every edge goes to a
	// higher ordinal, rank order is also a topological order.
	rank       []int // by node position
	byRank     []int // node position of each rank
	layerStart []int // first rank of each ordinal, then len(nodes)
	subgraph   []int // by node position: component index
	subgraphs  int
	// desc holds, when the graph is at most closureLimit nodes, one bit set
	// of descendent ranks per node, rank-major: row r is desc[r*words:][:words].
	desc  []uint64
	words int
}

// topology returns dimension dim's topology, computing it if it is stale.
func (g *Graph[K]) topology(dim int) *topology {
	g.checkDim(dim)
	s := &g.state[dim]
	if t := s.topo.Load(); t != nil {
		return t
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.topo.Load(); t != nil {
		return t // another reader computed it meanwhile
	}
	t := g.compute(dim)
	s.topo.Store(t)
	return t
}

// invalidate marks dimension dim's topology stale.
func (g *Graph[K]) invalidate(dim int) {
	g.state[dim].topo.Store(nil)
}

// invalidateAll marks every dimension's topology stale.
func (g *Graph[K]) invalidateAll() {
	for d := range g.state {
		g.state[d].topo.Store(nil)
	}
}

// compute runs pydagger's three passes over dimension dim: Kahn's algorithm
// for ordinals, a reverse topological sweep for descendents and union-find
// for components. It is O(N + E) plus O(N·E/64) for the descendent sets.
func (g *Graph[K]) compute(dim int) *topology {
	nodes := g.nodes
	n := len(nodes)
	t := &topology{
		ordinal:  make([]int, n),
		rank:     make([]int, n),
		byRank:   make([]int, n),
		subgraph: make([]int, n),
	}
	if n == 0 {
		t.layerStart = []int{0}
		return t
	}

	// Pass 1: ordinals as the longest path from any root, by Kahn's BFS.
	indegree := make([]int, n)
	queue := make([]int, 0, n)
	for p, nd := range nodes {
		indegree[p] = len(nd.in[dim])
		if indegree[p] == 0 {
			queue = append(queue, p)
		}
	}
	for head := 0; head < len(queue); head++ {
		p := queue[head]
		next := t.ordinal[p] + 1
		t.maxOrdinal = max(t.maxOrdinal, t.ordinal[p])
		for _, c := range nodes[p].out[dim] {
			t.ordinal[c.pos] = max(t.ordinal[c.pos], next)
			indegree[c.pos]--
			if indegree[c.pos] == 0 {
				queue = append(queue, c.pos)
			}
		}
	}
	if len(queue) != n {
		// Connect never admits a cycle, so this is a bug in this package.
		panic(fmt.Sprintf("dag: internal error: dimension %d has a cycle", dim))
	}

	// Layers: a counting sort by ordinal keeps insertion order within one.
	t.layerStart = make([]int, t.maxOrdinal+2)
	for _, o := range t.ordinal {
		t.layerStart[o+1]++
	}
	for o := 1; o < len(t.layerStart); o++ {
		t.layerStart[o] += t.layerStart[o-1]
	}
	fill := slices.Clone(t.layerStart[:t.maxOrdinal+1])
	for p, o := range t.ordinal {
		r := fill[o]
		fill[o]++
		t.rank[p] = r
		t.byRank[r] = p
	}

	// Pass 2: descendents bottom-up, in reverse rank order, so each node is
	// done once and its children are done before it. A child's descendents
	// all rank above it, so the union starts at the child's word.
	if n <= g.closureLimit {
		t.words = (n + 63) / 64
		t.desc = make([]uint64, n*t.words)
		for r := n - 1; r >= 0; r-- {
			row := t.row(r)
			for _, c := range nodes[t.byRank[r]].out[dim] {
				cr := t.rank[c.pos]
				row[cr/64] |= 1 << (cr % 64)
				crow := t.row(cr)
				for w := cr / 64; w < t.words; w++ {
					row[w] |= crow[w]
				}
			}
		}
	}

	// Pass 3: components by union-find, numbered in order of first node.
	uf := newUnionFind(n)
	for p, nd := range nodes {
		for _, c := range nd.out[dim] {
			uf.union(p, c.pos)
		}
	}
	index := make([]int, n) // by root position: component index + 1
	for p := range nodes {
		root := uf.find(p)
		if index[root] == 0 {
			t.subgraphs++
			index[root] = t.subgraphs
		}
		t.subgraph[p] = index[root] - 1
	}
	return t
}

// row returns the descendent bit set of the node with rank r.
func (t *topology) row(r int) []uint64 {
	return t.desc[r*t.words : (r+1)*t.words]
}

// Ordinal returns k's ordinal in dim: the number of edges on the longest
// path from a root to k. Roots have ordinal 0; along every edge the ordinal
// rises, so nodes with equal ordinals never depend on each other. It returns
// -1 if k is not a node, and panics if dim is not in [0, Dims()).
func (g *Graph[K]) Ordinal(dim int, k K) int {
	t := g.topology(dim)
	n, ok := g.index[k]
	if !ok {
		return -1
	}
	return t.ordinal[n.pos]
}

// MaxOrdinal returns the highest ordinal in dim; it is 0 for an empty graph,
// as in pydagger. It panics if dim is not in [0, Dims()).
func (g *Graph[K]) MaxOrdinal(dim int) int {
	return g.topology(dim).maxOrdinal
}

// Layers returns the nodes of dim grouped by ordinal: layer i holds the
// nodes with ordinal i, in the order they were added. Running the layers in
// order, the nodes of one layer concurrently, respects every edge; running
// them in reverse respects every edge backwards. An empty graph has no
// layers. It panics if dim is not in [0, Dims()).
func (g *Graph[K]) Layers(dim int) [][]K {
	t := g.topology(dim)
	if len(g.nodes) == 0 {
		return nil
	}
	layers := make([][]K, t.maxOrdinal+1)
	for o := range layers {
		layers[o] = g.keysOfRanks(t, t.layerStart[o], t.layerStart[o+1])
	}
	return layers
}

// Descendents returns every node reachable from k in dim, not including k,
// sorted by ordinal and then by the order the nodes were added. It returns
// nil if k is not a node or has no descendents, and panics if dim is not in
// [0, Dims()).
//
// Up to 16,384 nodes it reads the descendent bit set the topology pass
// built, in time proportional to the result plus N/64. Above that, each
// call walks the edges: O(N + E) time and O(N) scratch memory per call.
func (g *Graph[K]) Descendents(dim int, k K) []K {
	t := g.topology(dim)
	n, ok := g.index[k]
	if !ok {
		return nil
	}
	if t.desc == nil {
		return g.walk(t, n, []int{dim}, false)
	}
	row := t.row(t.rank[n.pos])
	count := 0
	for _, w := range row {
		count += bits.OnesCount64(w)
	}
	if count == 0 {
		return nil
	}
	keys := make([]K, 0, count)
	for i, w := range row {
		for w != 0 {
			r := i*64 + bits.TrailingZeros64(w)
			keys = append(keys, g.nodes[t.byRank[r]].key)
			w &= w - 1
		}
	}
	return keys
}

// Ascendents returns every node from which k is reachable in dim, not
// including k, sorted by ordinal and then by the order the nodes were added.
// It returns nil if k is not a node or has no ascendents, and panics if dim
// is not in [0, Dims()).
//
// The order deviates from pydagger, whose ascendents keep the order the
// nodes were added in; sorting by ordinal first makes the result a valid
// topological order (every node after all of its own ascendents), like
// Descendents.
//
// Up to 16,384 nodes it tests one bit per node in a lower layer, O(N).
// Above that, each call walks the edges backwards: O(N + E) time and O(N)
// scratch memory per call.
func (g *Graph[K]) Ascendents(dim int, k K) []K {
	t := g.topology(dim)
	n, ok := g.index[k]
	if !ok {
		return nil
	}
	if t.desc == nil {
		return g.walk(t, n, []int{dim}, true)
	}
	// Only nodes in lower layers can reach k.
	kr := t.rank[n.pos]
	word, bit := kr/64, uint64(1)<<(kr%64)
	var keys []K
	for r := 0; r < t.layerStart[t.ordinal[n.pos]]; r++ {
		if t.desc[r*t.words+word]&bit != 0 {
			keys = append(keys, g.nodes[t.byRank[r]].key)
		}
	}
	return keys
}

// DescendentsAcross returns every node reachable from k by edges of any of
// dims, not including k even if a path across dimensions returns to it. The
// result is sorted by ordinal in dims[0], then by the order the nodes were
// added, so DescendentsAcross([]int{d}, k) equals Descendents(d, k). It
// returns nil if dims is empty, k is not a node or nothing is reachable, and
// panics if any of dims is not in [0, Dims()). Each call walks the edges:
// O(N + E) time and O(N) scratch memory.
func (g *Graph[K]) DescendentsAcross(dims []int, k K) []K {
	for _, d := range dims {
		g.checkDim(d)
	}
	if len(dims) == 0 {
		return nil
	}
	t := g.topology(dims[0])
	n, ok := g.index[k]
	if !ok {
		return nil
	}
	return g.walk(t, n, dims, false)
}

// Subgraphs returns the connected components of dim, ignoring edge
// direction. Components are numbered, and listed, in the order of their
// first-added node; each lists its nodes in the order they were added. It
// panics if dim is not in [0, Dims()).
func (g *Graph[K]) Subgraphs(dim int) [][]K {
	t := g.topology(dim)
	if t.subgraphs == 0 {
		return nil
	}
	subgraphs := make([][]K, t.subgraphs)
	for p, n := range g.nodes {
		s := t.subgraph[p]
		subgraphs[s] = append(subgraphs[s], n.key)
	}
	return subgraphs
}

// Subgraph returns the index in Subgraphs(dim) of k's component, or -1 if k
// is not a node. It panics if dim is not in [0, Dims()).
func (g *Graph[K]) Subgraph(dim int, k K) int {
	t := g.topology(dim)
	n, ok := g.index[k]
	if !ok {
		return -1
	}
	return t.subgraph[n.pos]
}

// walk returns the nodes reachable from n over the out-edges (or, if up,
// the in-edges) of dims, not including n, sorted by rank in t. It is the
// query path for graphs too large for descendent bit sets, and for
// reachability across dimensions. It allocates its own scratch, so queries
// can run concurrently.
func (g *Graph[K]) walk(t *topology, n *node[K], dims []int, up bool) []K {
	seen := make([]bool, len(g.nodes))
	seen[n.pos] = true
	queue := []*node[K]{n}
	var ranks []int
	for i := 0; i < len(queue); i++ {
		u := queue[i]
		for _, d := range dims {
			next := u.out[d]
			if up {
				next = u.in[d]
			}
			for _, w := range next {
				if !seen[w.pos] {
					seen[w.pos] = true
					queue = append(queue, w)
					ranks = append(ranks, t.rank[w.pos])
				}
			}
		}
	}
	if len(ranks) == 0 {
		return nil
	}
	slices.Sort(ranks)
	keys := make([]K, len(ranks))
	for i, r := range ranks {
		keys[i] = g.nodes[t.byRank[r]].key
	}
	return keys
}

// keysOfRanks returns the keys of ranks [lo, hi) in rank order.
func (g *Graph[K]) keysOfRanks(t *topology, lo, hi int) []K {
	keys := make([]K, hi-lo)
	for i := range keys {
		keys[i] = g.nodes[t.byRank[lo+i]].key
	}
	return keys
}

// unionFind is a disjoint-set forest with union by rank and path
// compression, over the integers [0, n).
type unionFind struct {
	parent []int
	rank   []uint8
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), rank: make([]uint8, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (uf *unionFind) find(x int) int {
	r := x
	for uf.parent[r] != r {
		r = uf.parent[r]
	}
	for uf.parent[x] != r {
		uf.parent[x], x = r, uf.parent[x]
	}
	return r
}

func (uf *unionFind) union(a, b int) {
	ra, rb := uf.find(a), uf.find(b)
	if ra == rb {
		return
	}
	if uf.rank[ra] < uf.rank[rb] {
		ra, rb = rb, ra
	}
	uf.parent[rb] = ra
	if uf.rank[ra] == uf.rank[rb] {
		uf.rank[ra]++
	}
}
