package lock

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

func readExplicitFile(t *testing.T, name string) *Explicit {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "explicit-envs", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e, err := ReadExplicit(f)
	if err != nil {
		t.Fatalf("ReadExplicit(%s): %v", name, err)
	}
	return e
}

// Cases from rattler crates/rattler_conda_types/src/explicit_environment_spec.rs
// (test_parse over test-data/explicit-envs, copied to testdata/explicit-envs).
// rattler compares against insta snapshots; here the snapshot facts are
// asserted directly.
func TestParseRattlerExplicitFiles(t *testing.T) {
	cases := []struct {
		file     string
		platform string
		entries  int
		md5s     int
		first    ExplicitEntry
	}{
		{"ros-noetic_linux-64.txt", "linux-64", 568, 0, ExplicitEntry{
			URL: "https://conda.anaconda.org/conda-forge/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2"}},
		{"vs2015_runtime_win-64.txt", "win-64", 2, 2, ExplicitEntry{
			URL: "https://repo.anaconda.com/pkgs/main/win-64/vc-14.1-h0510ff6_4.conda",
			MD5: "dc9507a39ab328597820486c729cd279"}},
		{"xtensor_linux-64.txt", "linux-64", 7, 7, ExplicitEntry{
			URL: "https://conda.anaconda.org/conda-forge/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2",
			MD5: "d7c89558ba9fa0495403155b64376d81"}},
	}
	for _, c := range cases {
		e := readExplicitFile(t, c.file)
		if e.Platform != c.platform || len(e.Entries) != c.entries {
			t.Errorf("%s: platform %q, %d entries; want %q, %d", c.file, e.Platform, len(e.Entries), c.platform, c.entries)
			continue
		}
		md5s := 0
		for _, ent := range e.Entries {
			if ent.MD5 != "" {
				md5s++
			}
			if ent.SHA256 != "" || strings.Contains(ent.URL, "#") {
				t.Errorf("%s: unexpected entry %+v", c.file, ent)
			}
		}
		if md5s != c.md5s {
			t.Errorf("%s: %d md5 hashes, want %d", c.file, md5s, c.md5s)
		}
		if e.Entries[0] != c.first {
			t.Errorf("%s: first entry %+v, want %+v", c.file, e.Entries[0], c.first)
		}
	}
}

// fakeIndex returns a lookup that knows every URL in urls, with metadata
// derived from the URL, a sha256 of the URL string, and the given md5s
// (others get an md5 of the URL).
func fakeIndex(urls []string, md5s map[string]string) func(string) (*record.PackageRecord, bool) {
	known := map[string]*record.PackageRecord{}
	for _, u := range urls {
		fn := path.Base(u)
		ext := archiveExt(fn)
		name, version, build, _ := splitDist(strings.TrimSuffix(fn, ext))
		sum := sha256.Sum256([]byte(u))
		m := md5s[u]
		if m == "" {
			s := md5.Sum([]byte(u))
			m = hex.EncodeToString(s[:])
		}
		subdir := path.Base(path.Dir(u))
		noarch := ""
		if subdir == "noarch" {
			noarch = "generic"
		}
		known[u] = &record.PackageRecord{
			Name: name, Version: version, Build: build, Fn: fn, URL: u,
			Subdir: subdir, Noarch: noarch, SHA256: hex.EncodeToString(sum[:]), MD5: m,
			Depends: []string{},
		}
	}
	return func(u string) (*record.PackageRecord, bool) {
		r, ok := known[u]
		return r, ok
	}
}

func entryURLs(e *Explicit) ([]string, map[string]string) {
	var urls []string
	md5s := map[string]string{}
	for _, ent := range e.Entries {
		urls = append(urls, ent.URL)
		md5s[ent.URL] = ent.MD5
	}
	return urls, md5s
}

// Cases from rattler explicit_environment_spec.rs (test_to_spec_string,
// test_to_path): a parsed file written back out parses to the same
// platform and URLs. goomba writes explicit files from a lockfile, so the
// round trip goes through Resolve with an index that knows every URL;
// WriteExplicit reorders packages, so URLs are compared as sets.
func TestExplicitRoundTrip(t *testing.T) {
	for _, file := range []string{"ros-noetic_linux-64.txt", "vs2015_runtime_win-64.txt", "xtensor_linux-64.txt"} {
		e := readExplicitFile(t, file)
		urls, md5s := entryURLs(e)
		l, err := e.Resolve(fakeIndex(urls, md5s))
		if err != nil {
			t.Fatalf("%s: Resolve: %v", file, err)
		}
		var buf bytes.Buffer
		if err := l.WriteExplicit(&buf); err != nil {
			t.Fatal(err)
		}
		back, err := ReadExplicit(&buf)
		if err != nil {
			t.Fatalf("%s: ReadExplicit(WriteExplicit): %v", file, err)
		}
		if back.Platform != e.Platform {
			t.Errorf("%s: platform %q, want %q", file, back.Platform, e.Platform)
		}
		got, _ := entryURLs(back)
		sort.Strings(got)
		want := append([]string(nil), urls...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: URLs differ after round trip", file)
		}
		for _, ent := range back.Entries {
			if ent.MD5 == "" || ent.SHA256 != "" {
				t.Errorf("%s: written entry %+v should carry the md5", file, ent)
			}
		}
		l2, err := back.Resolve(fakeIndex(urls, md5s))
		if err != nil {
			t.Fatal(err)
		}
		if writeString(t, l2) != writeString(t, l) {
			t.Errorf("%s: lockfile changed after an explicit round trip", file)
		}
	}
}

// Cases from rattler explicit_environment_spec.rs (test_parse_empty,
// test_parse_no_explicit_tag, test_parse_invalid_url,
// test_parse_invalid_platform), plus goomba's stricter rules.
func TestReadExplicitErrors(t *testing.T) {
	const vs = "https://repo.anaconda.com/pkgs/main/win-64/vs2015_runtime-14.16.27012-hf0eaf9b_3.conda#a98ea1e3abfdbbd201d60ff6b43ea7e4"
	missing := []string{
		"",
		vs,
		"# platform: win-64\n" + vs + "\n@EXPLICIT\n",
		"# just a comment\n",
	}
	for _, in := range missing {
		_, err := ReadExplicit(strings.NewReader(in))
		if !errors.Is(err, ErrMissingExplicit) || !errors.Is(err, ErrInvalid) {
			t.Errorf("ReadExplicit(%q) = %v, want ErrMissingExplicit", in, err)
		}
	}
	invalid := []string{
		"@EXPLICIT\nimnotanurl",
		"# platform: notaplatform\n@EXPLICIT",
		"# platform: noarch\n@EXPLICIT",
		"# platform: linux-64\n# platform: osx-64\n@EXPLICIT",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg.conda",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4.conda",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/-6.4-bld.conda",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-.conda",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.zip",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda#",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda#dc9507a39ab328597820486c729c",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda#sha256:blablabla",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda#zz9507a39ab328597820486c729cd279",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda#a#b",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg 6.4-bld.conda",
		"@EXPLICIT\ngit+https://github.com/urllib3/urllib3.git@1.19.1#egg=urllib3",
		"@EXPLICIT\n/local/linux-64/pkg-6.4-bld.conda",
		"@EXPLICIT\nconda-forge::pkg-6.4-bld.conda",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda\xff",
		"@EXPLICIT\nA://0/0-0-0.tar.bz2?",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda?x=1",
		"@EXPLICIT\nHTTPS://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda",
		"@EXPLICIT\nftp://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda",
		"@EXPLICIT\nhttps://user:token@conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda",
		"@EXPLICIT\nhttps:conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda",
	}
	for _, in := range invalid {
		_, err := ReadExplicit(strings.NewReader(in))
		if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrMissingExplicit) {
			t.Errorf("ReadExplicit(%q) = %v, want ErrInvalid", in, err)
		}
	}
	_, err := ReadExplicit(strings.NewReader("@EXPLICIT\nimnotanurl"))
	if err == nil || !strings.Contains(err.Error(), "imnotanurl") || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error does not name the line: %v", err)
	}
}

// Cases from rattler explicit_environment_spec.rs (test_entry_package_hash,
// test_parse_entry_hash) and libmamba tests/src/specs/test_package_info.cpp
// (TEST_CASE "PackageInfo::from_url"). libmamba's git+https case is
// rejected: goomba's explicit files hold conda archives only.
func TestExplicitEntryHashes(t *testing.T) {
	const (
		base  = "https://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda"
		md5   = "7dbaa197d7ba6032caf7ae7f32c1efa0"
		sha   = "7dbaa197d7ba6032caf7ae7f32c1efa07dbaa197d7ba6032caf7ae7f32c1efa0"
		rsha  = "315f5bdb76d078c43b8ac0064e4a0164612b1fce77c869345bfc94c75894edd3"
		vsURL = "https://repo.anaconda.com/pkgs/main/win-64/vs2015_runtime-14.16.27012-hf0eaf9b_3.conda"
		token = "http://localhost:32826/t/1a5eb8d110994feaa53d0d9f8bf13bbb/get/proxy-channel/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2"
	)
	cases := []struct {
		line string
		want ExplicitEntry
	}{
		{base, ExplicitEntry{URL: base}},
		{base + "#" + md5, ExplicitEntry{URL: base, MD5: md5}},
		{base + "#" + sha, ExplicitEntry{URL: base, SHA256: sha}},
		{base + "#sha256:" + sha, ExplicitEntry{URL: base, SHA256: sha}},
		{token + "#d7c89558ba9fa0495403155b64376d81", ExplicitEntry{URL: token, MD5: "d7c89558ba9fa0495403155b64376d81"}},
		{vsURL + "#a98ea1e3abfdbbd201d60ff6b43ea7e4", ExplicitEntry{URL: vsURL, MD5: "a98ea1e3abfdbbd201d60ff6b43ea7e4"}},
		{base + "#sha256:" + rsha, ExplicitEntry{URL: base, SHA256: rsha}},
		{base + "#" + rsha, ExplicitEntry{URL: base, SHA256: rsha}},
		{base + "#" + strings.ToUpper(md5), ExplicitEntry{URL: base, MD5: md5}},
		{"  " + base + "#" + md5 + " \r", ExplicitEntry{URL: base, MD5: md5}},
		{"file:///channel/linux-64/pkg-6.4-bld.tar.bz2", ExplicitEntry{URL: "file:///channel/linux-64/pkg-6.4-bld.tar.bz2"}},
	}
	for _, c := range cases {
		e, err := ReadExplicit(strings.NewReader("@EXPLICIT\n" + c.line + "\n"))
		if err != nil {
			t.Errorf("ReadExplicit(%q): %v", c.line, err)
			continue
		}
		if len(e.Entries) != 1 || e.Entries[0] != c.want {
			t.Errorf("ReadExplicit(%q) = %+v, want %+v", c.line, e.Entries, c.want)
		}
	}
	bad := []string{
		"",
		"dc9507a39ab328597820486c729c",
		"sha256:blablabla",
		"315f5bdb76d078c43b8ac0064e4a0164612b1fce77c869345bfc94c7589",
		"sha256:" + md5,
	}
	for _, h := range bad {
		if _, err := ReadExplicit(strings.NewReader("@EXPLICIT\n" + base + "#" + h)); !errors.Is(err, ErrInvalid) {
			t.Errorf("hash %q: %v, want ErrInvalid", h, err)
		}
	}
	// libmamba's from_url cases also derive the channel and platform.
	for _, u := range []string{base} {
		ch, subdir, fn, err := splitArchiveURL(u)
		if err != nil || ch != "https://conda.anaconda.org/conda-forge" || subdir != "linux-64" || fn != "pkg-6.4-bld.conda" {
			t.Errorf("splitArchiveURL(%q) = %q, %q, %q, %v", u, ch, subdir, fn, err)
		}
		name, version, build, ok := splitDist(strings.TrimSuffix(fn, ".conda"))
		if !ok || name != "pkg" || version != "6.4" || build != "bld" {
			t.Errorf("splitDist(%q) = %q %q %q %v", fn, name, version, build, ok)
		}
	}
	if name, version, build, ok := splitDist("_libgcc_mutex-0.1-conda_forge"); !ok || name != "_libgcc_mutex" || version != "0.1" || build != "conda_forge" {
		t.Errorf("splitDist(_libgcc_mutex) = %q %q %q", name, version, build)
	}
	if name, _, _, ok := splitDist("ld_impl_linux-64-2.45.1-default_hbd61a6d_102"); !ok || name != "ld_impl_linux-64" {
		t.Errorf("splitDist keeps dashes in names: %q", name)
	}
}

func TestResolve(t *testing.T) {
	l := validLockfile()
	var buf bytes.Buffer
	if err := l.WriteExplicit(&buf); err != nil {
		t.Fatal(err)
	}
	want := "# platform: linux-64\n@EXPLICIT\n" +
		tzdataURL + "#sha256:" + tzdataSHA + "\n" +
		pythonURL + "#" + pythonMD5 + "\n" +
		pipURL + "#sha256:" + pipSHA + "\n"
	if buf.String() != want {
		t.Fatalf("WriteExplicit =\n%s\nwant\n%s", buf.String(), want)
	}
	index := map[string]*record.PackageRecord{}
	for _, r := range l.Records() {
		index[r.URL] = r
	}
	lookup := func(u string) (*record.PackageRecord, bool) { r, ok := index[u]; return r, ok }
	e, err := ReadExplicit(strings.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Resolve(lookup)
	if err != nil {
		t.Fatal(err)
	}
	expect := validLockfile()
	expect.Specs, expect.Virtual = []string{}, map[string]string{}
	if writeString(t, got) != writeString(t, expect) {
		t.Errorf("Resolve =\n%s\nwant\n%s", writeString(t, got), writeString(t, expect))
	}

	// The index supplies the sha256 for md5-only and unhashed entries.
	e2, _ := ReadExplicit(strings.NewReader("@EXPLICIT\n" + pythonURL + "\n" + tzdataURL + "\n"))
	got2, err := e2.Resolve(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Platform != "linux-64" || got2.Packages[0].SHA256 != pythonSHA {
		t.Errorf("Resolve without hashes: %+v", got2)
	}

	cases := []struct {
		name   string
		in     string
		lookup func(string) (*record.PackageRecord, bool)
		want   error
	}{
		{"not in index", "@EXPLICIT\n" + cf + "/linux-64/zlib-1.3-h0.conda\n", lookup, ErrNotInIndex},
		{"md5 mismatch", "@EXPLICIT\n" + pythonURL + "#" + strings.Repeat("0", 32) + "\n", lookup, ErrHashMismatch},
		{"sha256 mismatch", "@EXPLICIT\n" + pythonURL + "#sha256:" + strings.Repeat("0", 64) + "\n", lookup, ErrHashMismatch},
		{"index lacks md5", "@EXPLICIT\n" + pipURL + "#" + strings.Repeat("a", 32) + "\n", lookup, ErrHashMismatch},
		{"index lacks sha256", "@EXPLICIT\n" + pythonURL + "\n", func(u string) (*record.PackageRecord, bool) {
			r := *index[u]
			r.SHA256 = ""
			return &r, true
		}, ErrInvalid},
		{"index url differs", "@EXPLICIT\n" + pythonURL + "\n", func(u string) (*record.PackageRecord, bool) {
			r := *index[u]
			r.URL = "https://evil.example/linux-64/" + r.Fn
			return &r, true
		}, ErrInvalid},
		{"nil record", "@EXPLICIT\n" + pythonURL + "\n", func(string) (*record.PackageRecord, bool) { return nil, true }, ErrNotInIndex},
		{"http", "@EXPLICIT\nhttp://conda.anaconda.org/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda\n",
			func(u string) (*record.PackageRecord, bool) {
				r := *index[pythonURL]
				r.URL = u
				return &r, true
			}, ErrInvalid},
		{"only noarch", "@EXPLICIT\n" + tzdataURL + "\n", lookup, ErrInvalid},
		{"platform mismatch", "# platform: osx-64\n@EXPLICIT\n" + pythonURL + "\n", lookup, ErrInvalid},
		{"duplicate", "@EXPLICIT\n" + pythonURL + "\n" + pythonURL + "\n", lookup, ErrInvalid},
	}
	for _, c := range cases {
		e, err := ReadExplicit(strings.NewReader(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if _, err := e.Resolve(c.lookup); !errors.Is(err, c.want) {
			t.Errorf("%s: Resolve = %v, want %v", c.name, err, c.want)
		}
	}
	// Resolve never modifies the index's records.
	if index[pythonURL].SHA256 != pythonSHA || index[pythonURL].URL != pythonURL {
		t.Error("Resolve modified a looked-up record")
	}
}

func TestDepName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"python", "python"},
		{"python >=3.8", "python"},
		{"python>=3.8", "python"},
		{"python_abi 3.11.* *_cp311", "python_abi"},
		{"__glibc >=2.17,<3.0.a0", "__glibc"},
		{"conda-forge::python 3.11", "python"},
		{"https://conda.anaconda.org/conda-forge::python", "python"},
		{"conda-forge/linux-64::zlib", "zlib"},
		{"numpy[version='>=1.20']", "numpy"},
		{"  pip  ", "pip"},
		{"ld_impl_linux-64 >=2.36.1", "ld_impl_linux-64"},
		{"foo==1.0", "foo"},
		{"foo !=1.0", "foo"},
		{"", ""},
	}
	for _, c := range cases {
		if got := depName(c.in); got != c.want {
			t.Errorf("depName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func pkgs(deps map[string][]string) []Package {
	var out []Package
	for name, d := range deps {
		out = append(out, Package{Name: name, Depends: d})
	}
	return out
}

func names(ps []Package) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.Name)
	}
	return strings.Join(s, " ")
}

func TestSortTopologically(t *testing.T) {
	cases := []struct {
		deps map[string][]string
		want string
	}{
		{map[string][]string{}, ""},
		{map[string][]string{"a": nil, "b": nil, "c": nil}, "a b c"},
		{map[string][]string{"a": {"b"}, "b": {"c >=1"}, "c": nil}, "c b a"},
		{map[string][]string{"a": {"c"}, "b": {"a"}, "c": {"__glibc", "missing"}}, "c a b"},
		// Self and mutual dependencies: the search breaks cycles where it meets them.
		{map[string][]string{"a": {"a", "b"}, "b": {"a"}}, "b a"},
		{map[string][]string{"x": {"y"}, "y": {"z"}, "z": {"x"}}, "z y x"},
		// python's dependency on pip is reversed.
		{map[string][]string{"pip": {"python"}, "python": {"pip", "zlib"}, "zlib": nil, "wheel": {"python"}}, "zlib python pip wheel"},
		{map[string][]string{"pip": nil, "python": {"pip"}}, "python pip"},
		// Channel-qualified dependencies still link.
		{map[string][]string{"a": {"conda-forge::b >=1"}, "b": nil}, "b a"},
	}
	for _, c := range cases {
		in := pkgs(c.deps)
		got := names(sortTopologically(in))
		if got != c.want {
			t.Errorf("sortTopologically(%v) = %q, want %q", c.deps, got, c.want)
		}
		// The result does not depend on the input order.
		for i := 0; i < 5; i++ {
			rev := append([]Package(nil), in...)
			sort.Slice(rev, func(a, b int) bool { return rev[a].Name > rev[b].Name })
			if names(sortTopologically(rev)) != got {
				t.Errorf("order depends on input order for %v", c.deps)
			}
		}
	}
	// A deep chain does not recurse.
	var deep []Package
	for i := 0; i < 100000; i++ {
		p := Package{Name: "p" + strings.Repeat("0", 6-len(itoa(i))) + itoa(i)}
		if i > 0 {
			p.Depends = []string{deep[i-1].Name}
		}
		deep = append(deep, p)
	}
	if out := sortTopologically(deep); len(out) != len(deep) || out[0].Name != "p000000" {
		t.Errorf("deep chain sorted wrongly")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// TestWriteExplicitMatchesMicromamba compares WriteExplicit for the fixture
// environment with `micromamba env export --explicit` of the same
// environment (testdata/base-3.11.micromamba-explicit.txt), ignoring
// micromamba's two leading "This file may be used..." comment lines.
func TestWriteExplicitMatchesMicromamba(t *testing.T) {
	l, err := FromPrefix(filepath.Join("testdata", "prefix-base"), "linux-64")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := l.WriteExplicit(&buf); err != nil {
		t.Fatal(err)
	}
	oracle, err := os.ReadFile(filepath.Join("testdata", "base-3.11.micromamba-explicit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, line := range strings.SplitAfter(string(oracle), "\n") {
		if strings.HasPrefix(line, "# This file") || strings.HasPrefix(line, "# $ conda create") {
			continue
		}
		want = append(want, line)
	}
	if buf.String() != strings.Join(want, "") {
		t.Errorf("WriteExplicit =\n%s\nmicromamba wrote\n%s", buf.String(), strings.Join(want, ""))
	}
}

// TestExplicitInstallsWithMicromamba checks that micromamba accepts
// WriteExplicit's output, with md5 and with sha256 fragments, in a dry run.
// It runs only when GOOMBA_MICROMAMBA names a micromamba binary, and uses a
// scratch root prefix under the worktree's tmp/ directory.
func TestExplicitInstallsWithMicromamba(t *testing.T) {
	bin := os.Getenv("GOOMBA_MICROMAMBA")
	if bin == "" {
		t.Skip("set GOOMBA_MICROMAMBA to a micromamba binary")
	}
	l, err := FromPrefix(filepath.Join("testdata", "prefix-base"), "linux-64")
	if err != nil {
		t.Fatal(err)
	}
	noMD5, err := FromPrefix(filepath.Join("testdata", "prefix-base"), "linux-64")
	if err != nil {
		t.Fatal(err)
	}
	for i := range noMD5.Packages {
		noMD5.Packages[i].MD5 = ""
	}
	if err := os.MkdirAll(filepath.Join("..", "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	scratch, err := os.MkdirTemp(filepath.Join("..", "tmp"), "micromamba-")
	if err != nil {
		t.Fatal(err)
	}
	scratch, err = filepath.Abs(scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })
	for name, lf := range map[string]*Lockfile{"md5": l, "sha256": noMD5} {
		file := filepath.Join(scratch, name+".txt")
		var buf bytes.Buffer
		if err := lf.WriteExplicit(&buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "create", "--dry-run", "-y", "-p", filepath.Join(scratch, "env-"+name), "-f", file)
		cmd.Env = []string{
			"HOME=" + scratch,
			"MAMBA_ROOT_PREFIX=" + filepath.Join(scratch, "root"),
			"PATH=/usr/bin:/bin",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: micromamba create --dry-run: %v\n%s", name, err, out)
		}
		installs := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "+ ") {
				installs++
			}
		}
		if installs != len(lf.Packages) || !strings.Contains(string(out), "Dry run") {
			t.Errorf("%s: micromamba planned %d installs, want %d:\n%s", name, installs, len(lf.Packages), out)
		}
	}
}
