package spec

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func mkPred(op predOp, v Version) versionPredicate { return versionPredicate{op: op, ver: v} }

func predSpec(p versionPredicate) VersionSpec {
	return VersionSpec{nodes: []vsNode{{pred: p, kind: nodeLeaf, left: -1, right: -1}}}
}

func predString(p versionPredicate) string {
	var b strings.Builder
	p.writeTo(&b, false)
	return b.String()
}

func predCondaBuild(p versionPredicate) string {
	var b strings.Builder
	p.writeTo(&b, true)
	return b.String()
}

// Cases from libmamba tests/src/specs/test_version_spec.cpp ("VersionPredicate").
func TestVersionPredicate(t *testing.T) {
	v1, v2, v201 := mustVersion(t, "1.0"), mustVersion(t, "2.0"), mustVersion(t, "2.0.1")
	v3, v4 := mustVersion(t, "3.0"), mustVersion(t, "4.0")
	all := []Version{v1, v2, v201, v3, v4}

	cp2 := mkPred(opCompatible, v2)
	cp2.level = 2
	cp3 := mkPred(opCompatible, v2)
	cp3.level = 3

	cases := []struct {
		pred      versionPredicate
		contains  []bool // v1, v2, v201, v3, v4
		str       string
		condaB    string // "" = same as str
		glob      bool
		classic   bool
		extraIn   []string
		extraOut  []string
		skipFlags bool
	}{
		{pred: mkPred(opFree, Version{}), contains: []bool{true, true, true, true, true}, str: "=*"},
		{pred: mkPred(opEqual, v2), contains: []bool{false, true, false, false, false}, str: "==2.0", classic: true},
		{pred: mkPred(opNotEqual, v2), contains: []bool{true, false, true, true, true}, str: "!=2.0", classic: true},
		{pred: mkPred(opGreater, v2), contains: []bool{false, false, true, true, true}, str: ">2.0", classic: true},
		{pred: mkPred(opGreaterEqual, v2), contains: []bool{false, true, true, true, true}, str: ">=2.0", classic: true},
		{pred: mkPred(opLess, v2), contains: []bool{true, false, false, false, false}, str: "<2.0", classic: true},
		{pred: mkPred(opLessEqual, v2), contains: []bool{true, true, false, false, false}, str: "<=2.0", classic: true},
		{pred: mkPred(opStartsWith, v2), contains: []bool{false, true, true, false, false}, str: "=2.0", condaB: "2.0.*"},
		{pred: mkPred(opNotStartsWith, v2), contains: []bool{true, false, false, true, true}, str: "!=2.0.*"},
		{pred: cp2, contains: []bool{false, true, true, false, false}, str: "~=2.0"},
		{pred: cp3, contains: []bool{false, true, false, false, false}, str: "~=2.0.0"},
		{pred: mkPred(opGlob, mustVersion(t, "*")), contains: []bool{true, true, true, true, true}, str: "*", glob: true},
		{pred: mkPred(opGlob, mustVersion(t, "*.0.*")), contains: []bool{false, false, true, false, false}, str: "*.0.*", glob: true, extraIn: []string{"1.0.1.1.1"}},
		{pred: mkPred(opGlob, mustVersion(t, "*.0")), contains: []bool{true, true, false, true, true}, str: "*.0", glob: true},
		{pred: mkPred(opGlob, mustVersion(t, "2.*")), contains: []bool{false, true, true, false, false}, str: "2.*", glob: true},
		{pred: mkPred(opGlob, mustVersion(t, "2.0")), contains: []bool{false, true, false, false, false}, str: "2.0", glob: true},
		{pred: mkPred(opGlob, mustVersion(t, "2.*.1")), contains: []bool{false, false, true, false, false}, str: "2.*.1", glob: true},
		{pred: mkPred(opGlob, mustVersion(t, "2.*.1.1.*")), contains: []bool{false, false, false, false, false}, str: "2.*.1.1.*", glob: true, extraIn: []string{"2.0.1.0.1.1.3"}},
		{pred: mkPred(opNotGlob, mustVersion(t, "2.*.1")), contains: []bool{true, true, false, true, true}, str: "!=2.*.1", glob: true},
	}
	for _, c := range cases {
		for i, v := range all {
			if got := c.pred.contains(&v); got != c.contains[i] {
				t.Errorf("%s contains %s = %v", c.str, v, got)
			}
		}
		for _, s := range c.extraIn {
			v := mustVersion(t, s)
			if !c.pred.contains(&v) {
				t.Errorf("%s should contain %s", c.str, s)
			}
		}
		if got := predString(c.pred); got != c.str {
			t.Errorf("String = %q, want %q", got, c.str)
		}
		wantB := c.condaB
		if wantB == "" {
			wantB = c.str
		}
		if got := predCondaBuild(c.pred); got != wantB {
			t.Errorf("conda-build string = %q, want %q", got, wantB)
		}
		if c.pred.hasGlob() != c.glob {
			t.Errorf("%s hasGlob = %v", c.str, !c.glob)
		}
		if c.pred.isClassicOperator() != c.classic {
			t.Errorf("%s isClassicOperator = %v", c.str, !c.classic)
		}
	}
	if mustVersion(t, "*.0").Compare(mustVersion(t, "*.0.*")) == 0 {
		t.Error(`"*.0" == "*.0.*"`)
	}
	for i := range cases {
		if !cases[i].pred.equal(&cases[i].pred) {
			t.Errorf("predicate %d != itself", i)
		}
		for j := i + 1; j < len(cases); j++ {
			if cases[i].pred.equal(&cases[j].pred) {
				t.Errorf("predicate %d (%s) == %d (%s)", i, cases[i].str, j, cases[j].str)
			}
		}
	}
}

// Cases from libmamba tests/src/specs/test_version_spec.cpp ("Tree construction").
func TestVersionSpecTreeConstruction(t *testing.T) {
	var empty VersionSpec
	if !empty.Contains(Version{}) || empty.String() != "=*" {
		t.Error("empty spec")
	}

	s := predSpec(mkPred(opEqual, mustVersion(t, "1.0")))
	if !s.Contains(mustVersion(t, "1.0")) || s.Contains(mustVersion(t, "2.0")) || s.String() != "==1.0" {
		t.Error("from_predicate spec")
	}

	// <2.0|(>2.3,<=2.8.0)
	v20 := mkVer(0, []tp{{{2, ""}}, {{0, ""}}}, nil)
	v23 := mkVer(0, []tp{{{2, ""}}, {{3, ""}}}, nil)
	v28 := mkVer(0, []tp{{{2, ""}}, {{8, ""}}, {{0, ""}}}, nil)
	var p infixParser
	ok := p.pushVariable(mkPred(opLess, v20)) &&
		p.pushOperator(nodeOr) &&
		p.pushLeftParen() &&
		p.pushVariable(mkPred(opGreater, v23)) &&
		p.pushOperator(nodeAnd) &&
		p.pushVariable(mkPred(opLessEqual, v28)) &&
		p.pushRightParen() &&
		p.finalize()
	if !ok {
		t.Fatal("parser rejected <2.0|(>2.3,<=2.8.0)")
	}
	spec := VersionSpec{nodes: p.nodes}
	for _, v := range []Version{
		mkVer(0, []tp{{{2, ""}}, {{3, ""}}, {{1, ""}}}, nil),
		mkVer(0, []tp{{{2, ""}}, {{8, ""}}}, nil),
		mkVer(0, []tp{{{1, ""}}, {{8, ""}}}, nil),
	} {
		if !spec.Contains(v) {
			t.Errorf("should contain %s", v)
		}
	}
	for _, v := range []Version{
		mkVer(0, []tp{{{2, ""}}, {{0, ""}}, {{0, ""}}}, nil),
		mkVer(0, []tp{{{2, ""}}, {{1, ""}}}, nil),
		mkVer(0, []tp{{{2, ""}}, {{3, ""}}}, nil),
	} {
		if spec.Contains(v) {
			t.Errorf("should not contain %s", v)
		}
	}
	if got := spec.String(); got != "<2.0|(>2.3,<=2.8.0)" {
		t.Errorf("String = %q", got)
	}
}

// Cases from libmamba tests/src/specs/test_version_spec.cpp
// ("VersionSpec::parse" / "Successful").
func TestParseVersionSpecContains(t *testing.T) {
	cases := []struct {
		spec, ver string
		want      bool
	}{
		{"", "1.6", true},
		{"", "0.6+0.7", true},
		{"*", "1.4", true},
		{"=*", "1.4", true},

		{"1.7", "1.7", true},
		{"1.7", "1.7.0.0", true},
		{"1.7", "1.6", false},
		{"1.7", "1.7.7", false},
		{"1.7", "1.7.0.1", false},

		{"==1.7", "1.7", true},
		{"==1.7", "1.7.0.0", true},
		{"==1.7", "1.6", false},
		{"==1.7", "1.7.7", false},
		{"==1.7", "1.7.0.1", false},

		{"!=1.7", "1.7", false},
		{"!=1.7", "1.7.0.0", false},
		{"!=1.7", "1.6", true},
		{"!=1.7", "1.7.7", true},
		{"!=1.7", "1.7.0.1", true},

		{"<1.7", "1.7", false},
		{"<1.7", "1.7.0.0", false},
		{"<1.7", "1.6", true},
		{"<1.7", "1.7a", true},
		{"<1.7", "1.7.7", false},
		{"<1.7", "1.7.0.1", false},

		{"<=1.7", "1.7", true},
		{"<=1.7", "1.7.0.0", true},
		{"<=1.7", "1.6", true},
		{"<=1.7", "1.7a", true},
		{"<=1.7", "1.7.7", false},
		{"<=1.7", "1.7.0.1", false},

		{">1.7", "1.7", false},
		{">1.7", "1.7.0.0", false},
		{">1.7", "1.6", false},
		{">1.7", "1.7a", false},
		{">1.7", "1.7.7", true},
		{">1.7", "1.7.0.1", true},

		{">= 1.7", "1.7", true},
		{">= 1.7", "1.7.0.0", true},
		{">= 1.7", "1.6", false},
		{">= 1.7", "1.7a", false},
		{">= 1.7", "1.7.7", true},
		{">= 1.7", "1.7.0.1", true},

		{" = 1.8", "1.7.0.1", false},
		{" = 1.8", "1.8", true},
		{" = 1.8", "1.8.0", true},
		{" = 1.8", "1.8.1", true},
		{" = 1.8", "1.8alpha", true},
		{" = 1.8", "1.9", false},

		{" = 1.8.* ", "1.7.0.1", false},
		{" = 1.8.*", "1.8", true},
		{" = 1.8.*", "1.8.0", true},
		{" = 1.8.*", "1.8.1", true},
		{" = 1.8.*", "1.8alpha", true}, // Like Conda
		{" = 1.8.*", "1.9", false},

		{"  1.8.* ", "1.7.0.1", false},
		{"  1.8.*", "1.8", true},
		{"  1.8.*", "1.8.0", true},
		{"  1.8.*", "1.8.1", true},
		{"  1.8.*", "1.8alpha", true}, // Like Conda
		{"  1.8.*", "1.9", false},

		{" != 1.8.*", "1.7.0.1", true},
		{" != 1.8.*", "1.8", false},
		{" != 1.8.*", "1.8.0", false},
		{" != 1.8.*", "1.8.1", false},
		{" != 1.8.*", "1.8alpha", false}, // Like Conda
		{" != 1.8.*", "1.9", true},

		{" 1.*.3", "1.7.3", true},
		{" 1.*.3", "1.7.0.3", true},
		{" 1.*.3", "1.7.3.4", false},
		{" 1.*.3", "1.3", false},
		{" 1.*.3", "2.0.3", false},

		{" =1.*.3", "1.7.3", true},
		{" =1.*.3", "1.7.0.3", true},
		{" =1.*.3", "1.7.3.4", false},
		{" =1.*.3", "1.3", false},
		{" =1.*.3", "2.0.3", false},

		{"!=1.*.3 ", "1.7.3", false},
		{"!=1.*.3 ", "1.7.0.3", false},
		{"!=1.*.3 ", "1.7.3.4", true},
		{"!=1.*.3 ", "1.3", true},
		{"!=1.*.3 ", "2.0.3", true},

		{" ~= 1.8 ", "1.7.0.1", false},
		{" ~= 1.8 ", "1.8", true},
		{" ~= 1.8 ", "1.8.0", true},
		{" ~= 1.8 ", "1.8.1", true},
		{" ~= 1.8 ", "1.9", true},
		{" ~= 1.8 ", "1.8post", true},
		{" ~= 1.8 ", "1.8alpha", false},

		{" ~=1 ", "1.7.0.1", true},
		{" ~=1 ", "1.8", true},
		{" ~=1 ", "1.8post", true},
		{" ~=1 ", "2.0", true},
		{" ~=1 ", "0.1", false},
		{" ~=1 ", "1.0.alpha", false},

		{" (>= 1.7, <1.8) |>=1.9.0.0 ", "1.6", false},
		{" (>= 1.7, <1.8) |>=1.9.0.0 ", "1.7.0.0", true},
		{" (>= 1.7, <1.8) |>=1.9.0.0 ", "1.8.1", false},
		{" (>= 1.7, <1.8) |>=1.9.0.0 ", "6.33", true},

		// Test from Conda
		{"==1.7", "1.7.0", true},
		{"<=1.7", "1.7.0", true},
		{"<1.7", "1.7.0", false},
		{">=1.7", "1.7.0", true},
		{">1.7", "1.7.0", false},
		{">=1.7", "1.6.7", false},
		{">2013b", "2013a", false},
		{">2013b", "2013k", true},
		{">2013b", "3.0.0", false},
		{">1.0.0a", "1.0.0", true},
		{">1.0.0*", "1.0.0", true},
		{"1.0*", "1.0", true},
		{"1.0*", "1.0.0", true},
		{"1.0.0*", "1.0", true},
		{"1.0.0*", "1.0.1", false},
		{"2013a*", "2013a", true},
		{"2013b*", "2013a", false},
		{"1.2.4*", "1.3.4", false},
		{"1.2.3*", "1.2.3+4.5.6", true},
		{"1.2.3+4*", "1.2.3+4.5.6", true},
		{"1.2.3+5*", "1.2.3+4.5.6", false},
		{"1.2.4+5*", "1.2.3+4.5.6", false},
		{"1.7.*", "1.7.1", true},
		{"1.7.1", "1.7.1", true},
		{"1.7.0", "1.7.1", false},
		{"1.7", "1.7.1", false},
		{"1.5.*", "1.7.1", false},
		{">=1.5", "1.7.1", true},
		{"!=1.5", "1.7.1", true},
		{"!=1.7.1", "1.7.1", false},
		{"==1.7.1", "1.7.1", true},
		{"==1.7", "1.7.1", false},
		{"==1.7.2", "1.7.1", false},
		{"==1.7.1.0", "1.7.1", true},
		{"==1.7.1.*", "1.7.1.1", true}, // Degenerate case
		{"1.7.*|1.8.*", "1.7.1", true},
		{">1.7,<1.8", "1.7.1", true},
		{">1.7.1,<1.8", "1.7.1", false},
		{"*", "1.7.1", true},
		{"1.5.*|>1.7,<1.8", "1.7.1", true},
		{"1.5.*|>1.7,<1.7.1", "1.7.1", false},
		{"1.7.0.post123", "1.7.0.post123", true},
		{"1.7.0.post123.gabcdef9", "1.7.0.post123.gabcdef9", true},
		{"1.7.0.post123+gabcdef9", "1.7.0.post123+gabcdef9", true},
		{"=3.3", "3.3.1", true},
		{"=3.3", "3.3", true},
		{"=3.3", "3.4", false},
		{"3.3.*", "3.3.1", true},
		{"3.3.*", "3.3", true},
		{"3.3.*", "3.4", false},
		{"=3.3.*", "3.3.1", true},
		{"=3.3.*", "3.3", true},
		{"=3.3.*", "3.4", false},
		{"!=3.3.*", "3.3.1", false},
		{"!=3.3.*", "3.4", true},
		{"!=3.3.*", "3.4.1", true},
		{"!=3.3", "3.3.1", true},
		{"!=3.3", "3.3.0.0", false},
		{"!=3.3.*", "3.3.0.0", false},
		{">=2.7, !=3.0.*, !=3.1.*, !=3.2.*, !=3.3.*", "2.6.8", false},
		{">=2.7, !=3.0.*, !=3.1.*, !=3.2.*, !=3.3.*", "2.7.2", true},
		{">=2.7, !=3.0.*, !=3.1.*, !=3.2.*, !=3.3.*", "3.3", false},
		{">=2.7, !=3.0.*, !=3.1.*, !=3.2.*, !=3.3.*", "3.3.4", false},
		{">=2.7, !=3.0.*, !=3.1.*, !=3.2.*, !=3.3.*", "3.4", true},
		{">=2.7, !=3.0.*, !=3.1.*, !=3.2.*, !=3.3.*", "3.4a", true},
		{"~=1.10", "1.11.0", true},
		{"~=1.10.0", "1.11.0", false},
		{"~=3.3.2", "3.4.0", false},
		{"~=3.3.2", "3.3.1", false},
		{"~=3.3.2", "3.3.2.0", true},
		{"~=3.3.2", "3.3.3", true},
		{"~=3.3.2|==2.2", "2.2.0", true},
		{"~=3.3.2|==2.2", "3.3.3", true},
		{"~=3.3.2|==2.2", "2.2.1", false},
		{"*.*", "3.3", true},
		{"*.*", "3.3.3", true},
		{"*.*", "3", false},
		{"2.*.1.1.*", "2.0.1.0.1.1.3", true},
		{"2.*.1.1.*", "2.1.0.1.1", false},
		{"*.3", "2.1.0.1.1.3", true},
		{"*.3", "0.3.4", false},
		{"*.2023_10_12", "2.1.0.2023_10_12", true},
		{">=10.0,*.*", "10.1", true},
		{">=10.0,*.*", "11", false},
		{"1.*.1", "1.7.1", true},
		// Regexes are not supported (commented out upstream too).
	}
	for _, c := range cases {
		s := mustVersionSpec(t, c.spec)
		v := mustVersion(t, c.ver)
		if got := s.Contains(v); got != c.want {
			t.Errorf("%q contains %q = %v, want %v (parsed as %q)", c.spec, c.ver, got, c.want, s)
		}
		// The normalized form must mean the same.
		s2 := mustVersionSpec(t, s.String())
		if got := s2.Contains(v); got != c.want {
			t.Errorf("%q (from %q) contains %q = %v, want %v", s.String(), c.spec, c.ver, got, c.want)
		}
	}
}

// Cases from libmamba tests/src/specs/test_version_spec.cpp
// ("VersionSpec::parse" / "Unsuccessful").
func TestParseVersionSpecNegative(t *testing.T) {
	bad := []string{
		"><2.4.5",
		"!!2.4.5",
		"!",
		"(1.5",
		"1.5)",
		"1.5||1.6",
		"^1.5",
		"~",
		"^",
		"===3.3.2", // PEP440 arbitrary equality not implemented in Conda
		// Upstream lacks a comma here, so C++ concatenates the two literals
		// into "~=3.3.2.*1.2+"; libmamba accepts "~=3.3.2.*" itself.
		"~=3.3.2.*1.2+",
		// Conda tests
		"1.2+",
		"+1.2",
		"+1.2+",
		"++",
		"c +, 0/|0 *",
		"a[version=)|(",
		"a=)(=b",
		"==",
		"=",
		">=",
		"<=",
		// Added for goomba.
		"()",
		"1.5,",
		",1.5",
		"(1.5|)",
		"1.0|(2.0",
	}
	for _, s := range bad {
		_, err := ParseVersionSpec(s)
		if err == nil {
			t.Errorf("ParseVersionSpec(%q) succeeded", s)
			continue
		}
		if !errors.Is(err, ErrVersionSpec) {
			t.Errorf("ParseVersionSpec(%q): error %v is not ErrVersionSpec", s, err)
		}
	}
	if _, err := ParseVersionSpec("~=3.3.2.*"); err != nil {
		t.Errorf(`"~=3.3.2.*" (accepted by libmamba): %v`, err)
	}
}

// Cases from libmamba tests/src/specs/test_version_spec.cpp ("VersionSpec::str").
func TestVersionSpecStr(t *testing.T) {
	cases := []struct{ in, str, condaBuild string }{
		{"2.3", "==2.3", "==2.3"},
		{"=2.3,<3.0", "=2.3,<3.0", "2.3.*,<3.0"},
		{"~=1", "~=1", "~=1"},
		{"~=1.8", "~=1.8", "~=1.8"},
		{"~=1.8.0", "~=1.8.0", "~=1.8.0"},
	}
	for _, c := range cases {
		s := mustVersionSpec(t, c.in)
		if got := s.String(); got != c.str {
			t.Errorf("%q: String = %q, want %q", c.in, got, c.str)
		}
		if got := s.CondaBuildString(); got != c.condaBuild {
			t.Errorf("%q: CondaBuildString = %q, want %q", c.in, got, c.condaBuild)
		}
	}
}

// Cases from libmamba tests/src/specs/test_version_spec.cpp
// ("VersionSpec::is_explicitly_free", "has_glob",
// "is_classic_operator_expression", "Comparability and hashability").
func TestVersionSpecPredicates(t *testing.T) {
	var p infixParser
	if !p.pushVariable(mkPred(opFree, Version{})) || !p.finalize() {
		t.Fatal("parser")
	}
	if !(VersionSpec{nodes: p.nodes}).IsAny() {
		t.Error("tree with a single free predicate should be any")
	}
	for _, s := range []string{"*", "", "* ", "=*", "==*"} {
		if !mustVersionSpec(t, s).IsAny() {
			t.Errorf("%q should be any", s)
		}
	}
	if !(VersionSpec{}).IsAny() {
		t.Error("zero VersionSpec should be any")
	}
	for _, s := range []string{"==2.3|!=2.3", "=2.3,<3.0"} {
		if mustVersionSpec(t, s).IsAny() {
			t.Errorf("%q should not be any", s)
		}
	}

	for _, s := range []string{"*.4", "1.*.0", "1.0|4.*.0"} {
		if !mustVersionSpec(t, s).hasGlob() {
			t.Errorf("%q should have a glob", s)
		}
	}
	for _, s := range []string{"*", "3.*"} {
		if mustVersionSpec(t, s).hasGlob() {
			t.Errorf("%q should not have a glob", s)
		}
	}

	for _, s := range []string{"==1.0", "==1.0,<3.0", "<1.0,<3.0", "(<1.0,<3.0)|(>=4.0,!=4.1)"} {
		if !mustVersionSpec(t, s).isClassicOperatorExpression() {
			t.Errorf("%q should be a classic operator expression", s)
		}
	}
	for _, s := range []string{"*.4", "*", "3.*"} {
		if mustVersionSpec(t, s).isClassicOperatorExpression() {
			t.Errorf("%q should not be a classic operator expression", s)
		}
	}

	s1, s2, s3 := mustVersionSpec(t, "*"), mustVersionSpec(t, "*"), mustVersionSpec(t, "=2.4")
	if !s1.Equal(s2) || s1.Equal(s3) || s1.String() != s2.String() || s1.String() == s3.String() {
		t.Error("equality")
	}
}

// TestVersionSpecPrecedence pins conda's precedence, where "," binds
// tighter than "|". The first two cases are from libmamba
// tests/src/specs/test_match_spec.cpp, which expects libmamba's reading
// instead: ">=1.8,(<2|==1.9)" and ">1.8,((<2|==1.7),(!=1.9,~=1.7.1))".
// Chains of one operator are written without parentheses ("a,b,c", where
// libmamba writes "a,(b,c)"); they parse back to the same tree.
func TestVersionSpecPrecedence(t *testing.T) {
	cases := []struct{ in, str string }{
		{">=1.8,<2|1.9", "(>=1.8,<2)|==1.9"},
		{">1.8,<2|==1.7,!=1.9,~=1.7.1", "(>1.8,<2)|(==1.7,!=1.9,~=1.7.1)"},
		{"1|2,3|4", "==1|(==2,==3)|==4"},
		{"(1|2),3", "(==1|==2),==3"},
		{"1|(2,3)", "==1|(==2,==3)"},
	}
	for _, c := range cases {
		s := mustVersionSpec(t, c.in)
		if got := s.String(); got != c.str {
			t.Errorf("%q: String = %q, want %q", c.in, got, c.str)
		}
		if !mustVersionSpec(t, s.String()).Equal(s) {
			t.Errorf("%q: String does not round-trip", c.in)
		}
	}
	contains := []struct {
		spec, ver string
		want      bool
	}{
		// conda, rattler and libsolv (micromamba's repodata path) accept
		// 3.5; libmamba's parser would not.
		{">=3|<1,<0.9", "3.5", true},
		{">=3|<1,<0.9", "0.5", true},
		{">=3|<1,<0.9", "0.95", false},
		{"<0.9,<1|>=3", "3.5", true},
		{">=0.1|>=3,<1", "3.5", true},
		// The conda-forge spec where the readings differ.
		{">=0.17.6,<0.18|0.16.6.*", "0.16.6", true},
		{">=0.17.6,<0.18|0.16.6.*", "0.17.21", true},
		{">=0.17.6,<0.18|0.16.6.*", "0.18.1", false},
		{"1.0|2.0,3.0", "1.0", true},
		{"1.0|2.0,3.0", "2.0", false},
	}
	for _, c := range contains {
		if got := mustVersionSpec(t, c.spec).Contains(mustVersion(t, c.ver)); got != c.want {
			t.Errorf("%q contains %q = %v, want %v", c.spec, c.ver, got, c.want)
		}
	}
}

// TestVersionGlob covers glob parts that must backtrack, which libmamba's
// first-occurrence search gets wrong ("*.3" against "3.3").
func TestVersionGlob(t *testing.T) {
	cases := []struct {
		spec, ver string
		want      bool
	}{
		{"*.3", "3.3", true},
		{"*.3", "3.3.3", true},
		{"*.3", "3", false},
		{"*.3", "0.3.4", false},
		{"*.1.*", "1.1.1", true},
		{"*.1.*", "1.1", false},
		{"*.1.*", "2.1.0.1", true},
		{"1.*.1", "1.1.1.1", true},
		{"1.*.1", "1.1", false},
		{"*.*", "3.3", true},
		{"*.*", "3", false},
		{"*.0", "1!2.0", false}, // epochs must match
		{"*.0", "2.0+local", false},
		{"2.*.1.1.*", "2.1.1.1.1.1", true},
		{"!=*.3", "3.3", false},
	}
	for _, c := range cases {
		if got := mustVersionSpec(t, c.spec).Contains(mustVersion(t, c.ver)); got != c.want {
			t.Errorf("%q contains %q = %v, want %v", c.spec, c.ver, got, c.want)
		}
	}
}

func TestVersionSpecDepth(t *testing.T) {
	// Long flat chains are fine.
	var parts []string
	for i := 0; i < 120; i++ {
		parts = append(parts, "!=1."+strconv.Itoa(i))
	}
	s := mustVersionSpec(t, strings.Join(parts, ","))
	if !s.Contains(mustVersion(t, "2.0")) || s.Contains(mustVersion(t, "1.119")) {
		t.Error("flat chain")
	}
	if s2 := mustVersionSpec(t, s.String()); !s2.Equal(s) {
		t.Error("flat chain round trip")
	}
	// Moderate nesting is fine.
	nested := strings.Repeat("(", 50) + "1.0" + strings.Repeat(",>0)", 50) // depth 50, under the limit
	if !mustVersionSpec(t, nested).Contains(mustVersion(t, "1.0")) {
		t.Error("nested")
	}
	// Left-deep nesting beyond the limit is rejected.
	deep := strings.Repeat("(", maxVersionSpecDepth+1) + "1.0" + strings.Repeat(",>0)", maxVersionSpecDepth+1)
	if _, err := ParseVersionSpec(deep); err == nil {
		t.Error("deep nesting accepted")
	}
}
