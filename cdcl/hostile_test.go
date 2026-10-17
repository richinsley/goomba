package cdcl_test

// Providers that break their contract must not crash the solver: Solve
// either answers or fails with an error.

import (
	"context"
	"errors"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

// hostileProvider wraps a BundleBox provider and lets a test override any
// answer.
type hostileProvider struct {
	*bundlebox.Provider
	filter     func(cands []cdcl.SolvableID, vs cdcl.VersionSetID, inverse bool) []cdcl.SolvableID
	candidates func(ctx context.Context, name cdcl.NameID) (*cdcl.Candidates, error)
	sort       func(ctx context.Context, cache *cdcl.SolverCache, s []cdcl.SolvableID) error
	deps       func(ctx context.Context, s cdcl.SolvableID) (cdcl.Dependencies, error)
	union      func(u cdcl.VersionSetUnionID) []cdcl.VersionSetID
	condition  func(c cdcl.ConditionID) cdcl.Condition
	name       func(s cdcl.SolvableID) cdcl.NameID
}

func (h *hostileProvider) SolvableName(s cdcl.SolvableID) cdcl.NameID {
	if h.name != nil {
		return h.name(s)
	}
	return h.Provider.SolvableName(s)
}

func (h *hostileProvider) FilterCandidates(c []cdcl.SolvableID, vs cdcl.VersionSetID, inv bool) []cdcl.SolvableID {
	if h.filter != nil {
		return h.filter(c, vs, inv)
	}
	return h.Provider.FilterCandidates(c, vs, inv)
}

func (h *hostileProvider) GetCandidates(ctx context.Context, n cdcl.NameID) (*cdcl.Candidates, error) {
	if h.candidates != nil {
		return h.candidates(ctx, n)
	}
	return h.Provider.GetCandidates(ctx, n)
}

func (h *hostileProvider) SortCandidates(ctx context.Context, cache *cdcl.SolverCache, s []cdcl.SolvableID) error {
	if h.sort != nil {
		return h.sort(ctx, cache, s)
	}
	return h.Provider.SortCandidates(ctx, cache, s)
}

func (h *hostileProvider) GetDependencies(ctx context.Context, s cdcl.SolvableID) (cdcl.Dependencies, error) {
	if h.deps != nil {
		return h.deps(ctx, s)
	}
	return h.Provider.GetDependencies(ctx, s)
}

func (h *hostileProvider) VersionSetsInUnion(u cdcl.VersionSetUnionID) []cdcl.VersionSetID {
	if h.union != nil {
		return h.union(u)
	}
	return h.Provider.VersionSetsInUnion(u)
}

func (h *hostileProvider) ResolveCondition(c cdcl.ConditionID) cdcl.Condition {
	if h.condition != nil {
		return h.condition(c)
	}
	return h.Provider.ResolveCondition(c)
}

func hostileBase() *bundlebox.Provider {
	return fromPackages([]pk{
		{"a", 1, deps("b", "c 1..3")},
		{"a", 2, deps("b 2", "c; if b")},
		{"b", 1, deps("c")},
		{"b", 2, nil},
		{"c", 1, nil},
		{"c", 2, deps("a")},
	})
}

// solveHostile solves with a production solver (no debug checks) and
// returns the error, failing on panics.
func solveHostile(t *testing.T, h *hostileProvider, specs []string) ([]cdcl.SolvableID, error) {
	t.Helper()
	reqs := h.Requirements(specs)
	solver := cdcl.NewSolver(h)
	sol, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs})
	var conflict *cdcl.Conflict
	if errors.As(err, &conflict) {
		_ = conflict.Display(h)
		_ = conflict.Error()
	}
	return sol, err
}

func TestHostileProviderErrors(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("get candidates fails", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		h.candidates = func(context.Context, cdcl.NameID) (*cdcl.Candidates, error) { return nil, errBoom }
		if _, err := solveHostile(t, h, []string{"a"}); !errors.Is(err, errBoom) {
			t.Fatalf("got %v, want an error wrapping boom", err)
		}
	})
	t.Run("get dependencies fails", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		h.deps = func(context.Context, cdcl.SolvableID) (cdcl.Dependencies, error) { return cdcl.Dependencies{}, errBoom }
		if _, err := solveHostile(t, h, []string{"a"}); !errors.Is(err, errBoom) {
			t.Fatalf("got %v, want an error wrapping boom", err)
		}
	})
	t.Run("sort fails", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		h.sort = func(context.Context, *cdcl.SolverCache, []cdcl.SolvableID) error { return errBoom }
		if _, err := solveHostile(t, h, []string{"a"}); !errors.Is(err, errBoom) {
			t.Fatalf("got %v, want an error wrapping boom", err)
		}
	})
	t.Run("recursive sort", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		vs := h.VersionSets([]string{"a"})[0]
		h.sort = func(ctx context.Context, cache *cdcl.SolverCache, s []cdcl.SolvableID) error {
			_, err := cache.SortedCandidates(ctx, cdcl.SingleRequirement(vs))
			return err
		}
		if _, err := solveHostile(t, h, []string{"a"}); !errors.Is(err, cdcl.ErrRecursiveSort) {
			t.Fatalf("got %v, want ErrRecursiveSort", err)
		}
	})
	t.Run("cyclic condition", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		h.condition = func(c cdcl.ConditionID) cdcl.Condition { return cdcl.BinaryCondition(cdcl.Or, c, c) }
		if _, err := solveHostile(t, h, []string{"a 2"}); !errors.Is(err, cdcl.ErrConditionTooComplex) {
			t.Fatalf("got %v, want ErrConditionTooComplex", err)
		}
	})
	t.Run("exponential condition", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		b := h.VersionSets([]string{"b"})[0]
		// Condition 0 is a leaf; every other condition c below 1000 is
		// (c-1 or c-1) and (c-1 or c-1), whose DNF explodes.
		h.condition = func(c cdcl.ConditionID) cdcl.Condition {
			switch {
			case c == 0:
				return cdcl.RequirementCondition(b)
			case c >= 1000:
				return cdcl.BinaryCondition(cdcl.Or, c-1001, c-1001)
			}
			return cdcl.BinaryCondition(cdcl.And, c+1000, c+1000)
		}
		reqs := h.Requirements([]string{"c"})
		reqs[0] = reqs[0].Requirement.WithCondition(40)
		solver := cdcl.NewSolver(h)
		if _, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs}); !errors.Is(err, cdcl.ErrConditionTooComplex) {
			t.Fatalf("got %v, want ErrConditionTooComplex", err)
		}
	})
	t.Run("long conjunction", func(t *testing.T) {
		h := &hostileProvider{Provider: hostileBase()}
		b := h.VersionSets([]string{"b"})[0]
		// c and c, nested 200 deep: one disjunct of 2^200 version sets.
		h.condition = func(c cdcl.ConditionID) cdcl.Condition {
			if c == 0 {
				return cdcl.RequirementCondition(b)
			}
			return cdcl.BinaryCondition(cdcl.And, c-1, c-1)
		}
		reqs := h.Requirements([]string{"c"})
		reqs[0] = reqs[0].Requirement.WithCondition(200)
		solver := cdcl.NewSolver(h)
		if _, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: reqs}); !errors.Is(err, cdcl.ErrConditionTooComplex) {
			t.Fatalf("got %v, want ErrConditionTooComplex", err)
		}
	})
}

func TestHostileProviderInconsistentAnswers(t *testing.T) {
	cases := map[string]func(h *hostileProvider){
		"filter returns other packages": func(h *hostileProvider) {
			h.filter = func(c []cdcl.SolvableID, vs cdcl.VersionSetID, inv bool) []cdcl.SolvableID {
				var all []cdcl.SolvableID
				for i := 0; i < h.Pool.SolvableCount(); i++ {
					all = append(all, cdcl.SolvableID(i))
				}
				return all
			}
		},
		"filter returns duplicates": func(h *hostileProvider) {
			h.filter = func(c []cdcl.SolvableID, vs cdcl.VersionSetID, inv bool) []cdcl.SolvableID {
				out := h.Provider.FilterCandidates(c, vs, inv)
				return append(out, out...)
			}
		},
		"filter ignores inverse": func(h *hostileProvider) {
			h.filter = func(c []cdcl.SolvableID, vs cdcl.VersionSetID, inv bool) []cdcl.SolvableID {
				return h.Provider.FilterCandidates(c, vs, false)
			}
		},
		"empty union": func(h *hostileProvider) {
			h.union = func(cdcl.VersionSetUnionID) []cdcl.VersionSetID { return nil }
		},
		"candidates with foreign lock, favor and exclusion": func(h *hostileProvider) {
			h.candidates = func(ctx context.Context, n cdcl.NameID) (*cdcl.Candidates, error) {
				c, err := h.Provider.GetCandidates(ctx, n)
				if c == nil || err != nil {
					return c, err
				}
				foreign := cdcl.SolvableID(h.Pool.SolvableCount() - 1)
				c.Locked, c.HasLocked = foreign, true
				c.Favored, c.HasFavored = foreign, true
				c.Excluded = append(c.Excluded, cdcl.ExcludedSolvable{Solvable: foreign, Reason: h.Pool.InternString("x")})
				c.HintDependencies, c.HintSolvables = cdcl.HintSome, []cdcl.SolvableID{foreign}
				return c, nil
			}
		},
		"candidates of every package": func(h *hostileProvider) {
			h.candidates = func(ctx context.Context, n cdcl.NameID) (*cdcl.Candidates, error) {
				c, err := h.Provider.GetCandidates(ctx, n)
				if c == nil || err != nil {
					return c, err
				}
				c.Candidates = nil
				for i := 0; i < h.Pool.SolvableCount(); i++ {
					c.Candidates = append(c.Candidates, cdcl.SolvableID(i))
				}
				return c, nil
			}
		},
		"sort scrambles ids": func(h *hostileProvider) {
			h.sort = func(_ context.Context, _ *cdcl.SolverCache, s []cdcl.SolvableID) error {
				for i := range s {
					s[i] = cdcl.SolvableID(int(s[i]+1) % h.Pool.SolvableCount())
				}
				return nil
			}
		},
		"dependencies on own package": func(h *hostileProvider) {
			h.deps = func(ctx context.Context, s cdcl.SolvableID) (cdcl.Dependencies, error) {
				d, err := h.Provider.GetDependencies(ctx, s)
				name := h.SolvableName(s)
				vs := h.Pool.InternVersionSet(name, bundlebox.Full())
				d.Requirements = append(d.Requirements, cdcl.SingleRequirement(vs).Unconditional())
				d.Constrains = append(d.Constrains, vs, h.Pool.InternVersionSet(name, bundlebox.Ranges{}))
				return d, err
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			for _, specs := range [][]string{{"a"}, {"a", "b 1"}, {"a 2", "c"}, {"c 2"}} {
				h := &hostileProvider{Provider: hostileBase()}
				mutate(h)
				// Two runs must agree: answers stay deterministic even when
				// they are wrong.
				sol1, err1 := solveHostile(t, h, specs)
				t.Logf("%v: %d solvables, err %v", specs, len(sol1), err1)
				h2 := &hostileProvider{Provider: hostileBase()}
				mutate(h2)
				sol2, err2 := solveHostile(t, h2, specs)
				if (err1 == nil) != (err2 == nil) || len(sol1) != len(sol2) {
					t.Fatalf("non-deterministic: %v/%v %v/%v", sol1, err1, sol2, err2)
				}
			}
		})
	}
}

func TestSolveCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := hostileBase()
	solver := cdcl.NewSolver(p)
	_, err := solver.Solve(ctx, cdcl.Problem{Requirements: p.Requirements([]string{"a"})})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	// Cancelled while the provider is answering.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	h := &hostileProvider{Provider: hostileBase()}
	h.deps = func(c context.Context, s cdcl.SolvableID) (cdcl.Dependencies, error) {
		cancel()
		return h.Provider.GetDependencies(c, s)
	}
	solver = cdcl.NewSolver(h)
	_, err = solver.Solve(ctx, cdcl.Problem{Requirements: h.Requirements([]string{"a"})})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// The solver can be reused; its cache carries over between solves.
func TestSolverReuse(t *testing.T) {
	p := hostileBase()
	solver := newSolver(p)
	first, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: p.Requirements([]string{"a"})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = solver.Solve(context.Background(), cdcl.Problem{Requirements: p.Requirements([]string{"a 3"})})
	var conflict *cdcl.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v, want a conflict", err)
	}
	again, err := solver.Solve(context.Background(), cdcl.Problem{Requirements: p.Requirements([]string{"a"})})
	if err != nil || transactionToString(p, again) != transactionToString(p, first) {
		t.Fatalf("re-solve: %v %q vs %q", err, transactionToString(p, again), transactionToString(p, first))
	}
	// The earlier conflict stays displayable.
	assertInline(t, conflict.Display(p), "No candidates were found for a >=3, <4.")
}
