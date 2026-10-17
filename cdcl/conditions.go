// Ported from resolvo src/solver/conditions.rs (BSD-3-Clause, see NOTICE).

package cdcl

// Conditional requirements: `A requires B if C`.
//
// `A requires B` is the clause (¬A ∨ B1 ∨ … ∨ Bn). Adding the condition C
// gives (¬A ∨ ¬C ∨ B1 ∨ … ∨ Bn), where C, "a candidate matching C's version
// set is installed", is the auxiliary variable H_C defined exactly by
// (¬m ∨ H_C) for every matching candidate m and (¬H_C ∨ m1 ∨ … ∨ mk) (see
// encoder.conditionHolds). The clause becomes (¬A ∨ ¬H_C ∨ B1 ∨ … ∨ Bn).
//
// resolvo represents ¬C instead by the candidates of C's package that do
// not match C, (¬A ∨ C!1 ∨ … ∨ C!k ∨ B1 ∨ … ∨ Bn), with an at-least-one
// variable when every candidate matches. That also reads "C holds" when C's
// package is not installed at all, so a clause encoded once the condition
// held keeps forcing B, or a non-matching version of C, after backtracking:
// it can make a solvable problem unsat or install packages nobody needs,
// and it never enforces conditions on packages that allow several versions.
//
// Conditions are boolean expression trees, converted to disjunctive normal
// form. One requires clause is added per disjunct, with one H per version
// set of the disjunct. The normal form and the matching candidates of its
// version sets are computed once per condition (conditionInfo) and shared
// by every requirement that carries it; normal forms above
// maxConditionSize version sets are rejected.
//
// Lazy candidate loading: when the encoder reaches a conditional requirement
// whose condition does not hold yet, the requirement is deferred with the
// indices of its disjuncts that do not hold, and its candidates are not
// fetched. The solver loop checks after each round whether a pending
// disjunct has started to hold; if so it is drained and encoded exactly as
// the eager path would have.

// disjunction is one conjunctive clause of a condition's DNF, as the literals
// that appear in the requires clause.
type disjunction struct {
	literals []literal
}

// convertConditionToDNF converts a condition tree to disjunctive normal form:
// the outer slice is the disjunction (OR), each inner slice a conjunction
// (AND) of version sets. depth bounds the recursion so a cyclic or hostile
// condition graph cannot overflow the stack.
func convertConditionToDNF(c ConditionID, interner Interner, depth int) ([][]VersionSetID, error) {
	if depth > maxConditionDepth {
		return nil, errConditionTooDeep
	}
	cond := interner.ResolveCondition(c)
	switch {
	case cond.Kind == ConditionRequirement:
		return [][]VersionSetID{{cond.VersionSet}}, nil
	case cond.Op == Or:
		l, err := convertConditionToDNF(cond.LHS, interner, depth+1)
		if err != nil {
			return nil, err
		}
		r, err := convertConditionToDNF(cond.RHS, interner, depth+1)
		if err != nil {
			return nil, err
		}
		if dnfSize(l)+dnfSize(r) > maxConditionSize {
			return nil, errConditionTooLarge
		}
		return append(l, r...), nil
	default:
		l, err := convertConditionToDNF(cond.LHS, interner, depth+1)
		if err != nil {
			return nil, err
		}
		r, err := convertConditionToDNF(cond.RHS, interner, depth+1)
		if err != nil {
			return nil, err
		}
		// Distribute AND over OR.
		if len(l)*len(r) > maxConditionSize || len(l)*dnfSize(r)+len(r)*dnfSize(l) > maxConditionSize {
			return nil, errConditionTooLarge
		}
		out := make([][]VersionSetID, 0, len(l)*len(r))
		for _, a := range l {
			for _, b := range r {
				merged := make([]VersionSetID, 0, len(a)+len(b))
				merged = append(merged, a...)
				merged = append(merged, b...)
				out = append(out, merged)
			}
		}
		return out, nil
	}
}

const (
	// maxConditionDepth bounds the nesting of a condition tree.
	maxConditionDepth = 256
	// maxConditionSize bounds the number of version sets in a condition's
	// DNF, which can grow exponentially when ANDs of ORs are distributed
	// or shared subconditions are repeated. Every disjunct of the DNF is
	// one requires clause per solvable carrying the condition.
	maxConditionSize = 1 << 10
)

// dnfSize returns the number of version sets in a DNF.
func dnfSize(dnf [][]VersionSetID) int {
	n := 0
	for _, c := range dnf {
		n += len(c)
	}
	return n
}

// conditionInfo is a condition's DNF resolved against the cache: per
// disjunct, the version set of each conjunct and its matching candidates.
// It is computed once per condition and shared by every requirement that
// carries the condition, so the DNF is not expanded again per solvable.
type conditionInfo struct {
	disjuncts []disjunctData
	// indexed records that the matching candidates of every conjunct are
	// listed in solverState.deferredByCandidate.
	indexed bool
}

// holds reports whether disjunct i holds: every conjunct has a positively
// decided matching candidate. An empty conjunction trivially holds.
func (ci *conditionInfo) holds(i uint32, isPositivelyDecided func(SolvableID) bool) bool {
	for _, c := range ci.disjuncts[i].conjuncts {
		holds := false
		for _, s := range c.matching {
			if isPositivelyDecided(s) {
				holds = true
				break
			}
		}
		if !holds {
			return false
		}
	}
	return true
}

// disjunctMemo caches conditionInfo.holds for the disjuncts of one
// condition during one check.
type disjunctMemo struct {
	info  *conditionInfo
	state []int8 // 0 unknown, 1 holds, -1 does not hold
}

func (m *disjunctMemo) reset(info *conditionInfo) {
	m.info = info
	n := len(info.disjuncts)
	if cap(m.state) < n {
		m.state = make([]int8, n)
		return
	}
	m.state = m.state[:n]
	clear(m.state)
}

func (m *disjunctMemo) holds(i uint32, isPositivelyDecided func(SolvableID) bool) bool {
	if m.state[i] == 0 {
		m.state[i] = -1
		if m.info.holds(i, isPositivelyDecided) {
			m.state[i] = 1
		}
	}
	return m.state[i] == 1
}

// deferredRequirement is a conditional requirement whose condition did not
// fully hold when the encoder reached it; its candidates were therefore not
// loaded. pending lists, in DNF order, the disjuncts of the condition that
// did not hold, each one requires clause to encode once it holds.
type deferredRequirement struct {
	solvable    solvableOrRoot
	requirement ConditionalRequirement
	condition   ConditionID
	pending     []uint32
}

// firedDisjunct is a disjunct of a deferred requirement that started to
// hold: its requires clause is encoded, exactly once.
type firedDisjunct struct {
	solvable    solvableOrRoot
	requirement ConditionalRequirement
	condition   ConditionID
	disjunct    uint32
}
