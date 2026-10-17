// Ported from resolvo src/snapshot.rs (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// SnapshotSolvable is a solvable in a DependencySnapshot. It is plain data:
// safe for concurrent reads.
type SnapshotSolvable struct {
	// Display is the provider's DisplaySolvable text.
	Display string
	// Name is the package name.
	Name NameID
	// Order is the position of the solvable when the provider sorts all
	// candidates of its package.
	Order uint32
	// Dependencies are the solvable's dependencies.
	Dependencies Dependencies
	// HintDependenciesAvailable reports whether the provider hinted that
	// the dependencies are cheap to get.
	HintDependenciesAvailable bool
}

// SnapshotVersionSet is a version set in a DependencySnapshot. It is plain
// data: safe for concurrent reads.
type SnapshotVersionSet struct {
	// Name is the package name the version set applies to.
	Name NameID
	// Display is the provider's DisplayVersionSet text.
	Display string
	// MatchingCandidates are the matching solvables, sorted by id.
	MatchingCandidates []SolvableID
}

func (vs *SnapshotVersionSet) matches(s SolvableID) bool {
	_, found := slices.BinarySearch(vs.MatchingCandidates, s)
	return found
}

// SnapshotPackage is a package in a DependencySnapshot. It is plain data:
// safe for concurrent reads.
type SnapshotPackage struct {
	// Name is the provider's DisplayName text.
	Name string
	// Solvables are all candidates of the package.
	Solvables []SolvableID
	// Excluded are the excluded candidates.
	Excluded []ExcludedSolvable
}

// DependencySnapshot captures everything a DependencyProvider answered about
// a set of packages, so a problem can be reproduced without the provider.
// Its Provider method returns a DependencyProvider over the snapshot.
//
// A DependencySnapshot is safe for concurrent reads.
type DependencySnapshot struct {
	// Solvables maps solvable ids to their data.
	Solvables map[SolvableID]SnapshotSolvable
	// VersionSetUnions maps union ids to their version sets.
	VersionSetUnions map[VersionSetUnionID][]VersionSetID
	// VersionSets maps version set ids to their data.
	VersionSets map[VersionSetID]SnapshotVersionSet
	// Packages maps name ids to their data.
	Packages map[NameID]SnapshotPackage
	// Strings maps string ids to their text.
	Strings map[StringID]string
	// Conditions maps condition ids to their definition.
	Conditions map[ConditionID]Condition
}

type snapshotElementKind uint8

const (
	snapPackage snapshotElementKind = iota
	snapVersionSet
	snapSolvable
	snapString
	snapCondition
)

type snapshotElement struct {
	kind snapshotElementKind
	id   uint32
}

// NewDependencySnapshot captures the state of provider by recursively
// querying it, starting from the given names, version sets and solvables.
// A panic in the provider is returned as an error wrapping ErrInternal.
func NewDependencySnapshot(ctx context.Context, provider DependencyProvider, names []NameID, versionSets []VersionSetID, solvables []SolvableID) (snap *DependencySnapshot, err error) {
	defer func() {
		if r := recover(); r != nil {
			snap, err = nil, recoveredError(r, false)
		}
	}()
	cache := NewSolverCache(provider)
	res := &DependencySnapshot{
		Solvables:        map[SolvableID]SnapshotSolvable{},
		VersionSetUnions: map[VersionSetUnionID][]VersionSetID{},
		VersionSets:      map[VersionSetID]SnapshotVersionSet{},
		Packages:         map[NameID]SnapshotPackage{},
		Strings:          map[StringID]string{},
		Conditions:       map[ConditionID]Condition{},
	}

	var queue []snapshotElement
	seen := map[snapshotElement]bool{}
	enqueue := func(e snapshotElement) {
		if !seen[e] {
			seen[e] = true
			queue = append(queue, e)
		}
	}
	for _, n := range names {
		enqueue(snapshotElement{snapPackage, uint32(n)})
	}
	for _, vs := range versionSets {
		enqueue(snapshotElement{snapVersionSet, uint32(vs)})
	}
	for _, s := range solvables {
		enqueue(snapshotElement{snapSolvable, uint32(s)})
	}

	for i := 0; i < len(queue); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e := queue[i]
		switch e.kind {
		case snapPackage:
			name := NameID(e.id)
			cands, err := cache.Candidates(ctx, name)
			if err != nil {
				return nil, err
			}
			for _, s := range cands.Candidates {
				enqueue(snapshotElement{snapSolvable, uint32(s)})
			}
			for _, ex := range cands.Excluded {
				enqueue(snapshotElement{snapSolvable, uint32(ex.Solvable)})
				enqueue(snapshotElement{snapString, uint32(ex.Reason)})
			}
			res.Packages[name] = SnapshotPackage{
				Name:      provider.DisplayName(name),
				Solvables: slices.Clone(cands.Candidates),
				Excluded:  slices.Clone(cands.Excluded),
			}

		case snapSolvable:
			s := SolvableID(e.id)
			name := provider.SolvableName(s)
			enqueue(snapshotElement{snapPackage, uint32(name)})
			deps, err := cache.Dependencies(ctx, s)
			if err != nil {
				return nil, err
			}
			if deps.Unknown {
				enqueue(snapshotElement{snapString, uint32(deps.UnknownReason)})
			} else {
				for _, vs := range deps.Constrains {
					enqueue(snapshotElement{snapVersionSet, uint32(vs)})
				}
				for _, req := range deps.Requirements {
					if req.HasCondition {
						enqueue(snapshotElement{snapCondition, uint32(req.Condition)})
					}
					if u, ok := req.Requirement.Union(); ok {
						vss := slices.Clone(provider.VersionSetsInUnion(u))
						slices.Sort(vss)
						vss = slices.Compact(vss)
						for _, vs := range vss {
							enqueue(snapshotElement{snapVersionSet, uint32(vs)})
						}
						res.VersionSetUnions[u] = vss
					} else {
						vs, _ := req.Requirement.VersionSet()
						enqueue(snapshotElement{snapVersionSet, uint32(vs)})
					}
				}
			}
			d := *deps
			d.Requirements = slices.Clone(d.Requirements)
			d.Constrains = slices.Clone(d.Constrains)
			res.Solvables[s] = SnapshotSolvable{
				Display:                   provider.DisplaySolvable(s),
				Name:                      name,
				Dependencies:              d,
				HintDependenciesAvailable: cache.AreDependenciesAvailableFor(s),
			}

		case snapString:
			res.Strings[StringID(e.id)] = provider.DisplayString(StringID(e.id))

		case snapVersionSet:
			vs := VersionSetID(e.id)
			name := provider.VersionSetName(vs)
			enqueue(snapshotElement{snapPackage, uint32(name)})
			matching, err := cache.MatchingCandidates(ctx, vs)
			if err != nil {
				return nil, err
			}
			for _, m := range matching {
				enqueue(snapshotElement{snapSolvable, uint32(m)})
			}
			sorted := slices.Clone(matching)
			slices.Sort(sorted)
			res.VersionSets[vs] = SnapshotVersionSet{
				Name:               name,
				Display:            provider.DisplayVersionSet(vs),
				MatchingCandidates: slices.Compact(sorted),
			}

		case snapCondition:
			c := ConditionID(e.id)
			cond := provider.ResolveCondition(c)
			if cond.Kind == ConditionRequirement {
				enqueue(snapshotElement{snapVersionSet, uint32(cond.VersionSet)})
			} else {
				enqueue(snapshotElement{snapCondition, uint32(cond.LHS)})
				enqueue(snapshotElement{snapCondition, uint32(cond.RHS)})
			}
			res.Conditions[c] = cond
		}
	}

	// Record the order in which the provider sorts each package.
	pkgNames := make([]NameID, 0, len(res.Packages))
	for n := range res.Packages {
		pkgNames = append(pkgNames, n)
	}
	slices.Sort(pkgNames)
	for _, n := range pkgNames {
		sorted := slices.Clone(res.Packages[n].Solvables)
		if err := provider.SortCandidates(ctx, cache, sorted); err != nil {
			return nil, fmt.Errorf("cdcl: snapshot: sort candidates of %q: %w", res.Packages[n].Name, err)
		}
		for order, s := range sorted {
			sol, ok := res.Solvables[s]
			if !ok {
				return nil, fmt.Errorf("cdcl: snapshot: sort candidates of %q: unknown solvable %d", res.Packages[n].Name, s)
			}
			sol.Order = uint32(order)
			res.Solvables[s] = sol
		}
	}
	return res, nil
}

// Provider returns a DependencyProvider that answers from the snapshot.
func (d *DependencySnapshot) Provider() *SnapshotProvider {
	var maxVS VersionSetID
	for id := range d.VersionSets {
		maxVS = max(maxVS, id)
	}
	return &SnapshotProvider{snapshot: d, maxVersionSet: maxVS}
}

// SnapshotProvider is a DependencyProvider backed by a DependencySnapshot.
// Requests for ids the snapshot does not contain panic, as with a Pool. It
// is not safe for concurrent use.
type SnapshotProvider struct {
	snapshot      *DependencySnapshot
	maxVersionSet VersionSetID
	additional    []SnapshotVersionSet
}

// AddPackageRequirement adds a version set of package name that matches the
// candidates whose display contains matcher, or every candidate when
// matcher is "*", and returns its id. Additional version sets get ids
// directly after the snapshot's highest version set id.
func (p *SnapshotProvider) AddPackageRequirement(name NameID, matcher string) (VersionSetID, error) {
	pkg, ok := p.snapshot.Packages[name]
	if !ok {
		return 0, fmt.Errorf("cdcl: snapshot: add requirement %q: unknown package %d", matcher, name)
	}
	id := p.maxVersionSet + 1 + VersionSetID(len(p.additional))
	var matching []SolvableID
	for _, s := range pkg.Solvables {
		if matcher == "*" || strings.Contains(p.solvable(s).Display, matcher) {
			matching = append(matching, s)
		}
	}
	slices.Sort(matching)
	p.additional = append(p.additional, SnapshotVersionSet{Name: name, Display: matcher, MatchingCandidates: slices.Compact(matching)})
	return id, nil
}

func (p *SnapshotProvider) solvable(s SolvableID) SnapshotSolvable {
	sol, ok := p.snapshot.Solvables[s]
	if !ok {
		panic(fmt.Sprintf("cdcl: snapshot: missing solvable %d", s))
	}
	return sol
}

func (p *SnapshotProvider) pkg(n NameID) SnapshotPackage {
	pkg, ok := p.snapshot.Packages[n]
	if !ok {
		panic(fmt.Sprintf("cdcl: snapshot: missing package %d", n))
	}
	return pkg
}

func (p *SnapshotProvider) versionSet(vs VersionSetID) *SnapshotVersionSet {
	// Ids up to the snapshot's maximum belong to the snapshot; anything
	// beyond was added by AddPackageRequirement.
	if vs > p.maxVersionSet {
		idx := int(vs - p.maxVersionSet - 1)
		if idx < len(p.additional) {
			return &p.additional[idx]
		}
	} else if v, ok := p.snapshot.VersionSets[vs]; ok {
		return &v
	}
	panic(fmt.Sprintf("cdcl: snapshot: missing version set %d", vs))
}

// DisplaySolvable implements Interner.
func (p *SnapshotProvider) DisplaySolvable(s SolvableID) string { return p.solvable(s).Display }

// DisplayName implements Interner.
func (p *SnapshotProvider) DisplayName(n NameID) string { return p.pkg(n).Name }

// DisplayVersionSet implements Interner.
func (p *SnapshotProvider) DisplayVersionSet(vs VersionSetID) string {
	return p.versionSet(vs).Display
}

// DisplayString implements Interner.
func (p *SnapshotProvider) DisplayString(s StringID) string {
	str, ok := p.snapshot.Strings[s]
	if !ok {
		panic(fmt.Sprintf("cdcl: snapshot: missing string %d", s))
	}
	return str
}

// VersionSetName implements Interner.
func (p *SnapshotProvider) VersionSetName(vs VersionSetID) NameID { return p.versionSet(vs).Name }

// SolvableName implements Interner.
func (p *SnapshotProvider) SolvableName(s SolvableID) NameID { return p.solvable(s).Name }

// VersionSetsInUnion implements Interner.
func (p *SnapshotProvider) VersionSetsInUnion(u VersionSetUnionID) []VersionSetID {
	vss, ok := p.snapshot.VersionSetUnions[u]
	if !ok {
		panic(fmt.Sprintf("cdcl: snapshot: missing version set union %d", u))
	}
	return vss
}

// ResolveCondition implements Interner.
func (p *SnapshotProvider) ResolveCondition(c ConditionID) Condition {
	cond, ok := p.snapshot.Conditions[c]
	if !ok {
		panic(fmt.Sprintf("cdcl: snapshot: missing condition %d", c))
	}
	return cond
}

// FilterCandidates implements DependencyProvider.
func (p *SnapshotProvider) FilterCandidates(candidates []SolvableID, vs VersionSetID, inverse bool) []SolvableID {
	set := p.versionSet(vs)
	var out []SolvableID
	for _, c := range candidates {
		if set.matches(c) != inverse {
			out = append(out, c)
		}
	}
	return out
}

// GetCandidates implements DependencyProvider.
func (p *SnapshotProvider) GetCandidates(_ context.Context, name NameID) (*Candidates, error) {
	pkg := p.pkg(name)
	c := &Candidates{
		Candidates:       slices.Clone(pkg.Solvables),
		Excluded:         slices.Clone(pkg.Excluded),
		HintDependencies: HintSome,
	}
	for _, s := range pkg.Solvables {
		if p.solvable(s).HintDependenciesAvailable {
			c.HintSolvables = append(c.HintSolvables, s)
		}
	}
	return c, nil
}

// SortCandidates implements DependencyProvider.
func (p *SnapshotProvider) SortCandidates(_ context.Context, _ *SolverCache, solvables []SolvableID) error {
	sort.SliceStable(solvables, func(i, j int) bool {
		return p.solvable(solvables[i]).Order < p.solvable(solvables[j]).Order
	})
	return nil
}

// GetDependencies implements DependencyProvider.
func (p *SnapshotProvider) GetDependencies(_ context.Context, s SolvableID) (Dependencies, error) {
	return p.solvable(s).Dependencies, nil
}
