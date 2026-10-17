// Ported from resolvo src/solver/mod.rs (BSD-3-Clause, see NOTICE).

package cdcl

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// The solver implements conflict-driven clause learning as described in
// "An Extensible SAT-solver" (MiniSat), with data structures modelled on
// libsolv, adapted for lazily loaded package metadata.

// ErrUnsolvable is matched (with errors.Is) by the *Conflict error that
// Solve returns when the requirements cannot be satisfied.
var ErrUnsolvable = errors.New("cdcl: no solution satisfies the requirements")

// ErrConditionTooComplex is returned (wrapped) when a condition is nested
// more than 256 levels deep or its disjunctive normal form has more than
// 1024 version sets, for example because the provider's conditions form a
// cycle.
var ErrConditionTooComplex = errors.New("cdcl: condition too complex")

// ErrRecursiveSort is returned (wrapped) when a provider's SortCandidates
// asks the cache for the sorted candidates of the version set it is sorting.
var ErrRecursiveSort = errors.New("cdcl: recursive candidate sort")

// ErrInternal is returned (wrapped) when the solver detects a violation of
// its own invariants, or when anything panics during a solve, the solver or
// a provider method. It indicates a bug, or a provider that breaks its
// contract, for example by answering inconsistently.
var ErrInternal = errors.New("cdcl: internal error")

// ErrInvalidID is returned (wrapped) when a provider hands the solver an ID
// that is not below MaxID.
var ErrInvalidID = errors.New("cdcl: provider id out of range")

var (
	errConditionTooDeep  = fmt.Errorf("%w: nested deeper than %d", ErrConditionTooComplex, maxConditionDepth)
	errConditionTooLarge = fmt.Errorf("%w: more than %d version sets", ErrConditionTooComplex, maxConditionSize)
)

// internalError is panicked on invariant violations and turned into an
// error wrapping ErrInternal at the API boundary.
type internalError string

// Error implements error.
func (e internalError) Error() string { return "cdcl: internal error: " + string(e) }

// Unwrap returns ErrInternal.
func (e internalError) Unwrap() error { return ErrInternal }

// recoveredError turns a panic recovered at the API boundary into an error,
// so that the solver fails closed instead of crashing the process: an
// out-of-range provider ID wraps ErrInvalidID, anything else (an invariant
// violation, a runtime error, a provider or Pool panic) wraps ErrInternal
// and carries the panic value. With debug checks enabled (tests only)
// invariant violations are re-raised to keep their stack.
func recoveredError(r any, debug bool) error {
	switch e := r.(type) {
	case idError:
		return e
	case internalError:
		if debug {
			panic(r)
		}
		return e
	case error:
		return fmt.Errorf("%w: panic: %w", ErrInternal, e)
	}
	return fmt.Errorf("%w: panic: %v", ErrInternal, r)
}

// Problem describes what to solve. It is plain data: safe for concurrent
// reads; Solve does not modify it.
type Problem struct {
	// Requirements must each have one matching candidate in the solution.
	Requirements []ConditionalRequirement

	// Constraints restrict the packages they name: if such a package is in
	// the solution, it must match the constraint.
	Constraints []VersionSetID
}

// Solver drives the SAT solving process. It is not safe for concurrent use;
// use one Solver per goroutine.
type Solver struct {
	provider DependencyProvider
	cache    *SolverCache
	state    *solverState

	// activityAdd is added to the activity of every package that is part
	// of a conflict.
	activityAdd float32
	// activityDecay (between 0 and 1) multiplies every activity on each
	// conflict.
	activityDecay float32

	// debugChecks enables expensive invariant checks (resolvo's debug
	// assertions). Tests turn it on.
	debugChecks bool
}

// NewSolver returns a solver for the given provider.
func NewSolver(provider DependencyProvider) *Solver {
	return &Solver{
		provider:      provider,
		cache:         NewSolverCache(provider),
		activityAdd:   1.0,
		activityDecay: 0.95,
	}
}

// SetActivityParams tunes the conflict-driven decision heuristic: add is
// added to the activity of each package involved in a conflict and every
// activity is multiplied by decay after each conflict. The defaults are 1.0
// and 0.95.
func (s *Solver) SetActivityParams(add, decay float32) {
	s.activityAdd = add
	s.activityDecay = decay
}

// Provider returns the solver's provider.
func (s *Solver) Provider() DependencyProvider { return s.provider }

// Cache returns the solver's cache of provider answers.
func (s *Solver) Cache() *SolverCache { return s.cache }

// ClauseCount returns the number of clauses of the last solve.
func (s *Solver) ClauseCount() int {
	if s.state == nil {
		return 0
	}
	return len(s.state.clauses)
}

// DeferredRequirementsCount returns the number of deferred conditional
// requirement disjuncts of the last solve whose condition never fired.
func (s *Solver) DeferredRequirementsCount() int {
	if s.state == nil {
		return 0
	}
	n := 0
	for _, e := range s.state.deferred {
		for _, d := range e.entries {
			n += len(d.pending)
		}
	}
	return n
}

// Solve solves the problem.
//
// It returns the selected solvables in the order they were decided. If the
// requirements cannot be satisfied it returns a *Conflict (which matches
// ErrUnsolvable with errors.Is) explaining why. If ctx is cancelled it
// returns an error wrapping ctx.Err(). Errors from the provider are returned
// wrapped. Solve never panics: a panic during the solve, in the solver or in
// a provider method, is returned as an error wrapping ErrInternal.
func (s *Solver) Solve(ctx context.Context, problem Problem) (solution []SolvableID, err error) {
	defer func() {
		if r := recover(); r != nil {
			solution, err = nil, recoveredError(r, s.debugChecks)
		}
	}()

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cdcl: solve cancelled: %w", err)
	}

	s.state = newSolverState()
	s.state.decideQueue.hotOnly = s.activityAdd > 0 && s.activityDecay >= 0

	rootDeps := &Dependencies{Requirements: problem.Requirements, Constrains: problem.Constraints}

	// The first clause is always the install-root clause.
	s.state.addClause(clause{kind: clauseInstallRoot}, watchedLiterals{})

	if err := s.runSat(ctx, rootDeps); err != nil {
		return nil, s.wrapError(ctx, err)
	}
	return s.state.chosenSolvables(), nil
}

// wrapError turns an error from the search into the error Solve returns.
func (s *Solver) wrapError(ctx context.Context, err error) error {
	var conflict *Conflict
	if errors.As(err, &conflict) {
		return conflict
	}
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return fmt.Errorf("cdcl: solve cancelled: %w", err)
	}
	return err
}

// runSat runs the CDCL algorithm:
//
//  1. Set: decide a value for an unassigned variable, starting a new level.
//  2. Propagate: unit-propagate the consequences at the same level.
//  3. Learn: on a conflict, learn a clause that forbids the combination of
//     assignments that caused it, backjump and continue, or report the
//     problem unsolvable when the conflict cannot be avoided.
//
// Clauses are added lazily: whenever a partial solution selects solvables
// whose dependencies were not encoded yet, they are encoded and the search
// continues; if the new clauses invalidate the partial solution, it starts
// over. It returns nil when a solution was found and a *Conflict when none
// exists.
func (s *Solver) runSat(ctx context.Context, rootDeps *Dependencies) error {
	st := s.state
	root := solvableOrRoot{root: true}
	var level uint32
	for {
		// Level 0 means the decision loop was reset because new clauses
		// invalidated the partial solution.
		if level == 0 {
			level = 1

			// The root must be installed: it carries the requirements of
			// the problem.
			if _, conflict := st.decisionTracker.tryAddDecision(decision{variable: rootVariable, value: true, derivedFrom: installRootClause}, level); conflict {
				panic(internalError("root already decided"))
			}

			conflicting, err := newEncoder(ctx, st, s.cache, rootDeps, level).encode([]solvableOrRoot{root}, nil)
			if err != nil {
				return err
			}
			if len(conflicting) > 0 {
				return s.analyzeUnsolvable(conflicting[0])
			}
		}

		conflictClause, conflict, err := s.propagate(ctx, level)
		if err != nil {
			return err
		}
		if conflict {
			if level == 1 {
				return s.analyzeUnsolvable(conflictClause)
			}
			// New clauses invalidated the partial solution: start over.
			level = 0
			st.decisionTracker.undoUntil(0)
			continue
		}

		level, err = s.resolveDependencies(ctx, level)
		if err != nil {
			return err
		}

		// The partial solution satisfies every clause added so far. Find
		// the selected solvables whose dependencies were not encoded yet.
		var newSolvables []solvableOrRoot
		for _, d := range st.decisionTracker.stack {
			if !d.value {
				continue
			}
			sr, ok := st.variableMap.asSolvableOrRoot(d.variable)
			if !ok || st.clausesAddedForSolvable.contains(sr) {
				continue
			}
			newSolvables = append(newSolvables, sr)
		}

		// Deferred conditional requirements whose condition started to hold
		// must be encoded before the solution is complete.
		fired := st.firedDeferredConditions()

		// Re-derive shared-requires gates stranded by a backtrack (as
		// resolvo does).
		forcedStuckGates := st.forceStuckGates(level)

		if len(newSolvables) == 0 && len(fired) == 0 {
			// Before accepting the solution, re-derive every other
			// implication the encoder made outside propagation that a
			// backtrack stranded (see forceStrandedUnits).
			forcedStranded := st.forceStrandedUnits(level)
			if forcedStuckGates || forcedStranded {
				// Only stranded implications were repaired: propagate and
				// decide again.
				continue
			}
			return nil
		}

		var solvables []solvableOrRoot
		for _, sr := range newSolvables {
			if !sr.root {
				solvables = append(solvables, sr)
			}
		}
		var deferred []firedDisjunct
		for _, c := range fired {
			deferred = append(deferred, st.drainFiredDeferred(c)...)
		}

		conflicting, err := newEncoder(ctx, st, s.cache, rootDeps, level).encode(solvables, deferred)
		if err != nil {
			return err
		}
		if len(conflicting) > 0 {
			st.decisionTracker.undoUntil(0)
			level = 0
		}
	}
}

// errFalsifiedClause reports an eligible requires clause whose candidates
// are all false: a falsified clause that propagation did not see.
var errFalsifiedClause = internalError("every candidate of an eligible requirement is false")

// resolveDependencies repeatedly decides the next variable and propagates,
// until no decision is left to make. It returns the new level.
func (s *Solver) resolveDependencies(ctx context.Context, level uint32) (uint32, error) {
	for {
		d, ok := s.decide()
		if s.state.decideQueue.sawFalsified {
			s.state.decideQueue.sawFalsified = false
			return level, errFalsifiedClause
		}
		if !ok {
			return level, nil
		}
		var err error
		level, err = s.setPropagateLearn(ctx, level, d.candidate, d.clauseID)
		if err != nil {
			return 0, err
		}
	}
}

// decide selects the next solvable to install once every logical consequence
// has been propagated. The heuristics, in order of importance:
//
//  1. Prefer explicit (root) requirements, so direct dependencies are
//     maximized over transitive ones.
//  2. Prefer packages with a higher activity score, which is bumped for
//     packages involved in conflicts and decays over time (VSIDS-like).
//  3. Prefer requirements with fewer candidates left, which need less
//     backtracking to prove unsatisfiable.
func (s *Solver) decide() (queueDecision, bool) {
	st := s.state
	floor := st.decisionTracker.takeSyncFloor()
	st.decideQueue.sync(floor, st.decisionTracker.stack, st.decisionTracker.m())
	d, ok := st.decideQueue.nextDecision(st.decisionTracker.m(), &st.requirementToSortedCandidates, st.disjunctions, st.nameActivity, st.maxActivity, s.provider)
	if s.debugChecks && !st.decideQueue.sawFalsified {
		st.decideQueue.debugAssertInvariants(st.decisionTracker.m(), &st.requirementToSortedCandidates, st.disjunctions, st.nameActivity, st.maxActivity, s.provider)
	}
	return d, ok
}

// setPropagateLearn runs one iteration of the CDCL loop: install solvable at
// a new level (a decision always comes from a Requires clause) and propagate,
// learning from conflicts.
func (s *Solver) setPropagateLearn(ctx context.Context, level uint32, solvable variableID, cid clauseID) (uint32, error) {
	level++
	if _, conflict := s.state.decisionTracker.tryAddDecision(decision{variable: solvable, value: true, derivedFrom: cid}, level); conflict {
		panic(internalError("decided solvable was already decided"))
	}
	return s.propagateAndLearn(ctx, level)
}

func (s *Solver) propagateAndLearn(ctx context.Context, level uint32) (uint32, error) {
	for {
		v, cid, conflict, err := s.propagateConflict(ctx, level)
		if err != nil {
			return 0, err
		}
		if !conflict {
			return level, nil
		}
		level, err = s.learnFromConflict(level, v, cid)
		if err != nil {
			return 0, err
		}
	}
}

func (s *Solver) learnFromConflict(level uint32, conflictingVar variableID, conflictingClause clauseID) (uint32, error) {
	if level == 1 {
		return 0, s.analyzeUnsolvable(conflictingClause)
	}

	newLevel, learnedClause, lit := s.analyze(level, conflictingVar, conflictingClause)

	// The learnt clause is unit at the new level: propagate it right away.
	if _, conflict := s.state.decisionTracker.tryAddDecision(decision{variable: lit.variable(), value: lit.satisfyingValue(), derivedFrom: learnedClause}, newLevel); conflict {
		panic(internalError("asserting literal of a learnt clause was already decided"))
	}
	return newLevel, nil
}

// propagate runs unit propagation and reports the conflicting clause, if any.
func (s *Solver) propagate(ctx context.Context, level uint32) (clauseID, bool, error) {
	_, cid, conflict, err := s.propagateConflict(ctx, level)
	return cid, conflict, err
}

// propagateConflict is the propagate step of CDCL, implemented with watched
// literals: every clause with two or more literals watches two of them, and
// is visited only when one of those turns false. It then either finds
// another literal to watch or, if none is left, assigns the other watched
// literal. It returns the conflicting variable and clause on a conflict.
func (s *Solver) propagateConflict(ctx context.Context, level uint32) (variableID, clauseID, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}

	st := s.state
	dt := &st.decisionTracker

	// Assertions (single-literal clauses) have no watches.
	for _, a := range st.negativeAssertions {
		if _, conflict := dt.tryAddDecision(decision{variable: a.v, value: false, derivedFrom: a.c}, level); conflict {
			return a.v, a.c, true, nil
		}
	}
	for _, cid := range st.learntClauseIDs {
		lits := st.learntClauses[st.clauses[cid].x]
		if len(lits) > 1 {
			continue
		}
		l := lits[0]
		if _, conflict := dt.tryAddDecision(decision{variable: l.variable(), value: l.satisfyingValue(), derivedFrom: cid}, level); conflict {
			return l.variable(), cid, true, nil
		}
	}

	cl := st.clauseLiterals()
	for {
		d, ok := dt.nextUnpropagated()
		if !ok {
			return 0, 0, false, nil
		}
		// The literal that just turned false.
		watched := newLiteral(d.variable, d.value)

		cur, ok := st.watches.cursor(st.watched, watched)
		for ok {
			cid := cur.clause
			c := &st.clauses[cid]
			wl := &st.watched[cid]
			other := wl.lits[1-cur.index]

			if other.isTrue(dt.m()) {
				// The clause is already satisfied.
				ok = cur.next()
				continue
			}
			if !c.isBinary() {
				if l, found := wl.nextUnwatchedLiteral(c, cl, dt.m(), cur.index); found {
					ok = cur.update(l)
					continue
				}
			}
			// No other literal can be watched: the remaining watched
			// literal must become true.
			if _, conflict := dt.tryAddDecision(decision{variable: other.variable(), value: other.satisfyingValue(), derivedFrom: cid}, level); conflict {
				return other.variable(), cid, true, nil
			}
			ok = cur.next()
		}
	}
}

// analyzeUnsolvableClause adds a clause to the conflict. Learnt clauses mean
// nothing to users, so the clauses they were derived from are added instead.
func (st *solverState) analyzeUnsolvableClause(cid clauseID, conflict *Conflict, seen *bitSet) {
	stack := []clauseID{cid}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		cl := &st.clauses[c]
		if cl.kind != clauseLearnt {
			conflict.addClause(c)
			continue
		}
		if !seen.insert(uint32(c)) {
			continue
		}
		why := st.learntWhy[cl.x]
		for i := len(why) - 1; i >= 0; i-- {
			stack = append(stack, why[i])
		}
	}
}

// analyzeUnsolvable builds the Conflict for the clause that triggered an
// unrecoverable conflict, following the assignments it depends on.
func (s *Solver) analyzeUnsolvable(cid clauseID) *Conflict {
	st := s.state
	conflict := &Conflict{solver: s, state: st}
	cl := st.clauseLiterals()

	var involved bitSet
	cl.visit(&st.clauses[cid], func(l literal) bool {
		involved.insert(uint32(l.variable()))
		return true
	})

	var seen bitSet
	st.analyzeUnsolvableClause(cid, conflict, &seen)

	stack := st.decisionTracker.stack
	for i := len(stack) - 1; i >= 0; i-- {
		d := stack[i]
		if d.variable.isRoot() || !involved.contains(uint32(d.variable)) {
			continue
		}
		why := d.derivedFrom
		if why == installRootClause {
			continue
		}
		st.analyzeUnsolvableClause(why, conflict, &seen)
		cl.visit(&st.clauses[why], func(l literal) bool {
			if !l.isTrue(st.decisionTracker.m()) {
				involved.insert(uint32(l.variable()))
			}
			return true
		})
	}
	return conflict
}

// analyze finds the assignments that caused a conflict and learns a clause
// forbidding that combination (MiniSat's analyze). It returns the level to
// backjump to, the learnt clause and its asserting literal: every other
// literal of the learnt clause is false, so that one must become true.
func (s *Solver) analyze(currentLevel uint32, conflictingVar variableID, cid clauseID) (uint32, clauseID, literal) {
	st := s.state
	dt := &st.decisionTracker
	cl := st.clauseLiterals()

	var seen bitSet
	var causesAtCurrentLevel uint32
	var learnt []literal
	var backTrackTo uint32
	// Index of the highest-level non-asserting literal; it is moved to the
	// front so it becomes the second watch.
	highestLevelIdx := 0
	var sValue bool
	var learntWhy []clauseID
	firstIteration := true

	for {
		learntWhy = append(learntWhy, cid)

		cl.visit(&st.clauses[cid], func(l literal) bool {
			v := l.variable()
			// Only the causes of the conflict matter, not the variable
			// whose value was propagated.
			if !firstIteration && v == conflictingVar {
				return true
			}
			if !seen.insert(uint32(v)) {
				return true
			}
			decisionLevel := dt.level(v)
			switch {
			case decisionLevel == currentLevel:
				causesAtCurrentLevel++
			case currentLevel > 1:
				value, _ := dt.assignedValue(v)
				if decisionLevel > backTrackTo {
					backTrackTo = decisionLevel
					highestLevelIdx = len(learnt)
				}
				learnt = append(learnt, newLiteral(v, value))
			default:
				panic(internalError("conflict analysis reached level 1"))
			}
			return true
		})
		firstIteration = false

		// Select the next literal to look at: the most recent assignment
		// that is one of the causes.
		for {
			if len(dt.stack) == 0 {
				panic(internalError("conflict analysis exhausted the trail"))
			}
			last, lastLevel := dt.undoLast()
			conflictingVar = last.variable
			sValue = last.value
			cid = last.derivedFrom
			currentLevel = lastLevel
			if seen.contains(uint32(last.variable)) {
				break
			}
		}

		if causesAtCurrentLevel > 0 {
			causesAtCurrentLevel--
		}
		if causesAtCurrentLevel == 0 {
			break
		}
	}

	// Watch the two highest-level literals: the asserting literal (last) and
	// the highest-level of the rest (first). They are unassigned last on
	// backtracking, so the watches move least.
	if len(learnt) > 0 {
		learnt[0], learnt[highestLevelIdx] = learnt[highestLevelIdx], learnt[0]
	}
	lastLiteral := newLiteral(conflictingVar, sValue)
	learnt = append(learnt, lastLiteral)

	// Bump the activity of the packages in the learnt clause.
	for _, l := range learnt {
		sid, ok := st.variableMap.asSolvable(l.variable())
		if !ok {
			continue
		}
		name := s.provider.SolvableName(sid)
		checkID("name", uint32(name))
		st.nameActivity = growIndex(st.nameActivity, int(name))
		activity := st.nameActivity[name] + s.activityAdd
		st.nameActivity[name] = activity
		if activity > st.maxActivity {
			st.maxActivity = activity
		}
		st.decideQueue.markNameHot(name)
	}

	learntID := learntClauseID(len(st.learntClauses))
	st.learntClauses = append(st.learntClauses, learnt)
	st.learntWhy = append(st.learntWhy, learntWhy)
	c, wl := learntClause(learntID, learnt)
	newClause := st.addClause(c, wl)
	st.learntClauseIDs = append(st.learntClauseIDs, newClause)

	// Revert at most to the root level.
	target := max(backTrackTo, 1)
	dt.undoUntil(target)

	s.decayActivityScores()
	return target, newClause, lastLiteral
}

// decayActivityScores decays the activity of every package after a conflict.
func (s *Solver) decayActivityScores() {
	st := s.state
	for i := range st.nameActivity {
		st.nameActivity[i] *= s.activityDecay
	}
	// The same multiplication applied to the same value keeps maxActivity
	// bit-exact with the largest stored activity.
	st.maxActivity *= s.activityDecay
}

// assertion is a single-literal clause (¬v) that has no watches.
type assertion struct {
	v variableID
	c clauseID
}

type gateRequirer struct {
	requirer variableID
	reason   clauseID
}

type gateEntry struct {
	gate      variableID
	requirers []gateRequirer
}

type deferredEntry struct {
	condition ConditionID
	entries   []deferredRequirement
}

// solverState holds the state of one solve.
type solverState struct {
	clauses []clause
	watched []watchedLiterals
	watches watchMap

	// requirementToSortedCandidates maps a requirement to the variables of
	// its candidates, per version set.
	requirementToSortedCandidates reqMap[[][]variableID]

	// fullyQueuedRequirements are requirements whose candidates all had
	// their dependencies queued, so the encoder no longer tries.
	fullyQueuedRequirements reqMap[struct{}]

	variableMap variableMap

	negativeAssertions []assertion

	learntClauses   [][]literal
	learntWhy       [][]clauseID
	learntClauseIDs []clauseID

	disjunctions []disjunction

	clausesAddedForPackage  bitSet
	clausesAddedForSolvable rootSet
	allowMultipleNames      bitSet
	atMostOneTrackers       map[NameID]*atMostOnceTracker[variableID]

	// conditionVars is the holds variable of each condition version set
	// (see encoder.conditionHolds).
	conditionVars map[VersionSetID]variableID

	// conditionInfos caches the resolved DNF of every condition the
	// encoder has seen, shared by all requirements carrying it.
	conditionInfos map[ConditionID]*conditionInfo

	// constrainsAuxVars is the auxiliary variable of the shared constrains
	// encoding per version set.
	constrainsAuxVars map[VersionSetID]variableID

	// requiresAuxVars is the gate variable of the shared requires encoding
	// per requirement.
	requiresAuxVars map[Requirement]variableID

	// gates lists, per shared-requires gate in creation order, the
	// requirers that imply it and their implication clauses. The encoder
	// forces a gate true at its own (lazy, often higher) level, so a
	// backtrack can strand it: the implication is unit but watches never
	// re-fire on unassignment. forceStuckGates re-derives such gates.
	gates     []gateEntry
	gateIndex map[variableID]int

	// unitAtCreation lists, in creation order, the clauses that were unit
	// when the encoder added them: one literal was already false and the
	// encoder assigned (or found assigned) the implied literal directly
	// instead of through propagation. See forceStrandedUnits.
	unitAtCreation []clauseID

	decisionTracker decisionTracker

	nameActivity []float32
	// maxActivity is the largest value in nameActivity, maintained
	// bit-exactly next to the bumps and decays.
	maxActivity float32

	decideQueue *decideQueue

	// deferred holds, per condition in insertion order, the conditional
	// requirements whose condition did not hold when the encoder reached
	// them. A disjunct stays pending only until its requires clause is
	// encoded, an entry only while it has pending disjuncts.
	deferred      []deferredEntry
	deferredIndex map[ConditionID]int
	// memo is scratch space for checking deferred conditions.
	memo disjunctMemo

	// deferredByCandidate lists, per matching candidate of a deferred
	// condition, the conditions it can make hold, so firedDeferredConditions
	// only checks the conditions that assignments since its previous call
	// can have completed. Entries are not removed when a condition is
	// drained; stale ones are skipped.
	deferredByCandidate map[SolvableID][]ConditionID
}

func newSolverState() *solverState {
	return &solverState{
		variableMap:         newVariableMap(),
		atMostOneTrackers:   map[NameID]*atMostOnceTracker[variableID]{},
		constrainsAuxVars:   map[VersionSetID]variableID{},
		conditionVars:       map[VersionSetID]variableID{},
		conditionInfos:      map[ConditionID]*conditionInfo{},
		requiresAuxVars:     map[Requirement]variableID{},
		gateIndex:           map[variableID]int{},
		decideQueue:         newDecideQueue(),
		deferredIndex:       map[ConditionID]int{},
		deferredByCandidate: map[SolvableID][]ConditionID{},
	}
}

func (st *solverState) clauseLiterals() *clauseLiterals {
	return &clauseLiterals{learnt: st.learntClauses, sorted: &st.requirementToSortedCandidates, disjunctions: st.disjunctions}
}

// addClause allocates a clause and registers its watches.
func (st *solverState) addClause(c clause, wl watchedLiterals) clauseID {
	id := clauseID(len(st.clauses))
	st.clauses = append(st.clauses, c)
	st.watched = append(st.watched, wl)
	if wl.ok {
		st.watches.startWatching(&st.watched[id], id)
	}
	return id
}

// addRequiresClause registers a newly encoded requires clause with the
// decide queue.
func (st *solverState) addRequiresClause(parent variableID, req Requirement, cond *disjunctionID, cid clauseID, names []NameID) {
	value, assigned := st.decisionTracker.assignedValue(parent)
	st.decideQueue.registerClause(parent, req, cond, cid, names, st.disjunctions, value, assigned)
}

func (st *solverState) addGateRequirer(gate, requirer variableID, reason clauseID) {
	idx, ok := st.gateIndex[gate]
	if !ok {
		idx = len(st.gates)
		st.gateIndex[gate] = idx
		st.gates = append(st.gates, gateEntry{gate: gate})
	}
	st.gates[idx].requirers = append(st.gates[idx].requirers, gateRequirer{requirer: requirer, reason: reason})
}

// recordUnitAtCreation records a clause that was unit when it was added.
func (st *solverState) recordUnitAtCreation(c clauseID) {
	st.unitAtCreation = append(st.unitAtCreation, c)
}

// chosenSolvables returns the solvables assigned true, in trail order.
func (st *solverState) chosenSolvables() []SolvableID {
	var out []SolvableID
	for _, d := range st.decisionTracker.stack {
		if !d.value {
			continue
		}
		if s, ok := st.variableMap.asSolvable(d.variable); ok {
			out = append(out, s)
		}
	}
	return out
}

// isPositivelyDecided reports whether a solvable is assigned true. A solvable
// without a variable is undecided.
func (st *solverState) isPositivelyDecided(s SolvableID) bool {
	v, ok := st.variableMap.lookupSolvable(s)
	return ok && st.decisionTracker.isAssigned(v, true)
}

// forceStuckGates forces true every unassigned shared-requires gate that has
// an installed requirer, with the requirer's implication as the reason. It
// reports whether any gate was forced.
func (st *solverState) forceStuckGates(level uint32) bool {
	dt := &st.decisionTracker
	forced := false
	for _, g := range st.gates {
		if _, ok := dt.assignedValue(g.gate); ok {
			continue
		}
		for _, r := range g.requirers {
			if dt.isAssigned(r.requirer, true) {
				dt.tryAddDecision(decision{variable: g.gate, value: true, derivedFrom: r.reason}, level)
				forced = true
				break
			}
		}
	}
	return forced
}

// forceStrandedUnits re-derives implications stranded by a backtrack and
// reports whether it assigned anything.
//
// When the encoder adds a clause one of whose literals is already false, it
// assigns the implied literal directly at the current level instead of
// through propagation. A later backjump below that level, but not below the
// level of the false literal, unassigns the implied literal while the clause
// stays unit; watched literals never re-fire on an unassignment, so nothing
// re-derives it. If the implied variable is auxiliary (a condition-holds
// variable, an at-most-one helper, a constraint violation variable) it may
// stay unassigned to the end, and the solver then accepts an assignment
// whose completion violates the clause: a conditional requirement whose
// condition holds is silently dropped. resolvo repairs this for
// shared-requires gates only (forceStuckGates); this repairs every clause
// that was unit at creation. It assigns the implied literal of each such
// clause that is unit again, with the clause as the reason.
//
// runSat calls it only when it is about to accept a solution: a stranded
// clause matters only if it is still unit then, and repairing it any
// earlier would make the search path diverge from resolvo's on problems
// where resolvo is right.
func (st *solverState) forceStrandedUnits(level uint32) bool {
	cl := st.clauseLiterals()
	m := st.decisionTracker.m()
	forced := false
	for _, cid := range st.unitAtCreation {
		var implied literal
		unassigned, satisfied := 0, false
		cl.visit(&st.clauses[cid], func(l literal) bool {
			v, ok := l.eval(m)
			switch {
			case !ok:
				if unassigned == 0 || l != implied {
					unassigned++
					implied = l
				}
			case v:
				satisfied = true
			}
			return !satisfied && unassigned < 2
		})
		if satisfied || unassigned != 1 {
			continue
		}
		st.decisionTracker.tryAddDecision(decision{variable: implied.variable(), value: implied.satisfyingValue(), derivedFrom: cid}, level)
		forced = true
	}
	return forced
}

// firedDeferredConditions returns, in insertion order, the conditions of
// deferred requirements of which at least one disjunct holds.
//
// A disjunct that did not hold at the previous call (or when it was deferred)
// can only hold now if one of its matching candidates was installed since,
// so only the conditions indexed under the solvables installed since the
// previous call are checked (resolvo rescans every deferred entry).
func (st *solverState) firedDeferredConditions() []ConditionID {
	floor := st.decisionTracker.takeDeferredFloor()
	if len(st.deferred) == 0 {
		return nil
	}
	var candidates []ConditionID
	seen := map[ConditionID]bool{}
	for _, d := range st.decisionTracker.stack[floor:] {
		if !d.value {
			continue
		}
		s, ok := st.variableMap.asSolvable(d.variable)
		if !ok {
			continue
		}
		for _, c := range st.deferredByCandidate[s] {
			if !seen[c] {
				seen[c] = true
				candidates = append(candidates, c)
			}
		}
	}
	var out []ConditionID
	for _, c := range candidates {
		idx, ok := st.deferredIndex[c]
		if !ok {
			continue
		}
		st.memo.reset(st.conditionInfos[c])
	entries:
		for _, d := range st.deferred[idx].entries {
			for _, i := range d.pending {
				if st.memo.holds(i, st.isPositivelyDecided) {
					out = append(out, c)
					break entries
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return st.deferredIndex[out[i]] < st.deferredIndex[out[j]] })
	return out
}

// drainFiredDeferred removes and returns, in insertion order, the pending
// disjuncts of a condition's deferred requirements that hold. Remaining
// disjuncts and entries keep their relative order; a condition without
// remaining entries is removed by swapping the last condition into its
// place.
func (st *solverState) drainFiredDeferred(cond ConditionID) []firedDisjunct {
	idx, ok := st.deferredIndex[cond]
	if !ok {
		return nil
	}
	st.memo.reset(st.conditionInfos[cond])
	var fired []firedDisjunct
	kept := st.deferred[idx].entries[:0]
	for _, d := range st.deferred[idx].entries {
		rest := d.pending[:0]
		for _, i := range d.pending {
			if st.memo.holds(i, st.isPositivelyDecided) {
				fired = append(fired, firedDisjunct{solvable: d.solvable, requirement: d.requirement, condition: d.condition, disjunct: i})
			} else {
				rest = append(rest, i)
			}
		}
		if len(rest) > 0 {
			d.pending = rest
			kept = append(kept, d)
		}
	}
	clear(st.deferred[idx].entries[len(kept):])
	st.deferred[idx].entries = kept
	if len(kept) == 0 {
		last := len(st.deferred) - 1
		if idx != last {
			st.deferred[idx] = st.deferred[last]
			st.deferredIndex[st.deferred[idx].condition] = idx
		}
		st.deferred = st.deferred[:last]
		delete(st.deferredIndex, cond)
	}
	return fired
}

// pushDeferred registers a deferred requirement of the condition info
// describes, indexing the condition's candidates on first use.
func (st *solverState) pushDeferred(d deferredRequirement, info *conditionInfo) {
	idx, ok := st.deferredIndex[d.condition]
	if !ok {
		idx = len(st.deferred)
		st.deferredIndex[d.condition] = idx
		st.deferred = append(st.deferred, deferredEntry{condition: d.condition})
	}
	st.deferred[idx].entries = append(st.deferred[idx].entries, d)
	if info.indexed {
		return
	}
	info.indexed = true
	for _, dj := range info.disjuncts {
		for _, c := range dj.conjuncts {
			for _, s := range c.matching {
				list := st.deferredByCandidate[s]
				if n := len(list); n == 0 || list[n-1] != d.condition {
					st.deferredByCandidate[s] = append(list, d.condition)
				}
			}
		}
	}
}
