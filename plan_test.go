package goomba

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/spec"
)

// loadRecords reads a JSON list of repodata records, as in rattler's
// topological sort tests.
func loadRecords(t testing.TB, name string) []*record.PackageRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "topological-sort", name+".json"))
	must(t, err)
	var raw []struct {
		Name, Version, Build, Subdir string
		BuildNumber                  uint64 `json:"build_number"`
		Noarch                       json.RawMessage
		Depends, Constrains          []string
	}
	must(t, json.Unmarshal(data, &raw))
	var out []*record.PackageRecord
	for _, r := range raw {
		rec := &record.PackageRecord{Name: r.Name, Version: r.Version, Build: r.Build, Subdir: r.Subdir,
			BuildNumber: r.BuildNumber, Depends: r.Depends, Constrains: r.Constrains}
		var s string
		var b bool
		switch {
		case json.Unmarshal(r.Noarch, &s) == nil:
			rec.Noarch = s
		case json.Unmarshal(r.Noarch, &b) == nil && b:
			rec.Noarch = "generic"
		}
		out = append(out, rec)
	}
	return out
}

// mustPlan sorts recs by name and plans them.
func mustPlan(t testing.TB, recs []*record.PackageRecord) ([]*record.PackageRecord, *installPlan) {
	t.Helper()
	recs = slices.Clone(recs)
	slices.SortFunc(recs, func(a, b *record.PackageRecord) int { return cmp.Compare(a.Name, b.Name) })
	sc, err := scanSpecs(recs)
	must(t, err)
	p, err := planInstall(recs, sc)
	must(t, err)
	return recs, p
}

// linkOrder flattens the plan's layers into package names.
func linkOrder(recs []*record.PackageRecord, p *installPlan) []string {
	var out []string
	for _, l := range p.layers {
		for _, i := range l {
			out = append(out, recs[i].Name)
		}
	}
	return out
}

// checkPlan is rattler's sanity_check_topological_sort plus
// simulate_install: every package appears once, and every dependency on a
// locked package links first unless the plan moved that edge, which must
// lie on a dependency cycle.
func checkPlan(t *testing.T, recs []*record.PackageRecord, p *installPlan) {
	t.Helper()
	order := linkOrder(recs, p)
	pos := map[string]int{}
	for i, n := range order {
		if _, dup := pos[n]; dup {
			t.Fatalf("%s linked twice", n)
		}
		pos[n] = i
	}
	if len(order) != len(recs) {
		t.Fatalf("%d packages planned, want %d", len(order), len(recs))
	}
	moved := map[[2]string]bool{}
	for _, e := range p.moved {
		moved[e] = true
		if !slices.Contains(p.graph.DescendentsAcross([]int{dimLink, dimCycle}, e[1]), e[0]) {
			t.Errorf("moved edge %v does not lie on a cycle", e)
		}
	}
	for _, r := range recs {
		for _, d := range r.Depends {
			ms, err := spec.ParseMatchSpec(d)
			must(t, err)
			dep := ms.Name.String()
			if _, ok := pos[dep]; !ok || dep == r.Name || moved[[2]string{dep, r.Name}] {
				continue
			}
			if pos[dep] > pos[r.Name] {
				t.Errorf("%s links before its dependency %s", r.Name, dep)
			}
		}
	}
	// Layers hold no edge inside them.
	for _, l := range p.layers {
		for _, i := range l {
			for _, j := range l {
				if slices.Contains(p.graph.Descendents(dimLink, recs[i].Name), recs[j].Name) {
					t.Errorf("layer holds %s and its dependent %s", recs[i].Name, recs[j].Name)
				}
			}
		}
	}
}

// Cases from rattler crates/rattler_conda_types/src/repo_data/
// topological_sort.rs (test_get_graph_roots, test_topological_sort,
// test_topological_sort_disconnected_cycle), data in
// testdata/topological-sort. rattler's circular-dependency lists name the
// edges its depth-first search breaks; goomba breaks cycles of platform
// packages by name order instead, so the cases check goomba's own moved
// edges (wantMoved) and that each lies on a cycle. Where a noarch package is
// on the cycle (rootless_graph, big_resolution) goomba moves the edge
// rattler breaks.
func TestPlanRattlerCases(t *testing.T) {
	for _, tc := range []struct {
		data      string
		roots     []string // packages nothing depends on
		last      string   // rattler's expected last package
		wantMoved [][2]string
	}{
		{"python", []string{"python"}, "python", [][2]string{{"llvm-openmp", "_openmp_mutex"}}},
		{"numpy", []string{"numpy"}, "numpy", [][2]string{{"llvm-openmp", "_openmp_mutex"}}},
		{"two_roots", []string{"4ti2", "micromamba"}, "4ti2", [][2]string{{"llvm-openmp", "_openmp_mutex"}}},
		{"rootless_graph", nil, "pip", [][2]string{{"pip", "python"}}},
		{"python_pip", []string{"pip"}, "pip", [][2]string{{"llvm-openmp", "_openmp_mutex"}}},
		{"big_resolution", nil, "panel", nil},
		{"disconnected_cycle", []string{"root-pkg"}, "", [][2]string{{"cycle-b", "cycle-a"}}},
	} {
		t.Run(tc.data, func(t *testing.T) {
			recs, p := mustPlan(t, loadRecords(t, tc.data))
			checkPlan(t, recs, p)
			if tc.last != "" {
				last := p.layers[len(p.layers)-1]
				if len(last) != 1 || recs[last[0]].Name != tc.last {
					t.Errorf("last layer %v, want [%s]", names(recs, last), tc.last)
				}
			}
			if tc.roots != nil {
				var roots []string
				for _, n := range p.graph.Leaves(dimLink) {
					if slices.Contains(p.graph.Leaves(dimCycle), n) {
						roots = append(roots, n)
					}
				}
				if !slices.Equal(roots, tc.roots) {
					t.Errorf("roots %v, want %v", roots, tc.roots)
				}
			}
			if tc.wantMoved != nil && !slices.Equal(p.moved, tc.wantMoved) {
				t.Errorf("moved %v, want %v", p.moved, tc.wantMoved)
			}
		})
	}
	// big_resolution: holoviews depends on panel and panel on holoviews,
	// both noarch; rattler drops holoviews' dependency on panel.
	recs, p := mustPlan(t, loadRecords(t, "big_resolution"))
	if !slices.Contains(p.moved, [2]string{"panel", "holoviews"}) {
		t.Errorf("big_resolution moved %v, want panel -> holoviews among them", p.moved)
	}
	order := linkOrder(recs, p)
	if slices.Index(order, "holoviews") > slices.Index(order, "panel") {
		t.Error("big_resolution: panel must link after holoviews")
	}
}

func names(recs []*record.PackageRecord, idx []int) []string {
	var out []string
	for _, i := range idx {
		out = append(out, recs[i].Name)
	}
	return out
}

func rec(name, noarch string, depends ...string) *record.PackageRecord {
	return &record.PackageRecord{Name: name, Version: "1", Build: "0", Noarch: noarch, Depends: depends}
}

func TestPlanCycles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recs      []*record.PackageRecord
		wantOrder [][]string // layers
		wantMoved [][2]string
	}{
		{
			name:      "python <-> pip",
			recs:      []*record.PackageRecord{rec("python", "", "pip", "openssl"), rec("pip", "python", "python >=3.8"), rec("openssl", "")},
			wantOrder: [][]string{{"openssl"}, {"python"}, {"pip"}},
			wantMoved: [][2]string{{"pip", "python"}},
		},
		{
			name: "platform package between two noarch ones",
			// a (platform) -> n1 -> n2 -> a, and n2 -> n1
			recs:      []*record.PackageRecord{rec("a", "", "n1"), rec("n1", "python", "n2"), rec("n2", "python", "a", "n1")},
			wantOrder: [][]string{{"a", "n1"}, {"n2"}},
			wantMoved: [][2]string{{"n1", "a"}, {"n2", "n1"}},
		},
		{
			name:      "noarch cycle",
			recs:      []*record.PackageRecord{rec("sphinx", "python", "sphinxcontrib-applehelp"), rec("sphinxcontrib-applehelp", "python", "sphinx >=5")},
			wantOrder: [][]string{{"sphinx"}, {"sphinxcontrib-applehelp"}},
			wantMoved: [][2]string{{"sphinxcontrib-applehelp", "sphinx"}},
		},
		{
			name:      "platform cycle",
			recs:      []*record.PackageRecord{rec("c", "", "a"), rec("a", "", "b"), rec("b", "", "c")},
			wantOrder: [][]string{{"a"}, {"c"}, {"b"}},
			wantMoved: [][2]string{{"b", "a"}},
		},
		{
			name:      "self dependency, virtual, missing and glob names",
			recs:      []*record.PackageRecord{rec("a", "", "a >=1", "__glibc >=2.17", "missing", "b*", "B")},
			wantOrder: [][]string{{"a"}},
		},
		{
			name:      "case-insensitive names",
			recs:      []*record.PackageRecord{rec("a", "", "B >=1"), rec("b", "")},
			wantOrder: [][]string{{"b"}, {"a"}},
		},
		{
			name:      "diamond",
			recs:      []*record.PackageRecord{rec("top", "", "left", "right"), rec("left", "", "base"), rec("right", "", "base"), rec("base", "")},
			wantOrder: [][]string{{"base"}, {"left", "right"}, {"top"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs, p := mustPlan(t, tc.recs)
			checkPlan(t, recs, p)
			var got [][]string
			for _, l := range p.layers {
				got = append(got, names(recs, l))
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantOrder) {
				t.Errorf("layers %v, want %v", got, tc.wantOrder)
			}
			if fmt.Sprint(p.moved) != fmt.Sprint(tc.wantMoved) && (len(p.moved) > 0 || len(tc.wantMoved) > 0) {
				t.Errorf("moved %v, want %v", p.moved, tc.wantMoved)
			}
		})
	}
}

// TestPlanOrderIndependent shuffles the packages and their depends: the plan
// must not change.
func TestPlanOrderIndependent(t *testing.T) {
	for _, data := range []string{"python_pip", "numpy", "big_resolution", "disconnected_cycle"} {
		base := loadRecords(t, data)
		recs, p := mustPlan(t, base)
		want := fmt.Sprint(linkOrder(recs, p), p.moved, p.graph.Layers(dimConstrains))
		rng := rand.New(rand.NewPCG(1, 2))
		for range 5 {
			shuffled := make([]*record.PackageRecord, len(base))
			for i, r := range base {
				c := *r
				c.Depends = slices.Clone(r.Depends)
				rng.Shuffle(len(c.Depends), func(i, j int) { c.Depends[i], c.Depends[j] = c.Depends[j], c.Depends[i] })
				shuffled[i] = &c
			}
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			recs, p := mustPlan(t, shuffled)
			if got := fmt.Sprint(linkOrder(recs, p), p.moved, p.graph.Layers(dimConstrains)); got != want {
				t.Fatalf("%s: plan depends on input order", data)
			}
		}
	}
}

// TestPlanConstrains: constrains edges go to their own dimension, from the
// constrained package to the constraining one; a cycle of constraints
// (python and python_abi constrain each other) keeps the edge from the
// earlier name.
func TestPlanConstrains(t *testing.T) {
	python := rec("python", "")
	python.Constrains = []string{"python_abi 3.11.* *_cp311", "missing"}
	abi := rec("python_abi", "")
	abi.Constrains = []string{"python 3.11.* *_cpython"}
	numpy := rec("numpy", "", "python", "python_abi")
	numpy.Constrains = []string{"numpy-base <0a0", "__cuda >=12"}
	recs, p := mustPlan(t, []*record.PackageRecord{python, abi, numpy})
	checkPlan(t, recs, p)
	if got := p.graph.Descendents(dimConstrains, "python"); !slices.Equal(got, []string{"python_abi"}) {
		t.Errorf("python constrains-descendents %v", got)
	}
	if got := p.graph.Descendents(dimConstrains, "python_abi"); got != nil {
		t.Errorf("python_abi constrains-descendents %v", got)
	}
	if got := p.graph.Descendents(dimLink, "python"); !slices.Equal(got, []string{"numpy"}) {
		t.Errorf("python link-descendents %v", got)
	}
}

func TestPlanBadSpec(t *testing.T) {
	r := rec("a", "", "b ((")
	_, err := scanSpecs([]*record.PackageRecord{r})
	if err == nil || !strings.Contains(err.Error(), `a-1-0: depends "b (("`) {
		t.Errorf("err = %v", err)
	}
	r = rec("a", "")
	r.Constrains = []string{"=="}
	if _, err := scanSpecs([]*record.PackageRecord{r}); err == nil {
		t.Error("bad constrains accepted")
	}
}

func TestSCCs(t *testing.T) {
	// 0 -> 1 -> 2 -> 0, 2 -> 3, 3 -> 4 -> 3, 5 alone
	comp := sccs(6, [][2]int32{{0, 1}, {1, 2}, {2, 0}, {2, 3}, {3, 4}, {4, 3}})
	if comp[0] != comp[1] || comp[1] != comp[2] || comp[3] != comp[4] || comp[0] == comp[3] || comp[5] == comp[0] || comp[5] == comp[3] {
		t.Errorf("sccs = %v", comp)
	}
	// A long chain must not exhaust the stack.
	n := 200000
	edges := make([][2]int32, n-1)
	for i := range edges {
		edges[i] = [2]int32{int32(i), int32(i + 1)}
	}
	edges = append(edges, [2]int32{int32(n - 1), 0})
	comp = sccs(n, edges)
	for _, c := range comp {
		if c != comp[0] {
			t.Fatal("the cycle is not one component")
		}
	}
}
