package repodata

import (
	"strings"
	"testing"
)

func minor(t *testing.T, s string) *pythonMinor {
	t.Helper()
	p, err := parsePythonMinor(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Cases from libmamba tests/src/core/test_shard_python_minor_prefilter.cpp
// "matches_python_minor".
func TestMatchesPythonMinorUpstream(t *testing.T) {
	cases := []struct {
		dep, minor string
		want       bool
	}{
		// non-python dependency is not filtered
		{"numpy >=1.0", "3.12", true},
		{"libstdcxx-ng >=12", "3.12", true},
		// name starting with python but not the python package
		{"python_abi 3.12 1_cp312", "3.12", true},
		// python version range matches requested minor
		{"python >=3.12,<3.13", "3.12", true},
		{"python >=3.12", "3.12", true},
		// python version range does not match requested minor
		{"python >=3.12,<3.13", "3.11", false},
		// libmamba: false. 3.13.0rc1 is in goomba's 3.13 series and
		// satisfies "<3.13"; conda-forge's "<3.13.0a0" is still filtered.
		{"python >=3.12,<3.13", "3.13", true},
		{"python >=3.12,<3.13.0a0", "3.13", false},
		// exact three-token conda pin matches requested minor
		{"python 3.7.12 0_73_pypy", "3.7", true},
		// two-token exact pin matches requested minor
		{"python 3.7.12", "3.7", true},
		// exact pin does not match different minor
		{"python 3.8.0", "3.7", false},
		// leading whitespace on dependency line
		{"  python >=3.12,<3.13", "3.12", true},
		// unparsable python dependency does not filter (passes)
		{"python ,,not-a-valid-spec,,", "3.12", true},
		// namespaced python pin
		{"conda-forge::python 3.7.12 0_73_pypy", "3.7", true},
		// only python in range with no upper bound
		{"python", "3.12", true},
	}
	for _, c := range cases {
		if got := minor(t, c.minor).matches(c.dep); got != c.want {
			t.Errorf("matches(%q, %s) = %v, want %v", c.dep, c.minor, got, c.want)
		}
	}
}

// Further cases: the forms conda-forge uses, and the conservative choices.
func TestMatchesPythonMinor(t *testing.T) {
	cases := []struct {
		dep, minor string
		want       bool
	}{
		{"python >=3.12,<3.13.0a0", "3.12", true},
		{"python >=3.12,<3.13.0a0", "3.13", false},
		{"python >=3.12,<3.13.0a0", "3.11", false},
		{"python >=3.12,<3.13.0a0 *_cpython", "3.12", true},
		{"python >=3.9", "3.12", true},
		{"python >=3.9", "3.8", false},
		{"python >=3.9", "2.7", false},
		{"python >=3.9", "4.0", true},
		{"python 3.12.* *_cpython", "3.12", true},
		{"python 3.12.* *_cpython", "3.11", false},
		{"python 3.1.*", "3.12", false}, // component-wise, not a string prefix
		{"python 3.1*", "3.12", false},
		{"python 3.*", "3.12", true},
		{"python 2.7.*", "3.12", false},
		{"python 2.7.*|>=3.5", "3.12", true},
		{"python >=2.7,!=3.0.*,!=3.1.*,!=3.2.*", "3.12", true},
		{"python >=2.7,!=3.0.*,!=3.1.*,!=3.2.*", "3.1", false},
		{"python !=3.12.*", "3.12", false},
		{"python !=3.12", "3.12", true},
		{"python <3.12", "3.12", true}, // 3.12.0rc1 < 3.12 is in the series
		{"python <3.12.0a0", "3.12", false},
		// The series includes the pre-releases of X.Y.0 from X.Y.0a0 up
		// (WP7 review: python=3.12 with a channel holding only 3.12.0rc1).
		{"python >=3.8,<3.12", "3.12", true},
		{"python >=3.10,<3.12.0a0", "3.12", false},
		{"python <3.12.0a1", "3.12", true},
		{"python <3.12.0rc1", "3.12", true},
		{"python <=3.12.0a0", "3.12", true},
		{"python <3.12.0dev0", "3.12", false}, // dev builds sort below X.Y.0a0
		{"python <3.12.0b1,>=3.12.0a0", "3.12", true},
		{"python 3.12.0rc1", "3.12", true},
		{"python ==3.12.0a2", "3.12", true},
		{"python >3.12.0dev0", "3.12", true},
		// "!=V*" and "==V*" (no dot) compare with the literal version "V*"
		// in package spec and libmamba: not understood, kept (WP7 review).
		{"python !=3.12*", "3.12", true},
		{"python ==3.12*", "3.12", true},
		{"python !=3.12.*", "3.12", false},
		{"python 3.12*", "3.12", true},
		{"python 3.13*", "3.12", false},
		// A part whose number differs decides, whatever letters follow.
		{"python <4.dev0", "3.12", true},
		{"python >=4.dev0", "3.12", false},
		{"python <3.13a0", "3.12", true},
		{"python >3.13a0", "3.12", false},
		{"python >=3.13a0", "3.12", false},
		{"python <3.11rc1", "3.12", false},
		{"python <=3.11rc1", "3.12", false},
		{"python >3.11rc1", "3.12", true},
		{"python <2a", "3.12", false},
		{"python >=3.12post1", "3.12", true}, // not understood: same number, then letters
		{"python <3.12.1", "3.12", true},
		{"python <=3.12", "3.12", true},
		{"python <=3.11.99", "3.12", false},
		{"python >3.12", "3.12", true}, // 3.12.1 > 3.12
		{"python >3.13", "3.12", false},
		{"python ~=3.12", "3.12", true},
		{"python ~=3.13", "3.12", false},
		{"python ==3.12.5", "3.12", true},
		{"python ==3.12.0rc1", "3.12", true}, // libmamba relaxes "==X.Y.Z" to "==X.Y"
		{"python ==3.11.5", "3.12", false},
		{"python =3.12", "3.12", true},
		{"python =3.12", "3.11", false},
		{"python 3", "3.12", true}, // bare: exact 3 or fuzzy 3.*, whichever keeps more
		{"python 3", "2.7", false},
		{"python 3.12", "3.12", true},
		{"python >=3.8,<3.10", "3.10", true}, // 3.10.0rc1
		{"python >=3.8,<3.10.0a0", "3.10", false},
		{"python>=3.12.1", "3.12", true},          // libmamba drops this; X.Y.5 satisfies it
		{"python >=3.12.5,<3.12.3", "3.12", true}, // conservative: terms are checked one by one
		{"python 3.12.0post1", "3.12", true},
		{"python >3.12post1", "3.12", true}, // not understood: 3.12post2 starts with 3.12
		{"python <3.12post1", "3.12", true},
		{"python <3", "3.12", false},
		{"python * *_cpython", "3.12", true},
		{"python *", "3.12", true},
		// Not the canonical "python <spec> [<build>]" form: always kept.
		{"python=3.11", "3.12", true},
		{"python==3.11.5", "3.12", true},
		{"python<3", "3.12", true},
		{"python >= 3.8, < 3.10", "3.10", true},
		{"PYTHON <3", "3.12", true},
		{"conda-forge::python <3", "3.12", true},
		{"  python <3", "3.12", true},
		{"python  <3", "3.12", true},
		{"python <3 ", "3.12", true},
		{"python\t<3", "3.12", true},
		// Inside the version spec, not understood: keep.
		{"python[version='<3']", "3.12", true},
		{"python (>=3.8,<3.9)|3.12", "3.12", true},
		{"python 1!3.8", "3.12", true},
		{"python 3.8+local", "3.12", true},
		{"python 3.8_1", "3.12", true},
		{">=3.8.*", "3.12", true},
		{"python >=3.8.*", "3.12", true},
		{"python 3.8 build extra", "3.12", true},
		{"python 99999999999999999999999.1", "3.12", true},
		{"python <3.8|", "3.12", true},
	}
	for _, c := range cases {
		if got := minor(t, c.minor).matches(c.dep); got != c.want {
			t.Errorf("matches(%q, %s) = %v, want %v", c.dep, c.minor, got, c.want)
		}
	}
}

func TestParsePythonMinor(t *testing.T) {
	if p := minor(t, "3.12"); p.major != 3 || p.minor != 12 {
		t.Errorf("got %+v", p)
	}
	for _, s := range []string{"", "3", "3.", ".12", "3.12.1", "3.x", "a.b", "-3.1", "3.99999999999999999999"} {
		if _, err := parsePythonMinor(s); err == nil {
			t.Errorf("parsePythonMinor(%q) accepted", s)
		}
	}
	if _, err := (Options{CacheDir: "x", Platform: "linux-64", PythonMinor: "3"}).settings(); err == nil {
		t.Error("Options accepted PythonMinor \"3\"")
	}
}

// The lite version parser orders versions as libmamba does, on the inputs
// it accepts. Cases from libmamba tests/src/specs/test_version.cpp
// "Version::parse" (sorted_version); entries outside the lite grammar ('*',
// '_', '-', epochs) must be rejected and are skipped.
func TestLiteVersionOrdering(t *testing.T) {
	sorted := []string{
		"0.4", "0.4.0", "0.4.1a.vc11", "0.4.1.rc", "0.4.1.vc11", "0.4.1", "0.5*", "0.5a1", "0.5b3", "0.5C1",
		"0.5z", "0.5za", "0.5", "0.5_5", "0.5-5", "0.9.6", "0.960923", "1.0", "1.0.4a3", "1.0.4b1", "1.0.4",
		"1.1dev1", "1.1_", "1.1a1", "1.1.dev1", "1.1.a1", "1.1", "1.1.post1", "1.1.1dev1", "1.1.1rc1", "1.1.1",
		"1.1.1post1", "1.1post1", "2g6", "2.0b1pr0", "2.2be.ta29", "2.2be5ta29", "2.2beta29", "2.2.0.1",
		"3.1.1.6", "3.2.p.r0", "3.2.pr0", "3.2.pr.1", "5.5.kw", "11g", "14.3.1", "14.3.1.post26.g9d75ca2",
		"1996.07.12", "1!0.4.1", "1!3.1.1.6", "2!0.4.1",
	}
	cmp := func(va, vb liteVersion) int {
		for i := 0; i < max(len(va), len(vb)); i++ {
			if c := comparePart(va.part(i), vb.part(i)); c != 0 {
				return c
			}
		}
		return 0
	}
	var parsed []liteVersion
	var raws []string
	for _, s := range sorted {
		v, ok := parseLiteVersion(s)
		if strings.ContainsAny(s, "*_-!") {
			if ok {
				t.Errorf("parseLiteVersion(%q) accepted", s)
			}
			continue
		}
		if !ok {
			t.Errorf("parseLiteVersion(%q) rejected", s)
			continue
		}
		parsed, raws = append(parsed, v), append(raws, s)
	}
	for i := range parsed {
		for j := i + 1; j < len(parsed); j++ {
			if c := cmp(parsed[i], parsed[j]); c > 0 {
				t.Errorf("%s > %s", raws[i], raws[j])
			} else if c == 0 && !(raws[i] == "0.4" && raws[j] == "0.4.0") {
				t.Errorf("%s == %s", raws[i], raws[j])
			}
		}
	}
	if v, _ := parseLiteVersion("0.4.1.RC"); cmp(v, parsed[3]) != 0 {
		t.Error("0.4.1.RC != 0.4.1.rc")
	}
}

// Cases from libmamba tests/src/core/test_shard_python_minor_prefilter.cpp
// "Shards - python minor prefilter": the filter applies while decoding.
func TestShardPythonMinorPrefilter(t *testing.T) {
	run := func(dep string, py *pythonMinor) int {
		spec := recSpec{name: "test-pkg", version: "1.0.0", build: "0", sha256: testSHA, depends: []string{dep}}
		recs, err := decodeShardForTest(t, oneShard(t, spec.omap(t), "test-pkg-1.0.0-0.tar.bz2"), "test-pkg", py)
		if err != nil {
			t.Fatal(err)
		}
		return len(recs)
	}
	cases := []struct {
		name, dep string
		py        *pythonMinor
		want      int
	}{
		// libmamba discards "python >=3.11,<3.12" for 3.12; goomba keeps it,
		// because 3.12.0rc1 satisfies it, and discards conda-forge's
		// "<3.12.0a0" form.
		{"mismatching python minor is discarded before record creation", "python >=3.11,<3.12.0a0", minor(t, "3.12"), 0},
		{"a pre-release of the minor satisfies a bound below it", "python >=3.11,<3.12", minor(t, "3.12"), 1},
		{"matching python minor is retained", "python >=3.12,<3.13", minor(t, "3.12"), 1},
		{"no python minor context does not apply prefilter", "python >=3.11,<3.12", nil, 1},
		{"exact python pin matches requested minor (conda three-token depends)", "python 3.7.12 0_73_pypy", minor(t, "3.7"), 1},
	}
	for _, c := range cases {
		if got := run(c.dep, c.py); got != c.want {
			t.Errorf("%s: %d records, want %d", c.name, got, c.want)
		}
	}
}
