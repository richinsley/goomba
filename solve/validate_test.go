package solve

import (
	"errors"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

func TestValidate(t *testing.T) {
	foo1 := pkg("foo").version("1.0").depends("bar >=2").constrains("baz <2").rec()
	bar2 := pkg("bar").version("2.0").rec()
	bar1 := pkg("bar").version("1.0").rec()
	baz1 := pkg("baz").version("1.0").rec()
	baz3 := pkg("baz").version("3.0").rec()
	gpu := pkg("gpu").version("1.0").depends("__cuda >=12").constrains("__glibc >=2.30").rec()
	cuda := virtualPkg("__cuda", "12.4", "0")
	glibc := virtualPkg("__glibc", "2.28", "0")
	cf := pkg("cf").version("1.0").channel("https://conda.anaconda.org/conda-forge").rec()

	for _, c := range []struct {
		name string
		req  Request
		recs []*record.PackageRecord
		err  string // "" for valid
	}{
		{"valid", Request{Specs: []string{"foo"}}, []*record.PackageRecord{foo1, bar2}, ""},
		{"valid with constrained package", Request{Specs: []string{"foo"}}, []*record.PackageRecord{bar2, baz1, foo1}, ""},
		{"empty", Request{}, nil, ""},
		{"spec not installed", Request{Specs: []string{"foo", "qux"}}, []*record.PackageRecord{foo1, bar2}, `spec "qux" is not satisfied: qux is not installed`},
		{"spec not matched", Request{Specs: []string{"foo >=2"}}, []*record.PackageRecord{foo1, bar2}, `spec "foo >=2" is not satisfied by foo=1.0=h123456_0`},
		{"dependency missing", Request{Specs: []string{"foo"}}, []*record.PackageRecord{foo1}, `foo=1.0=h123456_0 depends on "bar >=2", but bar is not installed`},
		{"dependency not matched", Request{Specs: []string{"foo"}}, []*record.PackageRecord{foo1, bar1}, `depends on "bar >=2", which bar=1.0=h123456_0 does not satisfy`},
		{"constrains violated", Request{Specs: []string{"foo"}}, []*record.PackageRecord{foo1, bar2, baz3}, `constrains "baz <2", which baz=3.0=h123456_0 does not satisfy`},
		{"root constraint violated", Request{Specs: []string{"foo"}, Constraints: []string{"bar <2"}}, []*record.PackageRecord{foo1, bar2}, `constraint "bar <2" does not hold for bar=2.0=h123456_0`},
		{"root constraint on absent package", Request{Specs: []string{"foo"}, Constraints: []string{"qux <2"}}, []*record.PackageRecord{foo1, bar2}, ""},
		{"two records of one package", Request{}, []*record.PackageRecord{bar1, bar2}, `two records for "bar"`},
		{"nil record", Request{}, []*record.PackageRecord{nil}, "record 0 is nil"},
		{"virtual record", Request{}, []*record.PackageRecord{pkg("__cuda").rec()}, "virtual packages come only from the target machine"},
		{"virtual record shadowing the machine", Request{Virtual: []*record.PackageRecord{cuda}}, []*record.PackageRecord{pkg("__cuda").rec()}, "virtual packages come only from the target machine"},
		{"no sha256", Request{}, []*record.PackageRecord{pkg("x").sha256("").rec()}, "no valid sha256"},
		{"bad sha256", Request{}, []*record.PackageRecord{pkg("x").sha256(strings.Repeat("z", 64)).rec()}, "no valid sha256"},
		{"upper-case name", Request{}, []*record.PackageRecord{pkg("X").rec()}, "invalid package name"},
		{"pattern name", Request{}, []*record.PackageRecord{pkg("x*").rec()}, "invalid package name"},
		{"bad version", Request{}, []*record.PackageRecord{pkg("x").version("1..2").rec()}, `parse version "1..2"`},
		{"unparsable dependency", Request{}, []*record.PackageRecord{pkg("x").depends("y[[").rec()}, `unparsable dependency "y[["`},
		{"conditional dependency", Request{}, []*record.PackageRecord{pkg("x").depends("y; if __win").rec()}, "uses conditional dependencies"},
		{"unparsable constraint", Request{}, []*record.PackageRecord{pkg("x").constrains("y[[").rec()}, `unparsable dependency "y[["`},
		{"virtual dependency met", Request{Specs: []string{"gpu"}, Virtual: []*record.PackageRecord{cuda}}, []*record.PackageRecord{gpu}, ""},
		{"virtual dependency missing", Request{Specs: []string{"gpu"}}, []*record.PackageRecord{gpu}, `depends on "__cuda >=12", but __cuda is not installed`},
		{"virtual constrains violated", Request{Specs: []string{"gpu"}, Virtual: []*record.PackageRecord{cuda, glibc}}, []*record.PackageRecord{gpu}, `constrains "__glibc >=2.30", which __glibc=2.28=0 does not satisfy`},
		{"virtual spec", Request{Specs: []string{"__glibc >=2.17"}, Virtual: []*record.PackageRecord{glibc}}, nil, ""},
		{"virtual spec not met", Request{Specs: []string{"__glibc >=2.34"}, Virtual: []*record.PackageRecord{glibc}}, nil, `spec "__glibc >=2.34" is not satisfied by __glibc=2.28=0`},
		{"locked kept", Request{Locked: []*record.PackageRecord{bar1}}, []*record.PackageRecord{pkg("bar").version("1.0").rec()}, ""},
		{"locked violated", Request{Locked: []*record.PackageRecord{bar1}}, []*record.PackageRecord{bar2}, "bar=2.0=h123456_0 is installed but bar=1.0=h123456_0 is locked"},
		{"locked absent", Request{Locked: []*record.PackageRecord{bar1}}, nil, ""},
		{"channel spec met", Request{Specs: []string{"conda-forge::cf"}}, []*record.PackageRecord{cf}, ""},
		{"channel spec on another channel", Request{Specs: []string{"other::cf"}}, []*record.PackageRecord{cf}, `spec "other::cf" is not satisfied`},
		{"channel spec on a virtual package", Request{Specs: []string{"conda-forge::__glibc"}, Virtual: []*record.PackageRecord{glibc}}, nil, "is not satisfied"},
	} {
		err := Validate(c.req, c.recs)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err != "" && err == nil:
			t.Errorf("%s: accepted", c.name)
		case c.err != "" && (!errors.Is(err, ErrInvalidSolution) || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: got %v, want ErrInvalidSolution with %q", c.name, err, c.err)
		}
	}
}
