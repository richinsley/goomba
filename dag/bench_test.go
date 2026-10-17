package dag

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
)

// randomGraph returns a DAG of n nodes and m distinct edges drawn uniformly
// from the pairs i -> j with i < j, with the nodes added in a shuffled
// order so that Connect has to reorder.
func randomGraph(tb testing.TB, n, m int) (*Graph[int], [][2]int) {
	tb.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	g := New[int](1)
	for _, k := range rng.Perm(n) {
		g.AddNode(k)
	}
	seen := make(map[[2]int]bool, m)
	edges := make([][2]int, 0, m)
	for len(edges) < m {
		i, j := rng.IntN(n), rng.IntN(n)
		if i == j {
			continue
		}
		e := [2]int{min(i, j), max(i, j)}
		if !seen[e] {
			seen[e] = true
			edges = append(edges, e)
		}
	}
	return g, edges
}

func connectAll(tb testing.TB, g *Graph[int], edges [][2]int) {
	for _, e := range edges {
		if err := g.Connect(0, e[0], e[1]); err != nil {
			tb.Fatal(err)
		}
	}
}

// BenchmarkRecompute times one dimension's full topology pass over 10,000
// nodes and 50,000 edges: with descendent bit sets (the default at this
// size) and without them (the path for graphs above the closure limit).
func BenchmarkRecompute(b *testing.B) {
	g, edges := randomGraph(b, 10000, 50000)
	connectAll(b, g, edges)
	for _, c := range []struct {
		name  string
		limit int
	}{{"closure", defaultClosureLimit}, {"walk", 0}} {
		b.Run(c.name, func(b *testing.B) {
			g.closureLimit = c.limit
			for b.Loop() {
				g.invalidate(0)
				g.topology(0)
			}
			b.ReportMetric(float64(g.MaxOrdinal(0)+1), "layers")
		})
	}
}

// BenchmarkConnect times building the 10,000-node, 50,000-edge graph,
// edge by edge, with every cycle check and reorder.
func BenchmarkConnect(b *testing.B) {
	_, edges := randomGraph(b, 10000, 50000)
	for b.Loop() {
		b.StopTimer()
		g, _ := randomGraph(b, 10000, 0)
		b.StartTimer()
		connectAll(b, g, edges)
	}
}

// backToFront returns n nodes 0..n-1, added in that order, and the edges of
// the chain n-1 -> n-2 -> ... -> 0 in the order that is worst for Connect's
// incremental topological order: each edge puts a later-added node in front
// of the whole chain built so far, so Connect reorders all of it.
func backToFront(n int) (*Graph[int], [][2]int) {
	g := New[int](1)
	for i := range n {
		g.AddNode(i)
	}
	edges := make([][2]int, 0, n-1)
	for i := range n - 1 {
		edges = append(edges, [2]int{i + 1, i})
	}
	return g, edges
}

// BenchmarkChainBackToFront times building the backToFront chain edge by
// edge (quadratic) and as one ConnectAll (linear).
func BenchmarkChainBackToFront(b *testing.B) {
	for _, n := range []int{2000, 8000} {
		_, edges := backToFront(n)
		chain := func() *Graph[int] {
			g, _ := backToFront(n)
			return g
		}
		b.Run(fmt.Sprintf("Connect/%d", n), func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				g := chain()
				b.StartTimer()
				connectAll(b, g, edges)
			}
		})
		b.Run(fmt.Sprintf("ConnectAll/%d", n), func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				g := chain()
				b.StartTimer()
				if err := g.ConnectAll(0, edges); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkConnectAll times building the 10,000-node, 50,000-edge graph
// with one ConnectAll.
func BenchmarkConnectAll(b *testing.B) {
	_, edges := randomGraph(b, 10000, 50000)
	for b.Loop() {
		b.StopTimer()
		g, _ := randomGraph(b, 10000, 0)
		b.StartTimer()
		if err := g.ConnectAll(0, edges); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCachedQueryDuringCompute times a query on dimension 1's current
// topology while other goroutines keep recomputing dimension 0.
func BenchmarkCachedQueryDuringCompute(b *testing.B) {
	g, edges := randomGraph(b, 10000, 50000)
	connectAll(b, g, edges)
	g2 := New[int](2)
	for _, k := range g.Nodes() {
		g2.AddNode(k)
	}
	for _, e := range edges {
		g2.Connect(0, e[0], e[1])
		g2.Connect(1, e[0], e[1])
	}
	g2.topology(1)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				g2.state[0].topo.Store(nil) // test-only: a stale dimension 0
				g2.topology(0)
			}
		}
	}()
	for b.Loop() {
		g2.Ordinal(1, 0)
	}
	close(stop)
	wg.Wait()
}

// BenchmarkDescendents times every node's descendent query on the large
// graph.
func BenchmarkDescendents(b *testing.B) {
	g, edges := randomGraph(b, 10000, 50000)
	connectAll(b, g, edges)
	g.topology(0)
	for b.Loop() {
		for k := range 10000 {
			g.Descendents(0, k)
		}
	}
}

// BenchmarkPydaggerScenarios is pydagger's tests/test_benchmark.py
// TestBenchmark: calculate_topology() timed over its graph shapes.
func BenchmarkPydaggerScenarios(b *testing.B) {
	for _, sc := range []struct {
		name  string
		build func(testing.TB) *genGraph
	}{
		{"linear_20", func(t testing.TB) *genGraph { return buildLinearChain(t, 20, 1) }},
		{"linear_50", func(t testing.TB) *genGraph { return buildLinearChain(t, 50, 1) }},
		{"linear_100", func(t testing.TB) *genGraph { return buildLinearChain(t, 100, 1) }},
		{"linear_200", func(t testing.TB) *genGraph { return buildLinearChain(t, 200, 1) }},
		{"fan_out_3x3", func(t testing.TB) *genGraph { return buildWideFanOut(t, 3, 3, 1) }},
		{"fan_out_4x3", func(t testing.TB) *genGraph { return buildWideFanOut(t, 4, 3, 1) }},
		{"fan_out_3x4", func(t testing.TB) *genGraph { return buildWideFanOut(t, 3, 4, 1) }},
		{"diamond_4x3", func(t testing.TB) *genGraph { return buildDiamond(t, 4, 3, 1) }},
		{"diamond_5x4", func(t testing.TB) *genGraph { return buildDiamond(t, 5, 4, 1) }},
		{"diamond_6x3", func(t testing.TB) *genGraph { return buildDiamond(t, 6, 3, 1) }},
		{"diamond_5x5", func(t testing.TB) *genGraph { return buildDiamond(t, 5, 5, 1) }},
		{"random_30_p20", func(t testing.TB) *genGraph { return buildRandomDAG(t, 30, 0.2, 42, 1) }},
		{"random_50_p15", func(t testing.TB) *genGraph { return buildRandomDAG(t, 50, 0.15, 42, 1) }},
		{"random_50_p10", func(t testing.TB) *genGraph { return buildRandomDAG(t, 50, 0.10, 42, 1) }},
		{"random_100_p05", func(t testing.TB) *genGraph { return buildRandomDAG(t, 100, 0.05, 42, 1) }},
		{"parallel_5x20", func(t testing.TB) *genGraph { return buildParallelChains(t, 5, 20, 1) }},
		{"parallel_10x10", func(t testing.TB) *genGraph { return buildParallelChains(t, 10, 10, 1) }},
		{"hourglass_10_10_2", func(t testing.TB) *genGraph { return buildHourglass(t, 10, 10, 2, 1) }},
		{"hourglass_15_15_2", func(t testing.TB) *genGraph { return buildHourglass(t, 15, 15, 2, 1) }},
		{"diamond_4x3_dual", func(t testing.TB) *genGraph { return buildDiamond(t, 4, 3, 2) }},
	} {
		b.Run(sc.name, func(b *testing.B) {
			g := sc.build(b).g
			for b.Loop() {
				for d := range g.Dims() {
					g.invalidate(d)
					g.topology(d)
				}
			}
		})
	}
}
