package link

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

// The layout of nlohmann::json::dump(4), which micromamba uses for
// conda-meta records.
func TestMarshalNlohmann(t *testing.T) {
	v := jsonObject{
		"b":     []any{},
		"a":     jsonObject{},
		"c":     []string{"x", "y"},
		"d":     jsonObject{"z": nil, "y": true, "x": int64(-3), "w": uint64(7), "v": json.Number("1.5")},
		"e":     "q\"\\/\b\f\n\r\t\x01\x1f\x7f é ☃ <&>",
		"f":     []any{jsonObject{"k": 1}, []any{}},
		"Upper": false,
	}
	want := `{
    "Upper": false,
    "a": {},
    "b": [],
    "c": [
        "x",
        "y"
    ],
    "d": {
        "v": 1.5,
        "w": 7,
        "x": -3,
        "y": true,
        "z": null
    },
    "e": "q\"\\/\b\f\n\r\t\u0001\u001f` + "\x7f" + ` é ☃ <&>",
    "f": [
        {
            "k": 1
        },
        []
    ]
}`
	got, err := marshalNlohmann(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if _, err := marshalNlohmann("bad \xff utf-8"); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
	if _, err := marshalNlohmann(json.Number("1e")); err == nil {
		t.Error("invalid number accepted")
	}
	if _, err := marshalNlohmann(struct{}{}); err == nil {
		t.Error("unsupported type accepted")
	}
}

func TestWriteCondaMeta(t *testing.T) {
	tmp := t.TempDir()
	spec := pkgSpec{name: "meta", noarch: "generic", files: []pkgFile{{path: "share/x", data: "x"}}, index: map[string]any{
		"license_family": "MIT", "features": "", "license": "index-license", "nested": map[string]any{"b": []any{1, "two"}, "a": nil},
	}}
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), spec)
	rec.Noarch = "" // taken from index.json
	rec.TrackFeatures = []string{"tf1", "tf2"}
	rec.Constrains = []string{"other >=1"}
	rec.Timestamp = 1700000000999
	prefix := filepath.Join(tmp, "env")
	opt := Options{Prefix: prefix, Platform: "linux-64", RequestedSpec: "meta"}
	inst := linkAndRecord(t, pkgDir, rec, opt)
	if inst.Noarch != "generic" {
		t.Errorf("Noarch = %q", inst.Noarch)
	}
	data, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "meta-1.0-h0_0.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := decodeJSONValue(data)
	if err != nil {
		t.Fatal(err)
	}
	obj := v.(map[string]any)
	want := map[string]any{
		"arch":                      nil,
		"build":                     "h0_0",
		"build_number":              json.Number("0"),
		"build_string":              "h0_0",
		"channel":                   "conda-forge",
		"constrains":                []any{"other >=1"},
		"depends":                   []any{"python >=3.8"},
		"extracted_package_dir":     pkgDir,
		"features":                  "",
		"files":                     []any{"share/x"},
		"fn":                        "meta-1.0-h0_0.conda",
		"license":                   "MIT",
		"license_family":            "MIT",
		"link":                      map[string]any{"source": pkgDir, "type": json.Number("1")},
		"md5":                       strings.Repeat("cd", 16),
		"name":                      "meta",
		"nested":                    map[string]any{"a": nil, "b": []any{json.Number("1"), "two"}},
		"noarch":                    "generic",
		"package_tarball_full_path": pkgDir + ".tar.bz2",
		"paths_data": map[string]any{"paths": []any{map[string]any{
			"_path": "share/x", "path_type": "hardlink", "sha256": sha256Hex([]byte("x")),
			"sha256_in_prefix": sha256Hex([]byte("x")), "size_in_bytes": json.Number("1"),
		}}, "paths_version": json.Number("1")},
		"platform":       nil,
		"requested_spec": "meta",
		"sha256":         strings.Repeat("ab", 32),
		"size":           json.Number("1234"),
		"subdir":         "noarch",
		"timestamp":      json.Number("1700000000"),
		"track_features": "tf1,tf2",
		"url":            "https://conda.anaconda.org/conda-forge/noarch/meta-1.0-h0_0.conda",
		"version":        "1.0",
	}
	if !reflect.DeepEqual(obj, want) {
		for k := range want {
			if !reflect.DeepEqual(obj[k], want[k]) {
				t.Errorf("%s = %#v, want %#v", k, obj[k], want[k])
			}
		}
		for k := range obj {
			if _, ok := want[k]; !ok {
				t.Errorf("extra key %s", k)
			}
		}
	}

	recs, err := ReadCondaMeta(prefix)
	if err != nil {
		t.Fatal(err)
	}
	got := recs[0].Record
	if got.Noarch != "generic" || got.Timestamp != 1700000000000 || !reflect.DeepEqual(got.TrackFeatures, rec.TrackFeatures) || got.Channel != "https://conda.anaconda.org/conda-forge" {
		t.Errorf("read back %+v", got)
	}

	// A nil record or result is refused, as is an unsafe prefix.
	if err := WriteCondaMeta(prefix, nil, inst, opt); err == nil {
		t.Error("nil record accepted")
	}
	if err := WriteCondaMeta(prefix, rec, nil, opt); err == nil {
		t.Error("nil result accepted")
	}
	if err := WriteCondaMeta("rel", rec, inst, opt); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("relative prefix: %v", err)
	}
	bad := *rec
	bad.Name = "../../x"
	if err := WriteCondaMeta(prefix, &bad, inst, opt); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("unsafe name: %v", err)
	}
}

func TestChannelNames(t *testing.T) {
	cases := []struct {
		channel, url, subdir, fn string
		name, canonical          string
	}{
		{"https://conda.anaconda.org/conda-forge", "https://conda.anaconda.org/conda-forge/noarch/a-1-0.conda", "noarch", "a-1-0.conda", "conda-forge", "https://conda.anaconda.org/conda-forge"},
		{"https://conda.anaconda.org/conda-forge/", "", "", "", "conda-forge", "https://conda.anaconda.org/conda-forge"},
		{"https://repo.example.com/main", "https://repo.example.com/main/linux-64/a-1-0.tar.bz2", "linux-64", "a-1-0.tar.bz2", "https://repo.example.com/main", "https://repo.example.com/main"},
		{"", "https://conda.anaconda.org/bioconda/linux-64/a-1-0.conda", "linux-64", "a-1-0.conda", "bioconda", "https://conda.anaconda.org/bioconda"},
		{"", "", "", "", "", ""},
	}
	for _, c := range cases {
		rec := &record.PackageRecord{Channel: c.channel, URL: c.url, Subdir: c.subdir, Fn: c.fn}
		if got := channelName(rec); got != c.name {
			t.Errorf("channelName(%+v) = %q, want %q", c, got, c.name)
		}
		if got := canonicalChannel(c.name, c.url, c.subdir, c.fn); got != c.canonical {
			t.Errorf("canonicalChannel(%q, %q) = %q, want %q", c.name, c.url, got, c.canonical)
		}
	}
}

func TestParseCondaMeta(t *testing.T) {
	doc := `{"name": "a", "version": "1", "build_string": "b0", "build_number": 3,
		"track_features": "x y,z", "noarch": true, "timestamp": 1700000000,
		"features": "f", "size": 10, "channel": "conda-forge", "license": null,
		"files": ["a"], "paths_data": {"paths": [{"_path": "a", "path_type": "hardlink", "size_in_bytes": 2, "no_link": true}]},
		"requested_spec": "a", "extracted_package_dir": "/pkgs/a-1-b0"}`
	r, err := parseCondaMeta([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := &record.PackageRecord{Name: "a", Version: "1", Build: "b0", BuildNumber: 3, TrackFeatures: []string{"x", "y", "z"},
		Noarch: "generic", Timestamp: 1700000000000, Features: "f", Size: 10, Channel: "https://conda.anaconda.org/conda-forge"}
	if !reflect.DeepEqual(r.Record, want) {
		t.Errorf("record %+v", r.Record)
	}
	if !reflect.DeepEqual(r.Paths, []PathRecord{{Path: "a", PathType: PathHardlink, SizeInBytes: 2, NoLink: true}}) || r.RequestedSpec != "a" || r.PackageDir != "/pkgs/a-1-b0" {
		t.Errorf("parsed %+v", r)
	}
	// "build" wins over "build_string"; track_features may be a list.
	r, err = parseCondaMeta([]byte(`{"name": "a", "build": "b1", "build_string": "b0", "track_features": ["p", ""], "noarch": "python", "timestamp": 1.7e12}`))
	if err != nil || r.Record.Build != "b1" || !reflect.DeepEqual(r.Record.TrackFeatures, []string{"p"}) || r.Record.Noarch != "python" || r.Record.Timestamp != 1700000000000 {
		t.Errorf("parsed %+v, %v", r, err)
	}
	for _, bad := range []string{`[]`, `{"build_number": -1}`, `{"timestamp": "x"}`, `{"track_features": 3}`, `{"noarch": 1}`, `{"features": 1}`} {
		if _, err := parseCondaMeta([]byte(bad)); err == nil {
			t.Errorf("parseCondaMeta(%s) accepted", bad)
		}
	}
	// ReadCondaMeta on a prefix without conda-meta has no records.
	recs, err := ReadCondaMeta(t.TempDir())
	if err != nil || recs != nil {
		t.Errorf("empty prefix: %v %v", recs, err)
	}
}

// The history format of libmamba's History::add_entry.
func TestAppendHistory(t *testing.T) {
	prefix := t.TempDir()
	recA := &record.PackageRecord{Name: "a", Version: "1.0", Build: "h0", Subdir: "linux-64", Channel: "https://conda.anaconda.org/conda-forge"}
	recB := &record.PackageRecord{Name: "b", Version: "2", Build: "py_0", Subdir: "noarch", URL: "https://example.com/ch/noarch/b-2-py_0.conda", Fn: "b-2-py_0.conda"}
	when := time.Date(2026, 3, 29, 13, 9, 22, 0, time.UTC)
	if err := AppendHistory(prefix, "goomba create -n x a b", []*record.PackageRecord{recA, recB}, nil, when); err != nil {
		t.Fatal(err)
	}
	e := HistoryEntry{Time: when.Add(time.Hour), Cmd: "goomba remove a", Removed: []*record.PackageRecord{recA}, Added: []*record.PackageRecord{recB},
		Update: []string{`b >=2`, `c "quoted" \x`}, Remove: []string{"a"}, Neutered: []string{"n"}}
	if err := AppendHistoryEntry(prefix, e); err != nil {
		t.Fatal(err)
	}
	want := `==> 2026-03-29 13:09:22 <==
# cmd: goomba create -n x a b
# conda version: 3.8.0
+https://conda.anaconda.org/conda-forge/linux-64::a-1.0-h0
+https://example.com/ch/noarch::b-2-py_0
==> 2026-03-29 14:09:22 <==
# cmd: goomba remove a
# conda version: 3.8.0
-https://conda.anaconda.org/conda-forge/linux-64::a-1.0-h0
+https://example.com/ch/noarch::b-2-py_0
# update specs: ["b >=2", "c \"quoted\" \\x"]
# remove specs: ["a"]
# neutered specs: ["n"]
`
	if got := readString(t, filepath.Join(prefix, "conda-meta", "history")); got != want {
		t.Errorf("history:\n%s\nwant\n%s", got, want)
	}
	for _, bad := range []HistoryEntry{
		{Cmd: "x\n==> fake <=="},
		{Cmd: "x", Update: []string{"a\nb"}},
		{Cmd: "x", Added: []*record.PackageRecord{nil}},
		{Cmd: "x", Added: []*record.PackageRecord{{Name: "a\n", Version: "1", Build: "0"}}},
	} {
		if err := AppendHistoryEntry(prefix, bad); err == nil {
			t.Errorf("AppendHistoryEntry(%+v) accepted", bad)
		}
	}
	if got := readString(t, filepath.Join(prefix, "conda-meta", "history")); got != want {
		t.Error("history changed by refused entries")
	}
	if err := AppendHistory("relative", "x", nil, nil, when); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("relative prefix: %v", err)
	}
}
