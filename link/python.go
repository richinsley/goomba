// Ported from libmamba src/core/link.cpp (parse_entry_point,
// check_entry_point_command, check_python_identifier_chain,
// python_entry_point_template, python_shebang, pyc_path) and
// src/core/transaction_context.cpp (compute_short_python_version,
// effective_python_site_packages_path, build_python_params,
// get_python_noarch_target_path) (BSD-3-Clause, see NOTICE).

package link

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/richinsley/goomba/record"
)

// PythonInfo describes the Python interpreter of the target environment; it
// is required to link noarch: python packages. All paths are relative to the
// prefix and slash-separated. PythonInfo is a plain value and safe for
// concurrent use when not modified.
type PythonInfo struct {
	// Version is the short Python version, e.g. "3.11" (major.minor).
	Version string
	// SitePackagesPath is where site-packages/ files go, e.g.
	// "lib/python3.11/site-packages" or the record's python_site_packages_path.
	SitePackagesPath string
	// BinDir is where python-scripts/ files and entry points go: "bin" on
	// Unix, "Scripts" on Windows.
	BinDir string
	// Executable is the interpreter used in entry point shebangs, e.g.
	// "bin/python3.11".
	Executable string
}

// ShortPythonVersion returns "major.minor" for a Python version string, or
// the input unchanged when it has fewer than two components.
func ShortPythonVersion(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// NewPythonInfo derives the PythonInfo for an environment whose interpreter
// is the given python package record. The layout depends on the record's
// subdir; platform (the target subdir) is used when the record has none.
// Free-threaded builds (build string containing "cpXYt") use
// lib/pythonX.Yt/site-packages and bin/pythonX.Yt on Unix; Windows always
// uses Lib/site-packages and python.exe.
func NewPythonInfo(python *record.PackageRecord, platform string) (*PythonInfo, error) {
	if python == nil {
		return nil, fmt.Errorf("link: python info: %w: nil python record", ErrNoPython)
	}
	short := ShortPythonVersion(python.Version)
	if !validPythonVersion(short) {
		return nil, fmt.Errorf("link: python info: %w: unusable python version %q", ErrNoPython, python.Version)
	}
	if python.Subdir != "" && python.Subdir != "noarch" {
		platform = python.Subdir
	}
	win := isWindows(platform)
	sp := effectiveSitePackagesPath(short, python.Build, python.PythonSitePackagesPath, win)
	info := &PythonInfo{Version: short, BinDir: "bin", Executable: "bin/python" + short}
	if win {
		info.BinDir = "Scripts"
		info.Executable = "python.exe"
	}
	if sp != "" {
		info.SitePackagesPath = sp
		if !win && strings.Contains(sp, "python"+short+"t") {
			info.Executable = "bin/python" + short + "t"
		}
	} else if win {
		info.SitePackagesPath = "Lib/site-packages"
	} else {
		info.SitePackagesPath = "lib/python" + short + "/site-packages"
	}
	if err := info.validate(); err != nil {
		return nil, err
	}
	return info, nil
}

// effectiveSitePackagesPath ports effective_python_site_packages_path.
func effectiveSitePackagesPath(short, build, recorded string, win bool) string {
	compact := strings.ReplaceAll(short, ".", "")
	freethreaded := build != "" && compact != "" && strings.Contains(build, "cp"+compact+"t")
	missing := short == "" || build == ""
	var ftUnix, stdWin, stdUnix string
	if !missing {
		ftUnix = "lib/python" + short + "t/site-packages"
		stdWin = "Lib/site-packages"
		stdUnix = "lib/python" + short + "/site-packages"
	}
	ft, std := ftUnix, stdUnix
	if win {
		ft, std = stdWin, stdWin
	}
	if recorded != "" {
		if freethreaded && ft != "" {
			if recorded == std {
				return ft
			}
			if win && recorded == ftUnix {
				return ft
			}
		}
		return recorded
	}
	if missing || !freethreaded {
		return ""
	}
	return ft
}

// validPythonVersion accepts "major.minor" with an optional free-threading
// "t" suffix.
func validPythonVersion(v string) bool {
	major, minor, ok := strings.Cut(strings.TrimSuffix(v, "t"), ".")
	return ok && isDigits(major) && isDigits(minor) && len(v) <= 16
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (p *PythonInfo) validate() error {
	if !validPythonVersion(p.Version) {
		return fmt.Errorf("link: python info: %w: bad version %q", ErrNoPython, p.Version)
	}
	for _, f := range []struct{ name, val string }{
		{"site-packages path", p.SitePackagesPath},
		{"bin dir", p.BinDir},
		{"executable", p.Executable},
	} {
		if err := validRelPath(f.val); err != nil {
			return fmt.Errorf("link: python info: %s: %w", f.name, err)
		}
	}
	return nil
}

// NoarchTargetPath maps a path inside a noarch: python package to its
// location in the prefix: "site-packages/x" goes below sitePackages,
// "python-scripts/x" below binDir, anything else is unchanged.
func NoarchTargetPath(p, sitePackages, binDir string) string {
	if rest, ok := strings.CutPrefix(p, "site-packages/"); ok {
		return sitePackages + "/" + rest
	}
	if rest, ok := strings.CutPrefix(p, "python-scripts/"); ok {
		return binDir + "/" + rest
	}
	return p
}

// PycPath returns where CPython caches the bytecode of the .py file p for
// the given short Python version: next to it for Python 2, and
// __pycache__/<stem>.cpython-<XY>.pyc for Python 3 (free-threaded "t"
// suffixes are not part of the cache tag).
func PycPath(p, pyVersion string) string {
	if strings.HasPrefix(pyVersion, "2") {
		return p + "c"
	}
	dir, file := path.Split(p)
	stem := file
	if i := strings.LastIndexByte(file, '.'); i > 0 {
		stem = file[:i]
	}
	tag := strings.ReplaceAll(strings.ReplaceAll(pyVersion, ".", ""), "t", "")
	return dir + "__pycache__/" + stem + ".cpython-" + tag + ".pyc"
}

// isPycCandidate mirrors libmamba's py_file_re
// "^site-packages[/\\][^\t\n\r\f\v]+\.py$" on the package-relative path.
func isPycCandidate(p string) bool {
	rest, ok := strings.CutPrefix(p, "site-packages/")
	if !ok {
		rest, ok = strings.CutPrefix(p, `site-packages\`)
	}
	if !ok || !strings.HasSuffix(rest, ".py") || len(rest) < len(".py")+1 {
		return false
	}
	return !strings.ContainsAny(rest, "\t\n\r\f\v")
}

// PythonShebang returns the shebang line (without newline) that runs a
// script with the interpreter at the absolute path exe. Paths longer than
// the platform's shebang limit minus two, or containing a space, use a
// /bin/sh trampoline instead.
func PythonShebang(exe, platform string) string {
	if len(exe) > maxShebangLength(platform)-2 || strings.Contains(exe, " ") {
		return "#!/bin/sh\n'''exec' \"" + exe + "\" \"$0\" \"$@\" #'''"
	}
	return "#!" + exe
}

// EntryPoint is a parsed noarch: python console script declaration
// "command = module:callable". It is a plain value, safe for concurrent use.
type EntryPoint struct {
	Command string // file name created in the bin directory
	Module  string // dotted module path to import from
	Func    string // dotted callable path inside Module
}

// String returns the entry point in "command = module:func" form.
func (e EntryPoint) String() string { return e.Command + " = " + e.Module + ":" + e.Func }

// ParseEntryPoint parses and validates an entry point definition the way
// libmamba's parse_entry_point does: split on the first '=', strip
// whitespace and quotes from the right-hand side, split that on the last
// ':'. The command must be a simple file name and module and callable must
// be dotted chains of ASCII Python identifiers, so the definition can
// neither escape the bin directory nor inject code into the script. All
// problems are reported together.
func ParseEntryPoint(def string) (EntryPoint, error) {
	command, rhs, ok := strings.Cut(def, "=")
	if !ok {
		return EntryPoint{}, fmt.Errorf("link: entry point %q: %w: missing '='", def, ErrInvalidEntryPoint)
	}
	rhs = strings.Trim(rhs, " \t\r\n\"'")
	i := strings.LastIndexByte(rhs, ':')
	if i < 0 {
		return EntryPoint{}, fmt.Errorf("link: entry point %q: %w: missing ':'", def, ErrInvalidEntryPoint)
	}
	ep := EntryPoint{
		Command: stripNonGraphic(command),
		Module:  stripNonGraphic(rhs[:i]),
		Func:    stripNonGraphic(rhs[i+1:]),
	}
	var errs []error
	if err := checkEntryPointCommand(ep.Command); err != nil {
		errs = append(errs, err)
	}
	if !isIdentifierChain(ep.Module) {
		errs = append(errs, fmt.Errorf("module %q is not a dotted chain of Python identifiers", ep.Module))
	}
	if !isIdentifierChain(ep.Func) {
		errs = append(errs, fmt.Errorf("callable %q is not a dotted chain of Python identifiers", ep.Func))
	}
	if len(errs) > 0 {
		return EntryPoint{}, fmt.Errorf("link: entry point %q: %w: %w", def, ErrInvalidEntryPoint, errors.Join(errs...))
	}
	return ep, nil
}

// stripNonGraphic mirrors libmamba's util::strip, which removes leading and
// trailing bytes outside the printable ASCII range 0x21-0x7e.
func stripNonGraphic(s string) string {
	graphic := func(c byte) bool { return c > 0x20 && c < 0x7f }
	i, j := 0, len(s)
	for i < j && !graphic(s[i]) {
		i++
	}
	for j > i && !graphic(s[j-1]) {
		j--
	}
	return s[i:j]
}

// checkEntryPointCommand ports check_entry_point_command.
func checkEntryPointCommand(cmd string) error {
	if cmd == "" || cmd == "." || cmd == ".." {
		return fmt.Errorf("command %q must be a simple file name", cmd)
	}
	if strings.ContainsAny(cmd, "/\\\n\r\t ") || strings.IndexFunc(cmd, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("command %q contains a path separator, whitespace or control character", cmd)
	}
	if strings.HasPrefix(cmd, "..") {
		return fmt.Errorf("command %q must not start with '..'", cmd)
	}
	if hasDriveLetter(cmd) {
		return fmt.Errorf("command %q is a Windows drive path", cmd)
	}
	if !utf8.ValidString(cmd) {
		return fmt.Errorf("command %q is not valid UTF-8", cmd)
	}
	return nil
}

// isIdentifierChain matches ^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$.
func isIdentifierChain(s string) bool {
	if s == "" {
		return false
	}
	for part := range strings.SplitSeq(s, ".") {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			alpha := 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || c == '_'
			if !alpha && (i == 0 || c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}

// entryPointScript renders libmamba's python_entry_point_template behind the
// given shebang line.
func entryPointScript(shebang string, ep EntryPoint) string {
	importName, _, _ := strings.Cut(ep.Func, ".")
	var b strings.Builder
	if shebang != "" {
		b.WriteString(shebang)
		b.WriteString("\n")
	}
	b.WriteString("# -*- coding: utf-8 -*-\n")
	b.WriteString("import re\n")
	b.WriteString("import sys\n\n")
	b.WriteString("from " + ep.Module + " import " + importName + "\n\n")
	b.WriteString("if __name__ == '__main__':\n")
	b.WriteString("    sys.argv[0] = re.sub(r'(-script\\.pyw?|\\.exe)?$', '', sys.argv[0])\n")
	b.WriteString("    sys.exit(" + ep.Func + "())\n")
	return b.String()
}
