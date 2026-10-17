// Ported from resolvo src/solver/variable_map.rs (BSD-3-Clause, see NOTICE).

package cdcl

// originKind describes what a variable represents.
type originKind uint8

const (
	// originRoot is the root of the decision tree.
	originRoot originKind = iota
	// originSolvable represents a specific solvable.
	originSolvable
	// originForbidMultiple helps encode an at-most-one constraint.
	originForbidMultiple
	// originConditionHolds is true exactly when a candidate matching a
	// condition's version set is installed.
	originConditionHolds
	// originConstrainsViolation is true when a candidate excluded by a
	// constraint's version set is installed.
	originConstrainsViolation
	// originRequiresGate is the output of the OR-gate over a requirement's
	// candidate disjunction (Tseitin, one-sided Plaisted-Greenbaum form):
	// requirers imply the gate and the gate implies the disjunction, so the
	// often large disjunction is encoded once and shared by every requirer.
	originRequiresGate
)

// variableOrigin records the origin of a variable.
type variableOrigin struct {
	kind originKind
	// id is the solvable, name or version set id, depending on kind.
	id  uint32
	req Requirement
}

// variableMap allocates variables and records what each one represents.
type variableMap struct {
	// solvableToVariable maps a solvable to its variable; zero means none
	// (zero is the root variable, which never represents a solvable).
	solvableToVariable []variableID
	// origins is indexed by variable; every allocated variable has exactly
	// one entry.
	origins []variableOrigin
}

func newVariableMap() variableMap {
	return variableMap{origins: []variableOrigin{{kind: originRoot}}}
}

func (vm *variableMap) alloc(o variableOrigin) variableID {
	id := variableID(len(vm.origins))
	vm.origins = append(vm.origins, o)
	return id
}

// internSolvable returns the variable of a solvable, allocating one if
// needed.
func (vm *variableMap) internSolvable(s SolvableID) variableID {
	checkID("solvable", uint32(s))
	if int(s) < len(vm.solvableToVariable) {
		if v := vm.solvableToVariable[s]; v != 0 {
			return v
		}
	}
	v := vm.alloc(variableOrigin{kind: originSolvable, id: uint32(s)})
	if int(s) >= len(vm.solvableToVariable) {
		n := max(int(s)+1, 2*len(vm.solvableToVariable))
		ns := make([]variableID, n)
		copy(ns, vm.solvableToVariable)
		vm.solvableToVariable = ns
	}
	vm.solvableToVariable[s] = v
	return v
}

// lookupSolvable returns the variable of a solvable without allocating one.
func (vm *variableMap) lookupSolvable(s SolvableID) (variableID, bool) {
	if int(s) < len(vm.solvableToVariable) {
		if v := vm.solvableToVariable[s]; v != 0 {
			return v, true
		}
	}
	return 0, false
}

// internSolvableOrRoot returns the root variable for the root and the
// solvable's variable otherwise.
func (vm *variableMap) internSolvableOrRoot(s solvableOrRoot) variableID {
	if s.root {
		return rootVariable
	}
	return vm.internSolvable(s.id)
}

func (vm *variableMap) allocForbidMultiple(name NameID) variableID {
	return vm.alloc(variableOrigin{kind: originForbidMultiple, id: uint32(name)})
}

func (vm *variableMap) allocConditionHolds(vs VersionSetID) variableID {
	return vm.alloc(variableOrigin{kind: originConditionHolds, id: uint32(vs)})
}

func (vm *variableMap) allocConstrainsViolation(vs VersionSetID) variableID {
	return vm.alloc(variableOrigin{kind: originConstrainsViolation, id: uint32(vs)})
}

func (vm *variableMap) allocRequiresGate(r Requirement) variableID {
	return vm.alloc(variableOrigin{kind: originRequiresGate, req: r})
}

func (vm *variableMap) origin(v variableID) variableOrigin { return vm.origins[v] }

// asSolvable returns the solvable a variable represents.
func (vm *variableMap) asSolvable(v variableID) (SolvableID, bool) {
	o := vm.origins[v]
	if o.kind == originSolvable {
		return SolvableID(o.id), true
	}
	return 0, false
}

// asSolvableOrRoot returns the solvable or root a variable represents.
func (vm *variableMap) asSolvableOrRoot(v variableID) (solvableOrRoot, bool) {
	o := vm.origins[v]
	switch o.kind {
	case originSolvable:
		return solvableOrRoot{id: SolvableID(o.id)}, true
	case originRoot:
		return solvableOrRoot{root: true}, true
	}
	return solvableOrRoot{}, false
}

// solvableOrRoot refers to the solver root or to a provider solvable.
type solvableOrRoot struct {
	id   SolvableID
	root bool
}

func (s solvableOrRoot) display(in Interner) string {
	if s.root {
		return "root"
	}
	return in.DisplaySolvable(s.id)
}

// rootSet is a set of solvableOrRoot.
type rootSet struct {
	root      bool
	solvables bitSet
}

func (s *rootSet) insert(k solvableOrRoot) bool {
	if k.root {
		was := s.root
		s.root = true
		return !was
	}
	checkID("solvable", uint32(k.id))
	return s.solvables.insert(uint32(k.id))
}

func (s *rootSet) contains(k solvableOrRoot) bool {
	if k.root {
		return s.root
	}
	return s.solvables.contains(uint32(k.id))
}
