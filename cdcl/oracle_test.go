package cdcl_test

// Differential test against resolvo itself. randomProblem generates
// BundleBox problems from fixed seeds; testdata/oracle/expected.txt holds
// what resolvo v0.12.2 (run in its default synchronous mode) answers for
// each of them: the solution in decision order, or the conflict explanation
// and its Graphviz rendering, plus the clause and deferred-requirement
// counts. See testdata/oracle/README.md for how to regenerate it.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/cdcl/internal/bundlebox"
)

var (
	oracleIn  = flag.String("oracle-in", "", "with -oracle-out: solve the problems of this JSON file (oracle format)")
	oracleOut = flag.String("oracle-out", "", "with -oracle-in: write cdcl's answers in the oracle's output format to this file")
)

// TestOracleRun solves an arbitrary problem file for differential testing:
//
//	go test ./cdcl -run TestOracleRun -args -oracle-in=p.json -oracle-out=out.txt
func TestOracleRun(t *testing.T) {
	if *oracleIn == "" || *oracleOut == "" {
		t.Skip("set -oracle-in and -oracle-out")
	}
	data, err := os.ReadFile(*oracleIn)
	if err != nil {
		t.Fatal(err)
	}
	var problems []oracleProblem
	if err := json.Unmarshal(data, &problems); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, op := range problems {
		if len(op.Soft) > 0 {
			t.Fatalf("problem %d has soft requirements, which cdcl does not support", i)
		}
		fmt.Fprintf(&b, "=== %d\n%s", i, solveOracleFormat(t, op))
	}
	if err := os.WriteFile(*oracleOut, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

var updateOracleProblems = flag.String("write-oracle-problems", "", "write the oracle corpus problems as JSON to this file and skip the comparison")

// The corpus has oracleSmallProblems small problems followed by larger,
// mostly satisfiable ones.
const (
	oracleSmallProblems = 2500
	oracleCorpusSize    = 3000
)

type oraclePackage struct {
	Name       string   `json:"name"`
	Version    uint32   `json:"version"`
	Deps       []string `json:"deps"`
	Constrains []string `json:"constrains"`
	Unknown    bool     `json:"unknown"`
}

type oracleProblem struct {
	Packages      []oraclePackage `json:"packages"`
	Favored       [][]any         `json:"favored"`
	Locked        [][]any         `json:"locked"`
	Excluded      [][]any         `json:"excluded"`
	AllowMultiple []string        `json:"allow_multiple"`
	HintAll       bool            `json:"hint_all"`
	Requirements  []string        `json:"requirements"`
	Constraints   []string        `json:"constraints"`
	Soft          [][]any         `json:"soft"` // always empty: cdcl has no soft requirements
	// Activity, when set, holds the solver's activity add and decay
	// parameters.
	Activity []float32 `json:"activity,omitempty"`
}

type problemGen struct {
	r     *rand.Rand
	names []string
	large bool
	// condPct is the percentage of requirements that are conditional.
	condPct int
}

func (g *problemGen) chance(pct int) bool { return g.r.IntN(100) < pct }

func (g *problemGen) name() string {
	if !g.large && g.chance(4) {
		return "missing"
	}
	return g.names[g.r.IntN(len(g.names))]
}

func (g *problemGen) rangeSpec(name string) string {
	maxV := 7
	if g.large {
		maxV = 12
	}
	switch {
	case g.chance(45):
		return name
	case g.chance(30) && !g.large:
		return fmt.Sprintf("%s %d", name, g.r.IntN(maxV))
	default:
		lo := g.r.IntN(maxV)
		return fmt.Sprintf("%s %d..%d", name, lo, lo+1+g.r.IntN(maxV/2+2))
	}
}

// requirement returns a dependency spec, possibly a union and possibly
// conditional. exclude is the name to avoid (a package's own name).
func (g *problemGen) requirement(conditional bool) string {
	spec := g.rangeSpec(g.name())
	if g.chance(12) {
		for n := 1 + g.r.IntN(2); n > 0; n-- {
			spec += " | " + g.rangeSpec(g.name())
		}
	}
	if conditional && g.chance(g.condPct) {
		cond := g.rangeSpec(g.name())
		for n := g.r.IntN(3); n > 0; n-- {
			op := " and "
			if g.chance(50) {
				op = " or "
			}
			cond += op + g.rangeSpec(g.name())
		}
		spec += "; if " + cond
	}
	return spec
}

// randomProblem generates the corpus problem with the given seed.
func randomProblem(seed uint64) oracleProblem {
	return randomProblemWith(seed, seed%1_000_000 >= oracleSmallProblems, 12)
}

// randomProblemWith generates a random problem; condPct is the percentage
// of requirements that are conditional.
func randomProblemWith(seed uint64, large bool, condPct int) oracleProblem {
	g := &problemGen{r: rand.New(rand.NewPCG(seed, 0x5eed)), large: large, condPct: condPct}
	n := 2 + g.r.IntN(8)
	maxV, perVersion, depMax := 7, 4, 4
	if g.large {
		n, maxV, perVersion, depMax = 10+g.r.IntN(30), 12, 10, 5
	}
	for i := 0; i < n; i++ {
		g.names = append(g.names, fmt.Sprintf("p%d", i))
	}
	p := oracleProblem{
		Favored: [][]any{}, Locked: [][]any{}, Excluded: [][]any{}, Soft: [][]any{},
		AllowMultiple: []string{}, Requirements: []string{}, Constraints: []string{},
	}
	type nv struct {
		name    string
		version uint32
	}
	var all []nv
	for _, name := range g.names {
		perm := g.r.Perm(maxV)
		for _, v := range perm[:1+g.r.IntN(perVersion)] {
			pkg := oraclePackage{Name: name, Version: uint32(v), Deps: []string{}, Constrains: []string{}}
			for d := g.r.IntN(depMax); d > 0; d-- {
				pkg.Deps = append(pkg.Deps, g.requirement(true))
			}
			for c := g.r.IntN(3); c > 0; c-- {
				if !g.chance(40) {
					continue
				}
				target := g.name()
				if target == name {
					continue
				}
				pkg.Constrains = append(pkg.Constrains, g.rangeSpec(target))
			}
			pkg.Unknown = g.chance(4)
			p.Packages = append(p.Packages, pkg)
			all = append(all, nv{name, uint32(v)})
		}
	}
	// Shuffle the insertion order of package versions.
	g.r.Shuffle(len(p.Packages), func(i, j int) { p.Packages[i], p.Packages[j] = p.Packages[j], p.Packages[i] })

	for _, name := range g.names {
		if g.chance(8) {
			p.Favored = append(p.Favored, []any{name, g.r.IntN(maxV)})
		}
		if g.chance(6) {
			p.Locked = append(p.Locked, []any{name, g.r.IntN(maxV)})
		}
		if g.chance(6) {
			p.AllowMultiple = append(p.AllowMultiple, name)
		}
	}
	for _, x := range all {
		if g.chance(5) && !(g.large && g.chance(60)) {
			reason := "it is externally excluded"
			if g.chance(50) {
				reason = "it is retracted"
			}
			p.Excluded = append(p.Excluded, []any{x.name, x.version, reason})
		}
	}
	p.HintAll = g.chance(25)
	for r := 1 + g.r.IntN(3); r > 0; r-- {
		p.Requirements = append(p.Requirements, g.requirement(true))
	}
	if g.chance(15) {
		for c := 1 + g.r.IntN(2); c > 0; c-- {
			p.Constraints = append(p.Constraints, g.rangeSpec(g.name()))
		}
	}
	return p
}

func asUint32(v any) uint32 {
	switch x := v.(type) {
	case int:
		return uint32(x)
	case uint32:
		return x
	case float64:
		return uint32(x)
	}
	panic(fmt.Sprintf("unexpected %T", v))
}

// buildProblem builds the BundleBox provider and problem.
func buildProblem(op oracleProblem) (*bundlebox.Provider, cdcl.Problem) {
	p := bundlebox.New()
	for _, pkg := range op.Packages {
		pack := bundlebox.NewPack(pkg.Version)
		if pkg.Unknown {
			pack = pack.WithUnknownDeps()
		}
		p.AddPackage(pkg.Name, pack, pkg.Deps, pkg.Constrains)
	}
	for _, f := range op.Favored {
		p.SetFavored(f[0].(string), asUint32(f[1]))
	}
	for _, f := range op.Locked {
		p.SetLocked(f[0].(string), asUint32(f[1]))
	}
	for _, f := range op.Excluded {
		p.Exclude(f[0].(string), asUint32(f[1]), f[2].(string))
	}
	for _, name := range op.AllowMultiple {
		p.SetAllowMultiple(name)
	}
	p.HintDependenciesAvailable = op.HintAll
	problem := cdcl.Problem{
		Requirements: p.Requirements(op.Requirements),
		Constraints:  p.VersionSets(op.Constraints),
	}
	return p, problem
}

// solveOracleFormat solves a problem and renders the result in the oracle's
// output format.
func solveOracleFormat(t *testing.T, op oracleProblem) string {
	p, problem := buildProblem(op)
	solver := newSolver(p)
	if len(op.Activity) == 2 {
		solver.SetActivityParams(op.Activity[0], op.Activity[1])
	}
	var b strings.Builder
	sol, err := solver.Solve(context.Background(), problem)
	var conflict *cdcl.Conflict
	switch {
	case err == nil:
		b.WriteString("SOLUTION\n")
		for _, s := range sol {
			b.WriteString(p.DisplaySolvable(s) + "\n")
		}
	case errors.As(err, &conflict):
		b.WriteString("UNSAT\n")
		b.WriteString(conflict.Display(p))
		var dot strings.Builder
		graph, err := conflict.Graph()
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.Graphviz(&dot, p, true); err != nil {
			t.Fatal(err)
		}
		b.WriteString("GRAPHVIZ " + dot.String() + "\n")
	default:
		t.Fatalf("solve: %v", err)
	}
	fmt.Fprintf(&b, "CLAUSES %d\n", solver.ClauseCount())
	fmt.Fprintf(&b, "DEFERRED %d\n", solver.DeferredRequirementsCount())
	return b.String()
}

// activityProblem is a problem of the activity corpus: a small or large
// random problem solved with non-standard activity parameters, which
// disable the decide queue's activity shortcuts.
func activityProblem(i int) oracleProblem {
	params := [][]float32{{0, 0.95}, {1, -0.5}, {-1, 0.5}, {2, 1}, {0.5, 0.5}}
	op := randomProblemWith(uint64(2_000_000+i), i%3 == 2, 12)
	op.Activity = params[i%len(params)]
	return op
}

const activityCorpusSize = 500

var updateActivityProblems = flag.String("write-activity-problems", "", "write the activity corpus problems as JSON to this file and skip the comparison")

// TestOracleActivityCorpus compares with resolvo for non-standard activity
// parameters (testdata/oracle/expected_activity.txt).
func TestOracleActivityCorpus(t *testing.T) {
	if *updateActivityProblems != "" {
		problems := make([]oracleProblem, activityCorpusSize)
		for i := range problems {
			problems[i] = activityProblem(i)
		}
		data, err := json.Marshal(problems)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(*updateActivityProblems, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("wrote the activity problems")
	}
	compareCorpus(t, "expected_activity.txt", "deviations_activity.txt", activityCorpusSize, activityProblem)
}

// readSections reads an oracle-format file of testdata/oracle into its
// entries by problem index.
func readSections(t *testing.T, name string) map[int]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "oracle", name))
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]string{}
	for _, sec := range strings.Split(string(data), "=== ")[1:] {
		header, body, _ := strings.Cut(sec, "\n")
		var idx int
		if _, err := fmt.Sscan(header, &idx); err != nil {
			t.Fatalf("%s: bad header %q", name, header)
		}
		out[idx] = body
	}
	return out
}

// compareCorpus solves problems 0..n-1 and requires each answer to equal
// resolvo's, or, for the problems listed in the deviations file, cdcl's
// recorded answer (classified in deviations_classes.txt).
func compareCorpus(t *testing.T, expectedFile, deviationsFile string, n int, problem func(int) oracleProblem) {
	t.Helper()
	expected := readSections(t, expectedFile)
	deviations := readSections(t, deviationsFile)
	if len(expected) != n {
		t.Fatalf("%s has %d entries, want %d", expectedFile, len(expected), n)
	}
	failures := 0
	for i := 0; i < n; i++ {
		want, ok := expected[i]
		if !ok {
			t.Fatalf("%s: no entry %d", expectedFile, i)
		}
		if dev, ok := deviations[i]; ok {
			if dev == want {
				t.Errorf("%s: entry %d does not differ from resolvo", deviationsFile, i)
			}
			want = dev
		}
		if got := solveOracleFormat(t, problem(i)); got != want {
			failures++
			if failures <= 5 {
				js, _ := json.Marshal(problem(i))
				t.Errorf("problem %d differs\nproblem: %s\n--- got:\n%s--- want:\n%s", i, js, got, want)
			}
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d problems differ", failures, n)
	}
	t.Logf("%d problems match resolvo, %d match cdcl's recorded deviations", n-len(deviations), len(deviations))
}

// TestOracleDeviationClasses checks that deviations_classes.txt classifies
// every recorded deviation, and only with the classes README.md allows:
// differences in order, counts or explanation only, different valid
// solutions, explanations that show a repeated candidate once, and problems
// where resolvo panics or answers invalidly.
func TestOracleDeviationClasses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "oracle", "deviations_classes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{
		"same solution, order or counts differ",
		"both unsat, explanation or counts differ",
		"both valid, different solutions",
		"explanation shows repeated candidates once",
		"resolvo panics",
		"resolvo false unsat",
		"resolvo invalid: ",
	}
	classified := map[string]map[int]bool{"corpus": {}, "activity": {}}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		corpus, rest, _ := strings.Cut(line, " ")
		idxText, class, _ := strings.Cut(rest, " ")
		var idx int
		if _, err := fmt.Sscan(idxText, &idx); err != nil || classified[corpus] == nil {
			t.Fatalf("bad line %q", line)
		}
		ok := false
		for _, a := range allowed {
			if class == a || strings.HasSuffix(a, ": ") && strings.HasPrefix(class, a) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s %d: class %q is not an allowed deviation", corpus, idx, class)
		}
		classified[corpus][idx] = true
	}
	for corpus, file := range map[string]string{"corpus": "deviations.txt", "activity": "deviations_activity.txt"} {
		devs := readSections(t, file)
		for idx := range devs {
			if !classified[corpus][idx] {
				t.Errorf("%s: deviation %d is not classified", file, idx)
			}
		}
		for idx := range classified[corpus] {
			if _, ok := devs[idx]; !ok {
				t.Errorf("deviations_classes.txt: %s %d is not a deviation", corpus, idx)
			}
		}
	}
}

// TestOracleCorpus compares with resolvo on the random corpus
// (testdata/oracle/expected.txt).
func TestOracleCorpus(t *testing.T) {
	if *updateOracleProblems != "" {
		problems := make([]oracleProblem, oracleCorpusSize)
		for i := range problems {
			problems[i] = randomProblem(uint64(i))
		}
		data, err := json.Marshal(problems)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(*updateOracleProblems, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("wrote the oracle problems; run the resolvo oracle to refresh testdata/oracle/expected.txt")
	}
	compareCorpus(t, "expected.txt", "deviations.txt", oracleCorpusSize, func(i int) oracleProblem { return randomProblem(uint64(i)) })
}

// specDNF converts a parsed condition tree to disjunctive normal form.
func specDNF(c *bundlebox.SpecCondition) [][]bundlebox.Spec {
	if c.Requirement != nil {
		return [][]bundlebox.Spec{{*c.Requirement}}
	}
	l, r := specDNF(c.Left), specDNF(c.Right)
	if c.Op == cdcl.Or {
		return append(l, r...)
	}
	var out [][]bundlebox.Spec
	for _, a := range l {
		for _, b := range r {
			out = append(out, append(append([]bundlebox.Spec(nil), a...), b...))
		}
	}
	return out
}

// validateOracleSolution checks a solution against the problem's rules:
// every requirement (a conditional one whenever its condition holds, i.e.
// every conjunct of a disjunct has an installed matching version),
// constraint, lock, exclusion and the one-version-per-package rule.
func validateOracleSolution(op oracleProblem, p *bundlebox.Provider, sol []cdcl.SolvableID) error {
	selected := map[string][]bundlebox.Pack{}
	for _, s := range sol {
		rec := p.Pool.ResolveSolvable(s)
		name := p.DisplayName(rec.Name)
		selected[name] = append(selected[name], rec.Record)
	}
	multi := map[string]bool{}
	for _, n := range op.AllowMultiple {
		multi[n] = true
	}
	anyMatch := func(sp bundlebox.Spec) bool {
		for _, pack := range selected[sp.Name] {
			if sp.Versions.Contains(pack) {
				return true
			}
		}
		return false
	}
	reqOK := func(r string) bool {
		cs, err := bundlebox.ParseConditionalSpec(r)
		if err != nil {
			panic(err)
		}
		if cs.Condition != nil {
			holds := false
			for _, conj := range specDNF(cs.Condition) {
				all := true
				for _, c := range conj {
					if !anyMatch(c) {
						all = false
						break
					}
				}
				if all {
					holds = true
					break
				}
			}
			if !holds {
				return true
			}
		}
		for _, sp := range cs.Specs {
			if anyMatch(sp) {
				return true
			}
		}
		return false
	}
	respects := func(c string) bool {
		sp, err := bundlebox.ParseSpec(c)
		if err != nil {
			panic(err)
		}
		for _, pack := range selected[sp.Name] {
			if !sp.Versions.Contains(pack) {
				return false
			}
		}
		return true
	}
	for name, packs := range selected {
		if len(packs) > 1 && !multi[name] {
			return fmt.Errorf("%s: several versions selected", name)
		}
		for _, pack := range packs {
			var pkg *oraclePackage
			for i := range op.Packages {
				if op.Packages[i].Name == name && op.Packages[i].Version == pack.Version && op.Packages[i].Unknown == pack.UnknownDeps {
					pkg = &op.Packages[i]
				}
			}
			if pkg == nil || pkg.Unknown {
				return fmt.Errorf("%s=%d: not a usable package", name, pack.Version)
			}
			for _, d := range pkg.Deps {
				if !reqOK(d) {
					return fmt.Errorf("%s=%d: requirement %q unsatisfied", name, pack.Version, d)
				}
			}
			for _, c := range pkg.Constrains {
				if !respects(c) {
					return fmt.Errorf("%s=%d: constraint %q violated", name, pack.Version, c)
				}
			}
		}
	}
	for _, e := range op.Excluded {
		for _, pack := range selected[e[0].(string)] {
			if pack == bundlebox.NewPack(asUint32(e[1])) {
				return fmt.Errorf("%s=%d is excluded", e[0], pack.Version)
			}
		}
	}
	for _, l := range op.Locked {
		name, v := l[0].(string), asUint32(l[1])
		lockable := false
		for _, pkg := range op.Packages {
			if pkg.Name == name && pkg.Version == v && !pkg.Unknown {
				lockable = true
			}
		}
		for _, pack := range selected[name] {
			if lockable && pack != bundlebox.NewPack(v) {
				return fmt.Errorf("%s=%d selected but %d is locked", name, pack.Version, v)
			}
		}
	}
	for _, r := range op.Requirements {
		if !reqOK(r) {
			return fmt.Errorf("root requirement %q unsatisfied", r)
		}
	}
	for _, c := range op.Constraints {
		if !respects(c) {
			return fmt.Errorf("root constraint %q violated", c)
		}
	}
	return nil
}

// Every solution cdcl finds for random problems, small and large, satisfies
// the problem, and every unsat answer has a bounded explanation. (Unsat
// answers are checked against brute force in TestPropertyBruteForce.)
func TestRandomProblemSolutionsAreValid(t *testing.T) {
	n := 4000
	if testing.Short() {
		n = 400
	}
	valid := 0
	for i := 0; i < n; i++ {
		seed := uint64(1_000_000 + i)
		op := randomProblemWith(seed, i%2 == 1, 60)
		p, problem := buildProblem(op)
		sol, err := newSolver(p).Solve(context.Background(), problem)
		if err != nil {
			if !errors.Is(err, cdcl.ErrUnsolvable) {
				t.Fatalf("seed %d: %v", seed, err)
			}
			checkExplanation(t, p, err)
			continue
		}
		if err := validateOracleSolution(op, p, sol); err != nil {
			js, _ := json.Marshal(op)
			t.Errorf("seed %d: invalid solution %q: %v\nproblem: %s", seed, transactionToString(p, sol), err, js)
		}
		valid++
	}
	t.Logf("%d solutions validated", valid)
}
