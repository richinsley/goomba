# WP10 — `dag`: multi-dimensional layered topology (port of pydagger)

Read `AGENTS.md` first. You own `dag/`.

## Goal

Port pydagger's topology engine (`$GOOMBA_REFS/pydagger`,
especially `DaggerGraph.calculate_topology` in `pydagger/graph.py`) to Go.
pydagger is MIT and written by the same author as goomba; the file header
still names it. The pins, signals and auto-cloning are UI machinery and are
**not** ported: goomba needs plain node-to-node edges per dimension.

goomba will use three dimensions over the same package nodes, each acyclic on
its own:

- 0 — link order: an edge from a dependency to its dependent. Layers drive
  parallel linking (packages in one layer link concurrently; layers run in
  order; uninstall runs layers in reverse).
- 1 — requirement edges removed from dimension 0 to break cycles (conda has
  them: `python` depends on `pip` and `pip` on `python`; the edge from the
  platform package to the noarch one moves here).
- 2 — constrains edges (e.g. `python` → `python_abi`), for impact analysis.

For poppi's `base-3.11` (28 packages) dimension 0 has 10 layers; the widest
holds 11 packages.

## Required API

```go
// Graph is a set of nodes with Dims independent edge sets. Each dimension
// is a DAG; Connect refuses an edge that would close a cycle in its dimension.
type Graph[K comparable] struct{ ... }
func New[K comparable](dims int) *Graph[K]
func (g *Graph[K]) AddNode(k K) bool                         // false if present; insertion order is remembered
func (g *Graph[K]) RemoveNode(k K) bool
func (g *Graph[K]) Connect(dim int, from, to K) error         // from = dependency, to = dependent; duplicate = no-op
func (g *Graph[K]) Disconnect(dim int, from, to K) bool
// *CycleError carries the existing path to → … → from that the edge would close.
type CycleError[K comparable] struct{ Dim int; From, To K; Path []K }

// Topology is computed lazily on first query after a change (one pass per
// dimension: Kahn ordinals as longest path from roots, reverse sweep for
// descendents, union-find for components), as pydagger does.
func (g *Graph[K]) Ordinal(dim int, k K) int
func (g *Graph[K]) MaxOrdinal(dim int) int
func (g *Graph[K]) Layers(dim int) [][]K                       // by ordinal; within a layer, insertion order
func (g *Graph[K]) Descendents(dim int, k K) []K               // sorted by ordinal, then insertion order
func (g *Graph[K]) Ascendents(dim int, k K) []K
func (g *Graph[K]) DescendentsAcross(dims []int, k K) []K      // reachability over the union of dimensions
func (g *Graph[K]) Subgraphs(dim int) [][]K
func (g *Graph[K]) Subgraph(dim int, k K) int
func (g *Graph[K]) Roots(dim int) []K                          // no incoming edges (pydagger top level)
func (g *Graph[K]) Leaves(dim int) []K                         // no outgoing edges (bottom level)
func (g *Graph[K]) Nodes() []K                                 // insertion order
```

Everything deterministic. Not safe for concurrent mutation; document it.
Reads after the topology is computed may run concurrently.

## Tests

- Port pydagger's topology tests: `tests/test_graph.py`,
  `tests/test_descendents.py`, `tests/test_optimization.py`, using its
  generators in `tests/graph_generators.py` and `tests/topology_snapshot.py`
  where they help (76 test functions in the first two files; skip pin, signal
  and auto-clone tests and list them as skipped).
- Multi-dimension cases: an edge that is a cycle in dim 0 is accepted in dim 1;
  `DescendentsAcross`.
- The conda case: build the 28-package `base-3.11` graph from
  `$GOOMBA_REF_ROOT/envs/base-3.11/conda-meta/*.json` (names from
  `depends`, ignoring versions; edges only to installed names), put
  `python → pip` in dimension 1 because `Connect` in dimension 0 returns a
  `CycleError`, and assert 10 layers with layer 3 holding 11 packages.
- Benchmark: 10,000 nodes and 50,000 edges, recompute time.

Fuzz: `FuzzConnect` (random edge sequences: never a cycle accepted, ordinals
consistent with every edge, descendents equal brute-force reachability).
