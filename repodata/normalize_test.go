package repodata

import (
	"reflect"
	"testing"

	"github.com/richinsley/goomba/record"
)

// Cases from libmamba tests/src/core/test_shard_types.cpp "to_package_info
// conversion". In goomba the record already is a PackageRecord; the
// conversion left is placing it: Fn, URL, Channel and Subdir. The URL base
// goes through resolveBase like an index's base_url.
func TestPlaceRecord(t *testing.T) {
	cases := []struct {
		name     string
		rec      record.PackageRecord
		filename string
		channel  string
		platform string
		baseURL  string
		wantURL  string
	}{
		{
			name: "Basic conversion with all fields",
			rec: record.PackageRecord{
				Name: "test-package", Version: "1.2.3", Build: "py310_0", BuildNumber: 42,
				SHA256: "abc123sha256", MD5: "def456md5",
				Depends: []string{"python >=3.10", "numpy >=1.20"}, Constrains: []string{"scipy <2.0"},
				Noarch: "python", Size: 98765, License: "MIT", Timestamp: 1640995200,
			},
			filename: "test-package-1.2.3-py310_0.tar.bz2", channel: "conda-forge", platform: "linux-64",
			baseURL: "https://conda.anaconda.org/conda-forge/linux-64",
			wantURL: "https://conda.anaconda.org/conda-forge/linux-64/test-package-1.2.3-py310_0.tar.bz2",
		},
		{
			name:     "Conversion with generic noarch",
			rec:      record.PackageRecord{Name: "generic-pkg", Version: "1.0.0", Build: "0", Noarch: "generic"},
			filename: "generic-pkg-1.0.0-0.tar.bz2", channel: "conda-forge", platform: "noarch",
			baseURL: "https://conda.anaconda.org/conda-forge/noarch",
			wantURL: "https://conda.anaconda.org/conda-forge/noarch/generic-pkg-1.0.0-0.tar.bz2",
		},
		{
			name:     "Conversion without noarch",
			rec:      record.PackageRecord{Name: "native-pkg", Version: "2.0.0", Build: "h123_1"},
			filename: "native-pkg-2.0.0-h123_1.conda", channel: "conda-forge", platform: "linux-64",
			baseURL: "https://conda.anaconda.org/conda-forge/linux-64",
			wantURL: "https://conda.anaconda.org/conda-forge/linux-64/native-pkg-2.0.0-h123_1.conda",
		},
		{
			name:     "Conversion without optional hashes",
			rec:      record.PackageRecord{Name: "no-hash-pkg", Version: "3.0.0", Build: "0"},
			filename: "no-hash-pkg-3.0.0-0.tar.bz2", channel: "test-channel", platform: "osx-64",
			baseURL: "https://example.com/test-channel/osx-64",
			wantURL: "https://example.com/test-channel/osx-64/no-hash-pkg-3.0.0-0.tar.bz2",
		},
		{
			name:     "Conversion with optional metadata fields",
			rec:      record.PackageRecord{Name: "metadata-pkg", Version: "1.0.0", Build: "0", License: "BSD-3-Clause", Timestamp: 1234567890},
			filename: "metadata-pkg-1.0.0-0.tar.bz2", channel: "channel", platform: "linux-64",
			baseURL: "https://example.com/channel/linux-64",
			wantURL: "https://example.com/channel/linux-64/metadata-pkg-1.0.0-0.tar.bz2",
		},
		{
			name:     "Conversion without optional metadata fields",
			rec:      record.PackageRecord{Name: "minimal-pkg", Version: "1.0.0", Build: "0"},
			filename: "minimal-pkg-1.0.0-0.tar.bz2", channel: "channel", platform: "linux-64",
			baseURL: "https://example.com/channel/linux-64",
			wantURL: "https://example.com/channel/linux-64/minimal-pkg-1.0.0-0.tar.bz2",
		},
		{
			name:     "URL construction with trailing slash in base_url",
			rec:      record.PackageRecord{Name: "url-test-pkg", Version: "1.0.0", Build: "0"},
			filename: "url-test-pkg-1.0.0-0.tar.bz2", channel: "channel", platform: "win-64",
			baseURL: "https://example.com/channel/win-64/",
			wantURL: "https://example.com/channel/win-64/url-test-pkg-1.0.0-0.tar.bz2",
		},
		{
			name:     "Conversion with empty dependencies and constrains",
			rec:      record.PackageRecord{Name: "no-deps-pkg", Version: "1.0.0", Build: "0"},
			filename: "no-deps-pkg-1.0.0-0.tar.bz2", channel: "channel", platform: "linux-64",
			baseURL: "https://example.com",
			wantURL: "https://example.com/no-deps-pkg-1.0.0-0.tar.bz2",
		},
		{
			name:     "Preserve raw version formatting when available",
			rec:      record.PackageRecord{Name: "conda-forge-pinning", Version: "2026.04.07.10.45.57", Build: "hd8ed1ab_0"},
			filename: "conda-forge-pinning-2026.04.07.10.45.57-hd8ed1ab_0.conda", channel: "conda-forge", platform: "noarch",
			baseURL: "https://conda.anaconda.org/conda-forge/noarch",
			wantURL: "https://conda.anaconda.org/conda-forge/noarch/conda-forge-pinning-2026.04.07.10.45.57-hd8ed1ab_0.conda",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, err := resolveBase(mustURL(t, "https://index.example.org/x/"+c.platform+"/repodata.json"), c.baseURL, urlPolicy{})
			if err != nil {
				t.Fatal(err)
			}
			want := c.rec
			got := c.rec
			(&locator{channel: c.channel, subdir: c.platform, base: base}).place(&got, c.filename)
			if got.URL != c.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, c.wantURL)
			}
			want.Fn, want.URL, want.Channel, want.Subdir = c.filename, c.wantURL, c.channel, c.platform
			if !reflect.DeepEqual(got, want) {
				t.Errorf("placed record = %+v, want %+v", got, want)
			}
		})
	}
}

func TestPlaceEscapesFileName(t *testing.T) {
	base, _ := resolveBase(mustURL(t, "https://example.com/c/noarch/repodata.json"), "", urlPolicy{})
	var r record.PackageRecord
	(&locator{base: base}).place(&r, "pkg-1.0+local-0 x.conda")
	if want := "https://example.com/c/noarch/pkg-1.0+local-0%20x.conda"; r.URL != want {
		t.Errorf("URL = %q, want %q", r.URL, want)
	}
}

func rec(name, version, build, subdir, fn string) *record.PackageRecord {
	return &record.PackageRecord{Name: name, Version: version, Build: build, Subdir: subdir, Fn: fn}
}

func TestFinalizePrefersCondaAndDropsRemoved(t *testing.T) {
	in := []*record.PackageRecord{
		rec("bzip2", "1.0.8", "hd590300_5", "linux-64", "bzip2-1.0.8-hd590300_5.tar.bz2"),
		rec("bzip2", "1.0.8", "hd590300_5", "linux-64", "bzip2-1.0.8-hd590300_5.conda"),
		rec("bzip2", "1.0.8", "h7f98852_4", "linux-64", "bzip2-1.0.8-h7f98852_4.tar.bz2"),
		rec("bzip2", "1.0.8", "hd590300_5", "noarch", "bzip2-1.0.8-hd590300_5.tar.bz2"), // other subdir: kept
		rec("bzip2", "1.0.6", "0", "linux-64", "bzip2-1.0.6-0.tar.bz2"),
		rec("bzip2", "1.0.6", "0", "linux-64", "bzip2-1.0.6-0.conda"), // removed: its .tar.bz2 stays
		rec("bzip2", "1.0.5", "0", "linux-64", "bzip2-1.0.5-0.tar.bz2"),
	}
	removed := map[string]struct{}{"bzip2-1.0.6-0.conda": {}, "bzip2-1.0.5-0.tar.bz2": {}}
	got := fns(finalize(in, removed))
	want := []string{
		"bzip2-1.0.6-0.tar.bz2",
		"bzip2-1.0.8-h7f98852_4.tar.bz2",
		"bzip2-1.0.8-hd590300_5.conda",
		"bzip2-1.0.8-hd590300_5.tar.bz2", // noarch
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("finalize = %q, want %q", got, want)
	}
}

// Cases from libmamba tests/src/core/test_shard_traversal.cpp
// "shard_mentioned_packages". goomba follows depends only (the brief); the
// "Extract from constrains" case is inverted accordingly.
func TestDependencyNames(t *testing.T) {
	cases := []struct {
		name string
		recs []*record.PackageRecord
		want []string
	}{
		{"Extract from depends", []*record.PackageRecord{{Name: "python", Depends: []string{"libffi", "libzstd>=1.4"}}}, []string{"libffi", "libzstd"}},
		{"Extract from constrains (not followed)", []*record.PackageRecord{{Name: "numpy", Constrains: []string{"python>=3.9"}}}, []string{}},
		{"Extract from both packages and conda_packages", []*record.PackageRecord{{Name: "pkg1", Depends: []string{"dep_a"}}, {Name: "pkg2", Depends: []string{"dep_b"}}}, []string{"dep_a", "dep_b"}},
		{"Deduplicate across multiple records", []*record.PackageRecord{{Name: "pkg1", Depends: []string{"common_dep"}}, {Name: "pkg2", Depends: []string{"common_dep"}}}, []string{"common_dep"}},
		{"Skip invalid specs", []*record.PackageRecord{{Name: "pkg", Depends: []string{"valid>=1.0", "!!!invalid!!!", "another_valid"}}}, []string{"another_valid", "valid"}},
		{"Skip free name specs", []*record.PackageRecord{{Name: "pkg", Depends: []string{"normal_pkg", "*"}}}, []string{"normal_pkg"}},
		{"Empty shard", nil, []string{}},
		{"Shard with empty depends and constrains", []*record.PackageRecord{{Name: "pkg"}}, []string{}},
	}
	for _, c := range cases {
		if got := dependencyNames(c.recs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSpecName(t *testing.T) {
	cases := map[string]string{
		"python":                             "python",
		"python >=3.8,<3.9":                  "python",
		"  python >=3.12":                    "python",
		"python>=3.8":                        "python",
		"python=3.8":                         "python",
		"python==3.8.1":                      "python",
		"python!=3.8":                        "python",
		"python~=3.8":                        "python",
		"python<4":                           "python",
		"python 3.7.12 0_73_pypy":            "python",
		"Python 3.7":                         "python",
		"conda-forge::python 3.7.12":         "python",
		"conda-forge/linux-64::python":       "python",
		"python[version='>=3.8']":            "python",
		"python_abi 3.12.* *_cp312":          "python_abi",
		"libgcc-ng >=12":                     "libgcc-ng",
		"__glibc >=2.17,<3.0.a0":             "__glibc",
		"*":                                  "",
		"":                                   "",
		"   ":                                "",
		">=3.8":                              "",
		"!!!invalid!!!":                      "",
		"a/b":                                "",
		"ch::":                               "",
		"numpy|scipy":                        "numpy",
		"ca-certificates\t2024.2.2 hbcca054": "ca-certificates",
	}
	for in, want := range cases {
		if got := specName(in); got != want {
			t.Errorf("specName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrackFeaturesNormalization(t *testing.T) {
	cases := map[string][]string{
		"":                     nil,
		"mkl":                  {"mkl"},
		"blas_netlib,blas_x":   {"blas_netlib", "blas_x"},
		"a b":                  {"a", "b"},
		" a , b ,, c ":         {"a", "b", "c"},
		"pypy\tcuda\nopenblas": {"pypy", "cuda", "openblas"},
	}
	for in, want := range cases {
		if got := splitTrackFeatures(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitTrackFeatures(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cleanTrackFeatures([]string{"", " a ", "b", ""}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("cleanTrackFeatures = %q", got)
	}
	if got := cleanTrackFeatures([]string{""}); got != nil {
		t.Errorf("cleanTrackFeatures of empties = %q, want nil", got)
	}
}

func TestValidators(t *testing.T) {
	for _, s := range []string{"python", "3.11.15", "hd63d673_0_cpython", "1!2.0", "1.0+local", "_r-mutex", "py_0"} {
		if !validIdent(s) {
			t.Errorf("validIdent(%q) = false", s)
		}
	}
	for _, s := range []string{"", ".", "..", "a/b", `a\b`, "a b", "a\x00b", "a:b", "a*b", "a\nb", "\xff"} {
		if validIdent(s) {
			t.Errorf("validIdent(%q) = true", s)
		}
	}
	for _, c := range []struct {
		fn    string
		conda bool
		ok    bool
	}{
		{"six-1.17.0-pyhd8ed1ab_0.conda", true, true},
		{"six-1.15.0-pyh9f0ad1d_0.tar.bz2", false, true},
		{"A_0.1.0.tar.bz2", false, true},
		{"six-1.17.0-pyhd8ed1ab_0.conda", false, false},
		{"six-1.15.0-pyh9f0ad1d_0.tar.bz2", true, false},
		{".conda", true, false},
		{"../evil.conda", true, false},
		{"dir/evil.tar.bz2", false, false},
		{"evil.whl", false, false},
	} {
		if got := validFilename(c.fn, c.conda); got != c.ok {
			t.Errorf("validFilename(%q, %v) = %v", c.fn, c.conda, got)
		}
	}
	for _, p := range []string{"lib/python3.13t/site-packages", "Lib/site-packages"} {
		if !validSitePackagesPath(p) {
			t.Errorf("validSitePackagesPath(%q) = false", p)
		}
	}
	for _, p := range []string{"", "/usr/lib", "../x", "lib/../../x", "lib//x", "lib/./x", `lib\x`, "C:/x", "lib/\x00"} {
		if validSitePackagesPath(p) {
			t.Errorf("validSitePackagesPath(%q) = true", p)
		}
	}
	for in, want := range map[string]string{testSHA: testSHA, "ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890": testSHA} {
		if got, ok := normalizeHex(in, 32); !ok || got != want {
			t.Errorf("normalizeHex(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "abc", testSHA + "00", "zz" + testSHA[2:], testMD5} {
		if _, ok := normalizeHex(in, 32); ok {
			t.Errorf("normalizeHex(%q) accepted", in)
		}
	}
}
