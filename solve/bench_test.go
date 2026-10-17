package solve

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/virtual"
)

// benchMachine is the target machine of the benchmarks.
func benchMachine(b *testing.B) Request {
	b.Helper()
	virt, err := virtual.ApplyOverrides("linux-64", virtual.Defaults("linux-64"), diffMachine)
	if err != nil {
		b.Fatal(err)
	}
	return Request{Specs: []string{"python=3.11"}, Virtual: virt}
}

// BenchmarkSolvePython311FullIndex solves python=3.11 against the frozen
// conda-forge linux-64 + noarch full index ($GOOMBA_REF_ROOT), loaded once.
func BenchmarkSolvePython311FullIndex(b *testing.B) {
	f := startFrozenCondaForge(b)
	req := benchMachine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Solve(context.Background(), f.index, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSolvePython311ShardedCold opens conda-forge's sharded index with
// an empty cache and solves python=3.11 (live network): the time includes
// downloading the shard indexes and every shard the solve reaches.
func BenchmarkSolvePython311ShardedCold(b *testing.B) {
	if os.Getenv("GOOMBA_LIVE") == "" {
		b.Skip("set GOOMBA_LIVE=1")
	}
	req := benchMachine(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		ch, err := repodata.NewShardedChannel(ctx, "https://conda.anaconda.org/conda-forge", repodata.Options{
			CacheDir:    b.TempDir(),
			Platform:    "linux-64",
			PythonMinor: RequestedPythonMinor(req.Specs),
		})
		if err != nil {
			cancel()
			b.Fatal(err)
		}
		sol, err := Solve(ctx, ch, req)
		cancel()
		if err != nil {
			b.Fatal(err)
		}
		if i == 0 {
			st := ch.Stats()
			b.Logf("%d packages; shard stats %+v", len(sol.Records), st)
		}
	}
}
