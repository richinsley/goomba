// Ported from resolvo src/solver/watch_map.rs (BSD-3-Clause, see NOTICE).

package cdcl

// watchMap maps each literal to the head of the linked list of clauses that
// watch it. The links live in the clauses' watchedLiterals.
type watchMap struct {
	heads []clauseID
}

func (w *watchMap) head(l literal) clauseID {
	if int(l) < len(w.heads) {
		return w.heads[l]
	}
	return noClause
}

func (w *watchMap) setHead(l literal, c clauseID) {
	if int(l) >= len(w.heads) {
		n := max(int(l)+1, 2*len(w.heads))
		nh := make([]clauseID, n)
		copy(nh, w.heads)
		for i := len(w.heads); i < n; i++ {
			nh[i] = noClause
		}
		w.heads = nh
	}
	w.heads[l] = c
}

// startWatching prepends clause c to the lists of both literals it watches.
func (w *watchMap) startWatching(wl *watchedLiterals, c clauseID) {
	for i, l := range wl.lits {
		wl.next[i] = w.head(l)
		w.setHead(l, c)
	}
}

// watchCursor navigates and edits the linked list of clauses watching one
// literal.
type watchCursor struct {
	wm      *watchMap
	watches []watchedLiterals
	lit     literal

	hasPrev    bool
	prevClause clauseID
	prevIndex  int

	clause clauseID
	index  int
}

// cursor returns a cursor at the head of the list for l.
func (w *watchMap) cursor(watches []watchedLiterals, l literal) (watchCursor, bool) {
	c := w.head(l)
	if c == noClause {
		return watchCursor{}, false
	}
	return watchCursor{wm: w, watches: watches, lit: l, clause: c, index: watchIndexOf(&watches[c], l)}, true
}

func watchIndexOf(wl *watchedLiterals, l literal) int {
	if wl.lits[0] == l {
		return 0
	}
	return 1
}

// nextNode returns the node after the current one.
func (cur *watchCursor) nextNode() (clauseID, int, bool) {
	n := cur.watches[cur.clause].next[cur.index]
	if n == noClause {
		return 0, 0, false
	}
	return n, watchIndexOf(&cur.watches[n], cur.lit), true
}

// next advances to the next node, returning false at the end of the list.
func (cur *watchCursor) next() bool {
	n, idx, ok := cur.nextNode()
	if !ok {
		return false
	}
	cur.hasPrev, cur.prevClause, cur.prevIndex = true, cur.clause, cur.index
	cur.clause, cur.index = n, idx
	return true
}

// update moves the current clause's watch to newWatch, unlinking it from
// this list, and advances to the next node. It returns false at the end of
// the list.
func (cur *watchCursor) update(newWatch literal) bool {
	n, idx, hasNext := cur.nextNode()

	// Unlink the current node.
	switch {
	case cur.hasPrev:
		if hasNext {
			cur.watches[cur.prevClause].next[cur.prevIndex] = n
		} else {
			cur.watches[cur.prevClause].next[cur.prevIndex] = noClause
		}
	case hasNext:
		cur.wm.setHead(cur.lit, n)
	default:
		cur.wm.setHead(cur.lit, noClause)
	}

	// Watch the new literal.
	wl := &cur.watches[cur.clause]
	wl.lits[cur.index] = newWatch
	wl.next[cur.index] = cur.wm.head(newWatch)
	cur.wm.setHead(newWatch, cur.clause)

	if !hasNext {
		return false
	}
	cur.clause, cur.index = n, idx
	return true
}
