package lock

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata")

const (
	cf          = "https://conda.anaconda.org/conda-forge"
	pythonSHA   = "bf6a32c69889d38482436a786bea32276756cedf0e9805cc856ffd088e8d00f0"
	pythonMD5   = "a5ebcefec0c12a333bcd6d7bf3bddc1f"
	pipSHA      = "8e0e8a6ff1bdfb4ec3d6ed8d6e2a0c63ff23bf0fc0c5ca13e6e93b1c2e1fca4e"
	pythonURL   = cf + "/linux-64/python-3.11.15-hd63d673_0_cpython.conda"
	pipURL      = cf + "/noarch/pip-26.0.1-pyh8b19718_0.conda"
	tzdataURL   = cf + "/noarch/tzdata-2025c-hc9c84f9_1.conda"
	tzdataSHA   = "1111111111111111111111111111111111111111111111111111111111111111"
	examplePath = "testdata/example.lock.json"
)

// validLockfile returns a small valid lockfile; each call returns a fresh
// value the caller may modify.
func validLockfile() *Lockfile {
	return &Lockfile{
		Version:  1,
		Platform: "linux-64",
		Channels: []string{cf},
		Specs:    []string{"python=3.11"},
		Virtual:  map[string]string{"__glibc": "2.17", "__unix": "0", "__linux": "0", "__archspec": "x86_64"},
		Packages: []Package{
			{
				Name: "python", Version: "3.11.15", Build: "hd63d673_0_cpython", Subdir: "linux-64",
				URL: pythonURL, SHA256: pythonSHA, MD5: pythonMD5, Size: 30949404,
				Depends:    []string{"__glibc >=2.17,<3.0.a0", "tzdata", "pip"},
				Constrains: []string{"python_abi 3.11.* *_cp311"},
			},
			{
				Name: "pip", Version: "26.0.1", Build: "pyh8b19718_0", Subdir: "noarch", Noarch: "python",
				URL: pipURL, SHA256: pipSHA, Size: 1180000,
				Depends: []string{"python >=3.10,<3.14.0a0", "setuptools", "wheel"},
			},
			{
				Name: "tzdata", Version: "2025c", Build: "hc9c84f9_1", BuildNumber: 1, Subdir: "noarch", Noarch: "generic",
				URL: tzdataURL, SHA256: tzdataSHA,
				PythonSitePackagesPath: "",
			},
		},
	}
}

func writeString(t *testing.T, l *Lockfile) string {
	t.Helper()
	var buf bytes.Buffer
	if err := l.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.String()
}

func checkGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (run with -update to rewrite):\n%s", path, got)
	}
}

func TestRoundTrip(t *testing.T) {
	l := validLockfile()
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	out := writeString(t, l)
	checkGolden(t, examplePath, out)
	if !strings.HasSuffix(out, "}\n") || strings.Contains(out, "\\u003c") || !strings.Contains(out, "\n  \"platform\": \"linux-64\",\n") {
		t.Errorf("Write output is not 2-space indented, unescaped JSON with a trailing newline:\n%s", out)
	}
	back, err := Read(strings.NewReader(out))
	if err != nil {
		t.Fatalf("Read(Write): %v", err)
	}
	if again := writeString(t, back); again != out {
		t.Errorf("Write(Read(Write(l))) differs:\n%s\nvs\n%s", again, out)
	}
	// Read keeps file order; Write sorts by name.
	if back.Packages[0].Name != "pip" || back.Packages[2].Name != "tzdata" {
		t.Errorf("packages not sorted by name: %v", back.Packages)
	}
	back2, err := Read(strings.NewReader(writeString(t, back)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, back2) {
		t.Errorf("read -> write -> read differs:\n%+v\n%+v", back, back2)
	}
	// Write does not modify l.
	if l.Packages[0].Name != "python" || l.Packages[2].Depends != nil {
		t.Errorf("Write modified its receiver")
	}
	// Writing the same lockfile twice gives identical bytes.
	if writeString(t, l) != out {
		t.Error("Write is not deterministic")
	}
}

func TestWriteEmptyLists(t *testing.T) {
	l := validLockfile()
	l.Specs, l.Virtual, l.Packages = nil, nil, nil
	out := writeString(t, l)
	for _, want := range []string{`"specs": []`, `"virtual": {}`, `"packages": []`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s:\n%s", want, out)
		}
	}
	back, err := Read(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if back.Specs == nil || back.Virtual == nil || back.Packages == nil {
		t.Errorf("Read left nil lists: %+v", back)
	}
}

// TestValidateRejects mutates a valid lockfile in every way Validate must
// catch.
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(l *Lockfile)
	}{
		{"version 0", func(l *Lockfile) { l.Version = 0 }},
		{"version 2", func(l *Lockfile) { l.Version = 2 }},
		{"platform empty", func(l *Lockfile) { l.Platform = "" }},
		{"platform noarch", func(l *Lockfile) { l.Platform = "noarch" }},
		{"platform unknown", func(l *Lockfile) { l.Platform = "linux-65" }},
		{"channel http", func(l *Lockfile) { l.Channels = []string{"http://conda.anaconda.org/conda-forge"} }},
		{"channel trailing slash", func(l *Lockfile) { l.Channels = []string{cf + "/"} }},
		{"channel credentials", func(l *Lockfile) { l.Channels = []string{"https://user:pw@conda.anaconda.org/conda-forge"} }},
		{"channel twice", func(l *Lockfile) { l.Channels = []string{cf, cf} }},
		{"channel name", func(l *Lockfile) { l.Channels = []string{"conda-forge"} }},
		{"spec empty", func(l *Lockfile) { l.Specs = []string{" "} }},
		{"spec newline", func(l *Lockfile) { l.Specs = []string{"python\n=3.11"} }},
		{"virtual name", func(l *Lockfile) { l.Virtual["glibc"] = "2.17" }},
		{"virtual upper", func(l *Lockfile) { l.Virtual["__GLIBC"] = "2.17" }},
		{"virtual empty", func(l *Lockfile) { l.Virtual["__cuda"] = "" }},
		{"virtual version", func(l *Lockfile) { l.Virtual["__glibc"] = "2..17" }},
		{"virtual archspec dash", func(l *Lockfile) { l.Virtual["__archspec"] = "x86-64-v3" }},
		{"duplicate name", func(l *Lockfile) { l.Packages = append(l.Packages, l.Packages[0]) }},
		{"duplicate name case", func(l *Lockfile) {
			p := l.Packages[2]
			p.Name = "TZdata"
			p.URL = cf + "/noarch/TZdata-2025c-hc9c84f9_1.conda"
			l.Packages = append(l.Packages, p)
		}},
		{"name empty", func(l *Lockfile) { l.Packages[0].Name = "" }},
		{"name virtual", func(l *Lockfile) { l.Packages[0].Name = "__glibc" }},
		{"name space", func(l *Lockfile) { l.Packages[0].Name = "py thon" }},
		{"name slash", func(l *Lockfile) { l.Packages[0].Name = "../python" }},
		{"version empty", func(l *Lockfile) { l.Packages[0].Version = "" }},
		{"version bad", func(l *Lockfile) { l.Packages[0].Version = "3..11" }},
		{"version dash", func(l *Lockfile) { l.Packages[0].Version = "3.11-1" }},
		{"build empty", func(l *Lockfile) { l.Packages[0].Build = "" }},
		{"build dash", func(l *Lockfile) { l.Packages[0].Build = "h-1" }},
		{"subdir other platform", func(l *Lockfile) { l.Packages[0].Subdir = "osx-64" }},
		{"subdir empty", func(l *Lockfile) { l.Packages[0].Subdir = "" }},
		{"noarch bad", func(l *Lockfile) { l.Packages[1].Noarch = "true" }},
		{"virtual not on platform", func(l *Lockfile) { l.Virtual["__osx"] = "13.0" }},
		{"virtual win on linux", func(l *Lockfile) { l.Virtual["__win"] = "10.0" }},
		{"virtual archspec upper", func(l *Lockfile) { l.Virtual["__archspec"] = "X86_64" }},
		{"virtual value too long", func(l *Lockfile) { l.Virtual["__cuda"] = strings.Repeat("1", 65) }},
		{"virtual name too long", func(l *Lockfile) { l.Virtual["__"+strings.Repeat("a", 63)] = "1" }},
		{"url upper-case host", func(l *Lockfile) {
			l.Channels = []string{"https://Conda.anaconda.org/conda-forge"}
			for i := range l.Packages {
				l.Packages[i].URL = strings.Replace(l.Packages[i].URL, "conda.anaconda.org", "Conda.anaconda.org", 1)
			}
		}},
		{"url trailing-dot host", func(l *Lockfile) {
			l.Channels = []string{"https://conda.anaconda.org./conda-forge"}
			for i := range l.Packages {
				l.Packages[i].URL = strings.Replace(l.Packages[i].URL, "conda.anaconda.org", "conda.anaconda.org.", 1)
			}
		}},
		{"url default port", func(l *Lockfile) {
			l.Channels = []string{"https://conda.anaconda.org:443/conda-forge"}
			for i := range l.Packages {
				l.Packages[i].URL = strings.Replace(l.Packages[i].URL, "conda.anaconda.org", "conda.anaconda.org:443", 1)
			}
		}},
		{"channel upper-case host", func(l *Lockfile) { l.Channels = append(l.Channels, "https://Mirror.example/cf") }},
		{"url empty", func(l *Lockfile) { l.Packages[0].URL = "" }},
		{"url http", func(l *Lockfile) { l.Packages[0].URL = strings.Replace(pythonURL, "https:", "http:", 1) }},
		{"url file", func(l *Lockfile) { l.Packages[0].URL = "file:///linux-64/python-3.11.15-hd63d673_0_cpython.conda" }},
		{"url query", func(l *Lockfile) { l.Packages[0].URL = pythonURL + "?x=1" }},
		{"url fragment", func(l *Lockfile) { l.Packages[0].URL = pythonURL + "#" + pythonMD5 }},
		{"url credentials", func(l *Lockfile) {
			l.Packages[0].URL = strings.Replace(pythonURL, "https://", "https://u:p@", 1)
		}},
		{"url percent", func(l *Lockfile) { l.Packages[0].URL = strings.Replace(pythonURL, "conda-forge", "conda%2Dforge", 1) }},
		{"url dotdot", func(l *Lockfile) {
			l.Packages[0].URL = cf + "/osx-64/../linux-64/python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"url double slash", func(l *Lockfile) {
			l.Packages[0].URL = cf + "//linux-64/python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"url backslash", func(l *Lockfile) {
			l.Packages[0].URL = cf + "/linux-64\\python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"url space", func(l *Lockfile) { l.Packages[0].URL = " " + pythonURL }},
		{"url upper scheme", func(l *Lockfile) { l.Packages[0].URL = strings.Replace(pythonURL, "https", "HTTPS", 1) }},
		{"url no subdir", func(l *Lockfile) {
			l.Packages[0].URL = "https://example.org/python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"url other file", func(l *Lockfile) {
			l.Packages[0].URL = cf + "/linux-64/python-3.11.14-hd63d673_0_cpython.conda"
		}},
		{"url other extension", func(l *Lockfile) {
			l.Packages[0].URL = cf + "/linux-64/python-3.11.15-hd63d673_0_cpython.zip"
		}},
		{"url subdir mismatch", func(l *Lockfile) {
			l.Packages[0].URL = cf + "/noarch/python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"url channel not listed", func(l *Lockfile) {
			l.Packages[0].URL = "https://evil.example/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"sha256 missing", func(l *Lockfile) { l.Packages[0].SHA256 = "" }},
		{"sha256 upper", func(l *Lockfile) { l.Packages[0].SHA256 = strings.ToUpper(pythonSHA) }},
		{"sha256 short", func(l *Lockfile) { l.Packages[0].SHA256 = pythonSHA[:63] }},
		{"sha256 not hex", func(l *Lockfile) { l.Packages[0].SHA256 = "g" + pythonSHA[1:] }},
		{"md5 short", func(l *Lockfile) { l.Packages[0].MD5 = pythonMD5[:31] }},
		{"md5 upper", func(l *Lockfile) { l.Packages[0].MD5 = strings.ToUpper(pythonMD5) }},
		{"md5 sha256", func(l *Lockfile) { l.Packages[0].MD5 = pythonSHA }},
		{"size negative", func(l *Lockfile) { l.Packages[0].Size = -1 }},
		{"depends empty", func(l *Lockfile) { l.Packages[0].Depends = []string{""} }},
		{"depends control", func(l *Lockfile) { l.Packages[0].Depends = []string{"pip\x00"} }},
		{"depends replacement char", func(l *Lockfile) { l.Packages[0].Depends = []string{"pip\ufffd"} }},
		{"constrains newline", func(l *Lockfile) { l.Packages[0].Constrains = []string{"a\nb"} }},
		{"site-packages parent", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "../site-packages" }},
		{"site-packages absolute", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "/lib/site-packages" }},
		{"site-packages backslash", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "lib\\site-packages" }},
		{"site-packages drive", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "C:/site-packages" }},
		{"site-packages unclean", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "lib/./site-packages" }},
		{"site-packages trailing slash", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "lib/site-packages/" }},
		{"site-packages inner dotdot", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "lib/../../etc" }},
	}
	for _, c := range cases {
		l := validLockfile()
		c.mutate(l)
		err := l.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", c.name, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "lock: ") {
			t.Errorf("%s: error %q lacks the package prefix", c.name, err)
		}
		if werr := l.Write(&bytes.Buffer{}); !errors.Is(werr, ErrInvalid) {
			t.Errorf("%s: Write = %v, want ErrInvalid", c.name, werr)
		}
	}
	l := validLockfile()
	l.Version = 2
	if err := l.Validate(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("version 2: %v, want ErrUnsupportedVersion", err)
	}
	var nilLock *Lockfile
	if err := nilLock.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("nil lockfile: %v", err)
	}
}

func TestValidateAccepts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(l *Lockfile)
	}{
		{"no md5", func(l *Lockfile) { l.Packages[0].MD5 = "" }},
		{"tar.bz2", func(l *Lockfile) {
			l.Packages[0].URL = cf + "/linux-64/python-3.11.15-hd63d673_0_cpython.tar.bz2"
		}},
		{"label channel", func(l *Lockfile) {
			l.Channels = append(l.Channels, cf+"/label/rc")
			l.Packages[0].URL = cf + "/label/rc/linux-64/python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"root channel", func(l *Lockfile) {
			l.Channels = append(l.Channels, "https://channel.example:8443")
			l.Packages[0].URL = "https://channel.example:8443/linux-64/python-3.11.15-hd63d673_0_cpython.conda"
		}},
		{"epoch and local version", func(l *Lockfile) {
			l.Packages[0].Version = "1!3.11.15+cpu"
			l.Packages[0].URL = cf + "/linux-64/python-1!3.11.15+cpu-hd63d673_0_cpython.conda"
		}},
		{"site-packages", func(l *Lockfile) { l.Packages[0].PythonSitePackagesPath = "lib/python3.13t/site-packages" }},
		{"unicode spec", func(l *Lockfile) { l.Specs = []string{"python[license='Pythön']"} }},
		{"no specs, no virtual, no packages", func(l *Lockfile) { l.Specs, l.Virtual, l.Packages = nil, nil, nil }},
		// conda-forge ships noarch: python builds in platform subdirs (ruff,
		// uv-build, ty) and defaults has noarch-subdir packages without a
		// noarch value (CDT packages); both must lock.
		{"noarch python in linux-64", func(l *Lockfile) {
			l.Packages = append(l.Packages, Package{
				Name: "ruff", Version: "0.15.8", Build: "h7805a7d_0", Subdir: "linux-64", Noarch: "python",
				URL: cf + "/linux-64/ruff-0.15.8-h7805a7d_0.conda", SHA256: tzdataSHA,
			})
		}},
		{"no noarch in noarch subdir", func(l *Lockfile) {
			l.Packages = append(l.Packages, Package{
				Name: "mesa-libegl-cos6-x86_64", Version: "11.0.7", Build: "1", Subdir: "noarch",
				URL: cf + "/noarch/mesa-libegl-cos6-x86_64-11.0.7-1.tar.bz2", SHA256: tzdataSHA,
			})
		}},
		{"extra channel with a port", func(l *Lockfile) {
			l.Channels = append(l.Channels, "https://mirror.example:8443/cf")
		}},
		{"other platform", func(l *Lockfile) {
			l.Platform = "osx-arm64"
			l.Virtual = map[string]string{"__osx": "13.0", "__unix": "0", "__archspec": "arm64"}
			l.Packages[0].Subdir = "osx-arm64"
			l.Packages[0].URL = cf + "/osx-arm64/python-3.11.15-hd63d673_0_cpython.conda"
		}},
	}
	for _, c := range cases {
		l := validLockfile()
		c.mutate(l)
		if err := l.Validate(); err != nil {
			t.Errorf("%s: Validate = %v", c.name, err)
			continue
		}
		back, err := Read(strings.NewReader(writeString(t, l)))
		if err != nil {
			t.Errorf("%s: Read(Write) = %v", c.name, err)
			continue
		}
		if writeString(t, back) != writeString(t, l) {
			t.Errorf("%s: round trip changed the output", c.name)
		}
	}
}

// TestReadStrict checks that Read rejects JSON that encoding/json would
// silently accept or reinterpret.
func TestReadStrict(t *testing.T) {
	good := writeString(t, validLockfile())
	if _, err := Read(strings.NewReader(good)); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"whitespace", "  \n"},
		{"array", "[]"},
		{"null", "null"},
		{"truncated", good[:len(good)/2]},
		{"trailing object", good + "{}"},
		{"trailing garbage", good + "x"},
		{"bom", "\ufeff" + good},
		{"invalid utf-8", strings.Replace(good, "python=3.11", "python=3.11\xff", 1)},
		{"unknown top key", strings.Replace(good, `"version": 1,`, `"version": 1, "extra": 1,`, 1)},
		{"unknown package key", strings.Replace(good, `"name": "pip",`, `"name": "pip", "channel": "x",`, 1)},
		{"wrong case top key", strings.Replace(good, `"platform"`, `"Platform"`, 1)},
		{"wrong case package key", strings.Replace(good, `"sha256": "`+pythonSHA, `"SHA256": "`+pythonSHA, 1)},
		{"duplicate top key", strings.Replace(good, `"version": 1,`, `"version": 1, "version": 1,`, 1)},
		{"duplicate package key", strings.Replace(good, `"md5": "`+pythonMD5+`",`, `"md5": "`+pythonMD5+`", "md5": "",`, 1)},
		{"duplicate virtual key", strings.Replace(good, `"__glibc": "2.17",`, `"__glibc": "2.17", "__glibc": "2.28",`, 1)},
		{"null string", strings.Replace(good, `"md5": "`+pythonMD5+`"`, `"md5": null`, 1)},
		{"null list", strings.Replace(good, `"specs": [`, `"specs": null, "x": [`, 1)},
		{"null list element", strings.Replace(good, `"python=3.11"`, `null`, 1)},
		{"null virtual", strings.Replace(good, `"virtual": {`, `"virtual": null, "y": {`, 1)},
		{"number as string", strings.Replace(good, `"version": 1,`, `"version": "1",`, 1)},
		{"float version", strings.Replace(good, `"version": 1,`, `"version": 1.0,`, 1)},
		{"exponent version", strings.Replace(good, `"version": 1,`, `"version": 1e0,`, 1)},
		{"huge version", strings.Replace(good, `"version": 1,`, `"version": 99999999999,`, 1)},
		{"negative build number", strings.Replace(good, `"build_number": 1,`, `"build_number": -1,`, 1)},
		{"float build number", strings.Replace(good, `"build_number": 1,`, `"build_number": 1.5,`, 1)},
		{"overflow build number", strings.Replace(good, `"build_number": 1,`, `"build_number": 18446744073709551616,`, 1)},
		{"float size", strings.Replace(good, `"size": 30949404,`, `"size": 30949404.0,`, 1)},
		{"bool name", strings.Replace(good, `"name": "pip"`, `"name": true`, 1)},
		{"object depends", strings.Replace(good, `"depends": [
        "python`, `"depends": {"a": [
        "python`, 1)},
		{"number in list", strings.Replace(good, `"python=3.11"`, `3.11`, 1)},
		{"number in virtual", strings.Replace(good, `"__glibc": "2.17"`, `"__glibc": 2.17`, 1)},
		{"packages object", strings.Replace(good, `"packages": [`, `"packages": {"x": [`, 1)},
		{"package not object", strings.Replace(good, `"packages": [`, `"packages": [1, `, 1)},
		{"invalid after parse", strings.Replace(good, pythonSHA, strings.ToUpper(pythonSHA), 1)},
	}
	for _, c := range cases {
		if c.in == good {
			t.Fatalf("%s: replacement did not apply", c.name)
		}
		l, err := Read(strings.NewReader(c.in))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Read = %v, %v; want ErrInvalid", c.name, l, err)
		}
	}
	// Escapes in keys and values are fine: they decode to the same text.
	escaped := strings.Replace(good, `"platform": "linux-64"`, `"pl\u0061tform": "linux\u002d64"`, 1)
	if _, err := Read(strings.NewReader(escaped)); err != nil {
		t.Errorf("escaped key: %v", err)
	}
	// Errors name the offending location.
	_, err := Read(strings.NewReader(strings.Replace(good, `"name": "pip"`, `"name": 1`, 1)))
	if err == nil || !strings.Contains(err.Error(), "lockfile.packages[0].name") {
		t.Errorf("error does not name the location: %v", err)
	}
	// Oversized input.
	big := strings.Repeat(" ", maxInput+1)
	if _, err := Read(strings.NewReader(big)); !errors.Is(err, ErrInvalid) {
		t.Errorf("oversized: %v", err)
	}
	// Read errors wrap the reader's error.
	if _, err := Read(errReader{}); !errors.Is(err, errBoom) {
		t.Errorf("reader error: %v", err)
	}
}

var errBoom = errors.New("boom")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errBoom }

func TestRecordsAndFromRecords(t *testing.T) {
	l := validLockfile()
	recs := l.Records()
	if len(recs) != 3 {
		t.Fatalf("Records = %d records", len(recs))
	}
	py := recs[0]
	want := record.PackageRecord{
		Name: "python", Version: "3.11.15", Build: "hd63d673_0_cpython",
		Depends:    []string{"__glibc >=2.17,<3.0.a0", "tzdata", "pip"},
		Constrains: []string{"python_abi 3.11.* *_cp311"},
		Subdir:     "linux-64", MD5: pythonMD5, SHA256: pythonSHA, Size: 30949404,
		Fn: "python-3.11.15-hd63d673_0_cpython.conda", URL: pythonURL, Channel: cf,
	}
	if !reflect.DeepEqual(*py, want) {
		t.Errorf("Records()[0] =\n%+v\nwant\n%+v", *py, want)
	}
	if recs[2].Depends == nil || len(recs[2].Depends) != 0 {
		t.Errorf("nil depends not normalized: %#v", recs[2].Depends)
	}
	// Records are copies.
	py.Depends[0] = "changed"
	if l.Packages[0].Depends[0] == "changed" {
		t.Error("Records shares slices with the lockfile")
	}

	virt := &record.PackageRecord{Name: "__glibc", Version: "2.35", Build: "0", Channel: "@", Subdir: "linux-64"}
	in := append(l.Records(), virt)
	in[0].SHA256 = strings.ToUpper(in[0].SHA256)
	in[0].MD5 = strings.ToUpper(in[0].MD5)
	got, err := FromRecords("linux-64", []string{cf + "/"}, l.Specs, l.Virtual, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != 3 || got.Packages[0].Name != "pip" || got.Packages[1].Name != "python" {
		t.Errorf("FromRecords packages = %+v", got.Packages)
	}
	if got.Packages[1].SHA256 != pythonSHA || got.Packages[1].MD5 != pythonMD5 || got.Channels[0] != cf {
		t.Errorf("FromRecords did not normalize hashes or channels: %+v", got)
	}
	if writeString(t, got) != writeString(t, l) {
		t.Errorf("FromRecords(Records()) differs from the original")
	}
	// The result does not alias the inputs.
	in[1].Depends[0] = "mutated"
	l.Virtual["__glibc"] = "9.9"
	if got.Packages[0].Depends[0] == "mutated" || got.Virtual["__glibc"] == "9.9" {
		t.Error("FromRecords retained its inputs")
	}

	bad := l.Records()
	bad[0].Fn = "python-3.11.15-hd63d673_0_cpython.tar.bz2"
	if _, err := FromRecords("linux-64", []string{cf}, nil, nil, bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("Fn mismatch: %v", err)
	}
	if _, err := FromRecords("linux-64", []string{cf}, nil, nil, []*record.PackageRecord{nil}); !errors.Is(err, ErrInvalid) {
		t.Errorf("nil record: %v", err)
	}
	if _, err := FromRecords("linux-64", nil, nil, nil, l.Records()); !errors.Is(err, ErrInvalid) {
		t.Errorf("no channels: %v", err)
	}
	if _, err := FromRecords("win-64", []string{cf}, nil, nil, l.Records()); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong platform: %v", err)
	}
	// Scheme, host case, a trailing dot and :443 are normalized, so one
	// server is one channel.
	for _, spelling := range []string{"HTTPS://Conda.Anaconda.ORG", "https://conda.anaconda.org.:443", "https://CONDA.anaconda.org:443"} {
		shouty := l.Records()
		for _, r := range shouty {
			r.URL = strings.Replace(r.URL, "https://conda.anaconda.org", spelling, 1)
		}
		got, err = FromRecords("linux-64", []string{"HTTPS://CONDA.anaconda.org./conda-forge/"}, l.Specs, l.Virtual, shouty)
		if err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		if writeString(t, got) != writeString(t, l) {
			t.Errorf("%s: FromRecords did not canonicalize hosts:\n%s", spelling, writeString(t, got))
		}
	}
}

func TestExampleFileIsCanonical(t *testing.T) {
	data, err := os.ReadFile(filepath.FromSlash(examplePath))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := writeString(t, l); got != string(data) {
		t.Errorf("Write(Read(%s)) is not byte-identical:\n%s", examplePath, got)
	}
}

// TestBriefExample reads the lockfile example from the work package brief
// (docs/briefs/wp5-virtual-lock.md) verbatim.
func TestBriefExample(t *testing.T) {
	const example = `{
  "version": 1,
  "platform": "linux-64",
  "channels": ["https://conda.anaconda.org/conda-forge"],
  "specs": ["python=3.11"],
  "virtual": {"__glibc": "2.17", "__unix": "0", "__linux": "0"},
  "packages": [
    {"name": "python", "version": "3.11.15", "build": "hd63d673_0_cpython",
     "build_number": 0, "subdir": "linux-64", "noarch": "",
     "url": "https://conda.anaconda.org/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda",
     "sha256": "bf6a32c69889d38482436a786bea32276756cedf0e9805cc856ffd088e8d00f0",
     "md5": "a5ebcefec0c12a333bcd6d7bf3bddc1f", "size": 30949404,
     "depends": ["..."], "constrains": ["python_abi 3.11.* *_cp311"],
     "python_site_packages_path": ""}
  ]
}`
	l, err := Read(strings.NewReader(example))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Packages) != 1 || l.Packages[0].SHA256 != pythonSHA || l.Virtual["__glibc"] != "2.17" {
		t.Errorf("Read(example) = %+v", l)
	}
	back, err := Read(strings.NewReader(writeString(t, l)))
	if err != nil || !reflect.DeepEqual(back, l) {
		t.Errorf("round trip: %v\n%+v\n%+v", err, back, l)
	}
}

func TestCanonicalHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{cf, cf},
		{"HTTPS://Conda.Anaconda.ORG/conda-forge", cf},
		{"https://conda.anaconda.org:443/conda-forge", cf},
		{"https://conda.anaconda.org.:443/conda-forge", cf},
		{"https://conda.anaconda.org./conda-forge", cf},
		{"https://conda.anaconda.org:8443/Conda-Forge", "https://conda.anaconda.org:8443/Conda-Forge"},
		{"https://Example.COM", "https://example.com"},
		{"https://example.com:443", "https://example.com"},
		{"http://example.com:80/x", "http://example.com/x"},
		{"http://example.com:443/x", "http://example.com:443/x"},
		{"https://[2001:DB8::1]:443/c", "https://[2001:db8::1]/c"},
		{"https://[2001:db8::1]/c", "https://[2001:db8::1]/c"},
		{"not a url", "not a url"},
	}
	for _, c := range cases {
		if got := canonicalHost(c.in); got != c.want {
			t.Errorf("canonicalHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
