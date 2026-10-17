package link

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// testPlaceholder is a conda-build style 255-byte placeholder.
func testPlaceholder() string {
	return ("/opt/conda-bld/feedstock_1/_h_env_" + strings.Repeat("placehold_", 30))[:255]
}

// basicSpec is a generic package exercising every way a file is linked.
func basicSpec(ph string) pkgSpec {
	elf := "\x7fELF\x02\x01\x01\x00rpath=" + ph + "/lib:" + ph + "/lib64\x00other\x00"
	return pkgSpec{name: "basic", files: []pkgFile{
		{path: "bin/tool", data: "#!" + ph + "/bin/python3.11\nprint('" + ph + "')\n", mode: 0o755, placeholder: ph, fileMode: FileModeText},
		{path: "lib/libfoo.so.1", data: elf, mode: 0o755, placeholder: ph, fileMode: FileModeBinary},
		{path: "lib/libfoo.so", symlink: "libfoo.so.1"},
		{path: "lib/libdangling.so", symlink: "missing.so"},
		{path: "share/doc/README", data: "readme\n"},
		{path: "share/data", data: "data\n", mode: 0o640, noLink: true},
		{path: "share/empty", dir: true},
		{path: "share/doc-link", symlink: "doc"},
	}}
}

func TestLinkBasic(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	ph := testPlaceholder()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), basicSpec(ph))
	prefix := filepath.Join(tmp, "env")
	opt := Options{Prefix: prefix, Platform: "linux-64", RequestedSpec: "basic >=1"}

	inst, err := Link(ctx, pkgDir, rec, opt)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := []string{
		"bin/", "bin/tool", "lib/", "lib/libdangling.so@missing.so", "lib/libfoo.so.1", "lib/libfoo.so@libfoo.so.1",
		"share/", "share/data", "share/doc-link@doc", "share/doc/", "share/doc/README", "share/empty/",
	}
	if got := tree(t, prefix); !reflect.DeepEqual(got, wantTree) {
		t.Fatalf("prefix tree:\n got %q\nwant %q", got, wantTree)
	}

	// Plain files are hard linked, no_link files copied with their mode.
	same := func(rel string) bool {
		a, _ := os.Stat(filepath.Join(pkgDir, rel))
		b, _ := os.Stat(filepath.Join(prefix, rel))
		return os.SameFile(a, b)
	}
	if !same("share/doc/README") {
		t.Error("README was not hard linked")
	}
	if same("share/data") {
		t.Error("no_link file was hard linked")
	}
	if fi, _ := os.Stat(filepath.Join(prefix, "share/data")); fi.Mode().Perm() != 0o640 {
		t.Errorf("share/data mode %v", fi.Mode())
	}

	// Text replacement keeps the mode; binary replacement keeps the length.
	if got, want := readString(t, filepath.Join(prefix, "bin/tool")), "#!"+prefix+"/bin/python3.11\nprint('"+prefix+"')\n"; got != want {
		t.Errorf("bin/tool = %q, want %q", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(prefix, "bin/tool")); fi.Mode().Perm() != 0o755 {
		t.Errorf("bin/tool mode %v", fi.Mode())
	}
	bin := readString(t, filepath.Join(prefix, "lib/libfoo.so.1"))
	src := readString(t, filepath.Join(pkgDir, "lib/libfoo.so.1"))
	if len(bin) != len(src) || strings.Contains(bin, ph) || !strings.Contains(bin, "rpath="+prefix+"/lib:"+prefix+"/lib64\x00") || !strings.HasSuffix(bin, "\x00other\x00") {
		t.Errorf("binary replacement: %q", bin)
	}

	// Installed records.
	byPath := map[string]PathRecord{}
	for _, p := range inst.Paths {
		byPath[p.Path] = p
	}
	if got := inst.Files(); !reflect.DeepEqual(got, []string{"bin/tool", "lib/libfoo.so.1", "lib/libfoo.so", "lib/libdangling.so", "share/doc/README", "share/data", "share/empty", "share/doc-link"}) {
		t.Errorf("files %q", got)
	}
	if r := byPath["bin/tool"]; r.SHA256InPrefix != sha256Hex([]byte(readString(t, filepath.Join(prefix, "bin/tool")))) || r.SHA256 == r.SHA256InPrefix {
		t.Errorf("bin/tool record %+v", r)
	}
	if r := byPath["lib/libfoo.so"]; r.SHA256InPrefix != byPath["lib/libfoo.so.1"].SHA256InPrefix || r.SHA256 != "" || r.PathType != PathSoftlink {
		t.Errorf("symlink record %+v", r)
	}
	if r := byPath["lib/libdangling.so"]; r.SHA256InPrefix != emptySHA256 {
		t.Errorf("dangling symlink record %+v", r)
	}
	if r := byPath["share/doc-link"]; r.SHA256InPrefix != emptySHA256 {
		t.Errorf("directory symlink record %+v", r)
	}
	if r := byPath["share/doc/README"]; r.SHA256InPrefix != sha256Hex([]byte("readme\n")) || r.SizeInBytes != 7 {
		t.Errorf("README record %+v", r)
	}
	if r := byPath["share/data"]; !r.NoLink {
		t.Errorf("no_link record %+v", r)
	}
	if r := byPath["share/empty"]; r.PathType != PathDirectory || r.SHA256InPrefix != emptySHA256 {
		t.Errorf("directory record %+v", r)
	}

	// conda-meta round trip, then Unlink restores an empty prefix.
	if err := WriteCondaMeta(prefix, rec, inst, opt); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadCondaMeta(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || !reflect.DeepEqual(recs[0].Record, rec) || !reflect.DeepEqual(recs[0].Paths, inst.Paths) || recs[0].RequestedSpec != "basic >=1" || recs[0].PackageDir != pkgDir {
		t.Errorf("ReadCondaMeta = %+v", recs[0])
	}
	if err := Unlink(ctx, prefix, rec); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("after Unlink: %q", got)
	}
	// The package itself is untouched.
	if got := readString(t, filepath.Join(pkgDir, "bin/tool")); !strings.Contains(got, ph) {
		t.Error("package file modified")
	}
}

func TestLinkCopyOnly(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), basicSpec(testPlaceholder()))
	prefix := filepath.Join(tmp, "env")
	if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64", CopyOnly: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(filepath.Join(pkgDir, "share/doc/README"))
	b, _ := os.Stat(filepath.Join(prefix, "share/doc/README"))
	if os.SameFile(a, b) {
		t.Error("CopyOnly hard linked a file")
	}
	if readString(t, filepath.Join(prefix, "share/doc/README")) != "readme\n" {
		t.Error("copy content")
	}
}

// Prefixes: short, 200 characters, with spaces, and longer than the
// placeholder.
func TestLinkPrefixes(t *testing.T) {
	ph := testPlaceholder()
	tmp := t.TempDir()
	pkgs := filepath.Join(tmp, "pkgs")
	genDir, gen := buildPkg(t, pkgs, basicSpec(ph))
	pyDir, py := buildPkg(t, pkgs, pkgSpec{name: "tool", noarch: "python", entryPoints: []string{"tool-cli = tool.cli:main"}, files: []pkgFile{
		{path: "site-packages/tool/__init__.py", data: ""},
		{path: "site-packages/tool/cli.py", data: "def main(): pass\n"},
	}})

	// pad extends base to exactly n bytes with path components of at most
	// 50 bytes (file systems limit components to 255).
	pad := func(base string, n int) string {
		p := base
		for len(p) < n {
			switch rest := n - len(p); {
			case rest == 1:
				p += "x"
			default:
				p += "/" + strings.Repeat("x", min(rest-1, 50))
			}
		}
		return p
	}
	cases := []struct {
		name, prefix string
		toolShebang  string // first line of bin/tool; "" means "#!<prefix>/bin/python3.11"
		trampoline   bool   // entry point uses the /bin/sh trampoline
	}{
		{name: "short", prefix: filepath.Join(tmp, "e")},
		{name: "200 chars", prefix: pad(filepath.Join(tmp, "long"), 200), toolShebang: "#!/usr/bin/env python3.11", trampoline: true},
		// libmamba keeps a short shebang with a space in the prefix as is.
		{name: "spaces", prefix: filepath.Join(tmp, "with space", "env x"), trampoline: true},
	}
	if n := len(cases[1].prefix); n != 200 {
		t.Fatalf("long prefix has %d bytes", n)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opt := Options{Prefix: c.prefix, Platform: "linux-64", Python: pythonInfo()}
			if _, err := Link(context.Background(), genDir, gen, opt); err != nil {
				t.Fatal(err)
			}
			if _, err := Link(context.Background(), pyDir, py, opt); err != nil {
				t.Fatal(err)
			}
			want := c.toolShebang
			if want == "" {
				want = "#!" + c.prefix + "/bin/python3.11"
			}
			if got, _, _ := strings.Cut(readString(t, filepath.Join(c.prefix, "bin/tool")), "\n"); got != want {
				t.Errorf("bin/tool shebang %q, want %q", got, want)
			}
			bin := readString(t, filepath.Join(c.prefix, "lib/libfoo.so.1"))
			if len(bin) != len(readString(t, filepath.Join(genDir, "lib/libfoo.so.1"))) || !strings.Contains(bin, "rpath="+c.prefix+"/lib:") || strings.Contains(bin, ph) {
				t.Errorf("binary: %q", bin)
			}
			exe := c.prefix + "/bin/python3.11"
			wantEP := "#!" + exe + "\n"
			if c.trampoline {
				wantEP = "#!/bin/sh\n" + `'''exec' "` + exe + `" "$0" "$@" #'''` + "\n"
			}
			wantEP += "# -*- coding: utf-8 -*-\nimport re\nimport sys\n\nfrom tool.cli import main\n\n" +
				"if __name__ == '__main__':\n    sys.argv[0] = re.sub(r'(-script\\.pyw?|\\.exe)?$', '', sys.argv[0])\n    sys.exit(main())\n"
			if got := readString(t, filepath.Join(c.prefix, "bin/tool-cli")); got != wantEP {
				t.Errorf("entry point:\n%s\nwant\n%s", got, wantEP)
			}
			if fi, _ := os.Stat(filepath.Join(c.prefix, "bin/tool-cli")); fi.Mode().Perm() != entryPointMode {
				t.Errorf("entry point mode %v", fi.Mode())
			}
		})
	}

	t.Run("longer than placeholder", func(t *testing.T) {
		prefix := pad(filepath.Join(tmp, "toolong"), len(ph)+1)
		_, err := Link(context.Background(), genDir, gen, Options{Prefix: prefix, Platform: "linux-64"})
		if !errors.Is(err, ErrPrefixTooLong) {
			t.Fatalf("err = %v, want ErrPrefixTooLong", err)
		}
		if got := tree(t, prefix); len(got) != 0 {
			t.Errorf("left behind: %q", got)
		}
		// Windows does not patch binaries, so the length does not matter.
		if _, err := Link(context.Background(), genDir, gen, Options{Prefix: prefix, Platform: "win-64"}); err != nil {
			t.Errorf("win-64: %v", err)
		}
		// A package without binary placeholders links fine.
		if _, err := Link(context.Background(), pyDir, py, Options{Prefix: pad(filepath.Join(tmp, "toolong2"), 300), Platform: "linux-64", Python: pythonInfo()}); err != nil {
			t.Errorf("noarch package: %v", err)
		}
	})
}

func TestLinkExistingDestination(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), basicSpec(testPlaceholder()))
	prefix := filepath.Join(tmp, "env")
	if err := os.MkdirAll(filepath.Join(prefix, "share/doc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "share/doc/README"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tree(t, prefix)
	_, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if !strings.Contains(err.Error(), "share/doc/README") {
		t.Errorf("error does not name the path: %v", err)
	}
	if got := tree(t, prefix); !reflect.DeepEqual(got, before) {
		t.Errorf("prefix changed: %q", got)
	}
	if readString(t, filepath.Join(prefix, "share/doc/README")) != "mine" {
		t.Error("existing file modified")
	}
	// An existing directory for a directory entry is fine.
	if err := os.MkdirAll(filepath.Join(tmp, "env2", "share/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: filepath.Join(tmp, "env2"), Platform: "linux-64"}); err != nil {
		t.Errorf("existing directory entry: %v", err)
	}
}

// A failure after files were written removes everything Link created.
func TestLinkRollback(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), basicSpec(testPlaceholder()))
	prefix := filepath.Join(tmp, "env")
	if err := os.MkdirAll(filepath.Join(prefix, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	var signed []string
	boom := errors.New("boom")
	opt := Options{Prefix: prefix, Platform: "osx-arm64", Signer: func(p string) error {
		signed = append(signed, p)
		return boom
	}}
	_, err := Link(context.Background(), pkgDir, rec, opt)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the signer's error", err)
	}
	if want := []string{filepath.Join(prefix, "lib/libfoo.so.1")}; !reflect.DeepEqual(signed, want) {
		t.Errorf("signed %q, want %q", signed, want)
	}
	if got := tree(t, prefix); !reflect.DeepEqual(got, []string{"share/"}) {
		t.Errorf("left behind: %q", got)
	}

	// Cancellation part-way through also rolls back.
	ctx, cancel := context.WithCancel(context.Background())
	opt.Signer = func(string) error { cancel(); return nil }
	if _, err := Link(ctx, pkgDir, rec, opt); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := tree(t, prefix); !reflect.DeepEqual(got, []string{"share/"}) {
		t.Errorf("left behind after cancel: %q", got)
	}
	// A prefix Link created is removed with the rest.
	fresh := filepath.Join(tmp, "fresh")
	opt.Prefix = fresh
	opt.Signer = func(string) error { return boom }
	if _, err := Link(context.Background(), pkgDir, rec, opt); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := tree(t, fresh); len(got) != 0 {
		t.Errorf("left behind in a created prefix: %q", got)
	}
	opt.Prefix = prefix
	if _, err := Link(ctx, pkgDir, rec, opt); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
}

func TestLinkSigner(t *testing.T) {
	ph := testPlaceholder()
	spec := basicSpec(ph)
	spec.files = append(spec.files, pkgFile{path: "lib/libnoph.so", data: "\x7fELF none\x00", placeholder: ph, fileMode: FileModeBinary})
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), spec)
	for _, platform := range []string{"osx-arm64", "linux-64", "osx-64"} {
		var signed []string
		prefix := filepath.Join(tmp, platform)
		inst, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: platform, Signer: func(p string) error {
			signed = append(signed, p)
			// Signing changes the file; the recorded digest must follow.
			return os.WriteFile(p, []byte("signed"), 0o755)
		}})
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		if platform == "osx-arm64" {
			want = []string{filepath.Join(prefix, "lib/libfoo.so.1")}
			for _, p := range inst.Paths {
				if p.Path == "lib/libfoo.so.1" && p.SHA256InPrefix != sha256Hex([]byte("signed")) {
					t.Errorf("digest not recomputed after signing: %+v", p)
				}
			}
		}
		if !reflect.DeepEqual(signed, want) {
			t.Errorf("%s: signed %q, want %q", platform, signed, want)
		}
	}
}

func TestLinkUnsafe(t *testing.T) {
	cases := []struct {
		name  string
		spec  pkgSpec
		setup func(t *testing.T, prefix, outside string)
		want  error
	}{
		{name: "absolute symlink", spec: pkgSpec{files: []pkgFile{{path: "lib/x", symlink: "/etc/passwd"}}}, want: ErrUnsafePath},
		{name: "escaping symlink", spec: pkgSpec{files: []pkgFile{{path: "lib/x", symlink: "../../outside/secret"}}}, want: ErrUnsafePath},
		{name: "symlink to prefix parent", spec: pkgSpec{files: []pkgFile{{path: "x", symlink: ".."}}}, want: ErrUnsafePath},
		{
			// Each symlink stays inside the prefix when read on its own; the
			// chain leaves it, which the symlink digest pass detects.
			name: "symlink chain",
			spec: pkgSpec{files: []pkgFile{
				{path: "lib/sub/deep/c", symlink: "../../.."},
				{path: "lib/s1", symlink: "sub/deep/c/.."},
			}},
			want: ErrUnsafePath,
		},
		{
			// The same chain planted in the prefix by earlier packages: a
			// file written through it would land next to the prefix.
			name: "planted symlink chain",
			spec: pkgSpec{files: []pkgFile{{path: "lib/s1/evil", data: "evil"}}},
			setup: func(t *testing.T, prefix, outside string) {
				if err := os.MkdirAll(filepath.Join(prefix, "lib/sub/deep"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../..", filepath.Join(prefix, "lib/sub/deep/c")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("sub/deep/c/..", filepath.Join(prefix, "lib/s1")); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrUnsafePath,
		},
		{name: "conda-meta", spec: pkgSpec{files: []pkgFile{{path: "conda-meta/other-1-0.json", data: "{}"}}}, want: ErrUnsafePath},
		{name: "conda-meta case", spec: pkgSpec{files: []pkgFile{{path: "Conda-Meta/history", data: ""}}}, want: ErrUnsafePath},
		{name: "duplicate", spec: pkgSpec{files: []pkgFile{{path: "a", data: "1"}, {path: "a", data: "1"}}}, want: ErrInvalidPackage},
		{name: "file listed as symlink", spec: pkgSpec{files: []pkgFile{{path: "a", data: "1", pathType: "softlink"}}}, want: ErrInvalidPackage},
		{name: "symlink listed as file", spec: pkgSpec{files: []pkgFile{{path: "a", symlink: "b", pathType: "hardlink"}, {path: "b", data: "1"}}}, want: ErrInvalidPackage},
		{name: "dir listed as file", spec: pkgSpec{files: []pkgFile{{path: "a", dir: true, pathType: "hardlink"}}}, want: ErrInvalidPackage},
		{name: "placeholder on symlink", spec: pkgSpec{files: []pkgFile{{path: "a", symlink: "b", placeholder: "/ph", fileMode: FileModeText}}}, want: ErrInvalidPackage},
		{name: "index mismatch", spec: pkgSpec{files: []pkgFile{{path: "a", data: "1"}}, index: map[string]any{"name": "other"}}, want: ErrInvalidPackage},
		{name: "missing source", spec: pkgSpec{files: []pkgFile{{path: "a", data: "1"}}, info: map[string]string{
			"paths.json": `{"paths": [{"_path": "nope", "path_type": "hardlink"}], "paths_version": 1}`}}, want: fs.ErrNotExist},
		{
			name: "planted symlink in prefix",
			spec: pkgSpec{files: []pkgFile{{path: "lib/x", data: "1"}}},
			setup: func(t *testing.T, prefix, outside string) {
				if err := os.Symlink(outside, filepath.Join(prefix, "lib")); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrUnsafePath,
		},
		{
			name: "relative planted symlink in prefix",
			spec: pkgSpec{files: []pkgFile{{path: "lib/x", data: "1"}}},
			setup: func(t *testing.T, prefix, outside string) {
				if err := os.Symlink("../outside", filepath.Join(prefix, "lib")); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrUnsafePath,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			c.spec.name = "unsafe"
			pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), c.spec)
			prefix := filepath.Join(tmp, "env")
			outside := filepath.Join(tmp, "outside")
			for _, d := range []string{prefix, outside} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.setup != nil {
				c.setup(t, prefix, outside)
			}
			before := tree(t, prefix)
			_, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"})
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if got := tree(t, prefix); !reflect.DeepEqual(got, before) {
				t.Errorf("prefix changed: %q", got)
			}
			if got := tree(t, outside); len(got) != 0 {
				t.Errorf("wrote outside the prefix: %q", got)
			}
			if _, err := os.Lstat(filepath.Join(tmp, "evil")); err == nil {
				t.Error("wrote next to the prefix")
			}
		})
	}
}

// A symlinked directory that stays inside the prefix may be written through.
func TestLinkThroughInternalSymlink(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "a", files: []pkgFile{{path: "lib64/x", data: "1"}}})
	prefix := filepath.Join(tmp, "env")
	if err := os.MkdirAll(filepath.Join(prefix, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lib", filepath.Join(prefix, "lib64")); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"}); err != nil {
		t.Fatal(err)
	}
	if readString(t, filepath.Join(prefix, "lib/x")) != "1" {
		t.Error("file not written through the symlink")
	}
}

// Sources are confined to the package directory as well.
func TestLinkSourceEscape(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "a", files: []pkgFile{{path: "x", data: "1"}}})
	secret := filepath.Join(tmp, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(pkgDir, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "info/paths.json"), []byte(`{"paths": [{"_path": "lib/key", "path_type": "hardlink"}], "paths_version": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(tmp, "env")
	if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("left behind: %q", got)
	}
}

func TestLinkBadInputs(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "a", files: []pkgFile{{path: "x", data: "1"}}})
	ctx := context.Background()
	for _, prefix := range []string{"", "relative/env", filepath.Join(tmp, "a\nb")} {
		if _, err := Link(ctx, pkgDir, rec, Options{Prefix: prefix}); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("prefix %q: %v", prefix, err)
		}
	}
	for _, bad := range []*record.PackageRecord{
		{Name: "../x", Version: "1.0", Build: "h0_0"},
		{Name: "a", Version: "1/0", Build: "h0_0"},
		{Name: "a", Version: "1.0", Build: ""},
		{Name: ".a", Version: "1.0", Build: "h0_0"},
	} {
		if _, err := Link(ctx, pkgDir, bad, Options{Prefix: filepath.Join(tmp, "env")}); err == nil {
			t.Errorf("record %+v accepted", bad)
		}
	}
	if _, err := Link(ctx, pkgDir, nil, Options{Prefix: filepath.Join(tmp, "env")}); err == nil {
		t.Error("nil record accepted")
	}
	if _, err := Link(ctx, filepath.Join(tmp, "nope"), rec, Options{Prefix: filepath.Join(tmp, "env")}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing package dir: %v", err)
	}
	if got := tree(t, filepath.Join(tmp, "env")); len(got) != 0 {
		t.Errorf("left behind: %q", got)
	}
}

// Link scripts are refused unless allowed, and never run. The package is
// rattler's test-data/link-scripts. libmamba's test_link_scripts.cpp checks
// that scripts run; goomba checks the opposite.
func TestLinkScripts(t *testing.T) {
	ctx := context.Background()
	pkgDir := filepath.Join("testdata", "link-scripts-0.1.0-h4616a5c_0")
	rec := &record.PackageRecord{Name: "link-scripts", Version: "0.1.0", Build: "h4616a5c_0", Noarch: "generic", Subdir: "noarch"}
	prefix := filepath.Join(t.TempDir(), "env")
	opt := Options{Prefix: prefix, Platform: "linux-64", CopyOnly: true}
	if _, err := Link(ctx, pkgDir, rec, opt); !errors.Is(err, ErrLinkScript) {
		t.Fatalf("err = %v, want ErrLinkScript", err)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("left behind: %q", got)
	}
	opt.AllowScripts = []string{"other", "link-scripts"}
	inst, err := Link(ctx, pkgDir, rec, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Scripts/", "Scripts/.link-scripts-post-link.bat", "Scripts/.link-scripts-pre-unlink.bat", "bin/", "bin/.link-scripts-post-link.sh", "bin/.link-scripts-pre-unlink.sh"}
	if got := tree(t, prefix); !reflect.DeepEqual(got, want) {
		t.Errorf("installed %q, want %q (scripts must not run)", got, want)
	}
	if err := WriteCondaMeta(prefix, rec, inst, opt); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(ctx, prefix, rec); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("after Unlink (pre-unlink must not run): %q", got)
	}

	cases := []struct {
		name  string
		spec  pkgSpec
		allow []string
		want  error
	}{
		{"other package's script", pkgSpec{name: "foo", files: []pkgFile{{path: "bin/.bar-post-link.sh", data: "x"}}}, []string{"foo", "bar"}, ErrLinkScript},
		{"case-insensitive", pkgSpec{name: "foo", files: []pkgFile{{path: "BIN/.Foo-Post-Link.SH", data: "x"}}}, nil, ErrLinkScript},
		{"pre-link bat", pkgSpec{name: "foo", files: []pkgFile{{path: "Scripts/.foo-pre-link.bat", data: "x"}}}, nil, ErrLinkScript},
		{"post-unlink", pkgSpec{name: "foo", files: []pkgFile{{path: "bin/.foo-post-unlink.sh", data: "x"}}}, nil, ErrLinkScript},
		{"python-scripts", pkgSpec{name: "foo", noarch: "python", files: []pkgFile{{path: "python-scripts/.foo-pre-unlink.sh", data: "x"}}}, nil, ErrLinkScript},
		{"entry point", pkgSpec{name: "foo", noarch: "python", entryPoints: []string{".foo-post-link.sh = foo:main"}}, nil, ErrLinkScript},
		{"allowed own script", pkgSpec{name: "foo", files: []pkgFile{{path: "bin/.foo-post-link.sh", data: "x"}}}, []string{"foo"}, nil},
		{"other dotfiles", pkgSpec{name: "foo", files: []pkgFile{{path: "bin/.hidden", data: "x"}, {path: "share/.foo-bar.sh", data: "x"}, {path: "bin/sub/.config", data: "x"}}}, nil, nil},
		// Script names are refused anywhere, so that no directory holding
		// one can later become bin.
		{"script name elsewhere", pkgSpec{name: "foo", files: []pkgFile{{path: "share/.foo-post-link.sh", data: "x"}}}, nil, ErrLinkScript},
		{"script name nested in bin", pkgSpec{name: "foo", files: []pkgFile{{path: "bin/sub/.foo-post-link.sh", data: "x"}}}, []string{"foo"}, ErrLinkScript},
		{"allowed script outside bin", pkgSpec{name: "foo", files: []pkgFile{{path: "share/.foo-post-link.sh", data: "x"}}}, []string{"foo"}, ErrLinkScript},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			pkgDir, rec := buildPkg(t, tmp, c.spec)
			_, err := Link(ctx, pkgDir, rec, Options{Prefix: filepath.Join(tmp, "env"), Platform: "linux-64", Python: pythonInfo(), AllowScripts: c.allow})
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func noarchSpec() pkgSpec {
	return pkgSpec{name: "pkg", noarch: "python", entryPoints: []string{"pkg = pkg.mod:main", "pkg-alt = pkg:Class.method"}, files: []pkgFile{
		{path: "info/licenses/LICENSE", data: "MIT", unlisted: true},
		{path: "site-packages/pkg/__init__.py", data: ""},
		{path: "site-packages/pkg/mod.py", data: "def main(): pass\n"},
		{path: "site-packages/pkg-1.0.dist-info/METADATA", data: "Name: pkg\n"},
		{path: "site-packages/pkg/data.pyi", data: ""},
		{path: "python-scripts/pkg-script", data: "#!/opt/anaconda1anaconda2anaconda3/bin/python\n", mode: 0o755, placeholder: "/opt/anaconda1anaconda2anaconda3", fileMode: FileModeText},
		{path: "etc/pkg.conf", data: "x"},
	}}
}

func TestLinkNoarchPython(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), noarchSpec())
	prefix := filepath.Join(tmp, "env")
	opt := Options{Prefix: prefix, Platform: "linux-64", Python: pythonInfo()}
	inst, err := Link(ctx, pkgDir, rec, opt)
	if err != nil {
		t.Fatal(err)
	}
	sp := "lib/python3.11/site-packages/"
	wantFiles := []string{
		sp + "pkg/__init__.py", sp + "pkg/mod.py", sp + "pkg-1.0.dist-info/METADATA", sp + "pkg/data.pyi",
		"bin/pkg-script", "etc/pkg.conf",
		sp + "pkg/__pycache__/__init__.cpython-311.pyc", sp + "pkg/__pycache__/mod.cpython-311.pyc",
		"bin/pkg", "bin/pkg-alt",
	}
	if got := inst.Files(); !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("files:\n got %q\nwant %q", got, wantFiles)
	}
	var types []PathType
	for _, p := range inst.Paths {
		types = append(types, p.PathType)
	}
	wantTypes := []PathType{PathHardlink, PathHardlink, PathHardlink, PathHardlink, PathHardlink, PathHardlink, PathPycFile, PathPycFile, PathUnixPythonEntryPoint, PathUnixPythonEntryPoint}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Errorf("path types %v", types)
	}
	if want := []string{sp + "pkg/__init__.py", sp + "pkg/mod.py"}; !reflect.DeepEqual(inst.PythonSources, want) {
		t.Errorf("PythonSources %q", inst.PythonSources)
	}
	if got := readString(t, filepath.Join(prefix, "bin/pkg-script")); got != "#!"+prefix+"/bin/python\n" {
		t.Errorf("python-scripts file: %q", got)
	}
	if got := readString(t, filepath.Join(prefix, "bin/pkg-alt")); !strings.Contains(got, "from pkg import Class\n") || !strings.Contains(got, "sys.exit(Class.method())\n") {
		t.Errorf("entry point:\n%s", got)
	}
	if _, err := os.Lstat(filepath.Join(prefix, sp, "pkg/__pycache__")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("Link must not create pyc files")
	}
	if inst.Noarch != "python" {
		t.Errorf("Noarch %q", inst.Noarch)
	}

	// Unlink removes the entry points and the empty site-packages tree,
	// whether or not the pyc files were compiled.
	if err := WriteCondaMeta(prefix, rec, inst, opt); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, sp, "pkg/__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, sp, "pkg/__pycache__/mod.cpython-311.pyc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(ctx, prefix, rec); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("after Unlink: %q", got)
	}
}

func TestLinkNoarchPythonErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		extra  string // extra entry point
		mutate func(*Options, *record.PackageRecord)
		want   error
	}{
		{name: "no python", mutate: func(o *Options, r *record.PackageRecord) { o.Python = nil }, want: ErrNoPython},
		{name: "bad python", mutate: func(o *Options, r *record.PackageRecord) { o.Python.SitePackagesPath = "../x" }, want: ErrUnsafePath},
		{name: "noarch from index only", mutate: func(o *Options, r *record.PackageRecord) { o.Python = nil; r.Noarch = "" }, want: ErrNoPython},
		{name: "invalid entry point", extra: "evil = os:system('rm -rf /')", want: ErrInvalidEntryPoint},
		{name: "entry point clobbers package file", extra: "pkg-script = pkg:main", want: ErrInvalidPackage},
		{name: "windows entry points", mutate: func(o *Options, r *record.PackageRecord) {
			o.Platform = "win-64"
			o.Python = &PythonInfo{Version: "3.11", SitePackagesPath: "Lib/site-packages", BinDir: "Scripts", Executable: "python.exe"}
		}, want: ErrUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			spec := noarchSpec()
			if c.extra != "" {
				spec.entryPoints = append(spec.entryPoints, c.extra)
			}
			pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), spec)
			opt := Options{Prefix: filepath.Join(tmp, "env"), Platform: "linux-64", Python: pythonInfo()}
			if c.mutate != nil {
				c.mutate(&opt, rec)
			}
			_, err := Link(ctx, pkgDir, rec, opt)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if got := tree(t, opt.Prefix); len(got) != 0 {
				t.Errorf("left behind: %q", got)
			}
		})
	}
}

// Packages without paths.json use info/files, info/has_prefix and
// info/no_link, as libmamba's read_paths does.
func TestLinkLegacy(t *testing.T) {
	tmp := t.TempDir()
	const ph = "/opt/anaconda1anaconda2anaconda3"
	long := testPlaceholder()
	spec := pkgSpec{name: "old", legacy: true, files: []pkgFile{
		{path: "bin/old", data: "#!" + ph + "/bin/sh\necho " + ph + "\n", mode: 0o755, placeholder: ph, fileMode: FileModeText},
		{path: "lib/libold.so.1", data: "x" + long + "/lib\x00y", placeholder: long, fileMode: FileModeBinary},
		{path: "lib/libold.so", symlink: "libold.so.1"},
		{path: "share/copied", data: "c", noLink: true},
	}}
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), spec)
	prefix := filepath.Join(tmp, "e")
	inst, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"})
	if err != nil {
		t.Fatal(err)
	}
	if got := readString(t, filepath.Join(prefix, "bin/old")); got != "#!"+prefix+"/bin/sh\necho "+prefix+"\n" {
		t.Errorf("bin/old = %q", got)
	}
	if got := readString(t, filepath.Join(prefix, "lib/libold.so.1")); got != "x"+prefix+"/lib"+strings.Repeat("\x00", len(long)-len(prefix)+1)+"y" {
		t.Errorf("binary = %q", got)
	}
	want := []PathRecord{
		{Path: "bin/old", PathType: PathHardlink},
		{Path: "lib/libold.so.1", PathType: PathHardlink},
		{Path: "lib/libold.so", PathType: PathSoftlink},
		{Path: "share/copied", PathType: PathHardlink, NoLink: true, SHA256InPrefix: sha256Hex([]byte("c"))},
	}
	for i := range want {
		got := inst.Paths[i]
		if got.SHA256InPrefix == "" {
			t.Errorf("%s: no digest", got.Path)
		}
		if want[i].SHA256InPrefix == "" {
			got.SHA256InPrefix = ""
		}
		if got != want[i] {
			t.Errorf("paths[%d] = %+v, want %+v", i, got, want[i])
		}
	}
	if inst.Paths[2].SHA256InPrefix != inst.Paths[1].SHA256InPrefix {
		t.Error("symlink digest is not its target's")
	}
	a, _ := os.Stat(filepath.Join(pkgDir, "share/copied"))
	b, _ := os.Stat(filepath.Join(prefix, "share/copied"))
	if os.SameFile(a, b) {
		t.Error("no_link file hard linked")
	}
	// A has_prefix line with only a path uses the default placeholder in
	// text mode.
	if err := os.WriteFile(filepath.Join(pkgDir, "info/has_prefix"), []byte("bin/old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := readPackagePaths(newRoot(pkgDir))
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].PrefixPlaceholder != defaultPrefixPlaceholder || entries[0].FileMode != FileModeText || entries[1].PrefixPlaceholder != "" {
		t.Errorf("entries %+v", entries)
	}
}

// A no_link symlink is copied from the file it points to, as libmamba and
// rattler do.
func TestLinkNoLinkSymlink(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "a", files: []pkgFile{
		{path: "lib/real", data: "content"},
		{path: "lib/alias", symlink: "real", noLink: true},
	}})
	prefix := filepath.Join(tmp, "env")
	inst, err := Link(context.Background(), pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(prefix, "lib/alias"))
	if err != nil || !fi.Mode().IsRegular() || readString(t, filepath.Join(prefix, "lib/alias")) != "content" {
		t.Fatalf("alias: %v %v", fi, err)
	}
	if inst.Paths[1].SHA256InPrefix != sha256Hex([]byte("content")) || !inst.Paths[1].NoLink {
		t.Errorf("record %+v", inst.Paths[1])
	}
}

// Cases from rattler crates/rattler/src/install/link.rs:
// test_symlink_within_prefix_allowed and test_symlink_escape_rejected, plus
// test_missing_symlink_source_is_skipped, where goomba follows libmamba and
// fails instead of skipping.
func TestLinkSymlinkTargets(t *testing.T) {
	for _, c := range []struct {
		dst, target string
		ok          bool
	}{
		{"lib/safe-link", "../bin/real_file", true},
		{"lib/sneaky-link", "../../../../escape_target", false},
		{"lib/self", ".", true},
		{"top", "lib/x", true},
		{"top", "./..", false},
		{"lib/a", "/abs", false},
	} {
		err := checkSymlinkTarget(c.dst, c.target)
		if (err == nil) != c.ok {
			t.Errorf("checkSymlinkTarget(%q, %q) = %v", c.dst, c.target, err)
		}
	}

	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "a", files: []pkgFile{{path: "x", data: "1"}}, info: map[string]string{
		"paths.json": `{"paths": [{"_path": "missing-link", "path_type": "softlink"}], "paths_version": 1}`}})
	if _, err := Link(context.Background(), pkgDir, rec, Options{Prefix: filepath.Join(tmp, "env"), Platform: "linux-64"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing symlink source: %v", err)
	}
}

// Independent packages may be linked into one prefix concurrently; two
// packages claiming the same path cannot both succeed.
func TestLinkConcurrent(t *testing.T) {
	tmp := t.TempDir()
	prefix := filepath.Join(tmp, "env")
	const n = 8
	type pkg struct {
		dir string
		rec *record.PackageRecord
	}
	var pkgs []pkg
	for i := 0; i < n; i++ {
		name := "p" + string(rune('a'+i))
		files := []pkgFile{
			{path: "lib/deep/shared/" + name + ".so", data: name},
			{path: "bin/" + name, data: "#!/bin/sh\n", mode: 0o755},
			{path: "share/common", data: "clash"}, // every package claims this
		}
		dir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: name, files: files})
		pkgs = append(pkgs, pkg{dir, rec})
	}
	errs := make(chan error, n)
	for _, p := range pkgs {
		go func() {
			inst, err := Link(context.Background(), p.dir, p.rec, Options{Prefix: prefix, Platform: "linux-64"})
			if err == nil {
				err = WriteCondaMeta(prefix, p.rec, inst, Options{Prefix: prefix})
			}
			errs <- err
		}()
	}
	ok := 0
	for i := 0; i < n; i++ {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrExists):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d packages claimed share/common", ok)
	}
	recs, err := ReadCondaMeta(prefix)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records %v %v", recs, err)
	}
	want := []string{"bin/", "bin/" + recs[0].Record.Name, "conda-meta/", "conda-meta/" + recs[0].Record.Dist() + ".json",
		"lib/", "lib/deep/", "lib/deep/shared/", "lib/deep/shared/" + recs[0].Record.Name + ".so", "share/", "share/common"}
	if got := tree(t, prefix); !reflect.DeepEqual(got, want) {
		t.Errorf("prefix:\n got %q\nwant %q", got, want)
	}
}
