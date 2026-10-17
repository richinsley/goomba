// Ported from resolvo src/solver/cache.rs (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"context"
	"fmt"
)

// SolverCache caches what the solver learned from a DependencyProvider:
// candidates per package, matching and non-matching candidates per version
// set, sorted candidates per requirement and dependencies per solvable. A
// provider receives the cache in SortCandidates so its ordering can depend
// on cached information.
//
// The cache outlives a single solve: a Solver reuses it across calls to
// Solve. It is not safe for concurrent use.
type SolverCache struct {
	provider DependencyProvider

	candidates        []*Candidates
	candidatesFetched bitSet

	versionSetCandidates        reqMap[[]SolvableID]
	versionSetInverseCandidates reqMap[[]SolvableID]
	sortedCandidates            reqMap[[]SolvableID]

	dependencies       []*Dependencies
	hintDependencies   bitSet
	sortingVersionSets bitSet
}

// NewSolverCache returns an empty cache in front of provider.
func NewSolverCache(provider DependencyProvider) *SolverCache {
	return &SolverCache{provider: provider}
}

// Provider returns the provider behind the cache.
func (c *SolverCache) Provider() DependencyProvider { return c.provider }

// Candidates returns the candidates of the package with the given name,
// asking the provider on the first request. A package without candidates
// yields an empty, non-nil result.
func (c *SolverCache) Candidates(ctx context.Context, name NameID) (*Candidates, error) {
	checkID("name", uint32(name))
	if c.candidatesFetched.contains(uint32(name)) {
		return c.candidates[name], nil
	}
	// Getting the candidates may block, so check for cancellation first.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cands, err := c.provider.GetCandidates(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("cdcl: get candidates of %q: %w", c.provider.DisplayName(name), err)
	}
	if cands == nil {
		cands = &Candidates{}
	}

	// Remember which candidates' dependencies are cheap to retrieve.
	var hinted []SolvableID
	switch cands.HintDependencies {
	case HintAll:
		hinted = cands.Candidates
	case HintSome:
		hinted = cands.HintSolvables
	}
	for _, s := range hinted {
		checkID("solvable", uint32(s))
		c.hintDependencies.insert(uint32(s))
	}

	if int(name) >= len(c.candidates) {
		n := max(int(name)+1, 2*len(c.candidates))
		nc := make([]*Candidates, n)
		copy(nc, c.candidates)
		c.candidates = nc
	}
	c.candidates[name] = cands
	c.candidatesFetched.insert(uint32(name))
	return cands, nil
}

// MatchingCandidates returns the candidates of a version set's package that
// match the version set. The caller must not modify the result.
func (c *SolverCache) MatchingCandidates(ctx context.Context, vs VersionSetID) ([]SolvableID, error) {
	key := SingleRequirement(vs)
	if cached, ok := c.versionSetCandidates.get(key); ok {
		return cached, nil
	}
	cands, err := c.Candidates(ctx, c.provider.VersionSetName(vs))
	if err != nil {
		return nil, err
	}
	matching := c.provider.FilterCandidates(cands.Candidates, vs, false)
	c.versionSetCandidates.set(key, matching)
	return matching, nil
}

// NonMatchingCandidates returns the candidates of a version set's package
// that do not match the version set. The caller must not modify the result.
func (c *SolverCache) NonMatchingCandidates(ctx context.Context, vs VersionSetID) ([]SolvableID, error) {
	key := SingleRequirement(vs)
	if cached, ok := c.versionSetInverseCandidates.get(key); ok {
		return cached, nil
	}
	cands, err := c.Candidates(ctx, c.provider.VersionSetName(vs))
	if err != nil {
		return nil, err
	}
	nonMatching := c.provider.FilterCandidates(cands.Candidates, vs, true)
	c.versionSetInverseCandidates.set(key, nonMatching)
	return nonMatching, nil
}

// SortedCandidates returns the candidates that fulfil a requirement, sorted
// best first within each of the requirement's version sets. The caller must
// not modify the result.
func (c *SolverCache) SortedCandidates(ctx context.Context, r Requirement) ([]SolvableID, error) {
	if vs, ok := r.VersionSet(); ok {
		return c.sortedCandidatesForVersionSet(ctx, vs)
	}
	if cached, ok := c.sortedCandidates.get(r); ok {
		return cached, nil
	}
	var all []SolvableID
	for _, vs := range r.versionSets(c.provider) {
		sorted, err := c.sortedCandidatesForVersionSet(ctx, vs)
		if err != nil {
			return nil, err
		}
		all = append(all, sorted...)
	}
	c.sortedCandidates.set(r, all)
	return all, nil
}

// cachedSortedCandidates returns the sorted candidates of a requirement
// without ever calling the provider. It reports false when they were never
// computed.
func (c *SolverCache) cachedSortedCandidates(r Requirement) ([]SolvableID, bool) {
	if cached, ok := c.sortedCandidates.get(r); ok {
		return cached, true
	}
	if !r.IsUnion() {
		return nil, false
	}
	var all []SolvableID
	for _, vs := range r.versionSets(c.provider) {
		sorted, ok := c.sortedCandidates.get(SingleRequirement(vs))
		if !ok {
			return nil, false
		}
		all = append(all, sorted...)
	}
	return all, true
}

// sortedCandidatesForVersionSet returns the sorted candidates of a single
// version set.
func (c *SolverCache) sortedCandidatesForVersionSet(ctx context.Context, vs VersionSetID) ([]SolvableID, error) {
	key := SingleRequirement(vs)
	if cached, ok := c.sortedCandidates.get(key); ok {
		return cached, nil
	}
	matching, err := c.MatchingCandidates(ctx, vs)
	if err != nil {
		return nil, err
	}
	cands, err := c.Candidates(ctx, c.provider.VersionSetName(vs))
	if err != nil {
		return nil, err
	}

	// A provider whose SortCandidates asks for the sorted candidates of the
	// version set being sorted would recurse forever.
	checkID("version set", uint32(vs))
	if !c.sortingVersionSets.insert(uint32(vs)) {
		return nil, fmt.Errorf("cdcl: sort candidates of %q: %w", c.provider.DisplayVersionSet(vs), ErrRecursiveSort)
	}
	sorted := make([]SolvableID, len(matching))
	copy(sorted, matching)
	err = c.sortCandidates(ctx, vs, sorted)
	if err != nil {
		return nil, fmt.Errorf("cdcl: sort candidates of %q: %w", c.provider.DisplayVersionSet(vs), err)
	}

	// Move the favored solvable, if any, to the front so it is tried first.
	if cands.HasFavored {
		for pos, s := range sorted {
			if s == cands.Favored {
				copy(sorted[1:pos+1], sorted[:pos])
				sorted[0] = s
				break
			}
		}
	}

	c.sortedCandidates.set(key, sorted)
	return sorted, nil
}

// sortCandidates has the provider sort the candidates of vs, which is
// marked as being sorted. The mark is cleared even if SortCandidates panics
// (Solve recovers the panic), so a reused Solver does not report a spurious
// ErrRecursiveSort afterwards.
func (c *SolverCache) sortCandidates(ctx context.Context, vs VersionSetID, sorted []SolvableID) error {
	defer c.sortingVersionSets.remove(uint32(vs))
	return c.provider.SortCandidates(ctx, c, sorted)
}

// Dependencies returns the dependencies of a solvable, asking the provider
// on the first request.
func (c *SolverCache) Dependencies(ctx context.Context, s SolvableID) (*Dependencies, error) {
	checkID("solvable", uint32(s))
	if int(s) < len(c.dependencies) && c.dependencies[s] != nil {
		return c.dependencies[s], nil
	}
	// Getting the dependencies may block, so check for cancellation first.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deps, err := c.provider.GetDependencies(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("cdcl: get dependencies of %q: %w", c.provider.DisplaySolvable(s), err)
	}
	if int(s) >= len(c.dependencies) {
		n := max(int(s)+1, 2*len(c.dependencies))
		nd := make([]*Dependencies, n)
		copy(nd, c.dependencies)
		c.dependencies = nd
	}
	c.dependencies[s] = &deps
	return &deps, nil
}

// AreDependenciesAvailableFor reports whether the dependencies of a solvable
// are cheap to get: either they were requested before or the provider
// hinted that they are available.
func (c *SolverCache) AreDependenciesAvailableFor(s SolvableID) bool {
	if int(s) < len(c.dependencies) && c.dependencies[s] != nil {
		return true
	}
	return c.hintDependencies.contains(uint32(s))
}
