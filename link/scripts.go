package link

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// foldName reduces a file name to the form used to compare it with reserved
// names. Invisible code points (format and control characters, variation
// selectors and other default-ignorable code points, which HFS+ ignores)
// are dropped, and every other code point is case-folded with Unicode
// simple folding, so that case-insensitive file systems (APFS, NTFS, ext4
// with casefolding) cannot make "Conda-Meta", "bın" or ".x-poſt-linK.sh"
// name a reserved entry unnoticed. Folding may over-match, which only makes
// the checks stricter.
func foldName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isInvisible(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(unicode.ToUpper(r)))
	}
	return b.String()
}

func isInvisible(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Variation_Selector, r)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// isScriptDirName reports whether a top-level directory name is where conda
// and mamba look for link scripts: bin (Unix) or Scripts (Windows).
func isScriptDirName(name string) bool {
	f := foldName(name)
	return f == "bin" || f == "scripts"
}

// scriptLeafOwner reports whether a file name is a link script name,
// ".<name>-<action>.sh" or ".<name>-<action>.bat" after folding, and for
// which package (folded).
func scriptLeafOwner(leaf string) (string, bool) {
	lower := foldName(leaf)
	if !strings.HasPrefix(lower, ".") {
		return "", false
	}
	var stem string
	switch {
	case strings.HasSuffix(lower, ".sh"):
		stem = lower[1 : len(lower)-len(".sh")]
	case strings.HasSuffix(lower, ".bat"):
		stem = lower[1 : len(lower)-len(".bat")]
	default:
		return "", false
	}
	// conda and mamba run bin/.<name>-<action>.sh (Unix) and
	// Scripts/.<name>-<action>.bat (Windows).
	for _, action := range [...]string{"pre-link", "post-link", "pre-unlink", "post-unlink"} {
		if name, ok := strings.CutSuffix(stem, "-"+action); ok && name != "" {
			return name, true
		}
	}
	return "", false
}

// suspiciousScriptDirName reports a name that must not appear in bin or
// Scripts because a file system might take it for a link script: a name
// starting with "." (after folding) that is not plain ASCII.
func suspiciousScriptDirName(leaf string) bool {
	return !isASCII(leaf) && strings.HasPrefix(foldName(leaf), ".")
}

// checkScriptName applies the link-script rules to a destination: a file
// named like a link script is only accepted for the package's own script,
// when the package is allowed scripts, as a regular file directly in the
// top-level bin or Scripts directory. Anywhere else such a name is refused,
// so that a directory cannot later be turned into bin with a script inside.
// inScriptDir tells whether the destination's directory is bin or Scripts.
func checkScriptName(pkgName string, allow []string, leaf string, kind entryKind, canonical, inScriptDir bool) bool {
	if owner, ok := scriptLeafOwner(leaf); ok {
		return canonical && kind == kindFile && scriptAllowed(pkgName, owner, allow)
	}
	return !(inScriptDir && suspiciousScriptDirName(leaf))
}

// scriptAllowed reports whether a link script for scriptName may be
// installed by package pkgName: only a package's own scripts, and only for
// packages listed in allow.
func scriptAllowed(pkgName, scriptName string, allow []string) bool {
	if foldName(pkgName) != scriptName {
		return false
	}
	for _, a := range allow {
		if a == pkgName {
			return true
		}
	}
	return false
}

// windowsAlias reports a path component that Windows would resolve to
// another name: trailing dots or spaces (stripped by Win32), a colon (an
// alternate data stream), or an 8.3 short name ("CONDA-~1", "SCRIPT~1")
// that could stand for conda-meta or Scripts.
func windowsAlias(c string) bool {
	if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") || strings.ContainsRune(c, ':') {
		return true
	}
	if i := strings.IndexByte(c, '~'); i >= 0 && i+1 < len(c) && '0' <= c[i+1] && c[i+1] <= '9' {
		base := foldName(c[:i])
		return strings.HasPrefix("conda-meta", base) || strings.HasPrefix("scripts", base)
	}
	return false
}
