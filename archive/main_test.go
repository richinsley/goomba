package archive

import (
	"flag"
	"os"
	"testing"
)

// TestMain bounds fuzz input minimization to 1000 runs unless
// -test.fuzzminimizetime is given. Every extraction touches the filesystem
// (about 2 ms), and with Go's default of 60 s per newly interesting input
// the workers of a 60 s fuzz run spend nearly all their time minimizing.
func TestMain(m *testing.M) {
	flag.Parse()
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test.fuzzminimizetime" {
			set = true
		}
	})
	if !set {
		_ = flag.Set("test.fuzzminimizetime", "1000x")
	}
	os.Exit(m.Run())
}
