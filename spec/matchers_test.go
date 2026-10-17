package spec

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// Cases from libmamba tests/src/specs/test_glob_spec.cpp.
func TestGlobSpec(t *testing.T) {
	free := GlobSpec{}
	if !free.Contains("") || !free.Contains("hello") || free.String() != "*" || !free.IsAny() || free.IsExact() {
		t.Error("free glob")
	}
	if g := NewGlobSpec("*"); g != free {
		t.Error(`NewGlobSpec("*") != GlobSpec{}`)
	}

	mkl := NewGlobSpec("mkl")
	if !mkl.Contains("mkl") || mkl.Contains("") || mkl.Contains("nomkl") || mkl.Contains("hello") {
		t.Error("mkl contains")
	}
	if mkl.String() != "mkl" || mkl.IsAny() || !mkl.IsExact() {
		t.Error("mkl properties")
	}

	py := NewGlobSpec("*py*")
	for _, s := range []string{"py", "pypy", "cpython-linux-64"} {
		if !py.Contains(s) {
			t.Errorf("*py* should contain %q", s)
		}
	}
	for _, s := range []string{"rust", "hello"} {
		if py.Contains(s) {
			t.Errorf("*py* should not contain %q", s)
		}
	}
	if py.String() != "*py*" || py.IsAny() || py.IsExact() {
		t.Error("*py* properties")
	}

	if NewGlobSpec("py*") != NewGlobSpec("py*") || NewGlobSpec("py*") == NewGlobSpec("pyth*") {
		t.Error("equality")
	}
}

// Cases from libmamba tests/src/util/test_parsers.cpp ("glob_match"), plus
// cases libmamba gets wrong (it anchors each literal at its first
// occurrence).
func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"python", "python", true},
		{"cpython", "python", false},
		{"python", "cpython", false},
		{"python", "", false},

		{"py*", "py", true},
		{"py*", "python", true},
		{"py*", "cpython", false},
		{"py*", "", false},

		{"*37", "python37", true},
		{"*37", "37", true},
		{"*37", "python37-linux64", false},
		{"*37", "", false},

		{"*py*", "python", true},
		{"*py*", "cpython", true},
		{"*py*", "cpy", true},
		{"*py*", "linux", false},
		{"*py*", "", false},

		{"*py*-3*-*-64", "cpython-37-linux-64", true},
		{"*py*-3*-*-64", "python-37-more-linux-64", true},
		{"*py*-3*-*-64", "cpython-37-linux-64-more", false},
		{"*py*-3*-*-64", "", false},

		{"py**", "python", true},
		{"py**", "cpython", false},
		{"**37", "python37", true},
		{"**37", "python37-linux64", false},
		{"**py**", "python", true},
		{"**py**", "linux", false},

		// Added for goomba: repeated literals.
		{"*a", "aa", true},
		{"*_cp311", "py311_cp311_cp311", true},
		{"a*a", "a", false},
		{"a*a", "aa", true},
		{"*", "", true},
		{"", "", true},
		{"", "a", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// Cases from libmamba tests/src/specs/test_build_number_spec.cpp.
func TestBuildNumberSpec(t *testing.T) {
	type tc struct {
		spec BuildNumberSpec
		in   []bool // contains 0, 1, 2
		str  string
	}
	mk := func(op predOp) BuildNumberSpec { return BuildNumberSpec{num: 1, op: op} }
	preds := []tc{
		{BuildNumberSpec{}, []bool{true, true, true}, "=*"},
		{mk(opEqual), []bool{false, true, false}, "=1"},
		{mk(opNotEqual), []bool{true, false, true}, "!=1"},
		{mk(opGreater), []bool{false, false, true}, ">1"},
		{mk(opGreaterEqual), []bool{false, true, true}, ">=1"},
		{mk(opLess), []bool{true, false, false}, "<1"},
		{mk(opLessEqual), []bool{true, true, false}, "<=1"},
	}
	for i, c := range preds {
		for n, want := range c.in {
			if got := c.spec.Contains(uint64(n)); got != want {
				t.Errorf("%s contains %d = %v", c.str, n, got)
			}
		}
		if c.spec.String() != c.str {
			t.Errorf("String = %q, want %q", c.spec.String(), c.str)
		}
		for j := i + 1; j < len(preds); j++ {
			if preds[i].spec == preds[j].spec {
				t.Errorf("%s == %s", preds[i].str, preds[j].str)
			}
		}
	}

	parsed := []struct {
		spec string
		n    uint64
		want bool
	}{
		{"", 0, true}, {"", 1, true}, {"*", 1, true}, {"=*", 1, true},
		{"1", 1, true}, {"=1", 1, true}, {"1", 2, false}, {"=1", 2, false},
		{"!=1", 0, true}, {"!=1", 1, false}, {"!=1", 2, true},
		{">1", 0, false}, {">1", 1, false}, {">1", 2, true},
		{">=1", 0, false}, {">=1", 1, true}, {">=1", 2, true},
		{"<1", 0, true}, {"<1", 1, false}, {"<1", 2, false},
		{"<=1", 0, true}, {"<=1", 1, true}, {"<=1", 2, false},
		{" <= 1 ", 0, true},
	}
	for _, c := range parsed {
		s, err := ParseBuildNumberSpec(c.spec)
		if err != nil {
			t.Errorf("ParseBuildNumberSpec(%q): %v", c.spec, err)
			continue
		}
		if got := s.Contains(c.n); got != c.want {
			t.Errorf("%q contains %d = %v", c.spec, c.n, got)
		}
	}
	for _, bad := range []string{"<2.4", "<", "(3)", "<2+", "7=2+", "@7", "18446744073709551616"} {
		if _, err := ParseBuildNumberSpec(bad); !errors.Is(err, ErrBuildNumberSpec) {
			t.Errorf("ParseBuildNumberSpec(%q) err = %v", bad, err)
		}
	}

	strs := [][2]string{{"=3", "=3"}, {"<2", "<2"}, {"*", "=*"}}
	for _, c := range strs {
		s, _ := ParseBuildNumberSpec(c[0])
		if s.String() != c[1] {
			t.Errorf("%q: String = %q", c[0], s.String())
		}
	}
	if s, _ := ParseBuildNumberSpec("*"); !s.IsAny() {
		t.Error("* should be any")
	}
	for _, x := range []string{"=3", "<2"} {
		if s, _ := ParseBuildNumberSpec(x); s.IsAny() {
			t.Errorf("%q should not be any", x)
		}
	}
	a, _ := ParseBuildNumberSpec("=3")
	b, _ := ParseBuildNumberSpec("3")
	c, _ := ParseBuildNumberSpec("*")
	if a != b || a == c || a.String() != b.String() {
		t.Error("comparability")
	}
}

func mustStringMatcher(t *testing.T, s string) StringMatcher {
	t.Helper()
	m, err := ParseStringMatcher(s)
	if err != nil {
		t.Fatalf("ParseStringMatcher(%q): %v", s, err)
	}
	return m
}

type matcherCase struct {
	pattern             string
	in, out             []string
	str                 string
	free, exact, isGlob bool
}

func checkMatcher(t *testing.T, m StringMatcher, c matcherCase) {
	t.Helper()
	for _, s := range c.in {
		if !m.Contains(s) {
			t.Errorf("%q should contain %q", c.pattern, s)
		}
	}
	for _, s := range c.out {
		if m.Contains(s) {
			t.Errorf("%q should not contain %q", c.pattern, s)
		}
	}
	if m.String() != c.str || m.IsAny() != c.free || m.IsExact() != c.exact || m.IsGlob() != c.isGlob {
		t.Errorf("%q: String=%q any=%v exact=%v glob=%v", c.pattern, m.String(), m.IsAny(), m.IsExact(), m.IsGlob())
	}
}

// Cases from libmamba tests/src/specs/test_chimera_string_spec.cpp, with the
// "py.*" case changed as noted.
func TestStringMatcherChimera(t *testing.T) {
	checkMatcher(t, StringMatcher{}, matcherCase{"<zero>", []string{"", "hello"}, nil, "*", true, false, true})
	cases := []matcherCase{
		{"mkl", []string{"mkl"}, []string{"", "nomkl", "hello"}, "mkl", false, true, true},
		// Upstream expects the regex "^py.*$" here. Like conda and libsolv,
		// only "^...$" is a regex in goomba, so this is a glob whose '.' is
		// literal.
		{"py.*", []string{"py.", "py.x"}, []string{"python", "py", "pypy", "cpython"}, "py.*", false, false, true},
		{"^.*(accelerate|mkl)$", []string{"accelerate", "mkl"}, []string{"", "openblas"}, "^.*(accelerate|mkl)$", false, false, false},
		// Added for goomba.
		{"^mkl$", []string{"mkl"}, []string{"nomkl", "^mkl$"}, "mkl", false, true, true},
		{"^.*$", []string{"", "x"}, nil, "*", true, false, true},
		{"*_cpython", []string{"h1_cpython", "_cpython"}, []string{"cpython"}, "*_cpython", false, false, true},
		{"^(a|b)$", []string{"a", "b"}, []string{"ab", "c"}, "^(a|b)$", false, false, false},
		// regex_match must match the whole string, even with alternation.
		{"^a|b$", []string{"a", "b"}, []string{"ax", "xb"}, "^a|b$", false, false, false},
	}
	for _, c := range cases {
		checkMatcher(t, mustStringMatcher(t, c.pattern), c)
	}
	m1, m2, m3 := mustStringMatcher(t, "mkl"), mustStringMatcher(t, "mkl"), mustStringMatcher(t, "*")
	if !m1.Equal(m2) || m1.Equal(m3) {
		t.Error("comparability")
	}
	// A regex-looking pattern that does not compile is an error.
	if _, err := ParseStringMatcher("^(unclosed$"); !errors.Is(err, ErrStringMatcher) {
		t.Errorf("bad regex: err = %v", err)
	}
	// Unlike libmamba, other non-compiling patterns are errors too, and so
	// are unbalanced brackets, quotes and non-ASCII.
	for _, bad := range []string{"a(b", "(", "a)b(", "^a\\($", "x[", "a'b", "a\"b", "a\x00b", "é"} {
		if m, err := ParseStringMatcher(bad); !errors.Is(err, ErrStringMatcher) {
			t.Errorf("ParseStringMatcher(%q) = %q, %v; want an error", bad, m, err)
		}
	}
}

// Cases from libmamba tests/src/specs/test_regex_spec.cpp, through
// makeRegexMatcher (libmamba's RegexSpec).
func TestStringMatcherRegex(t *testing.T) {
	mk := func(p string) StringMatcher {
		m, err := makeRegexMatcher(p)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		return m
	}
	if r := regexify(".*"); r != "^.*$" {
		t.Errorf("regexify(.*) = %q", r)
	}
	if !mk(".*").IsAny() {
		t.Error("free regex")
	}
	if r := regexify("mkl"); r != "^mkl$" {
		t.Errorf("regexify(mkl) = %q", r)
	}
	checkMatcher(t, mk("mkl"), matcherCase{"mkl", []string{"mkl"}, []string{"", "nomkl", "hello"}, "mkl", false, true, true})
	checkMatcher(t, mk("py.*"), matcherCase{"py.*", []string{"python", "py", "pypy"}, []string{"", "cpython"}, "^py.*$", false, false, false})
	checkMatcher(t, mk("^.*(accelerate|mkl)$"), matcherCase{"^.*(accelerate|mkl)$", []string{"accelerate", "mkl"}, []string{"", "openblas"}, "^.*(accelerate|mkl)$", false, false, false})

	if r := regexify("pyth*"); r != "^pyth.*$" {
		t.Errorf("regexify(pyth*) = %q", r)
	}
	if !mk("pyth*").Equal(mk("pyth*")) || mk("pyth*").Equal(mk("python")) {
		t.Error("comparability")
	}
	if !mk("py3.10_cuda11.8*").Contains("py3.10_cuda11.8_cudnn8.7.0_0") {
		t.Error("py3.10_cuda11.8*")
	}
	star := mk("py3.*")
	for _, s := range []string{"py3.", "py3.10", "py3.10_cuda11.8_cudnn8.7.0_0"} {
		if !star.Contains(s) {
			t.Errorf("py3.* should contain %q", s)
		}
	}
}

// Cases from libmamba tests/src/specs/test_platform.cpp.
func TestPlatform(t *testing.T) {
	want := []string{
		"noarch", "linux-32", "linux-64", "linux-armv6l", "linux-armv7l",
		"linux-aarch64", "linux-ppc64le", "linux-ppc64", "linux-s390x", "linux-riscv32",
		"linux-riscv64", "osx-64", "osx-arm64", "win-32", "win-64",
		"win-arm64", "zos-z",
	}
	got := KnownPlatforms()
	if len(got) != len(want) {
		t.Fatalf("KnownPlatforms() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("KnownPlatforms()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !IsKnownPlatform(want[i]) {
			t.Errorf("IsKnownPlatform(%q) = false", want[i])
		}
		p := want[i]
		if !(IsLinux(p) || IsOSX(p) || IsWin(p) || p == "noarch" || p == "zos-z") {
			t.Errorf("%q has no OS", p)
		}
	}

	parse := []struct{ in, want string }{
		{"linux-armv6l", "linux-armv6l"}, {" win-32 ", "win-32"}, {" OSX-64", "osx-64"}, {"linus-46", ""},
	}
	for _, c := range parse {
		p, ok := ParsePlatform(c.in)
		if p != c.want || ok != (c.want != "") {
			t.Errorf("ParsePlatform(%q) = %q, %v", c.in, p, ok)
		}
	}

	if IsLinux("win-64") || IsLinux("osx-64") || !IsLinux("linux-64") {
		t.Error("IsLinux")
	}
	if IsOSX("win-64") || !IsOSX("osx-64") || IsOSX("linux-64") {
		t.Error("IsOSX")
	}
	if !IsWin("win-64") || IsWin("osx-64") || IsWin("linux-64") {
		t.Error("IsWin")
	}
	if !IsUnix("linux-aarch64") || !IsUnix("osx-arm64") || IsUnix("win-64") || IsUnix("noarch") {
		t.Error("IsUnix")
	}
	if !IsNoarch("noarch") || IsNoarch("linux-64") {
		t.Error("IsNoarch")
	}
	if IsLinux("lin") || !IsLinux("LINUX-64") {
		t.Error("IsLinux edge cases")
	}
}

func TestHostPlatform(t *testing.T) {
	pairs := []struct{ goos, goarch, want string }{
		{"linux", "amd64", "linux-64"},
		{"linux", "arm64", "linux-aarch64"},
		{"linux", "ppc64le", "linux-ppc64le"},
		{"linux", "386", "linux-32"},
		{"linux", "s390x", "linux-s390x"},
		{"linux", "riscv64", "linux-riscv64"},
		{"darwin", "amd64", "osx-64"},
		{"darwin", "arm64", "osx-arm64"},
		{"windows", "amd64", "win-64"},
		{"windows", "arm64", "win-arm64"},
		{"plan9", "amd64", ""},
		{"linux", "mips", ""},
	}
	for _, c := range pairs {
		p, ok := PlatformFor(c.goos, c.goarch)
		if p != c.want || ok != (c.want != "") {
			t.Errorf("PlatformFor(%s, %s) = %q, %v", c.goos, c.goarch, p, ok)
		}
	}
	if h := HostPlatform(); h != "" && !IsKnownPlatform(h) {
		t.Errorf("HostPlatform() = %q", h)
	}
}

// refGlob is a memoized backtracking glob matcher, the reference for
// globMatch (from the WP1 review).
func refGlob(p, s string) bool {
	memo := map[[2]int]int{}
	var f func(i, j int) bool
	f = func(i, j int) bool {
		k := [2]int{i, j}
		if v, ok := memo[k]; ok {
			return v == 1
		}
		var r bool
		switch {
		case i == len(p):
			r = j == len(s)
		case p[i] == '*':
			r = f(i+1, j) || (j < len(s) && f(i, j+1))
		default:
			r = j < len(s) && p[i] == s[j] && f(i+1, j+1)
		}
		memo[k] = map[bool]int{true: 1, false: 2}[r]
		return r
	}
	return f(0, 0)
}

// refParts is the reference for version globs: "*" matches one or more
// parts.
func refParts(p, s []string) bool {
	memo := map[[2]int]int{}
	var f func(i, j int) bool
	f = func(i, j int) bool {
		k := [2]int{i, j}
		if v, ok := memo[k]; ok {
			return v == 1
		}
		var r bool
		switch {
		case i == len(p):
			r = j == len(s)
		case p[i] == "*":
			r = j < len(s) && (f(i+1, j+1) || f(i, j+1))
		default:
			r = j < len(s) && p[i] == s[j] && f(i+1, j+1)
		}
		memo[k] = map[bool]int{true: 1, false: 2}[r]
		return r
	}
	return f(0, 0)
}

// TestGlobMatchReference compares globMatch with a backtracking reference
// on random patterns and strings.
func TestGlobMatchReference(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for n := 0; n < 50000; n++ {
		var p, s strings.Builder
		for i := rng.Intn(9); i > 0; i-- {
			p.WriteByte("ab*"[rng.Intn(3)])
		}
		for i := rng.Intn(12); i > 0; i-- {
			s.WriteByte("ab"[rng.Intn(2)])
		}
		if got, want := globMatch(p.String(), s.String()), refGlob(p.String(), s.String()); got != want {
			t.Fatalf("globMatch(%q, %q) = %v, want %v", p.String(), s.String(), got, want)
		}
	}
}

// TestVersionGlobReference compares version glob matching with a
// backtracking reference on random part sequences.
func TestVersionGlobReference(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	pAlpha, sAlpha := []string{"1", "2", "*", "*", "3a"}, []string{"1", "2", "3a"}
	for n := 0; n < 20000; n++ {
		maxP, maxS := 8, 10
		if n%10 == 0 {
			maxP, maxS = 40, 64
		}
		pp, ss := make([]string, 1+rng.Intn(maxP)), make([]string, 1+rng.Intn(maxS))
		for i := range pp {
			pp[i] = pAlpha[rng.Intn(len(pAlpha))]
		}
		for i := range ss {
			ss[i] = sAlpha[rng.Intn(len(sAlpha))]
		}
		pv, sv := mustVersion(t, strings.Join(pp, ".")), mustVersion(t, strings.Join(ss, "."))
		if got, want := versionGlobMatch(&sv, &pv), refParts(pp, ss); got != want {
			t.Fatalf("version %q against glob %q = %v, want %v", sv, strings.Join(pp, "."), got, want)
		}
	}
}
