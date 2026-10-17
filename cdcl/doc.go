// Package cdcl is a generic conflict-driven clause-learning (CDCL) package
// solver, ported from resolvo (https://github.com/prefix-dev/resolvo,
// BSD-3-Clause, see NOTICE). It knows nothing about any packaging
// ecosystem: a DependencyProvider supplies package names, candidates,
// version sets and dependencies, and the solver asks for them lazily, only
// for the packages it reaches.
//
// The solver is the MiniSat CDCL algorithm with watched literals, learnt
// clauses, non-chronological backjumping and a VSIDS-like activity
// heuristic, with data structures modelled on libsolv. When no solution
// exists, Solve returns a *Conflict whose Display method explains why in a
// tree like:
//
//	The following packages are incompatible
//	├─ bluesky-widgets >=0, <100 can be installed with any of the following options:
//	│  └─ bluesky-widgets 42 would require
//	│     └─ suitcase-utils >=0, <54, which can be installed with any of the following options:
//	│        └─ suitcase-utils 53
//	└─ suitcase-utils >=54, <100 cannot be installed because there are no viable options:
//	   └─ suitcase-utils 54, which conflicts with the versions reported above.
//
// # Providers
//
// A provider implements DependencyProvider (which embeds Interner). IDs
// (NameID, SolvableID, VersionSetID, ...) are small dense integers chosen
// by the provider; Pool interns names, solvables, version sets, unions,
// conditions and strings so providers do not have to. Provider methods are
// synchronous: they may block on I/O and must honour their context.
//
// # Determinism
//
// Given the same provider answers, Solve makes the same decisions and
// returns the same solution and the same explanation. The search order,
// clause encoding and explanation match resolvo v0.12.2 run with its
// default (non-yielding) runtime, except where noted below.
//
// # Differences from resolvo
//
//   - Providers are synchronous Go methods that take a context and may
//     return errors; resolvo's async methods and cancellation value are
//     replaced by ctx, and a provider error aborts the solve.
//   - SortCandidates receives the SolverCache, as in resolvo, so providers
//     can sort by cached candidates and dependencies.
//   - IDs are dense integers below MaxID; an out-of-range provider ID fails
//     the solve with ErrInvalidID.
//   - Soft requirements (resolvo's Problem::soft_requirements) are not
//     supported. resolvo solves each one on top of the decisions made
//     before it, and that is unsound: solutions can install two versions
//     of a package, excluded or locked-out packages, or drop conditional
//     requirements, and a soft requirement can turn a solvable problem into
//     a conflict. Fuzzing also showed resolvo leaving stale clauses after a
//     soft requirement's restart, returning unfinished assignments after
//     backjumping below a soft requirement's starting level, and panicking
//     ("already decided", "entered unreachable code"). Prefer installed
//     packages with Candidates.Favored or Candidates.Locked instead.
//   - A conditional requirement is encoded with an exact "condition holds"
//     variable per version set (see conditions.go). resolvo encodes the
//     negated condition by the versions that do not match it, which can
//     make solvable problems unsat, install packages nobody requires, and
//     never enforces a condition on a package that allows several versions.
//   - A solvable whose constraint excludes itself is encoded as the
//     assertion it is (resolvo watches one literal twice).
//   - When a backjump strands an implication the encoder made outside
//     propagation, resolvo can accept an assignment that violates it. cdcl
//     re-derives such implications before accepting a solution, as resolvo
//     already does for shared-requires gates.
//   - Hostile or inconsistent providers fail closed: cyclic conditions,
//     conditions nested deeper than 256 levels and conditions whose
//     disjunctive normal form has more than 1024 version sets return
//     ErrConditionTooComplex, a SortCandidates that sorts recursively
//     returns ErrRecursiveSort, and any panic during a solve (invariant
//     violation, provider, Pool) returns an error wrapping ErrInternal
//     instead of crashing. A condition's normal form is computed once per
//     solve and shared by every requirement that carries it.
//   - Explanations are bounded. resolvo expands a candidate again each time
//     the explanation tree reaches it, which is exponential in the depth of
//     the conflict graph and never terminates on a cyclic one (and Error
//     renders the explanation). cdcl explains a candidate that requires or
//     constrains other packages once and shows later occurrences as
//     "<candidate>, as reported above.", so the number of lines is linear in
//     the size of the conflict graph; Display and Error stop at 64 KiB or
//     2000 lines with a final "… explanation truncated" line, and Graphviz
//     at 1 MiB.
//   - Deferred conditions are checked from the assignments made since the
//     previous check instead of by rescanning every deferred requirement.
//   - Explanations follow resolvo's and can be incomplete where resolvo
//     cannot explain either: conditions do not appear in them, and resolvo
//     panics when a node of the conflict graph is not reachable from the
//     root, whereas cdcl reports the reachable part.
//   - DependencySnapshot is an in-memory structure; resolvo's optional serde
//     support is not ported.
package cdcl
