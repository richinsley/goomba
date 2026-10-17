// Package dag computes layered topological order over a multi-dimensional
// DAG: per dimension, ordinals, descendents, ascendents and components.
//
// It is a Go port of the topology engine of pydagger, a layered DAG library
// by goomba's author. pydagger connects pins on nodes and emits signals; dag
// keeps only what goomba needs: plain node-to-node edges, one independent
// edge set per dimension, and the topology computed from them.
//
// A [Graph] holds one set of nodes and Dims edge sets. Every dimension is
// acyclic on its own: [Graph.Connect] refuses an edge that would close a
// cycle in its dimension and reports the existing path in a [*CycleError].
// An edge refused in one dimension may still be added to another, so a
// cyclic relation can be split across dimensions. [Graph.ConnectAll] adds a
// batch of edges all or nothing in time linear in the graph, whatever their
// order, which suits building a graph from untrusted input. goomba uses
// three dimensions:
//
//   - 0, link order: an edge runs from a dependency to its dependent. Nodes
//     in one layer can be linked concurrently; layers run in order.
//   - 1, requirement edges moved out of dimension 0 to break cycles.
//   - 2, constrains edges, for impact analysis.
//
// For each dimension the graph computes, lazily on the first query after a
// change and in one pass, the ordinal of every node (the length of the
// longest path from a root to it) and the connected components, and, for
// graphs of up to 16,384 nodes, the descendents of every node (everything
// reachable from it) as bit sets. Larger graphs answer descendent and
// ascendent queries by walking the edges instead, which bounds memory. Every
// result is deterministic: nodes with the same ordinal are listed in the
// order they were added, and nothing depends on map iteration order.
package dag
