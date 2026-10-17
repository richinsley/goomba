// Ported from resolvo src/solver/decision.rs, src/solver/decision_map.rs and
// src/solver/decision_tracker.rs (BSD-3-Clause, see NOTICE).

package cdcl

// decision is an assignment of a value to a variable together with the
// clause it was derived from.
type decision struct {
	variable    variableID
	value       bool
	derivedFrom clauseID
}

// decisionMap stores the assignment of every variable and the level at which
// it was made: 0 means undecided, a positive level means true and a negative
// level means false.
type decisionMap struct {
	m []int32
}

func (d *decisionMap) reset(v variableID) {
	if int(v) < len(d.m) {
		d.m[v] = 0
	}
}

func (d *decisionMap) set(v variableID, value bool, level uint32) {
	if int(v) >= len(d.m) {
		n := max(int(v)+1, 2*len(d.m))
		nm := make([]int32, n)
		copy(nm, d.m)
		d.m = nm
	}
	if value {
		d.m[v] = int32(level)
	} else {
		d.m[v] = -int32(level)
	}
}

func (d *decisionMap) level(v variableID) uint32 {
	if int(v) >= len(d.m) {
		return 0
	}
	x := d.m[v]
	if x < 0 {
		return uint32(-x)
	}
	return uint32(x)
}

// value returns the value of v, or ok=false if v is undecided.
func (d *decisionMap) value(v variableID) (value, ok bool) {
	if int(v) >= len(d.m) {
		return false, false
	}
	x := d.m[v]
	return x > 0, x != 0
}

// decisionTracker tracks the assignments to variables. It keeps the trail
// (every assignment in chronological order, decisions and propagations
// alike) for backtracking and a map for querying current values.
type decisionTracker struct {
	dm decisionMap

	// stack is the trail. Assigning pushes onto it and backtracking pops
	// from it, so two snapshots always share a common prefix up to the
	// minimum length reached in between.
	stack []decision

	propagateIndex int

	// syncFloor is the minimum length of stack since the last
	// takeSyncFloor: the length of the trail prefix that is guaranteed
	// unchanged. The decide queue uses it to find what changed.
	syncFloor int

	// deferredFloor is the same for takeDeferredFloor, used by the check
	// for deferred conditions that started to hold.
	deferredFloor int
}

func (t *decisionTracker) clear() {
	clear(t.dm.m)
	t.stack = t.stack[:0]
	t.propagateIndex = 0
	t.syncFloor = 0
	t.deferredFloor = 0
}

func (t *decisionTracker) m() *decisionMap { return &t.dm }

func (t *decisionTracker) assignedValue(v variableID) (value, ok bool) {
	return t.dm.value(v)
}

// isAssigned reports whether v is assigned exactly value.
func (t *decisionTracker) isAssigned(v variableID, value bool) bool {
	x, ok := t.dm.value(v)
	return ok && x == value
}

// takeSyncFloor returns the minimum trail length reached since the previous
// call and resets the floor to the current trail length.
func (t *decisionTracker) takeSyncFloor() int {
	f := t.syncFloor
	t.syncFloor = len(t.stack)
	return f
}

// takeDeferredFloor is takeSyncFloor for a second, independent consumer.
func (t *decisionTracker) takeDeferredFloor() int {
	f := t.deferredFloor
	t.deferredFloor = len(t.stack)
	return f
}

func (t *decisionTracker) level(v variableID) uint32 { return t.dm.level(v) }

// findClauseForAssignment returns the clause that caused the assignment of v.
func (t *decisionTracker) findClauseForAssignment(v variableID) (clauseID, bool) {
	for _, d := range t.stack {
		if d.variable == v {
			return d.derivedFrom, true
		}
	}
	return 0, false
}

// tryAddDecision assigns d.value to d.variable. It returns true if the
// variable was undecided and false if it already had the same value; it
// returns conflict=true if the variable already has the opposite value.
func (t *decisionTracker) tryAddDecision(d decision, level uint32) (added, conflict bool) {
	v, ok := t.dm.value(d.variable)
	if !ok {
		t.dm.set(d.variable, d.value, level)
		t.stack = append(t.stack, d)
		return true, false
	}
	if v == d.value {
		return false, false
	}
	return false, true
}

// undoUntil backtracks until every remaining assignment was made at level or
// below. Level 0 clears everything.
func (t *decisionTracker) undoUntil(level uint32) {
	if level == 0 {
		t.clear()
		return
	}
	for len(t.stack) > 0 {
		d := t.stack[len(t.stack)-1]
		if t.level(d.variable) <= level {
			break
		}
		t.undoLast()
	}
}

// undoLast pops the last assignment and returns it together with the level
// of the assignment that is now on top of the trail.
func (t *decisionTracker) undoLast() (decision, uint32) {
	d := t.stack[len(t.stack)-1]
	t.stack = t.stack[:len(t.stack)-1]
	t.dm.reset(d.variable)
	t.propagateIndex = len(t.stack)
	t.syncFloor = min(t.syncFloor, len(t.stack))
	t.deferredFloor = min(t.deferredFloor, len(t.stack))
	if len(t.stack) == 0 {
		return d, 0
	}
	return d, t.dm.level(t.stack[len(t.stack)-1].variable)
}

// nextUnpropagated returns the next assignment for which unit propagation
// still has to run and marks it as propagated.
func (t *decisionTracker) nextUnpropagated() (decision, bool) {
	if t.propagateIndex >= len(t.stack) {
		return decision{}, false
	}
	d := t.stack[t.propagateIndex]
	t.propagateIndex++
	return d, true
}
