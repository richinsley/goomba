// Ported from resolvo src/conflict.rs (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Conflict explains why a problem is unsolvable. Solve returns it as an
// error; it matches ErrUnsolvable with errors.Is.
//
// A Conflict refers to the state of the solve that produced it and stays
// valid when the Solver is used again. It is not safe for concurrent use.
type Conflict struct {
	// clauses are the clauses involved in the conflict, without
	// duplicates, in the order they were found.
	clauses []clauseID
	// inClauses holds the members of clauses, so adding one is O(1).
	inClauses bitSet
	solver    *Solver
	state     *solverState
}

// addClause appends id to the conflict's clauses unless it is already
// there. A conflict can involve hundreds of thousands of clauses (one per
// candidate of a large package), so membership is a set lookup rather
// than a scan.
func (c *Conflict) addClause(id clauseID) {
	if c.inClauses.insert(uint32(id)) {
		c.clauses = append(c.clauses, id)
	}
}

// Error returns the user-friendly explanation (see Display, which bounds
// its size), prefixed with a summary line.
func (c *Conflict) Error() string {
	return ErrUnsolvable.Error() + ":\n" + strings.TrimRight(c.Display(c.solver.provider), "\n")
}

// Is reports whether target is ErrUnsolvable.
func (c *Conflict) Is(target error) bool { return target == ErrUnsolvable }

// Display returns a human-readable explanation of the conflict, using
// interner to format names and versions. The text has one line per entry,
// each terminated by a newline, drawn as a tree:
//
//	The following packages are incompatible
//	├─ a * can be installed with any of the following options:
//	│  └─ a 1
//	└─ b * cannot be installed because there are no viable options:
//	   └─ b 1, which conflicts with the versions reported above.
//
// A candidate that would require or constrain other packages is explained
// once; where the tree reaches it again (a shared dependency, or a cycle) it
// is shown as "<candidate>, as reported above." (resolvo expands it again,
// which never terminates on a cyclic conflict graph). The text is capped at
// 64 KiB and 2000 lines; an explanation that reaches the cap ends with the
// line "… explanation truncated".
//
// Like resolvo's, the explanation does not mention conditions and covers
// only the part of the conflict graph reachable from the root, so it can be
// incomplete.
func (c *Conflict) Display(interner Interner) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = recoveredError(r, c.solver.debugChecks).Error() + "\n"
		}
	}()
	g, err := c.Graph()
	if err != nil {
		return err.Error() + "\n"
	}
	return newDisplayUnsat(g, interner).String()
}

// ConflictNodeKind discriminates the nodes of a ConflictGraph. It is a plain
// value, safe for concurrent use.
type ConflictNodeKind uint8

const (
	// ConflictNodeRoot is the root of the problem.
	ConflictNodeRoot ConflictNodeKind = iota
	// ConflictNodeSolvable is a solvable.
	ConflictNodeSolvable
	// ConflictNodeUnresolved is the sink of requirements without
	// candidates.
	ConflictNodeUnresolved
	// ConflictNodeExcluded is an exclusion reason.
	ConflictNodeExcluded
)

// ConflictNode is a node of a ConflictGraph. It is a plain value, safe for
// concurrent use.
type ConflictNode struct {
	// Kind selects which other field is meaningful.
	Kind ConflictNodeKind
	// Solvable is set for ConflictNodeSolvable.
	Solvable SolvableID
	// Reason is set for ConflictNodeExcluded.
	Reason StringID
}

func conflictNodeOf(s solvableOrRoot) ConflictNode {
	if s.root {
		return ConflictNode{Kind: ConflictNodeRoot}
	}
	return ConflictNode{Kind: ConflictNodeSolvable, Solvable: s.id}
}

// solvableOrRoot returns the solvable or root of a node; other kinds are an
// internal error.
func (n ConflictNode) solvableOrRoot() solvableOrRoot {
	switch n.Kind {
	case ConflictNodeRoot:
		return solvableOrRoot{root: true}
	case ConflictNodeSolvable:
		return solvableOrRoot{id: n.Solvable}
	}
	panic(internalError("expected a solvable node"))
}

// ConflictEdgeKind discriminates the edges of a ConflictGraph. It is a plain
// value, safe for concurrent use.
type ConflictEdgeKind uint8

const (
	// ConflictEdgeRequires: the target is a candidate for Requirement.
	ConflictEdgeRequires ConflictEdgeKind = iota
	// ConflictEdgeConflict: the target is involved in a conflict.
	ConflictEdgeConflict
)

// ConflictCause says why two nodes conflict. It is a plain value, safe for
// concurrent use.
type ConflictCause uint8

const (
	// CauseLocked: the target cannot be installed because another
	// solvable (ConflictEdge.Locked) is locked.
	CauseLocked ConflictCause = iota
	// CauseConstrains: the target is excluded by the constraint
	// ConflictEdge.VersionSet of the source.
	CauseConstrains
	// CauseForbidMultipleInstances: only one solvable per package may be
	// installed.
	CauseForbidMultipleInstances
	// CauseExcluded: the source was excluded for the reason in the target.
	CauseExcluded
)

// ConflictEdge is an edge of a ConflictGraph. It is a plain value, safe for
// concurrent use.
type ConflictEdge struct {
	// Kind selects which other fields are meaningful.
	Kind ConflictEdgeKind
	// Requirement is set for ConflictEdgeRequires.
	Requirement Requirement
	// Cause is set for ConflictEdgeConflict.
	Cause ConflictCause
	// Locked is the locked solvable of a CauseLocked edge.
	Locked SolvableID
	// VersionSet is the constraint of a CauseConstrains edge.
	VersionSet VersionSetID
}

func requiresEdge(r Requirement) ConflictEdge {
	return ConflictEdge{Kind: ConflictEdgeRequires, Requirement: r}
}

func conflictEdge(cause ConflictCause) ConflictEdge {
	return ConflictEdge{Kind: ConflictEdgeConflict, Cause: cause}
}

// ConflictGraph is the graph representation of a Conflict. Its root is the
// root of the problem; only the requirements involved in the conflict are
// included. It is not safe for concurrent use.
type ConflictGraph struct {
	g          digraph
	root       int
	unresolved int
}

// NodeCount returns the number of nodes.
func (cg *ConflictGraph) NodeCount() int { return len(cg.g.nodes) }

// Node returns node i, for 0 <= i < NodeCount().
func (cg *ConflictGraph) Node(i int) ConflictNode { return cg.g.nodes[i].weight }

// EdgeCount returns the number of edges.
func (cg *ConflictGraph) EdgeCount() int { return len(cg.g.edges) }

// Edge returns edge i, for 0 <= i < EdgeCount(), with its source and target
// node indices.
func (cg *ConflictGraph) Edge(i int) (source, target int, edge ConflictEdge) {
	e := cg.g.edges[i]
	return e.node[0], e.node[1], e.weight
}

// RootNode returns the index of the root node.
func (cg *ConflictGraph) RootNode() int { return cg.root }

// constrainsEdgeKey deduplicates constrains edges reconstructed from the
// shared encoding.
type constrainsEdgeKey struct {
	parent, candidate solvableOrRoot
	vs                VersionSetID
}

// Graph builds the graph representation of the conflict. It fails with an
// error wrapping ErrInternal if anything panics, for example the provider.
func (c *Conflict) Graph() (graph *ConflictGraph, err error) {
	defer func() {
		if r := recover(); r != nil {
			graph, err = nil, recoveredError(r, c.solver.debugChecks)
		}
	}()
	st := c.state
	vm := &st.variableMap
	cg := &ConflictGraph{}
	g := &cg.g

	nodes := map[solvableOrRoot]int{}
	addNode := func(s solvableOrRoot) int {
		if n, ok := nodes[s]; ok {
			return n
		}
		n := g.addNode(conflictNodeOf(s))
		nodes[s] = n
		return n
	}
	excludedNodes := map[StringID]int{}

	rootNode := addNode(solvableOrRoot{root: true})
	unresolvedNode := g.addNode(ConflictNode{Kind: ConflictNodeUnresolved})
	lastNodeByName := map[NameID]int{}

	mustSolvableOrRoot := func(v variableID) solvableOrRoot {
		s, ok := vm.asSolvableOrRoot(v)
		if !ok {
			panic(internalError("expected a solvable or root variable"))
		}
		return s
	}

	// The shared constrains encoding splits each (parent, excluded
	// candidate) pair over two clauses linked by an auxiliary variable.
	// Collect both sides per auxiliary variable so the original
	// parent -> candidate edges can be reconstructed.
	auxParents := map[variableID][]solvableOrRoot{}
	auxCandidates := map[variableID][]solvableOrRoot{}
	for _, id := range c.clauses {
		cl := st.clauses[id]
		switch cl.kind {
		case clauseConstrainsParent:
			auxParents[cl.v2()] = append(auxParents[cl.v2()], mustSolvableOrRoot(cl.v1))
		case clauseConstrainsExcluded:
			auxCandidates[cl.v2()] = append(auxCandidates[cl.v2()], mustSolvableOrRoot(cl.v1))
		}
	}

	// If only one side of a chain is part of the conflict, recover the other
	// side from the assignment reason of the auxiliary variable.
	reasonClause := func(aux variableID) (clause, bool) {
		id, ok := st.decisionTracker.findClauseForAssignment(aux)
		if !ok {
			return clause{}, false
		}
		return st.clauses[id], true
	}
	for _, aux := range sortedVariableKeys(auxParents) {
		if _, ok := auxCandidates[aux]; ok {
			continue
		}
		if rc, ok := reasonClause(aux); ok && rc.kind == clauseConstrainsExcluded {
			auxCandidates[aux] = []solvableOrRoot{mustSolvableOrRoot(rc.v1)}
		}
	}
	for _, aux := range sortedVariableKeys(auxCandidates) {
		if _, ok := auxParents[aux]; ok {
			continue
		}
		if rc, ok := reasonClause(aux); ok && rc.kind == clauseConstrainsParent {
			auxParents[aux] = []solvableOrRoot{mustSolvableOrRoot(rc.v1)}
		}
	}

	// The shared requires encoding links a requirer to its candidate
	// disjunction through a gate: AnyOf(gate, parent) per requirer plus one
	// Requires(gate, …). Collect the requirers per gate so the original
	// parent -> candidate edges can be drawn from the gate's clause.
	gateParents := map[variableID][]solvableOrRoot{}
	for _, id := range c.clauses {
		cl := st.clauses[id]
		if cl.kind == clauseAnyOf && vm.origin(cl.v1).kind == originRequiresGate {
			if p, ok := vm.asSolvableOrRoot(cl.v2()); ok {
				gateParents[cl.v1] = append(gateParents[cl.v1], p)
			}
		}
	}
	// If a gate is part of the conflict but no implication clause of a
	// requirer is, recover the requirer from the gate's assignment reason.
	for _, id := range c.clauses {
		cl := st.clauses[id]
		if cl.kind != clauseRequires || vm.origin(cl.v1).kind != originRequiresGate {
			continue
		}
		if _, ok := gateParents[cl.v1]; ok {
			continue
		}
		if rc, ok := reasonClause(cl.v1); ok && rc.kind == clauseAnyOf {
			if p, ok := vm.asSolvableOrRoot(rc.v2()); ok {
				gateParents[cl.v1] = []solvableOrRoot{p}
			}
		}
	}

	constrainsEdges := map[constrainsEdgeKey]struct{}{}

	for _, id := range c.clauses {
		cl := st.clauses[id]
		switch cl.kind {
		case clauseInstallRoot, clauseAnyOf:
			// AnyOf clauses get no edge: requires-gate implications are
			// represented by the edges drawn from the gate's Requires
			// clause, and conditions (condition-holds definitions) are not
			// represented in explanations.
		case clauseExcluded:
			s, ok := vm.asSolvable(cl.v1)
			if !ok {
				panic(internalError("only solvables can be excluded"))
			}
			reason := StringID(cl.x)
			pkg := addNode(solvableOrRoot{id: s})
			ex, ok := excludedNodes[reason]
			if !ok {
				ex = g.addNode(ConflictNode{Kind: ConflictNodeExcluded, Reason: reason})
				excludedNodes[reason] = ex
			}
			g.addEdge(pkg, ex, conflictEdge(CauseExcluded))
		case clauseLearnt:
			panic(internalError("learnt clause in a conflict"))
		case clauseRequires:
			// With the shared encoding the parent is a gate; expand it to
			// the real requirers.
			var parents []solvableOrRoot
			if p, ok := vm.asSolvableOrRoot(cl.v1); ok {
				parents = []solvableOrRoot{p}
			} else {
				parents = gateParents[cl.v1]
			}
			candidates, ok := c.solver.cache.cachedSortedCandidates(cl.req)
			if !ok {
				panic(internalError("candidates of an encoded requirement are not cached"))
			}
			for _, p := range parents {
				pkg := addNode(p)
				if len(candidates) == 0 {
					g.addEdge(pkg, unresolvedNode, requiresEdge(cl.req))
					continue
				}
				for _, cand := range candidates {
					cn := addNode(solvableOrRoot{id: cand})
					g.addEdge(pkg, cn, requiresEdge(cl.req))
				}
			}
		case clauseLock:
			locked, ok1 := vm.asSolvable(cl.v1)
			forbidden, ok2 := vm.asSolvable(cl.v2())
			if !ok1 || !ok2 {
				panic(internalError("lock clause on a non-solvable"))
			}
			n2 := addNode(solvableOrRoot{id: forbidden})
			e := conflictEdge(CauseLocked)
			e.Locked = locked
			g.addEdge(rootNode, n2, e)
		case clauseForbidMultiple:
			n1 := addNode(mustSolvableOrRoot(cl.v1))
			o := vm.origin(literal(cl.x).variable())
			if o.kind != originForbidMultiple {
				panic(internalError("forbid-multiple clause without a forbid variable"))
			}
			name := NameID(o.id)
			if prev, ok := lastNodeByName[name]; ok {
				g.addEdge(prev, n1, conflictEdge(CauseForbidMultipleInstances))
			}
			lastNodeByName[name] = n1
		case clauseConstrains:
			pn := addNode(mustSolvableOrRoot(cl.v1))
			dn := addNode(mustSolvableOrRoot(cl.v2()))
			e := conflictEdge(CauseConstrains)
			e.VersionSet = VersionSetID(cl.y)
			g.addEdge(pn, dn, e)
		case clauseConstrainsParent:
			parent := mustSolvableOrRoot(cl.v1)
			vs := VersionSetID(cl.y)
			for _, dep := range auxCandidates[cl.v2()] {
				key := constrainsEdgeKey{parent: parent, candidate: dep, vs: vs}
				if _, dup := constrainsEdges[key]; dup {
					continue
				}
				constrainsEdges[key] = struct{}{}
				pn := addNode(parent)
				dn := addNode(dep)
				e := conflictEdge(CauseConstrains)
				e.VersionSet = vs
				g.addEdge(pn, dn, e)
			}
		case clauseConstrainsExcluded:
			dep := mustSolvableOrRoot(cl.v1)
			vs := VersionSetID(cl.y)
			for _, parent := range auxParents[cl.v2()] {
				key := constrainsEdgeKey{parent: parent, candidate: dep, vs: vs}
				if _, dup := constrainsEdges[key]; dup {
					continue
				}
				constrainsEdges[key] = struct{}{}
				pn := addNode(parent)
				dn := addNode(dep)
				e := conflictEdge(CauseConstrains)
				e.VersionSet = vs
				g.addEdge(pn, dn, e)
			}
		}
	}

	cg.root = rootNode
	cg.unresolved = unresolvedNode
	if g.nodes[unresolvedNode].next[1] == noEdge {
		g.removeNode(unresolvedNode)
		cg.unresolved = -1
	}
	return cg, nil
}

func sortedVariableKeys(m map[variableID][]solvableOrRoot) []variableID {
	keys := make([]variableID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// mergedConflictNode is a group of solvables that are displayed as one:
// they have the same name and the same predecessors and successors.
type mergedConflictNode struct {
	ids []SolvableID
}

type adjacencyKey struct {
	isRequires bool
	req        Requirement
	node       int
}

func adjacencyLess(a, b adjacencyKey) bool {
	if a.isRequires != b.isRequires {
		return !a.isRequires
	}
	if a.isRequires && a.req != b.req {
		return a.req.less(b.req)
	}
	return a.node < b.node
}

func (cg *ConflictGraph) adjacency(edges []int, useSource bool) string {
	keys := make([]adjacencyKey, 0, len(edges))
	for _, e := range edges {
		ge := cg.g.edges[e]
		k := adjacencyKey{node: ge.node[1]}
		if useSource {
			k.node = ge.node[0]
		}
		if ge.weight.Kind == ConflictEdgeRequires {
			k.isRequires = true
			k.req = ge.weight.Requirement
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return adjacencyLess(keys[i], keys[j]) })
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%t:%t:%d:%d,", k.isRequires, k.req.union, k.req.id, k.node)
	}
	return b.String()
}

// simplify groups the nodes that can be displayed as one candidate: same
// name, same predecessors and same successors. Candidates that require
// different version sets stay separate, so each distinct requirement is
// reported to the user.
func (cg *ConflictGraph) simplify(interner Interner) map[SolvableID]*mergedConflictNode {
	type group struct {
		ids []SolvableID
	}
	groups := map[string]*group{}
	var order []string
	for n := range cg.g.nodes {
		node := cg.g.nodes[n].weight
		if node.Kind != ConflictNodeSolvable {
			continue
		}
		name := interner.DisplayName(interner.SolvableName(node.Solvable))
		key := name + "\x00" + cg.adjacency(cg.g.incoming(n), true) + "\x00" + cg.adjacency(cg.g.outgoing(n), false)
		gr, ok := groups[key]
		if !ok {
			gr = &group{}
			groups[key] = gr
			order = append(order, key)
		}
		gr.ids = append(gr.ids, node.Solvable)
	}
	merged := map[SolvableID]*mergedConflictNode{}
	for _, key := range order {
		gr := groups[key]
		if len(gr.ids) <= 1 {
			continue
		}
		m := &mergedConflictNode{ids: gr.ids}
		for _, id := range gr.ids {
			merged[id] = m
		}
	}
	return merged
}

// installableSet computes which nodes are installable: a node is
// installable if it has no outgoing conflict edges and each of its
// requirements has at least one installable candidate. Installability is
// propagated bottom-up.
func (cg *ConflictGraph) installableSet() []bool {
	g := &cg.g
	installable := make([]bool, len(g.nodes))
outer:
	for _, nx := range g.dfsPostOrder(cg.root) {
		if nx == cg.unresolved {
			continue
		}
		// Nodes excluded for external reasons are not installable.
		for _, e := range g.incoming(nx) {
			w := g.edges[e].weight
			if w.Kind == ConflictEdgeConflict && w.Cause == CauseExcluded {
				continue outer
			}
		}
		out := g.outgoing(nx)
		for _, e := range out {
			if g.edges[e].weight.Kind == ConflictEdgeConflict {
				continue outer
			}
		}
		for _, chunk := range chunkByRequirement(g, out) {
			any := false
			for _, e := range chunk {
				if installable[g.target(e)] {
					any = true
					break
				}
			}
			if !any {
				continue outer
			}
		}
		installable[nx] = true
	}
	return installable
}

// missingSet computes which nodes are missing: not involved in any
// conflict, yet not installable because a requirement has no candidates.
func (cg *ConflictGraph) missingSet() []bool {
	g := &cg.g
	missing := make([]bool, len(g.nodes))
	if cg.unresolved < 0 {
		return missing
	}
	missing[cg.unresolved] = true
outer:
	for _, nx := range g.dfsPostOrder(cg.root) {
		out := g.outgoing(nx)
		for _, e := range out {
			if g.edges[e].weight.Kind == ConflictEdgeConflict {
				continue outer
			}
		}
		for _, chunk := range chunkByRequirement(g, out) {
			all := true
			for _, e := range chunk {
				if !missing[g.target(e)] {
					all = false
					break
				}
			}
			if all {
				missing[nx] = true
				break
			}
		}
	}
	return missing
}

// chunkByRequirement groups consecutive requires edges with the same
// requirement.
func chunkByRequirement(g *digraph, edges []int) [][]int {
	var out [][]int
	for i, e := range edges {
		if i > 0 && g.edges[e].weight.Requirement == g.edges[edges[i-1]].weight.Requirement {
			out[len(out)-1] = append(out[len(out)-1], e)
			continue
		}
		out = append(out, []int{e})
	}
	return out
}

// Graphviz writes the graph in Graphviz dot format. With simplify, merged
// nodes are drawn once. The output is linear in the size of the graph and
// capped at 1 MiB: edges beyond that are left out and a
// "/* graph truncated */" comment marks the cut. It fails with an error
// wrapping ErrInternal if the interner panics.
func (cg *ConflictGraph) Graphviz(w io.Writer, interner Interner, simplify bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recoveredError(r, false)
		}
	}()
	g := &cg.g
	var merged map[SolvableID]*mergedConflictNode
	if simplify {
		merged = cg.simplify(interner)
	}
	var b strings.Builder
	b.WriteString("digraph {")
	truncated := false
	for nx := 0; nx < len(g.nodes) && !truncated; nx++ {
		node := g.nodes[nx].weight
		if node.Kind != ConflictNodeRoot && node.Kind != ConflictNodeSolvable {
			continue
		}
		id := node.solvableOrRoot()
		// Only draw the first node of a merged group.
		if !id.root {
			if m, ok := merged[id.id]; ok && m.ids[0] != id.id {
				continue
			}
		}

		var added []solvableOrRoot
	edges:
		for _, e := range g.outgoing(nx) {
			ge := g.edges[e]
			target := g.nodes[ge.node[1]].weight
			color := "red"
			if ge.weight.Kind == ConflictEdgeRequires && target.Kind != ConflictNodeUnresolved {
				color = "black"
			}
			var label string
			switch {
			case ge.weight.Kind == ConflictEdgeRequires:
				label = ge.weight.Requirement.Display(interner)
			case ge.weight.Cause == CauseConstrains:
				label = interner.DisplayVersionSet(ge.weight.VersionSet)
			case ge.weight.Cause == CauseExcluded:
				label = "excluded"
			default:
				label = "already installed"
			}

			var targetLabel string
			switch target.Kind {
			case ConflictNodeRoot, ConflictNodeSolvable:
				s2 := target.solvableOrRoot()
				// Replace a merged target by the first id of its group.
				if !s2.root {
					if m, ok := merged[s2.id]; ok {
						s2 = solvableOrRoot{id: m.ids[0]}
						for _, a := range added {
							if a == s2 {
								continue edges
							}
						}
						added = append(added, s2)
					}
				}
				targetLabel = s2.display(interner)
			case ConflictNodeUnresolved:
				targetLabel = "unresolved"
			case ConflictNodeExcluded:
				targetLabel = "reason: " + interner.DisplayString(target.Reason)
			}
			line := fmt.Sprintf("\"%s\" -> \"%s\"[color=%s, label=\"%s\"];", id.display(interner), targetLabel, color, label)
			if b.Len()+len(line)+len(graphvizTruncated)+1 > maxGraphvizBytes {
				truncated = true
				break
			}
			b.WriteString(line)
		}
	}
	if truncated {
		b.WriteString(graphvizTruncated)
	}
	b.WriteString("}")
	_, err = io.WriteString(w, b.String())
	return err
}

type childOrder uint8

const (
	hasRemainingSiblings childOrder = iota
	lastChild
)

// indenter draws the tree prefixes of the conflict report.
type indenter struct {
	levels         []childOrder
	topLevelIndent bool
}

func newIndenter(topLevelIndent bool) indenter {
	return indenter{topLevelIndent: topLevelIndent}
}

func (in indenter) isAtTopLevel() bool { return len(in.levels) == 1 }

func (in indenter) pushLevel() indenter { return in.pushLevelWithOrder(hasRemainingSiblings) }

func (in indenter) pushLevelWithOrder(o childOrder) indenter {
	levels := make([]childOrder, len(in.levels)+1)
	copy(levels, in.levels)
	levels[len(in.levels)] = o
	return indenter{levels: levels, topLevelIndent: in.topLevelIndent}
}

func (in *indenter) setLast() { in.levels[len(in.levels)-1] = lastChild }

func (in indenter) indent() string {
	var b strings.Builder
	deepest := len(in.levels) - 1
	for level, order := range in.levels {
		if level == 0 && !in.topLevelIndent {
			continue
		}
		isDeepest := level == deepest
		switch {
		case isDeepest && order == hasRemainingSiblings:
			b.WriteString("├─")
		case isDeepest:
			b.WriteString("└─")
		case order == hasRemainingSiblings:
			b.WriteString("│ ")
		default:
			b.WriteString("  ")
		}
		b.WriteByte(' ')
	}
	return b.String()
}

const (
	// maxExplanationBytes and maxExplanationLines cap a rendered
	// explanation, marker included. Error renders the explanation
	// implicitly, so its size must stay reasonable whatever the conflict.
	maxExplanationBytes = 64 << 10
	maxExplanationLines = 2000
	// explanationTruncated ends an explanation that reached the cap.
	explanationTruncated = "… explanation truncated\n"

	// maxGraphvizBytes caps the Graphviz rendering of a conflict graph.
	maxGraphvizBytes = 1 << 20
	// graphvizTruncated marks a Graphviz rendering that reached the cap.
	graphvizTruncated = "/* graph truncated */"
)

// boundedText accumulates the lines of an explanation up to the size cap.
type boundedText struct {
	b     strings.Builder
	lines int
	// full is set once a line did not fit; every later line is dropped.
	full bool
}

// printf appends a line (format ends in a newline) unless it would exceed
// the cap, in which case it and every later line are dropped.
func (t *boundedText) printf(format string, args ...any) {
	if t.full {
		return
	}
	line := fmt.Sprintf(format, args...)
	lines := strings.Count(line, "\n")
	if t.lines+lines > maxExplanationLines-1 || t.b.Len()+len(line) > maxExplanationBytes-len(explanationTruncated) {
		t.full = true
		return
	}
	t.b.WriteString(line)
	t.lines += lines
}

// String returns the text, with a marker if lines were dropped.
func (t *boundedText) String() string {
	if t.full {
		return t.b.String() + explanationTruncated
	}
	return t.b.String()
}

// displayUnsat renders a ConflictGraph as a user-friendly explanation.
type displayUnsat struct {
	g           *ConflictGraph
	merged      map[SolvableID]*mergedConflictNode
	installable []bool
	missing     []bool
	interner    Interner
}

func newDisplayUnsat(g *ConflictGraph, interner Interner) *displayUnsat {
	return &displayUnsat{
		g:           g,
		merged:      g.simplify(interner),
		installable: g.installableSet(),
		missing:     g.missingSet(),
		interner:    interner,
	}
}

// excludedReason returns the reason a solvable was excluded, if it was.
func (d *displayUnsat) excludedReason(s SolvableID) (StringID, bool) {
	g := &d.g.g
	for n := range g.nodes {
		w := g.nodes[n].weight
		if w.Kind != ConflictNodeSolvable || w.Solvable != s {
			continue
		}
		for _, e := range g.outgoing(n) {
			ew := g.edges[e].weight
			if ew.Kind == ConflictEdgeConflict && ew.Cause == CauseExcluded {
				t := g.nodes[g.target(e)].weight
				if t.Kind != ConflictNodeExcluded {
					panic(internalError("an excluded edge must point to an excluded node"))
				}
				return t.Reason, true
			}
		}
		return 0, false
	}
	return 0, false
}

type displayOpKind uint8

const (
	opRequirement displayOpKind = iota
	opCandidate
)

type displayOp struct {
	kind  displayOpKind
	req   Requirement
	edges []int
	node  int
	ind   indenter
}

// requirementOps groups requires edges by requirement and orders the groups
// so that the installable ones are popped (and so printed) first.
func (d *displayUnsat) requirementOps(edges []int, parent indenter) []displayOp {
	g := &d.g.g
	var ops []displayOp
	for _, chunk := range chunkByRequirement(g, edges) {
		ops = append(ops, displayOp{kind: opRequirement, req: g.edges[chunk[0]].weight.Requirement, edges: chunk})
	}
	anyInstallable := func(op displayOp) bool {
		for _, e := range op.edges {
			if d.installable[g.target(e)] {
				return true
			}
		}
		return false
	}
	sort.SliceStable(ops, func(i, j int) bool { return !anyInstallable(ops[i]) && anyInstallable(ops[j]) })
	for i := range ops {
		ops[i].ind = parent.pushLevel()
	}
	if len(ops) > 0 {
		ops[0].ind.setLast()
	}
	return ops
}

// candidateOps turns requires edges into candidate operations, displaying
// each merged group once.
func (d *displayUnsat) candidateOps(edges []int, parent indenter, onlyInstallable bool) []displayOp {
	g := &d.g.g
	var ops []displayOp
	seenMerged := map[SolvableID]bool{}
	for _, e := range edges {
		t := g.target(e)
		if onlyInstallable && !d.installable[t] {
			continue
		}
		w := g.nodes[t].weight
		if onlyInstallable {
			// Panics on non-solvable nodes, like resolvo.
			if w.solvableOrRoot().root {
				continue
			}
		} else if w.Kind != ConflictNodeSolvable {
			continue
		}
		if seenMerged[w.Solvable] {
			continue
		}
		if m, ok := d.merged[w.Solvable]; ok {
			for _, id := range m.ids {
				seenMerged[id] = true
			}
		}
		ops = append(ops, displayOp{kind: opCandidate, node: t, ind: parent.pushLevel()})
	}
	if len(ops) > 0 {
		ops[0].ind.setLast()
	}
	return ops
}

// fmtGraph renders the requirement trees below topLevelEdges.
//
// resolvo marks a candidate as reported only when it belongs to a merged
// group, so every other candidate is expanded again each time the tree
// reaches it: the output is exponential in the depth of an acyclic graph
// and infinite for a cyclic one. Here a candidate that would require or
// constrain something is expanded once; later visits print a
// back-reference, so the output is linear in the number of edges.
func (d *displayUnsat) fmtGraph(b *boundedText, topLevelEdges []int, topLevelIndent bool) {
	g := &d.g.g
	in := d.interner
	reported := map[solvableOrRoot]bool{}
	expanded := map[solvableOrRoot]bool{}

	// Only requires edges are of interest here.
	var reqEdges []int
	for _, e := range topLevelEdges {
		if g.edges[e].weight.Kind == ConflictEdgeRequires {
			reqEdges = append(reqEdges, e)
		}
	}
	stack := d.requirementOps(reqEdges, newIndenter(topLevelIndent))

	for len(stack) > 0 && !b.full {
		op := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		topLevel := op.ind.isAtTopLevel()
		indent := op.ind.indent()

		switch op.kind {
		case opRequirement:
			installable := false
			for _, e := range op.edges {
				if d.installable[g.target(e)] {
					installable = true
					break
				}
			}
			req := op.req.Display(in)
			missing := len(op.edges) == 1 && g.nodes[g.target(op.edges[0])].weight.Kind == ConflictNodeUnresolved
			switch {
			case missing:
				// No candidates for the requirement.
				if topLevel {
					b.printf("%sNo candidates were found for %s.\n", indent, req)
				} else {
					b.printf("%s%s, for which no candidates were found.\n", indent, req)
				}
			case installable:
				// Installable (only mentioned for top-level requirements).
				if topLevel {
					b.printf("%s%s can be installed with any of the following options:\n", indent, req)
				} else {
					b.printf("%s%s, which can be installed with any of the following options:\n", indent, req)
				}
				stack = append(stack, d.candidateOps(op.edges, op.ind, true)...)
			default:
				// Not installable: the conflict is further down the tree.
				if topLevel {
					b.printf("%s%s cannot be installed because there are no viable options:\n", indent, req)
				} else {
					b.printf("%s%s, which cannot be installed because there are no viable options:\n", indent, req)
				}
				stack = append(stack, d.candidateOps(op.edges, op.ind, false)...)
			}

		case opCandidate:
			sr := g.nodes[op.node].weight.solvableOrRoot()
			if reported[sr] {
				continue
			}
			var version string
			if m, ok := d.merged[sr.id]; ok && !sr.root {
				for _, id := range m.ids {
					reported[solvableOrRoot{id: id}] = true
				}
				version = DisplayMergedSolvables(in, m.ids)
			} else if !sr.root {
				version = DisplayMergedSolvables(in, []SolvableID{sr.id})
			} else {
				version = "<root>"
			}

			out := g.outgoing(op.node)
			var excluded *StringID
			alreadyInstalled, constrainsConflict := false, false
			for _, e := range out {
				w := g.edges[e].weight
				if w.Kind != ConflictEdgeConflict {
					continue
				}
				switch w.Cause {
				case CauseExcluded:
					if excluded == nil {
						t := g.nodes[g.target(e)].weight
						if t.Kind != ConflictNodeExcluded {
							panic(internalError("an excluded edge must point to an excluded node"))
						}
						r := t.Reason
						excluded = &r
					}
				case CauseForbidMultipleInstances:
					alreadyInstalled = true
				case CauseConstrains:
					constrainsConflict = true
				}
			}
			isLeaf := len(out) == 0

			switch {
			case excluded != nil:
				b.printf("%s%s is excluded because %s\n", indent, version, in.DisplayString(*excluded))
			case isLeaf:
				b.printf("%s%s\n", indent, version)
			case alreadyInstalled:
				b.printf("%s%s, which conflicts with the versions reported above.\n", indent, version)
			case expanded[sr]:
				b.printf("%s%s, as reported above.\n", indent, version)
			case constrainsConflict:
				expanded[sr] = true
				var vss []VersionSetID
				for _, e := range out {
					w := g.edges[e].weight
					if w.Kind != ConflictEdgeConflict || w.Cause != CauseConstrains {
						continue
					}
					if len(vss) > 0 && vss[len(vss)-1] == w.VersionSet {
						continue
					}
					vss = append(vss, w.VersionSet)
				}
				b.printf("%s%s would constrain\n", indent, version)
				sub := op.ind.pushLevel()
				for i, vs := range vss {
					if i == len(vss)-1 {
						sub.setLast()
					}
					b.printf("%s%s %s, which conflicts with any installable versions previously reported\n",
						sub.indent(), in.DisplayName(in.VersionSetName(vs)), in.DisplayVersionSet(vs))
				}
			default:
				expanded[sr] = true
				b.printf("%s%s would require\n", indent, version)
				for _, e := range out {
					if g.edges[e].weight.Kind != ConflictEdgeRequires {
						panic(internalError("expected a requires edge"))
					}
				}
				stack = append(stack, d.requirementOps(out, op.ind)...)
			}
		}
	}
}

// String renders the explanation.
func (d *displayUnsat) String() string {
	g := &d.g.g
	in := d.interner
	var b boundedText

	var topMissing, topConflicts []int
	for _, e := range g.outgoing(d.g.root) {
		if d.missing[g.target(e)] {
			topMissing = append(topMissing, e)
		} else {
			topConflicts = append(topConflicts, e)
		}
	}

	if len(topMissing) > 0 {
		d.fmtGraph(&b, topMissing, false)
	}

	if len(topConflicts) > 0 {
		b.printf("The following packages are incompatible\n")
		d.fmtGraph(&b, topConflicts, true)

		// Conflicts caused by locked packages. Every Lock clause produces
		// its own root edge, one per forbidden candidate, so the lines are
		// deduplicated by the locked solvable.
		reportedLocked := map[SolvableID]bool{}
		var lines []string
		for _, e := range g.outgoing(d.g.root) {
			w := g.edges[e].weight
			if w.Kind == ConflictEdgeRequires {
				continue
			}
			switch w.Cause {
			case CauseConstrains:
				lines = append(lines, fmt.Sprintf("the constraint %s %s cannot be fulfilled",
					in.DisplayName(in.VersionSetName(w.VersionSet)), in.DisplayVersionSet(w.VersionSet)))
			case CauseForbidMultipleInstances:
				panic(internalError("forbid-multiple edge from the root"))
			case CauseLocked:
				if reportedLocked[w.Locked] {
					continue
				}
				reportedLocked[w.Locked] = true
				// A locked solvable that is itself excluded rules the
				// package out entirely; report the reason.
				if reason, ok := d.excludedReason(w.Locked); ok {
					lines = append(lines, fmt.Sprintf("%s is locked, but it is excluded because %s",
						DisplayMergedSolvables(in, []SolvableID{w.Locked}), in.DisplayString(reason)))
				} else {
					lines = append(lines, fmt.Sprintf("%s is locked, but another version is required as reported above",
						DisplayMergedSolvables(in, []SolvableID{w.Locked})))
				}
			}
		}

		ind := newIndenter(true)
		for i, line := range lines {
			order := hasRemainingSiblings
			if i == len(lines)-1 {
				order = lastChild
			}
			b.printf("%s%s\n", ind.pushLevelWithOrder(order).indent(), line)
		}
	}
	return b.String()
}
