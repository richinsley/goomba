package spec

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// seedLines returns every step-th non-comment line of a testdata file, cut
// at the first '=' when cut is set (parsed_versions.txt).
func seedLines(f *testing.F, name string, step int, cut bool) []string {
	file, err := os.Open("testdata/" + name)
	if err != nil {
		f.Fatal(err)
	}
	defer file.Close()
	var out []string
	sc := bufio.NewScanner(file)
	for i := 0; sc.Scan(); i++ {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") || i%step != 0 {
			continue
		}
		if cut {
			line, _, _ = strings.Cut(line, "=")
		}
		out = append(out, strings.TrimPrefix(line, "!"))
	}
	return out
}

var fuzzVersionSeeds = []string{
	"", "0", "1.2.3", "1!2.0", "1.0.post1", "1.0.dev1", "3.7b_", "1.2+local.3", "1-2", "1_2",
	"1.1_", "_", "1__", "a1.b2", "0*", "*", "*.1.*", "1.0a_", "18446744073709551615",
	"18446744073709551616", "1-1_", "1-1_1", "1..2", "1.", "!1", "1!", "5.5++", "0.4.1a.vc11",
	"14.3.1.post26.g9d75ca2", "2.2be5ta29", "1.0.1rc1", "1!1.2+123abc456", "1.K", "  0.4.1.RC  ",
}

var fuzzSpecSeeds = []string{
	"", "*", "=*", "==*", "1.7", "==1.7", "!=1.7", ">= 1.7", " = 1.8.* ", "!=1.8.*", " 1.*.3",
	"=1.*.3", "!=1.*.3 ", " ~= 1.8 ", "~=1", " (>= 1.7, <1.8) |>=1.9.0.0 ", "1.0.0*", "2013a*",
	"1.2.3+4*", "==1.7.1.*", "1.5.*|>1.7,<1.8", ">=2.7, !=3.0.*, !=3.1.*", "*.*", "2.*.1.1.*",
	"*.2023_10_12", ">=10.0,*.*", "><2.4.5", "!!2.4.5", "(1.5", "1.5)", "1.5||1.6", "^1.5",
	"===3.3.2", "~=3.3.2.*", "c +, 0/|0 *", "a[version=)|(", "a=)(=b", "==0.11.*.*|=0.12.0",
	"0.2.18|0.2.18.*.", "=1-*", "!=1-*.*", "1.*0", "((1|2),3)|4", ">=3|<1,<0.9",
}

var fuzzMatchSpecSeeds = []string{
	"", "xtensor==0.12.3", "xtensor      >=       0.12.3", "scipy   >=    1.5.0,  < 2.0.0",
	"kytea >=0.1.4, 0.2.0", "abc[version='>3']", "mingw-w64-ucrt-x86_64-crt-git v12.0.0.r2.ggc561118da h707e725_0",
	"openblas 0.2.18|0.2.18.*.", "_libgcc_mutex    0.1       conda_forge     ", "GDAL!=3.6.*,!=3.7.*",
	"importlib-metadata  # drop this", "numpy 1.7*", "conda-forge:pypi:xtensor==0.12.3",
	"numpy[version='1.7|1.8']", "conda-forge/linux-64::xtensor==0.12.3",
	"conda-forge::foo[build=bld](target=blarg,optional)", "python[build_number='<=3']",
	`blas[track_features="mkl avx"]`,
	"https://conda.anaconda.org/conda-forge/linux-64/ncurses-6.4-h59595ed_2.conda#7dbaa197d7ba6032caf7ae7f32c1efa0",
	"/srv/downloads/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
	"xtensor[url=file:///srv/downloads/xtensor-0.21.4-hc9558a2_0.tar.bz2]",
	"foo=1.0=2[md5=123123123, license=BSD-3, fn='test 123.tar.bz2', url='abcdef']",
	`defaults::numpy [ name="pytorch",channel='anaconda',version=">=1.8,<2|1.9", build='3']`,
	"numpy >1.8,<2|==1.7,!=1.9,~=1.7.1 py34_0", "python[version='~=3.11.0',build=*_cpython]",
	"*[md5=fewjaflknd]", "libblas=*=*mkl", "libblas=0.15*", "conda-forge[noarch]::tzdata[subdir=linux64]",
	"libblas[build=^.*(accelerate|mkl)$]", "python[version='*,=3.12.*']", "python[version>3]",
	"pytorch~=2.3.1=py3.10_cuda11.8*", "name *,*.* build*", "python_abi 3.11.* *_cp311",
	"__glibc >=2.17,<3.0.a0", "pkg[subdir='linux-64,linux-64-512']", "dask[array] >=2023",
	"pkg[license='MIT, BSD']", "https://x/pkg-1-0.conda#sha256:" + strings.Repeat("0", 64),
}

// FuzzParseVersion checks that parsing never panics and that a parsed
// version's String parses back to an equal version.
func FuzzParseVersion(f *testing.F) {
	for _, s := range fuzzVersionSeeds {
		f.Add(s)
	}
	for _, s := range seedLines(f, "parsed_versions.txt", 97, true) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseVersion(s)
		if err != nil {
			return
		}
		str := v.String()
		w, err := ParseVersion(str)
		if err != nil {
			t.Fatalf("ParseVersion(%q).String() = %q does not parse: %v", s, str, err)
		}
		if v.Compare(w) != 0 || !sameStructure(v, w) {
			t.Fatalf("ParseVersion(%q) = %q reparses to %q", s, str, w.String())
		}
		if w.String() != str {
			t.Fatalf("String not stable for %q: %q then %q", s, str, w.String())
		}
	})
}

func sign(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	}
	return 0
}

// FuzzVersionOrder checks that Compare is a total preorder: reflexive,
// antisymmetric and transitive on three parsed inputs.
func FuzzVersionOrder(f *testing.F) {
	seeds := append([]string(nil), fuzzVersionSeeds...)
	seeds = append(seeds, seedLines(f, "parsed_versions.txt", 211, true)...)
	for i := range seeds {
		f.Add(seeds[i], seeds[(i+1)%len(seeds)], seeds[(i*7+3)%len(seeds)])
	}
	f.Fuzz(func(t *testing.T, sa, sb, sc string) {
		a, errA := ParseVersion(sa)
		b, errB := ParseVersion(sb)
		c, errC := ParseVersion(sc)
		if errA != nil || errB != nil || errC != nil {
			return
		}
		vs := []Version{a, b, c}
		for _, x := range vs {
			if x.Compare(x) != 0 {
				t.Fatalf("%q != itself", x)
			}
			for _, y := range vs {
				if sign(x.Compare(y)) != -sign(y.Compare(x)) {
					t.Fatalf("not antisymmetric: %q, %q", x, y)
				}
				for _, z := range vs {
					xy, yz, xz := x.Compare(y), y.Compare(z), x.Compare(z)
					if xy <= 0 && yz <= 0 && xz > 0 {
						t.Fatalf("not transitive: %q <= %q <= %q but %q > %q", x, y, z, x, z)
					}
					if xy == 0 && yz == 0 && xz != 0 {
						t.Fatalf("equality not transitive: %q, %q, %q", x, y, z)
					}
				}
			}
		}
	})
}

var fuzzProbeVersions = []string{"0", "1.0", "1.7.1", "1.8alpha", "2.0.1", "3.3", "1!2.0", "1.0+local", "2013a", "1.0.1_"}

// FuzzParseVersionSpec checks that parsing never panics, that the String
// form parses back to an equal spec, and that both agree on sample
// versions.
func FuzzParseVersionSpec(f *testing.F) {
	for _, s := range fuzzSpecSeeds {
		f.Add(s)
	}
	for _, line := range seedLines(f, "realworld_specs.txt", 13, false) {
		if _, v, ok := strings.Cut(line, " "); ok {
			f.Add(v)
		}
	}
	probes := make([]Version, len(fuzzProbeVersions))
	for i, s := range fuzzProbeVersions {
		probes[i] = mustVersion(f, s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		spec, err := ParseVersionSpec(s)
		if err != nil {
			return
		}
		str := spec.String()
		spec2, err := ParseVersionSpec(str)
		if err != nil {
			t.Fatalf("ParseVersionSpec(%q).String() = %q does not parse: %v", s, str, err)
		}
		if !spec2.Equal(spec) || spec2.String() != str {
			t.Fatalf("ParseVersionSpec(%q) = %q reparses to %q", s, str, spec2.String())
		}
		_ = spec.CondaBuildString()
		for _, v := range probes {
			if spec.Contains(v) != spec2.Contains(v) {
				t.Fatalf("%q and %q disagree on %q", s, str, v)
			}
		}
	})
}

// FuzzParseMatchSpec checks that parsing never panics and that the String
// form parses back to an equal spec that matches the same records.
func FuzzParseMatchSpec(f *testing.F) {
	for _, s := range fuzzMatchSpecSeeds {
		f.Add(s)
	}
	for _, s := range seedLines(f, "realworld_specs.txt", 7, false) {
		f.Add(s)
	}
	recs := []*record.PackageRecord{
		{Name: "numpy", Version: "1.26.4", Build: "py311h64a7726_0", Subdir: "linux-64"},
		{Name: "python", Version: "3.11.15", Build: "hd63d673_0_cpython", Subdir: "linux-64"},
		{Name: "foo", Version: "1.0", Build: "2", BuildNumber: 2, MD5: "123123123", License: "BSD-3"},
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseMatchSpec(s)
		if err != nil {
			return
		}
		str := m.String()
		m2, err := ParseMatchSpec(str)
		if err != nil {
			t.Fatalf("ParseMatchSpec(%q).String() = %q does not parse: %v", s, str, err)
		}
		if m2.String() != str {
			t.Fatalf("ParseMatchSpec(%q) = %q reparses to %q", s, str, m2.String())
		}
		_ = m.CondaBuildForm()
		_, _ = m.ChannelURL(DefaultChannelAlias)
		// The text form is lossless: every field survives the round trip,
		// package URL specs included.
		if !m2.Equal(m) {
			t.Fatalf("ParseMatchSpec(%q) = %q reparses to a different spec: %+v vs %+v", s, str, m, m2)
		}
		for _, r := range recs {
			if m.Matches(r) != m2.Matches(r) {
				t.Fatalf("%q and %q disagree on %s", s, str, r)
			}
		}
	})
}
