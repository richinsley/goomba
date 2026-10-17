// Helpers ported from rattler crates/rattler_solve/tests/backends/helpers/
// (package_builder.rs, solver_case.rs) and main.rs (BSD-3-Clause, see
// NOTICE).

package solve

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
)

// Digests of rattler's dummy_md5_hash and dummy_sha256_hash.
const (
	dummyMD5    = "b3af409bb8423187c75e6c7f5b683908"
	dummySHA256 = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
)

// builder is rattler's PackageBuilder: a record with sensible defaults.
type builder struct{ r record.PackageRecord }

func pkg(name string) builder {
	b := builder{r: record.PackageRecord{
		Name:    name,
		Version: "0.0.0",
		Build:   "h123456_0",
		Subdir:  "linux-64",
		MD5:     dummyMD5,
		SHA256:  dummySHA256,
		URL:     "http://example.com",
	}}
	b.r.Fn = b.r.Dist() + ".conda"
	return b
}

func (b builder) version(v string) builder {
	b.r.Version = v
	b.r.Fn = b.r.Dist() + ".conda"
	return b
}

func (b builder) build(s string) builder {
	b.r.Build = s
	b.r.Fn = b.r.Dist() + ".conda"
	return b
}

func (b builder) buildNumber(n uint64) builder { b.r.BuildNumber = n; return b }

func (b builder) depends(d ...string) builder {
	b.r.Depends = append([]string(nil), d...)
	return b
}

func (b builder) constrains(c ...string) builder {
	b.r.Constrains = append([]string(nil), c...)
	return b
}

func (b builder) channel(c string) builder { b.r.Channel = c; return b }

func (b builder) subdir(s string) builder { b.r.Subdir = s; return b }

func (b builder) trackFeatures(tf ...string) builder {
	b.r.TrackFeatures = append([]string(nil), tf...)
	return b
}

func (b builder) sha256(s string) builder { b.r.SHA256 = s; return b }

func (b builder) tarBz2() builder {
	b.r.Fn = b.r.Dist() + ".tar.bz2"
	return b
}

// timestamp takes an RFC 3339 time.
func (b builder) timestamp(ts string) builder {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	b.r.Timestamp = t.UnixMilli()
	return b
}

// rec returns a fresh copy of the record.
func (b builder) rec() *record.PackageRecord {
	r := b.r
	return &r
}

// installedPackage is rattler's installed_package helper.
func installedPackage(channel, subdir, name, version, build string, buildNumber uint64) *record.PackageRecord {
	return pkg(name).channel(channel).subdir(subdir).version(version).build(build).buildNumber(buildNumber).rec()
}

// virtualPkg returns a virtual package record.
func virtualPkg(name, version, build string) *record.PackageRecord {
	return &record.PackageRecord{Name: name, Version: version, Build: build, Channel: "@"}
}

// matcher selects records of a solution, like rattler's PkgMatcher.
type matcher struct {
	name, version, build string
	exact                *record.PackageRecord
}

func (m matcher) String() string {
	if m.exact != nil {
		return displayRecord(m.exact) + " from " + m.exact.Channel
	}
	s := m.name
	if m.version != "" {
		s += "=" + m.version
	}
	if m.build != "" {
		s += "=" + m.build
	}
	return s
}

func (m matcher) matches(r *record.PackageRecord) bool {
	if m.exact != nil {
		e := m.exact
		return r.Name == e.Name && r.Version == e.Version && r.Build == e.Build &&
			r.BuildNumber == e.BuildNumber && r.Channel == e.Channel && r.Subdir == e.Subdir && r.Fn == e.Fn
	}
	return r.Name == m.name && (m.version == "" || r.Version == m.version) && (m.build == "" || r.Build == m.build)
}

func name(n string) matcher                 { return matcher{name: n} }
func nv(n, v string) matcher                { return matcher{name: n, version: v} }
func nvb(n, v, b string) matcher            { return matcher{name: n, version: v, build: b} }
func exact(r *record.PackageRecord) matcher { return matcher{exact: r} }

// solverCase is rattler's SolverCase: repositories (highest priority
// first, combined with strict channel priority by repodata.Multi), a
// request and the expected outcome.
type solverCase struct {
	name        string
	repos       [][]*record.PackageRecord
	specs       []string
	constraints []string
	favored     []*record.PackageRecord
	locked      []*record.PackageRecord
	virtual     []*record.PackageRecord
	present     []matcher
	absent      []matcher
	unsolvable  bool
}

func source(repos ...[]*record.PackageRecord) repodata.Source {
	if len(repos) == 1 {
		return repodata.NewMemory(repos[0])
	}
	srcs := make([]repodata.Source, len(repos))
	for i, r := range repos {
		srcs[i] = repodata.NewMemory(r)
	}
	return repodata.NewMulti(srcs...)
}

func (c solverCase) request() Request {
	return Request{Specs: c.specs, Constraints: c.constraints, Favored: c.favored, Locked: c.locked, Virtual: c.virtual}
}

func (c solverCase) run(t *testing.T) {
	t.Helper()
	sol, err := Solve(context.Background(), source(c.repos...), c.request())
	if c.unsolvable {
		var u *Unsolvable
		switch {
		case err == nil:
			t.Fatalf("case %q: expected no solution, found %s", c.name, formatRecords(sol.Records))
		case !errors.As(err, &u):
			t.Fatalf("case %q: expected no solution, but the solver failed: %v", c.name, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("case %q failed:\n%v", c.name, err)
	}
	t.Logf("case %q: solution = [%s]", c.name, formatRecords(sol.Records))
	for _, m := range c.present {
		if !anyMatch(sol.Records, m) {
			t.Errorf("case %q: expected %s to be present, found %s", c.name, m, formatRecords(sol.Records))
		}
	}
	for _, m := range c.absent {
		if anyMatch(sol.Records, m) {
			t.Errorf("case %q: expected %s to be absent, found %s", c.name, m, formatRecords(sol.Records))
		}
	}
}

func anyMatch(recs []*record.PackageRecord, m matcher) bool {
	for _, r := range recs {
		if m.matches(r) {
			return true
		}
	}
	return false
}

func formatRecords(recs []*record.PackageRecord) string {
	if len(recs) == 0 {
		return "<empty>"
	}
	parts := make([]string, len(recs))
	for i, r := range recs {
		parts[i] = displayRecord(r)
		if r.Channel != "" {
			parts[i] += " from " + r.Channel
		}
	}
	return strings.Join(parts, ", ")
}

// loadChannel loads a repodata.json (or .json.gz) fixture from testdata.
func loadChannel(t testing.TB, rel, channelURL, subdir string) []*record.PackageRecord {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "channels", rel))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(rel, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		r = gz
	}
	recs, err := repodata.LoadRepodataJSON(r, channelURL, subdir)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// dummyChannel is rattler's dummy test channel, loaded as conda-forge.
func dummyChannel(t testing.TB) []*record.PackageRecord {
	return loadChannel(t, "dummy/linux-64/repodata.json", "https://conda.anaconda.org/conda-forge", "linux-64")
}

// solveDummy solves against the dummy channel, like rattler's solve helper.
func solveDummy(t *testing.T, req Request) (*Solution, error) {
	t.Helper()
	return Solve(context.Background(), repodata.NewMemory(dummyChannel(t)), req)
}

// snapshotBody returns the body of testdata/snapshots/<name>.snap.
func snapshotBody(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "snapshots", name+".snap"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	first := strings.Index(s, "---\n")
	rest := s[first+4:]
	second := strings.Index(rest, "\n---\n")
	if first < 0 || second < 0 {
		t.Fatalf("snapshot %s: bad header", name)
	}
	return rest[second+5:]
}

// normalizeText trims trailing whitespace and surrounding blank lines.
func normalizeText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// rattlerPrefix starts rattler's SolveError::Unsolvable text.
const rattlerPrefix = "Cannot solve the request because of: "

// explanation returns the explanation of an Unsolvable error.
func explanation(t *testing.T, err error) string {
	t.Helper()
	var u *Unsolvable
	if !errors.As(err, &u) {
		t.Fatalf("expected *Unsolvable, got %v", err)
	}
	return u.Explanation
}

// solutionLines formats a solution like rattler's solve_real_world: sorted
// "name version build" lines.
func solutionLines(recs []*record.PackageRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		if r.Build == "" {
			out[i] = r.Name + " " + r.Version
		} else {
			out[i] = r.Name + " " + r.Version + " " + r.Build
		}
	}
	sort.Strings(out)
	return out
}
