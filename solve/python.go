// Ported from libmamba src/api/utils.cpp (extract_requested_python_minor)
// and src/core/shard_python_minor_prefilter.cpp
// (version_from_single_equality_spec, relax_version_spec_to_minor)
// (BSD-3-Clause, see NOTICE).

package solve

import (
	"strings"

	"github.com/richinsley/goomba/spec"
)

// RequestedPythonMinor returns the python minor series, e.g. "3.11", that
// specs pin python to, or "" if they do not pin one. It is a port of
// libmamba's extract_requested_python_minor: the first spec named "python"
// whose version is a single equality ("python=3.11", "python 3.12.5",
// "python ==3.11", "python 3.7.12 0_73_pypy") gives the minor of that
// version; a pin on a major version only ("python=3") stops the search
// with no answer, and specs of any other shape are skipped.
//
// The caller passes the result to repodata.Options.PythonMinor when
// opening channels, so records whose python dependency excludes every
// version of the series X.Y.* from X.Y.0a0 up are dropped while loading.
// That loses no solution only if every python the pin admits is in the
// series, so, unlike libmamba, RequestedPythonMinor returns "" for a pin
// that sorts below X.Y (a pre-release such as "python==3.11.0rc1",
// "python=3.11.0a1" or "python=3.11.dev0") and for a minor that is not
// MAJOR.MINOR in decimal (an epoch or local version in the pin). One gap
// remains, shared with conda-forge's "<X.Y.0a0" bounds: a pin such as
// "python=3.11" also admits the versions starting with 3.11 that sort
// below 3.11.0a0, which are outside the series: development builds such as
// 3.11.0dev0 and 3.11.dev0, and spellings such as 3.11a1 or 3.11rc1.
func RequestedPythonMinor(specs []string) string {
	for _, s := range specs {
		name, err := spec.ExtractName(s)
		if err != nil || name != "python" {
			continue
		}
		ms, err := spec.ParseMatchSpec(s)
		if err != nil || ms.Version.IsAny() {
			// libmamba's free version spec has no predicate at all.
			continue
		}
		raw := ms.Version.String()
		pinned, single := singleEqualityVersion(raw)
		// A pin on the major version only ("python=3") would relax to
		// "3.0", which says nothing about the minor; libmamba gives up.
		if single && versionParts(pinned) <= 1 {
			return ""
		}
		if v, ok := singleEqualityVersion(relaxToMinor(raw)); ok {
			minor := v.String()
			if !isMajorMinor(minor) || (single && pinned.Compare(v) < 0) {
				// Not a plain minor, or a pre-release pin below X.Y.
				return ""
			}
			return minor
		}
	}
	return ""
}

// equalityTail returns the version of a single "==V" or "=V" predicate as
// libmamba prints it.
func equalityTail(vs string) (string, bool) {
	if strings.ContainsAny(vs, ",|()") {
		return "", false // not a single predicate
	}
	switch {
	case strings.HasPrefix(vs, "=="):
		return strings.TrimLeft(vs[2:], " "), true
	case strings.HasPrefix(vs, "="):
		return strings.TrimLeft(vs[1:], " "), true
	}
	return "", false
}

// singleEqualityVersion is libmamba's version_from_single_equality_spec:
// the version of a version spec made of one "==V" or "=V" predicate.
func singleEqualityVersion(vs string) (spec.Version, bool) {
	tail, ok := equalityTail(vs)
	if !ok {
		return spec.Version{}, false
	}
	v, err := spec.ParseVersion(tail)
	if err != nil {
		return spec.Version{}, false
	}
	return v, true
}

// relaxToMinor is libmamba's relax_version_spec_to_minor: a single
// equality predicate becomes "==X.Y" for its first two parts; any other
// spec is returned as it is.
func relaxToMinor(vs string) string {
	v, ok := singleEqualityVersion(vs)
	if !ok {
		return vs
	}
	minor := versionLevel2(v.String())
	if _, err := spec.ParseVersion(minor); err != nil {
		return vs
	}
	return "==" + minor
}

// versionLevel2 formats a normalized version string with exactly two parts
// in the version and in the local version, padding with "0", as libmamba's
// Version::to_string(2).
func versionLevel2(s string) string {
	epoch := ""
	if i := strings.IndexByte(s, '!'); i >= 0 {
		epoch, s = s[:i+1], s[i+1:]
	}
	local := ""
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s, local = s[:i], s[i+1:]
	}
	out := epoch + firstParts(s, 2)
	if local != "" {
		out += "+" + firstParts(local, 2)
	}
	return out
}

// firstParts returns the first n dot-separated parts of s, padded with "0".
func firstParts(s string, n int) string {
	parts := strings.Split(s, ".")
	for len(parts) < n {
		parts = append(parts, "0")
	}
	return strings.Join(parts[:n], ".")
}

// versionParts counts the dot-separated parts of a version, ignoring its
// epoch and local version.
func versionParts(v spec.Version) int {
	s := v.String()
	if i := strings.IndexByte(s, '!'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	return strings.Count(s, ".") + 1
}

// isMajorMinor reports whether s is "X.Y" with decimal numbers.
func isMajorMinor(s string) bool {
	major, minor, ok := strings.Cut(s, ".")
	return ok && isDigits(major) && isDigits(minor)
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
