// Supports the port of resolvo src/conflict.rs (BSD-3-Clause, see NOTICE).
// The edge-list layout and traversal order follow petgraph's Graph and
// DfsPostOrder (MIT OR Apache-2.0).

package cdcl

// resolvo builds its conflict graph with petgraph, and the conflict report
// depends on the exact iteration order of petgraph's directed Graph: edge
// lists are linked lists with the most recently added edge first, removing
// a node swaps the last node into its index, and DfsPostOrder visits
// successors in edge-list order. digraph is an independent implementation
// with the same layout and observable order; no petgraph code is copied.

const noEdge = -1

type graphNode struct {
	weight ConflictNode
	// next holds the heads of the outgoing (0) and incoming (1) edge lists.
	next [2]int
}

type graphEdge struct {
	weight ConflictEdge
	// node holds the source (0) and target (1).
	node [2]int
	// next holds the next edge in the source's outgoing list (0) and in
	// the target's incoming list (1).
	next [2]int
}

type digraph struct {
	nodes []graphNode
	edges []graphEdge
}

func (g *digraph) addNode(w ConflictNode) int {
	g.nodes = append(g.nodes, graphNode{weight: w, next: [2]int{noEdge, noEdge}})
	return len(g.nodes) - 1
}

func (g *digraph) addEdge(a, b int, w ConflictEdge) int {
	idx := len(g.edges)
	e := graphEdge{weight: w, node: [2]int{a, b}}
	if a == b {
		an := &g.nodes[a]
		e.next = an.next
		an.next[0] = idx
		an.next[1] = idx
	} else {
		e.next = [2]int{g.nodes[a].next[0], g.nodes[b].next[1]}
		g.nodes[a].next[0] = idx
		g.nodes[b].next[1] = idx
	}
	g.edges = append(g.edges, e)
	return idx
}

// edgesDirected returns the edges leaving (dir 0) or entering (dir 1) node
// a, most recently added first.
func (g *digraph) edgesDirected(a, dir int) []int {
	var out []int
	for e := g.nodes[a].next[dir]; e != noEdge; e = g.edges[e].next[dir] {
		out = append(out, e)
	}
	return out
}

func (g *digraph) outgoing(a int) []int { return g.edgesDirected(a, 0) }

func (g *digraph) incoming(a int) []int { return g.edgesDirected(a, 1) }

func (g *digraph) source(e int) int { return g.edges[e].node[0] }

func (g *digraph) target(e int) int { return g.edges[e].node[1] }

// unlinkEdge unlinks edge e from the list of direction dir of node n.
func (g *digraph) unlinkEdge(e, n, dir int) {
	if g.nodes[n].next[dir] == e {
		g.nodes[n].next[dir] = g.edges[e].next[dir]
		return
	}
	for cur := g.nodes[n].next[dir]; cur != noEdge; cur = g.edges[cur].next[dir] {
		if g.edges[cur].next[dir] == e {
			g.edges[cur].next[dir] = g.edges[e].next[dir]
			return
		}
	}
}

// replaceEdgeLinks replaces links to edge from with links to edge to.
func (g *digraph) replaceEdgeLinks(from, to int) {
	for dir := 0; dir < 2; dir++ {
		n := g.edges[to].node[dir]
		if g.nodes[n].next[dir] == from {
			g.nodes[n].next[dir] = to
			continue
		}
		for cur := g.nodes[n].next[dir]; cur != noEdge; cur = g.edges[cur].next[dir] {
			if g.edges[cur].next[dir] == from {
				g.edges[cur].next[dir] = to
				break
			}
		}
	}
}

// removeEdge removes an edge, swapping the last edge into its index.
func (g *digraph) removeEdge(e int) {
	src, dst := g.edges[e].node[0], g.edges[e].node[1]
	g.unlinkEdge(e, src, 0)
	g.unlinkEdge(e, dst, 1)
	last := len(g.edges) - 1
	if e != last {
		g.edges[e] = g.edges[last]
		g.replaceEdgeLinks(last, e)
	}
	g.edges = g.edges[:last]
}

// removeNode removes a node and its edges, swapping the last node into its
// index like petgraph does.
func (g *digraph) removeNode(a int) {
	for dir := 0; dir < 2; dir++ {
		for g.nodes[a].next[dir] != noEdge {
			g.removeEdge(g.nodes[a].next[dir])
		}
	}
	last := len(g.nodes) - 1
	g.nodes[a] = g.nodes[last]
	g.nodes = g.nodes[:last]
	if a == last {
		return
	}
	for dir := 0; dir < 2; dir++ {
		for e := g.nodes[a].next[dir]; e != noEdge; e = g.edges[e].next[dir] {
			g.edges[e].node[dir] = a
		}
	}
}

// dfsPostOrder returns the nodes reachable from start in petgraph's
// DfsPostOrder order.
func (g *digraph) dfsPostOrder(start int) []int {
	discovered := make([]bool, len(g.nodes))
	finished := make([]bool, len(g.nodes))
	stack := []int{start}
	var out []int
	for len(stack) > 0 {
		nx := stack[len(stack)-1]
		if !discovered[nx] {
			discovered[nx] = true
			for e := g.nodes[nx].next[0]; e != noEdge; e = g.edges[e].next[0] {
				succ := g.edges[e].node[1]
				if !discovered[succ] {
					stack = append(stack, succ)
				}
			}
			continue
		}
		stack = stack[:len(stack)-1]
		if !finished[nx] {
			finished[nx] = true
			out = append(out, nx)
		}
	}
	return out
}
