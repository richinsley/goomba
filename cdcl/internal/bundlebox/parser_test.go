package bundlebox

import (
	"errors"
	"strings"
	"testing"
)

// Spec forms used throughout resolvo tests/solver/main.rs.
func TestParseConditionalSpec(t *testing.T) {
	cases := []struct {
		in   string
		want string // specs joined by " | ", then "; if" and the condition key
	}{
		{"asdf", "asdf *"},
		{"conflicting 1", "conflicting >=1, <2"},
		{"opentelemetry-api 2..4", "opentelemetry-api >=2, <4"},
		{"a 0..100", "a >=0, <100"},
		{"B 0..20", "B >=0, <20"},
		{"a 5..3", "a ∅"},
		{"p0 1 .. 3", "p0 >=1, <3"},
		{"  pad  ", "pad *"},
		{"c | d", "c * | d *"},
		{"x 1..2 | x 3..4", "x >=1, <2 | x >=3, <4"},
		{"icons 2 | intl", "icons >=2, <3 | intl *"},
		{"baz; if bar", "baz *; if (bar *)"},
		{"baz 2; if bar 1", "baz >=2, <3; if (bar >=1, <2)"},
		{"icon; if bar and baz", "icon *; if ((bar *) and (baz *))"},
		{"icon; if bar and baz or menu", "icon *; if (((bar *) and (baz *)) or (menu *))"},
		{"b; if cond_a and cond_b", "b *; if ((cond_a *) and (cond_b *))"},
		{"p4 2..7; if p1 or missing and p3 1..2", "p4 >=2, <7; if (((p1 *) or (missing *)) and (p3 >=1, <2))"},
	}
	for _, c := range cases {
		cs, err := ParseConditionalSpec(c.in)
		if err != nil {
			t.Errorf("ParseConditionalSpec(%q): %v", c.in, err)
			continue
		}
		if got := render(cs); got != c.want {
			t.Errorf("ParseConditionalSpec(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func render(cs ConditionalSpec) string {
	parts := make([]string, len(cs.Specs))
	for i, s := range cs.Specs {
		parts[i] = s.Name + " " + s.Versions.String()
	}
	out := strings.Join(parts, " | ")
	if cs.Condition != nil {
		out += "; if " + renderCond(cs.Condition)
	}
	return out
}

func renderCond(c *SpecCondition) string {
	if c.Requirement != nil {
		return "(" + c.Requirement.Name + " " + c.Requirement.Versions.String() + ")"
	}
	return "(" + renderCond(c.Left) + " " + c.Op.String() + " " + renderCond(c.Right) + ")"
}

func TestParseConditionalSpecErrors(t *testing.T) {
	for _, in := range []string{
		"", "1abc", "a 01", "a 1..", "a | ", "a; if", "a;if b", "a 99999999999", "a 4294967295",
		"a b", "a 1..2..3", "é", "a |", "a; if b and",
	} {
		if _, err := ParseConditionalSpec(in); !errors.Is(err, ErrSyntax) {
			t.Errorf("ParseConditionalSpec(%q): got %v, want ErrSyntax", in, err)
		}
	}
}

func TestPackOrderAndRanges(t *testing.T) {
	r := Between(NewPack(1), NewPack(3))
	for _, c := range []struct {
		p    Pack
		want bool
	}{
		{NewPack(0), false},
		{NewPack(1), true},
		{NewPack(1).WithUnknownDeps(), true},
		{NewPack(2).WithCancelDuringGetDependencies(), true},
		{NewPack(3), false},
	} {
		if got := r.Contains(c.p); got != c.want {
			t.Errorf("%v contains %+v = %v", r, c.p, got)
		}
	}
	if Full().String() != "*" || (Ranges{}).String() != "∅" || r.String() != ">=1, <3" {
		t.Errorf("display: %q %q %q", Full(), Ranges{}, r)
	}
}

// FuzzParseConditionalSpec checks that the spec parser never panics and
// that anything it accepts has at least one spec with a valid name.
func FuzzParseConditionalSpec(f *testing.F) {
	for _, s := range []string{
		"asdf", "conflicting 1", "opentelemetry-api 2..4", "c | d", "x 1..2 | x 3..4",
		"baz; if bar", "baz 2; if bar 1", "icon; if bar and baz or menu", "a 5..3",
		"p4 2..7; if p1 or missing and p3 1..2", "a 01", "a; if", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		cs, err := ParseConditionalSpec(s)
		if err != nil {
			if !errors.Is(err, ErrSyntax) {
				t.Fatalf("error %v does not wrap ErrSyntax", err)
			}
			return
		}
		if len(cs.Specs) == 0 {
			t.Fatalf("%q: accepted without specs", s)
		}
		for _, sp := range cs.Specs {
			if sp.Name == "" {
				t.Fatalf("%q: empty name", s)
			}
		}
		if _, err := ParseSpec(s); err == nil && (len(cs.Specs) != 1 || cs.Condition != nil) {
			t.Fatalf("%q: ParseSpec accepted a union or condition", s)
		}
	})
}
