// Ported from libmamba src/specs/unresolved_channel.cpp, src/specs/archive.cpp
// and the name/URL parts of src/specs/channel.cpp (BSD-3-Clause, see NOTICE).

package spec

import (
	"path"
	"sort"
	"strconv"
	"strings"
)

// DefaultChannelAlias is the URL conda prepends to channel names such as
// "conda-forge".
const DefaultChannelAlias = "https://conda.anaconda.org"

// anacondaRepo hosts the "pkgs/..." channels behind "defaults".
const anacondaRepo = "https://repo.anaconda.com"

// isAnacondaChannel reports whether name is, or is under, one of conda's
// built-in custom channels, which are served from anacondaRepo rather than
// the channel alias: "pkgs/main", "pkgs/r", "pkgs/pro" and "pkgs/msys2".
func isAnacondaChannel(name string) bool {
	rest, ok := strings.CutPrefix(name, "pkgs/")
	if !ok {
		return false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	switch rest {
	case "main", "r", "pro", "msys2":
		return true
	}
	return false
}

type channelKind uint8

const (
	chanUnknown channelKind = iota
	chanName
	chanURL
	chanPackageURL
	chanPath
	chanPackagePath
)

// unresolvedChannel is libmamba's UnresolvedChannel: a channel as written,
// split into its location and platform filters.
type unresolvedChannel struct {
	location  string
	platforms []string // sorted, unique
	kind      channelKind
}

func (c unresolvedChannel) isPackage() bool {
	return c.kind == chanPackageURL || c.kind == chanPackagePath
}

// String returns the location followed by any platform filters, as in
// "conda-forge[linux-64,noarch]".
func (c unresolvedChannel) String() string {
	if len(c.platforms) == 0 {
		return c.location
	}
	return c.location + "[" + strings.Join(c.platforms, ",") + "]"
}

const unknownChannel = "<unknown>"

func isUnknownChannel(s string) bool {
	switch asciiLower(s) {
	case "", "<unknown>", "none:///<unknown>", "none", ":///<unknown>":
		return true
	}
	return false
}

// archiveExtension returns the package archive suffix of p that libmamba
// recognizes (".tar.bz2", ".conda", ".whl" or ".tar.gz"), or "".
func archiveExtension(p string) string {
	for _, ext := range [...]string{".tar.bz2", ".conda", ".whl", ".tar.gz"} {
		if strings.HasSuffix(p, ext) {
			return ext
		}
	}
	return ""
}

func hasArchiveExtension(p string) bool { return archiveExtension(p) != "" }

func stripArchiveExtension(p string) string {
	return p[:len(p)-len(archiveExtension(p))]
}

// urlScheme returns the scheme of a "scheme://..." string, or "".
func urlScheme(u string) string {
	sep := strings.Index(u, "://")
	if sep <= 0 {
		return ""
	}
	for i := 0; i < sep; i++ {
		c := u[i]
		if !isAlnum(c) && c != '.' && c != '-' && c != '_' {
			return ""
		}
	}
	return u[:sep]
}

// isExplicitPath reports whether s is clearly a filesystem path.
func isExplicitPath(s string) bool {
	if urlScheme(s) != "" {
		return false
	}
	if strings.HasPrefix(s, "~") || strings.HasPrefix(s, "/") || s == "." ||
		strings.HasPrefix(s, "./") || s == ".." || strings.HasPrefix(s, "../") {
		return true
	}
	return len(s) >= 3 && isAlpha(s[0]) && s[1] == ':' && (s[2] == '/' || s[2] == '\\')
}

// parsePlatformList splits "linux-64,noarch" (separators ",", "|", ";")
// into a sorted set of lower-case platforms. Any name is accepted.
func parsePlatformList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '|' || r == ';' }) {
		if p := asciiLower(strip(f)); p != "" {
			out = append(out, p)
		}
	}
	return sortedUnique(out)
}

func sortedUnique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	sort.Strings(s)
	out := s[:1]
	for _, x := range s[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// findPlatformInPath finds the first "/<platform>" path component, as
// libmamba's find_slash_and_platform. It returns the index of the slash,
// the end of the component and the normalized platform.
func findPlatformInPath(p string) (start, end int, plat string) {
	start = 0
	for start >= 0 {
		next := -1
		if start+1 <= len(p) {
			if k := strings.IndexByte(p[start+1:], '/'); k >= 0 {
				next = start + 1 + k
			}
		}
		compEnd := next
		if compEnd < 0 {
			compEnd = len(p)
		}
		if start+1 <= compEnd {
			if pl, ok := ParsePlatform(p[start+1 : compEnd]); ok {
				return start, compEnd, pl
			}
		}
		start = next
	}
	return -1, -1, ""
}

func splitLocationPlatform(str string) (string, []string, string) {
	if strings.HasSuffix(str, "]") {
		open := strings.LastIndexByte(str, '[')
		if open <= 0 {
			return "", nil, `unexpected closing bracket "]"`
		}
		return rstrip(str[:open]), parsePlatformList(str[open+1 : len(str)-1]), ""
	}
	if !hasArchiveExtension(str) {
		if start, end, plat := findPlatformInPath(str); plat != "" {
			rest := strings.TrimRight(str[:start]+str[end:], "/")
			return rest, []string{plat}, ""
		}
	}
	return rstrip(str), nil, ""
}

// normalizePath cleans a filesystem path lexically, keeping a leading "./".
func normalizePath(p string) string {
	out := path.Clean(p)
	if strings.HasPrefix(p, "./") && !strings.HasPrefix(out, ".") {
		out = "./" + out
	}
	if out != "/" {
		out = strings.TrimRight(out, "/")
	}
	return out
}

func parseUnresolvedChannel(s string) (unresolvedChannel, error) {
	if len(s) > maxSpecLen {
		return unresolvedChannel{}, parseErr(ErrChannel, truncate(s), "longer than "+strconv.Itoa(maxSpecLen)+" bytes")
	}
	str := strip(s)
	if isUnknownChannel(str) {
		return unresolvedChannel{location: unknownChannel, kind: chanUnknown}, nil
	}
	location, platforms, msg := splitLocationPlatform(str)
	if msg != "" {
		return unresolvedChannel{}, parseErr(ErrChannel, s, msg)
	}
	var kind channelKind
	switch scheme := urlScheme(location); {
	case scheme == "file":
		kind = chanPath
		if hasArchiveExtension(location) {
			kind = chanPackagePath
		}
	case scheme != "":
		kind = chanURL
		if hasArchiveExtension(location) {
			kind = chanPackageURL
		}
	case isExplicitPath(location) || hasArchiveExtension(location):
		location = normalizePath(location)
		kind = chanPath
		if hasArchiveExtension(location) {
			kind = chanPackagePath
		}
	default:
		kind = chanName
	}
	if location == "" {
		return unresolvedChannel{}, parseErr(ErrChannel, s, "empty location")
	}
	// Names and URLs never contain whitespace, quotes, brackets or '#'
	// (URLs percent-encode them); these would also break match spec syntax.
	for i := 0; i < len(location); i++ {
		if c := location[i]; !isGraphic(c) || strings.IndexByte(`"'[]()#`, c) >= 0 {
			return unresolvedChannel{}, parseErr(ErrChannel, s, "invalid character "+strconv.Quote(location[i:i+1]))
		}
	}
	if msg := checkCredentials(location); msg != "" {
		return unresolvedChannel{}, parseErr(ErrChannel, s, msg)
	}
	return unresolvedChannel{location: location, platforms: platforms, kind: kind}, nil
}

// Channel is a channel resolved to its canonical base URL.
//
// A Channel is a plain value; it is safe for concurrent use as long as no
// goroutine modifies it.
type Channel struct {
	// URL is the canonical base URL, without platform subdir or trailing
	// slash, e.g. "https://conda.anaconda.org/conda-forge". It is the form
	// stored in record.PackageRecord.Channel.
	URL string
	// Name is the display name: the channel name when the URL lies under
	// the channel alias ("conda-forge", "conda-forge/label/dev",
	// "pkgs/main"), otherwise the URL.
	Name string
	// Platforms holds the platform filters written with the channel, as in
	// "conda-forge/linux-64" or "conda-forge[linux-64,noarch]", sorted. It
	// is nil when the channel was written without filters.
	Platforms []string
}

// ParseChannel resolves a channel written as a name ("conda-forge",
// "conda-forge/label/dev", "pkgs/main") or a URL ("https://...",
// "file:///...") against DefaultChannelAlias. See ParseChannelAlias.
func ParseChannel(s string) (Channel, error) {
	return ParseChannelAlias(s, DefaultChannelAlias)
}

// ParseChannelAlias resolves a channel against the given channel alias
// (e.g. "https://conda.anaconda.org"; a bare host means https).
//
// Names expand to alias/name, except conda's built-in "pkgs/main",
// "pkgs/r", "pkgs/pro" and "pkgs/msys2", which live on
// https://repo.anaconda.com. A trailing platform component or a bracketed
// platform list becomes Platforms.
//
// To keep resolution minimal and fail closed, ParseChannelAlias rejects the
// "defaults" multichannel, local paths (use a file:// URL), package archive
// URLs, URLs with credentials, "/t/<token>" paths, queries or fragments,
// and names that are not plain path segments of [A-Za-z0-9._-].
func ParseChannelAlias(s, alias string) (Channel, error) {
	aliasURL, err := canonicalChannelURL(alias, true)
	if err != nil {
		return Channel{}, wrapErr(ErrChannel, s, "channel alias "+strconv.Quote(alias), err)
	}
	uc, err := parseUnresolvedChannel(s)
	if err != nil {
		return Channel{}, err
	}
	switch uc.kind {
	case chanUnknown:
		return Channel{}, parseErr(ErrChannel, s, "unknown channel")
	case chanPackageURL, chanPackagePath:
		return Channel{}, parseErr(ErrChannel, s, "a package archive is not a channel")
	case chanPath:
		if urlScheme(uc.location) != "file" {
			return Channel{}, parseErr(ErrChannel, s, "local paths are not supported, use a file:// URL")
		}
		fallthrough
	case chanURL:
		u, err := canonicalChannelURL(uc.location, false)
		if err != nil {
			return Channel{}, wrapErr(ErrChannel, s, "", err)
		}
		return Channel{URL: u, Name: channelDisplayName(u, aliasURL), Platforms: uc.platforms}, nil
	}
	// chanName
	name := uc.location
	if name == "defaults" {
		return Channel{}, parseErr(ErrChannel, s, `the "defaults" multichannel is not supported, list its channels explicitly`)
	}
	if msg := checkChannelName(name); msg != "" {
		return Channel{}, parseErr(ErrChannel, s, msg)
	}
	base := aliasURL
	if isAnacondaChannel(name) {
		base = anacondaRepo
	}
	return Channel{URL: base + "/" + name, Name: name, Platforms: uc.platforms}, nil
}

// checkChannelName returns a message if name is not a "/"-separated list of
// plain segments.
func checkChannelName(name string) string {
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "invalid channel name"
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !isAlnum(c) && c != '-' && c != '_' && c != '.' {
				return "invalid character " + strconv.Quote(seg[i:i+1]) + " in channel name"
			}
		}
	}
	return ""
}

// checkCredentials returns a message if a URL carries credentials, either
// as user information ("https://user:pw@host/...") or as a conda token path
// ("https://host/t/<token>/..."): goomba refuses them everywhere, so that
// secrets cannot end up in specs, lockfiles or logs.
func checkCredentials(location string) string {
	scheme := urlScheme(location)
	if scheme == "" {
		return ""
	}
	rest := location[len(scheme)+3:]
	host, p := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, p = rest[:i], rest[i:]
	}
	if strings.IndexByte(host, '@') >= 0 {
		return "credentials in channel URLs are not supported"
	}
	if !strings.EqualFold(scheme, "file") && (p == "/t" || strings.HasPrefix(p, "/t/")) {
		return "tokens in channel URLs are not supported"
	}
	return ""
}

// canonicalChannelURL lower-cases the scheme and host and drops trailing
// slashes. With bareHost, a string without scheme is taken as an https
// host and path.
func canonicalChannelURL(raw string, bareHost bool) (string, error) {
	raw = strip(raw)
	scheme := urlScheme(raw)
	rest := ""
	switch {
	case scheme != "":
		rest = raw[len(scheme)+3:]
	case bareHost && raw != "":
		scheme, rest = "https", raw
	default:
		return "", parseErr(ErrChannel, raw, "not a URL")
	}
	scheme = asciiLower(scheme)
	if strings.ContainsAny(raw, "?#") {
		return "", parseErr(ErrChannel, raw, "queries and fragments are not supported")
	}
	for i := 0; i < len(raw); i++ {
		if !isGraphic(raw[i]) {
			return "", parseErr(ErrChannel, raw, "invalid character "+strconv.Quote(raw[i:i+1]))
		}
	}
	host, p := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, p = rest[:i], rest[i:]
	}
	if strings.IndexByte(host, '@') >= 0 {
		return "", parseErr(ErrChannel, raw, "credentials in channel URLs are not supported")
	}
	if host == "" && scheme != "file" {
		return "", parseErr(ErrChannel, raw, "missing host")
	}
	p = strings.TrimRight(p, "/")
	if p == "/t" || strings.HasPrefix(p, "/t/") {
		return "", parseErr(ErrChannel, raw, "tokens in channel URLs are not supported")
	}
	return scheme + "://" + asciiLower(host) + p, nil
}

// channelDisplayName names a channel URL relative to the alias or to
// repo.anaconda.com's built-in channels, or returns the URL itself.
func channelDisplayName(u, aliasURL string) string {
	if rest, ok := strings.CutPrefix(u, anacondaRepo+"/"); ok && isAnacondaChannel(rest) {
		return rest
	}
	if rest, ok := strings.CutPrefix(u, aliasURL+"/"); ok && rest != "" {
		return rest
	}
	return u
}
