package link

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzParsePathsJSON(f *testing.F) {
	for _, s := range []string{
		`{"paths": [{"_path": "file.txt", "path_type": "hardlink"}], "paths_version": 1}`,
		`{"paths": [{"_path": "bin/tool", "path_type": "hardlink", "file_mode": "text", "prefix_placeholder": "/opt/ph",
		  "sha256": "` + emptySHA256 + `", "size_in_bytes": 12, "no_link": true},
		  {"_path": "lib/x.so", "path_type": "softlink"}, {"_path": "share/d", "path_type": "directory"}], "paths_version": 1}`,
		`{"paths": [{"_path": "../x", "path_type": "hardlink"}], "paths_version": 1}`,
		`{"paths": [], "paths_version": 2}`,
		`{"paths": [{"_path": "a\u0000b", "path_type": "softlink"}], "paths_version": 1}`,
		`{"paths": null, "paths_version": 1}`,
		`[]`,
	} {
		f.Add([]byte(s))
	}
	if data, err := os.ReadFile("testdata/link-scripts-0.1.0-h4616a5c_0/info/paths.json"); err == nil {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := ParsePathsJSON(data)
		if err != nil {
			if entries != nil {
				t.Fatal("entries returned with an error")
			}
			return
		}
		for _, e := range entries {
			if err := validRelPath(e.Path); err != nil {
				t.Fatalf("accepted unsafe path %q: %v", e.Path, err)
			}
			switch e.PathType {
			case PathHardlink, PathSoftlink, PathDirectory:
			default:
				t.Fatalf("accepted path type %q", e.PathType)
			}
			switch e.FileMode {
			case "", FileModeText, FileModeBinary:
			default:
				t.Fatalf("accepted file mode %q", e.FileMode)
			}
			if e.SHA256 != "" && !isHexDigest(e.SHA256, 64) {
				t.Fatalf("accepted sha256 %q", e.SHA256)
			}
		}
	})
}

func FuzzParseEntryPoint(f *testing.F) {
	for _, s := range []string{
		"pip = pip._internal.cli.main:main",
		`findpython = "findpython.__main__:main"`,
		"cmd = 'pkg.mod:func'",
		"tool = pkg:Class.method",
		`bad/cmd = "..evil:also/bad"`,
		"../bin/pip = innocuous_pkg.evil:main",
		`C:\evil = innocuous_pkg.evil:main`,
		"jlpm = jupyterlab.jlpmapp:main(); __import__('os').system('x')",
		"\x00 = innocuous_pkg.evil:main",
		"not-an-entry-point",
		".hidden = a:b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, def string) {
		ep, err := ParseEntryPoint(def)
		if err != nil {
			if !errors.Is(err, ErrInvalidEntryPoint) {
				t.Fatalf("error %v is not ErrInvalidEntryPoint", err)
			}
			return
		}
		if err := checkEntryPointCommand(ep.Command); err != nil {
			t.Fatalf("accepted command %q: %v", ep.Command, err)
		}
		if err := validRelPath("bin/" + ep.Command); err != nil && len(ep.Command) < maxPathLength-4 {
			t.Fatalf("command %q escapes the bin directory: %v", ep.Command, err)
		}
		if !isIdentifierChain(ep.Module) || !isIdentifierChain(ep.Func) {
			t.Fatalf("accepted %+v", ep)
		}
		script := entryPointScript("#!/p/bin/python", ep)
		if strings.Count(script, "\n") != 10 {
			t.Fatalf("entry point %+v changes the script's shape:\n%s", ep, script)
		}
		// Parsing the canonical form gives the same entry point.
		again, err := ParseEntryPoint(ep.String())
		if err != nil || again != ep {
			t.Fatalf("round trip of %+v: %+v, %v", ep, again, err)
		}
	})
}

func FuzzReplaceBinaryPrefix(f *testing.F) {
	for _, c := range []struct{ in, ph, prefix string }{
		{"12345Hello, fabulous world!\x006789", "fabulous", "cruel"},
		{"beginrandomdataPATH=/placeholder/etc/share:/placeholder/bin/:\x00somemoretext", "/placeholder", "/target"},
		{"short\x00", "short", "verylong"},
		{"xx/placeholder/lib", "/placeholder", "/t"},
		{"aabab\x00", "aab", "a"},
		{"", "", ""},
	} {
		f.Add([]byte(c.in), c.ph, c.prefix)
	}
	f.Fuzz(func(t *testing.T, data []byte, placeholder, prefix string) {
		orig := bytes.Clone(data)
		out, n, err := ReplaceBinaryPrefix(data, placeholder, prefix)
		if !bytes.Equal(data, orig) {
			t.Fatal("input modified")
		}
		if len(prefix) > len(placeholder) {
			if !errors.Is(err, ErrPrefixTooLong) {
				t.Fatalf("err = %v, want ErrPrefixTooLong", err)
			}
			return
		}
		if strings.IndexByte(placeholder, 0) >= 0 {
			if !errors.Is(err, ErrInvalidPackage) {
				t.Fatalf("err = %v, want ErrInvalidPackage", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != len(data) {
			t.Fatalf("length %d, want %d", len(out), len(data))
		}
		if n < 0 || (n == 0 && !bytes.Equal(out, data)) {
			t.Fatalf("n = %d but output changed", n)
		}
		if placeholder != "" && !bytes.Contains(data, []byte(placeholder)) && n != 0 {
			t.Fatalf("replaced %d occurrences of an absent placeholder", n)
		}
		if ref := replaceBinaryPrefixLibmamba(data, placeholder, prefix); !bytes.Equal(out, ref) {
			t.Fatalf("ReplaceBinaryPrefix(%q, %q, %q) = %q; libmamba's algorithm gives %q", data, placeholder, prefix, out, ref)
		}
		// Without padding the replacement is a plain non-overlapping
		// substitution.
		if len(prefix) == len(placeholder) && placeholder != "" {
			if want := bytes.ReplaceAll(data, []byte(placeholder), []byte(prefix)); !bytes.Equal(out, want) || n != bytes.Count(data, []byte(placeholder)) {
				t.Fatalf("same-length replacement %q (%d), want %q", out, n, want)
			}
		}
	})
}

func FuzzReplaceTextPrefix(f *testing.F) {
	f.Add([]byte("#!/this/is/placeholder/executable -m 123123\n\nimport sys\n"), "/this/is/placeholder", "/super/long/"+strings.Repeat("verylongstring/", 15), "linux-64")
	f.Add([]byte("Hello, cruel world!"), "cruel", "fabulous", "osx-arm64")
	f.Add([]byte("#!P/x\nP"), "P", `C:\x`, "win-64")
	f.Fuzz(func(t *testing.T, data []byte, placeholder, prefix, platform string) {
		if placeholder == "" {
			return
		}
		out := ReplaceTextPrefix(data, placeholder, prefix, platform)
		if isWindows(platform) || !bytes.HasPrefix(out, []byte("#!")) {
			return
		}
		line, _, _ := bytes.Cut(out, []byte("\n"))
		replaced := bytes.ReplaceAll(data, []byte(placeholder), []byte(prefix))
		origLine, _, _ := bytes.Cut(replaced, []byte("\n"))
		if len(origLine) <= maxShebangLength(platform) && !bytes.Equal(out, replaced) {
			t.Fatal("short shebang rewritten")
		}
		if !bytes.Equal(out[len(line):], replaced[len(origLine):]) {
			t.Fatal("content after the shebang changed")
		}
	})
}

func FuzzParseHasPrefix(f *testing.F) {
	f.Add([]byte("/opt/anaconda1anaconda2anaconda3 text lib/pkgconfig/zlib.pc\nlib/x\n/p binary lib/y\n"))
	f.Add([]byte(`"/opt/anaconda1 anaconda2anaconda3" binary "lib/pkg config/zlib.pc"`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := parseHasPrefix(data)
		if err != nil {
			return
		}
		for p, pf := range m {
			if p == "" || pf.placeholder == "" || (pf.mode != FileModeText && pf.mode != FileModeBinary) {
				t.Fatalf("entry %q: %+v", p, pf)
			}
		}
	})
}

func FuzzParseCondaMeta(f *testing.F) {
	f.Add([]byte(`{"name": "a", "version": "1", "build_string": "b0", "build_number": 3, "track_features": "x y,z",
		"noarch": true, "timestamp": 1700000000, "files": ["a"], "paths_data": {"paths": [{"_path": "a"}]}}`))
	f.Add([]byte(`{"name": "a", "build": "b1", "track_features": ["p", ""], "noarch": "python", "timestamp": 1.7e12}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := parseCondaMeta(data)
		if err != nil {
			return
		}
		for _, tf := range r.Record.TrackFeatures {
			if tf == "" {
				t.Fatal("empty track feature")
			}
		}
	})
}

func FuzzMatchShebang(f *testing.F) {
	for _, s := range []string{
		"#!/simple/shebang", "#!    /simple/shebang", `#!/simple/shebang/escaped\ space --and --flags -x`,
		"#!/bin/sh\r", `#!/a\\ b\`, "#!/a\tb", "#! /x \xff\xfe",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		exe, rest, ok := matchShebang(s)
		rexe, rrest, rok := matchShebangRegexp(s)
		if !utf8.ValidString(s) {
			// Go's regexp decodes invalid UTF-8 as U+FFFD; only the
			// outcome is comparable.
			rexe, rrest = exe, rest
		}
		if ok != rok || exe != rexe || rest != rrest {
			t.Fatalf("matchShebang(%q) = %q %q %v; regexp: %q %q %v", s, exe, rest, ok, rexe, rrest, rok)
		}
	})
}

// replaceBinaryPrefixLibmamba is a direct transcription of the binary
// branch of libmamba's LinkPackage::link_path, used to check the linear
// implementation. Callers must ensure a non-empty placeholder without NULs
// and a prefix that is not longer.
func replaceBinaryPrefixLibmamba(data []byte, placeholder, prefix string) []byte {
	buffer := string(data)
	padding := strings.Repeat("\x00", len(placeholder)-len(prefix))
	if placeholder == "" {
		return data
	}
	pos := strings.Index(buffer, placeholder)
	for pos >= 0 {
		end := pos + len(placeholder)
		suffixEnd := end
		for suffixEnd < len(buffer) && buffer[suffixEnd] != 0 {
			suffixEnd++
		}
		buffer = buffer[:pos] + prefix + buffer[end:suffixEnd] + padding + buffer[suffixEnd:]
		next := pos + len(prefix)
		if next >= len(buffer) {
			break
		}
		i := strings.Index(buffer[next:], placeholder)
		if i < 0 {
			break
		}
		pos = next + i
	}
	return []byte(buffer)
}
