package spec

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// chanStr is libmamba's ms.channel()->str(): the package URL, or the
// channel with its platform filters.
func chanStr(m MatchSpec) string {
	if m.URL != "" {
		return m.URL
	}
	if m.Channel == "" || m.Subdir == "" {
		return m.Channel
	}
	return m.Channel + "[" + m.Subdir + "]"
}

type msCase struct {
	in         string
	name       string // Name.String()
	version    string // Version.String(); "=*" is any
	build      string // Build.String(); "*" is any
	bn         string // BuildNumber.String(); "=*" is any
	channel    string // chanStr; "-" means no channel
	location   string // Channel
	subdirs    string // Subdir
	ns         string // Namespace
	fn, md5    string
	str        string // String()
	condaBuild string // CondaBuildForm()
	onlyName   int    // 1: IsOnlyPackageName, -1: not
	optional   bool
}

func (c msCase) check(t *testing.T) {
	t.Helper()
	m, err := ParseMatchSpec(c.in)
	if err != nil {
		t.Errorf("ParseMatchSpec(%q): %v", c.in, err)
		return
	}
	checkStr := func(what, got, want string) {
		t.Helper()
		if want != "" && got != want {
			t.Errorf("%q: %s = %q, want %q", c.in, what, got, want)
		}
	}
	checkStr("name", m.Name.String(), c.name)
	checkStr("version", m.Version.String(), c.version)
	checkStr("build", m.Build.String(), c.build)
	checkStr("build number", m.BuildNumber.String(), c.bn)
	if c.channel == "-" {
		checkStr("channel", "-"+chanStr(m), "-")
	} else {
		checkStr("channel", chanStr(m), c.channel)
	}
	checkStr("channel location", m.Channel, c.location)
	checkStr("subdir", m.Subdir, c.subdirs)
	checkStr("namespace", m.Namespace, c.ns)
	checkStr("fn", m.Fn, c.fn)
	checkStr("md5", m.MD5, c.md5)
	checkStr("String()", m.String(), c.str)
	checkStr("CondaBuildForm()", m.CondaBuildForm(), c.condaBuild)
	if c.onlyName != 0 && m.IsOnlyPackageName() != (c.onlyName > 0) {
		t.Errorf("%q: IsOnlyPackageName = %v", c.in, !(c.onlyName > 0))
	}
	if m.Optional != c.optional {
		t.Errorf("%q: Optional = %v", c.in, m.Optional)
	}
	// Every spec must survive a String round trip.
	m2, err := ParseMatchSpec(m.String())
	if err != nil {
		t.Errorf("%q: reparse %q: %v", c.in, m.String(), err)
	} else if m2.String() != m.String() {
		t.Errorf("%q: String %q reparses to %q", c.in, m.String(), m2.String())
	}
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp ("MatchSpec parse").
// Upstream's sample home-directory paths are rewritten under /srv.
func TestParseMatchSpec(t *testing.T) {
	cases := []msCase{
		{in: "", name: "*", version: "=*", build: "*", bn: "=*", str: "*", onlyName: -1},
		{in: "xtensor==0.12.3", name: "xtensor", version: "==0.12.3", str: "xtensor==0.12.3", onlyName: -1},
		{in: "xtensor      >=       0.12.3", name: "xtensor", version: ">=0.12.3", build: "*", bn: "=*", str: "xtensor>=0.12.3", onlyName: -1},
		{in: "python > 3.11", name: "python", version: ">3.11", build: "*", bn: "=*", str: "python>3.11", onlyName: -1},
		{in: "numpy < 2.0", name: "numpy", version: "<2.0", build: "*", bn: "=*", str: "numpy<2.0", onlyName: -1},
		{in: "pytorch-cpu = 1.13.0", name: "pytorch-cpu", version: "=1.13.0", build: "*", bn: "=*", str: "pytorch-cpu=1.13.0", onlyName: -1},
		{in: "scipy   >=    1.5.0,  < 2.0.0", name: "scipy", version: ">=1.5.0,<2.0.0", build: "*", bn: "=*", str: `scipy[version=">=1.5.0,<2.0.0"]`, onlyName: -1},
		{in: "scikit-learn >1.0.0", name: "scikit-learn", version: ">1.0.0", build: "*", bn: "=*", str: "scikit-learn>1.0.0"},
		{in: "kytea >=0.1.4, 0.2.0", name: "kytea", version: ">=0.1.4,==0.2.0", build: "*", bn: "=*", str: `kytea[version=">=0.1.4,==0.2.0"]`},
		{in: "abc>12", name: "abc", version: ">12", build: "*", bn: "=*", str: "abc>12"},
		{in: "abc[version='>3']", name: "abc", version: ">3", build: "*", bn: "=*", str: "abc>3"},
		{in: "numpy~=1.26.0", name: "numpy", version: "~=1.26.0", build: "*", bn: "=*", str: "numpy~=1.26.0"},
		{
			in:   "mingw-w64-ucrt-x86_64-crt-git v12.0.0.r2.ggc561118da h707e725_0",
			name: "mingw-w64-ucrt-x86_64-crt-git", version: "==v12.0.0.r2.ggc561118da", build: "h707e725_0", bn: "=*",
			str: "mingw-w64-ucrt-x86_64-crt-git==v12.0.0.r2.ggc561118da=h707e725_0",
		},
		{in: "openblas 0.2.18|0.2.18.*.", name: "openblas", version: "==0.2.18|=0.2.18"},
		{in: "_libgcc_mutex 0.1 conda_forge", name: "_libgcc_mutex", version: "==0.1", build: "conda_forge", bn: "=*", str: "_libgcc_mutex==0.1=conda_forge"},
		{in: "_libgcc_mutex    0.1       conda_forge     ", name: "_libgcc_mutex", version: "==0.1", build: "conda_forge", bn: "=*", str: "_libgcc_mutex==0.1=conda_forge"},
		{in: "ipykernel", name: "ipykernel", version: "=*", str: "ipykernel", onlyName: 1},
		{in: "ipykernel ", name: "ipykernel", version: "=*", onlyName: 1},
		// MatchSpec names are case insensitive (mamba issue 4064).
		{in: "GDAL!=3.6.*,!=3.7.*", name: "gdal", version: "!=3.6.*,!=3.7.*", str: `gdal[version="!=3.6.*,!=3.7.*"]`},
		{in: "disperse=v0.9.24", name: "disperse", version: "=v0.9.24", build: "*", bn: "=*", str: "disperse=v0.9.24"},
		{in: "disperse v0.9.24", name: "disperse", version: "==v0.9.24", build: "*", bn: "=*", str: "disperse==v0.9.24"},
		{in: "importlib-metadata  # drop this when dropping Python 3.8", name: "importlib-metadata", version: "=*", str: "importlib-metadata"},
		{in: "foo=V0.9.24", name: "foo", version: "=v0.9.24", build: "*", bn: "=*", str: "foo=v0.9.24"},
		{in: "numpy 1.7*", name: "numpy", version: "=1.7", condaBuild: "numpy 1.7.*", str: "numpy=1.7"},
		{in: "conda-forge:pypi:xtensor==0.12.3", name: "xtensor", version: "==0.12.3", channel: "conda-forge", ns: "pypi", str: "conda-forge:pypi:xtensor==0.12.3"},
		{in: "numpy[version='1.7|1.8']", name: "numpy", version: "==1.7|==1.8", str: `numpy[version="==1.7|==1.8"]`},
		{in: "conda-forge/linux-64::xtensor==0.12.3", name: "xtensor", version: "==0.12.3", location: "conda-forge", subdirs: "linux-64", str: "conda-forge[linux-64]::xtensor==0.12.3"},
		{in: "conda-forge::foo[build=bld](target=blarg,optional)", name: "foo", version: "=*", location: "conda-forge", build: "bld", optional: true, str: "conda-forge::foo=*=bld[optional]"},
		{in: "python[build_number=3]", name: "python", version: "=*", bn: "=3", str: `python[build_number="=3"]`},
		{in: "python[build_number='<=3']", name: "python", bn: "<=3", str: `python[build_number="<=3"]`},
		{
			in:   "https://conda.anaconda.org/conda-forge/linux-64/ncurses-6.4-h59595ed_2.conda#7dbaa197d7ba6032caf7ae7f32c1efa0",
			name: "ncurses", version: "==6.4", build: "h59595ed_2",
			channel: "https://conda.anaconda.org/conda-forge/linux-64/ncurses-6.4-h59595ed_2.conda",
			fn:      "ncurses-6.4-h59595ed_2.conda", md5: "7dbaa197d7ba6032caf7ae7f32c1efa0",
			str: "https://conda.anaconda.org/conda-forge/linux-64/ncurses-6.4-h59595ed_2.conda#7dbaa197d7ba6032caf7ae7f32c1efa0",
		},
		{
			in:   "https://conda.anaconda.org/conda-forge/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
			name: "_libgcc_mutex", version: "==0.1", build: "conda_forge",
			channel: "https://conda.anaconda.org/conda-forge/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
			fn:      "_libgcc_mutex-0.1-conda_forge.tar.bz2",
			str:     "https://conda.anaconda.org/conda-forge/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
		},
		{
			in:   "https://conda.anaconda.org/conda-forge/linux-64/libgcc-ng-11.2.0-h1d223b6_13.tar.bz2",
			name: "libgcc-ng", version: "==11.2.0", build: "h1d223b6_13",
			channel: "https://conda.anaconda.org/conda-forge/linux-64/libgcc-ng-11.2.0-h1d223b6_13.tar.bz2",
			fn:      "libgcc-ng-11.2.0-h1d223b6_13.tar.bz2",
			str:     "https://conda.anaconda.org/conda-forge/linux-64/libgcc-ng-11.2.0-h1d223b6_13.tar.bz2",
		},
		{
			in:   "https://conda.anaconda.org/conda-canary/linux-64/conda-4.3.21.post699+1dab973-py36h4a561cd_0.tar.bz2",
			name: "conda", version: "==4.3.21.post699+1dab973", build: "py36h4a561cd_0",
			str: "https://conda.anaconda.org/conda-canary/linux-64/conda-4.3.21.post699+1dab973-py36h4a561cd_0.tar.bz2",
		},
		{
			in:   "/srv/downloads/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
			name: "_libgcc_mutex", version: "==0.1", build: "conda_forge",
			channel: "/srv/downloads/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
			fn:      "_libgcc_mutex-0.1-conda_forge.tar.bz2",
			str:     "/srv/downloads/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
		},
		{
			in:   "xtensor[url=file:///srv/downloads/xtensor-0.21.4-hc9558a2_0.tar.bz2]",
			name: "xtensor", channel: "file:///srv/downloads/xtensor-0.21.4-hc9558a2_0.tar.bz2",
			// libmamba prints the bare URL, which reads back with the file
			// name's version and build pinned; goomba's text form is
			// lossless, so the open version stays in brackets.
			str: `xtensor[url="file:///srv/downloads/xtensor-0.21.4-hc9558a2_0.tar.bz2"]`,
		},
		{in: "foo=1.0=2", name: "foo", version: "=1.0", build: "2", condaBuild: "foo 1.0.* 2", str: "foo=1.0=2"},
		{in: "foo   =    1.0    =    2", name: "foo", version: "=1.0", build: "2", condaBuild: "foo 1.0.* 2", str: "foo=1.0=2"},
		{
			in:   "foo=1.0=2[md5=123123123, license=BSD-3, fn='test 123.tar.bz2']",
			name: "foo", version: "=1.0", build: "2", condaBuild: "foo 1.0.* 2",
			str: `foo=1.0=2[fn="test 123.tar.bz2",md5=123123123,license=BSD-3]`,
		},
		{
			in:      "foo=1.0=2[md5=123123123, license=BSD-3, fn='test 123.tar.bz2', url='abcdef']",
			channel: "abcdef", name: "foo", version: "=1.0", build: "2", condaBuild: "foo 1.0.* 2",
			str: `abcdef::foo=1.0=2[fn="test 123.tar.bz2",md5=123123123,license=BSD-3]`,
		},
		{
			in:      `defaults::numpy=1.8=py27_0 [name="pytorch",channel='anaconda',version=">=1.8,<2|1.9", build='3']`,
			channel: "anaconda", name: "numpy", version: "=1.8", build: "py27_0", str: "anaconda::numpy=1.8=py27_0",
		},
		// The next two differ from upstream, which expects libmamba's
		// precedence: ">=1.8,(<2|==1.9)" and
		// ">1.8,((<2|==1.7),(!=1.9,~=1.7.1))". goomba uses conda's (see
		// VersionSpec) and writes same-operator chains without parentheses.
		{
			in:      `defaults::numpy [ name="pytorch",channel='anaconda',version=">=1.8,<2|1.9", build='3']`,
			channel: "anaconda", name: "numpy", version: "(>=1.8,<2)|==1.9", build: "3",
			str: `anaconda::numpy[version="(>=1.8,<2)|==1.9",build="3"]`,
		},
		{
			in:   "numpy >1.8,<2|==1.7,!=1.9,~=1.7.1 py34_0",
			name: "numpy", version: "(>1.8,<2)|(==1.7,!=1.9,~=1.7.1)", build: "py34_0",
			str: `numpy[version="(>1.8,<2)|(==1.7,!=1.9,~=1.7.1)",build="py34_0"]`,
		},
		{in: "python-graphviz~=0.20", name: "python-graphviz", version: "~=0.20", str: "python-graphviz~=0.20"},
		{in: "python-graphviz  ~=      0.20", name: "python-graphviz", version: "~=0.20", str: "python-graphviz~=0.20"},
		{in: "python[version='~=3.11.0',build=*_cpython]", name: "python", version: "~=3.11.0", build: "*_cpython", str: `python[version="~=3.11.0",build="*_cpython"]`},
		{in: "*[md5=fewjaflknd]", name: "*", md5: "fewjaflknd", str: "*[md5=fewjaflknd]"},
		{in: "libblas=*=*mkl", name: "libblas", version: "=*", build: "*mkl", str: `libblas[build="*mkl"]`, condaBuild: "libblas * *mkl"},
		// '*' is part of the version, not the glob.
		{in: "libblas=0.15*", name: "libblas", version: "=0.15*", build: "*", str: "libblas=0.15*", condaBuild: "libblas 0.15*.*"},
		{in: "xtensor =0.15*", name: "xtensor", version: "=0.15*", build: "*", str: "xtensor=0.15*", condaBuild: "xtensor 0.15*.*"},
		{in: "numpy=1.20", name: "numpy", version: "=1.20", build: "*", str: "numpy=1.20"},
		{in: "conda-forge::tzdata", channel: "conda-forge", name: "tzdata", version: "=*", build: "*", str: "conda-forge::tzdata"},
		{in: "conda-forge/noarch::tzdata", channel: "conda-forge[noarch]", name: "tzdata", version: "=*", build: "*", str: "conda-forge[noarch]::tzdata"},
		// Upstream's "conda-forge[noarch]::tzdata" section parses
		// "conda-forge/noarch::tzdata" again; both forms are tested here.
		{in: "conda-forge[noarch]::tzdata", channel: "conda-forge[noarch]", name: "tzdata", version: "=*", build: "*", str: "conda-forge[noarch]::tzdata"},
		{in: "pkgs/main::tzdata", channel: "pkgs/main", name: "tzdata", version: "=*", build: "*", str: "pkgs/main::tzdata"},
		{in: "pkgs/main/noarch::tzdata", channel: "pkgs/main[noarch]", name: "tzdata", version: "=*", build: "*", str: "pkgs/main[noarch]::tzdata"},
		{in: "conda-forge[noarch]::tzdata[subdir=linux64]", channel: "conda-forge[noarch]", subdirs: "noarch", name: "tzdata", version: "=*", build: "*", str: "conda-forge[noarch]::tzdata"},
		{in: "conda-forge::tzdata[subdir=mamba-37]", channel: "conda-forge[mamba-37]", subdirs: "mamba-37", name: "tzdata", version: "=*", build: "*", str: "conda-forge[mamba-37]::tzdata"},
		{
			in:      "conda-canary/linux-64::conda==4.3.21.post699+1dab973=py36h4a561cd_0",
			channel: "conda-canary[linux-64]", subdirs: "linux-64", name: "conda", version: "==4.3.21.post699+1dab973", build: "py36h4a561cd_0",
			str: "conda-canary[linux-64]::conda==4.3.21.post699+1dab973=py36h4a561cd_0",
		},
		{in: "libblas[build=^.*(accelerate|mkl)$]", name: "libblas", build: "^.*(accelerate|mkl)$"},
	}
	for _, c := range cases {
		c.check(t)
	}

	// numpy~=1.26.0 round-trips to an equal spec.
	ms := mustMatchSpec(t, "numpy~=1.26.0")
	if !mustMatchSpec(t, ms.String()).Equal(ms) {
		t.Error("numpy~=1.26.0 round trip")
	}
	// track_features are a sorted set.
	if m := mustMatchSpec(t, `blas[track_features="mkl avx"]`); !reflect.DeepEqual(m.TrackFeatures, []string{"avx", "mkl"}) || m.String() != `blas[track_features="avx mkl"]` || m.Name.String() != "blas" {
		t.Errorf("track_features: %v %q", m.TrackFeatures, m.String())
	}
	// Platforms of a channel.
	if m := mustMatchSpec(t, "conda-forge/linux-64::xtensor==0.12.3"); !reflect.DeepEqual(m.Subdirs(), []string{"linux-64"}) {
		t.Errorf("subdirs = %v", m.Subdirs())
	}
	if m := mustMatchSpec(t, "libblas[build=^.*(accelerate|mkl)$]"); m.Build.IsGlob() {
		t.Error("regex build is a glob")
	}
	if m := mustMatchSpec(t, "python[version='*,=3.12.*']"); m.Version.expressionSize() != 3 || m.IsSimple() || m.Version.IsAny() {
		t.Errorf("python[version='*,=3.12.*']: size %d", m.Version.expressionSize())
	}
}

func TestParseMatchSpecErrors(t *testing.T) {
	// From libmamba ("foo V0.9.24"): upper-case versions need an operator.
	_, err := ParseMatchSpec("foo V0.9.24")
	if err == nil || !strings.Contains(err.Error(), `invalid version predicate "V0.9.24"`) {
		t.Errorf("foo V0.9.24: err = %v", err)
	}
	if !errors.Is(err, ErrMatchSpec) || !errors.Is(err, ErrVersionSpec) {
		t.Errorf("foo V0.9.24: err %v should be both ErrMatchSpec and ErrVersionSpec", err)
	}
	bad := []string{
		">=1.0",
		"=1.0",
		"numpy >=1..2",
		"numpy[version='>=1.2'",
		"numpy[build=^(unclosed$]",
		"numpy[build_number=x]",
		"numpy=1.2=^(bad$",
		"https://x/pkg.conda#nothex",
		"https://x/pkg.conda#abcd",
		"xtensor[url=https://x/xtensor-1-0.conda, fn=other.conda]",
		"pkg.conda",
		"nodash.conda",
		"conda-forge]::numpy",
	}
	for _, s := range bad {
		if m, err := ParseMatchSpec(s); !errors.Is(err, ErrMatchSpec) {
			t.Errorf("ParseMatchSpec(%q) = %q, %v; want ErrMatchSpec", s, m.String(), err)
		}
	}
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp ("parse_url").
// The Windows-only case is skipped.
func TestParseMatchSpecURL(t *testing.T) {
	cases := []msCase{
		{in: "https://conda.com/pkg-2-bld.conda", name: "pkg", version: "==2", str: "https://conda.com/pkg-2-bld.conda", build: "bld", fn: "pkg-2-bld.conda"},
		{in: "/srv/mamba/micromamba/tests/data/cph_test_data-0.0.1-0.tar.bz2", name: "cph_test_data", version: "==0.0.1", str: "/srv/mamba/micromamba/tests/data/cph_test_data-0.0.1-0.tar.bz2", build: "0", fn: "cph_test_data-0.0.1-0.tar.bz2"},
		// Added for goomba: percent-encoded names, hashes, derived channel and subdir.
		{
			in:   "https://conda.anaconda.org/conda-forge/linux-64/x264-1%21164.3095-h166bdaf_2.tar.bz2",
			name: "x264", version: "==1!164.3095", build: "h166bdaf_2", fn: "x264-1!164.3095-h166bdaf_2.tar.bz2",
			location: "https://conda.anaconda.org/conda-forge", subdirs: "linux-64",
		},
		{
			in:   "https://conda.anaconda.org/conda-forge/noarch/wheel-0.46.3-pyhd8ed1ab_0.conda#sha256:" + strings.Repeat("ab", 32),
			name: "wheel", version: "==0.46.3", build: "pyhd8ed1ab_0", location: "https://conda.anaconda.org/conda-forge", subdirs: "noarch",
			str: `wheel==0.46.3=pyhd8ed1ab_0[url="https://conda.anaconda.org/conda-forge/noarch/wheel-0.46.3-pyhd8ed1ab_0.conda",sha256=` + strings.Repeat("ab", 32) + "]",
		},
	}
	for _, c := range cases {
		c.check(t)
		if m := mustMatchSpec(t, c.in); m.URL == "" || m.Fn == "" {
			t.Errorf("%q: not a file spec", c.in)
		}
	}
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp ("MatchSpec::extract_name").
func TestExtractName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"python", "python"},
		{"python >=3.11", "python"},
		{"numpy>=2", "numpy"},
		{"python=3.12", "python"},
		{"libzstd!=1.5", "libzstd"},
		{"openssl~=3.0", "openssl"},
		{"   scipy >=1.11", "scipy"},
		{"CppInterOp>=1.9", "cppinterop"},
		{"   PyThOn >=3.11", "python"},
	}
	for _, c := range cases {
		if got, err := ExtractName(c.in); err != nil || got != c.want {
			t.Errorf("ExtractName(%q) = %q, %v", c.in, got, err)
		}
	}
	for _, s := range []string{"", "   ", ">=1.0"} {
		if _, err := ExtractName(s); !errors.Is(err, ErrMatchSpec) {
			t.Errorf("ExtractName(%q) err = %v", s, err)
		}
	}
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp ("Conda discrepancies").
func TestMatchSpecCondaDiscrepancies(t *testing.T) {
	// conda parses python=3.7 and python=3.7=bld differently; libmamba does not.
	m := mustMatchSpec(t, "python=3.7=bld")
	if m.Version.String() != "=3.7" || m.Build.String() != "bld" {
		t.Errorf("python=3.7=bld: %q %q", m.Version, m.Build)
	}
	// Supported by conda, refused here: use version=">3".
	_, err := ParseMatchSpec("python[version>3]")
	if err == nil || !strings.Contains(err.Error(), `use "version='>3'" instead`) {
		t.Errorf("python[version>3]: %v", err)
	}
	// Ambiguous: version= is an assignment, so 3.7 means ==3.7.
	if m := mustMatchSpec(t, "python[version=3.7]"); m.Version.String() != "==3.7" {
		t.Errorf("python[version=3.7]: %q", m.Version)
	}
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp ("is_simple").
func TestMatchSpecIsSimple(t *testing.T) {
	for _, s := range []string{
		"libblas", "libblas=12.9=abcdef", "libblas=0.15*", "libblas=0.15.*", "libblas *",
		"libblas[version=12.2]", "xtensor =0.15*", "python>=3.6,<4.0",
	} {
		if !mustMatchSpec(t, s).IsSimple() {
			t.Errorf("%q should be simple", s)
		}
	}
	for _, s := range []string{
		"pkg[build_number=3]", "pkg[md5=85094328554u9543215123]", "pkg[sha256=0320104934325453]",
		"pkg[license=MIT]", "pkg[track_features=mkl]", "pkg[version='(>2,<3)|=4']",
		"conda-forge::pkg", "pypi:pkg", "python[version='*,=3.12.*']", "pkg *.0.*", "pkg *.1",
	} {
		if mustMatchSpec(t, s).IsSimple() {
			t.Errorf("%q should not be simple", s)
		}
	}
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp ("MatchSpec::to_named_spec").
func TestMatchSpecNameOnly(t *testing.T) {
	for _, s := range []string{"foo", "foo "} {
		m := mustMatchSpec(t, s)
		if !m.Equal(m.NameOnly()) {
			t.Errorf("%q: NameOnly differs", s)
		}
	}
	for _, s := range []string{"foo>1.0", "foo =*", "foo=3=bld"} {
		m := mustMatchSpec(t, s)
		n := m.NameOnly()
		if n.Name != m.Name || !n.Version.IsAny() || !n.Build.IsAny() || !n.BuildNumber.IsAny() {
			t.Errorf("%q: NameOnly = %q", s, n)
		}
	}
}

// pkg is libmamba's test Pkg: a record plus its parsed version.
type pkg struct {
	name, version, build string
	bn                   uint64
	md5, sha256, license string
	platform             string
	tf                   []string
}

func (p pkg) record() *record.PackageRecord {
	return &record.PackageRecord{
		Name: p.name, Version: p.version, Build: p.build, BuildNumber: p.bn,
		MD5: p.md5, SHA256: p.sha256, License: p.license, Subdir: p.platform, TrackFeatures: p.tf,
	}
}

func matchesPkg(t *testing.T, m MatchSpec, p pkg) bool {
	t.Helper()
	r := p.record()
	vs := p.version
	if vs == "" {
		vs = "0"
	}
	v := mustVersion(t, vs)
	got := m.MatchesVersion(r, v)
	if m.Matches(r) != got {
		t.Errorf("%q: Matches and MatchesVersion disagree on %+v", m, p)
	}
	return got
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp ("MatchSpec::contains").
func TestMatchSpecContains(t *testing.T) {
	full := func(name, ver, build string, bn uint64, md5, license string, tf ...string) pkg {
		return pkg{name: name, version: ver, build: build, bn: bn, md5: md5, sha256: "somesha256", license: license, platform: "linux-64", tf: tf}
	}
	cases := []struct {
		spec string
		in   []pkg
		out  []pkg
	}{
		{"python", []pkg{{name: "python"}}, []pkg{{name: "pypy"}}},
		{"py*", []pkg{{name: "python"}, {name: "pypy"}}, []pkg{{name: "rust"}}},
		{
			"py*>=3.7",
			[]pkg{{name: "python", version: "3.7"}, {name: "python", version: "3.7", build: "bld"}},
			[]pkg{{name: "pypy", version: "3.6"}, {name: "rust", version: "3.7"}, {name: "pypy", version: "3.6", build: "bld"}, {name: "rust", version: "3.7", build: "bld"}},
		},
		{
			"py*>=3.7=*cpython",
			[]pkg{{name: "python", version: "3.7", build: "37_cpython"}},
			[]pkg{{name: "pypy", version: "3.6", build: "cpython"}, {name: "pypy", version: "3.8", build: "pypy"}, {name: "rust", version: "3.7", build: "cpyhton"}},
		},
		{
			"py*[version='>=3.7', build=*cpython]",
			[]pkg{{name: "python", version: "3.7", build: "37_cpython"}},
			[]pkg{{name: "pypy", version: "3.6", build: "cpython"}, {name: "pypy", version: "3.8", build: "pypy"}, {name: "rust", version: "3.7", build: "cpyhton"}},
		},
		{"pkg[build_number='>3']", []pkg{{name: "pkg", bn: 4}}, []pkg{{name: "pkg", bn: 2}}},
		{
			"name *,*.* build*",
			[]pkg{{name: "name", version: "3.7", build: "build_foo"}},
			[]pkg{{name: "name", version: "3", build: "build_foo"}, {name: "name", version: "3.7", build: "bar"}},
		},
		{
			"pkg[md5=helloiamnotreallymd5haha]",
			[]pkg{{name: "pkg", md5: "helloiamnotreallymd5haha"}},
			[]pkg{{name: "pkg", md5: "helloiamnotreallymd5hahaevillaugh"}, {name: "pkg", md5: "hello"}, {name: "pkg"}},
		},
		{
			"pkg[sha256=helloiamnotreallysha256hihi]",
			[]pkg{{name: "pkg", sha256: "helloiamnotreallysha256hihi"}},
			[]pkg{{name: "pkg", sha256: "helloiamnotreallysha256hihicutelaugh"}, {name: "pkg", sha256: "hello"}, {name: "pkg"}},
		},
		{
			"pkg[license=helloiamnotreallylicensehoho]",
			[]pkg{{name: "pkg", license: "helloiamnotreallylicensehoho"}},
			[]pkg{{name: "pkg", license: "helloiamnotreallylicensehohodadlaugh"}, {name: "pkg", license: "hello"}, {name: "pkg"}},
		},
		{
			"pkg[subdir='linux-64,linux-64-512']",
			[]pkg{{name: "pkg", platform: "linux-64"}, {name: "pkg", platform: "linux-64-512"}},
			[]pkg{{name: "pkg", platform: "linux"}, {name: "pkg", platform: "linux-512"}, {name: "pkg", platform: ""}, {name: "pkg", platform: "linux-64,linux-64-512"}},
		},
		{
			"pkg[track_features='mkl,openssl']",
			[]pkg{{name: "pkg", tf: []string{"openssl", "mkl"}}},
			[]pkg{{name: "pkg", tf: []string{"openssl"}}, {name: "pkg", tf: []string{"mkl"}}, {name: "pkg"}},
		},
		{
			"py*>=3.7=bld[build_number='<=2', md5=lemd5, track_features='mkl,openssl']",
			[]pkg{full("python", "3.8.0", "bld", 2, "lemd5", "MIT", "openssl", "mkl"), full("python", "3.12.0", "bld", 0, "lemd5", "GPL", "openssl", "mkl")},
			[]pkg{full("python", "3.3.0", "bld", 0, "lemd5", "GPL", "openssl", "mkl"), full("python", "3.12.0", "bld", 0, "wrong", "GPL", "openssl", "mkl")},
		},
		{
			"pytorch=2.3.1=py3.10_cuda11.8*",
			[]pkg{full("pytorch", "2.3.1", "py3.10_cuda11.8_cudnn8.7.0_0", 0, "lemd5", "GPL")},
			nil,
		},
		{
			"pytorch~=2.3.1=py3.10_cuda11.8*",
			[]pkg{full("pytorch", "2.3.1", "py3.10_cuda11.8_cudnn8.7.0_0", 0, "lemd5", "GPL"), full("pytorch", "2.3.2", "py3.10_cuda11.8_cudnn8.7.0_0", 0, "lemd5", "GPL")},
			[]pkg{full("pytorch", "2.4.0", "py3.10_cuda11.8_cudnn8.7.0_0", 0, "lemd5", "GPL"), full("pytorch", "3.0", "py3.10_cuda11.8_cudnn8.7.0_0", 0, "lemd5", "GPL"), full("pytorch", "2.3.0", "py3.10_cuda11.8_cudnn8.7.0_0", 0, "lemd5", "GPL")},
		},
		{
			"numpy~=1.26.0",
			[]pkg{full("numpy", "1.26.0", "py310h1d0b8b9_0", 0, "lemd5", "GPL"), full("numpy", "1.26.1", "py310h1d0b8b9_0", 0, "lemd5", "GPL")},
			[]pkg{full("numpy", "1.27", "py310h1d0b8b9_0", 0, "lemd5", "GPL"), full("numpy", "2.0.0", "py310h1d0b8b9_1", 1, "lemd5", "GPL"), full("numpy", "1.25.0", "py310h1d0b8b9_0", 0, "lemd5", "GPL")},
		},
		{
			"numpy~=1.26",
			[]pkg{full("numpy", "1.26.0", "py310h1d0b8b9_0", 0, "lemd5", "GPL"), full("numpy", "1.26.1", "py310h1d0b8b9_0", 0, "lemd5", "GPL"), full("numpy", "1.27", "py310h1d0b8b9_0", 0, "lemd5", "GPL")},
			[]pkg{full("numpy", "2.0.0", "py310h1d0b8b9_1", 1, "lemd5", "GPL")},
		},
		{
			"python=3.*",
			[]pkg{full("python", "3.12.0", "bld", 0, "lemd5", "some-license")},
			[]pkg{full("python", "2.7.12", "bld", 0, "lemd5", "some-license")},
		},
		{
			"python=3.*.1",
			[]pkg{full("python", "3.12.1", "bld", 0, "lemd5", "some-license")},
			[]pkg{full("python", "3.12.0", "bld", 0, "lemd5", "some-license")},
		},
		{
			"python=*.13.1",
			[]pkg{full("python", "3.13.1", "bld", 0, "lemd5", "some-license")},
			[]pkg{full("python", "3.12.0", "bld", 0, "lemd5", "some-license")},
		},
		// Added for goomba.
		{
			"python_abi 3.11.* *_cp311",
			[]pkg{{name: "python_abi", version: "3.11", build: "8_cp311"}},
			[]pkg{{name: "python_abi", version: "3.12", build: "8_cp312"}, {name: "python_abi", version: "3.11", build: "8_cp311t"}},
		},
		{
			"__glibc >=2.17,<3.0.a0",
			[]pkg{{name: "__glibc", version: "2.17"}, {name: "__glibc", version: "2.39"}},
			[]pkg{{name: "__glibc", version: "2.12"}, {name: "__glibc", version: "3.0"}},
		},
		{
			"numpy[version='>=1.2',build=py*]",
			[]pkg{{name: "numpy", version: "1.26.4", build: "py311h64a7726_0"}},
			[]pkg{{name: "numpy", version: "1.1", build: "py311h64a7726_0"}, {name: "numpy", version: "1.26.4", build: "h1_0"}},
		},
		{
			"conda-forge::numpy>=1.2",
			[]pkg{{name: "numpy", version: "1.26.4"}},
			[]pkg{{name: "numpy", version: "1.1"}},
		},
		{
			"pkg[md5=ABCDEF0123456789ABCDEF0123456789]",
			[]pkg{{name: "pkg", md5: "abcdef0123456789abcdef0123456789"}},
			[]pkg{{name: "pkg", md5: "abcdef0123456789abcdef012345678a"}},
		},
		{
			"pkg[fn=pkg-1-0.conda]",
			nil,
			[]pkg{{name: "pkg"}},
		},
	}
	for _, c := range cases {
		m := mustMatchSpec(t, c.spec)
		for _, p := range c.in {
			if !matchesPkg(t, m, p) {
				t.Errorf("%q should match %+v", c.spec, p)
			}
		}
		for _, p := range c.out {
			if matchesPkg(t, m, p) {
				t.Errorf("%q should not match %+v", c.spec, p)
			}
		}
	}
	// An unparsable record version never matches.
	if mustMatchSpec(t, "foo").Matches(&record.PackageRecord{Name: "foo", Version: "1..2"}) {
		t.Error("unparsable version matched")
	}
	// Fn is checked when present.
	m := mustMatchSpec(t, "pkg[fn=pkg-1-0.conda]")
	if !m.Matches(&record.PackageRecord{Name: "pkg", Version: "1", Fn: "pkg-1-0.conda"}) ||
		m.Matches(&record.PackageRecord{Name: "pkg", Version: "1", Fn: "pkg-1-0.tar.bz2"}) {
		t.Error("fn")
	}
}

// Cases from libmamba tests/src/specs/test_match_spec.cpp
// ("MatchSpec comparability and hashability").
func TestMatchSpecEqual(t *testing.T) {
	s1 := mustMatchSpec(t, "py*>=3.7=bld[build_number='<=2', md5=lemd5, track_features='mkl,openssl']")
	s2 := mustMatchSpec(t, "py*>=3.7=bld[build_number='<=2', md5=lemd5, track_features='mkl,openssl']")
	s3 := mustMatchSpec(t, "py*>=3.7=bld[build_number='<=2', md5=lemd5, track_features='mkl']")
	if !s1.Equal(s2) || s1.Equal(s3) || s1.String() != s2.String() || s1.String() == s3.String() {
		t.Error("comparability")
	}
}

func TestMatchSpecChannelURL(t *testing.T) {
	cases := []struct{ spec, alias, want string }{
		{"numpy", DefaultChannelAlias, ""},
		{"conda-forge::numpy", DefaultChannelAlias, "https://conda.anaconda.org/conda-forge"},
		{"conda-forge/linux-64::numpy", "https://my.mirror/", "https://my.mirror/conda-forge"},
		{"https://repo.example.org/chan/noarch::numpy", DefaultChannelAlias, "https://repo.example.org/chan"},
		{"pkgs/main::numpy", DefaultChannelAlias, "https://repo.anaconda.com/pkgs/main"},
	}
	for _, c := range cases {
		got, err := mustMatchSpec(t, c.spec).ChannelURL(c.alias)
		if err != nil || got != c.want {
			t.Errorf("%q: ChannelURL = %q, %v; want %q", c.spec, got, err, c.want)
		}
	}
	if _, err := mustMatchSpec(t, "defaults::numpy").ChannelURL(DefaultChannelAlias); !errors.Is(err, ErrChannel) {
		t.Errorf("defaults: err = %v", err)
	}
}

// TestMatchSpecBriefForms covers the forms named in the work package brief.
func TestMatchSpecBriefForms(t *testing.T) {
	cases := []msCase{
		{in: "numpy 1.2.* py39*", name: "numpy", version: "=1.2", build: "py39*", str: `numpy[version="=1.2",build="py39*"]`},
		{in: "conda-forge::numpy>=1.2", name: "numpy", version: ">=1.2", location: "conda-forge", str: "conda-forge::numpy>=1.2"},
		{in: "numpy[version='>=1.2',build=py*]", name: "numpy", version: ">=1.2", build: "py*", str: `numpy[version=">=1.2",build="py*"]`},
		{in: "python=3.11", name: "python", version: "=3.11", str: "python=3.11"},
		{in: "python==3.11", name: "python", version: "==3.11", str: "python==3.11"},
		{in: "python_abi 3.11.* *_cp311", name: "python_abi", version: "=3.11", build: "*_cp311", str: `python_abi[version="=3.11",build="*_cp311"]`},
		{in: "__glibc >=2.17,<3.0.a0", name: "__glibc", version: ">=2.17,<3.0.a0", str: `__glibc[version=">=2.17,<3.0.a0"]`},
		{in: "pkg *.0.*", name: "pkg", version: "*.0.*", str: `pkg[version="*.0.*"]`},
		{in: "pkg 1.*.3 bld", name: "pkg", version: "1.*.3", build: "bld", str: `pkg[version="1.*.3",build="bld"]`},
		{in: "pkg[subdir=linux-64]", name: "pkg", subdirs: "linux-64", str: `pkg[subdir="linux-64"]`},
		// As in libmamba, spaces after ',', '=', '<' and '>' are dropped
		// everywhere, even inside quotes.
		{in: "pkg[license='MIT, BSD']", name: "pkg", str: `pkg[license="MIT,BSD"]`},
		{in: "pkg[license='MIT BSD']", name: "pkg", str: `pkg[license="MIT BSD"]`},
		{in: "  ", name: "*", str: "*"},
	}
	for _, c := range cases {
		c.check(t)
	}
}

// TestMatchSpecUnknownKeys checks that bracket keys outside the known set
// are errors (libmamba ignores them, so a typo matched everything).
func TestMatchSpecUnknownKeys(t *testing.T) {
	for _, s := range []string{"foo[versoin='<1']", "foo[build_nmber=2]", "foo[version='<1',sha=abc]", "foo(target=x,frob)", "pin_compatible('xsimd')"} {
		if _, err := ParseMatchSpec(s); !errors.Is(err, ErrMatchSpec) || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("ParseMatchSpec(%q) err = %v", s, err)
		}
	}
	// name= and target= are accepted and ignored, as in libmamba.
	for _, s := range []string{"foo[name=bar]", "conda-forge::foo[build=bld](target=blarg,optional)"} {
		if m := mustMatchSpec(t, s); m.Name.String() != "foo" {
			t.Errorf("%q: name %q", s, m.Name)
		}
	}
}

// TestMatchSpecLiteralBuilds checks that builds are literal globs: only
// "^...$" is a regular expression (libmamba made "abc+def" a regex that
// matched "abcdef" but not "abc+def").
func TestMatchSpecLiteralBuilds(t *testing.T) {
	cases := []struct {
		spec, build string
		want        bool
	}{
		{"foo 1.0 abc+def", "abc+def", true},
		{"foo 1.0 abc+def", "abcdef", false},
		{"foo 1.0 abc+def", "abccdef", false},
		{"foo 1.0 py3.10_0", "py3.10_0", true},
		{"foo 1.0 py3.10_0", "py3x10_0", false},
		{"foo 1.0 py3.10*", "py3.10_cuda11.8", true},
		{"foo 1.0 py3.10*", "py3x10_cuda11.8", false},
		{"foo 1.0 ^py3.10.*$", "py3x10_0", true},
	}
	for _, c := range cases {
		m := mustMatchSpec(t, c.spec)
		r := &record.PackageRecord{Name: "foo", Version: "1.0", Build: c.build}
		if got := m.Matches(r); got != c.want {
			t.Errorf("%q (build %q) matches build %q = %v, want %v", c.spec, m.Build, c.build, got, c.want)
		}
		if m2 := mustMatchSpec(t, m.String()); !m2.Equal(m) {
			t.Errorf("%q: String %q does not round-trip", c.spec, m)
		}
	}
	// Half a regex, or characters no build string uses, are errors.
	for _, s := range []string{"foo 1.0 ^abc", "foo 1.0 abc$", "foo[build=':']", "foo[build='a b']", "foo 1.0 a/b"} {
		if _, err := ParseMatchSpec(s); !errors.Is(err, ErrStringMatcher) {
			t.Errorf("ParseMatchSpec(%q) err = %v", s, err)
		}
	}
}

// TestMatchSpecCredentials checks that channels carrying credentials are
// refused wherever they appear, so secrets never round-trip into strings.
func TestMatchSpecCredentials(t *testing.T) {
	for _, s := range []string{
		"foo[channel='https://user:pw@host/c']",
		"https://user:pw@host/c::foo",
		"foo[url='https://user@host/c/linux-64/foo-1-0.conda']",
		"https://host/t/tk-123/conda-forge::foo",
		"https://host/t/tk-123/conda-forge/linux-64/foo-1-0.conda",
	} {
		if _, err := ParseMatchSpec(s); !errors.Is(err, ErrMatchSpec) || !errors.Is(err, ErrChannel) {
			t.Errorf("ParseMatchSpec(%q) err = %v", s, err)
		}
	}
	if _, err := ParseMatchSpec("file:///srv/t/chan::foo"); err != nil {
		t.Errorf("file URL with a t directory: %v", err)
	}
}

// TestMatchSpecStringLossless checks that String round-trips every field,
// in particular for package URL specs (from the WP7 review, which found
// distinct specs sharing one String: a subdir= next to url= was dropped,
// and a URL spec with an open version printed as the bare URL).
func TestMatchSpecStringLossless(t *testing.T) {
	const u = "https://conda.anaconda.org/x/linux-64/foo-1.0-0.conda"
	for _, c := range []struct{ in, str string }{
		{u, u},
		{u + "#" + strings.Repeat("a", 32), u + "#" + strings.Repeat("a", 32)},
		{"foo==1.0=0[url=" + u + "]", u},
		{"foo[url=" + u + "]", `foo[url="` + u + `"]`},
		{"foo[url=" + u + ",subdir=osx-64]", `foo[url="` + u + `",subdir="osx-64"]`},
		{"foo[subdir=osx-64,url=" + u + "]", `foo[url="` + u + `",subdir="osx-64"]`},
		{"foo[url=" + u + ",subdir=linux-64]", `foo[url="` + u + `"]`},
		{"ns:foo[url=" + u + "]", `ns:foo[url="` + u + `"]`},
		{"*[url=" + u + "]", `*[url="` + u + `"]`},
		{"bar[url=" + u + "]", `bar[url="` + u + `"]`},
		{"foo==1.0[url=" + u + "]", `foo==1.0[url="` + u + `"]`},
		{"foo[url=" + u + ",sha256=" + strings.Repeat("b", 64) + "]", `foo[url="` + u + `",sha256=` + strings.Repeat("b", 64) + `]`},
	} {
		m, err := ParseMatchSpec(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got := m.String(); got != c.str {
			t.Errorf("ParseMatchSpec(%q).String() = %q, want %q", c.in, got, c.str)
		}
		m2, err := ParseMatchSpec(m.String())
		if err != nil || !m2.Equal(m) {
			t.Errorf("%q: %q reparses to %+v (%v), want %+v", c.in, m.String(), m2, err, m)
		}
	}

	// The reviewer's search: random bracket specs never lose a field.
	attrs := []string{"version='>=1'", "version='1.0'", "build=0", "build='py*'", "build_number=2", "subdir=osx-64",
		"subdir='linux-64,osx-64'", "channel=x/osx-64", "version='==1.0'", "version='1.0.*'", "build_number='>=2'",
		"md5=" + strings.Repeat("A", 32), "subdir=linux-64", "channel=x", "channel=conda-forge", "fn=foo-1.0-0.conda",
		"md5=" + strings.Repeat("a", 32), "sha256=" + strings.Repeat("c", 64), "license=MIT", "license_family=BSD",
		"features=mkl", "track_features=tf", "optional", "name=bar", "target=x", "url=" + u,
		"url=https://conda.anaconda.org/x/foo-1.0-0.tar.bz2", "url=" + u + "[osx-64]"}
	prefixes := []string{"foo", "x::foo", "x/linux-64::foo", "conda-forge::foo", "x[osx-64]::foo", "foo 1.0", "foo 1.0 0",
		"foo=1.0=0", "foo >=1", "x:ns:foo", "x/osx-64::foo", "foo[subdir=osx-64]", "foo 1.0.*", "foo==1.0", "ns:foo", u}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		s := prefixes[rng.Intn(len(prefixes))]
		if n := rng.Intn(4); n > 0 && s != u {
			var parts []string
			for j := 0; j < n; j++ {
				parts = append(parts, attrs[rng.Intn(len(attrs))])
			}
			s += "[" + strings.Join(parts, ",") + "]"
		}
		m, err := ParseMatchSpec(s)
		if err != nil {
			continue
		}
		m2, err := ParseMatchSpec(m.String())
		if err != nil || !m2.Equal(m) {
			t.Fatalf("%q prints as %q, which reparses to %+v (%v); want %+v", s, m.String(), m2, err, m)
		}
	}
}
