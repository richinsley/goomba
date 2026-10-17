// Ported from resolvo src/requirement.rs and src/conditional_requirement.rs
// (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"strings"
	"unicode"
)

// Requirement is a dependency of a solvable on either a single version set or
// a union (logical OR) of version sets. A solvable that belongs to any of the
// version sets satisfies the requirement.
//
// The zero value is a requirement on VersionSetID 0. Requirement is a small
// comparable value; it is safe for concurrent use.
type Requirement struct {
	id    uint32
	union bool
}

// SingleRequirement returns a requirement on one version set.
func SingleRequirement(versionSet VersionSetID) Requirement {
	return Requirement{id: uint32(versionSet)}
}

// UnionRequirement returns a requirement on a union of version sets. The
// union is typically used for requirements that can be satisfied by version
// sets of different packages.
func UnionRequirement(union VersionSetUnionID) Requirement {
	return Requirement{id: uint32(union), union: true}
}

// IsUnion reports whether the requirement is on a version set union.
func (r Requirement) IsUnion() bool { return r.union }

// VersionSet returns the version set of a single requirement. The boolean is
// false for a union requirement.
func (r Requirement) VersionSet() (VersionSetID, bool) {
	if r.union {
		return 0, false
	}
	return VersionSetID(r.id), true
}

// Union returns the union of a union requirement. The boolean is false for a
// single requirement.
func (r Requirement) Union() (VersionSetUnionID, bool) {
	if !r.union {
		return 0, false
	}
	return VersionSetUnionID(r.id), true
}

// WithCondition returns a conditional requirement that is only enforced when
// condition holds.
func (r Requirement) WithCondition(condition ConditionID) ConditionalRequirement {
	return ConditionalRequirement{Requirement: r, Condition: condition, HasCondition: true}
}

// Unconditional returns a conditional requirement without a condition, i.e.
// one that is always enforced.
func (r Requirement) Unconditional() ConditionalRequirement {
	return ConditionalRequirement{Requirement: r}
}

// less orders requirements like resolvo's derived Ord: every single
// requirement sorts before every union, then by id.
func (r Requirement) less(o Requirement) bool {
	if r.union != o.union {
		return !r.union
	}
	return r.id < o.id
}

// versionSets returns the version sets that make up the requirement, in the
// order the interner reports them.
func (r Requirement) versionSets(interner Interner) []VersionSetID {
	if !r.union {
		return []VersionSetID{VersionSetID(r.id)}
	}
	return interner.VersionSetsInUnion(VersionSetUnionID(r.id))
}

// Display formats the requirement for humans: "name versionset", with the
// version sets of a union separated by " | ".
func (r Requirement) Display(interner Interner) string {
	if !r.union {
		return displayVersionSetWithName(interner, VersionSetID(r.id))
	}
	parts := interner.VersionSetsInUnion(VersionSetUnionID(r.id))
	var b strings.Builder
	for i, vs := range parts {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(displayVersionSetWithName(interner, vs))
	}
	return b.String()
}

// displayVersionSetWithName formats "name versionset" with trailing
// whitespace removed, so an empty version set display yields just the name.
func displayVersionSetWithName(interner Interner, vs VersionSetID) string {
	s := interner.DisplayName(interner.VersionSetName(vs)) + " " + interner.DisplayVersionSet(vs)
	return strings.TrimRightFunc(s, unicode.IsSpace)
}

// ConditionalRequirement is a requirement that is only enforced while its
// condition holds. A requirement without a condition is always enforced.
//
// ConditionalRequirement is a comparable value; it is safe for concurrent
// use.
type ConditionalRequirement struct {
	// Requirement is the requirement on another package.
	Requirement Requirement
	// Condition gates the requirement when HasCondition is true.
	Condition ConditionID
	// HasCondition reports whether Condition is set.
	HasCondition bool
}

// LogicalOperator combines two conditions. It is a plain value, safe for
// concurrent use.
type LogicalOperator uint8

const (
	// And holds when both operands hold.
	And LogicalOperator = iota
	// Or holds when either operand holds.
	Or
)

// String returns "and" or "or".
func (op LogicalOperator) String() string {
	if op == And {
		return "and"
	}
	return "or"
}

// ConditionKind discriminates the two shapes of a Condition. It is a plain
// value, safe for concurrent use.
type ConditionKind uint8

const (
	// ConditionRequirement holds when a solvable matching VersionSet is
	// selected.
	ConditionRequirement ConditionKind = iota
	// ConditionBinary combines the conditions LHS and RHS with Op.
	ConditionBinary
)

// Condition is a boolean expression over version sets. It holds or not
// depending on which solvables are selected.
//
// Condition is a comparable value; it is safe for concurrent use.
type Condition struct {
	// Kind selects which of the other fields are meaningful.
	Kind ConditionKind
	// VersionSet is the version set of a ConditionRequirement.
	VersionSet VersionSetID
	// Op combines LHS and RHS of a ConditionBinary.
	Op LogicalOperator
	// LHS is the left operand of a ConditionBinary.
	LHS ConditionID
	// RHS is the right operand of a ConditionBinary.
	RHS ConditionID
}

// RequirementCondition returns a condition that holds when a solvable
// matching versionSet is selected.
func RequirementCondition(versionSet VersionSetID) Condition {
	return Condition{Kind: ConditionRequirement, VersionSet: versionSet}
}

// BinaryCondition returns a condition combining lhs and rhs with op.
func BinaryCondition(op LogicalOperator, lhs, rhs ConditionID) Condition {
	return Condition{Kind: ConditionBinary, Op: op, LHS: lhs, RHS: rhs}
}

// reqMap is a map keyed by Requirement, split into one dense slice per
// requirement kind so that lookups on the propagation hot path are a branch
// and an index instead of a hash.
type reqMap[V any] struct {
	singles  []V
	singleOK []bool
	unions   []V
	unionOK  []bool
}

func (m *reqMap[V]) get(r Requirement) (V, bool) {
	var zero V
	if r.union {
		if int(r.id) < len(m.unions) && m.unionOK[r.id] {
			return m.unions[r.id], true
		}
		return zero, false
	}
	if int(r.id) < len(m.singles) && m.singleOK[r.id] {
		return m.singles[r.id], true
	}
	return zero, false
}

// ptr returns a pointer to the value for r, or nil when r has no entry. The
// pointer is invalidated by the next insertion.
func (m *reqMap[V]) ptr(r Requirement) *V {
	if r.union {
		if int(r.id) < len(m.unions) && m.unionOK[r.id] {
			return &m.unions[r.id]
		}
		return nil
	}
	if int(r.id) < len(m.singles) && m.singleOK[r.id] {
		return &m.singles[r.id]
	}
	return nil
}

func (m *reqMap[V]) set(r Requirement, v V) {
	if r.union {
		checkID("version set union", r.id)
	} else {
		checkID("version set", r.id)
	}
	if r.union {
		m.unions, m.unionOK = growPair(m.unions, m.unionOK, int(r.id))
		m.unions[r.id] = v
		m.unionOK[r.id] = true
		return
	}
	m.singles, m.singleOK = growPair(m.singles, m.singleOK, int(r.id))
	m.singles[r.id] = v
	m.singleOK[r.id] = true
}

// getOrInsert returns a pointer to the value for r, inserting the result of
// def first when r has no entry yet. The pointer is invalidated by the next
// insertion.
func (m *reqMap[V]) getOrInsert(r Requirement, def func() V) *V {
	if p := m.ptr(r); p != nil {
		return p
	}
	m.set(r, def())
	return m.ptr(r)
}

func growPair[V any](vals []V, ok []bool, idx int) ([]V, []bool) {
	if idx < len(vals) {
		return vals, ok
	}
	n := max(idx+1, 2*len(vals))
	nv := make([]V, n)
	copy(nv, vals)
	nb := make([]bool, n)
	copy(nb, ok)
	return nv, nb
}
