// Ported from pydagger pydagger/graph.py, pydagger/output_pin.py and
// pydagger/input_pin.py (MIT, see NOTICE).

package dag

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// ErrCycle is the error a [*CycleError] unwraps to, so callers that do not
// need the path can test errors.Is(err, ErrCycle).
var ErrCycle = errors.New("edge would close a cycle")

// ErrUnknownNode is wrapped by errors from [Graph.Connect] and
// [Graph.ConnectAll] when an endpoint has not been added to the graph.
var ErrUnknownNode = errors.New("unknown node")

// ErrDimension is wrapped by errors from [Graph.Connect] and
// [Graph.ConnectAll] when the dimension is not in [0, Dims()).
var ErrDimension = errors.New("dimension out of range")

// defaultClosureLimit is the largest node count for which the topology pass
// stores every node's descendents as a bit set. The sets take n²/8 bytes per
// dimension (32 MiB at the limit); above it, descendent and ascendent
// queries walk the edges instead, so a hostile input cannot force quadratic
// memory.
const defaultClosureLimit = 1 << 14

// CycleError reports an edge that [Graph.Connect] or [Graph.ConnectAll]
// refused because it would close a cycle in its dimension. It unwraps to
// [ErrCycle].
//
// A CycleError shares nothing with the Graph that returned it (Path is its
// own slice), so it stays valid after the graph changes and may be read from
// any goroutine; like any struct, it must not be modified while others read
// it.
type CycleError[K comparable] struct {
	// Dim is the dimension the edge was offered to.
	Dim int
	// From and To are the refused edge's endpoints: From is the dependency,
	// To the dependent.
	From, To K
	// Path is an existing path To → … → From in dimension Dim, so the edge
	// From → To would close the cycle Path[0] → … → Path[len-1] → Path[0].
	// It is a shortest such path; among equally short ones, the first found
	// by a breadth-first search that follows edges in the order they were
	// added. For a self-edge it is [From].
	Path []K
}

// Error describes the refused edge and the path it would close.
func (e *CycleError[K]) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dag: connect %s -> %s in dimension %d: %v: existing path ",
		keyString(e.From), keyString(e.To), e.Dim, ErrCycle)
	for i, k := range e.Path {
		if i > 0 {
			b.WriteString(" -> ")
		}
		b.WriteString(keyString(k))
	}
	return b.String()
}

// Unwrap returns [ErrCycle].
func (e *CycleError[K]) Unwrap() error { return ErrCycle }

// Graph is a set of nodes with Dims independent edge sets. Each dimension
// is a DAG; Connect refuses an edge that would close a cycle in its
// dimension. Nodes are identified by their keys and remember the order in
// which they were added.
//
// The topology of a dimension (ordinals, layers, descendents, components) is
// computed lazily, in one pass, by the first query after a change to that
// dimension, as pydagger's calculate_topology does after every change.
//
// A Graph is not safe for concurrent mutation: AddNode, RemoveNode, Connect,
// ConnectAll and Disconnect must not run concurrently with each other or
// with any other method. Queries may run concurrently with each other: a
// query on a dimension whose topology is current takes no lock, and
// computing a stale one blocks only other queries on that dimension. Create
// a Graph with [New].
type Graph[K comparable] struct {
	dims  int
	nodes []*node[K] // insertion order; nodes[i].pos == i
	index map[K]*node[K]
	edges map[edgeKey[K]]struct{}

	nextOrder    int    // next value of node.ord handed out by AddNode
	stamp        uint64 // traversal stamp for Connect's searches
	closureLimit int

	state []dimState // per dimension: the cached topology
}

// node is one vertex. Its edge lists keep edges in the order they were added.
type node[K comparable] struct {
	key K
	pos int          // index in Graph.nodes
	out [][]*node[K] // per dimension: dependents (edge targets)
	in  [][]*node[K] // per dimension: dependencies (edge sources)
	ord []int        // per dimension: position in a maintained topological order
	via *node[K]     // Connect's forward search: predecessor on the search tree
	vis uint64       // Connect's searches: stamp of the last visit
}

type edgeKey[K comparable] struct {
	dim      int
	from, to *node[K]
}

// New returns an empty graph with dims independent edge sets, numbered 0 to
// dims-1. It panics if dims is less than 1.
//
// Every node holds two edge lists and an order slot per dimension, and every
// dimension's topology takes O(N) memory, plus N²/8 bytes of descendent bit
// sets up to 16,384 nodes; keep dims small (goomba uses 3).
func New[K comparable](dims int) *Graph[K] {
	if dims < 1 {
		panic(fmt.Sprintf("dag: New: dims %d is less than 1", dims))
	}
	return &Graph[K]{
		dims:         dims,
		index:        make(map[K]*node[K]),
		edges:        make(map[edgeKey[K]]struct{}),
		closureLimit: defaultClosureLimit,
		state:        make([]dimState, dims),
	}
}

// Dims returns the number of dimensions.
func (g *Graph[K]) Dims() int { return g.dims }

// Len returns the number of nodes.
func (g *Graph[K]) Len() int { return len(g.nodes) }

// Has reports whether k is a node of the graph.
func (g *Graph[K]) Has(k K) bool {
	_, ok := g.index[k]
	return ok
}

// Nodes returns every node in the order it was added.
func (g *Graph[K]) Nodes() []K {
	keys := make([]K, len(g.nodes))
	for i, n := range g.nodes {
		keys[i] = n.key
	}
	return keys
}

// AddNode adds k as a node with no edges, after every node already present.
// It returns false, and changes nothing, if k is already a node. It also
// refuses, with false, a key that is not equal to itself, such as a
// floating-point NaN or a struct or array holding one: no lookup could ever
// find that node again.
func (g *Graph[K]) AddNode(k K) bool {
	if k != k { // NaN, or a value holding one
		return false
	}
	if _, ok := g.index[k]; ok {
		return false
	}
	adj := make([][]*node[K], 2*g.dims)
	n := &node[K]{
		key: k,
		pos: len(g.nodes),
		out: adj[:g.dims:g.dims],
		in:  adj[g.dims:],
		ord: make([]int, g.dims),
	}
	// A node without edges may sit anywhere in a topological order; the end
	// is as good as any.
	for d := range n.ord {
		n.ord[d] = g.nextOrder
	}
	g.nextOrder++
	g.nodes = append(g.nodes, n)
	g.index[k] = n
	g.invalidateAll()
	return true
}

// RemoveNode removes k and every edge to or from it in every dimension. It
// returns false if k is not a node. The remaining nodes keep their order.
//
// It costs O(N) to renumber the nodes added after k plus, for each of k's
// neighbours, O(that neighbour's degree) to drop the shared edge. That is
// cheap for package graphs, but removing many nodes one by one from a large
// graph (say, every leaf of a hub) is quadratic; building a new graph is
// then cheaper.
func (g *Graph[K]) RemoveNode(k K) bool {
	n, ok := g.index[k]
	if !ok {
		return false
	}
	for d := 0; d < g.dims; d++ {
		for _, c := range n.out[d] {
			c.in[d] = without(c.in[d], n)
			delete(g.edges, edgeKey[K]{d, n, c})
		}
		for _, p := range n.in[d] {
			p.out[d] = without(p.out[d], n)
			delete(g.edges, edgeKey[K]{d, p, n})
		}
		n.out[d], n.in[d] = nil, nil
	}
	g.nodes = slices.Delete(g.nodes, n.pos, n.pos+1)
	for i := n.pos; i < len(g.nodes); i++ {
		g.nodes[i].pos = i
	}
	delete(g.index, k)
	g.invalidateAll()
	return true
}

// Connect adds the edge from → to in dimension dim: from is the dependency,
// to the dependent, so to's ordinal will exceed from's. Adding an edge that
// is already present does nothing and returns nil.
//
// Connect returns a [*CycleError] if the edge would close a cycle in dim,
// that is if from == to or to already reaches from in dim; the graph is then
// unchanged. It returns an error wrapping [ErrUnknownNode] if either
// endpoint is not a node, and one wrapping [ErrDimension] if dim is not in
// [0, Dims()).
//
// Connect keeps a topological order of each dimension current (the
// Pearce–Kelly algorithm), so it costs time in proportion to the part of the
// order it has to rearrange: little when edges arrive roughly dependencies
// first, but O(N + E) in the worst case. Building a graph edge by edge in an
// adversarial order therefore costs O(E·(N + E)): a chain whose every edge
// puts a later-added node in front of the chain built so far takes seconds
// at a few thousand nodes, against milliseconds for [Graph.ConnectAll],
// which costs O(N + E) whatever the order.
func (g *Graph[K]) Connect(dim int, from, to K) error {
	if dim < 0 || dim >= g.dims {
		return fmt.Errorf("dag: connect %s -> %s in dimension %d: %w [0, %d)",
			keyString(from), keyString(to), dim, ErrDimension, g.dims)
	}
	f, t, err := g.endpoints(dim, from, to)
	if err != nil {
		return err
	}
	key := edgeKey[K]{dim, f, t}
	if _, dup := g.edges[key]; dup {
		return nil
	}
	if path := g.reorder(dim, f, t); path != nil {
		return &CycleError[K]{Dim: dim, From: from, To: to, Path: path}
	}
	g.addEdge(key)
	g.invalidate(dim)
	return nil
}

// ConnectAll adds edges, each a (from, to) pair, to dimension dim as if by
// calling Connect on each in turn, but all or nothing: if Connect would
// return an error for edges[i] once edges[:i] were added, ConnectAll adds no
// edge at all and returns that same error. It returns an error wrapping
// [ErrDimension] if dim is not in [0, Dims()), even for no edges.
//
// ConnectAll checks the whole batch with one topological sort of dim and
// rebuilds dim's maintained order from it, so it costs O(N + E) whatever
// the order of the edges (O((N + E)·log len(edges)) when it fails, to find
// the first offending edge). Prefer it to Connect for building a graph from
// untrusted input.
func (g *Graph[K]) ConnectAll(dim int, edges [][2]K) error {
	if dim < 0 || dim >= g.dims {
		return fmt.Errorf("dag: connect %d edges in dimension %d: %w [0, %d)",
			len(edges), dim, ErrDimension, g.dims)
	}
	// The new edges, up to the first one Connect would reject outright.
	// given keeps the caller's keys for each batch entry, so a CycleError
	// names them exactly as Connect would (keys can compare equal yet print
	// differently, e.g. 0 and -0.0).
	var batch []edgeKey[K]
	var given [][2]K
	var invalid error
	seen := make(map[edgeKey[K]]struct{})
	for _, e := range edges {
		f, t, err := g.endpoints(dim, e[0], e[1])
		if err != nil {
			invalid = err
			break
		}
		key := edgeKey[K]{dim, f, t}
		if _, dup := g.edges[key]; dup {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		batch = append(batch, key)
		given = append(given, e)
	}
	order := g.sortWith(dim, batch)
	if order == nil {
		// Some prefix closes a cycle; the shortest one ends with the edge
		// Connect would have refused. batch[:lo] is acyclic, batch[:hi] not.
		lo, hi := 0, len(batch)
		for hi-lo > 1 {
			mid := int(uint(lo+hi) >> 1)
			if g.sortWith(dim, batch[:mid]) == nil {
				hi = mid
			} else {
				lo = mid
			}
		}
		e := batch[hi-1]
		return &CycleError[K]{Dim: dim, From: given[hi-1][0], To: given[hi-1][1],
			Path: g.pathWith(dim, batch[:hi-1], e.to, e.from)}
	}
	if invalid != nil {
		return invalid
	}
	if len(batch) == 0 {
		return nil
	}
	for _, key := range batch {
		g.addEdge(key)
	}
	for i, n := range order {
		n.ord[dim] = i // below nextOrder, which counts every node ever added
	}
	g.invalidate(dim)
	return nil
}

// Disconnect removes the edge from → to in dimension dim and reports whether
// it was present. It panics if dim is not in [0, Dims()).
func (g *Graph[K]) Disconnect(dim int, from, to K) bool {
	g.checkDim(dim)
	f, ok := g.index[from]
	if !ok {
		return false
	}
	t, ok := g.index[to]
	if !ok {
		return false
	}
	key := edgeKey[K]{dim, f, t}
	if _, ok := g.edges[key]; !ok {
		return false
	}
	delete(g.edges, key)
	f.out[dim] = without(f.out[dim], t)
	t.in[dim] = without(t.in[dim], f)
	g.invalidate(dim)
	return true
}

// Roots returns the nodes with no incoming edge in dim (pydagger's top-level
// nodes), in the order they were added. It panics if dim is not in
// [0, Dims()).
func (g *Graph[K]) Roots(dim int) []K {
	g.checkDim(dim)
	var keys []K
	for _, n := range g.nodes {
		if len(n.in[dim]) == 0 {
			keys = append(keys, n.key)
		}
	}
	return keys
}

// Leaves returns the nodes with no outgoing edge in dim (pydagger's
// bottom-level nodes), in the order they were added. It panics if dim is not
// in [0, Dims()).
func (g *Graph[K]) Leaves(dim int) []K {
	g.checkDim(dim)
	var keys []K
	for _, n := range g.nodes {
		if len(n.out[dim]) == 0 {
			keys = append(keys, n.key)
		}
	}
	return keys
}

// endpoints returns the nodes of an edge offered to Connect, or the error
// Connect returns for it.
func (g *Graph[K]) endpoints(dim int, from, to K) (f, t *node[K], err error) {
	f, ok := g.index[from]
	if !ok {
		return nil, nil, fmt.Errorf("dag: connect %s -> %s in dimension %d: %w %s",
			keyString(from), keyString(to), dim, ErrUnknownNode, keyString(from))
	}
	t, ok = g.index[to]
	if !ok {
		return nil, nil, fmt.Errorf("dag: connect %s -> %s in dimension %d: %w %s",
			keyString(from), keyString(to), dim, ErrUnknownNode, keyString(to))
	}
	if f == t {
		return nil, nil, &CycleError[K]{Dim: dim, From: from, To: to, Path: []K{from}}
	}
	return f, t, nil
}

// addEdge records an edge that is known to be new and acyclic.
func (g *Graph[K]) addEdge(key edgeKey[K]) {
	key.from.out[key.dim] = append(key.from.out[key.dim], key.to)
	key.to.in[key.dim] = append(key.to.in[key.dim], key.from)
	g.edges[key] = struct{}{}
}

// reorder keeps dimension dim's topological order (node.ord) valid for a new
// edge f → t, using the Pearce–Kelly dynamic topological sort. If t already
// reaches f it changes nothing and returns that path, t first; otherwise it
// returns nil.
//
// pydagger instead tests whether f is among t's descendents, which needs
// the full topology after every edge; maintaining an order only touches the
// nodes between t and f in it.
func (g *Graph[K]) reorder(dim int, f, t *node[K]) []K {
	lb, ub := t.ord[dim], f.ord[dim]
	if lb > ub {
		return nil // t already comes after f
	}
	// Forward: everything t reaches that sits before f in the order. Any
	// path t → … → f lies entirely in that region, so f is reached iff the
	// edge would close a cycle. Breadth-first, so the path is a shortest one.
	g.stamp++
	fwd := g.stamp
	t.vis, t.via = fwd, nil
	forward := []*node[K]{t}
	var path []K
search:
	for i := 0; i < len(forward); i++ {
		u := forward[i]
		for _, w := range u.out[dim] {
			if w == f {
				path = cyclePath(u, f)
				break search
			}
			if w.vis != fwd && w.ord[dim] < ub {
				w.vis, w.via = fwd, u
				forward = append(forward, w)
			}
		}
	}
	for _, n := range forward {
		n.via = nil // keep no pointers to nodes that may be removed later
	}
	if path != nil {
		return path
	}
	// Backward: everything that reaches f and sits after t in the order.
	g.stamp++
	bwd := g.stamp
	f.vis = bwd
	backward := []*node[K]{f}
	for i := 0; i < len(backward); i++ {
		for _, w := range backward[i].in[dim] {
			if w.vis != bwd && w.ord[dim] > lb {
				w.vis = bwd
				backward = append(backward, w)
			}
		}
	}
	// The two regions are disjoint (a node in both would put t → … → f).
	// Hand their order slots back out, backward region first, each keeping
	// its internal order.
	byOrder := func(a, b *node[K]) int { return cmp.Compare(a.ord[dim], b.ord[dim]) }
	slices.SortFunc(forward, byOrder)
	slices.SortFunc(backward, byOrder)
	slots := make([]int, 0, len(forward)+len(backward))
	for _, n := range backward {
		slots = append(slots, n.ord[dim])
	}
	for _, n := range forward {
		slots = append(slots, n.ord[dim])
	}
	slices.Sort(slots)
	for i, n := range backward {
		n.ord[dim] = slots[i]
	}
	for i, n := range forward {
		n.ord[dim] = slots[len(backward)+i]
	}
	return nil
}

// cyclePath returns the search-tree path from the forward search's root to
// u, followed by f.
func cyclePath[K comparable](u, f *node[K]) []K {
	path := []K{f.key}
	for n := u; n != nil; n = n.via {
		path = append(path, n.key)
	}
	slices.Reverse(path)
	return path
}

// extraOut indexes extra edges by source position, after the edges already
// in each node's list, as appending them would place them.
func (g *Graph[K]) extraOut(extra []edgeKey[K]) [][]*node[K] {
	out := make([][]*node[K], len(g.nodes))
	for _, e := range extra {
		out[e.from.pos] = append(out[e.from.pos], e.to)
	}
	return out
}

// sortWith returns a topological order of dimension dim with the extra
// edges added (Kahn's algorithm), or nil if they close a cycle. It does not
// change the graph.
func (g *Graph[K]) sortWith(dim int, extra []edgeKey[K]) []*node[K] {
	more := g.extraOut(extra)
	indegree := make([]int, len(g.nodes))
	for _, n := range g.nodes {
		indegree[n.pos] = len(n.in[dim])
	}
	for _, e := range extra {
		indegree[e.to.pos]++
	}
	order := make([]*node[K], 0, len(g.nodes))
	for _, n := range g.nodes {
		if indegree[n.pos] == 0 {
			order = append(order, n)
		}
	}
	for i := 0; i < len(order); i++ {
		u := order[i]
		for _, list := range [2][]*node[K]{u.out[dim], more[u.pos]} {
			for _, w := range list {
				indegree[w.pos]--
				if indegree[w.pos] == 0 {
					order = append(order, w)
				}
			}
		}
	}
	if len(order) < len(g.nodes) {
		return nil
	}
	return order
}

// pathWith returns the path src → … → dst in dimension dim with the extra
// edges added that Connect's search would report: a breadth-first search
// following edges in the order they were (or would be) added.
func (g *Graph[K]) pathWith(dim int, extra []edgeKey[K], src, dst *node[K]) []K {
	more := g.extraOut(extra)
	parent := make([]*node[K], len(g.nodes))
	seen := make([]bool, len(g.nodes))
	seen[src.pos] = true
	queue := []*node[K]{src}
	for i := 0; i < len(queue); i++ {
		u := queue[i]
		for _, list := range [2][]*node[K]{u.out[dim], more[u.pos]} {
			for _, w := range list {
				if seen[w.pos] {
					continue
				}
				seen[w.pos] = true
				parent[w.pos] = u
				if w == dst {
					path := []K{w.key}
					for n := u; n != nil; n = parent[n.pos] {
						path = append(path, n.key)
					}
					slices.Reverse(path)
					return path
				}
				queue = append(queue, w)
			}
		}
	}
	panic("dag: internal error: cyclic edge set without a path")
}

// without returns list with its single element n removed, keeping order.
func without[K comparable](list []*node[K], n *node[K]) []*node[K] {
	i := slices.Index(list, n)
	if i < 0 {
		return list
	}
	return slices.Delete(list, i, i+1)
}

// checkDim panics if dim is not a dimension of g.
func (g *Graph[K]) checkDim(dim int) {
	if dim < 0 || dim >= g.dims {
		panic(fmt.Sprintf("dag: dimension %d out of range [0, %d)", dim, g.dims))
	}
}

// keyString formats a key for an error message so that hostile input cannot
// forge one: keys whose kind is string, including named string types, are
// always quoted, and other keys are quoted when their text has a space, a
// quote, an arrow or anything unprintable.
func keyString[K comparable](k K) string {
	if v := reflect.ValueOf(k); v.Kind() == reflect.String {
		return strconv.Quote(v.String())
	}
	s := fmt.Sprint(k)
	if strings.ContainsAny(s, ` "`) || strings.Contains(s, "->") ||
		strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return strconv.Quote(s)
	}
	return s
}
