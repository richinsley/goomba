// Install ordering after rattler crates/rattler_conda_types/src/repo_data/
// topological_sort.rs (sort_topologically, break_cycles) (BSD-3-Clause, see
// NOTICE): noarch packages link after the platform packages of their cycle.

package goomba

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/richinsley/goomba/dag"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/spec"
)

// Dimensions of the install plan's graph. Every edge runs from the package
// that must come first to the one that depends on it.
const (
	// dimLink orders linking: dependency → dependent.
	dimLink = 0
	// dimCycle holds the dependency edges moved out of dimLink to break
	// cycles.
	dimCycle = 1
	// dimConstrains holds constrains edges, constrained → constraining
	// package, for impact analysis.
	dimConstrains = 2
)

// specScan is what the install needs from the locked packages' depends and
// constrains: the edges between locked packages and the specs naming
// virtual packages. Everything else of a parsed spec is dropped at once,
// so memory grows with the number of distinct edges, not with the number
// of spec strings.
type specScan struct {
	// deps are (dependency, dependent) and cons (constrained,
	// constraining) pairs of indexes into the records, without
	// duplicates or self-edges, sorted.
	deps, cons [][2]int32
	// virt are the depends and constrains entries naming a virtual
	// package, in record order and then list order (depends first), each
	// (package, spec, kind) once.
	virt []virtualSpec
	// vspecs are the distinct virtual specs virt refers to, parsed once.
	vspecs []parsedSpec
}

// parsedSpec is a match spec as written and parsed.
type parsedSpec struct {
	text string
	ms   spec.MatchSpec
}

// virtualSpec is a depends or constrains entry naming a virtual package.
type virtualSpec struct {
	pkg       int32 // index of the record
	spec      int32 // index into specScan.vspecs
	constrain bool  // from constrains rather than depends
}

// scanSpecs parses every depends and constrains entry of recs, which must
// have unique names. A spec that does not parse makes the lockfile
// unusable: the solver that produced it could not have read it either.
// Names that are not exact, not locked, virtual (kept in virt) or the
// package's own give no edge.
func scanSpecs(recs []*record.PackageRecord) (*specScan, error) {
	index := make(map[string]int32, len(recs))
	for i, r := range recs {
		index[strings.ToLower(r.Name)] = int32(i)
	}
	sc := &specScan{}
	vindex := map[string]int32{}       // spec text -> index into sc.vspecs
	seenVirt := map[virtualSpec]bool{} // per package, reset below
	var targets []int32                // scratch, reused per package and list
	for i, r := range recs {
		clear(seenVirt)
		for _, l := range []struct {
			what      string
			in        []string
			out       *[][2]int32
			constrain bool
		}{{"depends", r.Depends, &sc.deps, false}, {"constrains", r.Constrains, &sc.cons, true}} {
			targets = targets[:0]
			for _, s := range l.in {
				if k, ok := vindex[s]; ok { // a virtual spec seen before
					sc.addVirt(seenVirt, virtualSpec{pkg: int32(i), spec: k, constrain: l.constrain})
					continue
				}
				ms, err := spec.ParseMatchSpec(s)
				if err != nil {
					return nil, fmt.Errorf("goomba: %s: %s %q: %w", r.Dist(), l.what, s, err)
				}
				if !ms.Name.IsExact() {
					continue
				}
				name := ms.Name.String()
				if strings.HasPrefix(name, "__") && len(name) > 2 {
					k := int32(len(sc.vspecs))
					sc.vspecs = append(sc.vspecs, parsedSpec{text: s, ms: ms})
					vindex[s] = k
					sc.addVirt(seenVirt, virtualSpec{pkg: int32(i), spec: k, constrain: l.constrain})
					continue
				}
				if j, ok := index[name]; ok && int(j) != i {
					targets = append(targets, j)
				}
			}
			slices.Sort(targets)
			for _, j := range slices.Compact(targets) {
				*l.out = append(*l.out, [2]int32{j, int32(i)})
			}
		}
	}
	slices.SortFunc(sc.deps, cmpEdge)
	slices.SortFunc(sc.cons, cmpEdge)
	return sc, nil
}

// addVirt records v unless the package already has it.
func (sc *specScan) addVirt(seen map[virtualSpec]bool, v virtualSpec) {
	if !seen[v] {
		seen[v] = true
		sc.virt = append(sc.virt, v)
	}
}

// installPlan is the link order of a transaction.
type installPlan struct {
	// graph has one node per package name and the three dimensions above.
	graph *dag.Graph[string]
	// layers are dimLink's layers as indexes into the planned records: the
	// packages of one layer may be linked concurrently, layers in order.
	layers [][]int
	// moved are the dependency edges (dependency, dependent) moved to
	// dimCycle, in order.
	moved [][2]string
}

// planInstall builds the install graph for recs, which must be sorted by
// name with unique names. It never depends on the order of depends entries.
//
// Dependency edges that lie on a cycle are moved to dimCycle before any edge
// is added, by two rules applied in turn:
//
//  1. rattler's: a platform package's dependency on a noarch package of its
//     cycle is moved, so noarch packages link after the platform packages
//     of their cycle (python before pip);
//  2. on cycles that remain (only platform packages, or only noarch ones),
//     the back edges of a depth-first search are moved; the search starts
//     from the cycle's first name and follows edges in name order.
//
// Rule 2 is goomba's: real environments hold such cycles (rattler's own test
// data has libzlib → libgcc-ng → _openmp_mutex → llvm-openmp → libzlib),
// and libsolv and rattler break them too, rattler with a depth-first search
// that depends on the order of the depends entries. A package depending on
// its own name gets no edge. Cycles among constrains edges are broken the
// same way, by dropping the back edges.
func planInstall(recs []*record.PackageRecord, sc *specScan) (*installPlan, error) {
	n := len(recs)
	g := dag.New[string](3)
	for _, r := range recs {
		if !g.AddNode(r.Name) {
			return nil, fmt.Errorf("goomba: plan: duplicate package %q", r.Name)
		}
	}
	deps := sc.deps
	noarch := func(i int32) bool { return recs[i].Noarch != "" }

	comp := sccs(n, deps)
	var rest, moved [][2]int32
	for _, e := range deps {
		if comp[e[0]] == comp[e[1]] && noarch(e[0]) && !noarch(e[1]) {
			moved = append(moved, e)
		} else {
			rest = append(rest, e)
		}
	}
	back := backEdges(n, rest)
	var keep [][2]int32
	for _, e := range rest {
		if back[e] {
			moved = append(moved, e)
		} else {
			keep = append(keep, e)
		}
	}
	slices.SortFunc(moved, cmpEdge)

	cons := slices.Clone(sc.cons)
	back = backEdges(n, cons)
	cons = slices.DeleteFunc(cons, func(e [2]int32) bool { return back[e] })

	p := &installPlan{graph: g}
	for _, d := range []struct {
		dim   int
		edges [][2]int32
	}{{dimLink, keep}, {dimCycle, moved}, {dimConstrains, cons}} {
		named := make([][2]string, len(d.edges))
		for i, e := range d.edges {
			named[i] = [2]string{recs[e[0]].Name, recs[e[1]].Name}
		}
		if err := g.ConnectAll(d.dim, named); err != nil {
			// Unreachable: the rules above leave every dimension acyclic.
			// Fail closed should that ever change.
			var ce *dag.CycleError[string]
			if errors.As(err, &ce) {
				return nil, fmt.Errorf("goomba: plan: dependency cycle %s: %w", strings.Join(append(ce.Path, ce.Path[0]), " -> "), err)
			}
			return nil, fmt.Errorf("goomba: plan: %w", err)
		}
		if d.dim == dimCycle {
			p.moved = named
		}
	}
	index := make(map[string]int, n)
	for i, r := range recs {
		index[r.Name] = i
	}
	for _, layer := range g.Layers(dimLink) {
		l := make([]int, len(layer))
		for i, name := range layer {
			l[i] = index[name]
		}
		p.layers = append(p.layers, l)
	}
	return p, nil
}

func cmpEdge(a, b [2]int32) int {
	if c := cmp.Compare(a[0], b[0]); c != 0 {
		return c
	}
	return cmp.Compare(a[1], b[1])
}

// backEdges returns the back edges of a depth-first search over the edges
// that lie on a cycle (both ends in one strongly connected component). The
// search visits start nodes and successors in index order, so the result
// depends only on the edge set, and removing the back edges leaves the graph
// acyclic.
func backEdges(n int, edges [][2]int32) map[[2]int32]bool {
	comp := sccs(n, edges)
	adj := make([][]int32, n)
	for _, e := range edges {
		if comp[e[0]] == comp[e[1]] {
			adj[e[0]] = append(adj[e[0]], e[1])
		}
	}
	for _, a := range adj {
		slices.Sort(a)
	}
	const (
		white = iota
		grey
		black
	)
	state := make([]uint8, n)
	back := map[[2]int32]bool{}
	type frame struct{ v, next int32 }
	var stack []frame
	for s := range int32(n) {
		if state[s] != white || len(adj[s]) == 0 {
			continue
		}
		state[s] = grey
		stack = append(stack[:0], frame{v: s})
		for len(stack) > 0 {
			top := len(stack) - 1
			v := stack[top].v
			if int(stack[top].next) == len(adj[v]) {
				state[v] = black
				stack = stack[:top]
				continue
			}
			w := adj[v][stack[top].next]
			stack[top].next++
			switch state[w] {
			case white:
				state[w] = grey
				stack = append(stack, frame{v: w})
			case grey:
				back[[2]int32{v, w}] = true
			}
		}
	}
	return back
}

// sccs returns, for each of n nodes, the number of its strongly connected
// component under edges (Tarjan's algorithm, iterative so that deep graphs
// cannot exhaust the stack).
func sccs(n int, edges [][2]int32) []int {
	adj := make([][]int32, n)
	for _, e := range edges {
		adj[e[0]] = append(adj[e[0]], e[1])
	}
	index := make([]int, n) // 0: unvisited, else discovery number + 1
	low := make([]int, n)
	onStack := make([]bool, n)
	comp := make([]int, n)
	var stack []int
	type frame struct{ v, next int }
	var call []frame
	counter, ncomp := 0, 0
	visit := func(v int) {
		counter++
		index[v], low[v] = counter, counter
		stack = append(stack, v)
		onStack[v] = true
		call = append(call, frame{v: v})
	}
	for s := range n {
		if index[s] != 0 {
			continue
		}
		visit(s)
		for len(call) > 0 {
			top := len(call) - 1
			v := call[top].v
			if call[top].next < len(adj[v]) {
				w := int(adj[v][call[top].next])
				call[top].next++
				if index[w] == 0 {
					visit(w)
				} else if onStack[w] {
					low[v] = min(low[v], index[w])
				}
				continue
			}
			call = call[:top]
			if top > 0 {
				p := call[top-1].v
				low[p] = min(low[p], low[v])
			}
			if low[v] == index[v] {
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp[w] = ncomp
					if w == v {
						break
					}
				}
				ncomp++
			}
		}
	}
	return comp
}
