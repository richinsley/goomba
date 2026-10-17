// Ported from resolvo src/utils/pool.rs (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"fmt"
	"sync"
)

// Solvable is a single candidate of a package as stored in a Pool: its
// interned name plus a provider-defined record, typically the package
// metadata. It is plain data: safe for concurrent reads.
type Solvable[R any] struct {
	// Name is the interned package name.
	Name NameID
	// Record is the provider's data for this candidate.
	Record R
}

// Pool interns the names, solvables, version sets, unions, conditions and
// strings a DependencyProvider hands to the solver, so providers do not have
// to implement interning themselves. VS is the provider's version set type
// (it must be comparable so equal version sets share an ID) and R the record
// stored with each solvable. Package names are strings.
//
// A pool never forgets: IDs stay valid for the lifetime of the pool. All
// methods are safe for concurrent use. The Resolve methods panic when given
// an ID the pool never handed out, like an out-of-range slice index; use the
// Lookup methods to test for existence.
type Pool[VS comparable, R any] struct {
	mu sync.RWMutex

	solvables []Solvable[R]

	names    []string
	nameToID map[string]NameID
	strs     []string
	strToID  map[string]StringID
	vsets    []poolVersionSet[VS]
	vsetToID map[poolVersionSet[VS]]VersionSetID
	unions   [][]VersionSetID
	conds    []Condition
	condToID map[Condition]ConditionID
}

type poolVersionSet[VS comparable] struct {
	name NameID
	vs   VS
}

// NewPool returns an empty pool.
func NewPool[VS comparable, R any]() *Pool[VS, R] {
	return &Pool[VS, R]{
		nameToID: map[string]NameID{},
		strToID:  map[string]StringID{},
		vsetToID: map[poolVersionSet[VS]]VersionSetID{},
		condToID: map[Condition]ConditionID{},
	}
}

// InternString interns s and returns its id. Equal strings share an id.
func (p *Pool[VS, R]) InternString(s string) StringID {
	p.mu.RLock()
	id, ok := p.strToID[s]
	p.mu.RUnlock()
	if ok {
		return id
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.strToID[s]; ok {
		return id
	}
	id = StringID(len(p.strs))
	p.strs = append(p.strs, s)
	p.strToID[s] = id
	return id
}

// ResolveString returns the string with the given id.
func (p *Pool[VS, R]) ResolveString(id StringID) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.strs) {
		panic(fmt.Sprintf("cdcl: pool: unknown string id %d", id))
	}
	return p.strs[id]
}

// InternPackageName interns a package name and returns its id. Interning the
// same name twice returns the same id.
func (p *Pool[VS, R]) InternPackageName(name string) NameID {
	p.mu.RLock()
	id, ok := p.nameToID[name]
	p.mu.RUnlock()
	if ok {
		return id
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.nameToID[name]; ok {
		return id
	}
	id = NameID(len(p.names))
	p.names = append(p.names, name)
	p.nameToID[name] = id
	return id
}

// ResolvePackageName returns the package name with the given id.
func (p *Pool[VS, R]) ResolvePackageName(id NameID) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.names) {
		panic(fmt.Sprintf("cdcl: pool: unknown name id %d", id))
	}
	return p.names[id]
}

// LookupPackageName returns the id of a name that was interned before.
func (p *Pool[VS, R]) LookupPackageName(name string) (NameID, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.nameToID[name]
	return id, ok
}

// PackageNameCount returns the number of interned package names; valid ids
// are 0 through PackageNameCount()-1.
func (p *Pool[VS, R]) PackageNameCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.names)
}

// InternSolvable adds a solvable for the given package and returns its id.
// Solvables are not deduplicated: every call returns a new id.
func (p *Pool[VS, R]) InternSolvable(name NameID, record R) SolvableID {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := SolvableID(len(p.solvables))
	p.solvables = append(p.solvables, Solvable[R]{Name: name, Record: record})
	return id
}

// ResolveSolvable returns the solvable with the given id.
func (p *Pool[VS, R]) ResolveSolvable(id SolvableID) Solvable[R] {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.solvables) {
		panic(fmt.Sprintf("cdcl: pool: unknown solvable id %d", id))
	}
	return p.solvables[id]
}

// SolvableCount returns the number of solvables in the pool; valid ids are 0
// through SolvableCount()-1, in allocation order.
func (p *Pool[VS, R]) SolvableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.solvables)
}

// InternVersionSet interns a version set of the given package and returns
// its id. Equal (name, version set) pairs share an id.
func (p *Pool[VS, R]) InternVersionSet(name NameID, versionSet VS) VersionSetID {
	key := poolVersionSet[VS]{name: name, vs: versionSet}
	p.mu.RLock()
	id, ok := p.vsetToID[key]
	p.mu.RUnlock()
	if ok {
		return id
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.vsetToID[key]; ok {
		return id
	}
	id = VersionSetID(len(p.vsets))
	p.vsets = append(p.vsets, key)
	p.vsetToID[key] = id
	return id
}

// ResolveVersionSet returns the version set with the given id.
func (p *Pool[VS, R]) ResolveVersionSet(id VersionSetID) VS {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.vsets) {
		panic(fmt.Sprintf("cdcl: pool: unknown version set id %d", id))
	}
	return p.vsets[id].vs
}

// ResolveVersionSetPackageName returns the package name of the version set
// with the given id.
func (p *Pool[VS, R]) ResolveVersionSetPackageName(id VersionSetID) NameID {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.vsets) {
		panic(fmt.Sprintf("cdcl: pool: unknown version set id %d", id))
	}
	return p.vsets[id].name
}

// VersionSetCount returns the number of interned version sets.
func (p *Pool[VS, R]) VersionSetCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.vsets)
}

// InternVersionSetUnion interns the union of first and others and returns
// its id. Unions are not deduplicated: every call returns a new id.
func (p *Pool[VS, R]) InternVersionSetUnion(first VersionSetID, others ...VersionSetID) VersionSetUnionID {
	sets := make([]VersionSetID, 0, 1+len(others))
	sets = append(sets, first)
	sets = append(sets, others...)
	p.mu.Lock()
	defer p.mu.Unlock()
	id := VersionSetUnionID(len(p.unions))
	p.unions = append(p.unions, sets)
	return id
}

// ResolveVersionSetUnion returns the version sets of the union with the
// given id. The caller must not modify the returned slice.
func (p *Pool[VS, R]) ResolveVersionSetUnion(id VersionSetUnionID) []VersionSetID {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.unions) {
		panic(fmt.Sprintf("cdcl: pool: unknown version set union id %d", id))
	}
	return p.unions[id]
}

// InternCondition interns a condition and returns its id. Equal conditions
// share an id.
func (p *Pool[VS, R]) InternCondition(c Condition) ConditionID {
	p.mu.RLock()
	id, ok := p.condToID[c]
	p.mu.RUnlock()
	if ok {
		return id
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.condToID[c]; ok {
		return id
	}
	id = ConditionID(len(p.conds))
	p.conds = append(p.conds, c)
	p.condToID[c] = id
	return id
}

// ResolveCondition returns the condition with the given id.
func (p *Pool[VS, R]) ResolveCondition(id ConditionID) Condition {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.conds) {
		panic(fmt.Sprintf("cdcl: pool: unknown condition id %d", id))
	}
	return p.conds[id]
}
