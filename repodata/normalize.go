// Record normalization is ported from libmamba src/specs/repo_data.cpp
// (from_json), src/core/shards.cpp (parse_shard_package_record) and
// src/core/shard_types.cpp (to_package_info) (BSD-3-Clause, see NOTICE).
// The .conda-over-.tar.bz2 preference and the "removed" filter follow
// rattler crates/rattler_repodata_gateway/src/sparse.

package repodata

import (
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/richinsley/goomba/record"
)

const (
	extConda  = ".conda"
	extTarBz2 = ".tar.bz2"
)

// locator places records of one channel subdir: it fills Fn, URL, Channel
// and Subdir.
type locator struct {
	channel string   // canonical channel URL, no subdir
	subdir  string   // e.g. "linux-64"
	base    *url.URL // package base URL, ends in '/'
}

func (l *locator) place(r *record.PackageRecord, fn string) {
	r.Fn = fn
	r.URL = joinFile(l.base, fn)
	r.Channel = l.channel
	r.Subdir = l.subdir
}

// validIdent reports whether s may be used as a package name, version or
// build string. These end up in file names (Dist), so path separators,
// control characters, whitespace and characters Windows forbids are refused.
func validIdent(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c < 0x20 || c == 0x7f:
			return false
		case c == ' ', c == '/', c == '\\', c == ':', c == '*', c == '?', c == '"', c == '<', c == '>', c == '|':
			return false
		}
	}
	return s != "." && s != ".."
}

// validFilename reports whether fn is a safe archive file name with the
// extension its index section requires.
func validFilename(fn string, conda bool) bool {
	ext := extTarBz2
	if conda {
		ext = extConda
	}
	return len(fn) > len(ext) && strings.HasSuffix(fn, ext) && validIdent(fn)
}

// validSitePackagesPath accepts a relative, forward-slash path without empty,
// "." or ".." segments, such as "lib/python3.13t/site-packages". Package
// linking uses it to place files, so anything that could escape the prefix
// is refused.
func validSitePackagesPath(p string) bool {
	if p == "" || !utf8.ValidString(p) || strings.ContainsAny(p, "\\:\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for i := 0; i < len(seg); i++ {
			if seg[i] < 0x20 || seg[i] == 0x7f {
				return false
			}
		}
	}
	return true
}

// normalizeHex lowercases a hex digest and checks it encodes n bytes.
func normalizeHex(s string, n int) (string, bool) {
	if len(s) != 2*n {
		return "", false
	}
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
			b[i] = c + ('a' - 'A')
		default:
			return "", false
		}
	}
	return string(b), true
}

// splitTrackFeatures splits a track_features string on commas and
// whitespace, dropping empty entries.
func splitTrackFeatures(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(f) == 0 {
		return nil
	}
	return f
}

// cleanTrackFeatures trims list entries and drops empty ones.
func cleanTrackFeatures(list []string) []string {
	var out []string
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// noarchString maps the string form of the noarch field.
func noarchString(s string) (string, bool) {
	switch s {
	case "", "python", "generic":
		return s, true
	}
	return "", false
}

// validStrings reports whether every entry is valid UTF-8 without NUL.
func validStrings(list []string) bool {
	for _, s := range list {
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			return false
		}
	}
	return true
}

// checkRecord validates the fields every record needs. Decoders call it after
// filling a record; a false result drops the record.
func checkRecord(r *record.PackageRecord) bool {
	return validIdent(r.Name) && validIdent(r.Version) && validIdent(r.Build) &&
		validSpecs(r.Depends) && validSpecs(r.Constrains) && validStrings(r.TrackFeatures) &&
		r.Size >= 0 && r.Timestamp >= 0
}

// validSpecs is validStrings plus: no blank entries. An empty dependency
// cannot be honoured, and ignoring it would not fail closed.
func validSpecs(list []string) bool {
	for _, s := range list {
		if strings.TrimSpace(s) == "" {
			return false
		}
	}
	return validStrings(list)
}

// finalize drops records listed in removed and, when the same
// name-version-build exists as .conda and .tar.bz2 in one subdir, keeps only
// the .conda record. The result is sorted by subdir and file name.
func finalize(recs []*record.PackageRecord, removed map[string]struct{}) []*record.PackageRecord {
	type key struct{ subdir, dist string }
	conda := make(map[key]struct{})
	out := recs[:0:0]
	for _, r := range recs {
		if _, gone := removed[r.Fn]; gone {
			continue
		}
		out = append(out, r)
		if strings.HasSuffix(r.Fn, extConda) {
			conda[key{r.Subdir, r.Dist()}] = struct{}{}
		}
	}
	kept := out[:0]
	for _, r := range out {
		if strings.HasSuffix(r.Fn, extTarBz2) {
			if _, dup := conda[key{r.Subdir, r.Dist()}]; dup {
				continue
			}
		}
		kept = append(kept, r)
	}
	sortRecords(kept)
	return kept
}

func sortRecords(recs []*record.PackageRecord) {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Subdir != recs[j].Subdir {
			return recs[i].Subdir < recs[j].Subdir
		}
		return recs[i].Fn < recs[j].Fn
	})
}

// specName returns the package name a match spec string refers to, with the
// cheap split libmamba's MatchSpec::extract_name uses: skip leading
// whitespace, stop at whitespace or a version operator. A channel prefix
// ("conda-forge::python") is removed and the name lowercased. It returns ""
// for free specs ("*") and for strings without a usable name.
func specName(spec string) string {
	s := strings.TrimLeft(spec, " \t\r\n")
	if i := strings.IndexAny(s, " \t\r\n<>=!~[(,|"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	if s == "" || s == "*" || strings.ContainsAny(s, "/\\:*") || !utf8.ValidString(s) {
		return ""
	}
	return strings.ToLower(s)
}

// dependencyNames returns the sorted, de-duplicated package names named by
// the depends of recs. Constrains are not followed: they only matter for
// packages something else depends on.
func dependencyNames(recs []*record.PackageRecord) []string {
	seen := make(map[string]struct{})
	for _, r := range recs {
		for _, d := range r.Depends {
			if n := specName(d); n != "" {
				seen[n] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
