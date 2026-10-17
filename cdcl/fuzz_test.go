package cdcl_test

import (
	"math/rand/v2"
	"testing"
)

// byteReader hands out the fuzz input one byte at a time, then zeros.
type byteReader struct {
	data []byte
	pos  int
}

func (b *byteReader) next() int {
	if b.pos >= len(b.data) {
		return 0
	}
	v := b.data[b.pos]
	b.pos++
	return int(v)
}

// decodeProblem turns arbitrary bytes into a small problem (at most 6
// packages of at most 4 versions), with unions, conditions, constraints,
// locks, exclusions, unknown dependencies and multi-version packages.
func decodeProblem(data []byte) propProblem {
	b := &byteReader{data: data}
	n := 1 + b.next()%6
	spec := func() propSpec {
		x := b.next()
		s := propSpec{name: x % n}
		if (x>>4)%3 == 0 {
			s.full = true
			return s
		}
		s.lo = uint32(b.next() % 5)
		s.hi = s.lo + 1 + uint32(b.next()%3)
		return s
	}
	req := func() propReq {
		flags := b.next()
		q := propReq{specs: []propSpec{spec()}}
		if flags&1 != 0 {
			q.specs = append(q.specs, spec())
		}
		if flags&2 != 0 {
			q.cond = []propSpec{spec()}
			for k := (flags >> 2) % 3; k > 0; k-- {
				q.ands = append(q.ands, b.next()&1 == 0)
				q.cond = append(q.cond, spec())
			}
		}
		return q
	}
	p := propProblem{pkgs: make([]propPackage, n)}
	for i := range p.pkgs {
		f := b.next()
		pkg := propPackage{locked: -1, allowMultiple: f&0x10 != 0}
		seen := map[uint32]bool{}
		for k := 1 + f%4; k > 0; k-- {
			vb := b.next()
			v := propVersion{version: uint32(vb % 6), unknown: vb&0x40 != 0 && vb&0x20 != 0, excluded: vb&0x80 != 0 && vb&0x08 != 0}
			for d := b.next() % 3; d > 0; d-- {
				v.deps = append(v.deps, req())
			}
			for c := b.next() % 2; c > 0; c-- {
				// Self-constraints are allowed: cdcl encodes them as
				// assertions.
				v.constrains = append(v.constrains, spec())
			}
			if seen[v.version] {
				continue
			}
			seen[v.version] = true
			pkg.versions = append(pkg.versions, v)
		}
		if f&0x20 != 0 {
			pkg.locked = b.next() % len(pkg.versions)
		}
		p.pkgs[i] = pkg
	}
	for k := 1 + b.next()%3; k > 0; k-- {
		p.roots = append(p.roots, req())
	}
	for k := b.next() % 2; k > 0; k-- {
		p.constraints = append(p.constraints, spec())
	}
	return p
}

// FuzzSolveRandom decodes random bytes into a small problem and solves it.
// The solver must never panic, the answer must agree with exhaustive
// enumeration, every solution must satisfy the problem, and the explanation
// of an unsat answer must be linear in the size of its conflict graph.
func FuzzSolveRandom(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{5, 0x13, 1, 2, 3, 0x22, 1, 0, 4, 3, 0x7f, 0x30, 1, 2, 2, 0x11, 0x02})
	// Seed with byte strings that decode to varied problems.
	r := rand.New(rand.NewPCG(7, 7))
	for i := 0; i < 64; i++ {
		seed := make([]byte, 16+r.IntN(96))
		for j := range seed {
			seed[j] = byte(r.Uint32())
		}
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 512 {
			return
		}
		checkProblem(t, decodeProblem(data))
	})
}

// Self-constraints: allowed by the fuzz decoder; spot-check a few seeds in
// regular test runs.
func TestFuzzSeeds(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	for i := 0; i < 2000; i++ {
		data := make([]byte, 8+r.IntN(120))
		for j := range data {
			data[j] = byte(r.Uint32())
		}
		checkProblem(t, decodeProblem(data))
	}
}
