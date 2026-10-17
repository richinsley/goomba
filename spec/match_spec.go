// Ported from libmamba src/specs/match_spec.cpp and src/util/parsers.cpp
// (BSD-3-Clause, see NOTICE).

package spec

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/richinsley/goomba/record"
)

// MatchSpec is a conda match spec: a query on package records such as
// "numpy >=1.2", "python=3.11", "conda-forge::numpy 1.2.* py39*",
// "numpy[version='>=1.2',build=py*]" or a package URL.
//
// Every field's zero value means "any", so the zero MatchSpec matches every
// record. A MatchSpec is safe for concurrent use as long as no goroutine
// modifies it.
type MatchSpec struct {
	// Name is the package name pattern, lower-case; usually exact, "*"
	// allowed.
	Name GlobSpec
	// Version constrains the version; IsAny when absent.
	Version VersionSpec
	// Build constrains the build string.
	Build StringMatcher
	// BuildNumber constrains the build number.
	BuildNumber BuildNumberSpec

	// Channel is the channel as written: a name such as "conda-forge" or
	// "pkgs/main", or a URL without trailing slash or platform subdir.
	// "" means any channel. Resolve names with ChannelURL or
	// ParseChannelAlias.
	Channel string
	// Subdir restricts the platform subdir, e.g. "linux-64". Several
	// subdirs are joined by "," in sorted order; any of them matches.
	// "" means any subdir.
	Subdir string
	// Namespace is the rarely used namespace in "channel:namespace:name".
	Namespace string

	// MD5 and SHA256 pin the archive's hex digests.
	MD5, SHA256 string
	// Fn is the archive file name, e.g. "numpy-1.26.4-py311h64a7726_0.conda".
	Fn string
	// URL is the archive URL or path when the spec names a single package
	// file, as in "https://conda.anaconda.org/conda-forge/linux-64/x-1-0.conda#<md5>".
	URL string

	// License and LicenseFamily are the bracket keys license= and
	// license_family=.
	License, LicenseFamily string
	// Features is the legacy features= bracket key, kept verbatim.
	Features string
	// TrackFeatures lists required track_features, sorted and unique.
	TrackFeatures []string
	// Optional is the optional bracket flag.
	Optional bool
}

// Paren-aware search tokens: brackets, parentheses and both quote kinds.
const (
	openTokens  = `[("'`
	closeTokens = `])"'`
)

// updateDepths applies byte c to the depth counters, as libmamba's
// find_*_parentheses_impl does. Index 4 collects non-token bytes. Quotes,
// whose open and close bytes are the same, toggle between 0 and 1. It
// reports whether any depth went negative.
func updateDepths(depths *[5]int, open, close string, c byte) bool {
	oi, ci := strings.IndexByte(open, c), strings.IndexByte(close, c)
	if oi < 0 {
		oi = 4
	}
	if ci < 0 {
		ci = 4
	}
	if oi < 4 {
		depths[oi]++
	}
	if ci < 4 {
		depths[ci]--
	}
	if oi == ci {
		if depths[oi] > 0 {
			depths[oi] = 0
		} else {
			depths[oi] = 1
		}
	}
	depths[4] = 0
	neg := false
	for _, d := range depths {
		if d < 0 {
			neg = true
		}
	}
	return neg
}

func allZero(d *[5]int) bool { return *d == [5]int{} }

// findNotInParens returns the index of the first (or, with reverse, last)
// byte val of text that is not inside brackets, parentheses or quotes, or
// -1. It reports false on mismatched parentheses.
func findNotInParens(text string, val byte, reverse bool) (int, bool) {
	open, close := openTokens, closeTokens
	if reverse {
		open, close = close, open
	}
	var depths [5]int
	bad := false
	isToken := func(c byte) bool {
		return c == val || strings.IndexByte(openTokens, c) >= 0 || strings.IndexByte(closeTokens, c) >= 0
	}
	n := len(text)
	for k := 0; k < n; k++ {
		pos := k
		if reverse {
			pos = n - 1 - k
		}
		c := text[pos]
		if !isToken(c) {
			continue
		}
		if updateDepths(&depths, open, close, c) {
			bad = true
		}
		if c == val && allZero(&depths) {
			return pos, !bad
		}
	}
	if bad || !allZero(&depths) {
		return -1, false
	}
	return -1, true
}

// rfindMatchingParens finds the bracketed section ending with the last
// token of text, returning the indices of its opening and closing bytes.
func rfindMatchingParens(text string) (start, end int, ok bool) {
	var depths [5]int
	bad := false
	end = -1
	for pos := len(text) - 1; pos >= 0; pos-- {
		c := text[pos]
		if strings.IndexByte(openTokens, c) < 0 && strings.IndexByte(closeTokens, c) < 0 {
			continue
		}
		if end < 0 {
			end = pos
		}
		// Scanning backwards, closing bytes open a section.
		if updateDepths(&depths, closeTokens, openTokens, c) {
			bad = true
		}
		if allZero(&depths) {
			return pos, end, !bad
		}
	}
	return -1, -1, false
}

// stripWhitespaceQuotes trims non-printable bytes, spaces and quotes from
// both ends of an attribute value.
func stripWhitespaceQuotes(s string) string {
	trim := func(c byte) bool { return !isGraphic(c) || c == '"' || c == '\'' }
	for len(s) > 0 && trim(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && trim(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

func isTrueString(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case 'y', 'Y', 't', 'T', '1':
		return true
	}
	return false
}

// splitFeatures splits on spaces and commas into a sorted set.
func splitFeatures(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' }) {
		if f = strip(f); f != "" {
			out = append(out, f)
		}
	}
	return sortedUnique(out)
}

// msParser holds the state of one ParseMatchSpec call. libmamba keeps the
// channel's platform filters and the subdir= key apart; they are merged into
// MatchSpec.Subdir at the end.
type msParser struct {
	input        string
	ms           MatchSpec
	hasChan      bool
	ch           unresolvedChannel
	extraSubdirs []string
	extraFn      string
}

func (p *msParser) errorf(msg string, err error) error {
	return wrapErr(ErrMatchSpec, p.input, msg, err)
}

func (p *msParser) setChannel(uc unresolvedChannel) {
	p.hasChan, p.ch = true, uc
}

func (p *msParser) setAttribute(key, val string) error {
	switch key {
	case "build_number":
		bn, err := ParseBuildNumberSpec(val)
		if err != nil {
			return err
		}
		p.ms.BuildNumber = bn
	case "build", "build_string":
		b, err := ParseStringMatcher(val)
		if err != nil {
			return err
		}
		p.ms.Build = b
	case "version":
		v, err := ParseVersionSpec(val)
		if err != nil {
			return err
		}
		p.ms.Version = v
	case "channel", "url":
		uc, err := parseUnresolvedChannel(val)
		if err != nil {
			return err
		}
		p.setChannel(uc)
	case "subdir":
		if !p.hasChan || len(p.ch.platforms) == 0 {
			plats := parsePlatformList(val)
			if p.hasChan {
				p.ch.platforms = plats
				p.extraSubdirs = nil
			} else {
				p.extraSubdirs = plats
			}
		}
	case "fn", "filename":
		p.extraFn = val
	case "md5":
		p.ms.MD5 = val
	case "sha256":
		p.ms.SHA256 = val
	case "license":
		p.ms.License = val
	case "license_family":
		p.ms.LicenseFamily = val
	case "features":
		p.ms.Features = val
	case "track_features":
		p.ms.TrackFeatures = splitFeatures(val)
	case "optional":
		p.ms.Optional = isTrueString(val)
	case "name", "target":
		// Accepted and ignored, as in libmamba: the name before the
		// brackets wins, and target only matters to conda-build.
	default:
		// libmamba ignores unknown keys, so a typo such as
		// "foo[versoin='<1']" would match every version.
		return p.errorf("unknown key "+strconv.Quote(key), nil)
	}
	return nil
}

// setAttributes applies a comma-separated bracket section, e.g.
// `version='>=1.2', build=py*`.
func (p *msParser) setAttributes(attrs string) error {
	for {
		next, ok := findNotInParens(attrs, ',', false)
		if !ok {
			return p.errorf("parentheses mismatch in "+strconv.Quote(attrs), nil)
		}
		kv := attrs
		if next >= 0 {
			kv = attrs[:next]
		}
		if strings.HasPrefix(kv, "version") {
			opVal := lstripChars(kv, "version")
			for _, op := range [...]string{"==", "!=", "~=", ">", "<"} {
				if strings.HasPrefix(opVal, op) {
					return p.errorf(`implicit format "`+kv+`" is not allowed, use "version='`+opVal+`'" instead`, nil)
				}
			}
		}
		key, val, hasVal := strings.Cut(kv, "=")
		if !hasVal {
			val = "true"
		}
		key = asciiLower(strip(key))
		val = stripWhitespaceQuotes(val)
		if strings.ContainsAny(val, `"'`) {
			return p.errorf("quote inside the value of "+key, nil)
		}
		if err := p.setAttribute(key, val); err != nil {
			var pe *ParseError
			if errors.As(err, &pe) && pe.Kind == ErrMatchSpec {
				return err
			}
			return p.errorf("attribute "+key+"="+strconv.Quote(val), err)
		}
		if next < 0 {
			return nil
		}
		attrs = attrs[next+1:]
	}
}

// rparseAttributes applies every trailing bracket section, last first, as
// in "conda-forge::foo[build=3](target=blarg,optional)", and returns what
// precedes them.
func (p *msParser) rparseAttributes(str string) (string, error) {
	for strings.HasSuffix(str, "]") || strings.HasSuffix(str, ")") {
		start, end, ok := rfindMatchingParens(str)
		if !ok {
			return "", p.errorf("parentheses mismatch", nil)
		}
		if err := p.setAttributes(str[start+1 : end]); err != nil {
			return "", err
		}
		str = str[:start]
	}
	return str, nil
}

// splitVersionAndBuild splits "pkg>=1.3=bld" style remainders, as
// libmamba's split_version_and_build.
func splitVersionAndBuild(str string) (string, string) {
	str = strip(str)
	// Faulty specs such as `libblas=[build=*mkl]`, the repr of `libblas=*=*mkl`.
	str = rstripChars(str, "=")
	if pos := lastIndexAny(str, " ="); pos <= 0 {
		return str, ""
	}
	pos := strings.LastIndexByte(str, '=')
	if pos < 0 {
		// No operator: version and build are separated by spaces.
		pos = strings.LastIndexByte(str, ' ')
		return strip(str[:pos]), str[pos+1:]
	}
	// A '=' at the start is an operator too; libmamba reads str[-1] there.
	if pos == 0 || strings.IndexByte("=!|,<>~", str[pos-1]) >= 0 {
		// The last '=' belongs to an operator; a build may follow the
		// version after spaces.
		versionStart := indexNot(str, ' ', pos+1)
		if versionStart < 0 {
			return str, ""
		}
		spaceStart := indexFrom(str, ' ', versionStart)
		if spaceStart < 0 {
			return str, ""
		}
		buildStart := indexNot(str, ' ', spaceStart)
		if buildStart >= 0 && buildStart != versionStart {
			return strip(str[:buildStart]), str[buildStart:]
		}
		return str, ""
	}
	// The build follows the last '='.
	buildStart := indexNot(str, ' ', pos+1)
	if buildStart < 0 {
		return str[:pos], ""
	}
	return str[:pos], str[buildStart:]
}

// indexNot returns the index of the first byte other than c at or after
// from, or -1.
func indexNot(s string, c byte, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] != c {
			return i
		}
	}
	return -1
}

// indexFrom returns the index of the first c at or after from, or -1.
func indexFrom(s string, c byte, from int) int {
	if from >= len(s) {
		return -1
	}
	if i := strings.IndexByte(s[from:], c); i >= 0 {
		return from + i
	}
	return -1
}

func isPackageVersionSep(c byte) bool {
	switch c {
	case ' ', '=', '<', '>', '~', '!':
		return true
	}
	return false
}

// removeSpacesAfterOperators deletes the spaces following '>', '<', '=' or
// ',', which is what libmamba's repeated replacement of ">= ", "<= ", "> ",
// "< ", "!= ", "= ", "== ", "~= " and ", " amounts to, in linear time. It
// handles improperly encoded repodata such as "cudatoolkit >= 10.2" and
// "kytea >=0.1.4, 0.2.0".
func removeSpacesAfterOperators(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' && i > 0 && len(b) > 0 {
			switch b[len(b)-1] {
			case '>', '<', '=', ',':
				continue
			}
		}
		if b == nil {
			if c != ' ' || i == 0 {
				continue
			}
			switch s[i-1] {
			case '>', '<', '=', ',':
				b = append(make([]byte, 0, len(s)), s[:i]...)
				continue
			}
			continue
		}
		b = append(b, c)
	}
	if b == nil {
		return s
	}
	return string(b)
}

// ParseMatchSpec parses a conda match spec. Accepted forms include
//
//	numpy                       name only
//	numpy 1.2.* py39*           name, version, build
//	numpy>=1.2,<2               name and version spec
//	python=3.11                 starts with 3.11
//	python==3.11                exactly 3.11
//	python=3.11=*_cpython       name, version, build
//	conda-forge::numpy>=1.2     channel
//	conda-forge/linux-64::numpy channel and subdir
//	numpy[version='>=1.2',build=py*,build_number='>=2',md5=...]
//	https://.../linux-64/numpy-1.26.4-py311h64a7726_0.conda#<md5>
//
// Text after " #" is a comment. Only printable ASCII is accepted, specs are
// limited to 1024 bytes, package names to 256 bytes of [A-Za-z0-9._-] and
// '*' (lower-cased), and bracket keys to those listed on MatchSpec plus
// "name" and "target", which are ignored. The empty string parses to the zero
// MatchSpec, which matches everything.
//
// Unlike libmamba, a URL fragment must be an MD5 (32 hex digits) or a
// SHA256 (64 hex digits, optionally prefixed "sha256:"), and an fn= key
// must agree with a package URL.
func ParseMatchSpec(s string) (MatchSpec, error) {
	p := msParser{input: s}
	if err := p.parse(s); err != nil {
		return MatchSpec{}, err
	}
	// The canonical form must also fit, so that it parses back.
	if len(s) > maxSpecLen/4 && len(p.ms.String()) > maxSpecLen {
		p.input = truncate(s)
		return MatchSpec{}, p.errorf("normalized form longer than "+strconv.Itoa(maxSpecLen)+" bytes", nil)
	}
	return p.ms, nil
}

func (p *msParser) parse(s string) error {
	if len(s) > maxSpecLen {
		p.input = truncate(s)
		return p.errorf("longer than "+strconv.Itoa(maxSpecLen)+" bytes", nil)
	}
	str := s
	if i := strings.Index(str, " #"); i >= 0 {
		str = str[:i]
	}
	raw := strip(removeSpacesAfterOperators(strip(str)))
	if raw == "" {
		return nil
	}
	// Conda specs are printable ASCII; anything else is rejected rather
	// than trimmed differently in different places.
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x20 || c > 0x7e {
			return p.errorf("invalid character "+strconv.Quote(raw[i:i+1]), nil)
		}
	}

	// A plain package URL or path.
	if hasArchiveExtension(raw) {
		return p.parseURL(raw)
	}
	// A package URL with a hash, as written by `conda list --explicit --md5`.
	if i := strings.LastIndexByte(raw, '#'); i >= 0 {
		u, hash := raw[:i], raw[i+1:]
		if hasArchiveExtension(u) {
			if err := p.parseURL(u); err != nil {
				return err
			}
			return p.setURLHash(hash)
		}
	}

	// Split "channel:namespace:spec".
	specPos, ok := findNotInParens(raw, ':', true)
	if !ok {
		return p.errorf("parentheses mismatch", nil)
	}
	chanStr, specStr := "", raw
	if specPos >= 0 {
		nsPos, ok := findNotInParens(raw[:specPos], ':', true)
		if !ok {
			return p.errorf("parentheses mismatch", nil)
		}
		specStr = raw[specPos+1:]
		if nsPos < 0 {
			p.ms.Namespace = raw[:specPos]
		} else {
			chanStr = raw[:nsPos]
			p.ms.Namespace = raw[nsPos+1 : specPos]
		}
	}
	for i := 0; i < len(p.ms.Namespace); i++ {
		if c := p.ms.Namespace[i]; !isAlnum(c) && c != '-' && c != '_' && c != '.' {
			return p.errorf("invalid character "+strconv.Quote(p.ms.Namespace[i:i+1])+" in namespace", nil)
		}
	}
	if chanStr != "" {
		uc, err := parseUnresolvedChannel(chanStr)
		if err != nil {
			return p.errorf("", err)
		}
		p.setChannel(uc)
	}

	rest, err := p.rparseAttributes(specStr)
	if err != nil {
		return err
	}
	rest = lstrip(rest)
	i := 0
	for i < len(rest) && !isPackageVersionSep(rest[i]) {
		i++
	}
	nameStr := rest[:i]
	verStr, bldStr := splitVersionAndBuild(rest[i:])
	if nameStr == "" {
		return p.errorf("empty package name", nil)
	}
	if err := p.setName(nameStr); err != nil {
		return err
	}
	// Keep values set by bracket keys, as in "numpy[version=1.12]".
	if verStr != "" {
		v, err := ParseVersionSpec(verStr)
		if err != nil {
			return p.errorf("", err)
		}
		p.ms.Version = v
	}
	if bldStr != "" {
		b, err := ParseStringMatcher(bldStr)
		if err != nil {
			return p.errorf("", err)
		}
		p.ms.Build = b
	}
	return p.finish()
}

// finish moves the channel and subdir state into p.ms.
func (p *msParser) finish() error {
	if !p.hasChan {
		p.ms.Subdir = strings.Join(p.extraSubdirs, ",")
		p.ms.Fn = p.extraFn
		return nil
	}
	if p.ch.isPackage() {
		explicit := p.ch.platforms
		if len(explicit) == 0 {
			explicit = p.extraSubdirs
		}
		if err := p.setPackage(p.ch.location); err != nil {
			return err
		}
		if len(explicit) > 0 {
			p.ms.Subdir = strings.Join(explicit, ",")
		}
		if p.extraFn != "" && p.extraFn != p.ms.Fn {
			return p.errorf("fn "+strconv.Quote(p.extraFn)+" does not match url "+strconv.Quote(p.ms.URL), nil)
		}
		return nil
	}
	p.ms.Channel = p.ch.location
	plats := p.ch.platforms
	if len(plats) == 0 {
		plats = p.extraSubdirs
	}
	p.ms.Subdir = strings.Join(plats, ",")
	p.ms.Fn = p.extraFn
	return nil
}

// setPackage records a package URL or path and derives Fn, and Channel and
// Subdir when the parent directory is a known platform.
func (p *msParser) setPackage(location string) error {
	p.ms.URL = location
	dir, base := "", location
	if i := strings.LastIndexByte(location, '/'); i >= 0 {
		dir, base = location[:i], location[i+1:]
	}
	if dec, err := url.PathUnescape(base); err == nil {
		base = dec
	}
	// The file name must describe a package, so that the URL alone is a
	// valid spec.
	if _, _, _, msg, err := parsePackageFilename(base); msg != "" || err != nil {
		return p.errorf(msg, err)
	}
	p.ms.Fn = base
	p.ms.Channel, p.ms.Subdir = dir, ""
	if i := strings.LastIndexByte(dir, '/'); i >= 0 {
		if plat, ok := ParsePlatform(dir[i+1:]); ok {
			p.ms.Channel, p.ms.Subdir = dir[:i], plat
		}
	}
	return nil
}

// parsePackageFilename splits an archive file name such as
// "numpy-1.26.4-py311h64a7726_0.conda" into a name, a version spec and a
// build matcher. It returns a message or an error when it cannot.
func parsePackageFilename(fn string) (string, VersionSpec, StringMatcher, string, error) {
	stem := stripArchiveExtension(fn)
	i := strings.LastIndexByte(stem, '-')
	if i < 0 {
		return "", VersionSpec{}, StringMatcher{}, "missing name and version in file name " + strconv.Quote(fn), nil
	}
	if i == len(stem)-1 {
		return "", VersionSpec{}, StringMatcher{}, "empty build in file name " + strconv.Quote(fn), nil
	}
	b, err := ParseStringMatcher(stem[i+1:])
	if err != nil {
		return "", VersionSpec{}, StringMatcher{}, "", err
	}
	head := stem[:i]
	j := strings.LastIndexByte(head, '-')
	if j <= 0 || j == len(head)-1 {
		return "", VersionSpec{}, StringMatcher{}, "missing name or version in file name " + strconv.Quote(fn), nil
	}
	v, err := ParseVersionSpec(head[j+1:])
	if err != nil {
		return "", VersionSpec{}, StringMatcher{}, "", err
	}
	// Names may contain '-'.
	return head[:j], v, b, "", nil
}

// parseURL is libmamba's MatchSpec::parse_url: name, version and build come
// from the archive file name.
func (p *msParser) parseURL(spec string) error {
	uc, err := parseUnresolvedChannel(spec)
	if err != nil {
		return p.errorf("", err)
	}
	if !uc.isPackage() {
		return p.errorf("not a package archive", nil)
	}
	if err := p.setPackage(uc.location); err != nil {
		return err
	}
	if len(uc.platforms) > 0 {
		p.ms.Subdir = strings.Join(uc.platforms, ",")
	}
	name, v, b, msg, err := parsePackageFilename(p.ms.Fn)
	if msg != "" || err != nil {
		return p.errorf(msg, err)
	}
	p.ms.Version, p.ms.Build = v, b
	return p.setName(name)
}

// maxNameLen bounds package names; conda-forge's longest is far shorter.
const maxNameLen = 256

// setName lower-cases and checks a package name. Like rattler, and unlike
// libmamba, names are restricted to ASCII letters, digits, '-', '_' and
// '.', plus '*' for globs; this rejects for example the pip-style extras
// in "dask[array] >=2023", which libmamba takes as a name that never
// matches.
func (p *msParser) setName(name string) error {
	if len(name) > maxNameLen {
		return p.errorf("package name longer than "+strconv.Itoa(maxNameLen)+" bytes", nil)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isAlnum(c) && c != '-' && c != '_' && c != '.' && c != '*' {
			return p.errorf("invalid character "+strconv.Quote(name[i:i+1])+" in package name "+strconv.Quote(name), nil)
		}
	}
	lower := asciiLower(name)
	if hasArchiveExtension(lower) {
		// It would print as, and read back as, a package file.
		return p.errorf("package name "+strconv.Quote(name)+" ends with an archive extension", nil)
	}
	p.ms.Name = NewGlobSpec(lower)
	return nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) && !('a' <= c && c <= 'f') && !('A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// setURLHash records the hash in "url#hash".
func (p *msParser) setURLHash(hash string) error {
	h := strings.TrimPrefix(hash, "sha256:")
	switch {
	case hash == "":
	case len(h) == 32 && isHex(h) && h == hash:
		p.ms.MD5 = h
	case len(h) == 64 && isHex(h):
		p.ms.SHA256 = h
	default:
		return p.errorf("invalid hash "+strconv.Quote(hash)+" after '#'", nil)
	}
	return nil
}

// ExtractName returns the lower-cased package name at the start of a match
// spec, without parsing the rest: the text before the first space or one
// of "<>=!~". It is much cheaper than ParseMatchSpec.
func ExtractName(s string) (string, error) {
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	start := i
	for i < len(s) && !isSpace(s[i]) && strings.IndexByte("<>=!~", s[i]) < 0 {
		i++
	}
	if i == start {
		return "", parseErr(ErrMatchSpec, s, "empty package name")
	}
	return asciiLower(s[start:i]), nil
}

// MatchesVersion reports whether record r, whose version was already
// parsed as v, satisfies every field of m except Channel and Namespace (as
// in libmamba and rattler, channel filtering is left to the caller, which
// knows the channel alias; see ChannelURL). It checks the name, version,
// build string, build number, MD5, SHA256 (hex, case-insensitive), license,
// file name, subdir and track features. LicenseFamily, Features and
// Optional are not checked. MatchesVersion does not allocate.
func (m MatchSpec) MatchesVersion(r *record.PackageRecord, v Version) bool {
	if !m.Name.Contains(r.Name) ||
		!m.Version.Contains(v) ||
		!m.Build.Contains(r.Build) ||
		!m.BuildNumber.Contains(r.BuildNumber) {
		return false
	}
	if (m.MD5 != "" && !asciiEqualFold(m.MD5, r.MD5)) ||
		(m.SHA256 != "" && !asciiEqualFold(m.SHA256, r.SHA256)) ||
		(m.License != "" && m.License != r.License) ||
		(m.Fn != "" && m.Fn != r.Fn) {
		return false
	}
	if m.Subdir != "" && !commaListContains(m.Subdir, r.Subdir) {
		return false
	}
	for _, f := range m.TrackFeatures {
		if !containsString(r.TrackFeatures, f) {
			return false
		}
	}
	return true
}

// Matches is MatchesVersion parsing r.Version itself; an empty version
// counts as "0" and an unparsable one never matches.
func (m MatchSpec) Matches(r *record.PackageRecord) bool {
	vs := r.Version
	if vs == "" {
		vs = "0"
	}
	v, err := ParseVersion(vs)
	if err != nil {
		return false
	}
	return m.MatchesVersion(r, v)
}

// ChannelURL resolves m.Channel to its canonical base URL using the given
// channel alias (see ParseChannelAlias). It returns "" when m has no
// channel.
func (m MatchSpec) ChannelURL(alias string) (string, error) {
	if m.Channel == "" {
		return "", nil
	}
	c, err := ParseChannelAlias(m.Channel, alias)
	if err != nil {
		return "", err
	}
	return c.URL, nil
}

// Subdirs returns the subdirs in m.Subdir, or nil.
func (m MatchSpec) Subdirs() []string {
	if m.Subdir == "" {
		return nil
	}
	return strings.Split(m.Subdir, ",")
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if isUpper(x) {
			x += 'a' - 'A'
		}
		if isUpper(y) {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func commaListContains(list, s string) bool {
	for list != "" {
		item := list
		if i := strings.IndexByte(list, ','); i >= 0 {
			item, list = list[:i], list[i+1:]
		} else {
			list = ""
		}
		if item == s {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// IsSimple reports whether m can be written as "<name> <version> <build>"
// with a version libsolv understands: at most one predicate or two classic
// operators (as in ">=3.6,<4.0"), no complex glob, a glob build string and
// no other field.
func (m MatchSpec) IsSimple() bool {
	n := m.Version.expressionSize()
	simpleVersion := ((n == 3 && m.Version.isClassicOperatorExpression()) || n <= 1) && !m.Version.hasGlob()
	return simpleVersion &&
		m.BuildNumber.IsAny() &&
		m.Build.IsGlob() &&
		m.Channel == "" && m.URL == "" &&
		m.Fn == "" &&
		m.Subdir == "" &&
		m.Namespace == "" &&
		m.MD5 == "" && m.SHA256 == "" &&
		m.License == "" && m.LicenseFamily == "" &&
		m.Features == "" &&
		len(m.TrackFeatures) == 0 &&
		!m.Optional
}

// IsOnlyPackageName reports whether m is an exact package name and nothing
// else, like "numpy".
func (m MatchSpec) IsOnlyPackageName() bool {
	return m.Name.IsExact() && m.Version.IsAny() && m.Build.IsAny() && m.IsSimple()
}

// NameOnly returns a MatchSpec with only m's name.
func (m MatchSpec) NameOnly() MatchSpec { return MatchSpec{Name: m.Name} }

// Equal reports whether every field of m and o is equal (version specs
// compared with VersionSpec.Equal).
func (m MatchSpec) Equal(o MatchSpec) bool {
	if len(m.TrackFeatures) != len(o.TrackFeatures) {
		return false
	}
	for i := range m.TrackFeatures {
		if m.TrackFeatures[i] != o.TrackFeatures[i] {
			return false
		}
	}
	return m.Name == o.Name &&
		m.Version.Equal(o.Version) &&
		m.Build.Equal(o.Build) &&
		m.BuildNumber == o.BuildNumber &&
		m.Channel == o.Channel && m.Subdir == o.Subdir && m.Namespace == o.Namespace &&
		m.MD5 == o.MD5 && m.SHA256 == o.SHA256 &&
		m.Fn == o.Fn && m.URL == o.URL &&
		m.License == o.License && m.LicenseFamily == o.LicenseFamily &&
		m.Features == o.Features && m.Optional == o.Optional
}

// CondaBuildForm returns "name version build" in the form conda-build
// understands, e.g. "numpy 1.7.*" or "libblas * *mkl".
func (m MatchSpec) CondaBuildForm() string {
	hasVersion := !m.Version.IsAny()
	hasBuild := !m.Build.IsAny()
	switch {
	case hasVersion && hasBuild:
		return m.Name.String() + " " + m.Version.CondaBuildString() + " " + m.Build.String()
	case hasVersion:
		return m.Name.String() + " " + m.Version.CondaBuildString()
	case hasBuild:
		return m.Name.String() + " * " + m.Build.String()
	}
	return m.Name.String()
}

// neededQuote returns the quote needed to write v as a bracket value: none
// for plain values, '"' when v contains a space, '=', or a bracket,
// parenthesis or comma, and '\'' when v also contains '"'.
func neededQuote(v string) string {
	if strings.ContainsAny(v, " =\"',[]()") {
		if strings.IndexByte(v, '"') >= 0 {
			return "'"
		}
		return `"`
	}
	return ""
}

// quoteValue writes v quoted with '"' unless it contains one.
func quoteValue(v string) string {
	if strings.IndexByte(v, '"') >= 0 {
		return "'" + v + "'"
	}
	return `"` + v + `"`
}

// urlOnly reports whether m is a package URL spec carrying nothing beyond
// what the URL and an MD5 imply, so that String can print just the URL:
// its name, version and build are either unconstrained or those of the
// file name.
func (m MatchSpec) urlOnly() bool {
	if m.URL == "" || m.SHA256 != "" || !m.BuildNumber.IsAny() ||
		m.License != "" || m.LicenseFamily != "" || m.Features != "" ||
		len(m.TrackFeatures) != 0 || m.Optional || m.Namespace != "" {
		return false
	}
	name, v, b, msg, err := parsePackageFilename(m.Fn)
	if msg != "" || err != nil {
		return false
	}
	return (m.Name.IsAny() || m.Name == NewGlobSpec(asciiLower(name))) &&
		(m.Version.IsAny() || m.Version.Equal(v)) &&
		(m.Build.IsAny() || m.Build.Equal(b))
}

// String returns the canonical form of m; parsing it yields an equal
// MatchSpec. Simple specs print as "name", "name==1.2", "name=1.2=bld" or
// "conda-forge::name>=1.2"; other fields go into a bracket section such as
// `name[version=">=1.2,<2",build="py*",md5=...]`. Package URL specs print
// as "url" or "url#md5", as in libmamba; parsing that form also pins the
// name, version and build given by the file name, so
// "xtensor[url=.../xtensor-0.21.4-hc9558a2_0.tar.bz2]" reparses to a
// stricter spec that matches the same file.
func (m MatchSpec) String() string {
	s := m.format(false)
	if hasArchiveExtension(s) {
		// "name==1.0.conda" would read back as a package file.
		s = m.format(true)
	}
	return s
}

// format writes m, putting the version and build in brackets when they are
// complex or when inBrackets is set.
func (m MatchSpec) format(inBrackets bool) string {
	var b strings.Builder
	if m.urlOnly() {
		b.WriteString(m.URL)
		if m.MD5 != "" {
			b.WriteByte('#')
			b.WriteString(m.MD5)
		}
		return b.String()
	}
	if m.URL == "" {
		switch {
		case m.Channel != "":
			b.WriteString(m.Channel)
			if m.Subdir != "" {
				b.WriteString("[" + m.Subdir + "]")
			}
			b.WriteString(":" + m.Namespace + ":")
		case m.Namespace != "":
			b.WriteString(m.Namespace + ":")
		}
	}
	b.WriteString(m.Name.String())

	verStr := m.Version.String()
	// A version must be written after the name only if it starts with an
	// operator; globs like "1.*.3" print bare and go in brackets.
	complexVersion := m.Version.expressionSize() > 1 ||
		(!m.Version.IsAny() && strings.IndexByte("=<>!~", verStr[0]) < 0)
	complexBuild := !(m.Build.IsExact() || m.Build.IsAny())
	if inBrackets {
		complexVersion = true
	}
	if !complexVersion && !complexBuild {
		if !m.Build.IsAny() {
			b.WriteString(verStr + "=" + m.Build.String())
		} else if !m.Version.IsAny() {
			b.WriteString(verStr)
		}
	}

	open := false
	attr := func(s string) {
		if open {
			b.WriteByte(',')
		} else {
			b.WriteByte('[')
			open = true
		}
		b.WriteString(s)
	}
	if complexVersion || complexBuild {
		if !m.Version.IsAny() {
			attr("version=" + quoteValue(verStr))
		}
		if !m.Build.IsAny() {
			attr("build=" + quoteValue(m.Build.String()))
		}
	}
	if !m.BuildNumber.IsAny() {
		attr("build_number=" + quoteValue(m.BuildNumber.String()))
	}
	if m.URL != "" {
		attr("url=" + quoteValue(m.URL))
	} else if m.Channel == "" && m.Subdir != "" {
		attr("subdir=" + quoteValue(m.Subdir))
	}
	if len(m.TrackFeatures) > 0 {
		attr("track_features=" + quoteValue(strings.Join(m.TrackFeatures, " ")))
	}
	if m.Features != "" {
		q := neededQuote(m.Features)
		attr("features=" + q + m.Features + q)
	}
	if m.Fn != "" && m.URL == "" {
		q := neededQuote(m.Fn)
		attr("fn=" + q + m.Fn + q)
	}
	if m.MD5 != "" {
		q := neededQuote(m.MD5)
		attr("md5=" + q + m.MD5 + q)
	}
	if m.SHA256 != "" {
		q := neededQuote(m.SHA256)
		attr("sha256=" + q + m.SHA256 + q)
	}
	if m.License != "" {
		q := neededQuote(m.License)
		attr("license=" + q + m.License + q)
	}
	if m.LicenseFamily != "" {
		q := neededQuote(m.LicenseFamily)
		attr("license_family=" + q + m.LicenseFamily + q)
	}
	if m.Optional {
		attr("optional")
	}
	if open {
		b.WriteByte(']')
	}
	return b.String()
}
