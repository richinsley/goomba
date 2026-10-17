package spec

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

// Cases from libmamba tests/src/specs/test_version.cpp ("VersionPartAtom").
func TestVersionPartAtom(t *testing.T) {
	mk := func(n uint64, l string) *atom {
		a := mkSection(nil, []tp{{{n, l}}})
		return &a[0]
	}
	// No literal, lowercase.
	if cmpAtom(mk(1, ""), mk(1, "")) != 0 || cmpAtom(mk(1, "dev"), mk(1, "DEV")) != 0 {
		t.Error("equal atoms compare unequal")
	}
	if cmpAtom(mk(1, ""), mk(2, "dev")) >= 0 || cmpAtom(mk(2, "dev"), mk(1, "")) <= 0 {
		t.Error("numerals compare wrong")
	}
	if cmpAtom(mk(1, "dev"), mk(1, "a")) >= 0 || cmpAtom(mk(1, "a"), mk(1, "dev")) <= 0 {
		t.Error("literals compare wrong")
	}
	sorted := []*atom{
		mk(1, "*"), mk(1, "dev"), mk(1, "_"), mk(1, "a"), mk(1, "alpha"), mk(1, "b"),
		mk(1, "beta"), mk(1, "c"), mk(1, "r"), mk(1, "rc"), mk(1, ""), mk(1, "post"),
	}
	for i := 1; i < len(sorted); i++ {
		if c := cmpAtom(sorted[i-1], sorted[i]); c >= 0 {
			t.Errorf("atom %d{%q} !< %d{%q}", i-1, sorted[i-1].lit, i, sorted[i].lit)
		}
	}
}

func partString(p []atom) string {
	var b strings.Builder
	formatSection(&b, p, -1, fmtNormal)
	return b.String()
}

// Cases from libmamba tests/src/specs/test_version.cpp ("VersionPartAtom::str",
// "VersionPart", "VersionPart::str").
func TestVersionPart(t *testing.T) {
	if s := partString(mkPart(false, tp{{1, "dev"}})); s != "1dev" {
		t.Errorf("atom str = %q", s)
	}
	if s := partString(mkPart(false, tp{{2, ""}})); s != "2" {
		t.Errorf("atom str = %q", s)
	}

	eq := []struct{ a, b []atom }{
		{mkPart(false, tp{{1, "dev"}}), mkPart(false, tp{{1, "dev"}})},
		{mkPart(false, tp{{1, "dev"}}), mkPart(false, tp{{1, "dev"}, {0, ""}})},
		{mkPart(false, tp{{1, "dev"}, {2, ""}}), mkPart(false, tp{{1, "dev"}, {2, ""}})},
		{mkPart(true, tp{{0, "dev"}, {2, ""}}), mkPart(false, tp{{0, "dev"}, {2, ""}})},
	}
	for i, c := range eq {
		if cmpParts(c.a, c.b) != 0 {
			t.Errorf("eq case %d unequal", i)
		}
	}
	if cmpParts(mkPart(false, tp{{0, "dev"}}), mkPart(false, tp{{0, "dev"}, {2, ""}})) == 0 {
		t.Error("{0dev} == {0dev,2}")
	}
	sorted := [][]atom{
		mkPart(false, tp{{0, ""}}),
		mkPart(false, tp{{1, "dev"}, {0, "alpha"}}),
		mkPart(false, tp{{1, "dev"}}),
		mkPart(false, tp{{1, "dev"}, {1, "dev"}}),
		mkPart(false, tp{{2, "dev"}, {1, "dev"}}),
		mkPart(false, tp{{2, ""}}),
		mkPart(false, tp{{2, ""}, {0, "post"}}),
	}
	for i := 1; i < len(sorted); i++ {
		if cmpParts(sorted[i-1], sorted[i]) >= 0 {
			t.Errorf("part %d !< part %d", i-1, i)
		}
	}

	strs := []struct {
		p    []atom
		want string
	}{
		{mkPart(false, tp{{1, "dev"}}), "1dev"},
		{mkPart(false, tp{{1, "dev"}, {2, ""}}), "1dev2"},
		{mkPart(false, tp{{1, "dev"}, {2, "foo"}, {33, "bar"}}), "1dev2foo33bar"},
		{mkPart(false, tp{{0, "dev"}, {2, ""}}), "0dev2"},
		{mkPart(true, tp{{0, "dev"}, {2, ""}}), "dev2"},
		{mkPart(true, tp{{0, "dev"}}), "dev"},
		{mkPart(true, tp{{0, ""}}), "0"},
	}
	for _, c := range strs {
		if got := partString(c.p); got != c.want {
			t.Errorf("part str = %q, want %q", got, c.want)
		}
	}
}

// Cases from libmamba tests/src/specs/test_version.cpp ("Version cmp").
func TestVersionCmp(t *testing.T) {
	v := mkVer(0, []tp{{{1, "post"}}}, nil)
	if countParts(v.ver()) != 1 || len(v.ver()) != 1 || v.ver()[0].lit != "post" || v.ver()[0].num != 1 {
		t.Fatal("bad construction")
	}
	eq := []struct{ a, b Version }{
		{mkVer(0, []tp{{{1, "post"}}}, nil), mkVer(0, []tp{{{1, "post"}}}, nil)},
		{mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "a"}, {}}}, nil)},
		{mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "a"}}, {{}}}, nil)},
		{mkVer(0, []tp{{{1, "a"}, {}, {}}}, nil), mkVer(0, []tp{{{1, "a"}}, {{}}}, nil)},
	}
	for i, c := range eq {
		if c.a.Compare(c.b) != 0 || c.b.Compare(c.a) != 0 {
			t.Errorf("eq case %d unequal", i)
		}
	}
	less := []struct{ a, b Version }{
		{mkVer(0, []tp{{{2, "post"}}}, nil), mkVer(1, []tp{{{1, "dev"}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{0, "dev"}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{0, ""}}, {{0, "dev"}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, "post"}}}, nil), mkVer(0, []tp{{{2, "dev"}}}, nil)},
		{mkVer(0, []tp{{{2, ""}, {0, ""}}}, nil), mkVer(0, []tp{{{11, ""}, {0, ""}, {0, "post"}}}, nil)},
	}
	for i, c := range less {
		if c.a.Compare(c.b) >= 0 || c.b.Compare(c.a) <= 0 {
			t.Errorf("less case %d: %s !< %s", i, c.a, c.b)
		}
	}
}

// Cases from libmamba tests/src/specs/test_version.cpp ("Version starts_with").
func TestVersionStartsWith(t *testing.T) {
	positive := []struct{ prefix, ver Version }{
		{Version{}, Version{}},
		{mkVer(0, []tp{{{1, "a"}, {2, "post"}}}, nil), mkVer(0, []tp{{{1, "a"}, {2, "post"}}}, nil)},
		{mkVer(0, []tp{{{1, "a"}, {2, "post"}}}, nil), mkVer(0, []tp{{{1, "a"}, {2, "post"}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{3, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, "a"}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, "a"}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{0, "a"}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, "post"}}}, nil)},
		{mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "a"}}, {{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "a"}}, {{1, "post"}, {3, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{0, ""}}, {{0, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{mkVer(0, []tp{{{0, ""}}}, nil), mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, nil)},
		{mkVer(0, []tp{{{0, ""}}, {{4, ""}}}, nil), mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, nil)},
		{mkVer(0, []tp{{{0, ""}}, {{4, ""}}}, nil), mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, "p"}, {1, ""}}}, nil)},
		{mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, "p"}}}, nil), mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, "p"}, {1, ""}}}, nil)},
		{mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, nil), mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, []tp{{{1, ""}}, {{3, ""}}})},
		{mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, []tp{{{1, ""}}}), mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, []tp{{{1, ""}}, {{3, ""}}})},
	}
	for _, c := range positive {
		if !c.ver.StartsWith(c.prefix) {
			t.Errorf("%s should start with %s", c.ver, c.prefix)
		}
	}
	negative := []struct{ prefix, ver Version }{
		{mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(1, []tp{{{1, "a"}}}, nil)},
		{mkVer(0, []tp{{{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{2, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{3, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "b"}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{1, "a"}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, "b"}}}, nil)},
		{mkVer(0, []tp{{{1, ""}}, {{1, "a"}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
	}
	for _, c := range negative {
		if c.ver.StartsWith(c.prefix) {
			t.Errorf("%s should not start with %s", c.ver, c.prefix)
		}
	}
}

// Cases from libmamba tests/src/specs/test_version.cpp ("Version compatible_with").
func TestVersionCompatibleWith(t *testing.T) {
	type tc struct {
		level        int
		older, newer Version
	}
	positive := []tc{
		{0, Version{}, Version{}},
		{1, Version{}, Version{}},
		{1, mkVer(0, []tp{{{1, "a"}, {2, "post"}}}, nil), mkVer(0, []tp{{{1, "a"}, {2, "post"}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{2, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{3, ""}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, "a"}}}, nil)},
		{0, mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{0, mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "b"}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}, {{1, "a"}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, "b"}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, "post"}}}, nil)},
		{0, mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "a"}}, {{1, ""}}}, nil)},
		{0, mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(0, []tp{{{1, "a"}}, {{1, "post"}, {3, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}, {{1, "a"}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}, {{0, ""}}, {{0, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{3, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{3, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{3, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{4, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{3, ""}}}, nil)},
	}
	for _, c := range positive {
		if !c.newer.compatibleWith(c.older, c.level) {
			t.Errorf("level %d: %s should be compatible with %s", c.level, c.newer, c.older)
		}
	}
	negative := []tc{
		{0, mkVer(0, []tp{{{1, "a"}}}, nil), mkVer(1, []tp{{{1, "a"}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, "a"}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{0, "a"}}}, nil)},
		{0, mkVer(0, []tp{{{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{2, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{3, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{3, ""}}, {{1, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{3, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{3, "a"}}, {{0, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{3, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{3, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}, {{2, ""}}, {{3, ""}}}, nil), mkVer(0, []tp{{{2, "a"}}}, nil)},
		{1, mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{2, mkVer(0, []tp{{{1, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{0, mkVer(0, []tp{{{1, ""}}, {{2, ""}}}, nil), mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
	}
	for _, c := range negative {
		if c.newer.compatibleWith(c.older, c.level) {
			t.Errorf("level %d: %s should not be compatible with %s", c.level, c.newer, c.older)
		}
	}
}

// Cases from libmamba tests/src/specs/test_version.cpp ("Version::str").
func TestVersionStr(t *testing.T) {
	cases := []struct {
		v      Version
		levels []string // to_string() then to_string(1), to_string(2), ...
		glob   string
	}{
		{
			mkVer(0, []tp{{{11, "a"}, {0, "post"}}, {{3, ""}}, {{4, "dev"}}}, nil),
			[]string{"11a0post.3.4dev", "11a0post", "11a0post.3", "11a0post.3.4dev", "11a0post.3.4dev.0", "11a0post.3.4dev.0.0"},
			"",
		},
		{
			mkVer(1, []tp{{{11, "a"}, {0, ""}}, {{3, ""}}, {{4, "dev"}}}, nil),
			[]string{"1!11a0.3.4dev", "1!11a0", "1!11a0.3", "1!11a0.3.4dev", "1!11a0.3.4dev.0"},
			"",
		},
		{
			mkVer(1, []tp{{{11, "a"}, {0, ""}}, {{3, ""}}, {{4, "dev"}}}, []tp{{{1, ""}}, {{2, ""}}}),
			[]string{"1!11a0.3.4dev+1.2", "1!11a0+1", "1!11a0.3+1.2", "1!11a0.3.4dev+1.2.0", "1!11a0.3.4dev.0+1.2.0.0"},
			"",
		},
		{
			mkVer(0, []tp{{{0, "*"}}, {{1, ""}}, {{0, "*"}}}, nil),
			[]string{"0*.1.0*", "0*", "0*.1", "0*.1.0*", "0*.1.0*.0"},
			"*.1.*",
		},
	}
	for _, c := range cases {
		if got := c.v.String(); got != c.levels[0] {
			t.Errorf("String() = %q, want %q", got, c.levels[0])
		}
		for lvl := 1; lvl < len(c.levels); lvl++ {
			if got := c.v.stringLevel(lvl); got != c.levels[lvl] {
				t.Errorf("stringLevel(%d) = %q, want %q", lvl, got, c.levels[lvl])
			}
		}
		if c.glob != "" {
			if got := c.v.globString(); got != c.glob {
				t.Errorf("globString() = %q, want %q", got, c.glob)
			}
		}
	}
}

// Cases from libmamba tests/src/specs/test_version.cpp ("Version::parse"),
// themselves from conda tests/models/test_version.py.
func TestParseVersion(t *testing.T) {
	sorted := []struct {
		raw  string
		want Version
	}{
		{"0.4", mkVer(0, []tp{{{0, ""}}, {{4, ""}}}, nil)},
		{"0.4.0", mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{0, ""}}}, nil)},
		{"0.4.1a.vc11", mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, "a"}}, {{0, "vc"}, {11, ""}}}, nil)},
		{"0.4.1.rc", mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}, {{0, "rc"}}}, nil)},
		{"0.4.1.vc11", mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}, {{0, "vc"}, {11, ""}}}, nil)},
		{"0.4.1", mkVer(0, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, nil)},
		{"0.5*", mkVer(0, []tp{{{0, ""}}, {{5, "*"}}}, nil)},
		{"0.5a1", mkVer(0, []tp{{{0, ""}}, {{5, "a"}, {1, ""}}}, nil)},
		{"0.5b3", mkVer(0, []tp{{{0, ""}}, {{5, "b"}, {3, ""}}}, nil)},
		{"0.5C1", mkVer(0, []tp{{{0, ""}}, {{5, "c"}, {1, ""}}}, nil)},
		{"0.5z", mkVer(0, []tp{{{0, ""}}, {{5, "z"}}}, nil)},
		{"0.5za", mkVer(0, []tp{{{0, ""}}, {{5, "za"}}}, nil)},
		{"0.5", mkVer(0, []tp{{{0, ""}}, {{5, ""}}}, nil)},
		{"0.5_5", mkVer(0, []tp{{{0, ""}}, {{5, ""}}, {{5, ""}}}, nil)},
		{"0.5-5", mkVer(0, []tp{{{0, ""}}, {{5, ""}}, {{5, ""}}}, nil)},
		{"0.9.6", mkVer(0, []tp{{{0, ""}}, {{9, ""}}, {{6, ""}}}, nil)},
		{"0.960923", mkVer(0, []tp{{{0, ""}}, {{960923, ""}}}, nil)},
		{"1.0", mkVer(0, []tp{{{1, ""}}, {{0, ""}}}, nil)},
		{"1.0.4a3", mkVer(0, []tp{{{1, ""}}, {{0, ""}}, {{4, "a"}, {3, ""}}}, nil)},
		{"1.0.4b1", mkVer(0, []tp{{{1, ""}}, {{0, ""}}, {{4, "b"}, {1, ""}}}, nil)},
		{"1.0.4", mkVer(0, []tp{{{1, ""}}, {{0, ""}}, {{4, ""}}}, nil)},
		{"1.1dev1", mkVer(0, []tp{{{1, ""}}, {{1, "dev"}, {1, ""}}}, nil)},
		{"1.1_", mkVer(0, []tp{{{1, ""}}, {{1, "_"}}}, nil)},
		{"1.1a1", mkVer(0, []tp{{{1, ""}}, {{1, "a"}, {1, ""}}}, nil)},
		{"1.1.dev1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}, {{0, "dev"}, {1, ""}}}, nil)},
		{"1.1.a1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}, {{0, "a"}, {1, ""}}}, nil)},
		{"1.1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}}, nil)},
		{"1.1.post1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}, {{0, "post"}, {1, ""}}}, nil)},
		{"1.1.1dev1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}, {{1, "dev"}, {1, ""}}}, nil)},
		{"1.1.1rc1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}, {{1, "rc"}, {1, ""}}}, nil)},
		{"1.1.1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}, {{1, ""}}}, nil)},
		{"1.1.1post1", mkVer(0, []tp{{{1, ""}}, {{1, ""}}, {{1, "post"}, {1, ""}}}, nil)},
		{"1.1post1", mkVer(0, []tp{{{1, ""}}, {{1, "post"}, {1, ""}}}, nil)},
		{"2g6", mkVer(0, []tp{{{2, "g"}, {6, ""}}}, nil)},
		{"2.0b1pr0", mkVer(0, []tp{{{2, ""}}, {{0, "b"}, {1, "pr"}, {0, ""}}}, nil)},
		{"2.2be.ta29", mkVer(0, []tp{{{2, ""}}, {{2, "be"}}, {{0, "ta"}, {29, ""}}}, nil)},
		{"2.2be5ta29", mkVer(0, []tp{{{2, ""}}, {{2, "be"}, {5, "ta"}, {29, ""}}}, nil)},
		{"2.2beta29", mkVer(0, []tp{{{2, ""}}, {{2, "beta"}, {29, ""}}}, nil)},
		{"2.2.0.1", mkVer(0, []tp{{{2, ""}}, {{2, ""}}, {{0, ""}}, {{1, ""}}}, nil)},
		{"3.1.1.6", mkVer(0, []tp{{{3, ""}}, {{1, ""}}, {{1, ""}}, {{6, ""}}}, nil)},
		{"3.2.p.r0", mkVer(0, []tp{{{3, ""}}, {{2, ""}}, {{0, "p"}}, {{0, "r"}, {0, ""}}}, nil)},
		{"3.2.pr0", mkVer(0, []tp{{{3, ""}}, {{2, ""}}, {{0, "pr"}, {0, ""}}}, nil)},
		{"3.2.pr.1", mkVer(0, []tp{{{3, ""}}, {{2, ""}}, {{0, "pr"}}, {{1, ""}}}, nil)},
		{"5.5.kw", mkVer(0, []tp{{{5, ""}}, {{5, ""}}, {{0, "kw"}}}, nil)},
		{"11g", mkVer(0, []tp{{{11, "g"}}}, nil)},
		{"14.3.1", mkVer(0, []tp{{{14, ""}}, {{3, ""}}, {{1, ""}}}, nil)},
		{"14.3.1.post26.g9d75ca2", mkVer(0, []tp{{{14, ""}}, {{3, ""}}, {{1, ""}}, {{0, "post"}, {26, ""}}, {{0, "g"}, {9, "d"}, {75, "ca"}, {2, ""}}}, nil)},
		{"1996.07.12", mkVer(0, []tp{{{1996, ""}}, {{7, ""}}, {{12, ""}}}, nil)},
		{"1!0.4.1", mkVer(1, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, nil)},
		{"1!3.1.1.6", mkVer(1, []tp{{{3, ""}}, {{1, ""}}, {{1, ""}}, {{6, ""}}}, nil)},
		{"2!0.4.1", mkVer(2, []tp{{{0, ""}}, {{4, ""}}, {{1, ""}}}, nil)},
	}
	for i, c := range sorted {
		got := mustVersion(t, c.raw)
		if got.Compare(c.want) != 0 || !sameStructure(got, c.want) {
			t.Errorf("ParseVersion(%q) = %s, want %s", c.raw, got, c.want)
		}
		// std::is_sorted: neighbours may be equal ("0.4" and "0.4.0").
		if i > 0 && sorted[i-1].want.Compare(c.want) > 0 {
			t.Errorf("%q > %q", sorted[i-1].raw, c.raw)
		}
	}

	if mustVersion(t, "0.0").Compare(Version{}) != 0 {
		t.Error("0.0 != Version{}")
	}
	equal := [][2]string{
		{"0.4.1.rc", "  0.4.1.RC  "},
		{"  0.4.rc  ", "0.4.RC"},
		{"0.4", "0.4.0"},
		{"0.4.a1", "0.4.0a1"},
	}
	for _, c := range equal {
		if mustVersion(t, c[0]).Compare(mustVersion(t, c[1])) != 0 {
			t.Errorf("%q != %q", c[0], c[1])
		}
	}
	notEqual := [][2]string{{"0.4", "0.4.1"}, {"0.4.a1", "0.4.1a1"}}
	for _, c := range notEqual {
		if mustVersion(t, c[0]).Compare(mustVersion(t, c[1])) == 0 {
			t.Errorf("%q == %q", c[0], c[1])
		}
	}

	// Implicit zeros.
	v := mustVersion(t, "0.4.a1")
	third := v.ver()[2:]
	if third[0].flags&flagImplicitZero == 0 {
		t.Error("0.4.a1: third part should have an implicit zero")
	}
	for _, s := range []string{"0.4.a1", "g56ffd88f"} {
		if got := mustVersion(t, s).String(); got != s {
			t.Errorf("String(%q) = %q", s, got)
		}
	}

	// '*' is a valid version character with special ordering.
	stars := []struct {
		raw  string
		want Version
	}{
		{"*", mkVer(0, []tp{{{0, "*"}}}, nil)},
		{"*.*", mkVer(0, []tp{{{0, "*"}}, {{0, "*"}}}, nil)},
		{"*.*.*", mkVer(0, []tp{{{0, "*"}}, {{0, "*"}}, {{0, "*"}}}, nil)},
		{"*.*.2023.12", mkVer(0, []tp{{{0, "*"}}, {{0, "*"}}, {{2023, ""}}, {{12, ""}}}, nil)},
		{"1.*", mkVer(0, []tp{{{1, ""}}, {{0, "*"}}}, nil)},
	}
	for _, c := range stars {
		got := mustVersion(t, c.raw)
		if got.Compare(c.want) != 0 || !sameStructure(got, c.want) {
			t.Errorf("ParseVersion(%q) = %s", c.raw, got)
		}
	}
}

// Cases from libmamba tests/src/specs/test_version.cpp ("Version::parse negative").
func TestParseVersionNegative(t *testing.T) {
	bad := []string{
		// Wrong epoch
		"!1.1", "-1!1.1", "foo!1.1", "0post1!1.1",
		// Empty parts
		"", "  ", "!2.2", "0!", "!", "1.", "1..1", "5.5..mw", "1.2post+", "1!+1.1",
		// Repeated delimiters
		"5.5++", "5.5+1+0.0", "1!2!3.0",
		// '-' and '_' delimiters not allowed together.
		"1-1_1",
		// Forbidden characters
		"3.5&1", "3.5|1",
		// Added for goomba: numbers must fit in 64 bits, non-ASCII is invalid.
		"18446744073709551616", "1.18446744073709551616", "18446744073709551616!1",
		"1.K", "1.é",
	}
	for _, s := range bad {
		_, err := ParseVersion(s)
		if err == nil {
			t.Errorf("ParseVersion(%q) succeeded", s)
			continue
		}
		var pe *ParseError
		if !errors.Is(err, ErrVersion) || !errors.As(err, &pe) || pe.Input != s {
			t.Errorf("ParseVersion(%q) error %v is not a version ParseError", s, err)
		}
	}
	if _, err := ParseVersion("18446744073709551615.1"); err != nil {
		t.Errorf("max uint64 rejected: %v", err)
	}
}

func checkStrictlySorted(t *testing.T, strs []string) {
	t.Helper()
	vs := make([]Version, len(strs))
	for i, s := range strs {
		vs[i] = mustVersion(t, s)
	}
	for i := 1; i < len(vs); i++ {
		if vs[i-1].Compare(vs[i]) >= 0 {
			t.Errorf("%q !< %q", strs[i-1], strs[i])
		}
		if vs[i].Compare(vs[i-1]) <= 0 {
			t.Errorf("%q !> %q", strs[i], strs[i-1])
		}
	}
	// Sorting a shuffled copy restores the order.
	shuffled := append([]Version(nil), vs...)
	for i := range shuffled {
		j := (i*7919 + 13) % len(shuffled)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i].Compare(shuffled[j]) < 0 })
	for i := range vs {
		if shuffled[i].Compare(vs[i]) != 0 {
			t.Errorf("sorted[%d] = %s, want %s", i, shuffled[i], vs[i])
		}
	}
}

// Cases from libmamba tests/src/specs/test_version.cpp ("parse_openssl"),
// from conda tests/models/test_version.py.
func TestVersionOpenSSL(t *testing.T) {
	checkStrictlySorted(t, []string{
		"1.0.1dev", "1.0.1_", "1.0.1a", "1.0.1b", "1.0.1c", "1.0.1d", "1.0.1r", "1.0.1rc",
		"1.0.1rc1", "1.0.1rc2", "1.0.1s", "1.0.1", "1.0.1post.a", "1.0.1post.b",
		"1.0.1post.z", "1.0.1post.za", "1.0.2",
	})
}

// Cases from libmamba tests/src/specs/test_version.cpp ("parse_pep440"),
// from conda tests/models/test_version.py.
func TestVersionPEP440(t *testing.T) {
	checkStrictlySorted(t, []string{
		"1.0a1", "1.0a2.dev456", "1.0a12.dev456", "1.0a12", "1.0b1.dev456", "1.0b2",
		"1.0b2.post345.dev456", "1.0b2.post345", "1.0c1.dev456", "1.0c1", "1.0c3", "1.0rc2",
		"1.0.dev456", "1.0", "1.0.post456.dev34", "1.0.post456", "1.1.dev1", "1.2.r32+123456",
		"1.2.rev33+123456", "1.2+abc", "1.2+abc123def", "1.2+abc123", "1.2+123abc",
		"1.2+123abc456", "1.2+1234.abc", "1.2+123456",
		"1!1.0a1", "1!1.0a2.dev456", "1!1.0a12.dev456", "1!1.0a12", "1!1.0b1.dev456", "1!1.0b2",
		"1!1.0b2.post345.dev456", "1!1.0b2.post345", "1!1.0c1.dev456", "1!1.0c1", "1!1.0c3",
		"1!1.0rc2", "1!1.0.dev456", "1!1.0", "1!1.0.post456.dev34", "1!1.0.post456",
		"1!1.1.dev1", "1!1.2.r32+123456", "1!1.2.rev33+123456", "1!1.2+abc", "1!1.2+abc123def",
		"1!1.2+abc123", "1!1.2+123abc", "1!1.2+123abc456", "1!1.2+1234.abc", "1!1.2+123456",
	})
}

// Rattler crates/rattler_conda_version/src/version/mod.rs
// (dev_is_only_special_for_exact_runs), as a cross-check.
func TestVersionDevPostExactRuns(t *testing.T) {
	less := [][2]string{
		{"1.2dev", "1.2.devdev"},
		{"1.2dev", "1.2devdev"},
		{"1.2postpost", "1.2post"},
	}
	for _, c := range less {
		if mustVersion(t, c[0]).Compare(mustVersion(t, c[1])) >= 0 {
			t.Errorf("%q !< %q", c[0], c[1])
		}
	}
}

func TestVersionRoundTrip(t *testing.T) {
	for _, s := range []string{
		"1.2.3", "1!2.0", "1.0.post1", "3.7b_", "1.2+local.3", "1-2", "1_2", "1.1_", "_",
		"1__", "a1.b2", "0*", "*.1.*", "1.0a_", "18446744073709551615", "1-1_",
	} {
		v := mustVersion(t, s)
		w := mustVersion(t, v.String())
		if v.Compare(w) != 0 || !sameStructure(v, w) {
			t.Errorf("%q: String() = %q reparses differently", s, v.String())
		}
	}
	if s := (Version{}).String(); s != "0" {
		t.Errorf("zero Version String() = %q", s)
	}
}
