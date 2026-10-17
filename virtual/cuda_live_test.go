package virtual

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDetectCUDALive runs the real detection on a machine with nvidia-smi at
// a built-in location and compares __cuda and __cuda_arch with the tool's
// own output, parsed here independently of the package's parsers.
func TestDetectCUDALive(t *testing.T) {
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	host := HostPlatform()
	if !strings.HasPrefix(host, "linux-") && !strings.HasPrefix(host, "win-") {
		t.Skipf("goomba detects CUDA only on Linux and Windows, not %s", host)
	}
	prog, ok := hostProbe{}.findNvidiaSMI("")
	if !ok {
		t.Skip("no nvidia-smi at a built-in location")
	}
	smi := prog.path
	ctx := context.Background()
	num := func(b []byte) int {
		n, err := strconv.Atoi(string(b))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Errors are ignored: nvidia-smi may exit non-zero with usable output.
	xml, _ := exec.CommandContext(ctx, smi, "--query", "-u", "-x").Output()
	wantCuda := ""
	if m := regexp.MustCompile(`<cuda_version>\s*([0-9]+)\.([0-9]+)\s*</cuda_version>`).FindSubmatch(xml); m != nil {
		wantCuda = fmt.Sprintf("%d.%d", num(m[1]), num(m[2]))
	}
	csv, _ := exec.CommandContext(ctx, smi, "--query-gpu=compute_cap", "--format=csv,noheader").Output()
	wantArch := ""
	minMajor, minMinor := -1, -1
	line := regexp.MustCompile(`^\s*([0-9]+)\.([0-9]+)[a-zA-Z]*\s*$`)
	for _, l := range strings.Split(string(csv), "\n") {
		if m := line.FindStringSubmatch(l); m != nil {
			major, minor := num([]byte(m[1])), num([]byte(m[2]))
			if minMajor < 0 || major < minMajor || (major == minMajor && minor < minMinor) {
				minMajor, minMinor = major, minor
			}
		}
	}
	if minMajor >= 0 && wantCuda != "" {
		wantArch = fmt.Sprintf("%d.%d", minMajor, minMinor)
	}
	t.Logf("%s reports CUDA %q and minimum compute capability %q", smi, wantCuda, wantArch)

	check := func(label string, opt Options) {
		t.Helper()
		recs, err := Detect(ctx, opt)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		t.Logf("%s: detected %q", label, triples(recs))
		for _, x := range []struct{ name, want string }{{"__cuda", wantCuda}, {"__cuda_arch", wantArch}} {
			r := find(recs, x.name)
			switch {
			case x.want == "" && r != nil:
				t.Errorf("%s: %s = %+v, want absent", label, x.name, *r)
			case x.want != "" && (r == nil || r.Version != x.want || r.Build != "0"):
				t.Errorf("%s: %s = %v, want version %s build 0", label, x.name, r, x.want)
			}
		}
	}
	check("built-in location", Options{})
	check("explicit path", Options{NvidiaSMI: prog.name})

	recs, err := Detect(ctx, Options{Overrides: map[string]string{"cuda": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if find(recs, "__cuda") != nil || find(recs, "__cuda_arch") != nil {
		t.Errorf("with __cuda removed: %q", triples(recs))
	}
}
