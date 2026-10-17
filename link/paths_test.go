package link

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestParsePathsJSON(t *testing.T) {
	doc := `{
	  "paths": [
	    {"_path": "bin/tool", "path_type": "hardlink", "file_mode": "text",
	     "prefix_placeholder": "/opt/ph", "sha256": "` + emptySHA256 + `", "size_in_bytes": 12},
	    {"_path": "lib/libx.so", "path_type": "softlink", "sha256": "` + emptySHA256 + `", "size_in_bytes": 7},
	    {"_path": "lib/libx.so.1", "path_type": "hardlink", "file_mode": "binary", "prefix_placeholder": "/opt/ph", "no_link": true},
	    {"_path": "share/empty", "path_type": "directory"},
	    {"_path": "etc/odd", "path_type": "hardlink", "file_mode": "other", "extra": [1, 2]}
	  ],
	  "paths_version": 1,
	  "unknown": true
	}`
	got, err := ParsePathsJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []PathEntry{
		{Path: "bin/tool", PathType: PathHardlink, FileMode: FileModeText, PrefixPlaceholder: "/opt/ph", SHA256: emptySHA256, SizeInBytes: 12},
		{Path: "lib/libx.so", PathType: PathSoftlink, SHA256: emptySHA256, SizeInBytes: 7},
		{Path: "lib/libx.so.1", PathType: PathHardlink, FileMode: FileModeBinary, PrefixPlaceholder: "/opt/ph", NoLink: true},
		{Path: "share/empty", PathType: PathDirectory},
		{Path: "etc/odd", PathType: PathHardlink},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParsePathsJSON:\n got %+v\nwant %+v", got, want)
	}
}

// Cases from rattler crates/rattler_conda_types/src/package/paths.rs:
// test_optional_fields_handling and test_deserialize_cep_examples. The
// draft-CEP "offsets" and "shebang_length" fields are ignored, as libmamba
// ignores them.
func TestParsePathsJSONRattler(t *testing.T) {
	minimal := `{"paths": [{"_path": "file.txt", "path_type": "hardlink"}], "paths_version": 1}`
	got, err := ParsePathsJSON([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if want := []PathEntry{{Path: "file.txt", PathType: PathHardlink}}; !reflect.DeepEqual(got, want) {
		t.Errorf("minimal: %+v", got)
	}
	cep := `{"paths": [{
	    "_path": "bin/example-script", "path_type": "hardlink", "file_mode": "text",
	    "prefix_placeholder": "/opt/placeholder",
	    "offsets": [{"encoding": "utf-8", "ranges": [71]}], "shebang_length": 30,
	    "sha256": "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3", "size_in_bytes": 512
	  }, {
	    "_path": "lib/libexample.so", "path_type": "hardlink", "file_mode": "binary",
	    "prefix_placeholder": "/opt/placeholder",
	    "offsets": [{"encoding": "utf-16-le", "ranges": [[384, 448]]}, {"encoding": "utf-8", "ranges": [[64, 96], [200, 240, 300]]}],
	    "sha256": "d7a8fbb307d7809469ca9abcb0082e4f8d5651e46d3cdb762d02d0bf37c9e592", "size_in_bytes": 4096
	  }], "paths_version": 1}`
	got, err = ParsePathsJSON([]byte(cep))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].FileMode != FileModeText || got[1].FileMode != FileModeBinary || got[1].SizeInBytes != 4096 {
		t.Errorf("cep examples: %+v", got)
	}
}

func TestParsePathsJSONInvalid(t *testing.T) {
	cases := map[string]struct {
		doc  string
		want error
	}{
		"not json":       {`{`, ErrInvalidPackage},
		"no version":     {`{"paths": []}`, ErrInvalidPackage},
		"version 2":      {`{"paths": [], "paths_version": 2}`, ErrInvalidPackage},
		"no paths":       {`{"paths_version": 1}`, ErrInvalidPackage},
		"no _path":       {`{"paths": [{"path_type": "hardlink"}], "paths_version": 1}`, ErrInvalidPackage},
		"no path_type":   {`{"paths": [{"_path": "a"}], "paths_version": 1}`, ErrInvalidPackage},
		"bad path_type":  {`{"paths": [{"_path": "a", "path_type": "hard"}], "paths_version": 1}`, ErrInvalidPackage},
		"bad sha":        {`{"paths": [{"_path": "a", "path_type": "hardlink", "sha256": "xyz"}], "paths_version": 1}`, ErrInvalidPackage},
		"negative size":  {`{"paths": [{"_path": "a", "path_type": "hardlink", "size_in_bytes": -1}], "paths_version": 1}`, ErrInvalidPackage},
		"wrong type":     {`{"paths": [{"_path": 3, "path_type": "hardlink"}], "paths_version": 1}`, ErrInvalidPackage},
		"absolute":       {`{"paths": [{"_path": "/etc/passwd", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"dotdot":         {`{"paths": [{"_path": "../x", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"inner dotdot":   {`{"paths": [{"_path": "a/../../x", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"dot":            {`{"paths": [{"_path": "./x", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"double slash":   {`{"paths": [{"_path": "a//x", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"trailing slash": {`{"paths": [{"_path": "a/", "path_type": "directory"}], "paths_version": 1}`, ErrUnsafePath},
		"backslash":      {`{"paths": [{"_path": "a\\..\\..\\x", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"drive":          {`{"paths": [{"_path": "C:/x", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"nul":            {`{"paths": [{"_path": "a\u0000b", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"newline":        {`{"paths": [{"_path": "a\nb", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
		"empty":          {`{"paths": [{"_path": "", "path_type": "hardlink"}], "paths_version": 1}`, ErrUnsafePath},
	}
	for name, c := range cases {
		if _, err := ParsePathsJSON([]byte(c.doc)); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

// Cases from rattler crates/rattler_conda_types/src/package/has_prefix.rs
// (test_parse_has_prefix), read with libmamba's read_has_prefix rules. The
// quoted form ("/opt/anaconda1 anaconda2anaconda3" binary "lib/pkg
// config/zlib.pc") is a rattler extension: libmamba splits on spaces and
// refuses the line, and so does this port.
func TestParseHasPrefix(t *testing.T) {
	got, err := parseHasPrefix([]byte("/opt/anaconda1anaconda2anaconda3 text lib/pkgconfig/zlib.pc\r\n" +
		"\n   lib/pkgconfig/other.pc  \n" +
		"/ph binary lib/libz.so\n" +
		"/ph xyz lib/odd\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]prefixFile{
		"lib/pkgconfig/zlib.pc":  {placeholder: "/opt/anaconda1anaconda2anaconda3", mode: FileModeText},
		"lib/pkgconfig/other.pc": {placeholder: defaultPrefixPlaceholder, mode: FileModeText},
		"lib/libz.so":            {placeholder: "/ph", mode: FileModeBinary},
		"lib/odd":                {placeholder: "/ph", mode: FileModeBinary},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseHasPrefix = %+v", got)
	}
	if defaultPrefixPlaceholder != "/opt/anaconda1anaconda2anaconda3" {
		t.Errorf("placeholder %q", defaultPrefixPlaceholder)
	}
	for _, bad := range []string{
		`"/opt/anaconda1 anaconda2anaconda3" binary "lib/pkg config/zlib.pc"`,
		"a b",
		"a b c d",
	} {
		if _, err := parseHasPrefix([]byte(bad)); !errors.Is(err, ErrInvalidPackage) {
			t.Errorf("parseHasPrefix(%q) err = %v", bad, err)
		}
	}
}

func TestReadLines(t *testing.T) {
	got := readLines([]byte(" a \r\n\n\tb\r\n  \nc"))
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("readLines = %q", got)
	}
}

// Cases from rattler crates/rattler_conda_types/src/package/link.rs
// (test_link_json), using rattler's test-data/link-json files.
func TestParseLinkJSON(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"testdata/link-json/jupyterlab-link.json", []string{
			"jlpm = jupyterlab.jlpmapp:main",
			"jupyter-lab = jupyterlab.labapp:main",
			"jupyter-labextension = jupyterlab.labextensions:main",
			"jupyter-labhub = jupyterlab.labhubapp:main",
		}},
		{"testdata/link-json/setuptools-link.json", nil},
		{"testdata/link-json/tzdata-link.json", nil},
	}
	for _, c := range cases {
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseLinkJSON(data)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q", c.file, got)
		}
		for _, def := range got {
			if _, err := ParseEntryPoint(def); err != nil {
				t.Errorf("%s: %v", c.file, err)
			}
		}
	}
	for _, bad := range []string{`[]`, `{"noarch": {"entry_points": [1]}}`, `{"noarch": {"entry_points": "x = y:z"}}`} {
		if _, err := parseLinkJSON([]byte(bad)); !errors.Is(err, ErrInvalidPackage) {
			t.Errorf("parseLinkJSON(%s) err = %v", bad, err)
		}
	}
}
