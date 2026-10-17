package repodata

import (
	"bytes"
	"compress/bzip2"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

const cfChannel = "https://conda.anaconda.org/conda-forge"

func loadJSON(t *testing.T, doc, subdir string) []*record.PackageRecord {
	t.Helper()
	recs, err := LoadRepodataJSON(strings.NewReader(doc), cfChannel, subdir)
	if err != nil {
		t.Fatalf("LoadRepodataJSON: %v", err)
	}
	return recs
}

// Cases from libmamba tests/src/specs/test_repo_data.cpp
// "RepoDataPackage_from_json". The record is wrapped in a linux-64
// repodata.json. Differences: Subdir is the subdir the record was loaded
// from ("linux-64"), not the record's own "subdir" field ("linux").
func TestRepoDataPackageFromJSON(t *testing.T) {
	const base = `"name": "mamba", "version": "1.1.0", "build": "foo1", "build_number": 2,
		"python_site_packages_path": "dummy_pspp", "subdir": "linux", "platform": null,
		"depends": ["libsolv>=1.0"], "constrains": [], "track_features": []`
	cases := []struct {
		noarch string // JSON value, "" = absent
		want   string
	}{
		{"", ""},
		{`"python"`, "python"},
		{`true`, "generic"}, // old behaviour
		{`false`, ""},
	}
	for _, c := range cases {
		fields := base
		if c.noarch != "" {
			fields += `, "noarch": ` + c.noarch
		}
		recs := loadJSON(t, `{"info": {"subdir": "linux-64"}, "packages": {"mamba-1.1.0-foo1.tar.bz2": {`+fields+`}}}`, "linux-64")
		if len(recs) != 1 {
			t.Fatalf("noarch %s: got %d records", c.noarch, len(recs))
		}
		p := recs[0]
		want := record.PackageRecord{
			Name: "mamba", Version: "1.1.0", Build: "foo1", BuildNumber: 2,
			PythonSitePackagesPath: "dummy_pspp",
			Depends:                []string{"libsolv>=1.0"},
			Noarch:                 c.want,
			Subdir:                 "linux-64",
			Fn:                     "mamba-1.1.0-foo1.tar.bz2",
			URL:                    cfChannel + "/linux-64/mamba-1.1.0-foo1.tar.bz2",
			Channel:                cfChannel,
		}
		if !reflect.DeepEqual(*p, want) {
			t.Errorf("noarch %s:\n got %+v\nwant %+v", c.noarch, *p, want)
		}
		if p.MD5 != "" || len(p.TrackFeatures) != 0 {
			t.Errorf("noarch %s: md5 %q, track_features %q", c.noarch, p.MD5, p.TrackFeatures)
		}
	}
}

// Cases from libmamba tests/src/specs/test_repo_data.cpp "RepoData_from_json".
// The "conda_packages" key is not "packages.conda" and is ignored.
// Deviation: the upstream file name "mamba-1.0-h12345.tar.bz2" does not match
// the record (mamba 1.1.0 foo1), so goomba drops it; the case is also run
// with the matching file name.
func TestRepoDataFromJSON(t *testing.T) {
	upstream := `{
		"version": 1,
		"info": {"subdir": "osx-arm64"},
		"packages": {"mamba-1.0-h12345.tar.bz2": {
			"name": "mamba", "version": "1.1.0", "build": "foo1", "build_number": 2,
			"subdir": "linux", "depends": ["libsolv>=1.0"], "constrains": [], "track_features": []}},
		"conda_packages": {},
		"removed": ["bad-package.tar.gz"]
	}`
	if recs := loadJSON(t, upstream, ""); len(recs) != 0 {
		t.Errorf("mismatched file name kept: %+v", recs)
	}
	doc := `{
		"version": 1,
		"info": {"subdir": "osx-arm64"},
		"packages": {"mamba-1.1.0-foo1.tar.bz2": {
			"name": "mamba", "version": "1.1.0", "build": "foo1", "build_number": 2,
			"subdir": "linux", "depends": ["libsolv>=1.0"], "constrains": [], "track_features": []}},
		"conda_packages": {},
		"removed": ["bad-package.tar.gz"]
	}`
	recs := loadJSON(t, doc, "")
	if len(recs) != 1 || recs[0].Name != "mamba" || recs[0].Subdir != "osx-arm64" {
		t.Fatalf("got %+v", recs)
	}
	for _, r := range recs {
		if strings.HasSuffix(r.Fn, ".conda") {
			t.Errorf("unexpected .conda record %s", r.Fn)
		}
	}
}

// Port of libmamba tests/src/specs/test_repo_data.cpp "repodata_json",
// which loads $MAMBA_REPODATA_JSON when set; here, the micromamba test
// channels copied into testdata. Their "A_0.1.0.tar.bz2"-style file names do
// not match name-version-build, so goomba drops those records.
func TestRepodataJSONFixtures(t *testing.T) {
	cases := []struct {
		path, subdir string
		want         []string
	}{
		{"micromamba/test-server/noarch/repodata.json", "noarch", []string{"test-package-0.1-0.tar.bz2"}},
		{"micromamba/channel_a/linux-64/repodata.json", "linux-64", []string{}},
		{"micromamba/channel_a/noarch/repodata.json", "noarch", []string{"_r-mutex-1.0.1-anacondar_1.tar.bz2"}},
		{"micromamba/channel_b/linux-64/repodata.json", "linux-64", []string{}},
		{"micromamba/channel_b/noarch/repodata.json", "noarch", []string{}},
	}
	for _, c := range cases {
		recs, err := LoadRepodataJSON(bytes.NewReader(readTestdata(t, c.path)), "https://example.com/channel", c.subdir)
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if got := fns(recs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.path, got, c.want)
		}
	}
	// The bz2 variant decodes to the same records.
	plain := loadJSON(t, string(readTestdata(t, "micromamba/test-server/noarch/repodata.json")), "noarch")
	bz, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(readTestdata(t, "micromamba/test-server/noarch/repodata.json.bz2"))))
	if err != nil {
		t.Fatal(err)
	}
	if fromBz := loadJSON(t, string(bz), "noarch"); !reflect.DeepEqual(plain, fromBz) {
		t.Errorf("bz2 fixture differs: %+v vs %+v", plain, fromBz)
	}
	want := record.PackageRecord{
		Name: "test-package", Version: "0.1", Build: "0", License: "BSD",
		MD5: "2a8595f37faa2950e1b433acbe91d481", SHA256: "b908ffce2d26d94c58c968abf286568d4bcf87d1cfe6c994958351724a6f6988",
		Size: 5719, Noarch: "generic", Subdir: "noarch", Timestamp: 1613117294885,
		Fn: "test-package-0.1-0.tar.bz2", URL: cfChannel + "/noarch/test-package-0.1-0.tar.bz2", Channel: cfChannel,
	}
	if !reflect.DeepEqual(*plain[0], want) {
		t.Errorf("test-package:\n got %+v\nwant %+v", *plain[0], want)
	}
	// channel_b/noarch is "{}": without a subdir argument there is no subdir.
	if _, err := LoadRepodataJSON(strings.NewReader("{}"), cfChannel, ""); err == nil {
		t.Error("no subdir anywhere: want error")
	}
}

func pkgDoc(fields string) string {
	return `{"info": {"subdir": "linux-64"}, "packages": {"pkg-1.0-0.tar.bz2": {"name": "pkg", "version": "1.0", "build": "0"` + fields + `}}}`
}

func TestRepodataJSONNormalization(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		check  func(*record.PackageRecord) bool
	}{
		{"track_features string", `, "track_features": "mkl,blas_x  cuda"`, func(r *record.PackageRecord) bool {
			return reflect.DeepEqual(r.TrackFeatures, []string{"mkl", "blas_x", "cuda"})
		}},
		{"track_features empty string", `, "track_features": ""`, func(r *record.PackageRecord) bool { return r.TrackFeatures == nil }},
		{"track_features list", `, "track_features": ["a", "", " b "]`, func(r *record.PackageRecord) bool {
			return reflect.DeepEqual(r.TrackFeatures, []string{"a", "b"})
		}},
		{"timestamp seconds", `, "timestamp": 1772730362`, func(r *record.PackageRecord) bool { return r.Timestamp == 1772730362000 }},
		{"timestamp milliseconds", `, "timestamp": 1772730362123`, func(r *record.PackageRecord) bool { return r.Timestamp == 1772730362123 }},
		{"timestamp float", `, "timestamp": 1.6e12`, func(r *record.PackageRecord) bool { return r.Timestamp == 1600000000000 }},
		{"uppercase hashes", `, "md5": "2A8595F37FAA2950E1B433ACBE91D481", "sha256": "B908FFCE2D26D94C58C968ABF286568D4BCF87D1CFE6C994958351724A6F6988"`, func(r *record.PackageRecord) bool {
			return r.MD5 == "2a8595f37faa2950e1b433acbe91d481" && r.SHA256 == "b908ffce2d26d94c58c968abf286568d4bcf87d1cfe6c994958351724a6f6988"
		}},
		{"null fields", `, "md5": null, "depends": null, "noarch": null, "license": null, "size": null`, func(r *record.PackageRecord) bool {
			return r.MD5 == "" && r.Depends == nil && r.Noarch == "" && r.Size == 0
		}},
		{"missing build_number is 0", ``, func(r *record.PackageRecord) bool { return r.BuildNumber == 0 }},
		{"features kept verbatim", `, "features": "vc9 blas"`, func(r *record.PackageRecord) bool { return r.Features == "vc9 blas" }},
		{"exact key match", `, "SHA256": "zz", "Depends": 5, "sha256": "` + testSHA + `"`, func(r *record.PackageRecord) bool {
			return r.SHA256 == testSHA && r.Depends == nil
		}},
		{"unknown fields ignored", `, "run_exports": {"weak": ["x"]}, "arch": "x86_64", "app_entry": 3`, func(r *record.PackageRecord) bool { return true }},
	}
	for _, c := range cases {
		recs := loadJSON(t, pkgDoc(c.fields), "linux-64")
		if len(recs) != 1 {
			t.Errorf("%s: got %d records", c.name, len(recs))
			continue
		}
		if !c.check(recs[0]) {
			t.Errorf("%s: unexpected record %+v", c.name, *recs[0])
		}
	}
}

func TestRepodataJSONDropsMalformedRecords(t *testing.T) {
	for _, fields := range []string{
		`, "name": 3`,
		`, "name": ""`,
		`, "name": "a/b"`,
		`, "version": null`,
		`, "build": "x y"`,
		`, "build_number": -1`,
		`, "build_number": 1.5`,
		`, "build_number": "1"`,
		`, "depends": "python"`,
		`, "depends": ["python", 3]`,
		`, "constrains": [null]`,
		`, "depends": [""]`,
		`, "depends": ["  "]`,
		`, "track_features": 7`,
		`, "track_features": [1]`,
		`, "noarch": "other"`,
		`, "noarch": 1`,
		`, "md5": "xyz"`,
		`, "sha256": "` + testSHA[:63] + `"`,
		`, "sha256": 5`,
		`, "size": -5`,
		`, "size": 18446744073709551615`,
		`, "timestamp": "123"`,
		`, "timestamp": -1`,
		`, "license": ["MIT"]`,
		`, "python_site_packages_path": "../../etc"`,
		`, "python_site_packages_path": "/abs"`,
		`, "features": {}`,
	} {
		if recs := loadJSON(t, pkgDoc(fields), "linux-64"); len(recs) != 0 {
			t.Errorf("fields %s: record kept: %+v", fields, *recs[0])
		}
	}
	// A record that is not an object, and unsafe or mismatched file names.
	doc := `{"packages": {
		"a-1-0.tar.bz2": 5,
		"b-1-0.tar.bz2": ["x"],
		"../c-1-0.tar.bz2": {"name": "c", "version": "1", "build": "0"},
		"d-1-0.conda": {"name": "d", "version": "1", "build": "0"},
		"e-1-0.tar.bz2": null,
		"ok-1-0.tar.bz2": {"name": "ok", "version": "1", "build": "0"}
	}, "packages.conda": {"f-1-0.tar.bz2": {"name": "f", "version": "1", "build": "0"}}}`
	if got := fns(loadJSON(t, doc, "noarch")); !reflect.DeepEqual(got, []string{"ok-1-0.tar.bz2"}) {
		t.Errorf("got %q", got)
	}
}

func TestRepodataJSONRemovedAndConda(t *testing.T) {
	doc := `{"info": {"subdir": "linux-64"}, "removed": ["gone-1-0.conda"],
	"packages": {
		"bzip2-1.0.8-hd590300_5.tar.bz2": {"name": "bzip2", "version": "1.0.8", "build": "hd590300_5", "md5": "` + testMD5 + `"},
		"bzip2-1.0.8-h7f98852_4.tar.bz2": {"name": "bzip2", "version": "1.0.8", "build": "h7f98852_4"},
		"gone-1-0.tar.bz2": {"name": "gone", "version": "1", "build": "0"}
	},
	"packages.conda": {
		"bzip2-1.0.8-hd590300_5.conda": {"name": "bzip2", "version": "1.0.8", "build": "hd590300_5", "sha256": "` + testSHA + `"},
		"gone-1-0.conda": {"name": "gone", "version": "1", "build": "0"}
	}}`
	got := fns(loadJSON(t, doc, "linux-64"))
	want := []string{"bzip2-1.0.8-h7f98852_4.tar.bz2", "bzip2-1.0.8-hd590300_5.conda", "gone-1-0.tar.bz2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRepodataJSONBaseURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{"", cfChannel + "/linux-64/pkg-1.0-0.tar.bz2"},
		{"https://mirror.example.com/cf/linux-64", "https://mirror.example.com/cf/linux-64/pkg-1.0-0.tar.bz2"},
		{"../../packages/", "https://conda.anaconda.org/packages/pkg-1.0-0.tar.bz2"},
		{"pkgs", cfChannel + "/linux-64/pkgs/pkg-1.0-0.tar.bz2"},
		{"/", cfChannel + "/linux-64/pkg-1.0-0.tar.bz2"},
	}
	for _, c := range cases {
		// info comes after packages: placement waits for it.
		doc := `{"repodata_version": 2, "packages": {"pkg-1.0-0.tar.bz2": {"name": "pkg", "version": "1.0", "build": "0"}},
			"info": {"subdir": "linux-64", "base_url": "` + c.base + `"}}`
		recs := loadJSON(t, doc, "linux-64")
		if len(recs) != 1 || recs[0].URL != c.want || recs[0].Channel != cfChannel {
			t.Errorf("base_url %q: got %+v, want URL %s", c.base, recs, c.want)
		}
	}
	doc := `{"info": {"subdir": "linux-64", "base_url": "http://evil.example.com/"}, "packages": {}}`
	if _, err := LoadRepodataJSON(strings.NewReader(doc), cfChannel, "linux-64"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("http base_url: err = %v, want ErrUnsupportedURL", err)
	}
}

func TestRepodataJSONDocumentErrors(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":             ``,
		"array":             `[]`,
		"truncated":         `{"packages": {"a-1-0.tar.bz2": {"name": "a"`,
		"trailing data":     `{} {}`,
		"trailing garbage":  `{}x`,
		"packages array":    `{"packages": []}`,
		"packages string":   `{"packages": "x"}`,
		"removed object":    `{"removed": {}}`,
		"removed numbers":   `{"removed": [1]}`,
		"info array":        `{"info": []}`,
		"info subdir int":   `{"info": {"subdir": 5}}`,
		"subdir mismatch":   `{"info": {"subdir": "osx-64"}}`,
		"invalid json":      `{"packages": {,}}`,
		"bad info.subdir":   `{"info": {"subdir": "../x"}}`,
		"base_url not text": `{"info": {"base_url": 3}}`,
	} {
		if _, err := LoadRepodataJSON(strings.NewReader(doc), cfChannel, map[bool]string{true: "", false: "linux-64"}[name == "bad info.subdir"]); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := LoadRepodataJSON(strings.NewReader(`{}`), "http://example.com/c", "noarch"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("http channel: err = %v", err)
	}
	if _, err := LoadRepodataJSON(strings.NewReader(`{}`), cfChannel, "linux/64"); err == nil {
		t.Error("bad subdir argument accepted")
	}
	// null sections are fine.
	if recs := loadJSON(t, `{"packages": null, "packages.conda": null, "removed": null, "info": null}`, "noarch"); len(recs) != 0 {
		t.Errorf("got %d records", len(recs))
	}
}

// A repeated file name means its last value (as Python's json reads it); an
// invalid last value drops the record. A repeated top-level section is an
// error.
func TestRepodataJSONDuplicateKeys(t *testing.T) {
	ff := strings.Repeat("f", 32)
	doc := `{"packages": {
		"a-1-0.tar.bz2": {"name": "a", "version": "1", "build": "0", "md5": "` + testMD5 + `"},
		"a-1-0.tar.bz2": {"name": "a", "version": "1", "build": "0", "md5": "` + ff + `"},
		"b-1-0.tar.bz2": {"name": "b", "version": "1", "build": "0"},
		"b-1-0.tar.bz2": {"name": "b", "version": 1, "build": "0"},
		"c-1-0.tar.bz2": {"name": "c", "version": "1", "build": "0", "version": null}
	}}`
	recs := loadJSON(t, doc, "noarch")
	if got := fns(recs); !reflect.DeepEqual(got, []string{"a-1-0.tar.bz2"}) || recs[0].MD5 != ff {
		t.Errorf("got %q", got)
	}
	for _, key := range []string{"packages", "packages.conda", "removed", "info"} {
		doc := `{"` + key + `": null, "` + key + `": null}`
		if _, err := LoadRepodataJSON(strings.NewReader(doc), cfChannel, "noarch"); err == nil || !strings.Contains(err.Error(), "repeated") {
			t.Errorf("repeated %q: err = %v", key, err)
		}
	}
}

// N2: a file name in the wrong section is dropped without touching the
// record of the same name in the right section.
func TestRepodataJSONMisplacedExtension(t *testing.T) {
	doc := `{"packages": {"a-1-0.conda": {"name": "a", "version": "1", "build": "0"}},
		"packages.conda": {"a-1-0.conda": {"name": "a", "version": "1", "build": "0", "md5": "` + testMD5 + `"}}}`
	recs := loadJSON(t, doc, "noarch")
	if len(recs) != 1 || recs[0].Fn != "a-1-0.conda" || recs[0].MD5 != testMD5 {
		t.Errorf("got %+v", recs)
	}
}
