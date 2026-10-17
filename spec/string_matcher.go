// Ported from libmamba src/specs/regex_spec.cpp and
// src/specs/chimera_string_spec.cpp (BSD-3-Clause, see NOTICE).

package spec

import (
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
)

// StringMatcher matches a build string. A pattern wrapped in '^' and '$',
// such as "^.*(accelerate|mkl)$", is a regular expression matched against
// the whole string; any other pattern is a glob where only '*' is special
// ("py311*", "*_cpython", an exact "h12345_0", or "py3.10_cuda11.8*"
// whose dots match only dots). This follows conda and libsolv; libmamba
// also treats patterns that merely start with '^', end with '$' or contain
// characters such as '.' or '+' as regular expressions, so that its
// "abc+def" never matches the build "abc+def".
//
// Regular expressions use Go's RE2 syntax, which runs in linear time.
// Globs are limited to 256 bytes of letters, digits and "_.+-*". Regular
// expressions are limited to 64 bytes of printable ASCII without quotes,
// with balanced brackets and parentheses, may not use counted repetition
// ("{n}") and must compile to a small program. Build strings longer than 256
// bytes never match.
//
// The zero StringMatcher matches everything. A StringMatcher is immutable
// and safe for concurrent use.
type StringMatcher struct {
	glob GlobSpec
	re   *regexp.Regexp // non-nil for a regex matcher
	raw  string         // regex source, always "^...$"
}

// Limits on build string patterns. The longest build string in conda-forge
// has well under 100 bytes and no pattern uses counted repetition. A
// regular expression's matching cost grows with its length times the
// build's, so regular expressions get a tighter budget than globs; the
// instruction cap is a second line of defense.
const (
	maxPatternLen     = 256
	maxRegexLen       = 64
	maxRegexProgInsts = 1024
)

// ParseStringMatcher parses a build string pattern; see StringMatcher.
func ParseStringMatcher(pattern string) (StringMatcher, error) {
	if len(pattern) > maxPatternLen {
		return StringMatcher{}, parseErr(ErrStringMatcher, truncate(pattern), "longer than "+strconv.Itoa(maxPatternLen)+" bytes")
	}
	var square, round int
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\'' {
			return StringMatcher{}, parseErr(ErrStringMatcher, pattern, "invalid character "+strconv.Quote(pattern[i:i+1]))
		}
		switch c {
		case '[':
			square++
		case ']':
			square--
		case '(':
			round++
		case ')':
			round--
		}
		if square < 0 || round < 0 {
			return StringMatcher{}, parseErr(ErrStringMatcher, pattern, "unbalanced brackets")
		}
	}
	if square != 0 || round != 0 {
		return StringMatcher{}, parseErr(ErrStringMatcher, pattern, "unbalanced brackets")
	}
	if isRegexPattern(pattern) {
		return makeRegexMatcher(pattern)
	}
	// Real build strings use letters, digits, '_' and '.'; other characters
	// (':', spaces, brackets) could also not be written back unambiguously.
	for i := 0; i < len(pattern); i++ {
		if c := pattern[i]; !isAlnum(c) && strings.IndexByte("_.+-*", c) < 0 {
			return StringMatcher{}, parseErr(ErrStringMatcher, pattern, "invalid character "+strconv.Quote(pattern[i:i+1])+" in build string")
		}
	}
	return StringMatcher{glob: NewGlobSpec(pattern)}, nil
}

// isRegexPattern reports whether p is written as a regular expression,
// wrapped in '^' and '$'.
func isRegexPattern(p string) bool {
	return len(p) >= 2 && p[0] == '^' && p[len(p)-1] == '$'
}

// regexify turns a pattern into an anchored regex: a pattern already
// wrapped in '^' and '$' is kept; otherwise each '*' not preceded by '.'
// becomes ".*" and the result is wrapped in '^' and '$'.
func regexify(raw string) string {
	if strings.HasPrefix(raw, "^") && strings.HasSuffix(raw, "$") && len(raw) >= 2 {
		return raw
	}
	var b strings.Builder
	b.Grow(len(raw) + 4)
	b.WriteByte('^')
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if i == 0 && c == '^' {
			continue
		}
		if i == len(raw)-1 && c == '$' {
			continue
		}
		if c == '*' && (i == 0 || raw[i-1] != '.') {
			b.WriteString(".*")
		} else {
			b.WriteByte(c)
		}
	}
	b.WriteByte('$')
	return b.String()
}

// makeRegexMatcher is libmamba's RegexSpec: it regexifies the pattern (see
// regexify), then keeps the lighter glob form when the expression is free
// or exact.
func makeRegexMatcher(pattern string) (StringMatcher, error) {
	raw := regexify(pattern)
	inner := raw[1 : len(raw)-1]
	if inner == ".*" {
		return StringMatcher{}, nil
	}
	if isExactRegexBody(inner) {
		// A regex without special characters is an exact string. libmamba
		// keeps the anchors in the resulting glob, so "^mkl$" never
		// matches "mkl"; this port matches it.
		return StringMatcher{glob: NewGlobSpec(inner)}, nil
	}
	if len(raw) > maxRegexLen {
		return StringMatcher{}, parseErr(ErrStringMatcher, truncate(pattern), "regular expression longer than "+strconv.Itoa(maxRegexLen)+" bytes")
	}
	if strings.IndexByte(raw, '{') >= 0 {
		// Counted repetition makes small patterns compile to huge
		// programs ("^(a{1000}){1000}$").
		return StringMatcher{}, parseErr(ErrStringMatcher, pattern, "counted repetition is not supported")
	}
	// Anchor the whole expression: libmamba uses std::regex_match, which
	// must match the entire string even across alternations.
	full := "^(?:" + raw + ")$"
	re, err := syntax.Parse(full, syntax.Perl)
	if err != nil {
		return StringMatcher{}, wrapErr(ErrStringMatcher, pattern, "", err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return StringMatcher{}, wrapErr(ErrStringMatcher, pattern, "", err)
	}
	if len(prog.Inst) > maxRegexProgInsts {
		return StringMatcher{}, parseErr(ErrStringMatcher, pattern, "regular expression too complex")
	}
	compiled, err := regexp.Compile(full)
	if err != nil {
		return StringMatcher{}, wrapErr(ErrStringMatcher, pattern, "", err)
	}
	return StringMatcher{re: compiled, raw: raw}, nil
}

func isExactRegexBody(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// Contains reports whether s matches. Strings longer than 256 bytes never
// match, whatever the pattern.
func (m StringMatcher) Contains(s string) bool {
	if len(s) > maxCandidateLen {
		return false
	}
	if m.re != nil {
		return m.re.MatchString(s)
	}
	return m.glob.Contains(s)
}

// IsAny reports whether the matcher matches everything ("*", "", "^.*$").
func (m StringMatcher) IsAny() bool { return m.re == nil && m.glob.IsAny() }

// IsExact reports whether the matcher matches exactly one string.
func (m StringMatcher) IsExact() bool { return m.re == nil && m.glob.IsExact() }

// IsGlob reports whether the matcher is a glob (including exact strings)
// rather than a regular expression.
func (m StringMatcher) IsGlob() bool { return m.re == nil }

// String returns the glob pattern, or the anchored regex "^...$".
func (m StringMatcher) String() string {
	if m.re != nil {
		return m.raw
	}
	return m.glob.String()
}

// Equal reports whether both matchers have the same pattern.
func (m StringMatcher) Equal(o StringMatcher) bool {
	return (m.re == nil) == (o.re == nil) && m.String() == o.String()
}
