package lock

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func osxLockfile() *Lockfile {
	l := validLockfile()
	l.Platform = "osx-arm64"
	l.Virtual = map[string]string{"__osx": "13.0", "__unix": "0", "__archspec": "arm64"}
	l.Packages[0].Subdir = "osx-arm64"
	l.Packages[0].URL = cf + "/osx-arm64/python-3.11.15-hd63d673_0_cpython.conda"
	return l
}

func TestBundleRoundTrip(t *testing.T) {
	b := &Bundle{Platforms: map[string]*Lockfile{"linux-64": validLockfile(), "osx-arm64": osxLockfile()}}
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	checkGolden(t, "testdata/example.bundle.json", out)
	if strings.Index(out, `"linux-64"`) > strings.Index(out, `"osx-arm64"`) {
		t.Error("platforms not sorted")
	}
	back, err := ReadBundle(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := back.Write(&again); err != nil {
		t.Fatal(err)
	}
	if again.String() != out {
		t.Errorf("bundle round trip is not byte-identical:\n%s", again.String())
	}
	back2, err := ReadBundle(strings.NewReader(again.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, back2) {
		t.Error("read -> write -> read differs")
	}
	// Each platform's lockfile is written exactly as Lockfile.Write would,
	// re-indented by one level.
	if !strings.Contains(out, `"platforms": {`+"\n"+`    "linux-64": {`+"\n"+`      "version": 1,`) {
		t.Errorf("unexpected bundle layout:\n%s", out)
	}
}

func TestBundleRejects(t *testing.T) {
	var buf bytes.Buffer
	if err := (&Bundle{Platforms: map[string]*Lockfile{"linux-64": validLockfile()}}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	good := buf.String()
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"lockfile not bundle", writeString(t, validLockfile())},
		{"no platforms", `{"version": 1, "platforms": {}}`},
		{"missing platforms", `{"version": 1}`},
		{"version 2", strings.Replace(good, `"version": 1,`+"\n  \"platforms\"", `"version": 2,`+"\n  \"platforms\"", 1)},
		{"unknown key", strings.Replace(good, `"platforms": {`, `"x": 1, "platforms": {`, 1)},
		{"key mismatch", strings.Replace(good, `"linux-64": {`, `"osx-64": {`, 1)},
		{"duplicate platform", strings.Replace(good, `"platforms": {`, `"platforms": {"linux-64": {}, `, 1)},
		{"null lockfile", `{"version": 1, "platforms": {"linux-64": null}}`},
		{"invalid lockfile", strings.Replace(good, pythonSHA, "x", 1)},
		{"trailing", good + "[]"},
	}
	for _, c := range cases {
		if c.in == good {
			t.Fatalf("%s: replacement did not apply", c.name)
		}
		if _, err := ReadBundle(strings.NewReader(c.in)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ReadBundle = %v, want ErrInvalid", c.name, err)
		}
	}
	bad := []*Bundle{
		nil,
		{},
		{Platforms: map[string]*Lockfile{"linux-64": nil}},
		{Platforms: map[string]*Lockfile{"osx-arm64": validLockfile()}},
	}
	for i, b := range bad {
		if err := b.Write(&bytes.Buffer{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad bundle %d: Write = %v", i, err)
		}
	}
	if _, err := ReadBundle(strings.NewReader(strings.Replace(good, `"version": 1,`+"\n  \"platforms\"", `"version": 2,`+"\n  \"platforms\"", 1))); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("version 2: %v, want ErrUnsupportedVersion", err)
	}
}
