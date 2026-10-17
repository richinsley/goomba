// Ported from resolvo tests/solver/decide_queue_prop.rs (BSD-3-Clause, see
// NOTICE). The proptest strategy is reproduced with a seeded generator.

package cdcl_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

// queueDep is (package, lo, len): the spec "p<package> lo..lo+len".
type queueDep struct{ pkg, lo, length int }

func queueSpecs(ds []queueDep) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = fmt.Sprintf("p%d %d..%d", d.pkg, d.lo, d.lo+d.length)
	}
	return out
}

// Random dependency graphs (cycles allowed): with the debug checks on, every
// decision verifies the decide queue's bookkeeping invariants, so this
// checks the queue's wake-up machinery across arbitrary search shapes.
func Test_decide_queue_matches_reference_on_random_problems(t *testing.T) {
	r := rand.New(rand.NewPCG(64, 64))
	spec := func(n int) queueDep { return queueDep{r.IntN(n), r.IntN(5), 1 + r.IntN(3)} }
	for c := 0; c < 256; c++ {
		n := 2 + r.IntN(8)
		p := bundlebox.New()
		for pkg := 0; pkg < n; pkg++ {
			versions := 1 + r.IntN(3)
			for v := 0; v < versions; v++ {
				var ds, cs []queueDep
				for k := r.IntN(4); k > 0; k-- {
					ds = append(ds, spec(n))
				}
				for k := r.IntN(2); k > 0; k-- {
					d := spec(n)
					// Upstream redirects self-constraints, which trip a
					// resolvo debug assertion; cdcl encodes them as
					// assertions (see TestSelfConstraint).
					if d.pkg == pkg {
						d.pkg = (d.pkg + 1) % n
					}
					cs = append(cs, d)
				}
				p.AddPackage(fmt.Sprintf("p%d", pkg), bundlebox.NewPack(uint32(v+1)), queueSpecs(ds), queueSpecs(cs))
			}
		}
		var roots []queueDep
		for k := 1 + r.IntN(3); k > 0; k-- {
			roots = append(roots, spec(n))
		}
		reqs := p.Requirements(queueSpecs(roots))
		solver := newSolver(p)
		_, _ = solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs})
	}
}

// A package whose constraint excludes itself can never be installed; cdcl
// encodes the clause as an assertion instead of watching one literal twice.
func TestSelfConstraint(t *testing.T) {
	p := bundlebox.New()
	p.AddPackage("a", bundlebox.NewPack(1), nil, []string{"a 2..3"})
	p.AddPackage("a", bundlebox.NewPack(2), nil, []string{"a 2..3"})
	p.AddPackage("b", bundlebox.NewPack(1), []string{"a"}, nil)
	assertInline(t, solveSnapshot(t, p, []string{"b"}), `
		a=2
		b=1
	`)

	q := bundlebox.New()
	q.AddPackage("a", bundlebox.NewPack(1), nil, []string{"a 2..3"})
	assertInline(t, solveUnsat(t, q, []string{"a"}), `
		The following packages are incompatible
		└─ a * cannot be installed because there are no viable options:
		   └─ a 1 would constrain
		      └─ a >=2, <3, which conflicts with any installable versions previously reported
	`)
}
