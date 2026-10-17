package solve

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
)

// FuzzSolveSmallChannel decodes random bytes into a tiny channel and a
// request (see genCase), solves it, and checks the answer with checkCase:
// Solve never panics or fails internally, every solution passes Validate,
// and every unsolvable answer is confirmed by brute force.
func FuzzSolveSmallChannel(f *testing.F) {
	// Seeds: the byte streams that drive genCase to a few shapes, plus
	// random streams.
	f.Add([]byte{})
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{2, 1, 2, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	f.Add([]byte("a >=2; if __v\x00b[extras=[x]]\x01c 1.*"))
	f.Add([]byte{4, 4, 4, 4, 4, 3, 3, 3, 3, 2, 2, 2, 1, 1, 1, 0, 0, 0, 16, 32, 64, 128, 255})
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 16; i++ {
		b := make([]byte, 64+rng.Intn(192))
		rng.Read(b)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		recs, req := genCase(&byteSource{data: data})
		checkCase(t, recs, req)
	})
}

// FuzzRequestedPythonMinor checks that RequestedPythonMinor never panics
// and returns "" or MAJOR.MINOR, for up to three specs separated by
// newlines. Seeds are the cases of TestRequestedPythonMinor.
func FuzzRequestedPythonMinor(f *testing.F) {
	for _, s := range []string{
		"", "numpy >=1.0\nopenssl 3", "python 3.12.5", "python 3.7.12 0_73_pypy", "python ==3.11",
		"python=3.13", "python=2", "python >=3.12,<3.13", "numpy 1.0\npython 3.10.0\npython 3.11",
		"python", "python=1!3.11", "python=3.11+local", "python=3.11a", "python[[\npython=3.12",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		specs := strings.SplitN(s, "\n", 3)
		got := RequestedPythonMinor(specs)
		if got != "" && !isMajorMinor(got) {
			t.Fatalf("RequestedPythonMinor(%q) = %q", specs, got)
		}
	})
}

// FuzzFoldConflictLeaves checks that folding never panics, never adds
// lines and is idempotent. Seeds are the cases of TestFoldConflictLeaves.
func FuzzFoldConflictLeaves(f *testing.F) {
	leaf := ", which conflicts with the versions reported above.\n"
	for _, s := range []string{
		"", "No candidates were found for foo *.\n",
		"├─ foo 2" + leaf + "├─ foo 1.0" + leaf + "└─ foo 1" + leaf,
		"│  ├─ foo 2" + leaf + "│  ├─ foo 1" + leaf + "│  ├─ bar 1" + leaf + "│  │  ├─ bar 4" + leaf + "│  └─ bar 6" + leaf,
		"   ├─ q 1 | 2 | ... | 9" + leaf + "   └─ q 10" + leaf,
		"├─ foo 1..2" + leaf + "└─ foo 1" + strings.TrimSuffix(leaf, "\n"),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := foldConflictLeaves(s)
		if strings.Count(once, "\n") > strings.Count(s, "\n") {
			t.Fatalf("folding added lines:\n%s\n->\n%s", s, once)
		}
		if twice := foldConflictLeaves(once); twice != once {
			t.Fatalf("folding is not idempotent:\n%s\n->\n%s\n->\n%s", s, once, twice)
		}
	})
}

// FuzzSanitize checks that sanitize's output is always clean text and that
// sanitizing twice changes nothing.
func FuzzSanitize(f *testing.F) {
	for _, s := range []string{"", "plain", "bar\x1b[2J\n\xe2\x94\x94\xe2\x94\x80 x", "\xe2\x80\xae", "\xff\xfe", "e\xcc\x81\xcc\x81\xcc\x81\xcc\x81\xcc\x81",
		"\xe3\x85\xa4\xe2\xa0\x80\xef\xbe\xa0", "back\\slash \\x1b", "\xf3\xa0\x80\x81"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := sanitize(s)
		if !isCleanText(out) || strings.Contains(out, "\n") {
			t.Fatalf("sanitize(%q) = %q is not clean", s, out)
		}
		if again := sanitize(out); again != out {
			t.Fatalf("sanitize(%q) = %q, then %q", s, out, again)
		}
	})
}

// FuzzSolveSanitizes (from the WP7 review's FuzzReviewSanitize) checks that
// every error and explanation Solve or Validate returns is clean text,
// whatever the hostile fields of records and requests contain, and that
// solutions carry lower-case digests.
func FuzzSolveSanitizes(f *testing.F) {
	f.Add("1.0\x1b[2J", "b\xe2\x80\xae", "bar\n└─ x", "https://x/\x1b", "z::foo\x07", "__v", "1\x00", "foo>=2", "linux-64\x1b", "foo-1.0-b.conda\n")
	f.Add("1", "0", "bar; if __win", "https://conda.anaconda.org/y", "y::foo", "__cuda", "12\xe2\x80\xa8", "foo", "noarch", "x")
	f.Add("1", "0", "bar", "https://conda.anaconda.org/x", "y::foo", "__glibc", "2.17", "foo", "\xff", "\\x1b")
	f.Fuzz(func(t *testing.T, version, build, dep, channel, rootSpec, vname, vversion, constraint, subdir, fn string) {
		if len(version)+len(build)+len(dep)+len(channel)+len(rootSpec)+len(vname)+len(vversion)+len(constraint)+len(subdir)+len(fn) > 2000 {
			return
		}
		mk := func(name, v, b string, deps ...string) *record.PackageRecord {
			r := pkg(name).version(v).build(b).depends(deps...).rec()
			r.Channel, r.Subdir = channel, subdir
			if fn != "" {
				r.Fn = fn
			}
			return r
		}
		recs := []*record.PackageRecord{
			mk("foo", version, build, dep),
			mk("foo", "2", build, "bar >=3"),
			mk("bar", version, "0"),
			mk("foo", version, build+"x"),
		}
		recs[3].Constrains = []string{dep}
		recs[3].SHA256 = strings.ToUpper(dummySHA256)
		weird := pkg(strings.ToLower(vname)).version(version).build(build).rec()
		recs = append(recs, weird)
		multi := repodata.NewMulti(repodata.NewMemory(recs[:2]), repodata.NewMemory(recs[2:]))
		reqs := []Request{
			{Specs: []string{rootSpec}},
			{Specs: []string{"foo", rootSpec}, Constraints: []string{constraint}},
			{Specs: []string{"foo>=2"}, Virtual: []*record.PackageRecord{virtualPkg(vname, vversion, build)}},
			{Specs: []string{"foo"}, Locked: []*record.PackageRecord{mk("foo", version, build+"l")}},
			{Specs: []string{"foo >=9"}, Favored: []*record.PackageRecord{mk("foo", vversion, build)}},
			{Specs: []string{"bar", "foo"}, ChannelAlias: channel},
		}
		for _, src := range []repodata.Source{repodata.NewMemory(recs), multi} {
			for _, req := range reqs {
				sol, err := Solve(context.Background(), src, req)
				if err != nil {
					if !isCleanText(err.Error()) {
						t.Fatalf("Solve error not clean: %q", err.Error())
					}
					var u *Unsolvable
					if errors.As(err, &u) {
						if !isCleanText(u.Explanation) {
							t.Fatalf("explanation not clean: %q", u.Explanation)
						}
					}
					continue
				}
				for _, r := range sol.Records {
					if r.SHA256 != strings.ToLower(r.SHA256) {
						t.Fatalf("solution record with upper-case sha256")
					}
				}
			}
			if err := Validate(reqs[1], recs); err != nil && !isCleanText(err.Error()) {
				t.Fatalf("Validate error not clean: %q", err.Error())
			}
			if err := Validate(reqs[2], recs[:1]); err != nil && !isCleanText(err.Error()) {
				t.Fatalf("Validate error not clean: %q", err.Error())
			}
		}
	})
}
