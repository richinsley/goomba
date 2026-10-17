package dag

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// condaRecord is the part of a conda-meta/*.json record the graph needs.
type condaRecord struct {
	Name       string   `json:"name"`
	Depends    []string `json:"depends"`
	Constrains []string `json:"constrains"`
}

// specName is the package name a match spec starts with, e.g. "libgcc" for
// "libgcc >=14".
func specName(spec string) string {
	if i := strings.IndexAny(spec, " =<>!~[;"); i >= 0 {
		return spec[:i]
	}
	return spec
}

// condaGraph builds the three-dimension package graph of a conda-meta
// directory, records in file-name order: dimension 0 gets an edge from each
// installed dependency to its dependent unless that would close a cycle, in
// which case the edge goes to dimension 1; dimension 2 gets an edge from
// each package to every installed package it constrains. It returns the
// edges moved to dimension 1 as the CycleErrors that moved them.
func condaGraph(t *testing.T, dir string) (*Graph[string], []*CycleError[string]) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	var records []condaRecord
	g := New[string](3)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r condaRecord
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !g.AddNode(r.Name) {
			t.Fatalf("%s: duplicate package %q", f, r.Name)
		}
		records = append(records, r)
	}
	var moved []*CycleError[string]
	for _, r := range records {
		for _, spec := range r.Depends {
			dep := specName(spec)
			if !g.Has(dep) {
				continue // virtual (__glibc) or not installed
			}
			err := g.Connect(0, dep, r.Name)
			var ce *CycleError[string]
			switch {
			case errors.As(err, &ce):
				moved = append(moved, ce)
				if err := g.Connect(1, dep, r.Name); err != nil {
					t.Fatalf("dimension 1: %v", err)
				}
			case err != nil:
				t.Fatal(err)
			}
		}
		for _, spec := range r.Constrains {
			if c := specName(spec); g.Has(c) {
				if err := g.Connect(2, r.Name, c); err != nil {
					t.Fatalf("dimension 2: %v", err)
				}
			}
		}
	}
	return g, moved
}

// TestCondaBase311 layers poppi's 28-package base-3.11 environment, made by
// micromamba. testdata/base-3.11 holds its conda-meta records without their
// file lists; the environment itself, $GOOMBA_REF_ROOT/envs/base-3.11, is
// checked too when GOOMBA_REF_ROOT is set.
func TestCondaBase311(t *testing.T) {
	t.Run("testdata", func(t *testing.T) {
		checkBase311(t, filepath.Join("testdata", "base-3.11", "conda-meta"))
	})
	t.Run("GOOMBA_REF_ROOT", func(t *testing.T) {
		root := os.Getenv("GOOMBA_REF_ROOT")
		if root == "" {
			t.Skip("set GOOMBA_REF_ROOT to check the micromamba-built environment")
		}
		checkBase311(t, filepath.Join(root, "envs", "base-3.11", "conda-meta"))
	})
}

func checkBase311(t *testing.T, dir string) {
	g, moved := condaGraph(t, dir)
	if g.Len() != 28 {
		t.Fatalf("%d packages, want 28", g.Len())
	}

	// python depends on pip and pip on python. pip's record comes first, so
	// python -> pip lands in dimension 0, and pip -> python (python
	// requiring pip) closes the cycle and moves to dimension 1.
	if len(moved) != 1 {
		t.Fatalf("moved %d edges to dimension 1, want 1: %v", len(moved), moved)
	}
	ce := moved[0]
	if ce.Dim != 0 || ce.From != "pip" || ce.To != "python" || !slices.Equal(ce.Path, []string{"python", "pip"}) {
		t.Fatalf("moved edge %+v, want pip -> python closing python -> pip", ce)
	}
	if want := `dag: connect "pip" -> "python" in dimension 0: edge would close a cycle: existing path "python" -> "pip"`; ce.Error() != want {
		t.Errorf("Error() = %s\nwant      %s", ce.Error(), want)
	}

	layers := g.Layers(0)
	want := [][]string{
		{"ca-certificates", "libgomp", "libzlib", "tzdata"},
		{"_openmp_mutex", "zstd"},
		{"ld_impl_linux-64", "libgcc"},
		{"bzip2", "libexpat", "libffi", "libgcc-ng", "liblzma", "libnsl", "libstdcxx", "libuuid", "ncurses", "openssl", "tk"},
		{"icu", "libxcrypt", "readline"},
		{"libsqlite"},
		{"python"},
		{"packaging", "setuptools"},
		{"wheel"},
		{"pip"},
	}
	if len(layers) != 10 || len(layers[3]) != 11 {
		t.Errorf("%d layers, layer 3 has %d packages; want 10 and 11", len(layers), len(layers[3]))
	}
	widest := 0
	for i, l := range layers {
		if len(l) > len(layers[widest]) {
			widest = i
		}
	}
	if widest != 3 {
		t.Errorf("widest layer is %d, want 3", widest)
	}
	if !equal2(layers, want) {
		t.Errorf("Layers(0) =\n%q\nwant\n%q", layers, want)
	}

	// Every requirement edge, in either dimension, runs between layers in
	// order; except the one moved edge, every one is in dimension 0.
	for _, k := range g.Nodes() {
		for _, c := range g.index[k].out[0] {
			if g.Ordinal(0, c.key) <= g.Ordinal(0, k) {
				t.Errorf("edge %s -> %s does not go down a layer", k, c.key)
			}
		}
	}
	wantKeys(t, "Layers(1)[1]", g.Layers(1)[1], "python")
	wantKeys(t, "Descendents(1, pip)", g.Descendents(1, "pip"), "python")

	// Dimension 2: libgcc constrains libgcc-ng and libgomp; no other
	// constrained package is installed (python_abi is not, here).
	wantKeys(t, "Descendents(2, libgcc)", g.Descendents(2, "libgcc"), "libgcc-ng", "libgomp")
	wantKeys(t, "packages constraining others", slices.DeleteFunc(g.Nodes(), func(k string) bool { return len(g.Descendents(2, k)) == 0 }), "libgcc")
	wantInt(t, "MaxOrdinal(2)", g.MaxOrdinal(2), 1)

	// Impact of libgomp across all three dimensions: everything that needs
	// it at link time, plus python's requirement on pip; libgomp itself is
	// left out although libgcc constrains it.
	wantKeys(t, "DescendentsAcross(libgomp)", g.DescendentsAcross([]int{0, 1, 2}, "libgomp"),
		"_openmp_mutex", "libgcc",
		"bzip2", "libexpat", "libffi", "libgcc-ng", "liblzma", "libnsl", "libstdcxx", "libuuid", "ncurses", "openssl", "tk",
		"icu", "libxcrypt", "readline", "libsqlite", "python", "packaging", "setuptools", "wheel", "pip")
	wantKeys(t, "Ascendents(0, wheel)", g.Ascendents(0, "wheel"),
		"ca-certificates", "libgomp", "libzlib", "tzdata", "_openmp_mutex", "zstd", "ld_impl_linux-64", "libgcc",
		"bzip2", "libexpat", "libffi", "libgcc-ng", "liblzma", "libnsl", "libstdcxx", "libuuid", "ncurses", "openssl", "tk",
		"icu", "libxcrypt", "readline", "libsqlite", "python", "packaging")
	wantInt(t, "subgraphs in dimension 0", len(g.Subgraphs(0)), 1)
	wantInt(t, "subgraphs in dimension 2", len(g.Subgraphs(2)), 26)
	checkGraph(t, g, refFrom(g), "absent")
}
