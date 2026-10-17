package dag

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// refGraph is a brute-force model of Graph used to check it: every query is
// recomputed from scratch by the simplest algorithm that is plainly right.
type refGraph[K comparable] struct {
	dims  int
	nodes []K
	out   []map[K][]K // per dimension; targets in the order edges were added
	ords  []map[K]int // per dimension; memoized ordinals, nil when stale
}

func newRef[K comparable](dims int) *refGraph[K] {
	r := &refGraph[K]{dims: dims, out: make([]map[K][]K, dims), ords: make([]map[K]int, dims)}
	for d := range r.out {
		r.out[d] = make(map[K][]K)
	}
	return r
}

func (r *refGraph[K]) changed() { clear(r.ords) }

func (r *refGraph[K]) has(k K) bool { return slices.Contains(r.nodes, k) }

func (r *refGraph[K]) indexOf(k K) int { return slices.Index(r.nodes, k) }

func (r *refGraph[K]) addNode(k K) bool {
	if r.has(k) {
		return false
	}
	r.nodes = append(r.nodes, k)
	r.changed()
	return true
}

func (r *refGraph[K]) removeNode(k K) bool {
	i := r.indexOf(k)
	if i < 0 {
		return false
	}
	r.nodes = slices.Delete(r.nodes, i, i+1)
	for d := range r.out {
		delete(r.out[d], k)
		for _, u := range r.nodes {
			r.out[d][u] = slices.DeleteFunc(r.out[d][u], func(v K) bool { return v == k })
		}
	}
	r.changed()
	return true
}

func (r *refGraph[K]) hasEdge(d int, a, b K) bool { return slices.Contains(r.out[d][a], b) }

// path returns a shortest path a → … → b in d, found by a breadth-first
// search that follows edges in the order they were added, or nil.
func (r *refGraph[K]) path(d int, a, b K) []K {
	if a == b {
		return []K{a}
	}
	parent := map[K]K{}
	seen := map[K]bool{a: true}
	queue := []K{a}
	for i := 0; i < len(queue); i++ {
		u := queue[i]
		for _, w := range r.out[d][u] {
			if seen[w] {
				continue
			}
			seen[w] = true
			parent[w] = u
			if w == b {
				p := []K{b}
				for x := b; x != a; {
					x = parent[x]
					p = append(p, x)
				}
				slices.Reverse(p)
				return p
			}
			queue = append(queue, w)
		}
	}
	return nil
}

// connect applies Connect's contract: unknown endpoints and cycles change
// nothing; it returns the cycle path, if any, and whether an edge was added.
func (r *refGraph[K]) connect(d int, a, b K) (cycle []K, added bool) {
	if !r.has(a) || !r.has(b) || r.hasEdge(d, a, b) {
		return nil, false
	}
	if p := r.path(d, b, a); p != nil {
		return p, false
	}
	r.out[d][a] = append(r.out[d][a], b)
	r.changed()
	return nil, true
}

// connectAll applies ConnectAll's contract: Connect on each edge in turn,
// all or nothing. It returns the index of the edge whose Connect would fail
// (-1 if none), with that failure's cycle path and endpoint knownness.
func (r *refGraph[K]) connectAll(d int, edges [][2]K) (fail int, cycle []K, aKnown, bKnown bool) {
	saved := make(map[K][]K, len(r.out[d]))
	for k, v := range r.out[d] {
		saved[k] = slices.Clone(v)
	}
	for i, e := range edges {
		aKnown, bKnown = r.has(e[0]), r.has(e[1])
		cycle, _ = r.connect(d, e[0], e[1])
		if !aKnown || !bKnown || cycle != nil {
			r.out[d] = saved
			r.changed()
			return i, cycle, aKnown, bKnown
		}
	}
	return -1, nil, true, true
}

func (r *refGraph[K]) disconnect(d int, a, b K) bool {
	if !r.hasEdge(d, a, b) {
		return false
	}
	r.out[d][a] = slices.DeleteFunc(r.out[d][a], func(v K) bool { return v == b })
	r.changed()
	return true
}

// ordinal is the longest path from a root to k: the longest path to any
// parent, plus one.
func (r *refGraph[K]) ordinal(d int, k K) int {
	if r.ords[d] == nil {
		r.ords[d] = map[K]int{}
	}
	if o, ok := r.ords[d][k]; ok {
		return o
	}
	best := 0
	for _, u := range r.nodes {
		if r.hasEdge(d, u, k) {
			best = max(best, r.ordinal(d, u)+1)
		}
	}
	r.ords[d][k] = best
	return best
}

// reach returns the set reachable from k over dims, without k.
func (r *refGraph[K]) reach(dims []int, k K) map[K]bool {
	seen := map[K]bool{k: true}
	stack := []K{k}
	for len(stack) > 0 {
		u := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range dims {
			for _, w := range r.out[d][u] {
				if !seen[w] {
					seen[w] = true
					stack = append(stack, w)
				}
			}
		}
	}
	delete(seen, k)
	return seen
}

// sorted lists set in (ordinal in d, insertion) order.
func (r *refGraph[K]) sorted(d int, set map[K]bool) []K {
	var keys []K
	for _, k := range r.nodes {
		if set[k] {
			keys = append(keys, k)
		}
	}
	slices.SortStableFunc(keys, func(a, b K) int { return cmp.Compare(r.ordinal(d, a), r.ordinal(d, b)) })
	return keys
}

func (r *refGraph[K]) descendents(d int, k K) []K { return r.sorted(d, r.reach([]int{d}, k)) }

func (r *refGraph[K]) descendentsAcross(dims []int, k K) []K {
	if len(dims) == 0 || !r.has(k) {
		return nil
	}
	return r.sorted(dims[0], r.reach(dims, k))
}

func (r *refGraph[K]) layers(d int) [][]K {
	var layers [][]K
	for _, k := range r.nodes {
		o := r.ordinal(d, k)
		for len(layers) <= o {
			layers = append(layers, nil)
		}
		layers[o] = append(layers[o], k)
	}
	return layers
}

func (r *refGraph[K]) maxOrdinal(d int) int {
	m := 0
	for _, k := range r.nodes {
		m = max(m, r.ordinal(d, k))
	}
	return m
}

// subgraphs are the undirected components, in order of first node.
func (r *refGraph[K]) subgraphs(d int) [][]K {
	comp := map[K]int{}
	var subs [][]K
	for _, k := range r.nodes {
		if _, ok := comp[k]; ok {
			continue
		}
		id := len(subs)
		comp[k] = id
		stack := []K{k}
		for len(stack) > 0 {
			u := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, w := range r.nodes {
				if _, ok := comp[w]; !ok && (r.hasEdge(d, u, w) || r.hasEdge(d, w, u)) {
					comp[w] = id
					stack = append(stack, w)
				}
			}
		}
		var members []K
		for _, w := range r.nodes {
			if c, ok := comp[w]; ok && c == id {
				members = append(members, w)
			}
		}
		subs = append(subs, members)
	}
	return subs
}

func (r *refGraph[K]) roots(d int) []K {
	var keys []K
	for _, k := range r.nodes {
		root := true
		for _, u := range r.nodes {
			if r.hasEdge(d, u, k) {
				root = false
			}
		}
		if root {
			keys = append(keys, k)
		}
	}
	return keys
}

func (r *refGraph[K]) leaves(d int) []K {
	var keys []K
	for _, k := range r.nodes {
		if len(r.out[d][k]) == 0 {
			keys = append(keys, k)
		}
	}
	return keys
}

// checkGraph compares every query of g with the reference model, plus the
// invariants the fuzz target promises, and the internal bookkeeping. absent
// must not be a node.
func checkGraph[K comparable](t testing.TB, g *Graph[K], ref *refGraph[K], absent K) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf(format, args...)
	}
	if got := g.Nodes(); !slices.Equal(got, ref.nodes) {
		fail("Nodes() = %v, want %v", got, ref.nodes)
	}
	if g.Len() != len(ref.nodes) {
		fail("Len() = %d, want %d", g.Len(), len(ref.nodes))
	}
	checkInternals(t, g)
	all := make([]int, g.Dims())
	for d := range all {
		all[d] = d
	}
	for d := 0; d < g.Dims(); d++ {
		if got, want := g.Roots(d), ref.roots(d); !slices.Equal(got, want) {
			fail("Roots(%d) = %v, want %v", d, got, want)
		}
		if got, want := g.Leaves(d), ref.leaves(d); !slices.Equal(got, want) {
			fail("Leaves(%d) = %v, want %v", d, got, want)
		}
		if got, want := g.MaxOrdinal(d), ref.maxOrdinal(d); got != want {
			fail("MaxOrdinal(%d) = %d, want %d", d, got, want)
		}
		if got, want := g.Layers(d), ref.layers(d); !equal2(got, want) {
			fail("Layers(%d) = %v, want %v", d, got, want)
		}
		subs := ref.subgraphs(d)
		if got := g.Subgraphs(d); !equal2(got, subs) {
			fail("Subgraphs(%d) = %v, want %v", d, got, subs)
		}
		for i, sub := range subs {
			for _, k := range sub {
				if got := g.Subgraph(d, k); got != i {
					fail("Subgraph(%d, %v) = %d, want %d", d, k, got, i)
				}
			}
		}
		reach := make(map[K]map[K]bool, len(ref.nodes))
		for _, k := range ref.nodes {
			reach[k] = ref.reach([]int{d}, k)
		}
		for _, k := range ref.nodes {
			ok := ref.ordinal(d, k)
			if got := g.Ordinal(d, k); got != ok {
				fail("Ordinal(%d, %v) = %d, want %d", d, k, got, ok)
			}
			for _, c := range ref.out[d][k] {
				if g.Ordinal(d, c) <= ok {
					fail("edge %v -> %v in %d: ordinals %d -> %d do not rise", k, c, d, ok, g.Ordinal(d, c))
				}
			}
			if got, want := g.Descendents(d, k), ref.sorted(d, reach[k]); !slices.Equal(got, want) {
				fail("Descendents(%d, %v) = %v, want %v", d, k, got, want)
			}
			ascendents := map[K]bool{}
			for _, u := range ref.nodes {
				if reach[u][k] {
					ascendents[u] = true
				}
			}
			if got, want := g.Ascendents(d, k), ref.sorted(d, ascendents); !slices.Equal(got, want) {
				fail("Ascendents(%d, %v) = %v, want %v", d, k, got, want)
			}
			// Single-dimension reachability must agree with Descendents.
			if got, want := g.DescendentsAcross([]int{d}, k), g.Descendents(d, k); !slices.Equal(got, want) {
				fail("DescendentsAcross([%d], %v) = %v, Descendents = %v", d, k, got, want)
			}
		}
		if g.Ordinal(d, absent) != -1 || g.Subgraph(d, absent) != -1 ||
			g.Descendents(d, absent) != nil || g.Ascendents(d, absent) != nil {
			fail("queries for absent node %v in %d are not -1/nil", absent, d)
		}
	}
	rev := slices.Clone(all)
	slices.Reverse(rev)
	for _, dims := range [][]int{all, rev, nil} {
		for _, k := range ref.nodes {
			if got, want := g.DescendentsAcross(dims, k), ref.descendentsAcross(dims, k); !slices.Equal(got, want) {
				fail("DescendentsAcross(%v, %v) = %v, want %v", dims, k, got, want)
			}
		}
		if g.DescendentsAcross(dims, absent) != nil {
			fail("DescendentsAcross(%v, absent) is not nil", dims)
		}
	}
}

// checkInternals verifies Graph's private bookkeeping: positions, index,
// edge set and the maintained topological order of every dimension.
func checkInternals[K comparable](t testing.TB, g *Graph[K]) {
	t.Helper()
	if len(g.index) != len(g.nodes) {
		t.Fatalf("index has %d entries, nodes %d", len(g.index), len(g.nodes))
	}
	edges := 0
	for i, n := range g.nodes {
		if n.pos != i || g.index[n.key] != n {
			t.Fatalf("node %v: pos %d at %d, index %p vs %p", n.key, n.pos, i, g.index[n.key], n)
		}
		for d := 0; d < g.dims; d++ {
			for _, c := range n.out[d] {
				edges++
				if _, ok := g.edges[edgeKey[K]{d, n, c}]; !ok {
					t.Fatalf("edge %v -> %v in %d missing from edge set", n.key, c.key, d)
				}
				if !slices.Contains(c.in[d], n) {
					t.Fatalf("edge %v -> %v in %d missing from in-list", n.key, c.key, d)
				}
				if n.ord[d] >= c.ord[d] {
					t.Fatalf("edge %v -> %v in %d: maintained order %d >= %d", n.key, c.key, d, n.ord[d], c.ord[d])
				}
			}
		}
	}
	if edges != len(g.edges) {
		t.Fatalf("edge set has %d edges, out-lists %d", len(g.edges), edges)
	}
}

// checkConnect compares a Connect result with the reference model's.
func checkConnect[K comparable](t testing.TB, err error, d int, a, b K, cycle []K, aKnown, bKnown bool) {
	t.Helper()
	switch {
	case !aKnown || !bKnown:
		if !errors.Is(err, ErrUnknownNode) {
			t.Fatalf("Connect(%d, %v, %v) = %v, want ErrUnknownNode", d, a, b, err)
		}
	case cycle != nil:
		var ce *CycleError[K]
		if !errors.As(err, &ce) || !errors.Is(err, ErrCycle) {
			t.Fatalf("Connect(%d, %v, %v) = %v, want a CycleError", d, a, b, err)
		}
		if ce.Dim != d || ce.From != a || ce.To != b || !slices.Equal(ce.Path, cycle) {
			t.Fatalf("Connect(%d, %v, %v) = %+v, want path %v", d, a, b, ce, cycle)
		}
	default:
		if err != nil {
			t.Fatalf("Connect(%d, %v, %v) = %v, want nil", d, a, b, err)
		}
	}
}

func equal2[K comparable](a, b [][]K) bool {
	return slices.EqualFunc(a, b, func(x, y []K) bool { return slices.Equal(x, y) })
}

// refFrom builds the reference model of a graph made by AddNode and
// Connect calls only.
func refFrom[K comparable](g *Graph[K]) *refGraph[K] {
	ref := newRef[K](g.Dims())
	for _, n := range g.nodes {
		ref.addNode(n.key)
	}
	for d := 0; d < g.Dims(); d++ {
		for _, n := range g.nodes {
			for _, c := range n.out[d] {
				ref.out[d][n.key] = append(ref.out[d][n.key], c.key)
			}
		}
	}
	return ref
}

func TestReferenceModelSelfCheck(t *testing.T) {
	// The reference model itself, on pydagger's longest-path example.
	r := newRef[string](1)
	for _, k := range []string{"A", "B", "C"} {
		r.addNode(k)
	}
	r.connect(0, "A", "B")
	r.connect(0, "B", "C")
	r.connect(0, "A", "C")
	if got := fmt.Sprint(r.ordinal(0, "C"), r.descendents(0, "A"), r.layers(0)); got != "2 [B C] [[A] [B] [C]]" {
		t.Fatalf("reference model: %s", got)
	}
	if p, _ := r.connect(0, "C", "A"); !slices.Equal(p, []string{"A", "C"}) {
		t.Fatalf("reference cycle path %v", p)
	}
}
