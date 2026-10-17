package solve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
)

// rattlerTestChannel is where rattler's tools crate downloads its frozen
// conda-forge test repodata from (fetch_test_conda_forge_repodata).
const rattlerTestChannel = "https://rattler-test.pixi.run/test-data/channels/conda-forge"

// rattlerCondaForge loads rattler's frozen conda-forge test index (linux-64
// and noarch, about 260 MB). It is cached under the system temporary
// directory so repeated live runs download it once.
func rattlerCondaForge(t *testing.T) *repodata.FullIndex {
	t.Helper()
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	dir := filepath.Join(os.TempDir(), "goomba-solve-live", "rattler-test-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	fi, err := repodata.NewFullIndex(ctx, rattlerTestChannel, repodata.Options{
		CacheDir: dir,
		Platform: "linux-64",
		MaxAge:   365 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// subdirOnly hides the records of every subdir but one, as rattler's tests
// that load only conda-forge's linux-64 index see it.
type subdirOnly struct {
	src    repodata.Source
	subdir string
}

func (s subdirOnly) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	recs, err := s.src.Records(ctx, name)
	out := recs[:0:0]
	for _, r := range recs {
		if r.Subdir == s.subdir {
			out = append(out, r)
		}
	}
	return out, err
}

func (s subdirOnly) HasName(ctx context.Context, name string) (bool, error) {
	recs, err := s.Records(ctx, name)
	return len(recs) > 0, err
}

// rechannel presents a source's records as records of another channel, as
// rattler's tests load the frozen index as "conda-forge". It copies the
// records once.
type rechannel struct {
	src     repodata.Source
	channel string
	mu      sync.Mutex
	copies  map[*record.PackageRecord]*record.PackageRecord
}

func (s *rechannel) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	recs, err := s.src.Records(ctx, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.copies == nil {
		s.copies = map[*record.PackageRecord]*record.PackageRecord{}
	}
	out := make([]*record.PackageRecord, len(recs))
	for i, r := range recs {
		c, ok := s.copies[r]
		if !ok {
			cp := *r
			cp.Channel = s.channel
			c = &cp
			s.copies[r] = c
		}
		out[i] = c
	}
	return out, err
}

func (s *rechannel) HasName(ctx context.Context, name string) (bool, error) {
	return s.src.(repodata.NameLister).HasName(ctx, name)
}

// TestRattlerRealWorld ports the real-world cases of rattler
// crates/rattler_solve/tests/backends/main.rs (solve_real_world and the
// channel priority tests) and the ordering cases of tests/sorting.rs,
// against the same frozen conda-forge index.
func TestRattlerRealWorld(t *testing.T) {
	cf := rattlerCondaForge(t)

	t.Run("snapshots", func(t *testing.T) {
		for _, c := range []struct {
			snapshot string
			specs    []string
		}{
			{"solve_quetz", []string{"quetz"}},
			{"solve_xtensor_xsimd", []string{"xtensor", "xsimd"}},
			{"solve_tensorflow", []string{"tensorflow"}},
			{"solve_tensorboard", []string{"tensorboard=2.1.1", "grpc-cpp=1.39.1"}},
			{"solve_python", []string{"python=3.9"}},
			{"solve_python_numpy", []string{"numpy==1.23.2", "scipy==1.8.1", "python=3.9.*"}},
		} {
			t.Run(c.snapshot, func(t *testing.T) {
				start := time.Now()
				sol, err := Solve(context.Background(), cf, Request{Specs: c.specs})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%q: %d records in %v", c.specs, len(sol.Records), time.Since(start))
				var want []string
				for _, l := range strings.Split(normalizeText(snapshotBody(t, "backends__resolvo__"+c.snapshot)), "\n") {
					want = append(want, strings.TrimPrefix(l, "- "))
				}
				got := solutionLines(sol.Records)
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("solution differs from rattler's:\n%s", lineDiff(want, got))
				}
			})
		}
	})

	pytorch := repodata.NewMemory(loadChannel(t, "pytorch/linux-64/repodata.json.gz", "https://conda.anaconda.org/pytorch", "linux-64"))
	channelOf := func(t *testing.T, src repodata.Source, specStr string) (string, error) {
		t.Helper()
		sol, err := Solve(context.Background(), src, Request{Specs: []string{specStr}})
		if err != nil {
			return "", err
		}
		for _, r := range sol.Records {
			if r.Name == "pytorch-cpu" {
				return r.Channel, nil
			}
		}
		t.Fatalf("no pytorch-cpu in %s", formatRecords(sol.Records))
		return "", nil
	}
	asCondaForge := &rechannel{src: cf, channel: "https://conda.anaconda.org/conda-forge"}
	cfFirst := repodata.NewMulti(asCondaForge, pytorch)
	pytorchFirst := repodata.NewMulti(pytorch, asCondaForge)

	t.Run("channel_specific_requirement", func(t *testing.T) {
		for _, c := range []struct {
			src  repodata.Source
			spec string
			want string // "" means unsolvable
		}{
			{cfFirst, "conda-forge::pytorch-cpu", "https://conda.anaconda.org/conda-forge"},
			{pytorchFirst, "pytorch::pytorch-cpu", "https://conda.anaconda.org/pytorch"},
			// rattler selects a channel named in a spec even when strict
			// priority excludes it; goomba's source applies strict
			// priority, so pytorch's pytorch-cpu is never seen.
			{cfFirst, "pytorch::pytorch-cpu", ""},
		} {
			got, err := channelOf(t, c.src, c.spec)
			var u *Unsolvable
			switch {
			case c.want == "" && !errors.As(err, &u):
				t.Errorf("%s: expected no solution, got %q, %v", c.spec, got, err)
			case c.want != "" && got != c.want:
				t.Errorf("%s: channel %q (%v), want %q", c.spec, got, err, c.want)
			}
		}
	})

	t.Run("channel_priority_strict", func(t *testing.T) {
		if got, err := channelOf(t, cfFirst, "pytorch-cpu"); err != nil || got != "https://conda.anaconda.org/conda-forge" {
			t.Errorf("conda-forge first: %q, %v", got, err)
		}
		if got, err := channelOf(t, pytorchFirst, "pytorch-cpu"); err != nil || got != "https://conda.anaconda.org/pytorch" {
			t.Errorf("pytorch first: %q, %v", got, err)
		}
	})

	t.Run("channel_priority_strict_panic", func(t *testing.T) {
		_, err := channelOf(t, cfFirst, "pytorch-cpu=0.4.1=py36_cpu_1")
		// rattler: "pytorch-cpu ==0.4.1 py36_cpu_1 cannot be installed
		// because there are no viable options: pytorch-cpu 0.4.1 is excluded
		// because due to strict channel priority not using this option
		// from: 'https://conda.anaconda.org/pytorch/'". goomba never sees
		// pytorch's records of a name conda-forge lists.
		if got := explanation(t, err); normalizeText(got) != "No candidates were found for pytorch-cpu 0.4.1.* py36_cpu_1." {
			t.Errorf("explanation:\n%s", got)
		}
	})

	t.Run("ordering", func(t *testing.T) {
		// rattler loads only conda-forge's linux-64 index for these.
		linux64 := subdirOnly{src: cf, subdir: "linux-64"}
		for _, c := range []struct{ snapshot, spec string }{
			{"pytorch", "pytorch >=1.12.0"},
			// rattler's "python ~=3.10.*" means 3.10.*; goomba's spec
			// package reads "~=X.*" as matching nothing, as libsolv does.
			{"python", "python 3.10.*"},
			{"libuuid", "libuuid"},
			{"abess", "abess"},
			{"libgcc-ng", "libgcc-ng"},
			{"certifi", "certifi >=2016.9.26"},
		} {
			t.Run(c.snapshot, func(t *testing.T) {
				p, _, err := newProvider(linux64, Request{})
				if err != nil {
					t.Fatal(err)
				}
				ps, err := parseSpec(c.spec, p.alias)
				if err != nil {
					t.Fatal(err)
				}
				vs := p.internSpec(ps)
				cache := cdcl.NewSolverCache(p)
				sorted, err := cache.SortedCandidates(context.Background(), cdcl.SingleRequirement(vs))
				if err != nil {
					t.Fatal(err)
				}
				got := make([]string, len(sorted))
				for i, s := range sorted {
					got[i] = p.DisplaySolvable(s)
				}
				want := strings.Split(normalizeText(snapshotBody(t, "sorting__test_ordering_"+c.snapshot+"_highest")), "\n")
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("order differs from rattler's:\n%s", lineDiff(want, got))
				}
			})
		}
	})
}

// lineDiff shows two line lists side by side where they differ.
func lineDiff(want, got []string) string {
	var b strings.Builder
	n := max(len(want), len(got))
	for i := 0; i < n; i++ {
		w, g := "", ""
		if i < len(want) {
			w = want[i]
		}
		if i < len(got) {
			g = got[i]
		}
		mark := "  "
		if w != g {
			mark = "! "
		}
		b.WriteString(mark + w + "  |  " + g + "\n")
	}
	return b.String()
}
