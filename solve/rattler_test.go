package solve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
)

// Cases from rattler crates/rattler_solve/tests/backends/solver_case_tests.rs.

func TestSolverCaseFavored(t *testing.T) {
	bors1 := pkg("bors").version("1.0").rec()
	bors2 := pkg("bors").version("2.0").rec()
	solverCase{
		name:    "bors is favored when already installed",
		repos:   [][]*record.PackageRecord{{bors1, bors2}},
		specs:   []string{"bors"},
		favored: []*record.PackageRecord{bors1},
		present: []matcher{nv("bors", "1.0")},
	}.run(t)
}

func TestSolverCaseConstraints(t *testing.T) {
	bors1 := pkg("bors").version("1.0").rec()
	bors2 := pkg("bors").version("2.0").rec()
	foobar := pkg("foobar").version("1.0").depends("bors").rec()
	for _, c := range []solverCase{
		{
			name:        "constraints limit package selection",
			repos:       [][]*record.PackageRecord{{bors1, bors2, foobar}},
			specs:       []string{"foobar"},
			constraints: []string{"bors <=1"},
			present:     []matcher{nv("bors", "1.0"), nv("foobar", "1.0")},
		},
		{
			name:        "constraints on non-existing packages are allowed",
			repos:       [][]*record.PackageRecord{{bors1, bors2, foobar}},
			specs:       []string{"foobar"},
			constraints: []string{"bors <=1", "nonexisting"},
			present:     []matcher{nv("bors", "1.0"), nv("foobar", "1.0")},
		},
	} {
		c.run(t)
	}
}

func TestSolverCaseUpgrade(t *testing.T) {
	foo1 := pkg("foo").version("1.0").rec()
	foo2 := pkg("foo").version("2.0").rec()
	solverCase{
		name:    "upgrade foo from 1.0 to 2.0",
		repos:   [][]*record.PackageRecord{{foo1, foo2}},
		specs:   []string{"foo>=2"},
		favored: []*record.PackageRecord{foo1},
		present: []matcher{nv("foo", "2.0")},
	}.run(t)
}

func TestSolverCaseDowngrade(t *testing.T) {
	foo1 := pkg("foo").version("1.0").rec()
	foo2 := pkg("foo").version("2.0").rec()
	solverCase{
		name:    "downgrade foo from 2.0 to 1.0",
		repos:   [][]*record.PackageRecord{{foo1, foo2}},
		specs:   []string{"foo<2"},
		favored: []*record.PackageRecord{foo2},
		present: []matcher{nv("foo", "1.0")},
	}.run(t)
}

func TestSolverCaseInstallNew(t *testing.T) {
	solverCase{
		name:    "install picks highest matching version",
		repos:   [][]*record.PackageRecord{{pkg("foo").version("1.0").rec(), pkg("foo").version("2.0").rec()}},
		specs:   []string{"foo<2"},
		present: []matcher{nv("foo", "1.0")},
	}.run(t)
}

func TestSolverCaseRemove(t *testing.T) {
	foo := pkg("foo").version("1.0").rec()
	solverCase{
		name:    "removing a package results in empty solution",
		repos:   [][]*record.PackageRecord{{foo}},
		favored: []*record.PackageRecord{foo},
		absent:  []matcher{name("foo")},
	}.run(t)
}

func TestSolverCaseNoop(t *testing.T) {
	foo1 := pkg("foo").version("1.0").rec()
	solverCase{
		name:    "keep installed package when it satisfies spec",
		repos:   [][]*record.PackageRecord{{foo1, pkg("foo").version("2.0").rec()}},
		specs:   []string{"foo<2"},
		favored: []*record.PackageRecord{foo1},
		present: []matcher{nv("foo", "1.0")},
	}.run(t)
}

func TestSolveWithUnparsableDependency(t *testing.T) {
	valid := pkg("sortme").version("1.0.0").build("build_a").rec()
	invalid := pkg("sortme").version("2.0.0").build("build_b").depends("this-is-not-a-valid-matchspec @#$%^&*()").rec()
	solverCase{
		name:    "solver handles unparsable dependencies gracefully",
		repos:   [][]*record.PackageRecord{{valid, invalid}},
		specs:   []string{"sortme"},
		present: []matcher{nv("sortme", "1.0.0")},
	}.run(t)
}

// TestResolvoIssue188 reproduces https://github.com/prefix-dev/resolvo/issues/188.
func TestResolvoIssue188(t *testing.T) {
	solverCase{
		name: "solver respects constraints on package versions",
		repos: [][]*record.PackageRecord{{
			pkg("dependency_package").version("0.1.0").rec(),
			pkg("dependency_package").version("0.2.0").rec(),
			pkg("dependent_package").version("1.0.0").depends("dependency_package ==0.2.0").rec(),
		}},
		specs:       []string{"dependency_package <0.2.0"},
		constraints: []string{"dependent_package ==1.0.0"},
		absent:      []matcher{name("dependent_package")},
	}.run(t)
}

// Cases from rattler crates/rattler_solve/tests/backends/sorting_tests.rs.

func duplicateDependencyRepository(bare bool) []*record.PackageRecord {
	reqs := func(pin string) []string {
		if bare {
			return []string{"dep", pin}
		}
		return []string{pin}
	}
	return []*record.PackageRecord{
		pkg("dep").version("24.19.0").rec(),
		pkg("dep").version("26.6.0").rec(),
		pkg("pkg").version("1.0").buildNumber(0).build("h_dep24_0").depends(reqs("dep >=24.18.0,<25.0a0")...).timestamp("2026-07-31T18:34:43Z").rec(),
		pkg("pkg").version("1.0").buildNumber(0).build("h_dep26_0").depends(reqs("dep >=26.5.1,<27.0a0")...).timestamp("2026-07-31T18:34:41Z").rec(),
	}
}

func TestPrefersBuildWithHighestDependency(t *testing.T) {
	solverCase{
		name:    "build pinning the newest dependency wins over a newer build timestamp",
		repos:   [][]*record.PackageRecord{duplicateDependencyRepository(false)},
		specs:   []string{"pkg"},
		present: []matcher{nvb("pkg", "1.0", "h_dep26_0"), nv("dep", "26.6.0")},
	}.run(t)
}

func TestPrefersBuildWithHighestDependencyWithBareRequirement(t *testing.T) {
	solverCase{
		name:    "a bare requirement next to a pin does not mask the pin",
		repos:   [][]*record.PackageRecord{duplicateDependencyRepository(true)},
		specs:   []string{"pkg"},
		present: []matcher{nvb("pkg", "1.0", "h_dep26_0"), nv("dep", "26.6.0")},
	}.run(t)
}

// Cases from rattler crates/rattler_solve/tests/backends/strategy_tests.rs,
// run with the default (highest) strategy, the only one goomba has: each
// expectation is what SolveStrategy::Highest gives for the same channel.

func TestStrategyCasesWithHighest(t *testing.T) {
	pkgV1B1 := pkg("pkg").version("1.0").buildNumber(1).build("build_1").rec()
	pkgV1B2 := pkg("pkg").version("1.0").buildNumber(2).build("build_2").rec()
	pkgV2 := pkg("pkg").version("2.0").rec()
	dep1 := pkg("dep").version("1.0").rec()
	dep2 := pkg("dep").version("2.0").rec()
	mainPkg := pkg("main").version("1.0").depends("dep").rec()
	for _, c := range []solverCase{
		{
			name:    "highest version strategy picks the highest version",
			repos:   [][]*record.PackageRecord{{pkgV1B1, pkgV1B2, pkgV2}},
			specs:   []string{"pkg"},
			present: []matcher{nv("pkg", "2.0")},
		},
		{
			name:    "highest version strategy picks the highest build number of a version",
			repos:   [][]*record.PackageRecord{{pkgV1B1, pkgV1B2, pkgV2}},
			specs:   []string{"pkg <2"},
			present: []matcher{nvb("pkg", "1.0", "build_2")},
		},
		{
			name:    "highest version strategy applies to transitive deps",
			repos:   [][]*record.PackageRecord{{dep1, dep2, mainPkg}},
			specs:   []string{"main"},
			present: []matcher{nv("main", "1.0"), nv("dep", "2.0")},
		},
	} {
		c.run(t)
	}
}

// Cases from rattler crates/rattler_solve/tests/backends/multichannel_tests.rs.
//
// goomba has no multichannels: strict channel priority is the source's job
// (repodata.Multi), and a multichannel, whose members share one priority
// tier, is one source holding the records of every member. The strict
// cases port directly; flexible and disabled priority do not exist.

func mcPackage(name, version, channel string) *record.PackageRecord {
	return pkg(name).version(version).channel("https://conda.anaconda.org/" + channel).rec()
}

func TestMultichannelChannelPriority(t *testing.T) {
	pkgA := mcPackage("pkg", "1.0", "chan-a")
	pkgB := mcPackage("pkg", "2.0", "chan-b")
	pkgC := mcPackage("pkg", "3.0", "chan-c")
	repos := [][]*record.PackageRecord{{pkgA, pkgB}, {pkgC}}
	for _, c := range []solverCase{
		{name: "strict priority picks the highest version within the multichannel", repos: repos, specs: []string{"pkg"}, present: []matcher{exact(pkgB)}},
		{name: "strict priority falls back to a later member", repos: repos, specs: []string{"pkg>=1.5"}, present: []matcher{exact(pkgB)}},
		{name: "strict priority does not fall back to a channel after the multichannel", repos: repos, specs: []string{"pkg>=2.5"}, unsolvable: true},
	} {
		c.run(t)
	}
}

func TestMultichannelStrictPriorityIsPerPackage(t *testing.T) {
	pkgA := mcPackage("pkg", "1.0", "chan-a")
	otherC := mcPackage("other", "1.0", "chan-c")
	solverCase{
		name:    "a package missing from every member comes from a later channel",
		repos:   [][]*record.PackageRecord{{pkgA}, {otherC}},
		specs:   []string{"pkg", "other"},
		present: []matcher{exact(pkgA), exact(otherC)},
	}.run(t)
}

func TestMultichannelAfterChannel(t *testing.T) {
	solverCase{
		name: "strict priority excludes a multichannel after the first channel",
		repos: [][]*record.PackageRecord{
			{mcPackage("pkg", "3.0", "chan-c")},
			{mcPackage("pkg", "1.0", "chan-a"), mcPackage("pkg", "2.0", "chan-b")},
		},
		specs:      []string{"pkg<3"},
		unsolvable: true,
	}.run(t)
}

func TestMultichannelMultipleMultichannels(t *testing.T) {
	pkgB := mcPackage("pkg", "2.0", "chan-b")
	repos := [][]*record.PackageRecord{
		{mcPackage("pkg", "1.0", "chan-a"), pkgB},
		{mcPackage("pkg", "3.0", "chan-c"), mcPackage("pkg", "4.0", "chan-d")},
	}
	for _, c := range []solverCase{
		{name: "strict priority picks from the first multichannel", repos: repos, specs: []string{"pkg"}, present: []matcher{exact(pkgB)}},
		{name: "strict priority does not fall back to the second multichannel", repos: repos, specs: []string{"pkg>=2.5"}, unsolvable: true},
	} {
		c.run(t)
	}
}

func TestMultichannelMembersApart(t *testing.T) {
	// The members a and b share the first tier although c is listed
	// between them in rattler; one source holds both.
	pkgB := mcPackage("pkg", "2.0", "chan-b")
	solverCase{
		name:    "a member after another channel keeps the tier of the multichannel",
		repos:   [][]*record.PackageRecord{{mcPackage("pkg", "1.0", "chan-a"), pkgB}, {mcPackage("pkg", "3.0", "chan-c")}},
		specs:   []string{"pkg"},
		present: []matcher{exact(pkgB)},
	}.run(t)
}

func TestMultichannelDependencyFromOtherMember(t *testing.T) {
	appA := pkg("app").version("1.0").channel("https://conda.anaconda.org/chan-a").depends("lib>=2").rec()
	libB := mcPackage("lib", "2.0", "chan-b")
	solverCase{
		name:    "a dependency comes from a later member",
		repos:   [][]*record.PackageRecord{{appA, mcPackage("lib", "1.0", "chan-a"), libB}},
		specs:   []string{"app"},
		present: []matcher{exact(appA), exact(libB)},
	}.run(t)
}

func TestMultichannelTies(t *testing.T) {
	pkgB := mcPackage("pkg", "2.0", "chan-b")
	pkgD := mcPackage("pkg", "2.0", "chan-d")
	pkgDRebuilt := pkg("pkg").version("2.0").buildNumber(1).channel("https://conda.anaconda.org/chan-d").rec()
	for _, c := range []solverCase{
		{
			name:    "an identical candidate comes from the first member",
			repos:   [][]*record.PackageRecord{{pkgB, pkgD}},
			specs:   []string{"pkg"},
			present: []matcher{exact(pkgB)},
			absent:  []matcher{exact(pkgD)},
		},
		{
			// rattler keeps the source order here and picks pkgD. goomba
			// orders a source's records canonically first (by subdir, file
			// name, then channel URL), so the answer never depends on the
			// order a source returns records in, and picks pkgB again.
			name:    "an identical candidate comes from the first member in reversed order",
			repos:   [][]*record.PackageRecord{{pkgD, pkgB}},
			specs:   []string{"pkg"},
			present: []matcher{exact(pkgB)},
			absent:  []matcher{exact(pkgD)},
		},
		{
			name:    "a higher build number in a later member wins",
			repos:   [][]*record.PackageRecord{{pkgB, pkgDRebuilt}},
			specs:   []string{"pkg"},
			present: []matcher{exact(pkgDRebuilt)},
		},
	} {
		c.run(t)
	}
}

func TestMultichannelChannelSpecificSpecs(t *testing.T) {
	pkgA := mcPackage("pkg", "1.0", "chan-a")
	pkgB := mcPackage("pkg", "2.0", "chan-b")
	pkgC := mcPackage("pkg", "3.0", "chan-c")
	repos := [][]*record.PackageRecord{{pkgA, pkgB}, {pkgC}}
	for _, c := range []solverCase{
		{name: "a member name selects only that member", repos: repos, specs: []string{"chan-a::pkg"}, present: []matcher{exact(pkgA)}},
		{name: "a member name excludes the other members", repos: repos, specs: []string{"chan-a::pkg>=1.5"}, unsolvable: true},
		{
			// rattler selects a channel named in a spec even when strict
			// priority excludes it; conda (and goomba, whose source applies
			// strict priority) reports it unsolvable.
			name: "a channel outside the first tier cannot be selected by name", repos: repos,
			specs: []string{"chan-c::pkg"}, unsolvable: true,
		},
	} {
		c.run(t)
	}
}

// Cases from rattler crates/rattler_solve/tests/backends/main.rs (the
// resolvo backend).

func TestSolveWithError(t *testing.T) {
	_, err := solveDummy(t, Request{Specs: []string{"foobar >=2", "bors >= 2"}})
	// cdcl's explanation is rattler's.
	var u *Unsolvable
	if !errors.As(err, &u) {
		t.Fatal(err)
	}
	raw := u.Conflict.Display(u.Interner())
	want := strings.TrimPrefix(snapshotBody(t, "backends__resolvo__solve_with_error"), rattlerPrefix)
	if normalizeText(raw) != normalizeText(want) {
		t.Errorf("cdcl explanation:\n%s\nwant:\n%s", raw, want)
	}
	// goomba folds the sibling candidates that conflict with versions
	// reported above.
	folded := `The following packages are incompatible
├─ bors >=2 cannot be installed because there are no viable options:
│  └─ bors 2.0 | 2.1, which conflicts with the versions reported above.
└─ foobar >=2 cannot be installed because there are no viable options:
   └─ foobar 2.0 | 2.1 would require
      └─ bors <2.0, which cannot be installed because there are no viable options:
         └─ bors 1.0 | 1.1 | 1.2.1, which conflicts with the versions reported above.`
	if got := normalizeText(u.Explanation); got != folded {
		t.Errorf("explanation:\n%s\nwant:\n%s", got, folded)
	}
}

func TestSolveDummyRepoInstallNonExistent(t *testing.T) {
	_, err := solveDummy(t, Request{Specs: []string{"asdfasdf", "foo<4"}})
	got := explanation(t, err)
	// The snapshot is the Debug form of SolveError::Unsolvable.
	if want := "No candidates were found for asdfasdf *.\n"; got != want {
		t.Errorf("explanation %q, want %q", got, want)
	}
	if !strings.Contains(snapshotBody(t, "backends__resolvo__solve_dummy_repo_install_non_existent"), `"No candidates were found for asdfasdf *.\n"`) {
		t.Error("snapshot changed")
	}
}

func TestSolveDummyRepoMissingVirtualPackage(t *testing.T) {
	_, err := solveDummy(t, Request{Specs: []string{"bar"}})
	var u *Unsolvable
	if !errors.As(err, &u) {
		t.Fatalf("expected *Unsolvable, got %v", err)
	}
	if !strings.Contains(u.Explanation, "__unix") {
		t.Errorf("explanation does not name the missing virtual package:\n%s", u.Explanation)
	}
}

func TestSolveDummyRepoWithVirtualPackage(t *testing.T) {
	sol, err := solveDummy(t, Request{Specs: []string{"bar"}, Virtual: []*record.PackageRecord{virtualPkg("__unix", "0", "0")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sol.Records) != 1 || sol.Records[0].Name != "bar" || sol.Records[0].Version != "1.2.3" {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
}

func TestSolveDummyRepoInstallNew(t *testing.T) {
	sol, err := solveDummy(t, Request{Specs: []string{"foo<4"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sol.Records) != 1 {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
	r := sol.Records[0]
	for _, c := range []struct{ got, want string }{
		{r.Fn, "foo-3.0.2-py36h1af98f8_3.conda"},
		{r.URL, "https://conda.anaconda.org/conda-forge/linux-64/foo-3.0.2-py36h1af98f8_3.conda"},
		{r.Channel, "https://conda.anaconda.org/conda-forge"},
		{r.Name, "foo"},
		{r.Subdir, "linux-64"},
		{r.Version, "3.0.2"},
		{r.Build, "py36h1af98f8_3"},
		{r.BuildNumberString(), "3"},
		{r.SHA256, "67a63bec3fd3205170eaad532d487595b8aaceb9814d13c6858d7bac3ef24cd4"},
		{r.MD5, "fb731d9290f0bcbf3a054665f33ec94f"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestSolveDummyRepoPrefersCondaPackage(t *testing.T) {
	sol, err := solveDummy(t, Request{Specs: []string{"foo=3.0.2=py36h1af98f8_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sol.Records) != 1 || sol.Records[0].Fn != "foo-3.0.2-py36h1af98f8_1.conda" {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
}

func TestDuplicateRecord(t *testing.T) {
	recs := dummyChannel(t)
	recs = append(recs, recs[0])
	_, err := Solve(context.Background(), repodata.NewMemory(recs), Request{Specs: []string{recs[0].Name}})
	if !errors.Is(err, ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord, got %v", err)
	}
}

func TestVirtualPackageConstrains(t *testing.T) {
	_, err := solveDummy(t, Request{Specs: []string{"cuda-version"}, Virtual: []*record.PackageRecord{virtualPkg("__cuda", "1", "0")}})
	got := explanation(t, err)
	want := strings.TrimPrefix(snapshotBody(t, "backends__resolvo__virtual_package_constrains"), rattlerPrefix)
	if normalizeText(got) != normalizeText(want) {
		t.Errorf("explanation:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnsatMergedVersionsAreAbbreviated(t *testing.T) {
	var recs []*record.PackageRecord
	for major := 1; major <= 8; major++ {
		recs = append(recs, pkg("foo").version(fmt.Sprintf("%d.0", major)).build(fmt.Sprintf("h%d_0", major)).depends("bar ==1.0").rec())
	}
	_, err := Solve(context.Background(), repodata.NewMemory(recs), Request{Specs: []string{"foo"}})
	want := `foo * cannot be installed because there are no viable options:
└─ foo 1.0 | 2.0 | ... | 8.0 would require
   └─ bar ==1.0, for which no candidates were found.`
	if got := explanation(t, err); normalizeText(got) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s", got, want)
	}
}

func TestSolveLocked(t *testing.T) {
	_, err := solveDummy(t, Request{
		Specs:  []string{"bors >=2"},
		Locked: []*record.PackageRecord{installedPackage("conda-forge", "linux-64", "bors", "1.0", "bla_1", 1)},
	})
	got := explanation(t, err)
	// rattler (with the lock as resolvo's only rule):
	//   └─ bors >=2 can be installed with any of the following options:
	//      └─ bors 2.0 | 2.1
	//   └─ bors 1.0 is locked, but another version is required as reported above
	// goomba also excludes every other candidate of a locked package, with
	// the lock as the reason, so the conflict is reported on them.
	want := `The following packages are incompatible
└─ bors >=2 cannot be installed because there are no viable options:
   └─ bors 2.0 | 2.1 is excluded because bors=1.0=bla_1 is locked`
	if normalizeText(got) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s\nrattler:\n%s", got, want, snapshotBody(t, "backends__resolvo__solve_locked"))
	}
}

func TestLockedPackageOfAnUnrequestedPackageIsHarmless(t *testing.T) {
	// From test_excluded_pin_of_an_unrequested_package_is_harmless: a
	// locked record of a package nothing requires does not enter the
	// solution, even when it cannot be installed (here: no sha256).
	locked := installedPackage("conda-forge", "linux-64", "bors", "1.0", "bla_1", 1)
	locked.SHA256 = ""
	sol, err := solveDummy(t, Request{Specs: []string{"foo"}, Locked: []*record.PackageRecord{locked}})
	if err != nil {
		t.Fatal(err)
	}
	if anyMatch(sol.Records, name("bors")) {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}
}

func TestIssue717(t *testing.T) {
	_, err := solveDummy(t, Request{Specs: []string{"issue_717"}})
	got := explanation(t, err)
	// rattler: "issue_717 2.1 is excluded because the constrains
	// 'ray[default,data] >=2.9.0,<3.0.0' failed to parse: invalid package
	// name ..."; the reason text is goomba's own.
	want := `The following packages are incompatible
└─ issue_717 * cannot be installed because there are no viable options:
   └─ issue_717 2.1 is excluded because unparsable dependency "ray[default,data] >=2.9.0,<3.0.0": invalid character "[" in package name "ray[default,data]"`
	if normalizeText(got) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s\nrattler:\n%s", got, want, snapshotBody(t, "backends__resolvo__issue_717"))
	}
}

func TestSolveOnURL(t *testing.T) {
	const url = "https://conda.anaconda.org/conda-forge/linux-64/_libgcc_mutex-0.1-conda_forge.tar.bz2"
	// rattler's record carries no sha256 and no channel. goomba excludes
	// records without a sha256, and a package URL spec also names the
	// channel the file is in, so the test record has both.
	rec := &record.PackageRecord{
		Name: "_libgcc_mutex", Version: "0.1", Build: "conda_forge", Subdir: "linux-64",
		Fn: "_libgcc_mutex-0.1-conda_forge.tar.bz2", URL: url, SHA256: dummySHA256,
		Channel: "https://conda.anaconda.org/conda-forge",
	}
	sol, err := Solve(context.Background(), repodata.NewMemory([]*record.PackageRecord{rec}), Request{Specs: []string{url}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sol.Records) != 1 || sol.Records[0].URL != url || sol.Records[0].Version != "0.1" {
		t.Fatalf("solution %s", formatRecords(sol.Records))
	}

	// With a different URL the record is not the one the spec names.
	broken := *rec
	broken.URL = "https://false.dont"
	_, err = Solve(context.Background(), repodata.NewMemory([]*record.PackageRecord{&broken}), Request{Specs: []string{url}})
	var u *Unsolvable
	if !errors.As(err, &u) {
		t.Fatalf("expected *Unsolvable, got %v", err)
	}
}

func TestCancellationPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Solve(ctx, repodata.NewMemory(nil), Request{Specs: []string{"foo"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestPanicOnConstraint(t *testing.T) {
	_, err := solveDummy(t, Request{
		Specs:       []string{"xbar"},
		Constraints: []string{"xfoo==1"},
		Locked:      []*record.PackageRecord{installedPackage("conda-forge", "linux-64", "xfoo", "1", "xxx", 1)},
	})
	got := explanation(t, err)
	// rattler blames the root constraint xfoo ==1 (resolvo's lock is a
	// separate rule there). goomba excludes xfoo 2 because xfoo 1 is
	// locked, and that exclusion is the conflict it reports; both are
	// reasons the request fails.
	want := `The following packages are incompatible
└─ xbar * cannot be installed because there are no viable options:
   └─ xbar 1 would require
      └─ xfoo >=2, which cannot be installed because there are no viable options:
         └─ xfoo 2 is excluded because xfoo=1=xxx is locked`
	if normalizeText(got) != want {
		t.Errorf("explanation:\n%s\nwant:\n%s\nrattler:\n%s", got, want, snapshotBody(t, "backends__resolvo__panic_on_constraint"))
	}
	if !errors.Is(err, cdcl.ErrUnsolvable) {
		t.Error("Unsolvable does not unwrap to cdcl.ErrUnsolvable")
	}
}
