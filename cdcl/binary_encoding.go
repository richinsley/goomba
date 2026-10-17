// Ported from resolvo src/solver/binary_encoding.rs (BSD-3-Clause, see NOTICE).

package cdcl

// atMostOnceTracker incrementally builds the clauses of a binary
// ("bitwise") at-most-one encoding over a growing set of variables. Each
// variable gets a distinct index whose bits are tied to helper variables, so
// two variables can never both be true with O(n log n) binary clauses.
type atMostOnceTracker[V comparable] struct {
	// variables are the variables of which at most one may be true, in
	// insertion order.
	variables []V
	index     map[V]int
	helpers   []V
}

func newAtMostOnceTracker[V comparable]() *atMostOnceTracker[V] {
	return &atMostOnceTracker[V]{index: map[V]int{}}
}

// add adds variable to the set. allocClause is called to add the clause
// (¬a ∨ b) or (¬a ∨ ¬b), positive selecting the polarity of b; allocVar
// allocates a new helper variable. It reports whether the variable was new.
func (t *atMostOnceTracker[V]) add(variable V, allocClause func(a, b V, positive bool), allocVar func() V) bool {
	if _, ok := t.index[variable]; ok {
		return false
	}

	// A single variable can never conflict with itself.
	if len(t.variables) == 0 {
		t.index[variable] = 0
		t.variables = append(t.variables, variable)
		return true
	}

	// Allocate helper variables as needed and add their clauses for the
	// existing variables.
	for len(t.variables) > (1<<len(t.helpers))-1 {
		helper := allocVar()
		bit := len(t.helpers)
		t.helpers = append(t.helpers, helper)
		mask := 1 << bit
		for idx, v := range t.variables {
			allocClause(v, helper, idx&mask == mask)
		}
	}

	varIdx := len(t.variables)
	t.index[variable] = varIdx
	t.variables = append(t.variables, variable)
	for bit, helper := range t.helpers {
		allocClause(variable, helper, (varIdx>>bit)&1 == 1)
	}
	return true
}
