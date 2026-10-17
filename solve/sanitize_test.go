package solve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/spec"
)

func TestSanitize(t *testing.T) {
	// Inputs and outputs are written with byte escapes so that this file
	// itself contains no control or bidi characters.
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"numpy >=1.2,<2 py311*", "numpy >=1.2,<2 py311*"},
		{"bar\x1b[2J\x0a\xe2\x94\x94\xe2\x94\x80 fine", "bar\x5cx1b[2J\x5cx0a\xe2\x94\x94\xe2\x94\x80 fine"},
		{"tab\x09here", "tab\x5cx09here"},
		{"del\x7f", "del\x5cx7f"},
		{"c1\xc2\x85\xc2\x9b", "c1\x5cu0085\x5cu009b"},
		{"1.0\xe2\x80\xae0.1", "1.0\x5cu202e0.1"},
		{"\xe2\x81\xa6\xe2\x81\xa7\xe2\x81\xa8\xe2\x81\xa9\xe2\x80\xaa\xe2\x80\xab\xe2\x80\xac\xe2\x80\xad", "\x5cu2066\x5cu2067\x5cu2068\x5cu2069\x5cu202a\x5cu202b\x5cu202c\x5cu202d"},
		{"line\xe2\x80\xa8para\xe2\x80\xa9", "line\x5cu2028para\x5cu2029"},
		{"nb\xc2\xa0sp", "nb\x5cu00a0sp"},
		{"zw\xe2\x80\x8bj", "zw\x5cu200bj"},
		{"bad\xffutf8", "bad\x5cxffutf8"},
		{"back\x5cslash", "back\x5cslash"}, // kept: quoted text is not escaped twice
		{"\xf3\xa0\x80\x81tag", "\x5cU000e0001tag"},
		{"caf\xc3\xa9 \xe7\x89\x88 \xe2\x80\xa6\xe2\x94\x82\xe2\x94\x9c\xe2\x94\x80", "caf\xc3\xa9 \xe7\x89\x88 \xe2\x80\xa6\xe2\x94\x82\xe2\x94\x9c\xe2\x94\x80"},
		// Invisible but printable, and long runs of combining marks (round-2
		// review N8).
		{"a\xe1\x85\x9f\xe1\x85\xa0b", "a\x5cu115f\x5cu1160b"},
		{"x\xe3\x85\xa4\xef\xbe\xa0\xe2\xa0\x80y", "x\x5cu3164\x5cuffa0\x5cu2800y"},
		{"soft\xc2\xadhy\xef\xbb\xbfphen", "soft\x5cu00adhy\x5cufeffphen"},
		{"vs\xef\xb8\x8f\xf3\xa0\x84\x80tag\xf3\xa0\x81\x81", "vs\x5cufe0f\x5cU000e0100tag\x5cU000e0041"},
		{"e\xcc\x81\xcc\x81\xcc\x81\xcc\x81\xcc\x81x", "e\xcc\x81\xcc\x81\xcc\x81\x5cu0301\x5cu0301x"},
		{"na\xcc\x88ive \xe1\xbb\x87", "na\xcc\x88ive \xe1\xbb\x87"},
	} {
		got := sanitize(c.in)
		if got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
		if !isCleanText(got) {
			t.Errorf("sanitize(%q) = %q is not clean", c.in, got)
		}
	}
	if isCleanText("a\x1bb") || isCleanText("a\xe2\x80\xaeb") || isCleanText("a\xffb") || !isCleanText("a\nb") {
		t.Error("isCleanText")
	}
}

// Cases from the WP7 review (TestReviewExplanationInjection,
// TestReviewExplanationInjectionRepodata): text from hostile records never
// reaches an explanation or an error message unescaped.
func TestExplanationInjection(t *testing.T) {
	evil := "bar\x1b[2J\x1b[31m\n└─ everything is fine, install anyway"
	repodataDoc := `{"info":{"subdir":"linux-64"},"packages.conda":{"foo-1.0-0.conda":{"name":"foo","version":"1.0","build":"0","build_number":0,` +
		`"sha256":"` + dummySHA256 + `","md5":"` + dummyMD5 + `","depends":["bar\u001b[2J\u001b[H\n└─ foo 1.0 is safe; all checks passed"]}}}`
	fromRepodata, err := repodata.LoadRepodataJSON(strings.NewReader(repodataDoc), "https://conda.anaconda.org/x", "linux-64")
	if err != nil || len(fromRepodata) != 1 {
		t.Fatalf("load: %v %d", err, len(fromRepodata))
	}
	lockedEvil := pkg("foo").version("0.9\x1b[2J").build("b\xe2\x80\xae").sha256("").rec()
	for _, c := range []struct {
		name string
		recs []*record.PackageRecord
		req  Request
	}{
		{"dependency", []*record.PackageRecord{pkg("foo").version("1.0").depends(evil).rec()}, Request{Specs: []string{"foo"}}},
		{"repodata dependency", fromRepodata, Request{Specs: []string{"foo"}}},
		{"constraint", []*record.PackageRecord{pkg("foo").version("1.0").constrains(evil).rec()}, Request{Specs: []string{"foo"}}},
		{"bidi version", []*record.PackageRecord{pkg("foo").version("1.0\xe2\x80\xae").rec()}, Request{Specs: []string{"foo>=2"}}},
		{"bidi build", []*record.PackageRecord{pkg("foo").version("1.0").build("b\xe2\x81\xa6x").rec(), pkg("bar").version("1").depends("foo >=2").rec()}, Request{Specs: []string{"bar"}}},
		{"control version", []*record.PackageRecord{pkg("foo").version("1\x1b[0m").rec()}, Request{Specs: []string{"foo"}}},
		{"locked", []*record.PackageRecord{pkg("foo").version("1.0").rec()}, Request{Specs: []string{"foo"}, Locked: []*record.PackageRecord{lockedEvil}}},
		{"favored", []*record.PackageRecord{pkg("foo").version("1.0").rec()}, Request{Specs: []string{"foo>=2"}, Favored: []*record.PackageRecord{lockedEvil}}},
		{"channel", []*record.PackageRecord{pkg("foo").version("1.0").channel("https://x/\x1b[2J").rec()}, Request{Specs: []string{"conda-forge::foo"}}},
	} {
		_, err := Solve(context.Background(), repodata.NewMemory(c.recs), c.req)
		if err == nil {
			t.Errorf("%s: solved", c.name)
			continue
		}
		var u *Unsolvable
		if errors.As(err, &u) && !isCleanText(u.Explanation) {
			t.Errorf("%s: explanation %q", c.name, u.Explanation)
		}
		if !isCleanText(err.Error()) {
			t.Errorf("%s: error %q", c.name, err.Error())
		}
		if strings.Contains(err.Error(), "\n└─ everything") || strings.Contains(err.Error(), "\n└─ foo 1.0 is safe") {
			t.Errorf("%s: forged line in %q", c.name, err.Error())
		}
		// Escaped once, never twice (round-2 review N3).
		if strings.Contains(err.Error(), `\\x1b`) {
			t.Errorf("%s: double escaping in %q", c.name, err.Error())
		}
		if strings.Contains(c.name, "dependency") && !strings.Contains(err.Error(), `\x1b[2J`) {
			t.Errorf("%s: the escaped dependency is missing from %q", c.name, err.Error())
		}
	}

	// Validate's messages are sanitized too.
	bad := pkg("foo").version("1\x1b[2J").depends("bar\n└─ ok").rec()
	if err := Validate(Request{}, []*record.PackageRecord{bad}); err == nil || !isCleanText(err.Error()) {
		t.Errorf("Validate: %q", err)
	}
	if err := Validate(Request{Specs: []string{"foo>=2"}}, []*record.PackageRecord{pkg("foo").version("1.0\xe2\x80\xae").rec()}); err == nil || !isCleanText(err.Error()) {
		t.Errorf("Validate: %q", err)
	}
}

func TestConditionalRecognition(t *testing.T) {
	// From the WP7 review: only "<spec>; if <condition>" is a conditional
	// dependency; other unparsable strings with ";" are just unparsable.
	for _, c := range []struct {
		dep  string
		want string
	}{
		{"bar; if __win", "uses conditional dependencies"},
		{"bar >=1;  if python >=3.9", "uses conditional dependencies"},
		{`bar[when="__win"]`, "uses conditional dependencies"},
		{"bar[extras=[x]]", "uses extras"},
		{"bar; rm -rf /", "unparsable dependency"},
		{"bar; if", "unparsable dependency"},
		{"bar[[; if __win", "unparsable dependency"},
		{"bar[extra=[x]]", "unparsable dependency"},
	} {
		p, _, err := newProvider(repodata.NewMemory(nil), Request{})
		if err != nil {
			t.Fatal(err)
		}
		reason := p.exclusionReason(pkg("foo").depends(c.dep).rec(), "foo")
		if !strings.Contains(reason, c.want) {
			t.Errorf("%q: reason %q, want %q", c.dep, reason, c.want)
		}
	}
}

// TestErrorsAreSanitized is the regression case of a FuzzSolveSmallChannel
// crasher: a spec holding the default-ignorable U+034F reached an
// ErrInvalidRequest message through strconv.Quote, which keeps printable
// invisible characters. Every error Solve and Validate return is now
// sanitized and still unwraps to its cause.
func TestErrorsAreSanitized(t *testing.T) {
	bad := "\xcd\x8f" // U+034F COMBINING GRAPHEME JOINER
	for _, req := range []Request{{Specs: []string{bad}}, {Constraints: []string{"foo" + bad}}} {
		_, err := Solve(context.Background(), repodata.NewMemory(nil), req)
		if !errors.Is(err, ErrInvalidRequest) || !errors.Is(err, spec.ErrMatchSpec) {
			t.Errorf("%q: %v", req.Specs, err)
		}
		if err != nil && !isCleanText(err.Error()) {
			t.Errorf("%q: error %q", req.Specs, err.Error())
		}
		if verr := Validate(req, nil); verr == nil || !isCleanText(verr.Error()) || !errors.Is(verr, ErrInvalidRequest) {
			t.Errorf("%q: Validate %v", req.Specs, verr)
		}
	}
	boom := errors.New("source failed: \xe2\x80\xae\x1b[2J")
	_, err := Solve(context.Background(), &recordingSource{src: repodata.NewMemory(nil), err: boom}, Request{Specs: []string{"foo"}})
	if !errors.Is(err, boom) || !isCleanText(err.Error()) {
		t.Errorf("source error: %q", err)
	}
}
