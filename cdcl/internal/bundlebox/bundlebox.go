// Ported from resolvo tests/solver/bundle_box/mod.rs (BSD-3-Clause, see
// NOTICE).

// Package bundlebox is resolvo's toy "BundleBox" packaging system, used to
// test package cdcl: a package is a name and an integer version, and a spec
// is a name with a version range such as "foo 1..3" (meaning [1, 3)).
// Requirements may be unions ("a | b 2") and conditional ("a; if b and c").
//
// It is a test helper. Provider is not safe for concurrent use.
package bundlebox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/richinsley/goomba/cdcl"
)

type packageDependencies struct {
	dependencies []cdcl.ConditionalRequirement
	constrains   []Spec
}

type pkg struct {
	packs []Pack
	deps  map[Pack]packageDependencies
}

type solvableKey struct {
	name cdcl.NameID
	pack Pack
}

// Provider is the BundleBox dependency provider.
type Provider struct {
	// Pool interns names, version sets and solvables.
	Pool *cdcl.Pool[Ranges, Pack]

	idToCondition []*SpecCondition
	conditions    map[string]cdcl.ConditionID

	names    []string
	packages map[string]*pkg

	favored       map[string]Pack
	locked        map[string]Pack
	excluded      map[string]map[Pack]string
	allowMultiple map[string]bool

	// Cancel, when set, is called with the cause "cancelled!" when the
	// dependencies of a pack with CancelDuringGetDependencies are
	// requested. Tests pass the cancel function of the solve's context.
	Cancel context.CancelCauseFunc

	// HintDependenciesAvailable makes GetCandidates report every
	// candidate's dependencies as cheaply available.
	HintDependenciesAvailable bool

	requestedCandidates   []cdcl.NameID
	requestedCandidateSet map[cdcl.NameID]bool
	requestedDependencies map[cdcl.SolvableID]bool
	internedSolvables     map[solvableKey]cdcl.SolvableID

	concurrentRequests int
	// ConcurrentRequestsMax is the largest number of provider requests
	// that were in flight at the same time.
	ConcurrentRequestsMax int
}

// New returns an empty provider.
func New() *Provider {
	return &Provider{
		Pool:                  cdcl.NewPool[Ranges, Pack](),
		conditions:            map[string]cdcl.ConditionID{},
		packages:              map[string]*pkg{},
		favored:               map[string]Pack{},
		locked:                map[string]Pack{},
		excluded:              map[string]map[Pack]string{},
		allowMultiple:         map[string]bool{},
		requestedCandidateSet: map[cdcl.NameID]bool{},
		requestedDependencies: map[cdcl.SolvableID]bool{},
		internedSolvables:     map[solvableKey]cdcl.SolvableID{},
	}
}

// PackageSpec describes one package version for FromPackages. It is plain
// data: safe for concurrent reads.
type PackageSpec struct {
	// Name is the package name.
	Name string
	// Version is the version number.
	Version uint32
	// Deps are dependency specs such as "b 1..3".
	Deps []string
}

// FromPackages returns a provider with the given packages and no
// constrains.
func FromPackages(packages []PackageSpec) *Provider {
	p := New()
	for _, ps := range packages {
		p.AddPackage(ps.Name, NewPack(ps.Version), ps.Deps, nil)
	}
	return p
}

// PackageName returns the id of an interned package name; it panics if the
// name is unknown.
func (p *Provider) PackageName(name string) cdcl.NameID {
	id, ok := p.Pool.LookupPackageName(name)
	if !ok {
		panic("bundlebox: package missing: " + name)
	}
	return id
}

func (p *Provider) internCondition(c *SpecCondition) cdcl.ConditionID {
	k := c.key()
	if id, ok := p.conditions[k]; ok {
		return id
	}
	if c.Requirement == nil {
		p.internCondition(c.Left)
		p.internCondition(c.Right)
	}
	id := cdcl.ConditionID(len(p.idToCondition))
	p.idToCondition = append(p.idToCondition, c)
	p.conditions[k] = id
	return id
}

// Requirements parses specs and interns them as requirements. It panics on
// a malformed spec.
func (p *Provider) Requirements(specs []string) []cdcl.ConditionalRequirement {
	out := make([]cdcl.ConditionalRequirement, 0, len(specs))
	for _, s := range specs {
		cs, err := ParseConditionalSpec(s)
		if err != nil {
			panic(err)
		}
		vss := make([]cdcl.VersionSetID, len(cs.Specs))
		for i, sp := range cs.Specs {
			vss[i] = p.InternVersionSet(sp)
		}
		var req cdcl.Requirement
		if len(vss) > 1 {
			req = cdcl.UnionRequirement(p.Pool.InternVersionSetUnion(vss[0], vss[1:]...))
		} else {
			req = cdcl.SingleRequirement(vss[0])
		}
		cr := req.Unconditional()
		if cs.Condition != nil {
			cr = req.WithCondition(p.internCondition(cs.Condition))
		}
		out = append(out, cr)
	}
	return out
}

// VersionSets parses specs (without unions or conditions) and interns them
// as version sets. It panics on a malformed spec.
func (p *Provider) VersionSets(specs []string) []cdcl.VersionSetID {
	out := make([]cdcl.VersionSetID, 0, len(specs))
	for _, s := range specs {
		sp, err := ParseSpec(s)
		if err != nil {
			panic(err)
		}
		name := p.Pool.InternPackageName(sp.Name)
		out = append(out, p.Pool.InternVersionSet(name, sp.Versions))
	}
	return out
}

// InternVersionSet interns a spec as a version set.
func (p *Provider) InternVersionSet(sp Spec) cdcl.VersionSetID {
	name := p.Pool.InternPackageName(sp.Name)
	return p.Pool.InternVersionSet(name, sp.Versions)
}

// SetFavored favors a version of a package.
func (p *Provider) SetFavored(name string, version uint32) { p.favored[name] = NewPack(version) }

// Exclude excludes a version of a package for the given reason.
func (p *Provider) Exclude(name string, version uint32, reason string) {
	if p.excluded[name] == nil {
		p.excluded[name] = map[Pack]string{}
	}
	p.excluded[name][NewPack(version)] = reason
}

// SetAllowMultiple lets several versions of a package be installed.
func (p *Provider) SetAllowMultiple(name string) { p.allowMultiple[name] = true }

// SetLocked locks a package to a version.
func (p *Provider) SetLocked(name string, version uint32) { p.locked[name] = NewPack(version) }

// AddPackage adds a package version with its dependency and constrains
// specs. It panics on a malformed spec.
func (p *Provider) AddPackage(name string, version Pack, deps, constrains []string) {
	p.Pool.InternPackageName(name)
	reqs := p.Requirements(deps)
	cons := make([]Spec, 0, len(constrains))
	for _, c := range constrains {
		sp, err := ParseSpec(c)
		if err != nil {
			panic(err)
		}
		cons = append(cons, sp)
	}
	pk, ok := p.packages[name]
	if !ok {
		pk = &pkg{deps: map[Pack]packageDependencies{}}
		p.packages[name] = pk
		p.names = append(p.names, name)
	}
	if _, exists := pk.deps[version]; !exists {
		pk.packs = append(pk.packs, version)
	}
	pk.deps[version] = packageDependencies{dependencies: reqs, constrains: cons}
}

// InternSolvable returns the solvable of a pack, interning it once.
func (p *Provider) InternSolvable(name cdcl.NameID, pack Pack) cdcl.SolvableID {
	k := solvableKey{name: name, pack: pack}
	if id, ok := p.internedSolvables[k]; ok {
		return id
	}
	id := p.Pool.InternSolvable(name, pack)
	p.internedSolvables[k] = id
	return id
}

// SolvableID returns the solvable of a package version.
func (p *Provider) SolvableID(name string, pack Pack) cdcl.SolvableID {
	return p.InternSolvable(p.Pool.InternPackageName(name), pack)
}

// RequestedPackageNames returns the names whose candidates the solver
// requested, in request order.
func (p *Provider) RequestedPackageNames() []string {
	out := make([]string, len(p.requestedCandidates))
	for i, id := range p.requestedCandidates {
		out[i] = p.Pool.ResolvePackageName(id)
	}
	return out
}

// Snapshot captures the provider in a DependencySnapshot, starting from all
// packages.
func (p *Provider) Snapshot(ctx context.Context) (*cdcl.DependencySnapshot, error) {
	var ids []cdcl.NameID
	for _, n := range p.names {
		if id, ok := p.Pool.LookupPackageName(n); ok {
			ids = append(ids, id)
		}
	}
	return cdcl.NewDependencySnapshot(ctx, p, ids, nil, nil)
}

// DisplaySolvable implements cdcl.Interner.
func (p *Provider) DisplaySolvable(s cdcl.SolvableID) string {
	sol := p.Pool.ResolveSolvable(s)
	return p.DisplayName(sol.Name) + "=" + sol.Record.String()
}

// DisplayMergedSolvables implements cdcl.MergedSolvablesDisplayer.
func (p *Provider) DisplayMergedSolvables(solvables []cdcl.SolvableID) string {
	if len(solvables) == 0 {
		return ""
	}
	name := p.DisplayName(p.Pool.ResolveSolvable(solvables[0]).Name)
	versions := make([]uint32, len(solvables))
	for i, s := range solvables {
		versions[i] = p.Pool.ResolveSolvable(s).Record.Version
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	parts := make([]string, len(versions))
	for i, v := range versions {
		parts[i] = fmt.Sprint(v)
	}
	return name + " " + strings.Join(parts, " | ")
}

// DisplayName implements cdcl.Interner.
func (p *Provider) DisplayName(n cdcl.NameID) string { return p.Pool.ResolvePackageName(n) }

// DisplayVersionSet implements cdcl.Interner.
func (p *Provider) DisplayVersionSet(vs cdcl.VersionSetID) string {
	return p.Pool.ResolveVersionSet(vs).String()
}

// DisplayString implements cdcl.Interner.
func (p *Provider) DisplayString(s cdcl.StringID) string { return p.Pool.ResolveString(s) }

// VersionSetName implements cdcl.Interner.
func (p *Provider) VersionSetName(vs cdcl.VersionSetID) cdcl.NameID {
	return p.Pool.ResolveVersionSetPackageName(vs)
}

// SolvableName implements cdcl.Interner.
func (p *Provider) SolvableName(s cdcl.SolvableID) cdcl.NameID {
	return p.Pool.ResolveSolvable(s).Name
}

// VersionSetsInUnion implements cdcl.Interner.
func (p *Provider) VersionSetsInUnion(u cdcl.VersionSetUnionID) []cdcl.VersionSetID {
	return p.Pool.ResolveVersionSetUnion(u)
}

// ResolveCondition implements cdcl.Interner.
func (p *Provider) ResolveCondition(c cdcl.ConditionID) cdcl.Condition {
	sc := p.idToCondition[c]
	if sc.Requirement != nil {
		return cdcl.RequirementCondition(p.InternVersionSet(*sc.Requirement))
	}
	return cdcl.BinaryCondition(sc.Op, p.conditions[sc.Left.key()], p.conditions[sc.Right.key()])
}

// FilterCandidates implements cdcl.DependencyProvider.
func (p *Provider) FilterCandidates(candidates []cdcl.SolvableID, vs cdcl.VersionSetID, inverse bool) []cdcl.SolvableID {
	r := p.Pool.ResolveVersionSet(vs)
	var out []cdcl.SolvableID
	for _, s := range candidates {
		if r.Contains(p.Pool.ResolveSolvable(s).Record) != inverse {
			out = append(out, s)
		}
	}
	return out
}

// SortCandidates implements cdcl.DependencyProvider: highest version first.
func (p *Provider) SortCandidates(_ context.Context, _ *cdcl.SolverCache, solvables []cdcl.SolvableID) error {
	sort.SliceStable(solvables, func(i, j int) bool {
		return p.Pool.ResolveSolvable(solvables[i]).Record.Version > p.Pool.ResolveSolvable(solvables[j]).Record.Version
	})
	return nil
}

func (p *Provider) beginRequest() {
	p.concurrentRequests++
	p.ConcurrentRequestsMax = max(p.ConcurrentRequestsMax, p.concurrentRequests)
}

func (p *Provider) endRequest() { p.concurrentRequests-- }

// GetCandidates implements cdcl.DependencyProvider. It panics on a
// duplicate request, which would mean the solver's cache is broken.
func (p *Provider) GetCandidates(_ context.Context, name cdcl.NameID) (*cdcl.Candidates, error) {
	p.beginRequest()
	defer p.endRequest()

	if p.requestedCandidateSet[name] {
		panic("bundlebox: duplicate get_candidates request")
	}
	p.requestedCandidateSet[name] = true
	p.requestedCandidates = append(p.requestedCandidates, name)

	pkgName := p.Pool.ResolvePackageName(name)
	pk, ok := p.packages[pkgName]
	if !ok {
		return nil, nil
	}

	c := &cdcl.Candidates{
		Candidates:    make([]cdcl.SolvableID, 0, len(pk.packs)),
		AllowMultiple: p.allowMultiple[pkgName],
	}
	if p.HintDependenciesAvailable {
		c.HintDependencies = cdcl.HintAll
	}
	favor, hasFavor := p.favored[pkgName]
	locked, hasLocked := p.locked[pkgName]
	excluded := p.excluded[pkgName]
	for _, pack := range pk.packs {
		s := p.InternSolvable(name, pack)
		c.Candidates = append(c.Candidates, s)
		if hasFavor && pack == favor {
			c.Favored, c.HasFavored = s, true
		}
		if hasLocked && pack == locked {
			c.Locked, c.HasLocked = s, true
		}
		if reason, ok := excluded[pack]; ok {
			c.Excluded = append(c.Excluded, cdcl.ExcludedSolvable{Solvable: s, Reason: p.Pool.InternString(reason)})
		}
	}
	return c, nil
}

// ErrCancelled is the cancellation cause used for packs with
// CancelDuringGetDependencies.
var ErrCancelled = errors.New("cancelled!")

// GetDependencies implements cdcl.DependencyProvider. It panics on a
// duplicate request.
func (p *Provider) GetDependencies(_ context.Context, s cdcl.SolvableID) (cdcl.Dependencies, error) {
	p.beginRequest()
	defer p.endRequest()

	if p.requestedDependencies[s] {
		panic("bundlebox: duplicate get_dependencies request")
	}
	p.requestedDependencies[s] = true

	sol := p.Pool.ResolveSolvable(s)
	pkgName := p.Pool.ResolvePackageName(sol.Name)
	pack := sol.Record

	if pack.CancelDuringGetDependencies {
		if p.Cancel != nil {
			p.Cancel(ErrCancelled)
		}
		return cdcl.UnknownDependencies(p.Pool.InternString("cancelled")), nil
	}
	if pack.UnknownDeps {
		return cdcl.UnknownDependencies(p.Pool.InternString("could not retrieve deps")), nil
	}

	pk, ok := p.packages[pkgName]
	if !ok {
		return cdcl.Dependencies{}, nil
	}
	deps, ok := pk.deps[pack]
	if !ok {
		return cdcl.Dependencies{}, nil
	}
	out := cdcl.Dependencies{
		Requirements: append([]cdcl.ConditionalRequirement(nil), deps.dependencies...),
	}
	for _, c := range deps.constrains {
		out.Constrains = append(out.Constrains, p.InternVersionSet(c))
	}
	return out, nil
}
