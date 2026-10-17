package spec

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// TestRealWorldSpecs parses the conda-forge match specs in
// testdata/realworld_specs.txt and checks that each one's String form
// parses back to an equal spec.
func TestRealWorldSpecs(t *testing.T) {
	f, err := os.Open("testdata/realworld_specs.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		if bad, ok := strings.CutPrefix(line, "!"); ok {
			if _, err := ParseMatchSpec(bad); !errors.Is(err, ErrMatchSpec) {
				t.Errorf("ParseMatchSpec(%q) err = %v, want an error", bad, err)
			}
			continue
		}
		m, err := ParseMatchSpec(line)
		if err != nil {
			t.Errorf("ParseMatchSpec(%q): %v", line, err)
			continue
		}
		s := m.String()
		m2, err := ParseMatchSpec(s)
		if err != nil || !m2.Equal(m) || m2.String() != s {
			t.Errorf("%q: String %q does not round-trip (%v)", line, s, err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 4000 {
		t.Fatalf("only %d specs", n)
	}
}

type condaMeta struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Build       string   `json:"build"`
	BuildNumber uint64   `json:"build_number"`
	Depends     []string `json:"depends"`
	Constrains  []string `json:"constrains"`
	Subdir      string   `json:"subdir"`
	MD5         string   `json:"md5"`
	SHA256      string   `json:"sha256"`
	Fn          string   `json:"fn"`
	URL         string   `json:"url"`
	License     string   `json:"license"`
}

// TestCondaMetaSpecs checks a micromamba-built environment
// ($GOOMBA_REF_ROOT/envs/base-3.11): every depends and constrains string
// parses, every installed record matches the specs that pin it, and the
// environment is consistent under this package's semantics: each
// dependency is satisfied by an installed record (virtual packages aside)
// and each constraint holds for installed packages.
func TestCondaMetaSpecs(t *testing.T) {
	root := os.Getenv("GOOMBA_REF_ROOT")
	if root == "" {
		t.Skip("set GOOMBA_REF_ROOT to a micromamba root holding envs/base-3.11")
	}
	files, err := filepath.Glob(filepath.Join(root, "envs", "base-3.11", "conda-meta", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no conda-meta records under GOOMBA_REF_ROOT: %v", err)
	}
	installed := map[string]*record.PackageRecord{}
	var metas []condaMeta
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var m condaMeta
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", filepath.Base(file), err)
		}
		metas = append(metas, m)
		installed[m.Name] = &record.PackageRecord{
			Name: m.Name, Version: m.Version, Build: m.Build, BuildNumber: m.BuildNumber,
			Subdir: m.Subdir, MD5: m.MD5, SHA256: m.SHA256, Fn: m.Fn, URL: m.URL, License: m.License,
		}
	}
	nSpecs := 0
	for _, m := range metas {
		r := installed[m.Name]
		if _, err := ParseVersion(m.Version); err != nil {
			t.Errorf("%s: version: %v", m.Name, err)
		}
		// The record matches the specs that pin it exactly.
		for _, s := range []string{
			m.Name + "==" + m.Version + "=" + m.Build,
			m.Name + " " + m.Version + " " + m.Build,
			m.URL + "#" + m.MD5,
			m.Name + "[md5=" + m.MD5 + ",sha256=" + m.SHA256 + ",build_number=" + r.BuildNumberString() + "]",
		} {
			if !mustMatchSpec(t, s).Matches(r) {
				t.Errorf("%s does not match %q", r, s)
			}
		}
		for _, s := range m.Depends {
			nSpecs++
			spec, err := ParseMatchSpec(s)
			if err != nil {
				t.Errorf("%s depends %q: %v", m.Name, s, err)
				continue
			}
			name := spec.Name.String()
			if strings.HasPrefix(name, "__") {
				continue // virtual package
			}
			if dep, ok := installed[name]; !ok || !spec.Matches(dep) {
				t.Errorf("%s depends %q, not satisfied by %v", m.Name, s, dep)
			}
		}
		for _, s := range m.Constrains {
			nSpecs++
			spec, err := ParseMatchSpec(s)
			if err != nil {
				t.Errorf("%s constrains %q: %v", m.Name, s, err)
				continue
			}
			if dep, ok := installed[spec.Name.String()]; ok && !spec.Matches(dep) {
				t.Errorf("%s constrains %q, violated by %v", m.Name, s, dep)
			}
		}
	}
	t.Logf("%d records, %d depends/constrains", len(metas), nSpecs)
}
