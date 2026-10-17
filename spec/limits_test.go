package spec

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

// within fails the test if f, run n times, takes longer than limit. The
// limits are generous (they must hold under -race on a slow machine) but
// far below what quadratic matching took: seconds per call.
func within(t *testing.T, what string, n int, limit time.Duration, f func()) {
	t.Helper()
	start := time.Now()
	for i := 0; i < n; i++ {
		f()
	}
	if d := time.Since(start); d > limit {
		t.Errorf("%s: %d runs took %v, limit %v", what, n, d, limit)
	}
}

// TestGlobWorstCases replays the WP1 reviews' worst cases for the three
// glob matchers (one '*', then a long literal that almost matches), which
// took 0.4-11 s per call when matching backtracked. Names and builds over
// 256 bytes are refused outright; up to that, matching is linear.
func TestGlobWorstCases(t *testing.T) {
	v := mustVersion(t, "1")
	for _, pat := range []string{
		"*" + strings.Repeat("a", 254) + "b",
		"*" + strings.Repeat("a", 62) + "b*",
		"*" + strings.Repeat("a", 200) + "b*",
		strings.Repeat("*a", 127) + "*",
		strings.Repeat("*a", 126) + "*b*",
		strings.Repeat("a", 120) + "*" + strings.Repeat("a", 120),
	} {
		g := NewGlobSpec(pat)
		m := mustMatchSpec(t, pat+" >=1")
		b := mustStringMatcher(t, pat)
		for _, n := range []int{255, 256, 257, 10000, 1 << 20} {
			s := strings.Repeat("a", n)
			r := &record.PackageRecord{Name: s, Version: "1", Build: s}
			within(t, "glob "+truncate(pat), 200, 2*time.Second, func() {
				g.Contains(s)
				b.Contains(s)
				m.MatchesVersion(r, v)
			})
			if n > maxCandidateLen && (g.Contains(s) || b.Contains(s) || m.MatchesVersion(r, v)) {
				t.Errorf("%d-byte name or build matched", n)
			}
		}
	}
	// Up to 256 bytes, matching still works.
	full := strings.Repeat("a", 255) + "b"
	if !NewGlobSpec("*"+strings.Repeat("a", 200)+"b").Contains(full) || !NewGlobSpec("*").Contains(full) {
		t.Error("256-byte name should match")
	}
	if NewGlobSpec("*").Contains(full+"c") || (StringMatcher{}).Contains(full+"c") {
		t.Error("257-byte name or build matched *")
	}

	// Version globs: versions are capped at 64 parts.
	cand := mustVersion(t, strings.Repeat("1.", 63)+"1")
	for _, pat := range []string{
		"*" + strings.Repeat(".1", 62) + ".2",
		"*." + strings.Repeat("1.", 30) + "2.*",
		strings.Repeat("*.1.", 31) + "*.2",
		"*." + strings.Repeat("1.", 61) + "2",
		strings.Repeat("*.", 63) + "*",
	} {
		vs := mustVersionSpec(t, pat)
		within(t, "version glob "+truncate(pat), 200, 2*time.Second, func() { vs.Contains(cand) })
	}
	if !mustVersionSpec(t, "*"+strings.Repeat(".1", 62)+".2").Contains(mustVersion(t, strings.Repeat("1.", 63)+"2")) {
		t.Error("should match a version ending in 1.2")
	}
	// A whole spec of glob predicates is capped at 1024 bytes and 16 globs.
	pred := "*." + strings.Repeat("1.", 20) + "2.*"
	vs := mustVersionSpec(t, strings.TrimSuffix(strings.Repeat(pred+"|", maxGlobPredicates), "|"))
	within(t, "16 glob predicates", 200, 2*time.Second, func() { vs.Contains(cand) })
	if _, err := ParseVersionSpec(strings.Repeat("*.2|", maxGlobPredicates) + "*.2"); !errors.Is(err, ErrVersionSpec) {
		t.Errorf("17 glob predicates: err = %v", err)
	}
	chain := mustVersionSpec(t, strings.TrimSuffix(strings.Repeat("!=1.2,", 170), ","))
	within(t, "1020-byte != chain", 200, 2*time.Second, func() { chain.Contains(cand) })
}

// TestRegexBuildCost replays the WP1 re-review's regex costs: matching costs
// pattern length times build length, so patterns are limited to 64 bytes
// and builds to 256.
func TestRegexBuildCost(t *testing.T) {
	accepted := []string{
		"^" + strings.Repeat("(.*a)", 12) + "$",
		"^" + strings.Repeat(".*a", 20) + "$",
		"^.*a.*a.*a.*a$",
		"^" + strings.Repeat("[^x]*", 12) + "$",
		"^" + strings.Repeat("(a|b|c|.)", 6) + "$",
	}
	for _, p := range accepted {
		m := mustStringMatcher(t, p)
		for _, n := range []int{100, 256, 257, 10000} {
			s := strings.Repeat("a", n-1) + "x"
			within(t, "regex "+p, 200, 2*time.Second, func() { m.Contains(s) })
			if n > maxCandidateLen && m.Contains(strings.Repeat("a", n)) {
				t.Errorf("%q matched a %d-byte build", p, n)
			}
		}
	}
	rejected := []string{
		"^" + strings.Repeat("(.*a)", 50) + "$",
		"^" + strings.Repeat(".*a", 84) + "$",
		"^" + strings.Repeat("(a|b|c|.)", 28) + "$",
		"^" + strings.Repeat("(a*b*)*", 36) + "$",
		"^" + strings.Repeat("a?", 127) + "$",
		"^" + strings.Repeat("[^x]*", 50) + "$",
		"^(?i)" + strings.Repeat(".*k", 82) + "$",
		"^" + strings.Repeat(`\pL*`, 63) + "$",
		"^" + strings.Repeat(".", 64) + "$",
	}
	for _, p := range rejected {
		if len(p) > maxPatternLen {
			p = p[:maxPatternLen-1] + "$"
		}
		if _, err := ParseStringMatcher(p); !errors.Is(err, ErrStringMatcher) {
			t.Errorf("ParseStringMatcher(%d-byte regex) err = %v", len(p), err)
		}
	}
}

// TestInputLimits checks the size limits on hostile input.
func TestInputLimits(t *testing.T) {
	ok := []string{strings.Repeat("a", 256), strings.Repeat("1.", 31) + "1+" + strings.Repeat("1.", 31) + "1"}
	for _, s := range ok {
		if _, err := ParseVersion(s); err != nil {
			t.Errorf("ParseVersion(%d bytes): %v", len(s), err)
		}
	}
	bad := []string{
		strings.Repeat("a", 257),
		strings.Repeat("1.", 64) + "1",
		strings.Repeat("1.", 32) + "1+" + strings.Repeat("1.", 32) + "1",
	}
	for _, s := range bad {
		if _, err := ParseVersion(s); !errors.Is(err, ErrVersion) {
			t.Errorf("ParseVersion(%d bytes) err = %v", len(s), err)
		}
	}
	if _, err := ParseVersionSpec(strings.Repeat("!=1,", 256) + "1"); !errors.Is(err, ErrVersionSpec) {
		t.Errorf("1025-byte version spec: err = %v", err)
	}
	if _, err := ParseVersionSpec(strings.Repeat("!=1,", 255) + "!=11"); err != nil {
		t.Errorf("1024-byte version spec: %v", err)
	}
	longSpecs := []string{
		"foo " + strings.Repeat(">=1,", 1100),
		strings.Repeat("a", 257),
		"foo 1 " + strings.Repeat("b", 257),
		"foo[md5=" + strings.Repeat("0", 4100) + "]",
	}
	for _, s := range longSpecs {
		if _, err := ParseMatchSpec(s); !errors.Is(err, ErrMatchSpec) {
			t.Errorf("ParseMatchSpec(%d bytes) err = %v", len(s), err)
		} else if len(err.Error()) > 1000 {
			t.Errorf("error message of %d bytes", len(err.Error()))
		}
	}
	// Every accepted spec's canonical form must parse back, even when
	// normalization makes it longer than the input.
	for _, vs := range []string{
		strings.TrimSuffix(strings.Repeat("1|", 340), "|"),
		strings.TrimSuffix(strings.Repeat("1|", 512), "|"),
		strings.TrimSuffix(strings.Repeat("1,1|", 256), "|"),
		strings.TrimSuffix(strings.Repeat("!=1.2,", 170), ","),
	} {
		for _, in := range []string{vs, "foo " + vs} {
			if len(in) > maxSpecLen {
				continue
			}
			if strings.HasPrefix(in, "foo ") {
				m, err := ParseMatchSpec(in)
				if err == nil {
					if m2, err := ParseMatchSpec(m.String()); err != nil || !m2.Equal(m) {
						t.Errorf("%d-byte match spec: String does not parse back: %v", len(in), err)
					}
				} else if !errors.Is(err, ErrMatchSpec) {
					t.Errorf("%d-byte match spec: %v", len(in), err)
				}
				continue
			}
			s, err := ParseVersionSpec(in)
			if err == nil {
				if s2, err := ParseVersionSpec(s.String()); err != nil || !s2.Equal(s) {
					t.Errorf("%d-byte version spec: String does not parse back: %v", len(in), err)
				}
			} else if !errors.Is(err, ErrVersionSpec) {
				t.Errorf("%d-byte version spec: %v", len(in), err)
			}
		}
	}
	if _, err := ParseVersionSpec(strings.TrimSuffix(strings.Repeat("1|", 512), "|")); err == nil {
		t.Error("spec whose canonical form exceeds 1024 bytes accepted")
	}
	if _, err := ParseMatchSpec(strings.Repeat("a", 256)); err != nil {
		t.Errorf("256-byte name: %v", err)
	}
	if _, err := ParseChannel(strings.Repeat("a", 1025)); !errors.Is(err, ErrChannel) {
		t.Errorf("long channel: err = %v", err)
	}
}

// TestRegexLimits replays the WP1 review's regex blow-ups: counted
// repetition and long patterns are rejected before compiling.
func TestRegexLimits(t *testing.T) {
	bad := []string{
		"^(a{1000}){1000}$",
		"^" + strings.Repeat("a{1000}", 100) + "$",
		"^((((((((((a{2}){2}){2}){2}){2}){2}){2}){2}){2}){2}){1000}$",
		"^" + strings.Repeat("(a|b)", 60) + "$",
		"^" + strings.Repeat("a", 300) + ".*$",
	}
	for _, p := range bad {
		var err error
		within(t, "regex "+truncate(p), 1, time.Second, func() { _, err = ParseStringMatcher(p) })
		if !errors.Is(err, ErrStringMatcher) {
			t.Errorf("ParseStringMatcher(%q) err = %v", truncate(p), err)
		}
	}
	good := []string{"^.*(accelerate|mkl)$", "^py3[0-9]+_.*$", "^(" + strings.Repeat("[a-z]", 12) + ")$"}
	for _, p := range good {
		if _, err := ParseStringMatcher(p); err != nil {
			t.Errorf("ParseStringMatcher(%q): %v", p, err)
		}
	}
}
