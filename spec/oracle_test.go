package spec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

// oracleVersionList holds the versions of package foo in the oracle
// channel: ordering corner cases from the libmamba and conda tests.
var oracleVersionList = []string{
	"0.1", "0.5", "0.9", "1.0", "1.0.0", "1.0.1", "1.0a1", "1.0.dev1", "1.0.post1", "1.0+local",
	"1.0.1_", "1.0.1a", "1.0.1rc1", "1.1", "1.6", "1.6.7", "1.7", "1.7.0.1", "1.7.1", "1.7.7",
	"1.7a", "1.8", "1.8.1", "1.8alpha", "1.8post", "1.9", "1.10", "1.11.0", "1.26.0", "1.26.1",
	"1.27", "2.0", "2.0.1", "2.0.3", "2.1", "2.2", "2.2.0", "2.2.1", "2.3", "2.3.1", "2.6.8",
	"2.7", "2.7.2", "2.7.12", "2.8", "3", "3.0", "3.0.0", "3.3", "3.3.1", "3.3.2", "3.3.2.0",
	"3.3.3", "3.3.4", "3.4", "3.4a", "3.4.1", "3.5", "3.6", "3.9", "3.10", "3.11", "3.11.0",
	"3.11.15", "3.12.0", "3.12.1", "3.13.1", "4.0", "6.33", "10.1", "11", "2013a", "2013b",
	"2013k", "1!0.4.1", "1!2.0", "2.1.0.1.1.3", "2.0.1.0.1.1.3", "0.3.4", "1.2.3+4.5.6",
	"0.16.6", "0.17.6", "0.17.21", "0.18.1", "2023.1.0", "2024.8.1", "2025.1", "v0.9.24",
	// From the WP1 review's probes.
	"0.23.0", "0.23.1", "0.5.p0", "1.0rc1", "1.0.0rc1", "1.0post1", "3.3rc1", "3.6.0rc1", "3.6.0",
	"3.6.1", "3.7", "4.0rc1", "7.1.0r", "7.1", "7.2", "1.2", "1.2.3", "1.20", "9.0.0.0", "9",
	"1.0a", "1.0alpha",
}

// oracleSpecList holds version and build specs from the libmamba tests and
// shapes found in real repodata, written after "foo ".
var oracleSpecList = []string{
	"", "*", "1.7", "==1.7", "!=1.7", "<1.7", "<=1.7", ">1.7", ">= 1.7", " = 1.8", "=1.8.*",
	"1.8.*", "!=1.8.*", "1.*.3", "=1.*.3", "!=1.*.3", "~=1.8", "~=1", "(>=1.7,<1.8)|>=1.9.0.0",
	">2013b", ">1.0.0a", "1.0*", "1.0.0*", "2013a*", "1.2.3*", "1.2.3+4*", "1.7.*|1.8.*",
	">1.7,<1.8", "1.5.*|>1.7,<1.8", "=3.3", "3.3.*", "!=3.3", ">=2.7,!=3.0.*,!=3.1.*,!=3.2.*,!=3.3.*",
	"~=1.10", "~=1.10.0", "~=3.3.2", "~=3.3.2|==2.2", "*.*", "2.*.1.1.*", "*.3", ">=10.0,*.*",
	"~=1.26.0", "~=1.26", "=3.*", "=3.*.1", "=*.13.1", ">=3|<1,<0.9", "<0.9,<1|>=3", ">=0.1|>=3,<1",
	">=0.17.6,<0.18|0.16.6.*", "==0.16.*.*|>=0.17.6,<0.18", ">=1.0.0,<1.0.1", "<1.0.1_", ">1.0.1_",
	"2.0|2.2,>2.1", "1!0.4.*", ">=1!0.0", "1.0+local", "=v0.9.24", "v0.9.24", ">=2.7,<3|>=3.4",
	"*.0", "!=*.0", "3.11.* *_cp311", ">=3.11 *_0", "* b1*", "* h*_2", "*[build_number='>=2']",
	"*[build_number=3]", "=2.0=b1_0", ">=1,<3=*_3",
}

// oracleKnownSearch lists the specs where micromamba 2.2.0's repoquery
// search is known to disagree with this package, and why.
var oracleKnownSearch = map[string]string{
	"~=1":                     "libsolv (used for simple specs) matches nothing for a single-part ~=; libmamba and rattler match every version >= 1",
	"!=1.*.3":                 "micromamba 2.2.0 reads it as not-equal to the version 1.*.3; libmamba 2.9 and goomba as a negated glob",
	"*.*":                     "micromamba 2.2.0 matches this glob across epochs and local versions; libmamba 2.9 requires them to match",
	">=10.0,*.*":              "same as *.*",
	"*.0":                     "same as *.*",
	"!=*.0":                   "same as *.*",
	">=1.0.0,<1.0.1":          `libsolv ignores local versions and sorts "1.0.1_" after "1.0.1a", unlike conda's VersionOrder`,
	"<1.0.1_":                 "same as >=1.0.0,<1.0.1",
	">1.0.1_":                 "same as >=1.0.0,<1.0.1",
	">=3|<1,<0.9":             "command-line specs go through libmamba's parser, where | binds tighter; repodata goes through libsolv (see the dependency checks)",
	"<0.9,<1|>=3":             "same as >=3|<1,<0.9",
	">=0.1|>=3,<1":            "same as >=3|<1,<0.9",
	">=0.17.6,<0.18|0.16.6.*": "same as >=3|<1,<0.9",
	"2.0|2.2,>2.1":            "same as >=3|<1,<0.9",
	"*[build_number='>=2']":   "repoquery search ignores build_number; the solver honours it",
	"*[build_number=3]":       "same as *[build_number='>=2']",
}

// oracleProbes are dependency lists of probe packages. micromamba parses
// repodata dependencies with libsolv, so solving for a probe shows which
// foo libsolv accepts: the highest version satisfying all of them.
var oracleProbes = [][]string{
	{"foo >=3|<1,<0.9"},
	{"foo <0.9,<1|>=3"},
	{"foo >=0.1|>=3,<1"},
	{"foo 1.0|2.0,3.0"},
	{"foo >=0.17.6,<0.18|0.16.6.*", "foo <0.17"},
	{"foo *.3"},
	{"foo 2.*.1.1.*"},
	{"foo ~=1.8", "foo <2"},
	{"foo 1.8.*"},
	{"foo !=1.8.*", "foo <1.9"},
	{"foo ~=1", "foo <2"},
	{"foo 1.0.0*", "foo <1.0"},
	// From the WP1 review: relational operators with ".*" and starts-with.
	{"foo ==3.*"},
	{"foo ==3.6.*"},
	{"foo >=3.6.*", "foo <3.6.0"},
	{"foo >0.23.*", "foo <0.23.1"},
	{"foo <=3.6.*", "foo >=3.6.0"},
	{"foo >=7.1.*", "foo <7.1"},
	{"foo >=0.5.*", "foo <0.5"},
	{"foo >=3.0,<4.*"},
	{"foo <4.*"},
	{"foo ~=3.6.*"},
	{"foo =1.0.0", "foo <1.0.0rc1"},
	{"foo 1.0.0.*", "foo <1.0.0rc1"},
	{"foo =1.0a"},
	{"foo 1.0a*", "foo >1.0a"},
	{"foo >=1.0,<2|3.6.*", "foo >=3"},
	{"foo !=3.6.*", "foo >=3.6.0rc1", "foo <3.7"},
	{"foo 9.0.0.*"},
	{"foo 1.2|1.20,<1.10"},
	{"foo >=1.0.*,<=2.*"},
	{"foo >=3.6.*", "foo <3.7"},
}

// oracleKnownProbes lists probes where libsolv is known to disagree.
var oracleKnownProbes = map[string]string{
	"foo ~=1 & foo <2":      oracleKnownSearch["~=1"],
	"foo 1.0.0* & foo <1.0": "libsolv ignores local versions; conda and goomba sort 1.0+local below 1.0, so it is the best match",
}

// TestMicromambaOracle compares this package with micromamba: MatchSpec
// against `micromamba repoquery search` on a local file channel for every
// spec in oracleSpecList, and the solver's choice for every probe in
// oracleProbes. Disagreements listed in oracleKnownSearch and
// oracleKnownProbes are logged, others fail. It runs only when
// GOOMBA_MICROMAMBA names a micromamba binary.
func TestMicromambaOracle(t *testing.T) {
	bin := os.Getenv("GOOMBA_MICROMAMBA")
	if bin == "" {
		t.Skip("set GOOMBA_MICROMAMBA to a micromamba binary")
	}
	// The scratch root prefix must live inside the working tree; tmp/ at the
	// module root is ignored by git.
	base, err := filepath.Abs(filepath.Join("..", "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "spec-oracle-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	type rec struct {
		Name        string   `json:"name"`
		Version     string   `json:"version"`
		Build       string   `json:"build"`
		BuildNumber uint64   `json:"build_number"`
		Depends     []string `json:"depends"`
		Subdir      string   `json:"subdir"`
		MD5         string   `json:"md5"`
		SHA256      string   `json:"sha256"`
		Size        int      `json:"size"`
	}
	builds := []string{"b1_0", "h12_1", "py39_2", "0_cp311", "h7_3", "b1x_0"}
	pkgs := map[string]rec{}
	var records []*record.PackageRecord
	for i, v := range oracleVersionList {
		b := builds[i%len(builds)]
		bn := uint64(b[len(b)-1] - '0')
		pkgs[fmt.Sprintf("foo-%d-%s.tar.bz2", i, b)] = rec{Name: "foo", Version: v, Build: b, BuildNumber: bn,
			Depends: []string{}, Subdir: "noarch", MD5: fmt.Sprintf("%032x", i), SHA256: fmt.Sprintf("%064x", i), Size: 1}
		records = append(records, &record.PackageRecord{Name: "foo", Version: v, Build: b, BuildNumber: bn, Subdir: "noarch"})
	}
	for i, deps := range oracleProbes {
		name := fmt.Sprintf("probe%d", i)
		pkgs[name+"-1-0.tar.bz2"] = rec{Name: name, Version: "1", Build: "0", Depends: deps, Subdir: "noarch",
			MD5: strings.Repeat("f", 32), SHA256: strings.Repeat("f", 64), Size: 1}
	}
	repodata, err := json.Marshal(map[string]any{"info": map[string]string{"subdir": "noarch"}, "packages": pkgs, "packages.conda": map[string]rec{}})
	if err != nil {
		t.Fatal(err)
	}
	chanDir := filepath.Join(dir, "chan")
	if err := os.MkdirAll(filepath.Join(chanDir, "noarch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chanDir, "noarch", "repodata.json"), repodata, 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")

	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		args = append(args, "--no-rc", "-r", root, "--override-channels", "-c", "file://"+chanDir,
			"--platform", "linux-64", "--json")
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = []string{"MAMBA_ROOT_PREFIX=" + root, "HOME=" + dir, "PATH=/usr/bin:/bin"}
		return cmd.Output()
	}
	parallel := func(n int, f func(i int)) {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				f(i)
			}(i)
		}
		wg.Wait()
	}

	// Part 1: repoquery search.
	type found struct {
		set map[string]bool
		err error
	}
	searches := make([]found, len(oracleSpecList))
	parallel(len(oracleSpecList), func(i int) {
		spec := oracleSpecQuery(oracleSpecList[i])
		out, err := run("repoquery", "search", spec)
		if err != nil {
			searches[i] = found{err: fmt.Errorf("micromamba search %q: %v: %s", spec, err, out)}
			return
		}
		var res struct {
			Result struct {
				Pkgs []struct {
					Version string `json:"version"`
					Build   string `json:"build"`
				} `json:"pkgs"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			searches[i] = found{err: fmt.Errorf("micromamba search %q: %v", spec, err)}
			return
		}
		set := map[string]bool{}
		for _, p := range res.Result.Pkgs {
			set[p.Version+"="+p.Build] = true
		}
		searches[i] = found{set: set}
	})
	for i, s := range oracleSpecList {
		if searches[i].err != nil {
			t.Error(searches[i].err)
			continue
		}
		m, err := ParseMatchSpec(oracleSpecQuery(s))
		if err != nil {
			t.Errorf("ParseMatchSpec(%q): %v", oracleSpecQuery(s), err)
			continue
		}
		var onlyMicromamba, onlyGoomba []string
		for _, r := range records {
			key := r.Version + "=" + r.Build
			switch mine := m.Matches(r); {
			case mine && !searches[i].set[key]:
				onlyGoomba = append(onlyGoomba, key)
			case !mine && searches[i].set[key]:
				onlyMicromamba = append(onlyMicromamba, key)
			}
		}
		if len(onlyMicromamba)+len(onlyGoomba) == 0 {
			if why, ok := oracleKnownSearch[s]; ok {
				t.Errorf("%q: listed as a known difference (%s) but micromamba now agrees", s, why)
			}
			continue
		}
		sort.Strings(onlyMicromamba)
		sort.Strings(onlyGoomba)
		msg := fmt.Sprintf("search %q: only micromamba matches %v; only goomba matches %v", oracleSpecQuery(s), onlyMicromamba, onlyGoomba)
		if why, ok := oracleKnownSearch[s]; ok {
			t.Logf("known: %s (%s)", msg, why)
		} else {
			t.Error(msg)
		}
	}

	// Part 2: solve for each probe package.
	chosen := make([]found, len(oracleProbes))
	parallel(len(oracleProbes), func(i int) {
		out, _ := run("create", "-y", "--dry-run", "-p", filepath.Join(root, "envs", "probe"), fmt.Sprintf("probe%d", i))
		var res struct {
			Actions struct {
				Link []struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"LINK"`
			} `json:"actions"`
		}
		set := map[string]bool{}
		if err := json.Unmarshal(out, &res); err == nil {
			for _, p := range res.Actions.Link {
				if p.Name == "foo" {
					set[p.Version] = true
				}
			}
		}
		chosen[i] = found{set: set}
	})
	for i, deps := range oracleProbes {
		var specs []MatchSpec
		for _, d := range deps {
			specs = append(specs, mustMatchSpec(t, d))
		}
		var best *Version
		for _, r := range records {
			ok := true
			for _, m := range specs {
				ok = ok && m.Matches(r)
			}
			if v := mustVersion(t, r.Version); ok && (best == nil || v.Compare(*best) > 0) {
				best = &v
			}
		}
		want := "nothing"
		if best != nil {
			want = best.String()
		}
		got := "nothing"
		for v := range chosen[i].set {
			got = v
		}
		agree := (best == nil && got == "nothing") || (best != nil && got != "nothing" && mustVersion(t, got).Compare(*best) == 0)
		key := strings.Join(deps, " & ")
		msg := fmt.Sprintf("probe depending on %q: micromamba picks foo %s, goomba allows at most %s", key, got, want)
		why, known := oracleKnownProbes[key]
		switch {
		case agree && known:
			t.Errorf("%s: listed as a known difference (%s) but they agree", key, why)
		case !agree && known:
			t.Logf("known: %s (%s)", msg, why)
		case !agree:
			t.Error(msg)
		}
	}
}

// oracleSpecQuery turns an oracleSpecList entry into a spec for foo.
func oracleSpecQuery(s string) string {
	if strings.HasPrefix(s, "*[") {
		return "foo" + s[1:]
	}
	return "foo " + s
}
