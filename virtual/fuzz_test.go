package virtual

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func FuzzParseLinuxRelease(f *testing.F) {
	for _, s := range []string{
		"5.10.102.1-microsoft-standard-WSL2", "2.6.32-220.17.1.el6.i686",
		"5.4.72-microsoft-standard-WSL2", "4.9.43-1-MANJARO", "3.16.0-31-generic",
		"6.8", "6", "", "v6.8", "6.8.", "1.2.3.4.5",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v := numericPrefix(s, 2, 4)
		if v == "" {
			return
		}
		if !strings.HasPrefix(s, v) {
			t.Fatalf("numericPrefix(%q) = %q is not a prefix", s, v)
		}
		if err := checkVersion(v); err != nil {
			t.Fatalf("numericPrefix(%q) = %q: %v", s, v, err)
		}
		if n := strings.Count(v, ".") + 1; n < 2 || n > 4 {
			t.Fatalf("numericPrefix(%q) = %q has %d parts", s, v, n)
		}
	})
}

func FuzzParseLibcVersionOutput(f *testing.F) {
	for _, s := range []string{
		"ldd (Ubuntu GLIBC 2.35-0ubuntu3.1) 2.35", "ldd (Gentoo 2.39-r9 (patchset 9)) 2.39",
		"ldd (GNU libc) 2.31", "ldd (Solus) 2.39", "glibc 2.35\n", "musl libc (x86_64)\nVersion 1.2.4\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if v := parseLibcVersionOutput(s); v != "" {
			if err := checkVersion(v); err != nil {
				t.Fatalf("parseLibcVersionOutput(%q) = %q: %v", s, v, err)
			}
		}
	})
}

func FuzzGlibcFromELF(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "elf", "*.so"))
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte("\x7fELF\x02\x01\x01"))
	f.Add([]byte("GNU C Library (x) stable release version 2.35."))
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := glibcFromELF(bytes.NewReader(data), amd64ELF)
		if err != nil {
			if v != "" {
				t.Fatalf("version %q with error %v", v, err)
			}
			return
		}
		if err := checkVersion(v); err != nil {
			t.Fatalf("glibcFromELF = %q: %v", v, err)
		}
	})
}

func FuzzOverrides(f *testing.F) {
	seeds := [][4]string{
		{"glibc", "2.17", "cuda", "12.4"},
		{"__archspec", "x86_64_v3", "unix", ""},
		{"GLIBC", "2.17", "glibc", "2.28"},
		{"archspec", "x86-64-v3", "osx", "13.0"},
		{"__", "1", "linux", "1!2.0+3"},
		{"cuda", "12.4", "cuda_arch", "8.6"},
		{"cuda", "", "__cuda_arch", "8.6"},
		{"CUDA_ARCH", "8.6", "osx", "13.0"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1], s[2], s[3])
	}
	f.Fuzz(func(t *testing.T, k1, v1, k2, v2 string) {
		ov := map[string]string{k1: v1, k2: v2}
		for _, p := range []string{"linux-64", "osx-arm64", "win-64"} {
			recs, err := ApplyOverrides(p, Defaults(p), ov)
			if err != nil {
				if !errors.Is(err, ErrInvalidOverride) {
					t.Fatalf("ApplyOverrides(%q, %q): unexpected error %v", p, ov, err)
				}
				return
			}
			for i, r := range recs {
				if err := checkName(r.Name); err != nil {
					t.Fatalf("record name %q: %v", r.Name, err)
				}
				if err := checkVersion(r.Version); err != nil {
					t.Fatalf("record %q version %q: %v", r.Name, r.Version, err)
				}
				if err := checkBuild(r.Build); err != nil {
					t.Fatalf("record %q build %q: %v", r.Name, r.Build, err)
				}
				if i > 0 && recs[i-1].Name >= r.Name {
					t.Fatalf("records not sorted and unique: %q", triples(recs))
				}
			}
			if find(recs, "__cuda_arch") != nil && find(recs, "__cuda") == nil {
				t.Fatalf("__cuda_arch without __cuda: %q", triples(recs))
			}
		}
	})
}

// nvidiaSMISeeds returns the fixtures and some test inputs.
func nvidiaSMISeeds(f *testing.F, extra ...string) {
	files, _ := filepath.Glob(filepath.Join("testdata", "nvidia-smi", "*"))
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	for _, s := range extra {
		f.Add([]byte(s))
	}
}

// cudaVersionShape matches a normalised major.minor CUDA version.
const cudaVersionShape = `^[1-9][0-9]?\.(0|[1-9][0-9]?)$`

func FuzzParseNvidiaSMICudaVersion(f *testing.F) {
	nvidiaSMISeeds(f,
		"<nvidia_smi_log>\n  <cuda_version>12.4</cuda_version>\n</nvidia_smi_log>",
		"<nvidia_smi_log></nvidia_smi_log>", "",
		"<cuda_version>12.4.1</cuda_version>", "<cuda_version>12</cuda_version>",
		"<cuda_version> 012.04 </cuda_version>", "<cuda_version>N/A</cuda_version>",
		"<cuda_version>99999.1</cuda_version>", "<cuda_version>-1.0</cuda_version>",
		"<cuda_version>12.4</cuda_version><cuda_version>11.8</cuda_version>",
		"<cuda_version>12.4", "\x00\xff<cuda_version>12.4</cuda_version>",
	)
	shape := regexp.MustCompile(cudaVersionShape)
	f.Fuzz(func(t *testing.T, data []byte) {
		v, ok := parseNvidiaSMICudaVersion(data)
		if !ok {
			if v != "" {
				t.Fatalf("version %q without ok", v)
			}
			return
		}
		if !shape.MatchString(v) || checkVersion(v) != nil {
			t.Fatalf("parseNvidiaSMICudaVersion(%q) = %q, not a normalised major.minor", data, v)
		}
		if !bytes.Contains(data, []byte("<cuda_version>")) || !bytes.Contains(data, []byte("</cuda_version>")) {
			t.Fatalf("parseNvidiaSMICudaVersion(%q) = %q without a cuda_version element", data, v)
		}
		if again, ok := parseNvidiaSMICudaVersion([]byte("<cuda_version>" + v + "</cuda_version>")); !ok || again != v {
			t.Fatalf("version %q does not reparse: %q, %v", v, again, ok)
		}
	})
}

// FuzzParseNvidiaSMIComputeCap compares the parser with a regular-expression
// statement of the same rule.
func FuzzParseNvidiaSMIComputeCap(f *testing.F) {
	nvidiaSMISeeds(f,
		"8.6\n7.5\n9.0", "8.6\n[N/A]\n7.5\ngarbage", "x.y\n8.0", "[N/A]\ngarbage\n", "",
		"9.0a\n12.0", "10.0f", "  8.6  \r\n\t7.5\t\r\n", "12.4.1\n8.6", "99999.1",
		"0.5\n8.6", "-7.5", "08.06", "8.10\n8.9", "compute_cap\n12.0\n8.6\n7.5\n",
		" 8.6 ", "\x00\xff\n7.5\n\xfe\x80",
	)
	shape := regexp.MustCompile(cudaVersionShape)
	capLine := regexp.MustCompile(`^([0-9]{1,9})\.([0-9]{1,9})[A-Za-z]*$`)
	f.Fuzz(func(t *testing.T, data []byte) {
		got, ok := parseNvidiaSMIComputeCap(data)
		want, found := "", false
		minMajor, minMinor := 0, 0
		for _, line := range strings.Split(string(data), "\n") {
			m := capLine.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			if major < 1 || major > 99 || minor > 99 {
				continue
			}
			if !found || major < minMajor || (major == minMajor && minor < minMinor) {
				found, minMajor, minMinor = true, major, minor
			}
		}
		if found {
			want = strconv.Itoa(minMajor) + "." + strconv.Itoa(minMinor)
		}
		if got != want || ok != found {
			t.Fatalf("parseNvidiaSMIComputeCap(%q) = %q, %v; want %q, %v", data, got, ok, want, found)
		}
		if ok && (!shape.MatchString(got) || checkVersion(got) != nil) {
			t.Fatalf("parseNvidiaSMIComputeCap(%q) = %q, not a normalised major.minor", data, got)
		}
	})
}
