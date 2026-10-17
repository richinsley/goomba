package dag

import (
	"testing"
)

// differ applies the same operation to a Graph and to the reference model
// and fails the test when their results differ.
type differ struct {
	t   testing.TB
	g   *Graph[int]
	ref *refGraph[int]
	// counts of outcomes, for tests that must exercise both
	cycles, added int
}

func newDiffer(t testing.TB, dims, keys int) *differ {
	x := &differ{t: t, g: New[int](dims), ref: newRef[int](dims)}
	for k := range keys {
		x.add(k)
	}
	return x
}

func (x *differ) connect(d, a, b int) {
	x.t.Helper()
	aKnown, bKnown := x.ref.has(a), x.ref.has(b)
	err := x.g.Connect(d, a, b)
	cycle, added := x.ref.connect(d, a, b)
	checkConnect(x.t, err, d, a, b, cycle, aKnown, bKnown)
	x.count(cycle != nil, added)
}

func (x *differ) connectAll(d int, edges [][2]int) {
	x.t.Helper()
	before := len(x.g.edges)
	err := x.g.ConnectAll(d, edges)
	fail, cycle, aKnown, bKnown := x.ref.connectAll(d, edges)
	if fail < 0 {
		if err != nil {
			x.t.Fatalf("ConnectAll(%d, %v) = %v, want nil", d, edges, err)
		}
	} else {
		checkConnect(x.t, err, d, edges[fail][0], edges[fail][1], cycle, aKnown, bKnown)
		if len(x.g.edges) != before {
			x.t.Fatalf("failed ConnectAll(%d, %v) added edges", d, edges)
		}
	}
	x.count(cycle != nil, fail < 0 && len(x.g.edges) > before)
}

func (x *differ) count(cycle, added bool) {
	if cycle {
		x.cycles++
	}
	if added {
		x.added++
	}
}

func (x *differ) disconnect(d, a, b int) {
	x.t.Helper()
	if got, want := x.g.Disconnect(d, a, b), x.ref.disconnect(d, a, b); got != want {
		x.t.Fatalf("Disconnect(%d, %d, %d) = %v, want %v", d, a, b, got, want)
	}
}

func (x *differ) add(a int) {
	x.t.Helper()
	if got, want := x.g.AddNode(a), x.ref.addNode(a); got != want {
		x.t.Fatalf("AddNode(%d) = %v, want %v", a, got, want)
	}
}

func (x *differ) remove(a int) {
	x.t.Helper()
	if got, want := x.g.RemoveNode(a), x.ref.removeNode(a); got != want {
		x.t.Fatalf("RemoveNode(%d) = %v, want %v", a, got, want)
	}
}

func (x *differ) check() {
	x.t.Helper()
	checkGraph(x.t, x.g, x.ref, -1)
}

// fuzzOp builds one FuzzConnect operation: kind 0-3 Connect(a, b), 4
// ConnectAll of the b%6 edges in the following byte pairs, 5 Disconnect(a,
// b), 6 RemoveNode(a), 7 AddNode(a); check runs the full comparison after
// it.
func fuzzOp(kind, dim byte, a, b int, check bool) []byte {
	op := kind | dim<<3
	if check {
		op |= 0x80
	}
	return []byte{op, byte(a), byte(b)}
}

// fuzzBatch builds a ConnectAll operation of the given edges.
func fuzzBatch(dim byte, check bool, edges ...[2]int) []byte {
	data := fuzzOp(4, dim, 0, len(edges), check)
	for _, e := range edges {
		data = append(data, byte(e[0]), byte(e[1]))
	}
	return data
}

// fuzzSeed encodes a graph of keys nodes and dims dimensions (walk selects
// the query path without descendent bit sets) followed by ops.
func fuzzSeed(keys, dims int, walk bool, ops ...[]byte) []byte {
	h := byte(dims - 1)
	if walk {
		h |= 0x80
	}
	data := []byte{byte(keys - 2), h}
	for _, op := range ops {
		data = append(data, op...)
	}
	return data
}

// FuzzConnect drives a Graph with operations decoded from the input and
// checks it against the brute-force reference model: no cycle is ever
// accepted (a refusal carries an existing shortest path, an acceptance had
// none), ConnectAll behaves as Connect on each edge but all or nothing,
// ordinals rise along every edge and equal the longest path, and
// descendents, ascendents, layers, components and reachability across
// dimensions equal brute-force results.
func FuzzConnect(f *testing.F) {
	c := func(dim byte, a, b int) []byte { return fuzzOp(0, dim, a, b, true) }
	// The pydagger shapes of test_descendents.py, with the back edges that
	// test_graph.py expects to be refused.
	f.Add(fuzzSeed(4, 1, false, c(0, 0, 1), c(0, 1, 2), c(0, 2, 3), c(0, 3, 0), c(0, 2, 1)))
	f.Add(fuzzSeed(4, 1, true, c(0, 0, 1), c(0, 0, 2), c(0, 1, 3), c(0, 2, 3), c(0, 3, 0)))
	f.Add(fuzzSeed(5, 1, false, c(0, 0, 3), c(0, 1, 3), c(0, 2, 3), c(0, 3, 4), c(0, 4, 1)))
	f.Add(fuzzSeed(6, 1, false, c(0, 0, 1), c(0, 0, 2), c(0, 1, 3), c(0, 2, 4), c(0, 3, 5), c(0, 4, 5), c(0, 5, 0)))
	// test_graph.py's dual topology: n4 -> n1 is fine in dimension 1, then
	// n3 -> n4 is disconnected.
	f.Add(fuzzSeed(4, 2, false, c(0, 0, 1), c(1, 1, 2), c(1, 2, 3), c(1, 3, 1), c(1, 3, 0),
		fuzzOp(5, 1, 2, 3, true)))
	// The conda python/pip cycle split across dimensions, with removal and
	// re-adding.
	f.Add(fuzzSeed(4, 3, false, c(0, 0, 1), c(0, 1, 0), c(1, 1, 0), c(2, 0, 2), c(0, 2, 3),
		fuzzOp(6, 0, 1, 0, true), fuzzOp(7, 0, 1, 0, true), c(0, 1, 0), c(0, 3, 1)))
	// Unknown nodes, self-edges and duplicates.
	f.Add(fuzzSeed(3, 1, true, c(0, 0, 3), c(0, 3, 0), c(0, 1, 1), c(0, 0, 1), c(0, 0, 1), fuzzOp(5, 0, 0, 1, true)))
	// Batches: a chain back to front, then one closing a cycle late, one
	// with an unknown node after a cycle, and one with duplicates.
	f.Add(fuzzSeed(6, 1, false, fuzzBatch(0, true, [2]int{4, 5}, [2]int{3, 4}, [2]int{2, 3}, [2]int{1, 2}, [2]int{0, 1}),
		fuzzBatch(0, true, [2]int{5, 0}),
		fuzzBatch(0, true, [2]int{0, 2}, [2]int{2, 2}, [2]int{5, 1}, [2]int{0, 6}),
		fuzzBatch(1, true, [2]int{0, 1}, [2]int{0, 1}, [2]int{1, 0})))
	f.Add([]byte("\x0e\x82 the quick brown fox jumps over the lazy dog, twice: the quick brown fox"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		keys := 2 + int(data[0]%14) // nodes 0 to keys-1; key `keys` starts absent
		x := newDiffer(t, 1+int(data[1]&0x7f)%3, keys)
		if data[1]&0x80 != 0 {
			x.g.closureLimit = 0
		}
		key := func(b byte) int { return int(b) % (keys + 1) }
		ops := data[2:]
		if len(ops) > 3*128 {
			ops = ops[:3*128]
		}
		for len(ops) >= 3 {
			op, a, b := ops[0], key(ops[1]), key(ops[2])
			n := int(ops[2]) % 6
			ops = ops[3:]
			d := int(op>>3&0xf) % x.g.Dims()
			switch op & 7 {
			case 0, 1, 2, 3:
				x.connect(d, a, b)
			case 4:
				var batch [][2]int
				for ; n > 0 && len(ops) >= 2; n-- {
					batch = append(batch, [2]int{key(ops[0]), key(ops[1])})
					ops = ops[2:]
				}
				x.connectAll(d, batch)
			case 5:
				x.disconnect(d, a, b)
			case 6:
				x.remove(a)
			case 7:
				x.add(a)
			}
			if op&0x80 != 0 {
				x.check()
			}
		}
		x.check()
	})
}

// BenchmarkFuzzConnectWorstCase times the fuzz body's checks on the largest
// input it accepts, with a full check after every operation.
func BenchmarkFuzzConnectWorstCase(b *testing.B) {
	for b.Loop() {
		x := newDiffer(b, 3, 15)
		for i := range 128 {
			x.connect(i*37%3, i*7%16, (i*11+3)%16)
			x.check()
		}
	}
}
