// Ported from resolvo src/utils/indexed_set.rs (BSD-3-Clause, see NOTICE).

package cdcl

import "math/bits"

const wordBits = 64

// bitSet is a dense set of small integers backed by machine words, so
// test-and-set is O(1) without hashing. It grows on demand to fit the
// largest inserted index.
type bitSet struct {
	words []uint64
}

// insert adds idx and reports whether it was newly inserted.
func (s *bitSet) insert(idx uint32) bool {
	w, b := int(idx/wordBits), uint64(1)<<(idx%wordBits)
	if w >= len(s.words) {
		n := max(w+1, 2*len(s.words))
		nw := make([]uint64, n)
		copy(nw, s.words)
		s.words = nw
	}
	was := s.words[w]&b != 0
	s.words[w] |= b
	return !was
}

// contains reports whether idx is in the set.
func (s *bitSet) contains(idx uint32) bool {
	w := int(idx / wordBits)
	return w < len(s.words) && s.words[w]&(uint64(1)<<(idx%wordBits)) != 0
}

// remove deletes idx from the set.
func (s *bitSet) remove(idx uint32) {
	w := int(idx / wordBits)
	if w < len(s.words) {
		s.words[w] &^= uint64(1) << (idx % wordBits)
	}
}

// nextFrom returns the smallest member >= idx.
func (s *bitSet) nextFrom(idx uint32) (uint32, bool) {
	w := int(idx / wordBits)
	if w >= len(s.words) {
		return 0, false
	}
	word := s.words[w] &^ (uint64(1)<<(idx%wordBits) - 1)
	for {
		if word != 0 {
			return uint32(w*wordBits + bits.TrailingZeros64(word)), true
		}
		w++
		if w >= len(s.words) {
			return 0, false
		}
		word = s.words[w]
	}
}
