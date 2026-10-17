// Ported from resolvo src/id.rs (BSD-3-Clause, see NOTICE).

package cdcl

import "fmt"

// NameID identifies an interned package name.
//
// IDs are small dense integers handed out by the provider (usually through a
// Pool). The solver indexes slices with them, so a provider should keep them
// dense and start near zero. Like every ID type in this package it is a
// plain value, safe for concurrent use.
type NameID uint32

// SolvableID identifies a single candidate of a package: one concrete
// version (and build) that can be selected by the solver. Like every ID in
// this package it should be small and dense.
type SolvableID uint32

// VersionSetID identifies an interned version set: a set of versions of one
// package, such as "foo >=1,<2". The provider decides what a version set
// means through DependencyProvider.FilterCandidates.
type VersionSetID uint32

// VersionSetUnionID identifies an interned union (logical OR) of two or more
// version sets, possibly of different packages.
type VersionSetUnionID uint32

// StringID identifies an interned string, for example the reason a candidate
// was excluded.
type StringID uint32

// ConditionID identifies an interned Condition that gates a conditional
// requirement.
type ConditionID uint32

// MaxID bounds every NameID, SolvableID, VersionSetID and VersionSetUnionID
// a provider hands to the solver: they must be below MaxID. The solver keeps
// its state in slices indexed by these IDs, so a larger ID would make it
// allocate memory in proportion; Solve fails with ErrInvalidID instead.
const MaxID = 1 << 24

// idError is panicked when a provider ID is out of range and turned into an
// error wrapping ErrInvalidID at the API boundary.
type idError struct {
	kind string
	id   uint32
}

// Error implements error.
func (e idError) Error() string {
	return fmt.Sprintf("cdcl: %s id %d is not below MaxID (%d)", e.kind, e.id, MaxID)
}

// Unwrap returns ErrInvalidID.
func (e idError) Unwrap() error { return ErrInvalidID }

// checkID panics with an idError if a provider ID is not below MaxID.
func checkID(kind string, id uint32) {
	if id >= MaxID {
		panic(idError{kind: kind, id: id})
	}
}
