package virtual

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/record"
)

// Cases from rattler crates/rattler_virtual_packages/src/cuda.rs
// (test_parse_nvidia_smi_cuda_version), then goomba's hostile cases.
func TestParseNvidiaSMICudaVersion(t *testing.T) {
	el := func(v string) string { return "<cuda_version>" + v + "</cuda_version>" }
	cases := []struct {
		in, want string // want "" = no version
	}{
		{"<nvidia_smi_log>\n  <cuda_version>12.4</cuda_version>\n</nvidia_smi_log>", "12.4"},
		{"<nvidia_smi_log></nvidia_smi_log>", ""},
		{"", ""},

		// Normalised to major.minor.
		{el("12"), "12.0"},
		{el("12.4.1"), "12.4"},
		{el("012.04"), "12.4"},
		{el("12.10"), "12.10"},
		{el("1.0"), "1.0"},
		{el("99.99"), "99.99"},
		{el("12.4.1.0"), ""},
		{el("12."), ""},
		{el(".4"), ""},
		{el("12..4"), ""},
		{el(""), ""},
		{el("12.4a"), ""},
		{el("1e2.0"), ""},
		{el("１２.4"), ""},
		{el("1234567890.0"), ""},
		{el(strings.Repeat("9", 70000)), ""},
		// Out of range.
		{el("99999.1"), ""},
		{el("100.0"), ""},
		{el("0.0"), ""},
		{el("0.9"), ""},
		{el("12.100"), ""},
		// Negative or signed numbers.
		{el("-1.0"), ""},
		{el("-12.4"), ""},
		{el("12.-4"), ""},
		{el("+12.4"), ""},
		// Not available.
		{el("N/A"), ""},
		{el("[N/A]"), ""},
		{el("Not Found"), ""},
		// Whitespace around the version is XML formatting; inside it is not.
		{el(" 12.4 "), "12.4"},
		{el("\n\t12.4\r\n"), "12.4"},
		{el("12 .4"), ""},
		{el("12.4\x00"), ""},
		// Several elements must agree.
		{el("12.4") + "\n" + el("12.4"), "12.4"},
		{el("12.4") + el("12.04"), "12.4"},
		{el("12.4") + "\n" + el("11.8"), ""},
		{el("12.4") + "\n" + el("N/A"), ""},
		{el("N/A") + "\n" + el("12.4"), ""},
		// Malformed markup.
		{"<cuda_version>12.4", ""},
		{el("12.4") + "<cuda_version>12.4", ""},
		{"<cuda_version><cuda_version>12.4</cuda_version></cuda_version>", ""},
		{"<CUDA_VERSION>12.4</CUDA_VERSION>", ""},
		{"<cuda_version >12.4</cuda_version>", ""},
		{"</cuda_version>12.4<cuda_version>", ""},
		{"<cuda_version/>" + el("12.4"), "12.4"},
		// An element in a comment still counts, and must agree (rattler
		// takes the first).
		{el("12.4") + "\n<!-- " + el("11.8") + " -->", ""},
		{el("12.4") + "\n<!-- " + el("12.4") + " -->", "12.4"},
		// Binary noise around a valid element.
		{"\x00\xff\xfe" + el("12.4") + "\x80\x00", "12.4"},
		{"\x00\xff\xfe\x80\x01", ""},
		// Huge output and many elements.
		{strings.Repeat("x", 1<<20) + el("12.4"), "12.4"},
		{strings.Repeat(el("12.4")+"\n", 5000), "12.4"},
		{strings.Repeat(el("12.4")+"\n", 5000) + el("12.5"), ""},
		{strings.Repeat("<driver_version>580.173.02</driver_version>\n", 5000) + el("13.0"), "13.0"},
	}
	for _, c := range cases {
		got, ok := parseNvidiaSMICudaVersion([]byte(c.in))
		if got != c.want || ok != (c.want != "") {
			t.Errorf("parseNvidiaSMICudaVersion(%.80q) = %q, %v; want %q", c.in, got, ok, c.want)
		}
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/cuda.rs
// (test_parse_nvidia_smi_compute_capabilities), then goomba's hostile cases.
func TestParseNvidiaSMIComputeCap(t *testing.T) {
	cases := []struct {
		in, want string // want "" = no result
	}{
		{"8.6\n7.5\n9.0", "7.5"},
		{"8.6\n[N/A]\n7.5\ngarbage", "7.5"},
		{"x.y\n8.0", "8.0"},
		{"[N/A]\ngarbage\n", ""},
		{"", ""},

		// Sub-architecture letters are dropped.
		{"8.6a", "8.6"},
		{"9.0a", "9.0"},
		{"9.0a\n12.0", "9.0"},
		{"9.0a\n8.9", "8.9"},
		{"10.0f\n12.1", "10.0"},
		{"9.0A", "9.0"},
		{"8a.6", ""},
		{"8.a", ""},
		{"8.6 a", ""},
		{"8.6a6", ""},
		{"eight.six", ""},
		// Whitespace and line endings.
		{"  8.6  \r\n\t7.5\t\r\n", "7.5"},
		{"8.6\r\n7.5\r\n", "7.5"},
		{"\u00a08.6\u00a0", "8.6"},
		{"\n\n\n8.6\n\n", "8.6"},
		// Not available.
		{"N/A", ""},
		{"[N/A]", ""},
		{"[N/A]\n[N/A]\n8.6", "8.6"},
		{"8.6\n[Unknown Error]\n[GPU requires reset]", "8.6"},
		{"Not Supported\n8.6", "8.6"},
		{"9.0abc", "9.0"},
		{"07.5", "7.5"},
		{"7.+5", ""},
		// Not a major.minor pair, or out of range: skipped.
		{"12", ""},
		{"12.4.1", ""},
		{"12.4.1\n8.6", "8.6"},
		{"99999.1", ""},
		{"99999.1\n8.6", "8.6"},
		{"100.0", ""},
		{"0.5", ""},
		{"0.5\n8.6", "8.6"},
		{"8.100", ""},
		{"1234567890.1", ""},
		{"-7.5", ""},
		{"-7.5\n8.6", "8.6"},
		{"+7.5", ""},
		{"7.-5", ""},
		{"7,5", ""},
		{"7.5, 8.6", ""},
		{"8.6\x00", ""},
		// Two-digit minors and leading zeros.
		{"8.10\n8.9", "8.9"},
		{"08.06", "8.6"},
		{"12.0\n12.00", "12.0"},
		// A header line (the query without noheader) is skipped.
		{"compute_cap\n12.0\n8.6\n7.5\n", "7.5"},
		// Many lines, binary noise and a huge line.
		{strings.Repeat("8.6\n", 10000) + "7.5", "7.5"},
		{strings.Repeat("garbage\n", 10000), ""},
		{strings.Repeat("9", 1<<20) + ".0\n8.6", "8.6"},
		{"\x00\xff\n7.5\n\xfe\x80", "7.5"},
		{"\x00\xff\xfe\x80", ""},
	}
	for _, c := range cases {
		got, ok := parseNvidiaSMIComputeCap([]byte(c.in))
		if got != c.want || ok != (c.want != "") {
			t.Errorf("parseNvidiaSMIComputeCap(%.80q) = %q, %v; want %q", c.in, got, ok, c.want)
		}
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/cuda.rs and lib.rs
// (test_is_valid_cuda_version_format; the two lists are identical).
func TestIsValidCudaVersionFormat(t *testing.T) {
	for _, s := range []string{"8.6", "7.5", "10.2", "0.0", "12.0"} {
		if !isValidCudaVersionFormat(s) {
			t.Errorf("isValidCudaVersionFormat(%q) = false", s)
		}
	}
	for _, s := range []string{
		"8", "8.6.1", "8.6.1.0", "", ".6", "8.", ".",
		"8.6a", "a.6", "8.b", "eight.six", "8-6", "8_6",
	} {
		if isValidCudaVersionFormat(s) {
			t.Errorf("isValidCudaVersionFormat(%q) = true", s)
		}
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/lib.rs
// (test_parse_cuda_arch, CudaArch::parse_version without the environment
// variable part), applied to one line of nvidia-smi output.
func TestParseComputeCap(t *testing.T) {
	for _, c := range []struct {
		in           string
		major, minor int
	}{{"8.6", 8, 6}, {"7.5", 7, 5}, {"9.0", 9, 0}} {
		if major, minor, ok := parseComputeCap(c.in); !ok || major != c.major || minor != c.minor {
			t.Errorf("parseComputeCap(%q) = %d, %d, %v", c.in, major, minor, ok)
		}
	}
	for _, s := range []string{"invalid", "8", "8.6.1"} {
		if _, _, ok := parseComputeCap(s); ok {
			t.Errorf("parseComputeCap(%q) succeeded", s)
		}
	}
}

// The range rattler enforces on the driver API's version integer
// (test_parse_cuda_driver_version: CUDA majors 1 to 99), applied to the
// version nvidia-smi prints.
func TestNormalizeCudaVersionRange(t *testing.T) {
	for in, want := range map[string]string{"12.4": "12.4", "11.8": "11.8", "1.0": "1.0", "99.99": "99.99"} {
		if got, ok := normalizeCudaVersion(in); !ok || got != want {
			t.Errorf("normalizeCudaVersion(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"0.0", "0.999", "100.0", "2147483647.0", "-1.0"} {
		if got, ok := normalizeCudaVersion(in); ok {
			t.Errorf("normalizeCudaVersion(%q) = %q, want none", in, got)
		}
	}
}

// The fixtures are real nvidia-smi output; see testdata/nvidia-smi/README.
func TestNvidiaSMIFixtures(t *testing.T) {
	xml, err := os.ReadFile(filepath.Join("testdata", "nvidia-smi", "query-unit.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := parseNvidiaSMICudaVersion(xml); !ok || v != "13.0" {
		t.Errorf("query-unit.xml: %q, %v; want 13.0", v, ok)
	}
	caps, err := os.ReadFile(filepath.Join("testdata", "nvidia-smi", "compute-cap.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := parseNvidiaSMIComputeCap(caps); !ok || v != "7.5" {
		t.Errorf("compute-cap.csv: %q, %v; want 7.5", v, ok)
	}
	// Each output is useless to the other parser.
	if v, ok := parseNvidiaSMICudaVersion(caps); ok {
		t.Errorf("compute-cap.csv as XML = %q", v)
	}
	if v, ok := parseNvidiaSMIComputeCap(xml); ok {
		t.Errorf("query-unit.xml as CSV = %q", v)
	}
}

const (
	testSMI      = "/usr/bin/nvidia-smi"
	runVersion   = testSMI + " --query -u -x"
	runArch      = testSMI + " --query-gpu=compute_cap --format=csv,noheader"
	xmlCuda124   = "<nvidia_smi_log>\n  <cuda_version>12.4</cuda_version>\n</nvidia_smi_log>"
	capsThreeGPU = "8.6\n7.5\n9.0\n"
)

// gpuHost is a host with nvidia-smi at a system location reporting CUDA
// 12.4 and GPUs of compute capability 8.6, 7.5 and 9.0.
func gpuHost(plat string) fakeProbe {
	return fakeProbe{
		plat: plat, release: "6.8.0", glibc: "2.35", win: "10.0.26100", arch: "x86_64_v3",
		smi: testSMI, xml: xmlCuda124, caps: capsThreeGPU,
	}
}

func TestDetectCUDA(t *testing.T) {
	with := func(p fakeProbe, edit func(*fakeProbe)) fakeProbe { edit(&p); return p }
	cases := []struct {
		name               string
		probe              fakeProbe
		ov                 map[string]string
		wantCuda, wantArch string // "" = absent
		wantRuns           []string
	}{
		{"linux GPU", gpuHost("linux-64"), nil, "12.4", "7.5", []string{runVersion, runArch}},
		{"linux-aarch64 GPU", gpuHost("linux-aarch64"), nil, "12.4", "7.5", []string{runVersion, runArch}},
		{"windows GPU", gpuHost("win-64"), nil, "12.4", "7.5", []string{runVersion, runArch}},
		{"macOS never probes", gpuHost("osx-arm64"), nil, "", "", nil},
		{"freebsd never probes", gpuHost("freebsd-64"), nil, "", "", nil},
		{"no nvidia-smi", with(gpuHost("linux-64"), func(p *fakeProbe) { p.smi = "" }), nil, "", "", nil},
		{"version N/A skips the arch query", with(gpuHost("linux-64"), func(p *fakeProbe) { p.xml = "<cuda_version>N/A</cuda_version>" }),
			nil, "", "", []string{runVersion}},
		{"driver not loaded", with(gpuHost("linux-64"), func(p *fakeProbe) {
			p.xml = "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n"
		}), nil, "", "", []string{runVersion}},
		{"version run fails", with(gpuHost("linux-64"), func(p *fakeProbe) { p.failXML = true }), nil, "", "", []string{runVersion}},
		{"arch N/A", with(gpuHost("linux-64"), func(p *fakeProbe) { p.caps = "[N/A]\n" }), nil, "12.4", "", []string{runVersion, runArch}},
		{"no GPUs", with(gpuHost("linux-64"), func(p *fakeProbe) { p.caps = "" }), nil, "12.4", "", []string{runVersion, runArch}},
		{"old driver without compute_cap", with(gpuHost("linux-64"), func(p *fakeProbe) {
			p.caps = "Field \"compute_cap\" is not a valid field to query.\n"
		}), nil, "12.4", "", []string{runVersion, runArch}},
		{"arch run fails", with(gpuHost("linux-64"), func(p *fakeProbe) { p.failCaps = true }), nil, "12.4", "", []string{runVersion, runArch}},

		// Overrides: an overridden package is not probed.
		{"cuda removed", gpuHost("linux-64"), map[string]string{"cuda": ""}, "", "", nil},
		{"cuda removed, upper case", gpuHost("win-64"), map[string]string{"CUDA": ""}, "", "", nil},
		{"cuda overridden", gpuHost("linux-64"), map[string]string{"cuda": "11.8"}, "11.8", "7.5", []string{runArch}},
		{"cuda overridden, no nvidia-smi", with(gpuHost("linux-64"), func(p *fakeProbe) { p.smi = "" }),
			map[string]string{"cuda": "11.8"}, "11.8", "", nil},
		{"cuda overridden on macOS", gpuHost("osx-64"), map[string]string{"cuda": "11.8"}, "11.8", "", nil},
		{"arch overridden", gpuHost("linux-64"), map[string]string{"cuda_arch": "8.0"}, "12.4", "8.0", []string{runVersion}},
		{"arch removed", gpuHost("linux-64"), map[string]string{"cuda_arch": ""}, "12.4", "", []string{runVersion}},
		{"both overridden", gpuHost("linux-64"), map[string]string{"cuda": "12.0", "cuda_arch": "8.6"}, "12.0", "8.6", nil},
		{"both removed", gpuHost("linux-64"), map[string]string{"cuda": "", "cuda_arch": ""}, "", "", nil},
		{"arch override dropped without a GPU", with(gpuHost("linux-64"), func(p *fakeProbe) { p.smi = "" }),
			map[string]string{"cuda_arch": "8.6"}, "", "", nil},
		{"arch override dropped when the version is unknown", with(gpuHost("linux-64"), func(p *fakeProbe) { p.xml = "" }),
			map[string]string{"cuda_arch": "8.6"}, "", "", []string{runVersion}},
		{"arch override on macOS without cuda", gpuHost("osx-arm64"), map[string]string{"cuda_arch": "8.6"}, "", "", nil},
	}
	for _, c := range cases {
		var runs []string
		p := c.probe
		p.runs = &runs
		recs := mustDetect(t, p, c.ov)
		for _, x := range []struct{ name, want string }{{"__cuda", c.wantCuda}, {"__cuda_arch", c.wantArch}} {
			r := find(recs, x.name)
			switch {
			case x.want == "" && r != nil:
				t.Errorf("%s: %s = %+v, want absent", c.name, x.name, *r)
			case x.want != "" && r == nil:
				t.Errorf("%s: %s absent, want %s", c.name, x.name, x.want)
			case r != nil && (r.Version != x.want || r.Build != "0" || r.Channel != "@" || r.Subdir != p.plat):
				t.Errorf("%s: %s = %+v, want version %s build 0", c.name, x.name, *r, x.want)
			}
		}
		if !reflect.DeepEqual(runs, c.wantRuns) {
			t.Errorf("%s: ran %q, want %q", c.name, runs, c.wantRuns)
		}
		for i := 1; i < len(recs); i++ {
			if recs[i-1].Name >= recs[i].Name {
				t.Errorf("%s: records not sorted: %q", c.name, triples(recs))
			}
		}
	}
}

func TestDetectCUDAFullLinux(t *testing.T) {
	got := triples(mustDetect(t, gpuHost("linux-64"), nil))
	want := []string{"__archspec 1 x86_64_v3", "__cuda 12.4 0", "__cuda_arch 7.5 0", "__glibc 2.35 0", "__linux 6.8.0 0", "__unix 0 0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detect = %q, want %q", got, want)
	}
	got = triples(mustDetect(t, gpuHost("win-64"), nil))
	want = []string{"__archspec 1 x86_64_v3", "__cuda 12.4 0", "__cuda_arch 7.5 0", "__win 10.0.26100 0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detect = %q, want %q", got, want)
	}
	// A detected set replays exactly, like any other.
	recs := mustDetect(t, gpuHost("linux-64"), nil)
	m := ToOverrides(recs)
	if err := CheckRecorded("linux-64", m); err != nil {
		t.Fatal(err)
	}
	if again, err := ApplyOverrides("linux-64", nil, m); err != nil || !reflect.DeepEqual(again, recs) {
		t.Errorf("replay = %q, %v; want %q", triples(again), err, triples(recs))
	}
}

func TestDetectNvidiaSMIOption(t *testing.T) {
	bad := []struct{ plat, path string }{
		{"linux-64", "nvidia-smi"},
		{"linux-64", "bin/nvidia-smi"},
		{"linux-64", "./nvidia-smi"},
		{"linux-64", "/usr/bin/nvidia-smi\x00x"},
		{"osx-arm64", "nvidia-smi"},
		{"win-64", "nvidia-smi.exe"},
		{"win-64", `Tools\nvidia-smi.exe`},
		{"win-64", `C:nvidia-smi.exe`},
		{"win-64", `C:\Tools\nvidia-smi`},
		{"win-64", `C:\Tools\nvidia-smi.exe.txt`},
		{"win-64", `C:\Tools\.exe`},
		{"win-64", `C:\Tools.exe\`},
		{"win-64", "/usr/bin/nvidia-smi"},
	}
	for _, c := range bad {
		var runs []string
		p := gpuHost(c.plat)
		p.runs = &runs
		// The path is checked even when nothing would run.
		_, err := detect(context.Background(), Options{NvidiaSMI: c.path, Overrides: map[string]string{"cuda": ""}}, p)
		if !errors.Is(err, ErrInvalidOption) || !strings.Contains(err.Error(), fmt.Sprintf("%q", c.path)) {
			t.Errorf("%s NvidiaSMI %q: err = %v, want ErrInvalidOption naming the path", c.plat, c.path, err)
		}
		if len(runs) != 0 {
			t.Errorf("%s NvidiaSMI %q: ran %q", c.plat, c.path, runs)
		}
	}

	good := []struct{ plat, path string }{
		{"linux-64", "/opt/nvidia/bin/nvidia-smi"},
		{"linux-64", "/usr/bin/nvidia-smi"},
		{"win-64", `C:\Tools\nvidia-smi.exe`},
		{"win-64", `D:/tools/NVIDIA-SMI.EXE`},
		{"win-64", `\\server\share\nvidia-smi.exe`},
		{"win-64", `\\?\C:\Tools\nvidia-smi.exe`},
	}
	for _, c := range good {
		var runs []string
		p := gpuHost(c.plat)
		p.runs = &runs
		recs, err := detect(context.Background(), Options{NvidiaSMI: c.path}, p)
		if err != nil {
			t.Errorf("%s NvidiaSMI %q: %v", c.plat, c.path, err)
			continue
		}
		if find(recs, "__cuda") == nil || find(recs, "__cuda_arch") == nil {
			t.Errorf("%s NvidiaSMI %q: %q", c.plat, c.path, triples(recs))
		}
		want := []string{c.path + " --query -u -x", c.path + " --query-gpu=compute_cap --format=csv,noheader"}
		if !reflect.DeepEqual(runs, want) {
			t.Errorf("%s NvidiaSMI %q: ran %q, want %q", c.plat, c.path, runs, want)
		}
	}

	// A chosen path that is not a regular file is an error on Linux and
	// Windows, even when nothing would run: a mistyped path must not quietly
	// mean "no GPU". The system locations are not tried instead.
	var runs []string
	for _, c := range []struct {
		plat, path string
		ov         map[string]string
	}{
		{"linux-64", "/opt/missing/nvidia-smi", nil},
		{"linux-64", "/opt/missing/nvidia-smi", map[string]string{"cuda": ""}},
		{"linux-64", "/opt/missing/nvidia-smi", map[string]string{"cuda": "12.4", "cuda_arch": "8.6"}},
		{"win-64", `C:\Tools\nvidia-smi.exe`, nil},
	} {
		p := gpuHost(c.plat)
		p.runs, p.explicitMissing = &runs, true
		recs, err := detect(context.Background(), Options{NvidiaSMI: c.path, Overrides: c.ov}, p)
		if !errors.Is(err, ErrInvalidOption) || !strings.Contains(err.Error(), "not a regular file") || recs != nil {
			t.Errorf("%s missing NvidiaSMI %q with %v: %q, %v; want ErrInvalidOption", c.plat, c.path, c.ov, triples(recs), err)
		}
	}
	if len(runs) != 0 {
		t.Errorf("missing NvidiaSMI: ran %q", runs)
	}
	var p fakeProbe

	// On macOS an absolute path is accepted and ignored, even a missing one.
	runs = nil
	p = gpuHost("osx-arm64")
	p.runs, p.explicitMissing = &runs, true
	if recs, err := detect(context.Background(), Options{NvidiaSMI: "/usr/local/bin/nvidia-smi"}, p); err != nil || find(recs, "__cuda") != nil || len(runs) != 0 {
		t.Errorf("macOS NvidiaSMI: %q, %v, ran %q", triples(recs), err, runs)
	}
}

func TestDetectCUDACancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs []string
	p := gpuHost("linux-64")
	p.runs, p.onRun = &runs, cancel
	if _, err := detect(ctx, Options{}, p); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(runs, []string{runVersion}) {
		t.Errorf("ran %q after cancellation, want only the first query", runs)
	}
}

// linkChain makes /usr/bin/nvidia-smi reach a trusted
// /usr/lib/nvidia/nvidia-smi through exactly n symbolic links.
func linkChain(f *fakeFS, n int) {
	f.addUnix("/usr/lib/nvidia/nvidia-smi", rootExe())
	f.addUnix("/usr/bin/placeholder", rootExe())
	if n == 1 {
		f.links["/usr/bin/nvidia-smi"] = "/usr/lib/nvidia/nvidia-smi"
		return
	}
	f.links["/usr/bin/nvidia-smi"] = "/usr/lib/l1" // link 1
	for i := 1; i < n-1; i++ {                     // links 2 to n-1
		f.links[fmt.Sprintf("/usr/lib/l%d", i)] = fmt.Sprintf("l%d", i+1)
	}
	f.links[fmt.Sprintf("/usr/lib/l%d", n-1)] = "nvidia/nvidia-smi" // link n
}

// fakeFS is a file system of absolute paths for the program searches.
// Unix paths may pass through symbolic links; Windows paths (with
// backslashes) are looked up literally.
type fakeFS struct {
	files  map[string]fileFacts // real paths of files and directories
	links  map[string]string    // real path of a link -> its target
	owners map[string]uint32    // owner of a link; root if absent
	dirs   map[string][]string  // directory -> entry names
	stats  *int                 // if set, counts stat calls
	lstats *int                 // if set, counts lstat calls
	reads  *int                 // if set, counts readDirNames calls
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files: map[string]fileFacts{}, links: map[string]string{},
		owners: map[string]uint32{}, dirs: map[string][]string{},
	}
}

func rootDir() fileFacts            { return fileFacts{mode: fs.ModeDir | 0o755} }
func rootExe() fileFacts            { return fileFacts{mode: 0o755} }
func userFile(uid uint32) fileFacts { return fileFacts{mode: 0o755, uid: uid} }
func winFile(mtime int64) fileFacts {
	return fileFacts{mode: 0o666, uid: 1<<32 - 1, modTime: time.Unix(mtime, 0)}
}
func dirWith(m fs.FileMode) fileFacts { return fileFacts{mode: fs.ModeDir | m} }

// addUnix adds a Unix file with root-owned 0755 ancestor directories that
// are not already present.
func (f *fakeFS) addUnix(p string, facts fileFacts) {
	f.files[p] = facts
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		if _, ok := f.files[d]; !ok {
			f.files[d] = rootDir()
		}
		if d == "/" {
			break
		}
	}
}

// resolve follows every link in a Unix path, as the kernel does.
func (f *fakeFS) resolve(name string) (string, error) {
	if !strings.HasPrefix(name, "/") {
		return name, nil
	}
	rest := splitUnixPath(name)
	cur := ""
	for hops := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		if c == ".." {
			cur = cur[:strings.LastIndexByte(cur, '/')+1]
			cur = strings.TrimSuffix(cur, "/")
			continue
		}
		next := cur + "/" + c
		if target, ok := f.links[next]; ok {
			if hops++; hops > 100 {
				return "", errors.New("too many levels of symbolic links")
			}
			if strings.HasPrefix(target, "/") {
				cur = ""
			}
			rest = append(splitUnixPath(target), rest...)
			continue
		}
		cur = next
	}
	if cur == "" {
		return "/", nil
	}
	return cur, nil
}

func (f *fakeFS) stat(name string) (fileFacts, error) {
	if f.stats != nil {
		*f.stats++
	}
	real, err := f.resolve(name)
	if err != nil {
		return fileFacts{}, err
	}
	fi, ok := f.files[real]
	if !ok {
		return fileFacts{}, fs.ErrNotExist
	}
	return fi, nil
}

func (f *fakeFS) lstat(name string) (fileFacts, error) {
	if f.lstats != nil {
		*f.lstats++
	}
	if _, ok := f.links[name]; ok {
		return fileFacts{mode: fs.ModeSymlink | 0o777, uid: f.owners[name]}, nil
	}
	fi, ok := f.files[name]
	if !ok {
		return fileFacts{}, fs.ErrNotExist
	}
	return fi, nil
}

func (f *fakeFS) readlink(name string) (string, error) {
	target, ok := f.links[name]
	if !ok {
		return "", fs.ErrInvalid
	}
	return target, nil
}

func (f *fakeFS) readDirNames(dir string, max int) ([]string, error) {
	if f.reads != nil {
		*f.reads++
	}
	names, ok := f.dirs[dir]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if len(names) > max {
		names = names[:max]
	}
	return names, nil
}

func TestUnixNvidiaSMISearch(t *testing.T) {
	builtin := unixNvidiaSMIPaths()
	if want := []string{
		"/usr/bin/nvidia-smi", "/usr/lib/wsl/lib/nvidia-smi",
		"/usr/local/nvidia/bin/nvidia-smi", "/run/current-system/sw/bin/nvidia-smi",
	}; !reflect.DeepEqual(builtin, want) {
		t.Fatalf("unixNvidiaSMIPaths = %q, want %q", builtin, want)
	}
	cases := []struct {
		name  string
		setup func(f *fakeFS)
		want  string // "" = none
	}{
		{"distribution package", func(f *fakeFS) { f.addUnix("/usr/bin/nvidia-smi", rootExe()) }, "/usr/bin/nvidia-smi"},
		{"read-only file", func(f *fakeFS) { f.addUnix("/usr/bin/nvidia-smi", fileFacts{mode: 0o555}) }, "/usr/bin/nvidia-smi"},
		{"first location wins", func(f *fakeFS) {
			f.addUnix("/usr/lib/wsl/lib/nvidia-smi", rootExe())
			f.addUnix("/usr/bin/nvidia-smi", rootExe())
		}, "/usr/bin/nvidia-smi"},
		{"WSL2", func(f *fakeFS) { f.addUnix("/usr/lib/wsl/lib/nvidia-smi", fileFacts{mode: 0o555}) }, "/usr/lib/wsl/lib/nvidia-smi"},
		{"container", func(f *fakeFS) { f.addUnix("/usr/local/nvidia/bin/nvidia-smi", rootExe()) }, "/usr/local/nvidia/bin/nvidia-smi"},
		{"NixOS store behind symlinks with a sticky group-writable store", func(f *fakeFS) {
			f.addUnix("/nix/store/ccc-nvidia-x11/bin/nvidia-smi", fileFacts{mode: 0o555})
			f.addUnix("/nix/store/bbb-sw/bin/placeholder", fileFacts{mode: 0o444})
			f.addUnix("/nix/store/aaa-system/placeholder", fileFacts{mode: 0o444})
			f.files["/nix/store"] = dirWith(fs.ModeSticky | 0o775)
			f.files["/run"] = rootDir()
			f.links["/run/current-system"] = "/nix/store/aaa-system"
			f.links["/nix/store/aaa-system/sw"] = "/nix/store/bbb-sw"
			f.links["/nix/store/bbb-sw/bin/nvidia-smi"] = "/nix/store/ccc-nvidia-x11/bin/nvidia-smi"
		}, "/nix/store/ccc-nvidia-x11/bin/nvidia-smi"},
		{"symlink to a trusted file", func(f *fakeFS) {
			f.addUnix("/usr/lib/nvidia/bin/nvidia-smi", rootExe())
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.links["/usr/bin/nvidia-smi"] = "/usr/lib/nvidia/bin/nvidia-smi"
		}, "/usr/lib/nvidia/bin/nvidia-smi"},
		{"relative symlink", func(f *fakeFS) {
			f.addUnix("/usr/lib/nvidia/nvidia-smi", rootExe())
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.links["/usr/bin/nvidia-smi"] = "../lib/./nvidia//nvidia-smi"
		}, "/usr/lib/nvidia/nvidia-smi"},
		{"relative symlink climbing past the root", func(f *fakeFS) {
			f.addUnix("/usr/lib/nvidia/nvidia-smi", rootExe())
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.links["/usr/bin/nvidia-smi"] = "../../../../usr/lib/nvidia/nvidia-smi"
		}, "/usr/lib/nvidia/nvidia-smi"},
		{"root's link in a sticky world-writable directory", func(f *fakeFS) {
			f.files["/usr"] = rootDir()
			f.files["/usr/local"] = dirWith(fs.ModeSticky | 0o777)
			f.addUnix("/opt/nv/bin/nvidia-smi", rootExe())
			f.links["/usr/local/nvidia"] = "/opt/nv"
		}, "/opt/nv/bin/nvidia-smi"},
		{"40 links, the most followed", func(f *fakeFS) { linkChain(f, maxLinkHops) }, "/usr/lib/nvidia/nvidia-smi"},
		{"1 link", func(f *fakeFS) { linkChain(f, 1) }, "/usr/lib/nvidia/nvidia-smi"},
		{"skip an untrusted file for a trusted one", func(f *fakeFS) {
			f.addUnix("/usr/bin/nvidia-smi", userFile(1000))
			f.addUnix("/usr/local/nvidia/bin/nvidia-smi", rootExe())
		}, "/usr/local/nvidia/bin/nvidia-smi"},

		{"nothing installed", func(f *fakeFS) {}, ""},
		{"owned by a user", func(f *fakeFS) { f.addUnix("/usr/bin/nvidia-smi", userFile(1000)) }, ""},
		{"group-writable", func(f *fakeFS) { f.addUnix("/usr/bin/nvidia-smi", fileFacts{mode: 0o775}) }, ""},
		{"world-writable", func(f *fakeFS) { f.addUnix("/usr/bin/nvidia-smi", fileFacts{mode: 0o757}) }, ""},
		{"a directory", func(f *fakeFS) { f.addUnix("/usr/bin/nvidia-smi", rootDir()) }, ""},
		{"a device", func(f *fakeFS) { f.addUnix("/usr/bin/nvidia-smi", fileFacts{mode: fs.ModeDevice | 0o755}) }, ""},
		{"in a group-writable directory", func(f *fakeFS) {
			f.files["/usr/local"] = dirWith(fs.ModeSetgid | 0o775)
			f.addUnix("/usr/local/nvidia/bin/nvidia-smi", rootExe())
		}, ""},
		{"in a world-writable directory", func(f *fakeFS) {
			f.files["/usr/local/nvidia/bin"] = dirWith(0o777)
			f.addUnix("/usr/local/nvidia/bin/nvidia-smi", rootExe())
		}, ""},
		{"in a user's directory", func(f *fakeFS) {
			f.files["/usr/local/nvidia"] = fileFacts{mode: fs.ModeDir | 0o755, uid: 1000}
			f.addUnix("/usr/local/nvidia/bin/nvidia-smi", rootExe())
		}, ""},
		{"symlink into a user's directory", func(f *fakeFS) {
			f.files["/opt/evil"] = fileFacts{mode: fs.ModeDir | 0o755, uid: 1000}
			f.addUnix("/opt/evil/nvidia-smi", rootExe())
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.links["/usr/bin/nvidia-smi"] = "/opt/evil/nvidia-smi"
		}, ""},
		{"trusted file reached through a user's directory", func(f *fakeFS) {
			f.addUnix("/usr/lib/nvidia/bin/nvidia-smi", rootExe())
			f.files["/usr/local/nvidia"] = fileFacts{mode: fs.ModeDir | 0o755, uid: 1000}
			f.addUnix("/usr/local/nvidia/placeholder", rootExe())
			f.links["/usr/local/nvidia/bin"] = "/usr/lib/nvidia/bin"
		}, ""},
		// The two review findings: the program exec re-resolves must be the
		// one checked, so every link on the way counts.
		{"intermediate link in a user's directory", func(f *fakeFS) {
			f.addUnix("/usr/lib/nvidia/nvidia-smi", rootExe())
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.files["/srv/alice"] = fileFacts{mode: fs.ModeDir | 0o755, uid: 1000}
			f.addUnix("/srv/alice/placeholder", userFile(1000))
			f.links["/usr/bin/nvidia-smi"] = "/srv/alice/nvidia-smi"
			f.links["/srv/alice/nvidia-smi"] = "/usr/lib/nvidia/nvidia-smi"
		}, ""},
		{"user's link in a sticky world-writable directory", func(f *fakeFS) {
			f.files["/usr"] = rootDir()
			f.files["/usr/local"] = dirWith(fs.ModeSticky | 0o777)
			f.addUnix("/opt/nv/bin/nvidia-smi", rootExe())
			f.links["/usr/local/nvidia"] = "/opt/nv"
			f.owners["/usr/local/nvidia"] = 1000
		}, ""},
		{"user's directory in a sticky world-writable directory", func(f *fakeFS) {
			f.files["/usr/local"] = dirWith(fs.ModeSticky | 0o777)
			f.files["/usr/local/nvidia"] = fileFacts{mode: fs.ModeDir | 0o755, uid: 1000}
			f.addUnix("/usr/local/nvidia/bin/nvidia-smi", rootExe())
		}, ""},
		{"41 links", func(f *fakeFS) { linkChain(f, maxLinkHops+1) }, ""},
		{"non-root link in a sticky group-writable directory", func(f *fakeFS) {
			f.files["/usr"] = rootDir()
			f.files["/usr/local"] = dirWith(fs.ModeSticky | 0o775) // like /nix/store
			f.addUnix("/opt/nv/bin/nvidia-smi", rootExe())
			f.links["/usr/local/nvidia"] = "/opt/nv"
			f.owners["/usr/local/nvidia"] = 30001 // a nixbld user
		}, ""},
		{"group-writable root directory", func(f *fakeFS) {
			f.addUnix("/usr/bin/nvidia-smi", rootExe())
			f.files["/"] = dirWith(0o775)
		}, ""},
		{"root directory owned by a user", func(f *fakeFS) {
			f.addUnix("/usr/bin/nvidia-smi", rootExe())
			f.files["/"] = fileFacts{mode: fs.ModeDir | 0o755, uid: 1000}
		}, ""},
		{"link to a directory", func(f *fakeFS) {
			f.addUnix("/usr/lib/nvidia/placeholder", rootExe())
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.links["/usr/bin/nvidia-smi"] = "/usr/lib/nvidia"
		}, ""},
		{"dangling link", func(f *fakeFS) {
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.links["/usr/bin/nvidia-smi"] = "/usr/lib/missing"
		}, ""},
		{"empty link", func(f *fakeFS) {
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.addUnix("/usr/bin", rootDir())
			f.links["/usr/bin/nvidia-smi"] = ""
		}, ""},
		{"symlink loop", func(f *fakeFS) {
			f.addUnix("/usr/bin/placeholder", rootExe())
			f.links["/usr/bin/nvidia-smi"] = "/usr/bin/nvidia-smi2"
			f.links["/usr/bin/nvidia-smi2"] = "/usr/bin/nvidia-smi"
		}, ""},
		{"parent is not a directory", func(f *fakeFS) {
			f.addUnix("/usr/bin/nvidia-smi", rootExe())
			f.files["/usr"] = rootExe()
		}, ""},
	}
	for _, c := range cases {
		f := newFakeFS()
		c.setup(f)
		got, ok := unixNvidiaSMI(f, "", builtin)
		if got.path != c.want || ok != (c.want != "") {
			t.Errorf("%s: unixNvidiaSMI = %+v, %v; want %q", c.name, got, ok, c.want)
		}
		// The program runs under the name of the location it was found at.
		if ok && !slices.Contains(builtin, got.name) {
			t.Errorf("%s: unixNvidiaSMI = %+v: name is not a built-in location", c.name, got)
		}
	}
	// A multi-call binary reached through a link keeps the link's name.
	f := newFakeFS()
	f.addUnix("/usr/lib/busybox", rootExe())
	f.addUnix("/usr/bin/placeholder", rootExe())
	f.links["/usr/bin/nvidia-smi"] = "../lib/busybox"
	if got, ok := unixNvidiaSMI(f, "", builtin); !ok || got != (program{path: "/usr/lib/busybox", name: "/usr/bin/nvidia-smi"}) {
		t.Errorf("multi-call binary: %+v, %v", got, ok)
	}

	// A machine without NVIDIA tools pays one stat per location.
	stats, lstats := 0, 0
	f = newFakeFS()
	f.stats, f.lstats = &stats, &lstats
	f.addUnix("/usr/bin/env", rootExe())
	if got, ok := unixNvidiaSMI(f, "", builtin); ok || stats != len(builtin) || lstats != 0 {
		t.Errorf("no nvidia-smi: %q, %v after %d stats and %d lstats, want %d and 0", got, ok, stats, lstats, len(builtin))
	}

	// A caller-chosen path need only be a regular file, and only it is tried.
	f = newFakeFS()
	f.addUnix("/usr/bin/nvidia-smi", rootExe())
	f.addUnix("/work/tools/nvidia-smi", fileFacts{mode: 0o777, uid: 1000})
	f.files["/work"] = fileFacts{mode: fs.ModeDir | 0o777, uid: 1000}
	f.addUnix("/work/dir", dirWith(0o755))
	for _, c := range []struct{ explicit, want string }{
		{"/work/tools/nvidia-smi", "/work/tools/nvidia-smi"},
		{"/work/dir", ""},
		{"/work/missing", ""},
	} {
		got, ok := unixNvidiaSMI(f, c.explicit, builtin)
		if ok != (c.want != "") || (ok && got != (program{path: c.want, name: c.want})) {
			t.Errorf("explicit %q: %q, %v; want %q", c.explicit, got, ok, c.want)
		}
	}
}

func TestWindowsNvidiaSMISearch(t *testing.T) {
	const (
		sys   = `C:\Windows\System32`
		pf    = `C:\Program Files`
		store = sys + `\DriverStore\FileRepository`
		nvsmi = pf + `\NVIDIA Corporation\NVSMI\nvidia-smi.exe`
	)
	withDriver := func(f *fakeFS) { f.files[sys+`\nvcuda.dll`] = winFile(1) }
	cases := []struct {
		name    string
		sys, pf string
		setup   func(f *fakeFS)
		want    string // "" = none
	}{
		{"system directory", sys, pf, func(f *fakeFS) {
			f.files[sys+`\nvidia-smi.exe`] = winFile(1)
			withDriver(f)
			f.dirs[store] = []string{"nv_dispi.inf_amd64_aaa"}
			f.files[store+`\nv_dispi.inf_amd64_aaa\nvidia-smi.exe`] = winFile(9)
			f.files[nvsmi] = winFile(9)
		}, sys + `\nvidia-smi.exe`},
		{"system directory with a trailing separator", sys + `\`, pf, func(f *fakeFS) {
			f.files[sys+`\nvidia-smi.exe`] = winFile(1)
		}, sys + `\nvidia-smi.exe`},
		{"newest driver store package", sys, pf, func(f *fakeFS) {
			withDriver(f)
			f.dirs[store] = []string{
				"prnms001.inf_amd64_x", "nv_dispi.inf_amd64_old", "nvhdci.inf_amd64_y",
				"nv_dispi.inf_amd64_new", "nv_dispi.inf_amd64_mid", "ntprint.inf_amd64_z",
			}
			f.files[store+`\prnms001.inf_amd64_x\nvidia-smi.exe`] = winFile(99)
			f.files[store+`\ntprint.inf_amd64_z\nvidia-smi.exe`] = winFile(99)
			f.files[store+`\nv_dispi.inf_amd64_old\nvidia-smi.exe`] = winFile(10)
			f.files[store+`\nv_dispi.inf_amd64_mid\nvidia-smi.exe`] = winFile(20)
			f.files[store+`\nv_dispi.inf_amd64_new\nvidia-smi.exe`] = winFile(30)
			f.files[nvsmi] = winFile(99)
		}, store + `\nv_dispi.inf_amd64_new\nvidia-smi.exe`},
		{"ties go to the first name", sys, pf, func(f *fakeFS) {
			withDriver(f)
			f.dirs[store] = []string{"nv_b", "nv_c", "nv_a"}
			for _, n := range []string{"nv_a", "nv_b", "nv_c"} {
				f.files[store+`\`+n+`\nvidia-smi.exe`] = winFile(5)
			}
		}, store + `\nv_a\nvidia-smi.exe`},
		{"upper-case package name", sys, pf, func(f *fakeFS) {
			withDriver(f)
			f.dirs[store] = []string{"NV_DISPI.INF_AMD64_A"}
			f.files[store+`\NV_DISPI.INF_AMD64_A\nvidia-smi.exe`] = winFile(5)
		}, store + `\NV_DISPI.INF_AMD64_A\nvidia-smi.exe`},
		{"driver store skipped without nvcuda.dll", sys, pf, func(f *fakeFS) {
			f.dirs[store] = []string{"nv_dispi.inf_amd64_aaa"}
			f.files[store+`\nv_dispi.inf_amd64_aaa\nvidia-smi.exe`] = winFile(9)
			f.files[nvsmi] = winFile(1)
		}, nvsmi},
		{"driver store without nvidia-smi.exe", sys, pf, func(f *fakeFS) {
			withDriver(f)
			f.dirs[store] = []string{"nv_dispi.inf_amd64_aaa"}
			f.files[store+`\nv_dispi.inf_amd64_aaa\nvidia-smi.exe`] = dirWith(0o755)
			f.files[nvsmi] = winFile(1)
		}, nvsmi},
		{"unreadable driver store", sys, pf, func(f *fakeFS) {
			withDriver(f)
			f.files[nvsmi] = winFile(1)
		}, nvsmi},
		{"Program Files", sys, pf, func(f *fakeFS) { f.files[nvsmi] = winFile(1) }, nvsmi},
		{"Program Files only", "", pf, func(f *fakeFS) { f.files[nvsmi] = winFile(1) }, nvsmi},
		{"no Program Files", sys, "", func(f *fakeFS) { f.files[nvsmi] = winFile(1) }, ""},
		{"no directories", "", "", func(f *fakeFS) { f.files[`\nvidia-smi.exe`] = winFile(1) }, ""},
		{"nothing installed", sys, pf, func(f *fakeFS) {}, ""},
		{"a directory named nvidia-smi.exe", sys, pf, func(f *fakeFS) { f.files[sys+`\nvidia-smi.exe`] = dirWith(0o755) }, ""},
	}
	for _, c := range cases {
		f := newFakeFS()
		c.setup(f)
		got, ok := windowsNvidiaSMI(f, c.sys, c.pf)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: windowsNvidiaSMI = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}

	// Without nvcuda.dll the driver store is not even listed.
	reads := 0
	f := newFakeFS()
	f.reads = &reads
	f.dirs[store] = []string{"nv_a"}
	if _, ok := windowsNvidiaSMI(f, sys, pf); ok || reads != 0 {
		t.Errorf("listed the driver store %d times without a driver", reads)
	}

	// The search is bounded: only the first maxDriverStoreCandidates nv*
	// packages by name are examined, and a file system that returns more
	// names than asked for is cut short.
	f = newFakeFS()
	withDriver(f)
	var names []string
	for i := 0; i < maxDriverStoreCandidates+10; i++ {
		names = append(names, fmt.Sprintf("nv_%04d", i))
	}
	last := names[len(names)-1]
	f.files[store+`\`+last+`\nvidia-smi.exe`] = winFile(1)
	f.dirs[store] = names
	if got, ok := windowsNvidiaSMI(f, sys, pf); ok {
		t.Errorf("examined more than %d packages: found %q", maxDriverStoreCandidates, got)
	}
	f.files[store+`\`+names[maxDriverStoreCandidates-1]+`\nvidia-smi.exe`] = winFile(1)
	if got, ok := windowsNvidiaSMI(f, sys, pf); !ok || !strings.Contains(got, names[maxDriverStoreCandidates-1]) {
		t.Errorf("bounded search = %q, %v", got, ok)
	}
	many := make([]string, maxDriverStoreEntries+5)
	for i := range many {
		many[i] = fmt.Sprintf("zz_%06d", i)
	}
	many[len(many)-1] = "nv_late"
	f = newFakeFS()
	withDriver(f)
	f.files[store+`\nv_late\nvidia-smi.exe`] = winFile(1)
	if got, ok := driverStoreNvidiaSMI(overReader{f, many}, store); ok {
		t.Errorf("read past maxDriverStoreEntries: %q", got)
	}
}

// overReader returns more directory names than asked for.
type overReader struct {
	*fakeFS
	names []string
}

func (o overReader) readDirNames(string, int) ([]string, error) { return o.names, nil }

func TestWinJoin(t *testing.T) {
	cases := []struct {
		base  string
		elems []string
		want  string
	}{
		{`C:\Windows\System32`, []string{"nvidia-smi.exe"}, `C:\Windows\System32\nvidia-smi.exe`},
		{`C:\`, []string{"x"}, `C:\x`},
		{`C:\Program Files/`, []string{"NVIDIA Corporation", "NVSMI"}, `C:\Program Files\NVIDIA Corporation\NVSMI`},
	}
	for _, c := range cases {
		if got := winJoin(c.base, c.elems...); got != c.want {
			t.Errorf("winJoin(%q, %q) = %q, want %q", c.base, c.elems, got, c.want)
		}
	}
}

// The arguments are those of rattler and libmamba.
func TestNvidiaSMIArgs(t *testing.T) {
	if got := cudaVersionArgs(); !reflect.DeepEqual(got, []string{"--query", "-u", "-x"}) {
		t.Errorf("cudaVersionArgs = %q", got)
	}
	if got := cudaArchArgs(); !reflect.DeepEqual(got, []string{"--query-gpu=compute_cap", "--format=csv,noheader"}) {
		t.Errorf("cudaArchArgs = %q", got)
	}
	// Each call returns a fresh slice.
	a := cudaVersionArgs()
	a[0] = "changed"
	if cudaVersionArgs()[0] != "--query" {
		t.Error("cudaVersionArgs shares its slice")
	}
	p := unixNvidiaSMIPaths()
	p[0] = "changed"
	if unixNvidiaSMIPaths()[0] != "/usr/bin/nvidia-smi" {
		t.Error("unixNvidiaSMIPaths shares its slice")
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/lib.rs
// (test_parse_cuda_arch, CudaArch::parse_version, whose check is
// is_valid_cuda_version_format), applied to __cuda_arch overrides and
// recorded values; then the shapes a conda version allows but a compute
// capability does not.
func TestCudaArchOverrideShape(t *testing.T) {
	host := fakeProbe{plat: "linux-64", release: "6.8.0", glibc: "2.35"}
	for _, v := range []string{"8.6", "7.5", "9.0", "10.2", "0.0", "12.0", "08.06"} {
		ov := map[string]string{"cuda": "12.4", "cuda_arch": v}
		if r := find(mustDetect(t, host, ov), "__cuda_arch"); r == nil || r.Version != v {
			t.Errorf("__cuda_arch=%q: %v", v, r)
		}
		if err := CheckRecorded("linux-64", map[string]string{"__cuda": "12.4", "__cuda_arch": v}); err != nil {
			t.Errorf("CheckRecorded __cuda_arch=%q: %v", v, err)
		}
	}
	for _, v := range []string{
		"invalid", "8", "8.6.1", "8.6.1.0", ".6", "8.", ".", "8.6a", "a.6", "8.b", "eight.six", "8-6", "8_6",
		"86", "1!8.6", "8.6+x", "abc", " 8.6", strings.Repeat("8", 70) + ".6",
	} {
		ov := map[string]string{"cuda": "12.4", "__cuda_arch": v}
		if _, err := detect(context.Background(), Options{Overrides: ov}, host); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("Detect __cuda_arch=%q: err = %v, want ErrInvalidOverride", v, err)
		}
		if _, err := ApplyOverrides("linux-64", nil, ov); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("ApplyOverrides __cuda_arch=%q: err = %v, want ErrInvalidOverride", v, err)
		}
		if err := CheckRecorded("linux-64", map[string]string{"__cuda": "12.4", "__cuda_arch": v}); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("CheckRecorded __cuda_arch=%q: err = %v, want ErrInvalidOverride", v, err)
		}
	}
	// __cuda keeps accepting any conda version, as in conda and libmamba.
	if r := find(mustDetect(t, host, map[string]string{"cuda": "12.4.1"}), "__cuda"); r == nil || r.Version != "12.4.1" {
		t.Errorf("__cuda=12.4.1: %v", r)
	}
}

// Cases from rattler crates/rattler_virtual_packages/src/lib.rs
// (test_cuda_arch_coupling): __cuda_arch never exists without __cuda.
// rattler's case 4 ("__cuda": "" with "__cuda_arch": "8.6") yields neither
// package; goomba rejects that contradictory pair as ErrInvalidOverride, so
// it yields no records at all.
func TestCudaArchCoupling(t *testing.T) {
	noGPU := gpuHost("linux-64")
	noGPU.smi = ""
	// Case 1: no overrides; when __cuda is absent, so is __cuda_arch.
	for _, p := range []fakeProbe{noGPU, gpuHost("linux-64"), gpuHost("osx-arm64")} {
		recs := mustDetect(t, p, nil)
		if find(recs, "__cuda") == nil && find(recs, "__cuda_arch") != nil {
			t.Errorf("case 1 %s: %q", p.plat, triples(recs))
		}
	}
	// Case 2: a __cuda_arch override without __cuda is dropped.
	if recs := mustDetect(t, noGPU, map[string]string{"cuda_arch": "8.6"}); find(recs, "__cuda") != nil || find(recs, "__cuda_arch") != nil {
		t.Errorf("case 2: %q", triples(recs))
	}
	// Case 3: both overridden: both present.
	recs := mustDetect(t, noGPU, map[string]string{"cuda": "12.0", "cuda_arch": "8.6"})
	if r := find(recs, "__cuda"); r == nil || r.Version != "12.0" {
		t.Errorf("case 3: __cuda = %v", r)
	}
	if r := find(recs, "__cuda_arch"); r == nil || r.Version != "8.6" {
		t.Errorf("case 3: __cuda_arch = %v", r)
	}
	// Case 4: __cuda removed with __cuda_arch set.
	for _, ov := range []map[string]string{
		{"cuda": "", "cuda_arch": "8.6"},
		{"__CUDA": "", "__cuda_arch": "8.6"},
	} {
		if recs, err := detect(context.Background(), Options{Overrides: ov}, gpuHost("linux-64")); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("case 4 %v: %q, %v; want ErrInvalidOverride", ov, triples(recs), err)
		}
		if _, err := ApplyOverrides("linux-64", nil, ov); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("case 4 ApplyOverrides %v: %v; want ErrInvalidOverride", ov, err)
		}
	}
	// Removing both is fine.
	if recs := mustDetect(t, gpuHost("linux-64"), map[string]string{"cuda": "", "cuda_arch": ""}); find(recs, "__cuda") != nil || find(recs, "__cuda_arch") != nil {
		t.Errorf("both removed: %q", triples(recs))
	}

	// ApplyOverrides follows the same rule.
	for _, plat := range []string{"linux-64", "win-64", "osx-arm64"} {
		recs, err := ApplyOverrides(plat, Defaults(plat), map[string]string{"cuda_arch": "8.6"})
		if err != nil || find(recs, "__cuda_arch") != nil {
			t.Errorf("ApplyOverrides(%s, Defaults, cuda_arch) = %q, %v", plat, triples(recs), err)
		}
		recs, err = ApplyOverrides(plat, Defaults(plat), map[string]string{"cuda": "12.4", "cuda_arch": "8.6"})
		if err != nil || find(recs, "__cuda") == nil || find(recs, "__cuda_arch") == nil {
			t.Errorf("ApplyOverrides(%s, Defaults, cuda+cuda_arch) = %q, %v", plat, triples(recs), err)
		}
		withGPU, err := ApplyOverrides(plat, nil, map[string]string{"__cuda": "12.4", "__cuda_arch": "8.6"})
		if err != nil {
			t.Fatal(err)
		}
		recs, err = ApplyOverrides(plat, withGPU, map[string]string{"cuda": ""})
		if err != nil || len(recs) != 0 {
			t.Errorf("ApplyOverrides(%s, cuda+arch, cuda removed) = %q, %v", plat, triples(recs), err)
		}
		// Records holding __cuda_arch alone lose it.
		lone := []*record.PackageRecord{newRecord(plat, "__cuda_arch", "8.6", "0")}
		if recs, err := ApplyOverrides(plat, lone, nil); err != nil || len(recs) != 0 {
			t.Errorf("ApplyOverrides(%s, lone __cuda_arch) = %q, %v", plat, triples(recs), err)
		}
		// CheckRecorded rejects a recorded __cuda_arch without __cuda.
		if err := CheckRecorded(plat, map[string]string{"__cuda_arch": "8.6"}); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("CheckRecorded(%s, lone __cuda_arch) = %v", plat, err)
		}
		if err := CheckRecorded(plat, map[string]string{"__cuda": "12.4", "__cuda_arch": "8.6"}); err != nil {
			t.Errorf("CheckRecorded(%s, cuda+cuda_arch) = %v", plat, err)
		}
	}
}
