// Ported from libmamba src/specs/version.cpp (BSD-3-Clause, see NOTICE).

package spec

import (
	"strconv"
	"strings"
)

// Literal kinds give certain literals a special rank inside a version part:
//
//	"*" < "dev" < "_" < any other literal < "" < "post"
//
// Two "other" literals compare as strings.
const (
	kindStar  int8 = -3
	kindDev   int8 = -2
	kindUnder int8 = -1
	kindOther int8 = 0
	kindEmpty int8 = 1
	kindPost  int8 = 2
)

func literalKind(lit string) int8 {
	switch lit {
	case "*":
		return kindStar
	case "dev":
		return kindDev
	case "_":
		return kindUnder
	case "":
		return kindEmpty
	case "post":
		return kindPost
	}
	return kindOther
}

// Atom flags.
const (
	// flagPartStart marks the first atom of a version part.
	flagPartStart uint8 = 1 << iota
	// flagImplicitZero marks a part written without its leading zero, as in
	// "1.0.dev1" whose third part is parsed like "0dev1". It only affects
	// formatting.
	flagImplicitZero
)

// atom is libmamba's VersionPartAtom: a number followed by a lower-case
// literal, e.g. {1, "dev"} in "1dev".
type atom struct {
	lit   string
	num   uint64
	kind  int8
	flags uint8
}

// Version is a parsed conda version: an epoch, a version made of parts and an
// optional local version, as in "1!2.0.3a1+build.4".
//
// The zero Version is "0". Versions are compared with Compare, never with
// ==; "1.0" and "1.0.0" are equal. A Version is immutable and safe for
// concurrent use.
type Version struct {
	// atoms holds the atoms of every part: atoms[:nver] are the version,
	// atoms[nver:] the local version. Each part starts with an atom
	// flagged flagPartStart.
	atoms []atom
	epoch uint64
	nver  int
}

func (v Version) ver() []atom   { return v.atoms[:v.nver] }
func (v Version) local() []atom { return v.atoms[v.nver:] }

// Epoch returns the version's epoch, 0 when none was written.
func (v Version) Epoch() uint64 { return v.epoch }

// HasLocal reports whether the version has a local part ("+...").
func (v Version) HasLocal() bool { return v.nver < len(v.atoms) }

// Limits on versions, far above real ones (the longest version in
// conda-forge has 55 bytes): they bound the cost of comparisons and globs on
// hostile input.
const (
	maxVersionLen   = 256 // bytes
	maxVersionParts = 64  // parts in the version and local version together
)

// ParseVersion parses a conda version such as "1.2.3", "1!2.0", "1.0.post1",
// "3.7b_" or "1.2+local.3". Letters are case-insensitive; '.', '-' and '_'
// separate parts ('-' and '_' cannot be mixed, except that a trailing '_' is
// part of the version). Numbers larger than 2^64-1, versions longer than 256
// bytes and versions of more than 64 parts are rejected.
func ParseVersion(s string) (Version, error) {
	str := strip(s)
	if len(str) > maxVersionLen {
		return Version{}, parseErr(ErrVersion, truncate(s), "longer than "+strconv.Itoa(maxVersionLen)+" bytes")
	}
	low := asciiLower(str)

	var epoch uint64
	rest := low
	if i := strings.IndexByte(low, '!'); i >= 0 {
		if i == 0 {
			return Version{}, parseErr(ErrVersion, s, "empty epoch")
		}
		e, ok := parseUint(low[:i])
		if !ok {
			return Version{}, parseErr(ErrVersion, s, "epoch "+strconv.Quote(str[:i])+" is not a number")
		}
		epoch = e
		rest = low[i+1:]
	}

	verStr, locStr := rest, ""
	if i := strings.LastIndexByte(rest, '+'); i >= 0 {
		if i+1 == len(rest) {
			return Version{}, parseErr(ErrVersion, s, "empty local version")
		}
		verStr, locStr = rest[:i], rest[i+1:]
	}
	if verStr == "" {
		return Version{}, parseErr(ErrVersion, s, "empty version")
	}

	atoms := make([]atom, 0, estimateAtoms(verStr)+estimateAtoms(locStr))
	atoms, msg := appendCommonVersion(atoms, verStr)
	if msg != "" {
		return Version{}, parseErr(ErrVersion, s, msg)
	}
	nver := len(atoms)
	if locStr != "" {
		atoms, msg = appendCommonVersion(atoms, locStr)
		if msg != "" {
			return Version{}, parseErr(ErrVersion, s, "local version: "+msg)
		}
	}
	if countParts(atoms) > maxVersionParts {
		return Version{}, parseErr(ErrVersion, s, "more than "+strconv.Itoa(maxVersionParts)+" parts")
	}
	return Version{atoms: atoms, epoch: epoch, nver: nver}, nil
}

// truncate shortens an over-long input for error messages.
func truncate(s string) string {
	const n = 64
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// parseUint parses a non-empty string of ASCII digits, reporting false on
// any other character or on overflow.
func parseUint(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) {
			return 0, false
		}
		d := uint64(c - '0')
		if n > (^uint64(0)-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

// estimateAtoms returns a capacity hint for the atoms of s.
func estimateAtoms(s string) int {
	n := 1
	prevDigit, partStart := false, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.' || c == '-' || c == '_':
			partStart, prevDigit = true, false
			continue
		case isDigit(c):
			if !prevDigit {
				n++
			}
			prevDigit = true
		default:
			if partStart {
				n++
			}
			prevDigit = false
		}
		partStart = false
	}
	return n
}

func isVersionDelim(c byte) bool { return c == '.' || c == '-' || c == '_' }

// appendCommonVersion parses the parts of a version or local version and
// appends their atoms to dst. It returns a non-empty message on error.
func appendCommonVersion(dst []atom, str string) ([]atom, string) {
	// '-' and '_' cannot be used together, except for a trailing '_'.
	if strings.IndexByte(str, '-') >= 0 {
		if u := strings.IndexByte(str, '_'); u >= 0 && u < len(str)-1 {
			return dst, "cannot use both '-' and '_' as delimiters"
		}
	}
	for i := 0; i < len(str); i++ {
		c := str[i]
		if !isAlnum(c) && !isVersionDelim(c) && c != '*' {
			return dst, "invalid character " + strconv.Quote(str[i:i+1])
		}
	}

	tail := str
	for {
		if tail == "" {
			return dst, "empty part"
		}
		pos := -1
		for i := 0; i < len(tail); i++ {
			if isVersionDelim(tail[i]) {
				pos = i
				break
			}
		}
		// '_' is both a delimiter and, at the very end, part of the version.
		if pos == len(tail)-1 && tail[pos] == '_' {
			pos = -1
		}
		if pos == 0 || pos == len(tail)-1 {
			return dst, "empty part"
		}
		part := tail
		if pos >= 0 {
			part = tail[:pos]
		}
		var msg string
		if dst, msg = appendPart(dst, part); msg != "" {
			return dst, msg
		}
		if pos < 0 {
			return dst, ""
		}
		tail = tail[pos+1:]
	}
}

// appendPart splits a non-empty part into atoms, e.g. "0post1dev" into
// {0, "post"} {1, "dev"}.
func appendPart(dst []atom, part string) ([]atom, string) {
	flags := flagPartStart
	if !isDigit(part[0]) {
		flags |= flagImplicitZero
	}
	for len(part) > 0 {
		var num uint64
		if isDigit(part[0]) {
			j := 1
			for j < len(part) && isDigit(part[j]) {
				j++
			}
			n, ok := parseUint(part[:j])
			if !ok {
				return dst, "number " + part[:j] + " is too large"
			}
			num, part = n, part[j:]
		}
		j := 0
		for j < len(part) && !isDigit(part[j]) {
			j++
		}
		lit := part[:j]
		part = part[j:]
		dst = append(dst, atom{lit: lit, num: num, kind: literalKind(lit), flags: flags})
		flags = 0
	}
	return dst, ""
}

// nextPart returns the index of the atom starting the part after the one
// starting at i.
func nextPart(sec []atom, i int) int {
	j := i + 1
	for j < len(sec) && sec[j].flags&flagPartStart == 0 {
		j++
	}
	return j
}

// countParts returns the number of parts in a section.
func countParts(sec []atom) int {
	n := 0
	for i := range sec {
		if sec[i].flags&flagPartStart != 0 {
			n++
		}
	}
	return n
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpAtom compares numbers first, then literals by kind, then other
// literals as strings.
func cmpAtom(a, b *atom) int {
	if a.num != b.num {
		return cmpUint(a.num, b.num)
	}
	if a.kind == kindOther && b.kind == kindOther {
		return strings.Compare(a.lit, b.lit)
	}
	switch {
	case a.kind < b.kind:
		return -1
	case a.kind > b.kind:
		return 1
	}
	return 0
}

// cmpParts compares two parts lexicographically, padding the shorter one
// with empty atoms {0, ""}.
func cmpParts(a, b []atom) int {
	empty := atom{kind: kindEmpty}
	for k := 0; k < len(a) || k < len(b); k++ {
		x, y := &empty, &empty
		if k < len(a) {
			x = &a[k]
		}
		if k < len(b) {
			y = &b[k]
		}
		if c := cmpAtom(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// cmpSection compares two sections lexicographically, padding the shorter
// one with empty parts. It also returns the index of the first part that
// differs.
func cmpSection(a, b []atom) (cmp, idx int) {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ni, nj := nextPart(a, i), nextPart(b, j)
		if c := cmpParts(a[i:ni], b[j:nj]); c != 0 {
			return c, idx
		}
		i, j = ni, nj
		idx++
	}
	for i < len(a) {
		ni := nextPart(a, i)
		if c := cmpParts(a[i:ni], nil); c != 0 {
			return c, idx
		}
		i = ni
		idx++
	}
	for j < len(b) {
		nj := nextPart(b, j)
		if c := cmpParts(nil, b[j:nj]); c != 0 {
			return c, idx
		}
		j = nj
		idx++
	}
	return 0, idx
}

// Compare returns -1, 0 or +1 as v sorts before, equal to or after o in
// conda's version order: epoch first, then the version parts, then the local
// version. Missing trailing parts and atoms count as zero, so "1.1" equals
// "1.1.0". Within a part, numbers compare numerically and literals rank as
// "*" < "dev" < "_" < other literals (alphabetical) < no literal < "post".
// Compare does not allocate.
func (v Version) Compare(o Version) int {
	if v.epoch != o.epoch {
		return cmpUint(v.epoch, o.epoch)
	}
	if c, _ := cmpSection(v.ver(), o.ver()); c != 0 {
		return c
	}
	c, _ := cmpSection(v.local(), o.local())
	return c
}

// Equal reports whether v.Compare(o) == 0.
func (v Version) Equal(o Version) bool { return v.Compare(o) == 0 }

// swAtom reports whether atom a "starts with" atom b: b has no literal and
// the same number, or the atoms are equal.
func swAtom(a, b *atom) bool {
	if a.num == b.num && b.kind == kindEmpty {
		return true
	}
	return cmpAtom(a, b) == 0
}

// swPart reports whether part a starts with part b: b's atoms equal a's,
// except that b's last atom may lack a's literal.
func swPart(a, b []atom) bool {
	k := 0
	for ; k < len(a) && k < len(b); k++ {
		if !swAtom(&a[k], &b[k]) {
			return false
		}
	}
	// Extra atoms in a are accepted; extra atoms in b must be empty-like.
	empty := atom{kind: kindEmpty}
	for ; k < len(b); k++ {
		if !swAtom(&empty, &b[k]) {
			return false
		}
	}
	return true
}

// swSection reports whether section a starts with section b, as conda and
// libsolv define it: every part of b but the last must equal a's part
// (missing parts count as zero), and b's last part must be a prefix of a's
// (swPart). libmamba applies the prefix rule to every part, so for it
// "1.0a1" starts with "1.0.0".
func swSection(a, b []atom) bool {
	i, j := 0, 0
	for j < len(b) {
		nj := nextPart(b, j)
		var ap []atom
		ni := i
		if i < len(a) {
			ni = nextPart(a, i)
			ap = a[i:ni]
		}
		if nj == len(b) {
			return swPart(ap, b[j:nj])
		}
		if cmpParts(ap, b[j:nj]) != 0 {
			return false
		}
		i, j = ni, nj
	}
	return true
}

// StartsWith reports whether v starts with prefix, as used by "=1.2" and
// "1.2.*": "1.2.3", "1.2a" and "1.2.dev1" start with "1.2", but "1.20" does
// not, and "1.0a1" does not start with "1.0.0". All parts of prefix but the
// last must equal v's; the last must be a prefix of v's part, atom by atom,
// with an exact final literal (so, as in libsolv but not conda, "1.0alpha"
// does not start with "1.0a"). When prefix has a local version, v's version
// must equal prefix's and v's local version must start with prefix's.
func (v Version) StartsWith(prefix Version) bool {
	if v.epoch != prefix.epoch {
		return false
	}
	if !prefix.HasLocal() {
		return swSection(v.ver(), prefix.ver())
	}
	if c, _ := cmpSection(v.ver(), prefix.ver()); c != 0 {
		return false
	}
	return swSection(v.local(), prefix.local())
}

func compatSection(newer, older []atom, level int) bool {
	c, idx := cmpSection(newer, older)
	return c == 0 || (c > 0 && idx >= level)
}

// compatibleWith reports whether v is a compatible upgrade of older at the
// given level: parts before level must be equal and v must not be smaller.
func (v Version) compatibleWith(older Version, level int) bool {
	return v.epoch == older.epoch &&
		compatSection(v.ver(), older.ver(), level) &&
		compatSection(v.local(), older.local(), level)
}

// CompatibleWith reports whether v satisfies "~=o": v >= o and v agrees
// with o on all but o's last part. For example 1.3.1 and 1.9 are compatible
// with 1.2, but 2.0 and 1.1 are not; 1.2.5 is compatible with 1.2.3, but
// 1.3.0 is not.
func (v Version) CompatibleWith(o Version) bool {
	return v.compatibleWith(o, compatibleLevel(o))
}

// compatibleLevel is the level of "~=o": the number of o's parts minus one.
func compatibleLevel(o Version) int {
	n := countParts(o.ver())
	if n < 1 {
		n = 1
	}
	return n - 1
}

// isGlobPart reports whether a part equals the glob part "*".
func isGlobPart(p []atom) bool {
	glob := [1]atom{{lit: "*", kind: kindStar, flags: flagPartStart}}
	return cmpParts(p, glob[:]) == 0
}

// String returns the normalized form of v: parts joined by '.', letters in
// lower case, no leading zeros, e.g. "1!2.0.dev1+abc". Parsing the result
// yields a Version equal to v.
func (v Version) String() string {
	var b strings.Builder
	b.Grow(2 * len(v.atoms))
	v.format(&b, -1, fmtNormal)
	return b.String()
}

// stringLevel formats v with exactly level parts in the version and in the
// local version, truncating or appending ".0" as needed.
func (v Version) stringLevel(level int) string {
	var b strings.Builder
	v.format(&b, level, fmtNormal)
	return b.String()
}

// globString formats v writing glob parts as "*" rather than "0*".
func (v Version) globString() string {
	var b strings.Builder
	v.format(&b, -1, fmtGlob)
	return b.String()
}

// Formatting modes.
const (
	fmtNormal   = iota
	fmtGlob     // glob parts print as "*"
	fmtExplicit // implicit leading zeros print, so no part starts with '*'
)

func (v Version) format(b *strings.Builder, level int, mode int) {
	if v.epoch != 0 {
		b.WriteString(strconv.FormatUint(v.epoch, 10))
		b.WriteByte('!')
	}
	if v.nver == 0 && level < 0 {
		b.WriteByte('0') // zero Version
	} else {
		formatSection(b, v.ver(), level, mode)
	}
	if v.HasLocal() {
		b.WriteByte('+')
		formatSection(b, v.local(), level, mode)
	}
}

// formatCompatible writes v with exactly level version parts and the full
// local version.
func (v Version) formatCompatible(b *strings.Builder, level int, mode int) {
	if v.epoch != 0 {
		b.WriteString(strconv.FormatUint(v.epoch, 10))
		b.WriteByte('!')
	}
	formatSection(b, v.ver(), level, mode)
	if v.HasLocal() {
		b.WriteByte('+')
		formatSection(b, v.local(), -1, mode)
	}
}

func formatSection(b *strings.Builder, sec []atom, level int, mode int) {
	n := level
	if n < 0 {
		n = countParts(sec)
	}
	i := 0
	for k := 0; k < n; k++ {
		if k != 0 {
			b.WriteByte('.')
		}
		if i >= len(sec) {
			b.WriteByte('0')
			continue
		}
		ni := nextPart(sec, i)
		p := sec[i:ni]
		i = ni
		if mode == fmtGlob && isGlobPart(p) {
			b.WriteByte('*')
			continue
		}
		for k, a := range p {
			if k == 0 && mode != fmtExplicit && a.flags&flagImplicitZero != 0 && a.num == 0 && a.lit != "" {
				b.WriteString(a.lit)
				continue
			}
			b.WriteString(strconv.FormatUint(a.num, 10))
			b.WriteString(a.lit)
		}
	}
}
