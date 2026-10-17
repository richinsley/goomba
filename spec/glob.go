// Ported from libmamba src/specs/glob_spec.cpp and src/util/parsers.cpp
// (BSD-3-Clause, see NOTICE).

package spec

import "strings"

// GlobSpec matches strings against a pattern where '*' stands for any run of
// characters (including none), e.g. "py*", "*_cp311" or the exact "numpy".
// No other character is special. The zero GlobSpec (like the pattern "" or
// "*") matches everything.
//
// A GlobSpec is immutable and safe for concurrent use.
type GlobSpec struct {
	pattern string // "" means "*"
}

// NewGlobSpec returns a GlobSpec for pattern. It never fails.
func NewGlobSpec(pattern string) GlobSpec {
	if pattern == "*" {
		pattern = ""
	}
	return GlobSpec{pattern: pattern}
}

// maxCandidateLen bounds the strings that name and build matchers accept:
// longer ones never match, whatever the pattern, so matching costs at most
// pattern length times 256. Real names and builds are under 100 bytes.
const maxCandidateLen = 256

// Contains reports whether s matches the pattern. Strings longer than 256
// bytes never match, even "*". Contains does not allocate.
func (g GlobSpec) Contains(s string) bool {
	if len(s) > maxCandidateLen {
		return false
	}
	return g.pattern == "" || globMatch(g.pattern, s)
}

// IsAny reports whether the pattern is "*", matching everything.
func (g GlobSpec) IsAny() bool { return g.pattern == "" }

// IsExact reports whether the pattern has no '*' and so matches exactly one
// string.
func (g GlobSpec) IsExact() bool {
	return g.pattern != "" && strings.IndexByte(g.pattern, '*') < 0
}

// String returns the pattern; "*" for the zero GlobSpec.
func (g GlobSpec) String() string {
	if g.pattern == "" {
		return "*"
	}
	return g.pattern
}

// globMatch reports whether s matches pattern, where '*' matches any run of
// bytes. The text before the first '*' must be a prefix of s and the text
// after the last a suffix; every literal in between is placed at its
// leftmost occurrence. Greedy placement is correct because '*' is the only
// wildcard, and it never backtracks, so the cost is linear in
// len(pattern)+len(s) (strings.Index). It does not allocate.
//
// libmamba's glob_match anchors each literal run at its first occurrence, so
// it rejects "aa" for "*a"; this implementation matches it, like conda.
func globMatch(pattern, s string) bool {
	first := strings.IndexByte(pattern, '*')
	if first < 0 {
		return pattern == s
	}
	last := strings.LastIndexByte(pattern, '*')
	prefix, suffix := pattern[:first], pattern[last+1:]
	if len(s) < len(prefix)+len(suffix) || !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return false
	}
	s = s[len(prefix) : len(s)-len(suffix)]
	mid := ""
	if last > first {
		mid = pattern[first+1 : last]
	}
	for mid != "" {
		seg := mid
		if i := strings.IndexByte(mid, '*'); i >= 0 {
			seg, mid = mid[:i], mid[i+1:]
		} else {
			mid = ""
		}
		if seg == "" {
			continue
		}
		j := strings.Index(s, seg)
		if j < 0 {
			return false
		}
		s = s[j+len(seg):]
	}
	return true
}
