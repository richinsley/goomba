package solve

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/spec"
)

// recordingSource wraps a source and records the names it is asked for and
// the names it is told to prefetch.
type recordingSource struct {
	src       repodata.Source
	mu        sync.Mutex
	asked     []string
	prefetch  [][]string
	err       error
	onRecords func(name string)
}

func (s *recordingSource) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	s.mu.Lock()
	s.asked = append(s.asked, name)
	s.mu.Unlock()
	if s.onRecords != nil {
		s.onRecords(name)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.src.Records(ctx, name)
}

func (s *recordingSource) Prefetch(_ context.Context, names []string) {
	s.mu.Lock()
	s.prefetch = append(s.prefetch, append([]string(nil), names...))
	s.mu.Unlock()
}

func mustSolve(t *testing.T, src repodata.Source, req Request) *Solution {
	t.Helper()
	sol, err := Solve(context.Background(), src, req)
	if err != nil {
		t.Fatalf("solve %q: %v", req.Specs, err)
	}
	return sol
}

func mustUnsolvable(t *testing.T, src repodata.Source, req Request) *Unsolvable {
	t.Helper()
	sol, err := Solve(context.Background(), src, req)
	if err == nil {
		t.Fatalf("solve %q: expected no solution, got %s", req.Specs, formatRecords(sol.Records))
	}
	var u *Unsolvable
	if !errors.As(err, &u) {
		t.Fatalf("solve %q: expected *Unsolvable, got %v", req.Specs, err)
	}
	return u
}

func TestExcludedWithoutSHA256(t *testing.T) {
	foo1 := pkg("foo").version("1.0").rec()
	foo2 := pkg("foo").version("2.0").sha256("").rec()
	foo3 := pkg("foo").version("3.0").sha256("abcd").rec()
	sol := mustSolve(t, repodata.NewMemory([]*record.PackageRecord{foo1, foo2, foo3}), Request{Specs: []string{"foo"}})
	if len(sol.Records) != 1 || sol.Records[0] != foo1 {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	u := mustUnsolvable(t, repodata.NewMemory([]*record.PackageRecord{foo1, foo2, foo3}), Request{Specs: []string{"foo>=2"}})
	want := `The following packages are incompatible
└─ foo >=2 cannot be installed because there are no viable options:
   ├─ foo 3.0 is excluded because malformed sha256: cannot be verified
   └─ foo 2.0 is excluded because no sha256: cannot be verified`
	if normalizeText(u.Explanation) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s", u.Explanation, want)
	}
}

func TestVirtualPackagesComeOnlyFromRequest(t *testing.T) {
	// A channel that lists a virtual package must not be able to supply it.
	src := &recordingSource{src: repodata.NewMemory([]*record.PackageRecord{
		pkg("__cuda").version("99").rec(),
		pkg("gpu").version("1.0").depends("__cuda >=12").rec(),
	})}
	u := mustUnsolvable(t, src, Request{Specs: []string{"gpu"}})
	want := `gpu * cannot be installed because there are no viable options:
└─ gpu 1.0 would require
   └─ __cuda >=12, for which no candidates were found.`
	if normalizeText(u.Explanation) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s", u.Explanation, want)
	}
	for _, n := range src.asked {
		if strings.HasPrefix(n, "__") {
			t.Errorf("the source was asked for virtual package %q", n)
		}
	}

	// With the virtual package on the machine, it is installed (and not
	// part of the solution); a too-old one blocks the package.
	sol := mustSolve(t, src, Request{Specs: []string{"gpu"}, Virtual: []*record.PackageRecord{virtualPkg("__cuda", "12.4", "0")}})
	if len(sol.Records) != 1 || sol.Records[0].Name != "gpu" {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	mustUnsolvable(t, src, Request{Specs: []string{"gpu"}, Virtual: []*record.PackageRecord{virtualPkg("__cuda", "11.8", "0")}})

	// A root spec on a virtual package checks the machine.
	vs := []*record.PackageRecord{virtualPkg("__glibc", "2.28", "0"), virtualPkg("__unix", "0", "0")}
	sol = mustSolve(t, src, Request{Specs: []string{"__glibc >=2.17"}, Virtual: vs})
	if len(sol.Records) != 0 {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	mustUnsolvable(t, src, Request{Specs: []string{"__glibc >=2.34"}, Virtual: vs})
}

func TestUnsupportedDependencyForms(t *testing.T) {
	recs := []*record.PackageRecord{
		pkg("foo").version("1.0").rec(),
		pkg("foo").version("2.0").depends("bar; if __win").rec(),
		pkg("foo").version("3.0").depends(`bar[when="__win"]`).rec(),
		pkg("foo").version("4.0").constrains("bar[extras=[x]]").rec(),
		pkg("foo").version("5.0").depends("bar*").rec(),
		pkg("bar").version("1.0").rec(),
	}
	sol := mustSolve(t, repodata.NewMemory(recs), Request{Specs: []string{"foo"}})
	if len(sol.Records) != 1 || sol.Records[0].Version != "1.0" {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	u := mustUnsolvable(t, repodata.NewMemory(recs), Request{Specs: []string{"foo>=2"}})
	for _, want := range []string{
		`foo 2.0 is excluded because its dependency "bar; if __win" uses conditional dependencies, which goomba does not support yet`,
		`foo 3.0 is excluded because its dependency "bar[when=\"__win\"]" uses conditional dependencies`,
		`foo 4.0 is excluded because its dependency "bar[extras=[x]]" uses extras`,
		`foo 5.0 is excluded because unparsable dependency "bar*": the name "bar*" is a pattern, not a package name`,
	} {
		if !strings.Contains(u.Explanation, want) {
			t.Errorf("explanation lacks %q:\n%s", want, u.Explanation)
		}
	}
}

func TestSourceOrderDoesNotMatter(t *testing.T) {
	recs := dummyChannel(t)
	for _, n := range []string{"a", "b"} {
		recs = append(recs,
			pkg("tie").version("1.0").build(n).rec(),
			pkg("tie2").version("1.0").build(n).depends("tie").rec())
	}
	reqs := []Request{
		{Specs: []string{"foo<4", "bors"}},
		{Specs: []string{"tie2"}},
		{Specs: []string{"foobar >=2", "bors >= 2"}},
		{Specs: []string{"xbar"}, Constraints: []string{"xfoo==1"}},
	}
	var want []string
	for _, req := range reqs {
		want = append(want, outcome(Solve(context.Background(), repodata.NewMemory(recs), req)))
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		shuffled := append([]*record.PackageRecord(nil), recs...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for j, req := range reqs {
			if got := outcome(Solve(context.Background(), repodata.NewMemory(shuffled), req)); got != want[j] {
				t.Fatalf("shuffle %d, request %q:\n%s\nwant:\n%s", i, req.Specs, got, want[j])
			}
		}
	}
}

// outcome formats a solve result for comparison.
func outcome(sol *Solution, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	for _, r := range sol.Records {
		b.WriteString(r.Channel + "/" + r.Subdir + "/" + r.Fn + "\n")
	}
	return b.String()
}

func TestPrefetchSpecNames(t *testing.T) {
	src := &recordingSource{src: repodata.NewMemory(dummyChannel(t))}
	mustSolve(t, src, Request{
		Specs:   []string{"foo<4", "bors", "foo", "conda-forge::foobar", "__unix"},
		Virtual: []*record.PackageRecord{virtualPkg("__unix", "0", "0")},
	})
	if len(src.prefetch) != 1 || strings.Join(src.prefetch[0], ",") != "foo,bors,foobar" {
		t.Fatalf("prefetch %q", src.prefetch)
	}
}

func TestInvalidRequests(t *testing.T) {
	src := repodata.NewMemory(dummyChannel(t))
	foo := pkg("foo").version("1.0").rec()
	for _, c := range []struct {
		name string
		req  Request
	}{
		{"bad spec", Request{Specs: []string{"foo >=<1"}}},
		{"pattern name", Request{Specs: []string{"foo*"}}},
		{"defaults channel", Request{Specs: []string{"defaults::foo"}}},
		{"conditional root spec", Request{Specs: []string{"foo; if __win"}}},
		{"bad constraint", Request{Constraints: []string{"[["}}},
		{"pattern constraint", Request{Constraints: []string{"*"}}},
		{"nil virtual", Request{Virtual: []*record.PackageRecord{nil}}},
		{"non-virtual virtual", Request{Virtual: []*record.PackageRecord{foo}}},
		{"duplicate virtual", Request{Virtual: []*record.PackageRecord{virtualPkg("__unix", "0", "0"), virtualPkg("__unix", "0", "0")}}},
		{"upper-case virtual", Request{Virtual: []*record.PackageRecord{virtualPkg("__UNIX", "0", "0")}}},
		{"bad virtual version", Request{Virtual: []*record.PackageRecord{virtualPkg("__glibc", "2..28", "0")}}},
		{"virtual locked", Request{Locked: []*record.PackageRecord{virtualPkg("__unix", "0", "0")}}},
		{"nil locked", Request{Locked: []*record.PackageRecord{nil}}},
		{"duplicate locked", Request{Locked: []*record.PackageRecord{foo, foo}}},
		{"duplicate favored", Request{Favored: []*record.PackageRecord{foo, foo}}},
		{"upper-case favored", Request{Favored: []*record.PackageRecord{pkg("Foo").rec()}}},
		{"pattern virtual", Request{Virtual: []*record.PackageRecord{virtualPkg("__*", "1", "0")}}},
		{"locked name with a space", Request{Locked: []*record.PackageRecord{pkg("a b").rec()}}},
		{"bad alias", Request{Specs: []string{"conda-forge::foo"}, ChannelAlias: "https://user:secret@example.com"}},
		{"bad alias without channel specs", Request{Specs: []string{"foo"}, ChannelAlias: "not a url"}},
	} {
		_, err := Solve(context.Background(), src, c.req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: got %v, want ErrInvalidRequest", c.name, err)
		}
		if err := Validate(c.req, nil); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: Validate got %v, want ErrInvalidRequest", c.name, err)
		}
	}
	if _, err := Solve(context.Background(), nil, Request{}); err == nil {
		t.Error("nil source accepted")
	}
}

func TestChannelQualifiedSpec(t *testing.T) {
	cf := pkg("foo").version("1.0").channel("https://conda.anaconda.org/conda-forge").rec()
	other := pkg("foo").version("2.0").channel("https://conda.anaconda.org/other").rec()
	src := repodata.NewMemory([]*record.PackageRecord{cf, other})
	sol := mustSolve(t, src, Request{Specs: []string{"conda-forge::foo"}})
	if len(sol.Records) != 1 || sol.Records[0] != cf {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	sol = mustSolve(t, src, Request{Specs: []string{"foo"}})
	if sol.Records[0] != other {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	// A channel alias changes what a channel name means.
	mirror := pkg("foo").version("1.5").channel("https://mirror.example.com/conda-forge").rec()
	sol = mustSolve(t, repodata.NewMemory([]*record.PackageRecord{cf, mirror}), Request{Specs: []string{"conda-forge::foo"}, ChannelAlias: "https://mirror.example.com"})
	if sol.Records[0] != mirror {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	u := mustUnsolvable(t, src, Request{Specs: []string{"conda-forge::foo>=2"}})
	if want := "No candidates were found for foo >=2 [channel=conda-forge]."; normalizeText(u.Explanation) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s", u.Explanation, want)
	}
}

func TestRootConstraintExplanation(t *testing.T) {
	// The panic_on_constraint case without the lock: the root constraint
	// is what fails, as in rattler's snapshot.
	_, err := solveDummy(t, Request{Specs: []string{"xbar"}, Constraints: []string{"xfoo==1"}})
	got := explanation(t, err)
	want := strings.TrimPrefix(snapshotBody(t, "backends__resolvo__panic_on_constraint"), rattlerPrefix)
	if normalizeText(got) != normalizeText(want) {
		t.Errorf("explanation:\n%s\nwant:\n%s", got, want)
	}
}

func TestLockedRecords(t *testing.T) {
	foo1 := pkg("foo").version("1.0").rec()
	foo2 := pkg("foo").version("2.0").rec()
	bar := pkg("bar").version("1.0").depends("foo").rec()
	src := repodata.NewMemory([]*record.PackageRecord{foo1, foo2, bar})

	// A lock identical to a source record replaces it: the solution holds
	// the caller's record.
	lock := pkg("foo").version("1.0").rec()
	sol := mustSolve(t, src, Request{Specs: []string{"bar"}, Locked: []*record.PackageRecord{lock}})
	if len(sol.Records) != 2 || sol.Records[1] != lock {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	// A lock the source does not have is still the only choice.
	gone := pkg("foo").version("0.5").rec()
	sol = mustSolve(t, src, Request{Specs: []string{"bar"}, Locked: []*record.PackageRecord{gone}})
	if sol.Records[1] != gone {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	// A locked record that cannot be verified makes its package
	// uninstallable rather than letting another version in.
	bad := pkg("foo").version("1.0").sha256("").rec()
	u := mustUnsolvable(t, src, Request{Specs: []string{"bar"}, Locked: []*record.PackageRecord{bad}})
	if !strings.Contains(u.Explanation, "no sha256: cannot be verified") {
		t.Errorf("explanation:\n%s", u.Explanation)
	}
	// Favored records of a locked package are ignored.
	sol = mustSolve(t, src, Request{Specs: []string{"bar"}, Locked: []*record.PackageRecord{lock}, Favored: []*record.PackageRecord{foo2}})
	if sol.Records[1] != lock {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
}

func TestFavoredRecords(t *testing.T) {
	foo1 := pkg("foo").version("1.0").rec()
	foo2 := pkg("foo").version("2.0").rec()
	src := repodata.NewMemory([]*record.PackageRecord{foo1, foo2})
	// Not in the source: added as a candidate and preferred.
	old := pkg("foo").version("0.5").rec()
	sol := mustSolve(t, src, Request{Specs: []string{"foo"}, Favored: []*record.PackageRecord{old}})
	if sol.Records[0] != old {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	// An excluded favored record is never preferred.
	bad := pkg("foo").version("0.5").sha256("").rec()
	sol = mustSolve(t, src, Request{Specs: []string{"foo"}, Favored: []*record.PackageRecord{bad}})
	if sol.Records[0] != foo2 {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	// The solver falls back from a favored record that does not fit.
	sol = mustSolve(t, src, Request{Specs: []string{"foo>=1"}, Favored: []*record.PackageRecord{old}})
	if sol.Records[0] != foo2 {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
}

func TestTrackFeaturesSortLast(t *testing.T) {
	plain := pkg("foo").version("1.0").rec()
	tracked := pkg("foo").version("2.0").trackFeatures("mkl").rec()
	sol := mustSolve(t, repodata.NewMemory([]*record.PackageRecord{plain, tracked}), Request{Specs: []string{"foo"}})
	if sol.Records[0] != plain {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	sol = mustSolve(t, repodata.NewMemory([]*record.PackageRecord{plain, tracked}), Request{Specs: []string{"foo>=2"}})
	if sol.Records[0] != tracked {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
}

func TestSourceErrorsAndCancellation(t *testing.T) {
	boom := errors.New("boom")
	src := &recordingSource{src: repodata.NewMemory(nil), err: boom}
	_, err := Solve(context.Background(), src, Request{Specs: []string{"foo"}})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	var u *Unsolvable
	if errors.As(err, &u) {
		t.Fatal("a source error is not an Unsolvable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src = &recordingSource{src: repodata.NewMemory(dummyChannel(t)), onRecords: func(string) { cancel() }}
	_, err = Solve(ctx, src, Request{Specs: []string{"foobar"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestDuplicateArchiveFormats(t *testing.T) {
	conda := pkg("foo").version("1.0").rec()
	bz2 := pkg("foo").version("1.0").tarBz2().rec()
	sol := mustSolve(t, repodata.NewMemory([]*record.PackageRecord{bz2, conda}), Request{Specs: []string{"foo"}})
	if sol.Records[0] != conda {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	// The .tar.bz2 twin stands in for an uninstallable .conda.
	badConda := pkg("foo").version("1.0").sha256("").rec()
	sol = mustSolve(t, repodata.NewMemory([]*record.PackageRecord{bz2, badConda}), Request{Specs: []string{"foo"}})
	if sol.Records[0] != bz2 {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
}

func TestNameMismatchFromSource(t *testing.T) {
	// repodata.Memory looks names up case-insensitively, so a record named
	// "Foo" answers for "foo"; it is excluded, and no spec on "foo"
	// matches it.
	src := repodata.NewMemory([]*record.PackageRecord{pkg("Foo").version("1.0").rec()})
	u := mustUnsolvable(t, src, Request{Specs: []string{"foo"}})
	if normalizeText(u.Explanation) != "No candidates were found for foo *." {
		t.Errorf("explanation:\n%s", u.Explanation)
	}
	p, _, err := newProvider(src, Request{})
	if err != nil {
		t.Fatal(err)
	}
	cands, err := p.GetCandidates(context.Background(), p.pool.InternPackageName("foo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands.Excluded) != 1 || p.DisplayString(cands.Excluded[0].Reason) != "its name \"Foo\" is not \"foo\"" {
		t.Errorf("candidates %+v", cands)
	}
}

func TestUnsolvableGraph(t *testing.T) {
	_, err := solveDummy(t, Request{Specs: []string{"foobar >=2", "bors >= 2"}})
	var u *Unsolvable
	if !errors.As(err, &u) {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.Error(), "solve: the request cannot be satisfied:\nThe following packages are incompatible\n") {
		t.Errorf("Error() = %q", u.Error())
	}
	g, err := u.Conflict.Graph()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for i := 0; i < g.NodeCount(); i++ {
		n := g.Node(i)
		if n.Kind != cdcl.ConflictNodeSolvable {
			continue
		}
		r := u.Record(n.Solvable)
		if r == nil {
			t.Fatalf("no record for solvable %d", n.Solvable)
		}
		names[r.Name] = true
		if u.Interner().DisplaySolvable(n.Solvable) != displayRecord(r) {
			t.Errorf("interner disagrees for %s", displayRecord(r))
		}
	}
	if !names["bors"] || !names["foobar"] {
		t.Errorf("graph names %v", names)
	}
	if u.Record(1<<20) != nil {
		t.Error("Record of an unknown id")
	}
}

func TestDisplayVersionSet(t *testing.T) {
	for _, c := range []struct{ spec, want string }{
		{"foo", "*"},
		{"foo >=1.2", ">=1.2"},
		{"foo=1.2", "1.2.*"},
		{"foo 1.2 py*", "==1.2 py*"},
		{"foo * py*", "* py*"},
		{"conda-forge::foo", "* [channel=conda-forge]"},
		{"conda-forge/linux-64::foo", "* [channel=conda-forge, subdir=linux-64]"},
		{"foo[build_number='>=3', md5=b3af409bb8423187c75e6c7f5b683908]", "* [build_number=>=3, md5=b3af409bb8423187c75e6c7f5b683908]"},
	} {
		ms, err := spec.ParseMatchSpec(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := displayVersionSet(ms); got != c.want {
			t.Errorf("displayVersionSet(%q) = %q, want %q", c.spec, got, c.want)
		}
	}
}

func TestRankVersions(t *testing.T) {
	// Cases from rattler crates/rattler_solve/src/resolvo/conda_sorting.rs
	// (rank_versions_follows_version_cmp, rank_versions_ranks_by_cmp_not_eq).
	for _, c := range []struct {
		versions []string
		want     []uint32
	}{
		{[]string{"1.0", "2.0", "0.9", "1.0.0"}, []uint32{1, 2, 0, 1}},
		// rattler's second case ranks "1.0_" and "1.0-", which compare
		// equal; package spec (like libmamba) rejects "1.0-", so the same
		// property is checked on versions spec accepts.
		{[]string{"1.00", "1.0", "1", "1.0.0.0"}, []uint32{0, 0, 0, 0}},
	} {
		p, _, err := newProvider(repodata.NewMemory(nil), Request{})
		if err != nil {
			t.Fatal(err)
		}
		name := p.pool.InternPackageName("foo")
		var ids []cdcl.SolvableID
		for i, v := range c.versions {
			ids = append(ids, p.addRecord(name, pkg("foo").version(v).build(fmt.Sprint(i)).rec(), ""))
		}
		p.rankVersions(ids)
		for i, id := range ids {
			if !p.solvables[id].versionOK {
				t.Fatalf("version %q does not parse", c.versions[i])
			}
			if got := p.solvables[id].rank; got != c.want[i] {
				t.Errorf("%q: rank of %q = %d, want %d", c.versions, c.versions[i], got, c.want[i])
			}
		}
	}
}

func TestSourceErrorWhileSorting(t *testing.T) {
	// Two equal builds of pkg depend on dep, so sorting them loads dep;
	// a failing source stops the solve with its error.
	boom := errors.New("boom")
	recs := duplicateDependencyRepository(false)
	src := &recordingSource{src: repodata.NewMemory(recs)}
	src.onRecords = func(name string) {
		if name == "dep" {
			src.err = boom
		}
	}
	_, err := Solve(context.Background(), src, Request{Specs: []string{"pkg"}})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	asked := 0
	for _, n := range src.asked {
		if n == "dep" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("the failing source was asked %d times for dep", asked)
	}
}

func TestDoubleUnderscoreIsAnOrdinaryName(t *testing.T) {
	// From the WP7 review (N1): "__" alone is not a virtual package, for
	// the solver as for record.PackageRecord.IsVirtual and Validate.
	r := pkg("__").version("1.0").rec()
	req := Request{Specs: []string{"__"}}
	sol := mustSolve(t, repodata.NewMemory([]*record.PackageRecord{r}), req)
	if len(sol.Records) != 1 || sol.Records[0] != r {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	if err := Validate(req, sol.Records); err != nil {
		t.Fatal(err)
	}
	if _, err := Solve(context.Background(), repodata.NewMemory(nil), Request{Virtual: []*record.PackageRecord{virtualPkg("__", "1", "0")}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("virtual package \"__\": %v", err)
	}
}

func TestDigestsNormalized(t *testing.T) {
	// From the WP7 review (N5): records from Memory, Favored and Locked
	// come out with lower-case digests.
	up := pkg("foo").version("1").sha256(strings.ToUpper(dummySHA256)).rec()
	up.MD5 = strings.ToUpper(dummyMD5)
	bar := pkg("bar").version("1").sha256(strings.ToUpper(dummySHA256)).rec()
	baz := pkg("baz").version("1").sha256(strings.ToUpper(dummySHA256)).rec()
	src := repodata.NewMemory([]*record.PackageRecord{up, pkg("bar").version("2").rec()})
	sol := mustSolve(t, src, Request{
		Specs:   []string{"foo[sha256=" + dummySHA256 + "]", "bar", "baz"},
		Favored: []*record.PackageRecord{bar},
		Locked:  []*record.PackageRecord{baz},
	})
	if len(sol.Records) != 3 {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	for _, r := range sol.Records {
		if r.SHA256 != dummySHA256 || (r.Name == "foo" && r.MD5 != dummyMD5) {
			t.Errorf("%s: sha256 %s md5 %s", displayRecord(r), r.SHA256, r.MD5)
		}
	}
	if up.SHA256 == dummySHA256 {
		t.Error("the source's record was modified")
	}
}

func TestCanonicalOrderComparesNames(t *testing.T) {
	// From the WP7 review (N6): records that differ only in their name
	// (one of them excluded for it) are ordered by name, not by the source.
	a := pkg("foo").version("1").rec()
	b := pkg("Foo").version("1").rec()
	b.Fn = a.Fn
	for _, order := range [][]*record.PackageRecord{{a, b}, {b, a}} {
		p, _, err := newProvider(repodata.NewMemory(order), Request{})
		if err != nil {
			t.Fatal(err)
		}
		cands, err := p.GetCandidates(context.Background(), p.pool.InternPackageName("foo"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cands.Candidates) != 2 || p.solvables[cands.Candidates[0]].rec != b {
			t.Fatalf("order %s: candidates %v", formatRecords(order), cands.Candidates)
		}
	}
}

// Cases from the WP7 review (S1): version sets were interned by
// MatchSpec.String(), which dropped subdir= next to url= and printed a URL
// spec with an open version as the bare URL, so different dependencies
// shared a version set.
func TestURLSpecVersionSets(t *testing.T) {
	const u = "https://conda.anaconda.org/x/linux-64/foo-1.0-0.conda"
	foo := pkg("foo").version("1.0").build("0").channel("https://conda.anaconda.org/x").rec()
	foo.URL = u

	// A dependency with an extra subdir= must not stand in for one
	// without (TestReviewURLSubdirInterningLosesSolution).
	qux2 := pkg("qux").version("2").depends("foo[url=" + u + ",subdir=osx-64]").rec()
	qux1 := pkg("qux").version("1").rec()
	baz := pkg("baz").version("1").depends("foo[url=" + u + "]").rec()
	req := Request{Specs: []string{"qux", "baz"}}
	sol := mustSolve(t, repodata.NewMemory([]*record.PackageRecord{foo, qux1, qux2, baz}), req)
	if !anyMatch(sol.Records, exact(qux1)) || !anyMatch(sol.Records, exact(foo)) {
		t.Errorf("solution %s", formatRecords(sol.Records))
	}

	// The answer does not depend on which spec is interned first
	// (TestReviewURLSubdirInterning).
	bar1 := pkg("bar").version("1").depends("foo[url=" + u + ",subdir=osx-64]").rec()
	bar2 := pkg("bar").version("2").depends("foo[url=" + u + ",subdir=osx-64]").rec()
	recs := []*record.PackageRecord{foo, bar1, bar2, baz}
	for _, specs := range [][]string{{"bar"}, {"foo[url=" + u + "]", "bar"}, {"baz", "bar"}, {"bar", "baz"}, {"bar", "foo"}} {
		_, err := Solve(context.Background(), repodata.NewMemory(recs), Request{Specs: specs})
		var un *Unsolvable
		if !errors.As(err, &un) {
			t.Errorf("%q: %v, want unsolvable (no foo for osx-64)", specs, err)
		}
	}
	sol = mustSolve(t, repodata.NewMemory(recs), Request{Specs: []string{"baz"}})
	if !anyMatch(sol.Records, exact(foo)) {
		t.Errorf("solution %s", formatRecords(sol.Records))
	}

	// A URL spec with an open version is not the bare URL, which pins the
	// file name's version (TestReviewURLSpecInterning): a record served at
	// that URL whose metadata says 2.0 matches the first only.
	foo2 := pkg("foo").version("2.0").build("0").channel("https://conda.anaconda.org/x").rec()
	foo2.URL, foo2.Fn = u, "foo-1.0-0.conda"
	dep := pkg("dep").version("1").depends("foo[url=" + u + "]").rec()
	for _, specs := range [][]string{{"dep"}, {"dep", "foo[url=" + u + "]"}} {
		sol := mustSolve(t, repodata.NewMemory([]*record.PackageRecord{foo2, dep}), Request{Specs: specs})
		if !anyMatch(sol.Records, exact(foo2)) {
			t.Errorf("%q: solution %s", specs, formatRecords(sol.Records))
		}
	}
	for _, specs := range [][]string{{u, "dep"}, {"dep", u}} {
		mustUnsolvable(t, repodata.NewMemory([]*record.PackageRecord{foo2, dep}), Request{Specs: specs})
	}
}

// From the WP7 review (N4, TestReviewMicromambaStrictPriorityChannelSpec):
// a spec naming a channel that strict channel priority did not use for
// the package says so, as micromamba's "excluded by strict repo priority"
// does, instead of reporting that no candidates exist.
func TestChannelHiddenByStrictPriority(t *testing.T) {
	const a, b = "https://conda.anaconda.org/a", "https://conda.anaconda.org/b"
	fooA := pkg("foo").version("1.0").channel(a).rec()
	fooB := pkg("foo").version("2.0").channel(b).rec()
	app := pkg("app").version("1").channel(a).depends("b::foo").rec()
	src := repodata.NewMulti(repodata.NewMemory([]*record.PackageRecord{fooA, app}), repodata.NewMemory([]*record.PackageRecord{fooB}))

	u := mustUnsolvable(t, src, Request{Specs: []string{"b::foo"}})
	want := `The following packages are incompatible
└─ foo * [channel=b] cannot be installed because there are no viable options:
   └─ foo from https://conda.anaconda.org/b is excluded because the channels in use offer foo only from https://conda.anaconda.org/a (strict channel priority)`
	if normalizeText(u.Explanation) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s", u.Explanation, want)
	}
	for i := 0; i < u.p.pool.SolvableCount(); i++ {
		if u.p.solvables[i].hidden && u.Record(cdcl.SolvableID(i)) != nil {
			t.Error("Record of a placeholder")
		}
	}

	// The same through a dependency.
	u = mustUnsolvable(t, src, Request{Specs: []string{"app"}})
	if !strings.Contains(u.Explanation, "foo from https://conda.anaconda.org/b is excluded because the channels in use offer foo only from https://conda.anaconda.org/a") {
		t.Errorf("explanation:\n%s", u.Explanation)
	}

	// The placeholder never stands in for a record: a spec without a
	// channel, or naming the channel in use, solves as before.
	sol := mustSolve(t, src, Request{Specs: []string{"a::foo", "foo"}})
	if len(sol.Records) != 1 || sol.Records[0] != fooA {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	// A package nobody offers still has no candidates.
	u = mustUnsolvable(t, src, Request{Specs: []string{"b::bar"}})
	if normalizeText(u.Explanation) != "No candidates were found for bar * [channel=b]." {
		t.Errorf("explanation:\n%s", u.Explanation)
	}
}

// Cases from the WP7 review (TestReviewSortingCases).
func TestSortingCases(t *testing.T) {
	// A tracked feature on a dependency's highest version: the build
	// whose best reachable dependency has no tracked feature comes first.
	d2 := pkg("d").version("2").trackFeatures("tf").rec()
	d1 := pkg("d").version("1").rec()
	a := pkg("top").version("1").build("a").depends("d").rec()
	b := pkg("top").version("1").build("b").depends("d <2").rec()
	sol := mustSolve(t, repodata.NewMemory([]*record.PackageRecord{d1, d2, a, b}), Request{Specs: []string{"top"}})
	if !anyMatch(sol.Records, exact(b)) {
		t.Errorf("tracked-feature dependency: %s", formatRecords(sol.Records))
	}
	// The newest timestamp wins exact ties; a missing timestamp loses.
	x1 := pkg("x").version("1").build("b1").timestamp("2024-01-01T00:00:00Z").rec()
	x2 := pkg("x").version("1").build("b2").timestamp("2025-01-01T00:00:00Z").rec()
	x3 := pkg("x").version("1").build("b3").rec()
	sol = mustSolve(t, repodata.NewMemory([]*record.PackageRecord{x3, x1, x2}), Request{Specs: []string{"x"}})
	if !anyMatch(sol.Records, exact(x2)) {
		t.Errorf("timestamps: %s", formatRecords(sol.Records))
	}
	sol = mustSolve(t, repodata.NewMemory([]*record.PackageRecord{x3, x1}), Request{Specs: []string{"x"}})
	if !anyMatch(sol.Records, exact(x1)) {
		t.Errorf("missing timestamp: %s", formatRecords(sol.Records))
	}
	// Dependencies not shared by every candidate are not scored; a
	// candidate with an unsatisfiable one is abandoned for the other.
	m1 := pkg("m").version("1").build("p").depends("q", "r").rec()
	m2 := pkg("m").version("1").build("o").depends("q", "nothere").rec()
	q := pkg("q").version("1").rec()
	r := pkg("r").version("5").rec()
	sol = mustSolve(t, repodata.NewMemory([]*record.PackageRecord{m2, m1, q, r}), Request{Specs: []string{"m"}})
	if !anyMatch(sol.Records, exact(m1)) {
		t.Errorf("missing dependency: %s", formatRecords(sol.Records))
	}
}

// From the WP7 review (TestReviewFavoredAbandoned): a favored record is a
// preference, not a guarantee (see the package documentation). Whatever
// the spec order, the solution is valid; with "foo" decided first the
// favored foo 1 is kept.
func TestFavoredIsAPreference(t *testing.T) {
	foo1 := pkg("foo").version("1").rec()
	foo2 := pkg("foo").version("2").rec()
	bar1 := pkg("bar").version("1").rec()
	bar2 := pkg("bar").version("2").depends("foo >=2").rec()
	recs := []*record.PackageRecord{foo1, foo2, bar1, bar2}
	for _, specs := range [][]string{{"foo", "bar"}, {"bar", "foo"}} {
		req := Request{Specs: specs, Favored: []*record.PackageRecord{foo1}}
		sol := mustSolve(t, repodata.NewMemory(recs), req)
		if err := Validate(req, sol.Records); err != nil {
			t.Fatal(err)
		}
		t.Logf("%q with foo 1 favored: %s", specs, formatRecords(sol.Records))
		if specs[0] == "foo" && !anyMatch(sol.Records, exact(foo1)) {
			t.Errorf("%q: favored foo 1 not kept: %s", specs, formatRecords(sol.Records))
		}
	}
}

// From the WP7 review (round 2, N7): loading a package with very many
// records stops soon after ctx is cancelled (1M records: 2.4 s before,
// within 1 ms of the deadline after). The context here reports
// cancellation from its 600th Err call on, which falls in the middle of
// loading; the provider must stop there rather than parse every record's
// dependencies.
func TestLoadingHonoursCancellation(t *testing.T) {
	const n = 20000
	recs := make([]*record.PackageRecord, 0, n)
	for i := 0; i < n; i++ {
		recs = append(recs, pkg("big").version(fmt.Sprintf("%d.%d", i/100, i%100)).depends(fmt.Sprintf("dep%d", i)).rec())
	}
	p, _, err := newProvider(repodata.NewMemory(recs), Request{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := &countdownContext{Context: context.Background(), left: 600}
	if _, err := p.GetCandidates(ctx, p.pool.InternPackageName("big")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(p.specCache) == 0 || len(p.specCache) > 2*cancelCheckInterval {
		t.Errorf("parsed %d dependencies, want loading to stop soon after cancellation", len(p.specCache))
	}
}

// countdownContext reports context.Canceled from Err once Err has been
// called left times.
type countdownContext struct {
	context.Context
	left int
}

func (c *countdownContext) Err() error {
	if c.left > 0 {
		c.left--
		return nil
	}
	return context.Canceled
}
