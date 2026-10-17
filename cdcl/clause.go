// Ported from resolvo src/solver/clause.rs and src/internal/id.rs
// (BSD-3-Clause, see NOTICE).

package cdcl

import "math"

// variableID identifies a variable of the SAT problem. Index 0 is the root
// variable; every other variable is a solvable or an auxiliary variable (see
// variableOrigin).
type variableID uint32

// rootVariable is the variable that represents the root of the decision tree.
const rootVariable variableID = 0

func (v variableID) isRoot() bool { return v == rootVariable }

// positive returns the literal that is true when v is assigned true.
func (v variableID) positive() literal { return literal(uint32(v) << 1) }

// negative returns the literal that is true when v is assigned false.
func (v variableID) negative() literal { return literal(uint32(v)<<1 | 1) }

// literal is a variable together with a polarity: A or ¬A. It is encoded as
// variable<<1 | negate.
type literal uint32

func newLiteral(v variableID, negate bool) literal {
	if negate {
		return v.negative()
	}
	return v.positive()
}

func (l literal) variable() variableID { return variableID(l >> 1) }

// negate reports whether the literal is the negation of its variable.
func (l literal) negate() bool { return l&1 == 1 }

// satisfyingValue returns the value that makes the literal true when
// assigned to its variable.
func (l literal) satisfyingValue() bool { return l&1 == 0 }

// eval evaluates the literal, or returns ok=false if its variable has no
// value yet.
func (l literal) eval(m *decisionMap) (value, ok bool) {
	v, ok := m.value(l.variable())
	if !ok {
		return false, false
	}
	return v != l.negate(), true
}

// isFalse reports whether the literal is assigned and evaluates to false.
func (l literal) isFalse(m *decisionMap) bool {
	v, ok := l.eval(m)
	return ok && !v
}

// isTrue reports whether the literal is assigned and evaluates to true.
func (l literal) isTrue(m *decisionMap) bool {
	v, ok := l.eval(m)
	return ok && v
}

// clauseID identifies a clause. Clause 0 is always the InstallRoot clause.
type clauseID uint32

// noClause terminates the watch linked lists.
const noClause clauseID = math.MaxUint32

const installRootClause clauseID = 0

// learntClauseID indexes solverState.learntClauses.
type learntClauseID uint32

// disjunctionID indexes solverState.disjunctions.
type disjunctionID uint32

// clauseKind discriminates the clause shapes the solver supports. Since the
// solver only targets dependency resolution, a small fixed set of shapes is
// enough and a clause does not need to store its literals explicitly.
type clauseKind uint8

const (
	// clauseInstallRoot asserts the root: (root).
	clauseInstallRoot clauseKind = iota
	// clauseRequires makes a solvable require the candidates of a
	// requirement, optionally only when a condition holds:
	// (¬A ∨ ¬D1 ∨ … ∨ ¬Dn ∨ B1 ∨ … ∨ Bm), where Di are the condition's
	// disjunction literals and Bi the requirement's candidates.
	clauseRequires
	// clauseForbidMultiple keeps two solvables of one package from both being
	// installed. With the binary at-most-one encoding the second literal is
	// a helper variable: (¬A ∨ ±H).
	clauseForbidMultiple
	// clauseConstrains forbids a candidate that does not satisfy a
	// solvable's constraint: (¬A ∨ ¬B).
	clauseConstrains
	// clauseConstrainsExcluded makes the constraint's auxiliary variable
	// true when an excluded candidate is installed: (¬candidate ∨ aux).
	clauseConstrainsExcluded
	// clauseConstrainsParent forbids a parent together with the constraint's
	// auxiliary variable: (¬parent ∨ ¬aux).
	clauseConstrainsParent
	// clauseLock forbids every candidate but the locked one: (¬root ∨ ¬B).
	// The locked solvable is carried for error messages only.
	clauseLock
	// clauseLearnt is a clause learnt from a conflict; its literals live in
	// solverState.learntClauses.
	clauseLearnt
	// clauseExcluded forbids a solvable for an external reason: (¬A).
	clauseExcluded
	// clauseAnyOf makes a selection variable true when a variable is true:
	// (selected ∨ ¬B).
	clauseAnyOf
)

// clause is a compact tagged union of the clause shapes.
type clause struct {
	kind clauseKind
	// v1 is the first variable: the parent, candidate, locked or selected
	// variable depending on kind.
	v1 variableID
	// x is the second variable, a literal (ForbidMultiple), a learnt clause
	// id or a string id (Excluded).
	x uint32
	// y is a version set id, a name id (ForbidMultiple) or a disjunction id
	// plus one (Requires; zero means no condition).
	y uint32
	// req is the requirement of a Requires clause.
	req Requirement
}

func (c *clause) v2() variableID { return variableID(c.x) }

func (c *clause) condition() (disjunctionID, bool) {
	if c.y == 0 {
		return 0, false
	}
	return disjunctionID(c.y - 1), true
}

// isBinary reports whether the clause has exactly two literals that never
// change. Propagation skips the search for a replacement watch for these.
func (c *clause) isBinary() bool {
	switch c.kind {
	case clauseConstrains, clauseConstrainsExcluded, clauseConstrainsParent,
		clauseForbidMultiple, clauseLock, clauseAnyOf:
		return true
	}
	return false
}

// watchedLiterals keeps the two literals a clause watches and its position
// in the two linked lists of clauses watching those literals.
type watchedLiterals struct {
	lits [2]literal
	next [2]clauseID
	// ok is false for clauses that watch nothing (assertions).
	ok bool
}

func newWatches(a, b literal) watchedLiterals {
	return watchedLiterals{lits: [2]literal{a, b}, next: [2]clauseID{noClause, noClause}, ok: true}
}

// clauseLiterals provides what is needed to enumerate the literals of a
// clause that does not store them inline.
type clauseLiterals struct {
	learnt       [][]literal
	sorted       *reqMap[[][]variableID]
	disjunctions []disjunction
}

// visit calls f for each literal of c, in clause order, until f returns
// false.
func (cl *clauseLiterals) visit(c *clause, f func(literal) bool) {
	switch c.kind {
	case clauseInstallRoot:
		panic("cdcl: internal error: visiting the literals of the install-root clause")
	case clauseExcluded:
		f(c.v1.negative())
	case clauseLearnt:
		for _, l := range cl.learnt[c.x] {
			if !f(l) {
				return
			}
		}
	case clauseRequires:
		if !f(c.v1.negative()) {
			return
		}
		if d, ok := c.condition(); ok {
			for _, l := range cl.disjunctions[d].literals {
				if !f(l) {
					return
				}
			}
		}
		cands, _ := cl.sorted.get(c.req)
		for _, vs := range cands {
			for _, v := range vs {
				if !f(v.positive()) {
					return
				}
			}
		}
	case clauseConstrains:
		if f(c.v1.negative()) {
			f(c.v2().negative())
		}
	case clauseConstrainsExcluded:
		if f(c.v1.negative()) {
			f(c.v2().positive())
		}
	case clauseConstrainsParent:
		if f(c.v1.negative()) {
			f(c.v2().negative())
		}
	case clauseForbidMultiple:
		if f(c.v1.negative()) {
			f(literal(c.x))
		}
	case clauseLock:
		if f(c.v2().negative()) {
			f(rootVariable.negative())
		}
	case clauseAnyOf:
		if f(c.v1.positive()) {
			f(c.v2().negative())
		}
	}
}

// nextUnwatchedLiteral finds a literal of c that can replace the watch at
// index forWatch: one that is not the other watch and is either unassigned
// or true.
func (w *watchedLiterals) nextUnwatchedLiteral(c *clause, cl *clauseLiterals, m *decisionMap, forWatch int) (literal, bool) {
	other := w.lits[1-forWatch]
	usable := func(l literal) bool {
		if l == other {
			return false
		}
		v, ok := l.eval(m)
		return !ok || v
	}
	switch c.kind {
	case clauseInstallRoot, clauseExcluded:
		panic("cdcl: internal error: clause without watches is being propagated")
	case clauseLearnt:
		for _, l := range cl.learnt[c.x] {
			if usable(l) {
				return l, true
			}
		}
		return 0, false
	case clauseRequires:
		if l := c.v1.negative(); usable(l) {
			return l, true
		}
		if d, ok := c.condition(); ok {
			for _, l := range cl.disjunctions[d].literals {
				if usable(l) {
					return l, true
				}
			}
		}
		cands, _ := cl.sorted.get(c.req)
		for _, vs := range cands {
			for _, v := range vs {
				if l := v.positive(); usable(l) {
					return l, true
				}
			}
		}
		return 0, false
	}
	// Binary clauses can never move their watches.
	return 0, false
}

// requiresClause builds a Requires clause. It returns the clause, its
// watches (ok=false when the clause is an assertion) and whether the clause
// conflicts with the current assignment.
//
// On the lazy conditional path the parent may already be false when the
// clause is built. The clause is then satisfied, but that assignment can be
// backtracked, so the clause must still be added.
func requiresClause(parent variableID, req Requirement, candidates []variableID, cond *disjunctionID, condLits []literal, dt *decisionTracker) (clause, watchedLiterals, bool) {
	parentIsFalse := dt.isAssigned(parent, false)
	c := clause{kind: clauseRequires, v1: parent, req: req}
	if cond != nil {
		c.y = uint32(*cond) + 1
	}

	total := len(condLits) + len(candidates)
	if total == 0 {
		// An assertion never conflicts: it either re-asserts an already
		// false parent or assigns the parent false during propagation.
		return c, watchedLiterals{}, false
	}
	litAt := func(i int) literal {
		if i < len(condLits) {
			return condLits[i]
		}
		return candidates[i-len(condLits)].positive()
	}
	for i := 0; i < total; i++ {
		l := litAt(i)
		if !l.isFalse(dt.m()) {
			return c, newWatches(parent.negative(), l), false
		}
	}
	// All candidates are false. Unless the parent is false as well the
	// clause conflicts with the current decisions. The watches are not
	// valid right now but become valid once the solver restarts.
	return c, newWatches(parent.negative(), litAt(0)), !parentIsFalse
}

// constrainsClause builds a pairwise Constrains clause; see requiresClause
// for the meaning of the results.
func constrainsClause(parent, forbidden variableID, via VersionSetID, dt *decisionTracker) (clause, watchedLiterals, bool) {
	parentIsFalse := dt.isAssigned(parent, false)
	conflict := !parentIsFalse && dt.isAssigned(forbidden, true)
	c := clause{kind: clauseConstrains, v1: parent, x: uint32(forbidden), y: uint32(via)}
	if parent == forbidden {
		// A solvable that constrains itself away reduces to the assertion
		// (¬A). Watching the same literal twice would corrupt the watch
		// lists, so the clause is added without watches and asserted.
		return c, watchedLiterals{}, conflict
	}
	return c, newWatches(parent.negative(), forbidden.negative()), conflict
}

func constrainsExcludedClause(candidate, aux variableID, via VersionSetID) (clause, watchedLiterals) {
	return clause{kind: clauseConstrainsExcluded, v1: candidate, x: uint32(aux), y: uint32(via)},
		newWatches(candidate.negative(), aux.positive())
}

func constrainsParentClause(parent, aux variableID, via VersionSetID, dt *decisionTracker) (clause, watchedLiterals, bool) {
	parentIsFalse := dt.isAssigned(parent, false)
	conflict := !parentIsFalse && dt.isAssigned(aux, true)
	return clause{kind: clauseConstrainsParent, v1: parent, x: uint32(aux), y: uint32(via)},
		newWatches(parent.negative(), aux.negative()), conflict
}

func forbidMultipleClause(candidate variableID, constrained literal, name NameID) (clause, watchedLiterals) {
	return clause{kind: clauseForbidMultiple, v1: candidate, x: uint32(constrained), y: uint32(name)},
		newWatches(candidate.negative(), constrained)
}

func lockClause(locked, other variableID) (clause, watchedLiterals) {
	return clause{kind: clauseLock, v1: locked, x: uint32(other)},
		newWatches(rootVariable.negative(), other.negative())
}

func learntClause(id learntClauseID, lits []literal) (clause, watchedLiterals) {
	c := clause{kind: clauseLearnt, x: uint32(id)}
	if len(lits) == 1 {
		// An assertion was learnt; it needs no watches.
		return c, watchedLiterals{}
	}
	return c, newWatches(lits[0], lits[len(lits)-1])
}

func excludedClause(candidate variableID, reason StringID) clause {
	return clause{kind: clauseExcluded, v1: candidate, x: uint32(reason)}
}

func anyOfClause(selected, other variableID) (clause, watchedLiterals) {
	return clause{kind: clauseAnyOf, v1: selected, x: uint32(other)},
		newWatches(selected.positive(), other.negative())
}
