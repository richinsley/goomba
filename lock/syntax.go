// The archive naming rules follow libmamba src/specs/package_info.cpp
// (BSD-3-Clause, see NOTICE); the platform list is rattler's Subdir, kept
// once in package virtual.

package lock

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/richinsley/goomba/virtual"
)

// Limits on field sizes. They are far above anything conda produces and
// exist so hostile input cannot make later stages do unbounded work.
const (
	maxNameLen   = 256
	maxVerLen    = 128
	maxURLLen    = 4096
	maxSpecLen   = 4096
	maxPathLen   = 1024
	maxListItems = 100000
)

// knownPlatform reports whether p is a conda platform subdir other than
// "noarch", using package virtual's single copy of rattler's Subdir list.
func knownPlatform(p string) bool { return virtual.KnownPlatform(p) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlnum(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) && !(c >= 'a' && c <= 'f') && !(c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// checkPackageName accepts a conda package name: ASCII letters, digits, '_',
// '.' and '-', starting with a letter, digit or '_', and not starting with
// "__" (that prefix is reserved for virtual packages).
func checkPackageName(n string) error {
	if n == "" {
		return errors.New("empty name")
	}
	if len(n) > maxNameLen {
		return errors.New("name too long")
	}
	if strings.HasPrefix(n, "__") {
		return errors.New("names starting with \"__\" are virtual packages")
	}
	if n[0] == '.' || n[0] == '-' {
		return errors.New("name must start with a letter, digit or '_'")
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !isAlnum(c) && c != '_' && c != '.' && c != '-' {
			return fmt.Errorf("invalid character %q", c)
		}
	}
	return nil
}

// checkVersion accepts the character set and shape of conda versions:
// ASCII letters, digits and the separators '.', '_', '+' and '!', with no
// empty segment, at most one '!' (after a numeric epoch) and at most one
// '+'. Package spec owns real version parsing; this rejects anything that
// cannot be one.
func checkVersion(v string) error {
	if v == "" {
		return errors.New("empty version")
	}
	if len(v) > maxVerLen {
		return errors.New("version too long")
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !isAlnum(c) && c != '.' && c != '_' && c != '+' && c != '!' {
			return fmt.Errorf("invalid character %q", c)
		}
	}
	if strings.Count(v, "!") > 1 {
		return errors.New("more than one '!'")
	}
	if strings.Count(v, "+") > 1 {
		return errors.New("more than one '+'")
	}
	if epoch, _, ok := strings.Cut(v, "!"); ok {
		for i := 0; i < len(epoch); i++ {
			if !isDigit(epoch[i]) {
				return errors.New("epoch is not a number")
			}
		}
	}
	prevSep := true
	for i := 0; i < len(v); i++ {
		sep := v[i] == '.' || v[i] == '_' || v[i] == '+' || v[i] == '!'
		if sep && prevSep {
			return errors.New("empty segment")
		}
		prevSep = sep
	}
	if prevSep {
		return errors.New("empty segment")
	}
	return nil
}

// checkBuild accepts a conda build string: ASCII letters, digits, '_', '.'
// and '+'. A dash would make the file name name-version-build ambiguous.
func checkBuild(b string) error {
	if b == "" {
		return errors.New("empty build string")
	}
	if len(b) > maxVerLen {
		return errors.New("build string too long")
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !isAlnum(c) && c != '_' && c != '.' && c != '+' {
			return fmt.Errorf("invalid character %q", c)
		}
	}
	return nil
}

// checkText accepts a free-form one-line string (a match spec): valid UTF-8,
// not empty or all blank, no control characters, at most max bytes.
func checkText(s string, max int) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("empty")
	}
	if len(s) > max {
		return errors.New("too long")
	}
	if !utf8.ValidString(s) {
		return errors.New("invalid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\u2028' || r == '\u2029' || r == utf8.RuneError {
			return fmt.Errorf("control character %U", r)
		}
	}
	return nil
}

// parseHTTPS parses an absolute https URL in canonical form: a lower-case
// host without a trailing dot or an explicit :443, a clean path, and no
// credentials, query, fragment, percent-encoding, backslash, space, control
// or non-ASCII character. Conda channel and package URLs never need any of
// those; refusing them means the string in a lockfile is exactly the URL
// that is fetched, and one server cannot appear as several channels.
func parseHTTPS(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("empty URL")
	}
	if len(raw) > maxURLLen {
		return nil, errors.New("URL too long")
	}
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c <= 0x20 || c >= 0x7f:
			return nil, fmt.Errorf("URL contains %q", c)
		case c == '%' || c == '?' || c == '#' || c == '\\':
			return nil, fmt.Errorf("URL contains %q", c)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case u.Scheme != "https":
		return nil, fmt.Errorf("scheme %q is not https", u.Scheme)
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return nil, errors.New("URL has no host")
	case u.User != nil:
		return nil, errors.New("URL contains credentials")
	case strings.HasSuffix(u.Host, ":"):
		return nil, errors.New("URL has an empty port")
	case u.Port() == "443":
		return nil, errors.New("URL names the default port 443")
	case strings.HasSuffix(u.Hostname(), "."):
		return nil, errors.New("URL host ends with '.'")
	case strings.ToLower(u.Host) != u.Host:
		return nil, errors.New("URL host is not lower-case")
	}
	for _, c := range []byte(u.Port()) {
		if !isDigit(c) {
			return nil, fmt.Errorf("URL port %q is not a number", u.Port())
		}
	}
	if u.Path != "" && path.Clean(u.Path) != u.Path {
		return nil, fmt.Errorf("URL path %q is not clean", u.Path)
	}
	if u.String() != raw || "https://"+u.Host+u.Path != raw {
		return nil, errors.New("URL is not in canonical form")
	}
	return u, nil
}

// canonicalHost puts the authority of an absolute URL in canonical form:
// scheme and host lower-cased, a trailing dot dropped from the host name and
// an explicit default port (:443 for https, :80 for http) removed. The rest
// of the URL is left alone; strings that do not look like a URL come back
// unchanged for validation to reject.
func canonicalHost(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	scheme = strings.ToLower(scheme)
	end := strings.IndexByte(rest, '/')
	if end < 0 {
		end = len(rest)
	}
	host, port := strings.ToLower(rest[:end]), ""
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host, port = host[:i], host[i+1:]
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if !strings.HasPrefix(host, "[") {
		host = strings.TrimRight(host, ".")
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host + rest[end:]
}

// checkChannel validates a channel base URL: canonical https with no
// trailing slash.
func checkChannel(raw string) error {
	if _, err := parseHTTPS(raw); err != nil {
		return err
	}
	if strings.HasSuffix(raw, "/") {
		return errors.New("channel URL ends with '/'")
	}
	return nil
}

// splitArchiveURL splits a package URL <channel>/<subdir>/<fn> into its
// parts after checking it with parseHTTPS.
func splitArchiveURL(raw string) (channel, subdir, fn string, err error) {
	u, err := parseHTTPS(raw)
	if err != nil {
		return "", "", "", err
	}
	dir, fn := path.Split(u.Path)
	parent, subdir := path.Split(strings.TrimSuffix(dir, "/"))
	if fn == "" || subdir == "" || parent == "" {
		return "", "", "", errors.New("URL is not <channel>/<subdir>/<file>")
	}
	return "https://" + u.Host + strings.TrimSuffix(parent, "/"), subdir, fn, nil
}

// archiveExt returns the archive extension of fn (".tar.bz2" or ".conda"),
// or "".
func archiveExt(fn string) string {
	for _, ext := range []string{".tar.bz2", ".conda"} {
		if strings.HasSuffix(fn, ext) {
			return ext
		}
	}
	return ""
}

// checkSitePackagesPath accepts "" or a clean relative slash-separated path
// with no "..", drive letter or backslash.
func checkSitePackagesPath(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > maxPathLen {
		return errors.New("path too long")
	}
	if err := checkText(p, maxPathLen); err != nil {
		return err
	}
	if strings.ContainsAny(p, "\\:") || strings.HasPrefix(p, "/") {
		return errors.New("path must be relative and use '/'")
	}
	if path.Clean(p) != p {
		return errors.New("path is not clean")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return errors.New("path must not contain '.' or '..'")
		}
	}
	return nil
}
