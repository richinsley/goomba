package virtual

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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
		}
	})
}
