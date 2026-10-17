// Ported from resolvo src/solver/decide_queue.rs (BSD-3-Clause, see NOTICE).

package cdcl

import "fmt"

// The decide queue is an incremental work queue for Solver.decide.
//
// decide must pick a candidate from a requires clause that is currently
// eligible: its parent is assigned true, its condition (if any) holds, and no
// candidate is assigned true yet. Only a small fraction of all requires
// clauses is eligible at any point, so instead of inspecting every clause on
// every call the queue tracks the eligible set incrementally.
//
//   - Every requires clause is registered as a trackedClause, identified by
//     its position: the parent's registration index and the clause's index
//     in the parent's list. Both are append-only, so positions are stable
//     and iterating by position visits clauses in a fixed order.
//   - queue holds the positions of possibly-eligible clauses: a superset of
//     the eligible clauses, restricted to clauses whose parent is true.
//     Clauses are removed only when an inspection proves them ineligible,
//     and every assignment change that can make a clause eligible
//     re-inserts it through occurrence lists (see sync).
//   - nextDecision takes the first eligible clause by position as the
//     initial best and then considers only the hot clauses after it. A
//     clause replaces the best only with strictly higher package activity;
//     activities are non-negative and a package never involved in a
//     conflict has activity exactly zero, so only clauses naming a package
//     whose activity was ever bumped (hot) can win.

// clausePosition packs a parent registration index and a clause index.
type clausePosition struct {
	parent uint32
	clause uint32
}

// positionSet is an ordered set of clause positions. Positions are dense, so
// it is a bitset per parent plus a bitset of non-empty parents.
type positionSet struct {
	parents bitSet
	members []bitSet
	counts  []int
}

func (s *positionSet) insert(p clausePosition) {
	for int(p.parent) >= len(s.members) {
		s.members = append(s.members, bitSet{})
		s.counts = append(s.counts, 0)
	}
	if s.members[p.parent].insert(p.clause) {
		s.counts[p.parent]++
		s.parents.insert(p.parent)
	}
}

func (s *positionSet) contains(p clausePosition) bool {
	return int(p.parent) < len(s.members) && s.members[p.parent].contains(p.clause)
}

func (s *positionSet) remove(p clausePosition) {
	if !s.contains(p) {
		return
	}
	s.members[p.parent].remove(p.clause)
	s.counts[p.parent]--
	if s.counts[p.parent] == 0 {
		s.parents.remove(p.parent)
	}
}

// first returns the smallest position in the set.
func (s *positionSet) first() (clausePosition, bool) {
	parent, ok := s.parents.nextFrom(0)
	if !ok {
		return clausePosition{}, false
	}
	c, _ := s.members[parent].nextFrom(0)
	return clausePosition{parent: parent, clause: c}, true
}

// after returns the smallest position strictly greater than p.
func (s *positionSet) after(p clausePosition) (clausePosition, bool) {
	if int(p.parent) < len(s.members) && p.clause < ^uint32(0) {
		if c, ok := s.members[p.parent].nextFrom(p.clause + 1); ok {
			return clausePosition{parent: p.parent, clause: c}, true
		}
	}
	if p.parent == ^uint32(0) {
		return clausePosition{}, false
	}
	parent, ok := s.parents.nextFrom(p.parent + 1)
	if !ok {
		return clausePosition{}, false
	}
	c, _ := s.members[parent].nextFrom(0)
	return clausePosition{parent: parent, clause: c}, true
}

// trackedClause is a requires clause as tracked by the queue.
type trackedClause struct {
	position clausePosition
	parent   variableID
	req      Requirement
	// cond is the condition disjunction plus one; zero means none.
	cond     uint32
	clauseID clauseID
	// hot reports whether any package name in the requirement ever had its
	// activity bumped. Only hot clauses can replace a running best.
	hot bool
}

// reqStateKind is the cached result of walking a requirement's candidates.
type reqStateKind uint8

const (
	// reqDirty: not evaluated yet, or a candidate assignment changed.
	reqDirty reqStateKind = iota
	// reqSatisfied: candidate `by` is true, so every clause with this
	// requirement is satisfied for as long as it stays true.
	reqSatisfied
	// reqFrontier: no candidate is true; `candidate` is the first undecided
	// candidate in walk order and count the number of undecided candidates
	// in its version set.
	reqFrontier
)

type requirementState struct {
	kind       reqStateKind
	candidate  variableID
	versionSet VersionSetID
	count      uint32
}

type requirementEntry struct {
	state requirementState
	// occurrencesRegistered is set on the first evaluation; until then the
	// entry stays dirty, so a missed wake-up cannot lose information.
	occurrencesRegistered bool
}

// queueDecision is the decision selected by nextDecision.
type queueDecision struct {
	candidate       variableID
	requiredBy      variableID
	clauseID        clauseID
	packageActivity float32
	candidateCount  uint32
}

type decideQueue struct {
	clauses []trackedClause
	// parentPositions maps a parent variable to its registration index
	// plus one; zero means unregistered.
	parentPositions []uint32
	clausesByParent [][]uint32

	queue    positionSet
	hotQueue positionSet

	// hotNames are the names whose activity was ever bumped. The set never
	// shrinks; decay can bring an activity back to zero, which only makes
	// it a conservative superset.
	hotNames bitSet
	// clausesByName lists the clauses whose requirement mentions a name.
	clausesByName [][]uint32

	// requirementsByCandidate lists, per candidate variable, the
	// requirements whose walk inspected it.
	requirementsByCandidate [][]Requirement
	// clausesByConditionVariable lists, per variable, the clauses whose
	// condition disjunction mentions it.
	clausesByConditionVariable [][]uint32

	requirementStates    reqMap[requirementEntry]
	clausesByRequirement reqMap[[]uint32]

	// mirror holds the trail variables as of the previous sync.
	mirror []variableID

	// hotOnly enables the activity shortcuts, which are only sound for
	// activity_add > 0 and activity_decay >= 0.
	hotOnly bool

	// sawFalsified is set when an inspection finds an eligible clause whose
	// candidates are all false: a falsified clause that propagation did not
	// see, which is an internal error (resolvo panics there).
	sawFalsified bool
}

func newDecideQueue() *decideQueue { return &decideQueue{hotOnly: true} }

func growIndex[T any](s []T, idx int) []T {
	if idx < len(s) {
		return s
	}
	n := max(idx+1, 2*len(s))
	ns := make([]T, n)
	copy(ns, s)
	return ns
}

// enqueueClause inserts a clause into the queue (and the hot queue if it is
// hot), unless its parent is not true. Filtering on the parent keeps the
// churn of candidates being forbidden away from the queue.
func (q *decideQueue) enqueueClause(m *decisionMap, id uint32) {
	c := &q.clauses[id]
	if v, ok := m.value(c.parent); !ok || !v {
		return
	}
	q.queue.insert(c.position)
	if c.hot {
		q.hotQueue.insert(c.position)
	}
}

// registerClause registers a newly encoded requires clause. It must be called
// exactly once per requires clause, in encoding order.
func (q *decideQueue) registerClause(parent variableID, req Requirement, cond *disjunctionID, cid clauseID, names []NameID, disjunctions []disjunction, parentValue, parentAssigned bool) {
	q.parentPositions = growIndex(q.parentPositions, int(parent))
	if q.parentPositions[parent] == 0 {
		q.clausesByParent = append(q.clausesByParent, nil)
		q.parentPositions[parent] = uint32(len(q.clausesByParent))
	}
	parentPos := q.parentPositions[parent] - 1
	position := clausePosition{parent: parentPos, clause: uint32(len(q.clausesByParent[parentPos]))}
	id := uint32(len(q.clauses))

	// A union can mention the same package several times; deduplicate so
	// the occurrence lists hold each clause once.
	hot := false
	var seen []NameID
outer:
	for _, name := range names {
		for _, s := range seen {
			if s == name {
				continue outer
			}
		}
		seen = append(seen, name)
		if q.hotNames.contains(uint32(name)) {
			hot = true
		}
		checkID("name", uint32(name))
		q.clausesByName = growIndex(q.clausesByName, int(name))
		q.clausesByName[name] = append(q.clausesByName[name], id)
	}

	var condField uint32
	if cond != nil {
		condField = uint32(*cond) + 1
		for _, l := range disjunctions[*cond].literals {
			v := l.variable()
			q.clausesByConditionVariable = growIndex(q.clausesByConditionVariable, int(v))
			q.clausesByConditionVariable[v] = append(q.clausesByConditionVariable[v], id)
		}
	}

	q.requirementStates.getOrInsert(req, func() requirementEntry { return requirementEntry{} })
	byReq := q.clausesByRequirement.getOrInsert(req, func() []uint32 { return nil })
	*byReq = append(*byReq, id)

	q.clausesByParent[parentPos] = append(q.clausesByParent[parentPos], id)
	q.clauses = append(q.clauses, trackedClause{
		position: position,
		parent:   parent,
		req:      req,
		cond:     condField,
		clauseID: cid,
		hot:      hot,
	})

	if parentAssigned && parentValue {
		q.queue.insert(position)
		if hot {
			q.hotQueue.insert(position)
		}
	}
}

// markNameHot marks a package name as hot and promotes the queued clauses
// that mention it into the hot queue.
func (q *decideQueue) markNameHot(name NameID) {
	checkID("name", uint32(name))
	if !q.hotNames.insert(uint32(name)) {
		return
	}
	if int(name) >= len(q.clausesByName) {
		return
	}
	for _, id := range q.clausesByName[name] {
		c := &q.clauses[id]
		if c.hot {
			continue
		}
		c.hot = true
		if q.queue.contains(c.position) {
			q.hotQueue.insert(c.position)
		}
	}
}

// sync brings the queue up to date with every assignment change since the
// previous call. trail is the solver's chronological assignment log and
// floor the lowest trail length reached since the previous sync: mirror
// [..floor] is unchanged, mirror[floor..] was popped at some point and
// trail[floor..] was pushed since. Variables in the last two ranges are
// re-routed through the occurrence lists; routing is idempotent.
func (q *decideQueue) sync(floor int, trail []decision, m *decisionMap) {
	floor = min(floor, len(q.mirror), len(trail))
	for i := floor; i < len(q.mirror); i++ {
		q.routeTouched(q.mirror[i], m)
	}
	q.mirror = q.mirror[:floor]
	for _, d := range trail[floor:] {
		q.mirror = append(q.mirror, d.variable)
		q.routeTouched(d.variable, m)
	}
}

// routeTouched routes one variable whose assignment may have changed through
// the occurrence lists.
func (q *decideQueue) routeTouched(v variableID, m *decisionMap) {
	value, ok := m.value(v)

	// Parent wake-up: a clause can only become eligible when its parent is
	// assigned true.
	if ok && value && int(v) < len(q.parentPositions) && q.parentPositions[v] != 0 {
		for _, id := range q.clausesByParent[q.parentPositions[v]-1] {
			q.enqueueClause(m, id)
		}
	}

	// Candidate wake-up: invalidate the walk caches of the requirements that
	// inspected this variable and wake clauses whose satisfaction broke.
	if int(v) < len(q.requirementsByCandidate) {
		for _, req := range q.requirementsByCandidate[v] {
			entry := q.requirementStates.ptr(req)
			switch entry.state.kind {
			case reqFrontier:
				entry.state.kind = reqDirty
			case reqSatisfied:
				if bv, bok := m.value(entry.state.candidate); bok && bv {
					// The satisfying candidate is still true.
					continue
				}
				entry.state.kind = reqDirty
				if woken, ok := q.clausesByRequirement.get(req); ok {
					for _, id := range woken {
						q.enqueueClause(m, id)
					}
				}
			}
		}
	}

	// Condition wake-up: condition literals carry polarity, so any change
	// can complete an all-false condition.
	if int(v) < len(q.clausesByConditionVariable) {
		for _, id := range q.clausesByConditionVariable[v] {
			q.enqueueClause(m, id)
		}
	}
}

// evalRequirement walks a requirement's sorted candidates through the cache:
// it returns the satisfying candidate if one is true, or the first
// undecided candidate, its version set and the number of undecided
// candidates in that version set.
func (q *decideQueue) evalRequirement(req Requirement, m *decisionMap, sorted *reqMap[[][]variableID], provider Interner) requirementState {
	entry := q.requirementStates.ptr(req)
	if entry.state.kind != reqDirty {
		return entry.state
	}
	vsCands, _ := sorted.get(req)

	if !entry.occurrencesRegistered {
		entry.occurrencesRegistered = true
		for _, cands := range vsCands {
			for _, c := range cands {
				q.requirementsByCandidate = growIndex(q.requirementsByCandidate, int(c))
				q.requirementsByCandidate[c] = append(q.requirementsByCandidate[c], req)
			}
		}
	}

	var first requirementState
	haveFirst := false
	vss := req.versionSets(provider)
walk:
	for i, cands := range vsCands {
		if i >= len(vss) {
			break
		}
		vs := vss[i]
		for _, c := range cands {
			value, ok := m.value(c)
			switch {
			case ok && value:
				entry.state = requirementState{kind: reqSatisfied, candidate: c}
				break walk
			case ok:
			case haveFirst:
				if first.versionSet == vs {
					first.count++
				}
			default:
				first = requirementState{kind: reqFrontier, candidate: c, versionSet: vs, count: 1}
				haveFirst = true
			}
		}
	}

	if entry.state.kind == reqDirty {
		if !haveFirst {
			// Propagation would have assigned the parent false; the
			// caller reports an internal error. The entry stays dirty.
			return requirementState{kind: reqDirty}
		}
		entry.state = first
	}
	return entry.state
}

// inspect checks whether a clause is eligible and returns its requirement
// frontier if it is.
func (q *decideQueue) inspect(c trackedClause, m *decisionMap, sorted *reqMap[[][]variableID], disjunctions []disjunction, provider Interner) (requirementState, bool) {
	// Only clauses whose parent is installed.
	if v, ok := m.value(c.parent); !ok || !v {
		return requirementState{}, false
	}
	// Skip clauses whose condition does not hold yet.
	if c.cond != 0 {
		for _, l := range disjunctions[c.cond-1].literals {
			if !l.isFalse(m) {
				return requirementState{}, false
			}
		}
	}
	st := q.evalRequirement(c.req, m, sorted, provider)
	switch st.kind {
	case reqSatisfied:
		return requirementState{}, false
	case reqDirty:
		// Every candidate is false: the clause is falsified.
		q.sawFalsified = true
		return requirementState{}, false
	}
	return st, true
}

func activityOf(nameActivity []float32, name NameID) float32 {
	if int(name) < len(nameActivity) {
		return nameActivity[name]
	}
	return 0
}

// nextDecision selects the best decision among the eligible clauses,
// visiting them in position order: the first eligible clause is the initial
// best, and a later clause replaces it only with strictly higher package
// activity and strictly fewer remaining candidates. Clauses of the root are
// always preferred. Ineligible clauses reached on the way are dequeued.
//
// maxActivity must be exactly the largest value in nameActivity.
func (q *decideQueue) nextDecision(m *decisionMap, sorted *reqMap[[][]variableID], disjunctions []disjunction, nameActivity []float32, maxActivity float32, provider Interner) (queueDecision, bool) {
	type best struct {
		position clausePosition
		explicit bool
		activity float32
		count    uint32
		decision queueDecision
	}
	hotOnly := q.hotOnly
	// Replacement needs strictly fewer candidates (so a count of one is
	// unbeatable) and, with standard activity parameters, strictly higher
	// activity (so the global maximum is unbeatable).
	unbeatable := func(b *best) bool {
		return b.count == 1 || (hotOnly && b.activity == maxActivity)
	}

	var b best
	found := false
	for {
		pos, ok := q.queue.first()
		if !ok {
			break
		}
		c := q.clauses[q.clausesByParent[pos.parent][pos.clause]]
		st, eligible := q.inspect(c, m, sorted, disjunctions, provider)
		if !eligible {
			q.queue.remove(pos)
			q.hotQueue.remove(pos)
			continue
		}
		act := activityOf(nameActivity, provider.VersionSetName(st.versionSet))
		b = best{
			position: pos,
			explicit: c.parent == rootVariable,
			activity: act,
			count:    st.count,
			decision: queueDecision{candidate: st.candidate, requiredBy: c.parent, clauseID: c.clauseID},
		}
		found = true
		break
	}
	if !found {
		return queueDecision{}, false
	}

	// Try to replace the best with an eligible clause after it. With
	// standard activity parameters only hot clauses can win.
	if !unbeatable(&b) {
		cursor := b.position
		for {
			var pos clausePosition
			var ok bool
			if hotOnly {
				pos, ok = q.hotQueue.after(cursor)
			} else {
				pos, ok = q.queue.after(cursor)
			}
			if !ok {
				break
			}
			cursor = pos

			c := q.clauses[q.clausesByParent[pos.parent][pos.clause]]
			isExplicit := c.parent == rootVariable

			// Explicit requirements (clauses of the root) are preferred;
			// other clauses are skipped without inspection and stay queued.
			if b.explicit && !isExplicit {
				continue
			}

			st, eligible := q.inspect(c, m, sorted, disjunctions, provider)
			if !eligible {
				q.queue.remove(pos)
				q.hotQueue.remove(pos)
				continue
			}
			act := activityOf(nameActivity, provider.VersionSetName(st.versionSet))

			// Prefer a higher package activity to root out conflicts
			// faster, and fewer candidates to reduce backtracking. Both
			// must improve strictly.
			if b.activity >= act {
				continue
			}
			if b.count <= st.count {
				continue
			}
			b = best{
				position: pos,
				explicit: isExplicit,
				activity: act,
				count:    st.count,
				decision: queueDecision{candidate: st.candidate, requiredBy: c.parent, clauseID: c.clauseID},
			}
			if unbeatable(&b) {
				break
			}
		}
	}

	d := b.decision
	d.packageActivity = b.activity
	d.candidateCount = b.count
	return d, true
}

// debugAssertInvariants verifies the heuristic-independent bookkeeping: every
// eligible clause is queued, hot flags match the hot names and the hot queue
// mirrors the queue for hot clauses. A violation would not crash or change
// solutions, it would only silently degrade the search.
func (q *decideQueue) debugAssertInvariants(m *decisionMap, sorted *reqMap[[][]variableID], disjunctions []disjunction, nameActivity []float32, maxActivity float32, provider Interner) {
	for id, c := range q.clauses {
		if v, ok := m.value(c.parent); !ok || !v {
			continue
		}
		if c.cond != 0 {
			allFalse := true
			for _, l := range disjunctions[c.cond-1].literals {
				if !l.isFalse(m) {
					allFalse = false
					break
				}
			}
			if !allFalse {
				continue
			}
		}
		satisfied := false
		vsCands, _ := sorted.get(c.req)
	walk:
		for _, cands := range vsCands {
			for _, cand := range cands {
				if v, ok := m.value(cand); ok && v {
					satisfied = true
					break walk
				}
			}
		}
		if satisfied {
			continue
		}
		if !q.queue.contains(c.position) {
			panic(internalError(fmt.Sprintf("eligible requires clause %d is not queued", id)))
		}
	}

	for id, c := range q.clauses {
		shouldBeHot := false
		for _, vs := range c.req.versionSets(provider) {
			if q.hotNames.contains(uint32(provider.VersionSetName(vs))) {
				shouldBeHot = true
				break
			}
		}
		if c.hot != shouldBeHot {
			panic(internalError(fmt.Sprintf("clause %d hot flag out of sync with the hot name set", id)))
		}
		if c.hot && q.queue.contains(c.position) != q.hotQueue.contains(c.position) {
			panic(internalError(fmt.Sprintf("hot queue out of lockstep for clause %d", id)))
		}
	}

	if q.hotOnly {
		var actual float32
		for _, a := range nameActivity {
			actual = max(actual, a)
		}
		if actual != maxActivity {
			panic(internalError("max activity diverged from the largest stored activity"))
		}
	}
}
