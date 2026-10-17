package link

import (
	"errors"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// Cases from libmamba tests/src/core/test_link_entry_points.cpp and rattler
// crates/rattler_conda_types/src/package/entry_point.rs (test_entry_point,
// test_entry_point_accepts_legitimate_names).
func TestParseEntryPointValid(t *testing.T) {
	cases := []struct {
		def  string
		want EntryPoint
	}{
		// libmamba
		{"pip = pip._internal.cli.main:main", EntryPoint{"pip", "pip._internal.cli.main", "main"}},
		{`findpython = "findpython.__main__:main"`, EntryPoint{"findpython", "findpython.__main__", "main"}},
		{"cmd = 'pkg.mod:func'", EntryPoint{"cmd", "pkg.mod", "func"}},
		// rattler
		{"jlpm = jupyterlab.jlpmapp:main", EntryPoint{"jlpm", "jupyterlab.jlpmapp", "main"}},
		{"jupyter=jupyterlab.jupyterapp:main", EntryPoint{"jupyter", "jupyterlab.jupyterapp", "main"}},
		{"jupyter-lab = jupyterlab.labapp:main", EntryPoint{"jupyter-lab", "jupyterlab.labapp", "main"}},
		{"pip3.11 = pip._internal.cli.main:main", EntryPoint{"pip3.11", "pip._internal.cli.main", "main"}},
		{"_private = pkg.mod:func", EntryPoint{"_private", "pkg.mod", "func"}},
		{"tool = pkg:Class.method", EntryPoint{"tool", "pkg", "Class.method"}},
		// rattler rejects names starting with '.'; libmamba (primary) only
		// rejects "." and names starting with "..".
		{".hidden = innocuous_pkg.evil:main", EntryPoint{".hidden", "innocuous_pkg.evil", "main"}},
		// Whitespace around every part is stripped.
		{"  wheel\t=\twheel._commands:main \n", EntryPoint{"wheel", "wheel._commands", "main"}},
	}
	for _, c := range cases {
		got, err := ParseEntryPoint(c.def)
		if err != nil {
			t.Errorf("ParseEntryPoint(%q): %v", c.def, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseEntryPoint(%q) = %+v, want %+v", c.def, got, c.want)
		}
	}
}

// Cases from libmamba tests/src/core/test_link_entry_points.cpp and rattler
// entry_point.rs (test_entry_point_rejects_path_traversal_in_command,
// test_entry_point_rejects_python_code_injection).
func TestParseEntryPointInvalid(t *testing.T) {
	cases := []string{
		// libmamba
		`bad/cmd = "..evil:also/bad"`,
		"../../../tmp/PWN = innocuous_pkg.evil:main",
		"../bin/pip = innocuous_pkg.evil:main",
		"/etc/cron.d/zzz = innocuous_pkg.evil:main",
		`C:\evil = innocuous_pkg.evil:main`,
		"C:evil = innocuous_pkg.evil:main",
		"my cmd = pkg.mod:main",
		"pip = ..evil:main",
		"pip = pip.evil:../../main",
		"not-an-entry-point",
		"no-colon = pkg.mod",
		// rattler
		"../../../etc/passwd = innocuous_pkg.evil:main",
		"/tmp/PWN = innocuous_pkg.evil:main",
		`..\..\..\AppData\Roaming\evil = innocuous_pkg.evil:main`,
		"foo/bar = innocuous_pkg.evil:main",
		`foo\bar = innocuous_pkg.evil:main`,
		".. = innocuous_pkg.evil:main",
		". = innocuous_pkg.evil:main",
		"\x00 = innocuous_pkg.evil:main",
		" = innocuous_pkg.evil:main",
		"jlpm = jupyterlab.jlpmapp:main(); __import__('os').system('x')",
		"jlpm = jupyterlab.jlpmapp; __import__('os').system('x'):main",
		"jlpm = ../evil:main",
		"jlpm = jupyterlab.jlpmapp:1main",
		// more
		"a\x07b = pkg:main",
		"..x = pkg:main",
		"x = pkg.:main",
		"x = pkg:",
		"x = :main",
		"x = pkg:ma-in",
		"x = pkg:main\nimport os",
	}
	for _, def := range cases {
		if ep, err := ParseEntryPoint(def); !errors.Is(err, ErrInvalidEntryPoint) {
			t.Errorf("ParseEntryPoint(%q) = %+v, %v; want ErrInvalidEntryPoint", def, ep, err)
		}
	}
}

// Cases from libmamba tests/src/core/test_cpp.cpp, TEST_CASE("python_shebang"),
// and rattler crates/rattler/src/install/python.rs (PythonInfo::shebang).
func TestPythonShebang(t *testing.T) {
	cases := []struct{ exe, platform, want string }{
		{"/usr/bin/python", "linux-64", "#!/usr/bin/python"},
		{"/usr/bin/pyth on with spaces", "linux-64", "#!/bin/sh\n'''exec' \"/usr/bin/pyth on with spaces\" \"$0\" \"$@\" #'''"},
		{"/" + string(make125('a')), "linux-64", "#!/" + string(make125('a'))},
		{"/a" + string(make125('a')), "linux-64", "#!/bin/sh\n'''exec' \"/a" + string(make125('a')) + "\" \"$0\" \"$@\" #'''"},
		{"/a" + string(make125('a')), "osx-arm64", "#!/a" + string(make125('a'))},
	}
	for _, c := range cases {
		if got := PythonShebang(c.exe, c.platform); got != c.want {
			t.Errorf("PythonShebang(%q, %s) = %q, want %q", c.exe, c.platform, got, c.want)
		}
	}
}

// make125 returns 124 bytes, so that "/" + it is 125 bytes long: the
// longest interpreter path a Linux shebang takes directly.
func make125(c byte) []byte {
	b := make([]byte, 124)
	for i := range b {
		b[i] = c
	}
	return b
}

// Cases from libmamba tests/src/core/test_cpp.cpp, TEST_CASE("pyc_path").
func TestPycPath(t *testing.T) {
	cases := []struct{ in, ver, want string }{
		{"lib/site-packages/six.py", "2.7", "lib/site-packages/six.pyc"},
		{"env/lib/python3.10/site-packages/six.py", "3.10", "env/lib/python3.10/site-packages/__pycache__/six.cpython-310.pyc"},
		{"env/lib/python3.13t/site-packages/six.py", "3.13", "env/lib/python3.13t/site-packages/__pycache__/six.cpython-313.pyc"},
		{"lib/python3.13t/site-packages/a.b.py", "3.13t", "lib/python3.13t/site-packages/__pycache__/a.b.cpython-313.pyc"},
	}
	for _, c := range cases {
		if got := PycPath(c.in, c.ver); got != c.want {
			t.Errorf("PycPath(%q, %q) = %q, want %q", c.in, c.ver, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"site-packages/a.py", true},
		{"site-packages/pkg/__init__.py", true},
		{"site-packages/.py", false},
		{"site-packages/a.pyi", false},
		{"python-scripts/a.py", false},
		{"lib/site-packages/a.py", false},
	} {
		if got := isPycCandidate(c.in); got != c.want {
			t.Errorf("isPycCandidate(%q) = %v", c.in, got)
		}
	}
}

// Cases from libmamba tests/src/core/test_transaction_context.cpp,
// TEST_CASE("get_python_noarch_target_path") (Unix layout), and
// TEST_CASE("compute_short_python_version").
func TestNoarchTargetPath(t *testing.T) {
	cases := []struct{ in, sp, bin, want string }{
		{"some_lib/some_folder", "bla", "bin", "some_lib/some_folder"},
		{"site-packages/some_random_package", "target_site_packages_short_path", "bin", "target_site_packages_short_path/some_random_package"},
		{"python-scripts/some_random_file", "target_site_packages_short_path", "bin", "bin/some_random_file"},
		{"python-scripts/some_random_file", "Lib/site-packages", "Scripts", "Scripts/some_random_file"},
	}
	for _, c := range cases {
		if got := NoarchTargetPath(c.in, c.sp, c.bin); got != c.want {
			t.Errorf("NoarchTargetPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, c := range [][2]string{{"", ""}, {"3.5", "3.5"}, {"3.5.0", "3.5"}} {
		if got := ShortPythonVersion(c[0]); got != c[1] {
			t.Errorf("ShortPythonVersion(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// Cases from libmamba tests/src/core/test_transaction_context.cpp:
// effective_python_site_packages_path (all sections, including the 3.14
// free-threading and #4286 cross-platform regressions),
// get_python_short_path and get_python_site_packages_short_path. The
// "empty platform falls back to build platform" section uses the platform
// argument instead of the host.
func TestNewPythonInfo(t *testing.T) {
	cases := []struct {
		version, build, subdir, sp, platform string
		wantSP, wantExe, wantBin             string
	}{
		{"3.13.1", "h9a34b6e_5_cp313t", "linux-64", "lib/python3.13t/site-packages", "", "lib/python3.13t/site-packages", "bin/python3.13t", "bin"},
		{"3.13.1", "h9a34b6e_5_cp313t", "win-64", "lib/python3.13t/site-packages", "", "Lib/site-packages", "python.exe", "Scripts"},
		{"3.13.1", "h9a34b6e_5_cp313t", "win-64", "Lib/site-packages", "", "Lib/site-packages", "python.exe", "Scripts"},
		{"3.13.1", "h9a34b6e_5_cp313t", "linux-64", "lib/python3.13/site-packages", "", "lib/python3.13t/site-packages", "bin/python3.13t", "bin"},
		{"3.14.0", "habcdef_0_cp314t", "win-64", "Lib/site-packages", "", "Lib/site-packages", "python.exe", "Scripts"},
		{"3.14.0", "habcdef_0_cp314t", "linux-64", "lib/python3.14/site-packages", "", "lib/python3.14t/site-packages", "bin/python3.14t", "bin"},
		{"3.14.0", "habcdef_0_cp314t", "win-64", "lib/python3.14t/site-packages", "", "Lib/site-packages", "python.exe", "Scripts"},
		{"3.14.0", "habcdef_0_cp314t", "win-64", "", "", "Lib/site-packages", "python.exe", "Scripts"},
		{"3.14.0", "habcdef_0_cp314t", "linux-64", "", "", "lib/python3.14t/site-packages", "bin/python3.14t", "bin"},
		{"3.14.1", "h4724d56_1_cp313t", "linux-64", "lib/python3.13t/site-packages", "", "lib/python3.13t/site-packages", "bin/python3.14", "bin"},
		{"3.14.1", "h7c1dbca_0_cp314t", "win-64", "Lib/site-packages", "", "Lib/site-packages", "python.exe", "Scripts"},
		{"3.14.0", "habcdef_0_cp314t", "osx-arm64", "", "", "lib/python3.14t/site-packages", "bin/python3.14t", "bin"},
		// Empty subdir: the target platform decides.
		{"3.14.0", "habcdef_0_cp314t", "", "", "win-64", "Lib/site-packages", "python.exe", "Scripts"},
		{"3.14.0", "habcdef_0_cp314t", "", "", "linux-64", "lib/python3.14t/site-packages", "bin/python3.14t", "bin"},
		// Regular builds.
		{"3.11.15", "hd63d673_0_cpython", "linux-64", "", "", "lib/python3.11/site-packages", "bin/python3.11", "bin"},
		{"3.5.0", "h0", "linux-64", "", "", "lib/python3.5/site-packages", "bin/python3.5", "bin"},
		{"3.5.0", "h0", "win-64", "", "", "Lib/site-packages", "python.exe", "Scripts"},
	}
	for _, c := range cases {
		rec := &record.PackageRecord{Name: "python", Version: c.version, Build: c.build, Subdir: c.subdir, PythonSitePackagesPath: c.sp}
		got, err := NewPythonInfo(rec, c.platform)
		if err != nil {
			t.Errorf("%+v: %v", c, err)
			continue
		}
		want := PythonInfo{Version: ShortPythonVersion(c.version), SitePackagesPath: c.wantSP, BinDir: c.wantBin, Executable: c.wantExe}
		if *got != want {
			t.Errorf("NewPythonInfo(%s %s %s %q) = %+v, want %+v", c.version, c.build, c.subdir, c.sp, *got, want)
		}
	}
	for _, v := range []string{"", "3", "x.y", "3.11; rm"} {
		if _, err := NewPythonInfo(&record.PackageRecord{Name: "python", Version: v, Build: "0", Subdir: "linux-64"}, ""); !errors.Is(err, ErrNoPython) {
			t.Errorf("version %q: err = %v, want ErrNoPython", v, err)
		}
	}
	if _, err := NewPythonInfo(nil, "linux-64"); !errors.Is(err, ErrNoPython) {
		t.Errorf("nil record: %v", err)
	}
}

// The script libmamba's python_entry_point_template writes. rattler's
// snapshot (entry_point.rs, test_entry_point_script) indents with tabs;
// micromamba, the reference environment and this port use four spaces.
func TestEntryPointScript(t *testing.T) {
	ep, err := ParseEntryPoint("jupyter-lab = jupyterlab.labapp:main")
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/prefix/bin/python3.11\n" +
		"# -*- coding: utf-8 -*-\n" +
		"import re\n" +
		"import sys\n\n" +
		"from jupyterlab.labapp import main\n\n" +
		"if __name__ == '__main__':\n" +
		"    sys.argv[0] = re.sub(r'(-script\\.pyw?|\\.exe)?$', '', sys.argv[0])\n" +
		"    sys.exit(main())\n"
	if got := entryPointScript(PythonShebang("/prefix/bin/python3.11", "linux-64"), ep); got != want {
		t.Errorf("script:\n%s\nwant\n%s", got, want)
	}
	ep = EntryPoint{Command: "tool", Module: "pkg", Func: "Class.method"}
	if got := entryPointScript("", ep); !strings.HasPrefix(got, "# -*-") || !strings.Contains(got, "from pkg import Class\n") || !strings.Contains(got, "sys.exit(Class.method())") {
		t.Errorf("dotted callable script:\n%s", got)
	}
}

// Cases from rattler crates/rattler/src/install/entry_point.rs
// (test_ensure_entry_point_relative_path_*): destinations are checked with
// validRelPath. rattler normalises "bin/../bin/pip" to "bin/pip"; goomba
// refuses any non-clean path instead.
func TestEntryPointRelativePath(t *testing.T) {
	if err := validRelPath("bin/pip"); err != nil {
		t.Errorf("bin/pip: %v", err)
	}
	for _, p := range []string{"bin/../../etc/passwd", "../etc/passwd", "..", "/tmp/PWN", "bin/../bin/pip"} {
		if err := validRelPath(p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("validRelPath(%q) = %v, want ErrUnsafePath", p, err)
		}
	}
}
