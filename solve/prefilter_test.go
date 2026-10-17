package solve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/spec"
)

// indexServer serves in-memory repodata.json documents, by path.
type indexServer struct {
	mu   sync.Mutex
	docs map[string][]byte
	srv  *httptest.Server
}

func newIndexServer(t *testing.T) *indexServer {
	s := &indexServer{docs: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		doc, ok := s.docs[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(doc)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// indexRecord is a package of a test repodata.json.
type indexRecord struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Build   string   `json:"build"`
	Depends []string `json:"depends"`
	SHA256  string   `json:"sha256"`
}

// publish stores a channel with linux-64 and noarch records and returns its
// URL.
func (s *indexServer) publish(channel string, linux, noarch []indexRecord) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sd, recs := range map[string][]indexRecord{"linux-64": linux, "noarch": noarch} {
		pk := map[string]indexRecord{}
		for _, r := range recs {
			r.SHA256 = dummySHA256
			if r.Depends == nil {
				r.Depends = []string{}
			}
			pk[r.Name+"-"+r.Version+"-"+r.Build+".conda"] = r
		}
		doc, _ := json.Marshal(map[string]any{"info": map[string]string{"subdir": sd}, "packages.conda": pk})
		s.docs["/"+channel+"/"+sd+"/repodata.json"] = doc
	}
	return s.srv.URL + "/" + channel
}

// solveWithPrefilter opens the channel with or without the python-minor
// prefilter RequestedPythonMinor derives from specs, and solves.
func solveWithPrefilter(t *testing.T, channel string, specs []string, filter bool) (*Solution, error) {
	t.Helper()
	opt := repodata.Options{CacheDir: t.TempDir(), Platform: "linux-64"}
	if filter {
		opt.PythonMinor = RequestedPythonMinor(specs)
	}
	idx, err := repodata.NewFullIndex(context.Background(), channel, opt)
	if err != nil {
		t.Fatal(err)
	}
	return Solve(context.Background(), idx, Request{Specs: specs})
}

// TestPrefilterKeepsPreReleaseSolutions is the WP7 review's
// TestReviewPrefilterLosesPreReleaseSolution: with python=3.11 and only a
// pre-release of 3.11 in the channel, a noarch package bounded "<3.11"
// stays installable.
func TestPrefilterKeepsPreReleaseSolutions(t *testing.T) {
	s := newIndexServer(t)
	channel := s.publish("ch",
		[]indexRecord{
			{Name: "python", Version: "3.11.0rc1", Build: "h0_0_cpython"},
			{Name: "python", Version: "3.10.4", Build: "h0_0_cpython"},
		},
		[]indexRecord{
			{Name: "oldpkg", Version: "1.0", Build: "pyhd8ed1ab_0", Depends: []string{"python >=3.8,<3.11"}},
			{Name: "cfpkg", Version: "1.0", Build: "pyhd8ed1ab_0", Depends: []string{"python >=3.10,<3.11.0a0"}},
		})
	for _, specs := range [][]string{{"python==3.11.0rc1", "oldpkg"}, {"python=3.11", "oldpkg"}} {
		for _, filter := range []bool{false, true} {
			sol, err := solveWithPrefilter(t, channel, specs, filter)
			if err != nil {
				t.Fatalf("%q (prefilter %v, minor %q): %v", specs, filter, RequestedPythonMinor(specs), err)
			}
			if !anyMatch(sol.Records, nv("python", "3.11.0rc1")) || !anyMatch(sol.Records, name("oldpkg")) {
				t.Errorf("%q: solution %s", specs, formatRecords(sol.Records))
			}
		}
	}
	// conda-forge's "<X.Y.0a0" bound is still filtered: with the prefilter
	// the record is gone, and the explanation says nothing about it.
	_, err := solveWithPrefilter(t, channel, []string{"python=3.11", "cfpkg"}, true)
	var u *Unsolvable
	if !errors.As(err, &u) || !strings.Contains(u.Explanation, "No candidates were found for cfpkg") {
		t.Errorf("cfpkg with the prefilter: %v", err)
	}
}

// TestPrefilterLosesNoSolution solves random channels with and without
// the python-minor prefilter and checks they agree. The python versions
// include pre-releases of X.Y.0 but no development builds (X.Y.0dev0),
// the documented gap of RequestedPythonMinor.
func TestPrefilterLosesNoSolution(t *testing.T) {
	pythons := []string{"3.10.4", "3.10.12", "3.11.0a1", "3.11.0b2", "3.11.0rc1", "3.11.0", "3.11.4", "3.11.4.post1", "3.12.0rc1", "3.12.1"}
	deps := []string{"python", "python >=3.8", "python >=3.8,<3.11", "python <3.11.0a0", "python >=3.11,<3.12.0a0", "python <3.11.0rc1",
		"python <=3.11.0a0", "python >=3.11.0b1", "python 3.11.*", "python >=3.12", "python <3.12", "python !=3.11.*", "python 3.10.*|>=3.12",
		"python ==3.11.0rc1", "python >3.11.0rc1,<3.11.1", "python >=3.11.5", "python ~=3.11.0", "python <3.11", "python 3.11.0rc1 *_cpython"}
	pins := []string{"python=3.11", "python 3.11.*", "python ==3.11.0rc1", "python=3.11.0", "python 3.11.4", "python=3.12", "python=3.10", "python=3.11.0a1"}
	s := newIndexServer(t)
	rng := rand.New(rand.NewSource(3))
	iters := 150
	if testing.Short() {
		iters = 30
	}
	for i := 0; i < iters; i++ {
		var linux, noarch []indexRecord
		for _, v := range pythons {
			if rng.Intn(2) == 0 {
				linux = append(linux, indexRecord{Name: "python", Version: v, Build: "h0_0_cpython"})
			}
		}
		for j := 0; j < 3; j++ {
			noarch = append(noarch, indexRecord{Name: "pkg", Version: fmt.Sprint(j + 1), Build: "pyh_0", Depends: []string{deps[rng.Intn(len(deps))]}})
		}
		channel := s.publish(fmt.Sprintf("ch%d", i), linux, noarch)
		specs := []string{pins[rng.Intn(len(pins))], "pkg"}
		plain, errPlain := solveWithPrefilter(t, channel, specs, false)
		filtered, errFiltered := solveWithPrefilter(t, channel, specs, true)
		if (errPlain == nil) != (errFiltered == nil) {
			t.Fatalf("case %d %q (minor %q): without prefilter %v %v, with prefilter %v %v\nlinux %v\nnoarch %v",
				i, specs, RequestedPythonMinor(specs), plain, errPlain, filtered, errFiltered, linux, noarch)
		}
	}
}

// TestPrefilterNeqBareStar is the WP7 review's TestReviewPrefilterNeqBareStar:
// "python !=3.11*" (no dot) means "not the literal version 3.11*" to
// package spec and libmamba, so python 3.11.4 satisfies it and the
// prefilter must keep the record.
func TestPrefilterNeqBareStar(t *testing.T) {
	s := newIndexServer(t)
	ch := s.publish("neq", []indexRecord{{Name: "python", Version: "3.11.4", Build: "h0_0_cpython"}},
		[]indexRecord{{Name: "pkg", Version: "1", Build: "0", Depends: []string{"python !=3.11*"}}})
	specs := []string{"python=3.11", "pkg"}
	for _, filter := range []bool{false, true} {
		if _, err := solveWithPrefilter(t, ch, specs, filter); err != nil {
			t.Errorf("prefilter %v (minor %q): %v", filter, RequestedPythonMinor(specs), err)
		}
	}
}

// TestPrefilterSemanticOracle is the WP7 review's
// TestReviewPrefilterSoundnessRandom: random canonical python
// dependencies, loaded through repodata.NewFullIndex with PythonMinor
// "3.11", must be kept whenever package spec says they admit some version
// of the series (versions matching 3.11.* from 3.11.0a0 up).
func TestPrefilterSemanticOracle(t *testing.T) {
	bounds := []string{"3", "3.10", "3.10.99", "3.11", "3.11.0", "3.11.0a0", "3.11.0a1", "3.11.0a", "3.11.0b0", "3.11.0rc1",
		"3.11.0dev0", "3.11.0.dev0", "3.11.dev0", "3.11a0", "3.11a", "3.11.0.0", "3.11.1", "3.11.0post1", "3.11.post1",
		"3.12", "3.12.0a0", "3.12a0", "3.12.dev0", "3.11.99", "4", "4.dev0", "3.011", "03.11", "3.11.00", "3.11.0_1",
		"1!3.11", "3.11+local", "3.11.0a0.post1", "3.11.0.post0", "3.11.0rc", "3.11.0c1", "3.11.0z", "3.11.0A1", "3.11.0.a1",
		"3.11.0a0.0", "3.11.0a.0", "3.11.0_", "3.11.0.0a0", "2.7", "3.11dev", "3.11.0a0dev"}
	ops := []string{"", "=", "==", "!=", ">=", ">", "<=", "<", "~="}
	globs := []string{"", "", "", ".*", "*"}
	// Samples of the series: every bound and a few neighbours that start
	// with 3.11 and are >= 3.11.0a0.
	star, err := spec.ParseMatchSpec("python 3.11.*")
	if err != nil {
		t.Fatal(err)
	}
	lo, err := spec.ParseVersion("3.11.0a0")
	if err != nil {
		t.Fatal(err)
	}
	var series []spec.Version
	var seriesText []string
	for _, b := range bounds {
		for _, suf := range []string{"", ".1", ".post1", "0", ".0", "a1", ".0.1", "rc2"} {
			v, err := spec.ParseVersion(b + suf)
			if err != nil {
				continue
			}
			r := &record.PackageRecord{Name: "python", Version: b + suf, Build: "0"}
			if star.MatchesVersion(r, v) && v.Compare(lo) >= 0 {
				series = append(series, v)
				seriesText = append(seriesText, b+suf)
			}
		}
	}
	iters := 4
	if testing.Short() {
		iters = 1
	}
	rng := rand.New(rand.NewSource(11))
	s := newIndexServer(t)
	total, dropped, kept := 0, 0, 0
	for it := 0; it < iters; it++ {
		var noarch []indexRecord
		var deps []string
		for i := 0; i < 2000; i++ {
			var vs strings.Builder
			for a, na := 0, 1+rng.Intn(2); a < na; a++ {
				if a > 0 {
					vs.WriteString("|")
				}
				for c, nc := 0, 1+rng.Intn(3); c < nc; c++ {
					if c > 0 {
						vs.WriteString(",")
					}
					vs.WriteString(ops[rng.Intn(len(ops))] + bounds[rng.Intn(len(bounds))] + globs[rng.Intn(len(globs))])
				}
			}
			dep := "python " + vs.String()
			if rng.Intn(6) == 0 {
				dep += " *_cpython"
			}
			deps = append(deps, dep)
			noarch = append(noarch, indexRecord{Name: fmt.Sprintf("p%d", i), Version: "1", Build: "0", Depends: []string{dep}})
		}
		ch := s.publish(fmt.Sprintf("oracle%d", it), nil, noarch)
		idx, err := repodata.NewFullIndex(context.Background(), ch, repodata.Options{CacheDir: t.TempDir(), Platform: "linux-64", PythonMinor: "3.11"})
		if err != nil {
			t.Fatal(err)
		}
		for i, dep := range deps {
			ms, err := spec.ParseMatchSpec(dep)
			if err != nil {
				continue
			}
			total++
			admits := ""
			for j, v := range series {
				r := &record.PackageRecord{Name: "python", Version: seriesText[j], Build: "h0_0_cpython"}
				if ms.MatchesVersion(r, v) {
					admits = seriesText[j]
					break
				}
			}
			recs, err := idx.Records(context.Background(), fmt.Sprintf("p%d", i))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case len(recs) == 1:
				kept++
			case admits != "":
				t.Errorf("%q was dropped for 3.11 but admits python %s", dep, admits)
			default:
				dropped++
			}
		}
	}
	t.Logf("%d dependencies: %d kept, %d dropped (all excluding the series)", total, kept, dropped)
	if dropped == 0 {
		t.Error("the prefilter dropped nothing")
	}
}
