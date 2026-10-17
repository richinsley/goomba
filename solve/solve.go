// Ported from rattler crates/rattler_solve/src/resolvo/mod.rs (the solve
// entry point) and src/lib.rs (SolverTask) (BSD-3-Clause, see NOTICE).

package solve

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/spec"
)

// ErrInvalidRequest is wrapped by the error Solve and Validate return for a
// malformed Request: a spec that does not parse, a spec on a name pattern
// rather than one package, a channel that cannot be resolved, or virtual,
// favored or locked records that are nil, misplaced or given twice.
var ErrInvalidRequest = errors.New("solve: invalid request")

// ErrInvalidSolution is wrapped by the error Validate returns when a set of
// records is not a valid solution of a request, and by the error Solve
// returns if its own solution ever failed that check (which would be a
// bug: Solve fails closed rather than return it).
var ErrInvalidSolution = errors.New("solve: invalid solution")

// ErrDuplicateRecord is wrapped by the error Solve returns when the source
// lists the same package file (channel, subdir and file name) twice.
var ErrDuplicateRecord = errors.New("solve: duplicate record")

// Request describes what to solve. It is plain data: Solve and Validate
// only read it, so one Request may be shared by concurrent calls as long as
// nobody modifies it.
type Request struct {
	// Specs are the match specs the user asked for, e.g. "python=3.11" or
	// "conda-forge::numpy >=1.26". Each must name exactly one package (no
	// "*" pattern). Every spec must be satisfied by the solution.
	Specs []string
	// Constraints are root constraints: a package they name must match
	// them if it is installed, but nothing requires it to be.
	Constraints []string
	// Virtual are the target machine's virtual packages (__glibc, __cuda,
	// ...), e.g. from package virtual. Packages whose names start with "__"
	// come only from here, never from the source; every one of them counts
	// as installed, so constrains on them are enforced.
	Virtual []*record.PackageRecord
	// Favored are records to keep when possible, e.g. the packages already
	// installed. The solver tries them first and falls back to other
	// candidates. At most one per package name.
	Favored []*record.PackageRecord
	// Locked are records that must stay exactly as they are: if a package
	// of that name is in the solution, it is this record. At most one per
	// package name.
	Locked []*record.PackageRecord
	// ChannelAlias resolves channel names in channel-qualified specs
	// ("conda-forge::numpy"); "" means spec.DefaultChannelAlias
	// (https://conda.anaconda.org).
	ChannelAlias string
}

// Solution is a solved environment. It is plain data: safe for concurrent
// reads.
type Solution struct {
	// Records are the selected package records, one per package name,
	// sorted by name. Virtual packages are not included. The records are
	// the ones the source (or Request.Favored/Locked) supplied, shared and
	// not copied, except that a record whose sha256 or md5 has upper-case
	// hex digits is replaced by a copy with the digests in lower case.
	// Callers must not modify them.
	Records []*record.PackageRecord
}

// Unsolvable is the error Solve returns when no valid solution exists. Its
// Error method returns a readable explanation of why, drawn as a tree (see
// package cdcl). Use errors.As to obtain it.
//
// An Unsolvable refers to the state of the solve that produced it. It is
// not safe for concurrent use.
type Unsolvable struct {
	// Explanation is the human-readable explanation, without the summary
	// line Error adds: cdcl's explanation (bounded to 64 KiB and 2000
	// lines), with each run of sibling candidates of one package that
	// "conflict with the versions reported above" folded into one line
	// listing their versions. Conflict.Display(Interner()) gives cdcl's
	// unfolded text.
	Explanation string
	// Conflict is the conflict cdcl found, for tools that walk its graph
	// (Conflict.Graph). Map its solvables back to records with Record and
	// render names and version sets with Interner.
	Conflict *cdcl.Conflict

	p *provider
}

// Error returns a summary line followed by the explanation.
func (u *Unsolvable) Error() string {
	return "solve: the request cannot be satisfied:\n" + strings.TrimRight(u.Explanation, "\n")
}

// Unwrap returns the cdcl conflict, so errors.Is(err, cdcl.ErrUnsolvable)
// holds.
func (u *Unsolvable) Unwrap() error { return u.Conflict }

// Interner returns the interner that names the conflict's packages,
// solvables and version sets, for Conflict.Display or ConflictGraph.Graphviz.
func (u *Unsolvable) Interner() cdcl.Interner { return u.p }

// Record returns the record of a solvable of the conflict graph, or nil if
// the id is unknown or stands for no record (a channel named by a spec
// that strict channel priority did not use for the package). Virtual
// packages have records too (from Request.Virtual).
func (u *Unsolvable) Record(id cdcl.SolvableID) *record.PackageRecord {
	if int(id) >= len(u.p.solvables) || u.p.solvables[id].hidden {
		return nil
	}
	return u.p.solvables[id].rec
}

// Solve resolves req against src: it returns one record per package such
// that every spec of req.Specs, every dependency of every selected record,
// every constrains of a selected record and every req.Constraints entry
// holds, choosing among the candidates in conda's order (see the package
// documentation). src is consulted one package name at a time, only for
// the names the solver reaches; if it implements repodata.Prefetcher, the
// names of req.Specs are prefetched first.
//
// An unsolvable request returns an error that errors.As to *Unsolvable. A
// malformed request returns an error wrapping ErrInvalidRequest; an error
// from src is returned wrapped; cancelling ctx stops the solve with an
// error wrapping ctx.Err(). Every solution is checked with Validate before
// it is returned.
//
// Error messages show untrusted text (specs, record fields, source errors)
// escaped as explanations do.
func Solve(ctx context.Context, src repodata.Source, req Request) (*Solution, error) {
	sol, err := solve(ctx, src, req)
	return sol, sanitizeError(err)
}

// solve is Solve with error messages as produced.
func solve(ctx context.Context, src repodata.Source, req Request) (*Solution, error) {
	if src == nil {
		return nil, errors.New("solve: nil source")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("solve: %w", err)
	}
	p, problem, err := newProvider(src, req)
	if err != nil {
		return nil, err
	}
	if pf, ok := src.(repodata.Prefetcher); ok && len(p.prefetch) > 0 {
		pf.Prefetch(ctx, p.prefetch)
	}

	solver := cdcl.NewSolver(p)
	ids, err := solver.Solve(ctx, problem)
	if err != nil {
		var conflict *cdcl.Conflict
		if errors.As(err, &conflict) {
			return nil, &Unsolvable{Explanation: foldConflictLeaves(conflict.Display(p)), Conflict: conflict, p: p}
		}
		return nil, fmt.Errorf("solve: %w", err)
	}

	recs := make([]*record.PackageRecord, 0, len(ids))
	for _, id := range ids {
		if int(id) >= len(p.solvables) {
			return nil, fmt.Errorf("solve: %w: unknown solvable %d in solution", cdcl.ErrInternal, id)
		}
		s := &p.solvables[id]
		if s.virtual {
			continue
		}
		recs = append(recs, s.rec)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	if err := Validate(req, recs); err != nil {
		return nil, fmt.Errorf("solve: the solver produced an invalid solution: %w", err)
	}
	return &Solution{Records: recs}, nil
}

// channelAlias returns the alias to resolve channel names against.
func channelAlias(alias string) string {
	if alias == "" {
		return spec.DefaultChannelAlias
	}
	return alias
}
