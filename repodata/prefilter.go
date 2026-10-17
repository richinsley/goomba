// Ported from libmamba src/core/shard_python_minor_prefilter.cpp
// (BSD-3-Clause, see NOTICE). Version ordering follows libmamba
// src/specs/version.cpp (VersionPartAtom).

package repodata

import (
	"strconv"
	"strings"

	"github.com/richinsley/goomba/record"
)

// The python-minor prefilter drops records whose "python" dependency cannot
// be satisfied by any release of the requested minor series. libmamba asks
// whether the dependency's version spec contains the point version X.Y (and
// relaxes a single "==X.Y.Z" pin to X.Y). This package must not depend on
// the version-spec parser (package spec), so it evaluates the spec with a
// small, conservative evaluator instead:
//
//   - Only the canonical form conda-build writes is evaluated:
//     "python <version-spec>" or "python <version-spec> <build>", split by
//     single spaces, with the name exactly "python". Every other form
//     (operators attached to the name, extra spaces, channel prefixes,
//     brackets, other capitalization) keeps the record.
//   - The series S is every release X.Y[.anything >= 0]: X.Y, X.Y.0,
//     X.Y.5, X.Y.5.post1, ... but not pre-releases of X.Y.0 such as
//     X.Y.0rc1 (they sort below X.Y).
//   - A conjunction can match S only if every term can; a disjunction if any
//     alternative can. This over-approximates (">=X.Y.5,<X.Y.3" is kept),
//     which only costs solver time.
//   - Anything the evaluator does not understand inside the version spec
//     (parentheses, epochs, local versions, unusual characters, globs with
//     ordering operators) keeps the record.
//
// A record is dropped only when its canonical python dependency excludes
// the whole series, so whatever X.Y.z the solver picks, the record could
// not be installed with it. In particular "python >=X.Y.1" is kept
// (libmamba drops it although X.Y.5 satisfies it).

// allows reports whether rec passes the filter. A nil filter allows all.
func (p *pythonMinor) allows(rec *record.PackageRecord) bool {
	if p == nil {
		return true
	}
	for _, d := range rec.Depends {
		if !p.matches(d) {
			return false
		}
	}
	return true
}

// matches reports whether the dependency spec dep can be satisfied by a
// release of the series. Specs for other packages, and specs not in the
// canonical "python <spec> [<build>]" form, always match.
func (p *pythonMinor) matches(dep string) bool {
	fields := strings.Split(dep, " ")
	if len(fields) < 2 || len(fields) > 3 || fields[0] != "python" {
		return true
	}
	for _, f := range fields[1:] {
		if f == "" || strings.ContainsAny(f, "\t\r\n") {
			return true
		}
	}
	return p.specPossible(fields[1])
}

// specPossible evaluates a version spec such as ">=3.8,<4|2.7.*".
func (p *pythonMinor) specPossible(vs string) bool {
	if vs == "" || vs == "*" {
		return true
	}
	for _, alt := range strings.Split(vs, "|") {
		all := true
		for _, term := range strings.Split(alt, ",") {
			possible, understood := p.termPossible(term)
			if !understood {
				return true
			}
			if !possible {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

// Position of a version relative to the series S.
const (
	posBelow   = iota // below every element of S
	posMinimum        // equal to X.Y, the least element of S
	posInside         // an element of S above X.Y
	posAbove          // above every element of S
)

// termPossible evaluates one "op version" term. understood is false when
// the term is outside the subset the evaluator handles.
func (p *pythonMinor) termPossible(term string) (possible, understood bool) {
	op := ""
	for _, o := range []string{"==", "!=", ">=", "<=", "~=", ">", "<", "="} {
		if strings.HasPrefix(term, o) {
			op = o
			break
		}
	}
	vs := term[len(op):]
	glob := false
	switch {
	case strings.HasSuffix(vs, ".*"):
		glob, vs = true, vs[:len(vs)-2]
	case strings.HasSuffix(vs, "*"):
		glob, vs = true, vs[:len(vs)-1]
	}
	if glob && vs == "" && (op == "" || op == "==" || op == "=") {
		return true, true // "*"
	}
	v, ok := parseLiteVersion(vs)
	if !ok {
		return false, false
	}
	switch op {
	case "", "=", "==":
		pre, _, known := p.prefix(v)
		if !known {
			return false, false
		}
		if glob || op == "=" {
			return pre, true // fuzzy: startswith
		}
		pos := p.position(v)
		exact := pos == posMinimum || pos == posInside || p.sameMinor(v)
		if op == "" {
			return exact || pre, true // bare version: exact or fuzzy, whichever keeps more
		}
		return exact, true
	case "!=":
		if glob {
			_, all, known := p.prefix(v)
			if !known {
				return false, false
			}
			return !all, true
		}
		return true, true
	}
	if glob {
		return false, false
	}
	pos := p.position(v)
	switch op {
	case ">=", ">", "~=": // ~=V implies >=V
		return pos != posAbove, true
	case "<=":
		return pos != posBelow, true
	default: // "<"
		return pos == posInside || pos == posAbove, true
	}
}

// sameMinor reports whether v starts with the numerals X and Y, the
// condition under which libmamba relaxes "==X.Y.Z..." to "==X.Y".
func (p *pythonMinor) sameMinor(v liteVersion) bool {
	return len(v) >= 2 && len(v[0]) == 1 && v[0][0] == (atom{num: p.major}) &&
		len(v[1]) >= 1 && v[1][0].num == p.minor
}

// position places v relative to the series.
func (p *pythonMinor) position(v liteVersion) int {
	if c := comparePart(v.part(0), []atom{{num: p.major}}); c != 0 {
		return sign(c, posBelow, posAbove)
	}
	if c := comparePart(v.part(1), []atom{{num: p.minor}}); c != 0 {
		return sign(c, posBelow, posAbove)
	}
	for i := 2; i < len(v); i++ {
		if c := comparePart(v[i], nil); c != 0 {
			return sign(c, posBelow, posInside)
		}
	}
	return posMinimum
}

func sign(c, neg, pos int) int {
	if c < 0 {
		return neg
	}
	return pos
}

// prefix reports whether some element of S starts with v (possible) and
// whether every element does (all), component-wise as conda's startswith.
// known is false for prefixes with literals in the first two parts.
func (p *pythonMinor) prefix(v liteVersion) (possible, all, known bool) {
	numeral := func(part []atom) (uint64, bool) {
		if len(part) == 1 && part[0].lit == "" {
			return part[0].num, true
		}
		return 0, false
	}
	x, ok := numeral(v[0])
	if !ok {
		return false, false, false
	}
	if x != p.major {
		return false, false, true
	}
	if len(v) == 1 {
		return true, true, true
	}
	y, ok := numeral(v[1])
	if !ok {
		return false, false, false
	}
	if y != p.minor {
		return false, false, true
	}
	return true, len(v) == 2, true
}

// liteVersion is a conda version restricted to digits, letters and dots
// (no epoch, no local version): parts split on '.', each part a list of
// atoms (numeral, literal), as libmamba's Version.
type liteVersion [][]atom

type atom struct {
	num uint64
	lit string
}

func (v liteVersion) part(i int) []atom {
	if i < len(v) {
		return v[i]
	}
	return nil
}

func parseLiteVersion(s string) (liteVersion, bool) {
	if s == "" {
		return nil, false
	}
	s = strings.ToLower(s)
	var v liteVersion
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return nil, false
		}
		var atoms []atom
		for i := 0; i < len(part); {
			j := i
			for j < len(part) && part[j] >= '0' && part[j] <= '9' {
				j++
			}
			var num uint64
			if j > i {
				n, err := strconv.ParseUint(part[i:j], 10, 64)
				if err != nil {
					return nil, false
				}
				num = n
			}
			k := j
			for k < len(part) && part[k] >= 'a' && part[k] <= 'z' {
				k++
			}
			if k == i {
				return nil, false // neither digit nor letter
			}
			atoms = append(atoms, atom{num: num, lit: part[j:k]})
			i = k
		}
		v = append(v, atoms)
	}
	return v, true
}

// litPriority mirrors libmamba's VersionPartAtom ordering of literals.
func litPriority(l string) int {
	switch l {
	case "*":
		return -3
	case "dev":
		return -2
	case "_":
		return -1
	case "":
		return 1
	case "post":
		return 2
	}
	return 0
}

func compareAtom(a, b atom) int {
	switch {
	case a.num < b.num:
		return -1
	case a.num > b.num:
		return 1
	}
	pa, pb := litPriority(a.lit), litPriority(b.lit)
	if pa == 0 && pb == 0 {
		return strings.Compare(a.lit, b.lit)
	}
	switch {
	case pa < pb:
		return -1
	case pa > pb:
		return 1
	}
	return 0
}

// comparePart compares two parts, padding the shorter with empty atoms
// (0, ""), so "1" == "1.0" component-wise.
func comparePart(a, b []atom) int {
	n := max(len(a), len(b))
	for i := 0; i < n; i++ {
		var x, y atom
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if c := compareAtom(x, y); c != 0 {
			return c
		}
	}
	return 0
}
