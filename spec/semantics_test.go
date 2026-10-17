package spec

import (
	"errors"
	"strings"
	"testing"
)

// condaStartsWithVersions and condaStartsWithCases are the versions each
// starts-with spec accepts according to conda 24.7.1's VersionSpec, from
// the WP1 review's conda oracle.
var condaStartsWithVersions = "0 0.1 0.9 1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.1 1.1.0 1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha 1.20 1.21 1.3 1.10 1.11.0 2 2.0 2.0.0 2.0a0 2.0.dev0 2.0rc1 2.1 2.10 3 3.0 3.11 3.11.0 3.11.15 3.12.0rc1 3.12 1!0.5 1!1.0 1!2.0 2!1.0 2013a 2013b 2023.10.12 v1.0 1.0.0.0.0.1 0.0.0 1.0.0-1 1.0-1 1.0-post1 1.* 1.0* * 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 10.0 1.0.0.0 4.0.0a0 7.1.0r 0.5.p0"

var condaStartsWithCases = []struct{ spec, accepted string }{
	{"=1.0", "1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.0.0.0.0.1 1.0.0-1 1.0-1 1.0-post1 1.* 1.0* 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 1.0.0.0"},
	{"1.0.*", "1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.0.0.0.0.1 1.0.0-1 1.0-1 1.0-post1 1.* 1.0* 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 1.0.0.0"},
	{"1.0*", "1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.0.0.0.0.1 1.0.0-1 1.0-1 1.0-post1 1.* 1.0* 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 1.0.0.0"},
	{"=1.0.*", "1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.0.0.0.0.1 1.0.0-1 1.0-1 1.0-post1 1.* 1.0* 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 1.0.0.0"},
	{"1.0.0.*", "1 1.0 1.0.0 1.0.0a1 1.0.0rc1 1.0.dev1 1.0.post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0.0.0.0.1 1.0.0-1 1.0-post1 1.0.dev 1.0.DEV1 1.0.Post1 1_0 1.0.0.0"},
	{"=1.0.0", "1 1.0 1.0.0 1.0.0a1 1.0.0rc1 1.0.dev1 1.0.post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0.0.0.0.1 1.0.0-1 1.0-post1 1.0.dev 1.0.DEV1 1.0.Post1 1_0 1.0.0.0"},
	{"=1.0.0.0", "1 1.0 1.0.0 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0.0.0.0.1 1_0 1.0.0.0"},
	{"1.0.0.0.*", "1 1.0 1.0.0 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0.0.0.0.1 1_0 1.0.0.0"},
	{"1.2*", "1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha"},
	{"1.2.*", "1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha"},
	{"=1.2", "1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha"},
	{"1.*", "1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.1 1.1.0 1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha 1.20 1.21 1.3 1.10 1.11.0 1.0.0.0.0.1 1.0.0-1 1.0-1 1.0-post1 1.* 1.0* 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 1.0.0.0"},
	{"=1", "1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.1 1.1.0 1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha 1.20 1.21 1.3 1.10 1.11.0 1.0.0.0.0.1 1.0.0-1 1.0-1 1.0-post1 1.* 1.0* 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 1.0.0.0"},
	{"1.0+local*", "1.0+local 1.0+local.1"},
	{"=1.0+local", "1.0+local 1.0+local.1"},
	{"=1!1", "1!1.0"},
	{"2013*", "2013a 2013b"},
	{"v1.*", "v1.0"},
	{"1.0dev*", "1.0dev"},
	{"1.0.dev*", "1.0.dev1 1.0.dev 1.0.DEV1"},
	{"=1.0.dev", "1.0.dev1 1.0.dev 1.0.DEV1"},
	{"1.0.post*", "1.0.post1 1.0-post1 1.0.Post1"},
	{"1.0rc*", "1.0rc1 1.0RC1"},
	{"1.0.0rc*", "1.0.0rc1"},
	{"3.12.*", "3.12.0rc1 3.12"},
	{"3.12.0.*", "3.12.0rc1 3.12"},
	{"=3.12.0", "3.12.0rc1 3.12"},
	{"1.0a*", "1.0a1"},
	{"1.2a*", "1.2a 1.2alpha"},
	{"=1.2a", "1.2a 1.2alpha"},
	{"!=1.0.*", "0 0.1 0.9 1.1 1.1.0 1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha 1.20 1.21 1.3 1.10 1.11.0 2 2.0 2.0.0 2.0a0 2.0.dev0 2.0rc1 2.1 2.10 3 3.0 3.11 3.11.0 3.11.15 3.12.0rc1 3.12 1!0.5 1!1.0 1!2.0 2!1.0 2013a 2013b 2023.10.12 v1.0 0.0.0 * 10.0 4.0.0a0 7.1.0r 0.5.p0"},
	{"!=1.0*", "0 0.1 0.9 1 1.0 1.0.0 1.0.1 1.0a1 1.0.0a1 1.0rc1 1.0.0rc1 1.0.dev1 1.0.post1 1.0post1 1.0+local 1.0+local.1 1.0+abc 1.0.0+abc.1 1.0_ 1.0.1_ 1.0.1a 1.1 1.1.0 1.2 1.2.0 1.2.3 1.2.3.4 1.2a 1.2alpha 1.20 1.21 1.3 1.10 1.11.0 2 2.0 2.0.0 2.0a0 2.0.dev0 2.0rc1 2.1 2.10 3 3.0 3.11 3.11.0 3.11.15 3.12.0rc1 3.12 1!0.5 1!1.0 1!2.0 2!1.0 2013a 2013b 2023.10.12 v1.0 1.0.0.0.0.1 0.0.0 1.0.0-1 1.0-1 1.0-post1 * 1.0.dev 1.0dev 1.0.DEV1 1.0.Post1 1.0RC1 1.0_1 1_0 10.0 1.0.0.0 4.0.0a0 7.1.0r 0.5.p0"},
}

// TestStartsWithMatchesConda checks starts-with specs ("=1.0", "1.0.*",
// "1.0*", "!=1.0.*") against conda: every part of the prefix but the last
// must be equal, so "1.0a1" does not start with "1.0.0" (libmamba says it
// does). The one deliberate difference follows libsolv, as micromamba
// does: the prefix's final literal must match exactly, so "=1.2a" does not
// accept "1.2alpha".
func TestStartsWithMatchesConda(t *testing.T) {
	libsolvExceptions := map[string]string{"1.2a* 1.2alpha": "", "=1.2a 1.2alpha": ""}
	versions := strings.Fields(condaStartsWithVersions)
	for _, c := range condaStartsWithCases {
		spec := mustVersionSpec(t, c.spec)
		accepted := map[string]bool{}
		for _, v := range strings.Fields(c.accepted) {
			accepted[v] = true
		}
		for _, vs := range versions {
			want := accepted[vs]
			if _, ok := libsolvExceptions[c.spec+" "+vs]; ok {
				want = !want
			}
			if got := spec.Contains(mustVersion(t, vs)); got != want {
				t.Errorf("%q contains %q = %v, conda %v", c.spec, vs, got, accepted[vs])
			}
		}
	}
}

// TestStartsWithMicromamba covers the starts-with probes that micromamba
// (libsolv) decides, from the WP1 review; TestMicromambaOracle checks the
// same cases against a live micromamba.
func TestStartsWithMicromamba(t *testing.T) {
	cases := []struct {
		spec, ver string
		want      bool
	}{
		{"=1.0.0", "1.0a1", false},
		{"=1.0.0", "1.0rc1", false},
		{"=1.0.0", "1.0post1", false},
		{"=1.0.0", "1.0_", false},
		{"=1.0.0", "1.0dev", false},
		{"=1.0.0", "1.0.0rc1", true},
		{"=1.0.0", "1.0.dev1", true},
		{"=1.0.0", "1", true},
		{"1.0.0.*", "1.0a1", false},
		{"1.0.0.*", "1.0.0.1", true},
		{"=1.0a", "1.0a1", true},
		{"=1.0a", "1.0alpha", false},
		{"1.0a*", "1.0a1", true},
		{"=1.8", "1.8alpha", true},
		{"=3.6", "3.6.0rc1", true},
		{"9.0.0.*", "9", true},
	}
	for _, c := range cases {
		if got := mustVersionSpec(t, c.spec).Contains(mustVersion(t, c.ver)); got != c.want {
			t.Errorf("%q contains %q = %v, want %v", c.spec, c.ver, got, c.want)
		}
	}
}

// TestRelationalGlobSuffix pins libsolv's reading of relational operators
// followed by ".*", which micromamba applies to repodata: ">=X.*" means
// ">=X"; ">X.*", "<X.*", "<=X.*" and "~=X.*" match nothing. "==X.*" stays
// a starts-with test, as in micromamba.
func TestRelationalGlobSuffix(t *testing.T) {
	cases := []struct {
		spec, ver string
		want      bool
	}{
		{">=3.6.*", "3.6.0", true},
		{">=3.6.*", "3.7", true},
		{">=3.6.*", "3.6.0rc1", false}, // libmamba accepts: "3.6.*" sorts below it
		{">=3.6.*", "3.5", false},
		{">=7.1.*", "7.1.0r", false},
		{">=0.5.*", "0.5.p0", false},
		{">=1.0.*.*", "1.0", true},
		{">0.23.*", "0.23.0", false},
		{">0.23.*", "1.0", false},
		{"<4.*", "3.0", false},
		{"<=3.6.*", "3.6.0", false},
		{"<=3.6.*", "1.0", false},
		{"~=3.6.*", "3.6.1", false},
		{">=3.0,<4.*", "3.5", false},
		{"==3.*", "3.7", true},
		{"==3.6.*", "3.6.1", true},
		{"!=3.6.*", "3.6.1", false},
		{"!=3.6.*", "3.7", true},
		{">1.0.0*", "1.0.0", true}, // no '.': the '*' is part of the version, as in libmamba
	}
	for _, c := range cases {
		s := mustVersionSpec(t, c.spec)
		if got := s.Contains(mustVersion(t, c.ver)); got != c.want {
			t.Errorf("%q (%q) contains %q = %v, want %v", c.spec, s, c.ver, got, c.want)
		}
		s2 := mustVersionSpec(t, s.String())
		if !s2.Equal(s) || s2.Contains(mustVersion(t, c.ver)) != c.want {
			t.Errorf("%q: String %q does not round-trip", c.spec, s)
		}
	}
	for _, spec := range []string{">=.*", "<.*.*"} {
		if _, err := ParseVersionSpec(spec); !errors.Is(err, ErrVersionSpec) {
			t.Errorf("ParseVersionSpec(%q) err = %v", spec, err)
		}
	}
}

// TestVersionSpecNestingError checks that too deep nesting is reported as
// such, not as an unexpected parenthesis.
func TestVersionSpecNestingError(t *testing.T) {
	deep := strings.Repeat("(", maxVersionSpecDepth+1) + "1.0" + strings.Repeat(",>0)", maxVersionSpecDepth+1)
	_, err := ParseVersionSpec(deep)
	if err == nil || !strings.Contains(err.Error(), "nested too deeply") {
		t.Errorf("err = %v", err)
	}
}
