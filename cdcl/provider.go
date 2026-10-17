// Ported from resolvo src/lib.rs (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"context"
	"sort"
	"strings"
)

// Interner is used by the solver to display and query interned objects. It
// knows how names, solvables, version sets and strings are represented by
// the provider.
//
// Implementations must be deterministic: the same ID must always produce the
// same answer. The solver calls an Interner from a single goroutine at a
// time.
type Interner interface {
	// DisplaySolvable formats a solvable for humans, including the package
	// name and any other identifying properties.
	DisplaySolvable(solvable SolvableID) string

	// DisplayName formats a package name for humans.
	DisplayName(name NameID) string

	// DisplayVersionSet formats a version set for humans, without the
	// package name (it is added where appropriate).
	DisplayVersionSet(versionSet VersionSetID) string

	// DisplayString returns the string with the given id.
	DisplayString(str StringID) string

	// VersionSetName returns the package name a version set applies to.
	VersionSetName(versionSet VersionSetID) NameID

	// SolvableName returns the package name of a solvable.
	SolvableName(solvable SolvableID) NameID

	// VersionSetsInUnion returns the version sets of a union, in a
	// deterministic order. The caller does not modify the returned slice.
	VersionSetsInUnion(union VersionSetUnionID) []VersionSetID

	// ResolveCondition returns the condition with the given id.
	ResolveCondition(condition ConditionID) Condition
}

// SolvableNameDisplayer is an optional Interner extension that formats the
// name of a solvable. Without it the solver uses
// DisplayName(SolvableName(solvable)). It is called like an Interner.
type SolvableNameDisplayer interface {
	// DisplaySolvableName formats the package name of solvable for humans.
	DisplaySolvableName(solvable SolvableID) string
}

// MergedSolvablesDisplayer is an optional Interner extension that formats
// several solvables of the same package on one line, for example
// "foo 1 | 2 | 3". Without it the solver uses DefaultDisplayMergedSolvables.
// It is called like an Interner.
type MergedSolvablesDisplayer interface {
	// DisplayMergedSolvables formats solvables, which all belong to one
	// package, on one line, e.g. "foo 1 | 2 | 3". It returns "" for no
	// solvables.
	DisplayMergedSolvables(solvables []SolvableID) string
}

// DisplaySolvableName formats the package name of solvable, using the
// interner's SolvableNameDisplayer extension when it has one.
func DisplaySolvableName(interner Interner, solvable SolvableID) string {
	if d, ok := interner.(SolvableNameDisplayer); ok {
		return d.DisplaySolvableName(solvable)
	}
	return interner.DisplayName(interner.SolvableName(solvable))
}

// DisplayMergedSolvables formats several solvables of one package on one
// line, using the interner's MergedSolvablesDisplayer extension when it has
// one and DefaultDisplayMergedSolvables otherwise.
func DisplayMergedSolvables(interner Interner, solvables []SolvableID) string {
	if d, ok := interner.(MergedSolvablesDisplayer); ok {
		return d.DisplayMergedSolvables(solvables)
	}
	return DefaultDisplayMergedSolvables(interner, solvables)
}

// DefaultDisplayMergedSolvables formats solvables as the name of the first
// one followed by the sorted, deduplicated DisplaySolvable strings joined
// with " | ". It returns "" for an empty slice.
func DefaultDisplayMergedSolvables(interner Interner, solvables []SolvableID) string {
	if len(solvables) == 0 {
		return ""
	}
	versions := make([]string, 0, len(solvables))
	for _, s := range solvables {
		versions = append(versions, interner.DisplaySolvable(s))
	}
	sort.Strings(versions)
	uniq := versions[:0]
	for i, v := range versions {
		if i == 0 || v != versions[i-1] {
			uniq = append(uniq, v)
		}
	}
	return DisplaySolvableName(interner, solvables[0]) + " " + strings.Join(uniq, " | ")
}

// DependencyProvider gives the solver access to the packages that are
// available. It knows nothing about the solver's internals; the solver asks
// for candidates and dependencies lazily, only for the packages it reaches.
//
// The solver calls a provider from a single goroutine. Methods that take a
// context may block on I/O and must return promptly with ctx.Err() (or an
// error wrapping it) when the context is cancelled. Any error returned by a
// provider method aborts the solve; Solver.Solve returns it wrapped.
type DependencyProvider interface {
	Interner

	// FilterCandidates returns the candidates that match versionSet, or, if
	// inverse is true, the candidates that do not match it. The order of
	// the input must be preserved.
	FilterCandidates(candidates []SolvableID, versionSet VersionSetID, inverse bool) []SolvableID

	// GetCandidates returns the solvables that should be considered when a
	// package with the given name is requested. A nil result means the
	// package has no candidates.
	GetCandidates(ctx context.Context, name NameID) (*Candidates, error)

	// SortCandidates sorts solvables in place so that the candidate the
	// solver should try first comes first. The solver tries the first
	// candidate and falls back to later ones on conflicts. cache gives
	// access to the solver's cached candidates and dependencies, for
	// providers whose ordering depends on them.
	SortCandidates(ctx context.Context, cache *SolverCache, solvables []SolvableID) error

	// GetDependencies returns the dependencies of a solvable.
	GetDependencies(ctx context.Context, solvable SolvableID) (Dependencies, error)
}

// Candidates lists the candidate solvables of one package. It is returned
// from DependencyProvider.GetCandidates. The solver keeps the returned value
// and never modifies it; the provider must not modify it afterwards either.
// It is plain data: safe for concurrent reads.
type Candidates struct {
	// Candidates lists every solvable of the package.
	Candidates []SolvableID

	// Favored, when HasFavored is set, is tried before every other
	// candidate. The solver falls back to the others if no solution exists
	// with it. Sorting the candidate first in SortCandidates has the same
	// effect, but favoring produces better error messages.
	Favored SolvableID
	// HasFavored reports whether Favored is set.
	HasFavored bool

	// Locked, when HasLocked is set, is the only candidate that may be
	// selected. Returning a single candidate has the same effect, but
	// locking produces better error messages.
	Locked SolvableID
	// HasLocked reports whether Locked is set.
	HasLocked bool

	// HintDependencies tells the solver for which candidates the
	// dependencies are cheap to retrieve, so it can request them up front.
	HintDependencies HintDependenciesAvailable

	// HintSolvables lists the candidates whose dependencies are cheap when
	// HintDependencies is HintSome.
	HintSolvables []SolvableID

	// Excluded lists candidates that are available but must not be
	// selected, each with the reason, used in error messages.
	Excluded []ExcludedSolvable

	// AllowMultiple lets the solver select several candidates of this
	// package at once. By default at most one is selected.
	AllowMultiple bool
}

// ExcludedSolvable is a candidate that was excluded for an external reason.
// It is a plain value, safe for concurrent use.
type ExcludedSolvable struct {
	// Solvable is the excluded candidate.
	Solvable SolvableID
	// Reason explains why, for error messages.
	Reason StringID
}

// HintDependenciesAvailable says for which candidates dependencies can be
// retrieved cheaply. It is a plain value, safe for concurrent use.
type HintDependenciesAvailable uint8

const (
	// HintNone means retrieving any dependencies is expensive.
	HintNone HintDependenciesAvailable = iota
	// HintAll means retrieving every candidate's dependencies is cheap.
	HintAll
	// HintSome means retrieving the dependencies of the candidates listed
	// in Candidates.HintSolvables is cheap.
	HintSome
)

// Dependencies describes the dependencies of a solvable. When Unknown is
// true the dependencies could not be determined and the solvable is
// excluded from the solution, with UnknownReason as the explanation.
//
// Dependencies is plain data: safe for concurrent reads. The solver keeps
// the value returned by GetDependencies and never modifies its slices.
type Dependencies struct {
	// Unknown marks the dependencies as unknown.
	Unknown bool
	// UnknownReason explains why the dependencies are unknown.
	UnknownReason StringID

	// Requirements lists packages that must be installed alongside the
	// solvable.
	Requirements []ConditionalRequirement

	// Constrains restricts packages that may or may not be part of the
	// solution: if a package named by a version set is installed, it must
	// match that version set, but nothing forces it to be installed.
	Constrains []VersionSetID
}

// KnownDependencies returns Dependencies with the given requirements and
// constrains.
func KnownDependencies(requirements []ConditionalRequirement, constrains []VersionSetID) Dependencies {
	return Dependencies{Requirements: requirements, Constrains: constrains}
}

// UnknownDependencies returns Dependencies that mark the solvable as
// unusable for the given reason.
func UnknownDependencies(reason StringID) Dependencies {
	return Dependencies{Unknown: true, UnknownReason: reason}
}
