// Ported from resolvo src/solver/encoding.rs (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"context"
	"errors"
	"fmt"
)

// constrainsAuxEncodingThreshold is the minimum number of non-matching
// candidates for which a constraint uses the shared auxiliary-variable
// encoding. Below it, pairwise clauses need at most as many clauses and
// propagate in a single hop.
const constrainsAuxEncodingThreshold = 4

// requiresAuxEncodingThreshold is the minimum number of candidates for which
// an unconditional requirement uses the shared gate encoding. Below it,
// repeating the small disjunction per requirer is cheaper than the gate.
const requiresAuxEncodingThreshold = 4

// encoder turns information from the dependency provider into clauses and
// variables.
//
// resolvo's encoder can drive provider futures concurrently. Go providers
// are synchronous, so this encoder runs each provider request as soon as it
// is queued and processes the results in FIFO order, exactly like resolvo's
// default (non-yielding) runtime.
type encoder struct {
	ctx      context.Context
	state    *solverState
	cache    *SolverCache
	provider DependencyProvider
	level    uint32
	rootDeps *Dependencies

	// pending holds results that are ready for processing, in FIFO order.
	pending     []taskResult
	pendingHead int

	// conflictingClauses lists clauses that conflict with the current
	// assignment.
	conflictingClauses []clauseID

	// pendingForbid lists, per package in first-seen order, the variables
	// that need forbid-multiple clauses.
	pendingForbid      []pendingForbidEntry
	pendingForbidIndex map[NameID]int

	// forbidSeen deduplicates pendingForbid; a variable belongs to exactly
	// one package so deduplicating globally is safe.
	forbidSeen bitSet

	// forbidRegistered lists the requirements whose candidates were
	// registered as forbid targets by this encoder.
	forbidRegistered map[Requirement]struct{}
}

type pendingForbidEntry struct {
	name NameID
	vars []variableID
}

type taskKind uint8

const (
	taskDependencies taskKind = iota
	taskCandidates
	taskConditionData
	taskRequirementCandidates
	taskConstraintCandidates
)

// taskResult is the outcome of one provider request.
type taskResult struct {
	kind taskKind
	err  error

	solvable solvableOrRoot

	// taskDependencies
	deps *Dependencies

	// taskCandidates
	name  NameID
	cands *Candidates

	// taskConditionData and taskRequirementCandidates
	requirement ConditionalRequirement
	condition   ConditionID
	condInfo    *conditionInfo

	// taskRequirementCandidates: the sorted candidates per version set, and
	// the condition disjuncts to encode right now.
	reqCandidates [][]SolvableID
	hasCondition  bool
	conditionDNF  [][]conditionConjunct

	// taskConstraintCandidates
	constraint      VersionSetID
	constraintCands []SolvableID
}

// conditionConjunct is one version set of a condition disjunct to encode,
// with its matching candidates sorted best first.
type conditionConjunct struct {
	versionSet VersionSetID
	sorted     []SolvableID
}

// conjunctData is one conjunct of a condition disjunct resolved against the
// cache: its matching candidates decide whether the conjunct holds.
type conjunctData struct {
	versionSet VersionSetID
	matching   []SolvableID
}

type disjunctData struct {
	conjuncts []conjunctData
}

// versionSets returns the version sets of the conjuncts.
func (d disjunctData) versionSets() []VersionSetID {
	vss := make([]VersionSetID, len(d.conjuncts))
	for i, c := range d.conjuncts {
		vss[i] = c.versionSet
	}
	return vss
}

func newEncoder(ctx context.Context, state *solverState, cache *SolverCache, rootDeps *Dependencies, level uint32) *encoder {
	return &encoder{
		ctx:                ctx,
		state:              state,
		cache:              cache,
		provider:           cache.provider,
		level:              level,
		rootDeps:           rootDeps,
		pendingForbidIndex: map[NameID]int{},
		forbidRegistered:   map[Requirement]struct{}{},
	}
}

func (e *encoder) push(r taskResult) { e.pending = append(e.pending, r) }

// encode adds the clauses for the given solvables and for deferred
// conditional requirements whose condition just fired. It returns the
// clauses that conflict with the current assignment.
func (e *encoder) encode(solvables []solvableOrRoot, deferred []firedDisjunct) ([]clauseID, error) {
	for _, s := range solvables {
		e.queueSolvable(s)
	}
	for _, d := range deferred {
		e.queueDeferredRequirement(d)
	}

	for e.pendingHead < len(e.pending) {
		r := e.pending[e.pendingHead]
		e.pending[e.pendingHead] = taskResult{}
		e.pendingHead++
		if r.err != nil {
			return nil, r.err
		}
		e.onTaskResult(r)
	}

	e.addPendingForbidClauses()
	return e.conflictingClauses, nil
}

func (e *encoder) onTaskResult(r taskResult) {
	switch r.kind {
	case taskDependencies:
		e.onDependenciesAvailable(r.solvable, r.deps)
	case taskCandidates:
		e.onCandidatesAvailable(r.name, r.cands)
	case taskConditionData:
		e.onConditionDataAvailable(r)
	case taskRequirementCandidates:
		e.onRequirementCandidatesAvailable(r)
	case taskConstraintCandidates:
		e.onConstraintCandidatesAvailable(r.solvable, r.constraint, r.constraintCands)
	}
}

// onDependenciesAvailable queues, for the dependencies of a solvable, the
// candidates of every package referenced by an unconditional requirement or
// a constraint, the matching candidates of every requirement and the
// non-matching candidates of every constraint. Packages reachable only
// through a conditional requirement are loaded lazily once the condition
// fires.
func (e *encoder) onDependenciesAvailable(s solvableOrRoot, deps *Dependencies) {
	if deps.Unknown {
		// Unknown dependencies exclude the solvable.
		e.addExclusionClause(s, deps.UnknownReason)
		return
	}

	for _, req := range deps.Requirements {
		if req.HasCondition {
			continue
		}
		for _, vs := range req.Requirement.versionSets(e.provider) {
			e.queuePackage(e.provider.VersionSetName(vs))
		}
	}
	for _, vs := range deps.Constrains {
		e.queuePackage(e.provider.VersionSetName(vs))
	}

	for _, req := range deps.Requirements {
		e.queueConditionalRequirement(s, req)
	}
	for _, vs := range deps.Constrains {
		e.queueConstraint(s, vs)
	}
}

// onCandidatesAvailable adds the package-level clauses: the lock clauses and
// the exclusion clauses.
func (e *encoder) onCandidatesAvailable(name NameID, cands *Candidates) {
	if cands.AllowMultiple {
		checkID("name", uint32(name))
		e.state.allowMultipleNames.insert(uint32(name))
	}
	if cands.HasLocked {
		e.addLockedPackageClauses(cands.Locked, cands.Candidates)
	}
	for _, ex := range cands.Excluded {
		e.addExclusionClause(solvableOrRoot{id: ex.Solvable}, ex.Reason)
	}
}

func (e *encoder) onRequirementCandidatesAvailable(r taskResult) {
	st := e.state
	variable := st.variableMap.internSolvableOrRoot(r.solvable)
	req := r.requirement.Requirement

	// On the lazy conditional path the parent may already be false. The
	// clause is still encoded: the assignment can be backtracked, and the
	// drained deferred entry is never revisited.

	vsVars := *st.requirementToSortedCandidates.getOrInsert(req, func() [][]variableID {
		out := make([][]variableID, len(r.reqCandidates))
		for i, cands := range r.reqCandidates {
			vars := make([]variableID, len(cands))
			for j, s := range cands {
				vars[j] = st.variableMap.internSolvable(s)
			}
			out[i] = vars
		}
		return out
	})

	// Every candidate we require needs forbid clauses so that at most one
	// solvable per package is installed. Only packages that can actually
	// be selected get them, to keep the clause count down.
	if _, seen := e.forbidRegistered[req]; !seen {
		e.forbidRegistered[req] = struct{}{}
		for i, cands := range r.reqCandidates {
			if i >= len(vsVars) || len(cands) == 0 {
				continue
			}
			name := e.provider.SolvableName(cands[0])
			if st.allowMultipleNames.contains(uint32(name)) {
				continue
			}
			entry := e.pendingForbidEntry(name)
			for _, v := range vsVars[i] {
				if e.forbidSeen.insert(uint32(v)) {
					e.pendingForbid[entry].vars = append(e.pendingForbid[entry].vars, v)
				}
			}
		}
	}

	// Queue the candidates' dependencies too if they are cheap to get.
	if _, done := st.fullyQueuedRequirements.get(req); !done {
		allQueued := true
		for _, cands := range r.reqCandidates {
			for _, c := range cands {
				if st.clausesAddedForSolvable.contains(solvableOrRoot{id: c}) {
					continue
				}
				if e.cache.AreDependenciesAvailableFor(c) {
					e.queueSolvable(solvableOrRoot{id: c})
				} else {
					allQueued = false
				}
			}
		}
		if allQueued {
			st.fullyQueuedRequirements.set(req, struct{}{})
		}
	}

	// Build one disjunction per condition disjunct: the condition holds
	// when, for every version set of the disjunct, a matching candidate is
	// installed, which the exact holds variable of the version set (see
	// conditionHolds) expresses.
	var conditions []*disjunctionID
	if r.hasCondition {
		for _, conjuncts := range r.conditionDNF {
			lits := make([]literal, 0, len(conjuncts))
			for _, c := range conjuncts {
				lits = append(lits, e.conditionHolds(c.versionSet, c.sorted).negative())
			}
			id := disjunctionID(len(st.disjunctions))
			st.disjunctions = append(st.disjunctions, disjunction{literals: lits})
			conditions = append(conditions, &id)
		}
	} else {
		conditions = append(conditions, nil)
	}

	candidateCount := 0
	for _, cands := range r.reqCandidates {
		candidateCount += len(cands)
	}
	noCandidates := candidateCount == 0

	for _, cond := range conditions {
		// Shared requires encoding: an unconditional requirement of a
		// non-root solvable with enough candidates encodes its candidate
		// disjunction once on a shared gate variable, and each requirer
		// only implies the gate. Root requirements stay direct so the
		// decision heuristic still recognizes them as explicit.
		if cond == nil && !noCandidates && !variable.isRoot() && candidateCount >= requiresAuxEncodingThreshold {
			e.addSharedRequires(variable, req)
			continue
		}

		vars := flattenVars(*st.requirementToSortedCandidates.ptr(req))
		var condLits []literal
		if cond != nil {
			condLits = st.disjunctions[*cond].literals
		}
		c, wl, conflict := requiresClause(variable, req, vars, cond, condLits, &st.decisionTracker)
		id := st.addClause(c, wl)
		st.addRequiresClause(variable, req, cond, id, e.requirementNames(req))

		if conflict {
			e.conflictingClauses = append(e.conflictingClauses, id)
		} else if noCandidates && cond == nil {
			// A requirement without candidates is a unit clause.
			st.negativeAssertions = append(st.negativeAssertions, assertion{v: variable, c: id})
		}
	}
}

func flattenVars(vss [][]variableID) []variableID {
	n := 0
	for _, vs := range vss {
		n += len(vs)
	}
	out := make([]variableID, 0, n)
	for _, vs := range vss {
		out = append(out, vs...)
	}
	return out
}

func (e *encoder) requirementNames(req Requirement) []NameID {
	vss := req.versionSets(e.provider)
	names := make([]NameID, len(vss))
	for i, vs := range vss {
		names[i] = e.provider.VersionSetName(vs)
	}
	return names
}

// conditionHolds returns the variable that is true exactly when a candidate
// matching versionSet is installed, encoding it on first use:
//
//   - (¬m ∨ holds) for every matching candidate m, so holds is true when a
//     matching candidate is installed;
//   - (¬holds ∨ m1 ∨ … ∨ mk), a requires clause on the version set with
//     holds as its parent, so holds is false when none is.
//
// resolvo instead encodes a condition through the candidates that do NOT
// match it, which also counts as "holds" when the package is not installed
// at all: a clause encoded once the condition held then keeps forcing the
// requirement (or a non-matching version) after backtracking, making
// solvable problems unsat or installing unrequested packages, and it never
// enforces conditions on packages that allow multiple versions.
func (e *encoder) conditionHolds(vs VersionSetID, sorted []SolvableID) variableID {
	st := e.state
	if v, ok := st.conditionVars[vs]; ok {
		return v
	}
	holds := st.variableMap.allocConditionHolds(vs)
	st.conditionVars[vs] = holds

	req := SingleRequirement(vs)
	vars := *st.requirementToSortedCandidates.getOrInsert(req, func() [][]variableID {
		out := make([]variableID, len(sorted))
		for i, s := range sorted {
			out[i] = st.variableMap.internSolvable(s)
		}
		return [][]variableID{out}
	})
	cands := flattenVars(vars)

	// The candidates must be decidable like any requirement's: they get
	// forbid-multiple clauses, and their package its lock and exclusion
	// clauses.
	name := e.provider.VersionSetName(vs)
	e.queuePackage(name)
	for _, v := range cands {
		e.registerForbidTarget(name, v)
	}

	c, wl, conflict := requiresClause(holds, req, cands, nil, nil, &st.decisionTracker)
	id := st.addClause(c, wl)
	st.addRequiresClause(holds, req, nil, id, []NameID{name})
	switch {
	case len(cands) == 0:
		// Nothing matches: the condition can never hold.
		st.negativeAssertions = append(st.negativeAssertions, assertion{v: holds, c: id})
	case conflict:
		// Every candidate is already false.
		st.decisionTracker.tryAddDecision(decision{variable: holds, value: false, derivedFrom: id}, e.level)
		st.recordUnitAtCreation(id)
	}

	for _, v := range cands {
		c, wl := anyOfClause(holds, v)
		id := st.addClause(c, wl)
		if st.decisionTracker.isAssigned(v, true) {
			st.decisionTracker.tryAddDecision(decision{variable: holds, value: true, derivedFrom: id}, e.level)
			st.recordUnitAtCreation(id)
		}
	}
	return holds
}

// addSharedRequires encodes the unconditional requirement
// parent -> (c1 ∨ … ∨ cN) through a per-requirement gate variable, so the
// candidate disjunction is emitted once per requirement instead of once per
// requirer: (¬gate ∨ c1 ∨ … ∨ cN) once, and (¬parent ∨ gate) per requirer.
func (e *encoder) addSharedRequires(parent variableID, req Requirement) {
	st := e.state
	gate, ok := st.requiresAuxVars[req]
	if !ok {
		gate = st.variableMap.allocRequiresGate(req)
		st.requiresAuxVars[req] = gate

		// The shared disjunction is a normal requires clause whose parent
		// is the gate.
		vars := flattenVars(*st.requirementToSortedCandidates.ptr(req))
		c, wl, conflict := requiresClause(gate, req, vars, nil, nil, &st.decisionTracker)
		id := st.addClause(c, wl)
		st.addRequiresClause(gate, req, nil, id, e.requirementNames(req))

		// All candidates are already false: the disjunction reduces to
		// ¬gate. The gate is fresh, so force it false; requirers then
		// propagate false through their implications.
		if conflict {
			st.decisionTracker.tryAddDecision(decision{variable: gate, value: false, derivedFrom: id}, e.level)
			st.recordUnitAtCreation(id)
		}
	}

	// The implication parent -> gate as the binary clause (gate ∨ ¬parent).
	c, wl := anyOfClause(gate, parent)
	id := st.addClause(c, wl)

	// Track the requirer so a backtrack that strands the gate can be
	// repaired (see solverState.forceStuckGates).
	st.addGateRequirer(gate, parent, id)

	// Honor assignments already made for either end of the implication.
	pv, pok := st.decisionTracker.assignedValue(parent)
	gv, gok := st.decisionTracker.assignedValue(gate)
	if (pok && pv) || (gok && !gv) {
		st.recordUnitAtCreation(id)
	}
	switch {
	case pok && pv && gok && !gv:
		// The requirer is installed but the gate is already false.
		e.conflictingClauses = append(e.conflictingClauses, id)
	case pok && pv && !gok:
		st.decisionTracker.tryAddDecision(decision{variable: gate, value: true, derivedFrom: id}, e.level)
	case !pok && gok && !gv:
		st.decisionTracker.tryAddDecision(decision{variable: parent, value: false, derivedFrom: id}, e.level)
	}
}

// onConstraintCandidatesAvailable adds the clauses that forbid the
// candidates excluded by a constraint.
func (e *encoder) onConstraintCandidatesAvailable(s solvableOrRoot, constraint VersionSetID, candidates []SolvableID) {
	st := e.state
	variable := st.variableMap.internSolvableOrRoot(s)

	if len(candidates) < constrainsAuxEncodingThreshold {
		// Pairwise encoding: one (¬parent ∨ ¬candidate) clause per excluded
		// candidate.
		for _, fc := range candidates {
			fv := st.variableMap.internSolvable(fc)
			c, wl, conflict := constrainsClause(variable, fv, constraint, &st.decisionTracker)
			id := st.addClause(c, wl)
			if !wl.ok {
				// The solvable constrains itself away: (¬A).
				st.negativeAssertions = append(st.negativeAssertions, assertion{v: variable, c: id})
			}
			if conflict {
				e.conflictingClauses = append(e.conflictingClauses, id)
			}
		}
		return
	}

	// Shared encoding: the (¬candidate ∨ aux) clauses are emitted once per
	// version set; each parent only adds (¬parent ∨ ¬aux).
	aux, ok := st.constrainsAuxVars[constraint]
	if !ok {
		aux = st.variableMap.allocConstrainsViolation(constraint)
		st.constrainsAuxVars[constraint] = aux
		for _, fc := range candidates {
			fv := st.variableMap.internSolvable(fc)
			c, wl := constrainsExcludedClause(fv, aux, constraint)
			id := st.addClause(c, wl)
			// An installed candidate must propagate immediately so the
			// parent clause below observes the violation, just like the
			// pairwise encoding does.
			if st.decisionTracker.isAssigned(fv, true) {
				st.decisionTracker.tryAddDecision(decision{variable: aux, value: true, derivedFrom: id}, e.level)
				st.recordUnitAtCreation(id)
			}
		}
	}

	c, wl, conflict := constrainsParentClause(variable, aux, constraint, &st.decisionTracker)
	id := st.addClause(c, wl)
	if conflict {
		e.conflictingClauses = append(e.conflictingClauses, id)
	}
}

// addLockedPackageClauses forbids every candidate but the locked one.
func (e *encoder) addLockedPackageClauses(locked SolvableID, all []SolvableID) {
	st := e.state
	lockedVar := st.variableMap.internSolvable(locked)
	for _, other := range all {
		if other == locked {
			continue
		}
		otherVar := st.variableMap.internSolvable(other)
		c, wl := lockClause(lockedVar, otherVar)
		st.addClause(c, wl)
	}
}

// addExclusionClause forbids a solvable for an external reason.
func (e *encoder) addExclusionClause(s solvableOrRoot, reason StringID) variableID {
	st := e.state
	v := st.variableMap.internSolvableOrRoot(s)
	id := st.addClause(excludedClause(v, reason), watchedLiterals{})

	// Exclusions are negative assertions, tracked outside the watches.
	st.negativeAssertions = append(st.negativeAssertions, assertion{v: v, c: id})

	// The clause conflicts if the solvable is already installed.
	if st.decisionTracker.isAssigned(v, true) {
		e.conflictingClauses = append(e.conflictingClauses, id)
	}
	return v
}

// queueSolvable requests the dependencies of a solvable, once.
func (e *encoder) queueSolvable(s solvableOrRoot) {
	if !e.state.clausesAddedForSolvable.insert(s) {
		return
	}
	r := taskResult{kind: taskDependencies, solvable: s}
	if s.root {
		r.deps = e.rootDeps
	} else {
		r.deps, r.err = e.cache.Dependencies(e.ctx, s.id)
	}
	e.push(r)
}

// queuePackage requests the candidates of a package, once.
func (e *encoder) queuePackage(name NameID) {
	checkID("name", uint32(name))
	if !e.state.clausesAddedForPackage.insert(uint32(name)) {
		return
	}
	r := taskResult{kind: taskCandidates, name: name}
	r.cands, r.err = e.cache.Candidates(e.ctx, name)
	e.push(r)
}

// queueConditionalRequirement requests the candidates of an unconditional
// requirement, or the condition data of a conditional one: its candidates
// are only fetched once at least one disjunct of the condition holds.
func (e *encoder) queueConditionalRequirement(s solvableOrRoot, req ConditionalRequirement) {
	if !req.HasCondition {
		e.queueRequirementCandidates(s, req, false, 0, nil)
		return
	}
	e.queueConditionData(s, req, req.Condition)
}

// getRequirementCandidates returns the sorted candidates of each version
// set of a requirement.
func (e *encoder) getRequirementCandidates(req Requirement) ([][]SolvableID, error) {
	vss := req.versionSets(e.provider)
	out := make([][]SolvableID, 0, len(vss))
	for _, vs := range vss {
		sorted, err := e.cache.sortedCandidatesForVersionSet(e.ctx, vs)
		if err != nil {
			return nil, err
		}
		out = append(out, sorted)
	}
	return out, nil
}

func (e *encoder) queueRequirementCandidates(s solvableOrRoot, req ConditionalRequirement, hasCond bool, cond ConditionID, dnf [][]VersionSetID) {
	r := taskResult{
		kind:         taskRequirementCandidates,
		solvable:     s,
		requirement:  req,
		hasCondition: hasCond,
		condition:    cond,
	}
	r.conditionDNF, r.err = e.sortedConditions(dnf)
	if r.err == nil {
		r.reqCandidates, r.err = e.getRequirementCandidates(req.Requirement)
	}
	e.push(r)
}

// sortedConditions returns the sorted matching candidates of every version
// set of the condition disjuncts to encode.
func (e *encoder) sortedConditions(dnf [][]VersionSetID) ([][]conditionConjunct, error) {
	out := make([][]conditionConjunct, 0, len(dnf))
	for _, conjuncts := range dnf {
		cs := make([]conditionConjunct, 0, len(conjuncts))
		for _, vs := range conjuncts {
			sorted, err := e.cache.sortedCandidatesForVersionSet(e.ctx, vs)
			if err != nil {
				return nil, err
			}
			cs = append(cs, conditionConjunct{versionSet: vs, sorted: sorted})
		}
		out = append(out, cs)
	}
	return out, nil
}

// queueConditionData requests the matching candidates of every conjunct of a
// condition. Only the condition's packages are fetched; the requirement's
// candidates are not, which is the point of the lazy path.
func (e *encoder) queueConditionData(s solvableOrRoot, req ConditionalRequirement, cond ConditionID) {
	r := taskResult{kind: taskConditionData, solvable: s, requirement: req, condition: cond}
	r.condInfo, r.err = e.conditionInfo(cond)
	if errors.Is(r.err, ErrConditionTooComplex) {
		r.err = fmt.Errorf("cdcl: condition %d of requirement %q of %s: %w", cond, req.Requirement.Display(e.provider), s.display(e.provider), r.err)
	}
	e.push(r)
}

// conditionInfo returns the shared conditionInfo of a condition, computing
// it on first use.
func (e *encoder) conditionInfo(cond ConditionID) (*conditionInfo, error) {
	if info, ok := e.state.conditionInfos[cond]; ok {
		return info, nil
	}
	disjuncts, err := e.conditionData(cond)
	if err != nil {
		return nil, err
	}
	info := &conditionInfo{disjuncts: disjuncts}
	e.state.conditionInfos[cond] = info
	return info, nil
}

func (e *encoder) conditionData(cond ConditionID) ([]disjunctData, error) {
	dnf, err := convertConditionToDNF(cond, e.provider, 0)
	if err != nil {
		return nil, err
	}
	out := make([]disjunctData, 0, len(dnf))
	for _, cnf := range dnf {
		d := disjunctData{conjuncts: make([]conjunctData, 0, len(cnf))}
		for _, vs := range cnf {
			matching, err := e.cache.MatchingCandidates(e.ctx, vs)
			if err != nil {
				return nil, err
			}
			d.conjuncts = append(d.conjuncts, conjunctData{versionSet: vs, matching: matching})
		}
		out = append(out, d)
	}
	return out, nil
}

// onConditionDataAvailable decides which disjuncts of a condition hold now.
// The requirement is encoded right away for those; every other disjunct is
// deferred until it fires.
func (e *encoder) onConditionDataAvailable(r taskResult) {
	st := e.state
	info := r.condInfo
	var fired [][]VersionSetID
	var pending []uint32
	for i, d := range info.disjuncts {
		if info.holds(uint32(i), st.isPositivelyDecided) {
			fired = append(fired, d.versionSets())
			continue
		}
		pending = append(pending, uint32(i))
	}

	if len(pending) > 0 {
		st.pushDeferred(deferredRequirement{
			solvable:    r.solvable,
			requirement: r.requirement,
			condition:   r.condition,
			pending:     pending,
		}, info)
	}

	if len(fired) > 0 {
		// The requirement is being encoded now; make sure its packages
		// have their candidates so lock and exclusion clauses are emitted
		// as on the eager path.
		for _, vs := range r.requirement.Requirement.versionSets(e.provider) {
			e.queuePackage(e.provider.VersionSetName(vs))
		}
		e.queueRequirementCandidates(r.solvable, r.requirement, true, r.condition, fired)
	}
}

// queueDeferredRequirement fetches the candidates of a deferred requirement
// whose disjunct just fired and emits its requires clause.
func (e *encoder) queueDeferredRequirement(d firedDisjunct) {
	for _, vs := range d.requirement.Requirement.versionSets(e.provider) {
		e.queuePackage(e.provider.VersionSetName(vs))
	}
	vss := e.state.conditionInfos[d.condition].disjuncts[d.disjunct].versionSets()
	e.queueRequirementCandidates(d.solvable, d.requirement, true, d.condition, [][]VersionSetID{vss})
}

// queueConstraint requests the candidates that do not match a constraint.
func (e *encoder) queueConstraint(s solvableOrRoot, constraint VersionSetID) {
	r := taskResult{kind: taskConstraintCandidates, solvable: s, constraint: constraint}
	r.constraintCands, r.err = e.cache.NonMatchingCandidates(e.ctx, constraint)
	e.push(r)
}

// pendingForbidEntry returns the index of the pending forbid entry of name,
// creating it if needed.
func (e *encoder) pendingForbidEntry(name NameID) int {
	if idx, ok := e.pendingForbidIndex[name]; ok {
		return idx
	}
	idx := len(e.pendingForbid)
	e.pendingForbidIndex[name] = idx
	e.pendingForbid = append(e.pendingForbid, pendingForbidEntry{name: name})
	return idx
}

// registerForbidTarget records variable as a candidate for forbid-multiple
// clauses of name, unless it was registered before or the package allows
// multiple versions.
func (e *encoder) registerForbidTarget(name NameID, v variableID) {
	if e.state.allowMultipleNames.contains(uint32(name)) {
		return
	}
	if e.forbidSeen.insert(uint32(v)) {
		idx := e.pendingForbidEntry(name)
		e.pendingForbid[idx].vars = append(e.pendingForbid[idx].vars, v)
	}
}

// addPendingForbidClauses adds the forbid-multiple clauses for solvables
// that were discovered as reachable from a requires clause. Only these get
// clauses, as an optimization.
func (e *encoder) addPendingForbidClauses() {
	st := e.state
	for _, entry := range e.pendingForbid {
		name := entry.name
		for _, candidate := range entry.vars {
			tracker := st.atMostOneTrackers[name]
			if tracker == nil {
				tracker = newAtMostOnceTracker[variableID]()
				st.atMostOneTrackers[name] = tracker
			}
			tracker.add(candidate,
				func(a, b variableID, positive bool) {
					litB := newLiteral(b, !positive)
					litA := a.negative()
					c, wl := forbidMultipleClause(a, litB, name)
					id := st.addClause(c, wl)

					// Propagate right away if one literal is already false.
					av, aok := litA.eval(st.decisionTracker.m())
					bv, bok := litB.eval(st.decisionTracker.m())
					if (aok && !av) || (bok && !bv) {
						st.recordUnitAtCreation(id)
					}
					var set literal
					switch {
					case aok && !av && !bok:
						set = litB
					case !aok && bok && !bv:
						set = litA
					case aok && !av && bok && !bv:
						// Both solvables are already installed but the
						// clause forbidding that did not exist yet.
						e.conflictingClauses = append(e.conflictingClauses, id)
						return
					default:
						return
					}
					st.decisionTracker.tryAddDecision(decision{variable: set.variable(), value: set.satisfyingValue(), derivedFrom: id}, e.level)
				},
				func() variableID { return st.variableMap.allocForbidMultiple(name) },
			)

		}
	}
	e.pendingForbid = nil
	clear(e.pendingForbidIndex)
}
