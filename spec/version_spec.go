// Ported from libmamba src/specs/version_spec.cpp and
// include/mamba/util/flat_bool_expr_tree.hpp (BSD-3-Clause, see NOTICE).

package spec

import (
	"strconv"
	"strings"
)

// predOp is the operator of a version predicate.
type predOp uint8

const (
	opFree          predOp = iota // "*", any version
	opEqual                       // "==1.2" or "1.2"
	opNotEqual                    // "!=1.2"
	opGreater                     // ">1.2"
	opGreaterEqual                // ">=1.2"
	opLess                        // "<1.2"
	opLessEqual                   // "<=1.2"
	opStartsWith                  // "=1.2", "1.2.*", "1.2*"
	opNotStartsWith               // "!=1.2.*"
	opCompatible                  // "~=1.2"
	opGlob                        // "1.*.3", "*.4"
	opNotGlob                     // "!=1.*.3"
)

// versionPredicate is libmamba's VersionPredicate: a unary condition on
// versions.
type versionPredicate struct {
	ver   Version
	level int // opCompatible only
	op    predOp
	// globSuffix marks ">X.*", "<X.*", "<=X.*" and "~=X.*", which match
	// nothing (see VersionSpec).
	globSuffix bool
}

func (p *versionPredicate) contains(v *Version) bool {
	if p.globSuffix {
		return false
	}
	switch p.op {
	case opFree:
		return true
	case opEqual:
		return v.Compare(p.ver) == 0
	case opNotEqual:
		return v.Compare(p.ver) != 0
	case opGreater:
		return v.Compare(p.ver) > 0
	case opGreaterEqual:
		return v.Compare(p.ver) >= 0
	case opLess:
		return v.Compare(p.ver) < 0
	case opLessEqual:
		return v.Compare(p.ver) <= 0
	case opStartsWith:
		return v.StartsWith(p.ver)
	case opNotStartsWith:
		return !v.StartsWith(p.ver)
	case opCompatible:
		return v.compatibleWith(p.ver, p.level)
	case opGlob:
		return versionGlobMatch(v, &p.ver)
	case opNotGlob:
		return !versionGlobMatch(v, &p.ver)
	}
	return false
}

func (p *versionPredicate) equal(o *versionPredicate) bool {
	if p.op != o.op || p.globSuffix != o.globSuffix || (p.op == opCompatible && p.level != o.level) {
		return false
	}
	if p.ver.Compare(o.ver) != 0 {
		return false
	}
	// In a glob, "*" and explicit trailing parts are significant.
	return p.op != opGlob || countParts(p.ver.ver()) == countParts(o.ver.ver())
}

func (p *versionPredicate) hasGlob() bool { return p.op == opGlob || p.op == opNotGlob }

func (p *versionPredicate) isClassicOperator() bool {
	switch p.op {
	case opEqual, opNotEqual, opGreater, opGreaterEqual, opLess, opLessEqual:
		return true
	}
	return false
}

func (p *versionPredicate) writeTo(b *strings.Builder, condaBuild bool) {
	switch p.op {
	case opFree:
		b.WriteString("=*")
	case opEqual:
		b.WriteString("==")
		p.writeVersion(b)
	case opNotEqual:
		b.WriteString("!=")
		p.writeVersion(b)
	case opGreater:
		b.WriteString(">")
		p.writeVersion(b)
	case opGreaterEqual:
		b.WriteString(">=")
		p.writeVersion(b)
	case opLess:
		b.WriteString("<")
		p.writeVersion(b)
	case opLessEqual:
		b.WriteString("<=")
		p.writeVersion(b)
	case opStartsWith:
		if condaBuild {
			p.writeVersion(b)
			b.WriteString(".*")
		} else {
			b.WriteString("=")
			p.writeVersion(b)
		}
	case opNotStartsWith:
		b.WriteString("!=")
		p.writeVersion(b)
		b.WriteString(".*")
	case opCompatible:
		// Print enough parts not to lose information.
		level := countParts(p.ver.ver())
		if p.level > level {
			level = p.level
		}
		b.WriteString("~=")
		// Unlike libmamba, which also truncates the local version to the
		// level, only the version part is padded.
		var t strings.Builder
		p.ver.formatCompatible(&t, level, fmtNormal)
		if s := t.String(); strings.Contains(s, ".*") || strings.HasPrefix(s, "*") {
			t.Reset()
			p.ver.formatCompatible(&t, level, fmtExplicit)
		}
		b.WriteString(t.String())
	case opGlob:
		b.WriteString(p.globText())
	case opNotGlob:
		b.WriteString("!=")
		b.WriteString(p.globText())
	}
	if p.globSuffix {
		b.WriteString(".*")
	}
}

// writeVersion writes the version of a non-glob predicate. A version with a
// '*' part, as in "==1.2.*.*", would print with a ".*" that reads back as a
// glob; such versions print their implicit zeros instead ("1.2.0*"), which
// parse to the same atoms.
func (p *versionPredicate) writeVersion(b *strings.Builder) {
	s := p.ver.String()
	if strings.Contains(s, ".*") || strings.HasPrefix(s, "*") {
		p.ver.format(b, -1, fmtExplicit)
		return
	}
	b.WriteString(s)
}

// globText returns a glob predicate's pattern, written so that it parses
// back as a glob: glob parts print as "*", unless that hides the glob (as
// for the part "*0", equal to "*"), in which case the parts print as parsed.
func (p *versionPredicate) globText() string {
	if s := p.ver.globString(); hasComplexGlob(s) && !isFreeVersionStr(s) {
		return s
	}
	return p.ver.String()
}

// versionGlobMatch reports whether v matches a glob pattern such as
// "1.*.3", where each "*" part stands for one or more parts.
func versionGlobMatch(v, pattern *Version) bool {
	return v.epoch == pattern.epoch &&
		sectionGlobMatch(v.ver(), pattern.ver()) &&
		sectionGlobMatch(v.local(), pattern.local())
}

// partStarts records the atom index at which each part of sec starts,
// followed by len(sec), and returns the number of parts, or -1 if sec has
// more than maxVersionParts parts (impossible for parsed versions).
func partStarts(sec []atom, starts *[maxVersionParts + 1]int) int {
	n := 0
	for i := range sec {
		if sec[i].flags&flagPartStart != 0 {
			if n == maxVersionParts {
				return -1
			}
			starts[n] = i
			n++
		}
	}
	starts[n] = len(sec)
	return n
}

// sectionGlobMatch reports whether the parts of cand match the parts of
// pat, where each glob part "*" stands for one or more parts and any other
// part must equal one part (libmamba's version_match_glob, which however
// anchors each literal run at its first occurrence and so rejects "3.3" for
// "*.3").
//
// The parts before the first glob must be a prefix of cand and the parts
// after the last a suffix; every literal run in between is placed at its
// leftmost possible position. Greedy placement is correct because globs are
// the only wildcards, and it never backtracks: the cost is bounded by
// len(cand)*len(pat) part comparisons, at most 64*64. It does not allocate.
func sectionGlobMatch(cand, pat []atom) bool {
	var cs, ps [maxVersionParts + 1]int
	nc, np := partStarts(cand, &cs), partStarts(pat, &ps)
	if nc < 0 || np < 0 {
		return false
	}
	partEq := func(ci, pi int) bool { return cmpParts(cand[cs[ci]:cs[ci+1]], pat[ps[pi]:ps[pi+1]]) == 0 }
	isGlob := func(pi int) bool { return isGlobPart(pat[ps[pi]:ps[pi+1]]) }

	first, last := -1, -1
	for pi := 0; pi < np; pi++ {
		if isGlob(pi) {
			if first < 0 {
				first = pi
			}
			last = pi
		}
	}
	if first < 0 {
		if nc != np {
			return false
		}
		for i := 0; i < np; i++ {
			if !partEq(i, i) {
				return false
			}
		}
		return true
	}
	// Anchored prefix and suffix.
	suffix := np - last - 1
	if nc < first+suffix {
		return false
	}
	for i := 0; i < first; i++ {
		if !partEq(i, i) {
			return false
		}
	}
	for i := 0; i < suffix; i++ {
		if !partEq(nc-suffix+i, last+1+i) {
			return false
		}
	}
	// The middle: runs of globs, each followed by a literal run, except
	// the last.
	cur, end := first, nc-suffix
	for pi := first; ; {
		globs := 0
		for pi <= last && isGlob(pi) {
			globs++
			pi++
		}
		if pi > last {
			return end-cur >= globs
		}
		runEnd := pi
		for !isGlob(runEnd) {
			runEnd++
		}
		run := runEnd - pi
		found := -1
		for at := cur + globs; at+run <= end && found < 0; at++ {
			ok := true
			for k := 0; k < run && ok; k++ {
				ok = partEq(at+k, pi+k)
			}
			if ok {
				found = at
			}
		}
		if found < 0 {
			return false
		}
		cur, pi = found+run, runEnd
	}
}

type nodeKind uint8

const (
	nodeLeaf nodeKind = iota
	nodeAnd
	nodeOr
)

type vsNode struct {
	pred        versionPredicate
	left, right int32
	kind        nodeKind
}

// maxSpecLen bounds the length of version specs, match specs and
// channels. The longest match spec in conda-forge has 227 bytes.
const maxSpecLen = 1024

// maxGlobPredicates bounds the glob predicates ("1.*.3", "!=*.4") in one
// version spec, each of which may cost up to 64x64 part comparisons.
const maxGlobPredicates = 16

// maxVersionSpecDepth bounds the nesting of a version spec's expression
// tree, so hostile input cannot drive evaluation into deep recursion. The
// depth counts left children only: evaluation and formatting iterate along
// right children, so long flat chains like "!=1,!=2,...,!=999" are fine;
// only parentheses can nest deeply, and real specs never come close (the
// deepest in conda-forge nests 2).
const maxVersionSpecDepth = 64

// VersionSpec is a set of versions: a boolean expression of "," (and) and
// "|" (or) over predicates such as ">=1.2", "!=1.3.*" or "~=2.0", with
// parentheses for grouping.
//
// "," binds tighter than "|", so "a,b|c" means "(a,b)|c", as in conda,
// rattler and libsolv, which micromamba uses for the depends and constrains
// of repodata records. libmamba's own parser, which micromamba applies only
// to complex specs given on its command line, reads "a,(b|c)" instead.
// The readings rarely differ in practice: of 31 mixed specs in conda-forge's
// repodata (March 2026) one does, "ruamel.yaml >=0.17.6,<0.18|0.16.6.*",
// which conda and micromamba satisfy with 0.16.6.
//
// A ".*" suffix after ">=", ">", "<", "<=" or "~=" follows libsolv:
// ">=3.6.*" means ">=3.6", and ">0.23.*", "<4.*", "<=3.6.*" and "~=3.6.*"
// match nothing. libmamba instead compares against a version whose last
// part is "*", which sorts lowest, so that ">=3.6.*" admits 3.6.0rc1; conda
// differs from both and rejects "~=X.*".
//
// Specs longer than 1024 bytes or with more than 16 glob predicates are
// rejected.
//
// The zero VersionSpec matches every version. A VersionSpec is immutable
// and safe for concurrent use.
type VersionSpec struct {
	nodes []vsNode // postfix order; the root is the last node
}

// ParseVersionSpec parses a version spec such as ">=1.2,<2", "1.2.*",
// "=1.2", "==1.2", "~=1.2", "1.2|1.4", "(>=1,<2)|>3" or "*". The empty
// string matches every version.
func ParseVersionSpec(s string) (VersionSpec, error) {
	if len(s) > maxSpecLen {
		return VersionSpec{}, parseErr(ErrVersionSpec, truncate(s), "longer than "+strconv.Itoa(maxSpecLen)+" bytes")
	}
	str := lstrip(s)
	if isFreeVersionStr(str) {
		return VersionSpec{}, nil
	}
	var p infixParser
	// A spec of n predicates has 2n-1 nodes.
	n := 1 + strings.Count(str, ",") + strings.Count(str, "|")
	p.nodes = make([]vsNode, 0, 2*n-1)
	p.depths = make([]int32, 0, 2*n-1)
	for str != "" {
		var ok bool
		var tok byte
		switch c := str[0]; c {
		case ',':
			tok, ok = c, p.pushOperator(nodeAnd)
		case '|':
			tok, ok = c, p.pushOperator(nodeOr)
		case '(':
			tok, ok = c, p.pushLeftParen()
		case ')':
			tok, ok = c, p.pushRightParen()
		default:
			i := indexAny(str, ",|()")
			if i < 0 {
				i = len(str)
			}
			pred, msg, err := parsePredicate(str[:i])
			if msg != "" || err != nil {
				return VersionSpec{}, wrapErr(ErrVersionSpec, s, msg, err)
			}
			if !p.pushVariable(pred) {
				return VersionSpec{}, parseErr(ErrVersionSpec, s, "unexpected version predicate "+strconv.Quote(strip(str[:i])))
			}
			str = lstrip(str[i:])
			continue
		}
		if p.tooDeep {
			return VersionSpec{}, parseErr(ErrVersionSpec, s, "expression nested too deeply")
		}
		if !ok {
			return VersionSpec{}, parseErr(ErrVersionSpec, s, "unexpected "+strconv.Quote(string(tok)))
		}
		str = lstrip(str[1:])
	}
	if !p.finalize() {
		if p.tooDeep {
			return VersionSpec{}, parseErr(ErrVersionSpec, s, "expression nested too deeply")
		}
		return VersionSpec{}, parseErr(ErrVersionSpec, s, "incomplete expression")
	}
	spec := VersionSpec{nodes: p.nodes}
	globs := 0
	for i := range spec.nodes {
		if spec.nodes[i].kind == nodeLeaf && spec.nodes[i].pred.hasGlob() {
			globs++
		}
	}
	if globs > maxGlobPredicates {
		return VersionSpec{}, parseErr(ErrVersionSpec, s, "more than "+strconv.Itoa(maxGlobPredicates)+" glob predicates")
	}
	if spec.IsAny() {
		// "* " or "." alone: the same as the empty spec.
		return VersionSpec{}, nil
	}
	// The canonical form must also fit, so that it parses back. It can be
	// longer than the input ("1|1" is "==1|==1"), by less than 4 times.
	if len(s) > maxSpecLen/4 && len(spec.String()) > maxSpecLen {
		return VersionSpec{}, parseErr(ErrVersionSpec, truncate(s), "normalized form longer than "+strconv.Itoa(maxSpecLen)+" bytes")
	}
	return spec, nil
}

func isFreeVersionStr(s string) bool {
	switch s {
	case "", "*", "=*", "==*":
		return true
	}
	return false
}

// parsePredicate is libmamba's parse_op_and_version. The order of the
// checks matters: some operators are prefixes of others. It reports either
// a message describing a malformed predicate or a version parse error.
func parsePredicate(raw string) (versionPredicate, string, error) {
	str := strip(raw)
	// conda-forge repodata has trailing dots, as in "openblas 0.2.18|0.2.18.*.".
	// Unlike libmamba, strip again, so that "1.* ." means "1.*".
	str = rstrip(strings.TrimSuffix(str, "."))
	if isFreeVersionStr(str) {
		return versionPredicate{op: opFree}, "", nil
	}
	mk := func(op predOp, verStr string) (versionPredicate, string, error) {
		v, err := ParseVersion(verStr)
		if err != nil {
			return versionPredicate{}, "", err
		}
		return versionPredicate{op: op, ver: v}, "", nil
	}
	// mkGlob makes a glob predicate. The lone glob "*", which libmamba
	// produces for "= *" or "* ." (but not "=*" or "*"), means any version.
	mkGlob := func(verStr string) (versionPredicate, string, error) {
		p, msg, err := mk(opGlob, verStr)
		if err == nil && p.ver.globString() == "*" && p.ver.String() == "*" {
			return versionPredicate{op: opFree}, "", nil
		}
		return p, msg, err
	}
	// relational handles the operators that compare: like libsolv, which
	// micromamba uses for repodata, ">=X.*" means ">=X" and ">X.*", "<X.*",
	// "<=X.*" and "~=X.*" match nothing.
	relational := func(op predOp, rest string) (versionPredicate, string, error) {
		base, globbed := rest, false
		for strings.HasSuffix(base, ".*") {
			base, globbed = base[:len(base)-2], true
		}
		p, msg, err := mk(op, base)
		if err == nil && op == opCompatible {
			// In "~=1.1" the level is 1, in "~=1.1.1" it is 2, etc.
			p.level = compatibleLevel(p.ver)
		}
		p.globSuffix = globbed && op != opGreaterEqual
		return p, msg, err
	}
	switch {
	case strings.HasPrefix(str, ">="):
		return relational(opGreaterEqual, lstrip(str[2:]))
	case strings.HasPrefix(str, ">"):
		return relational(opGreater, lstrip(str[1:]))
	case strings.HasPrefix(str, "<="):
		return relational(opLessEqual, lstrip(str[2:]))
	case strings.HasPrefix(str, "<"):
		return relational(opLess, lstrip(str[1:]))
	case strings.HasPrefix(str, "~="):
		return relational(opCompatible, lstrip(str[2:]))
	}

	hasGlobSuffix := strings.HasSuffix(str, ".*")
	globSuffixLen := 0
	if hasGlobSuffix {
		globSuffixLen = 2
	}
	switch {
	case strings.HasPrefix(str, "=="):
		if hasGlobSuffix {
			// "==1.3.*" means starts with 1.3.
			return mk(opStartsWith, lstrip(str[2:len(str)-globSuffixLen]))
		}
		return mk(opEqual, lstrip(str[2:]))
	case strings.HasPrefix(str, "!="):
		rest := lstrip(str[2:])
		if hasComplexGlob(rest) {
			return mk(opNotGlob, rest)
		}
		if hasGlobSuffix {
			return mk(opNotStartsWith, rest[:len(rest)-globSuffixLen])
		}
		return mk(opNotEqual, rest)
	case strings.HasPrefix(str, "="):
		rest := lstrip(str[1:])
		if hasComplexGlob(rest) {
			return mkGlob(rest)
		}
		// "=1.3.*" means the same as "=1.3".
		return mk(opStartsWith, rest[:len(rest)-globSuffixLen])
	}
	if hasComplexGlob(str) {
		return mkGlob(lstrip(str))
	}
	// Without an operator, a version must start with a digit or a
	// lower-case letter, as in conda's regex ^[\*\.\+!_0-9a-z]+$.
	if isDigit(str[0]) || isLower(str[0]) {
		if strings.HasSuffix(str, "*") {
			// "1.3.*" and "1.3*" both mean starts with 1.3.
			n := globSuffixLen
			if n < 1 {
				n = 1
			}
			return mk(opStartsWith, lstrip(str[:len(str)-n]))
		}
		return mk(opEqual, lstrip(str))
	}
	return versionPredicate{}, "invalid version predicate " + strconv.Quote(str), nil
}

// hasComplexGlob reports a glob that needs glob matching rather than a
// starts-with test: a leading "*" or a ".*" that is not the suffix.
func hasComplexGlob(expr string) bool {
	if strings.HasPrefix(expr, "*") {
		return true
	}
	i := strings.Index(expr, ".*")
	limit := len(expr)
	if limit < 2 {
		limit = 2
	}
	return i >= 0 && i < limit-2
}

// infixParser is libmamba's InfixParser feeding a PostfixParser, building
// the tree directly. "," has higher precedence than "|" (unlike libmamba,
// see VersionSpec); operators of equal precedence associate to the right.
type infixParser struct {
	nodes      []vsNode
	depths     []int32 // nesting depth of each node, see maxVersionSpecDepth
	orphans    []int32 // nodes without a parent yet
	ops        []byte  // pending operators: '&', '|' or '('
	parenLevel int
	expectsOp  bool
	tooDeep    bool
}

func opByte(k nodeKind) byte {
	if k == nodeOr {
		return '|'
	}
	return '&'
}

// precedence ranks "," above "|", as conda and libsolv do.
func precedence(op byte) int {
	if op == '&' {
		return 1
	}
	return 0
}

func (p *infixParser) pushVariable(pred versionPredicate) bool {
	if p.expectsOp {
		return false
	}
	p.expectsOp = true
	p.nodes = append(p.nodes, vsNode{pred: pred, kind: nodeLeaf, left: -1, right: -1})
	p.depths = append(p.depths, 0)
	p.orphans = append(p.orphans, int32(len(p.nodes)-1))
	return true
}

// applyOperator pops two orphans and joins them in a branch.
func (p *infixParser) applyOperator(op byte) bool {
	if len(p.orphans) < 2 {
		return false
	}
	right := p.orphans[len(p.orphans)-1]
	left := p.orphans[len(p.orphans)-2]
	p.orphans = p.orphans[:len(p.orphans)-2]
	kind := nodeAnd
	if op == '|' {
		kind = nodeOr
	}
	depth := p.depths[left] + 1
	if p.depths[right] > depth {
		depth = p.depths[right]
	}
	if depth > maxVersionSpecDepth {
		p.tooDeep = true
		return false
	}
	p.nodes = append(p.nodes, vsNode{kind: kind, left: left, right: right})
	p.depths = append(p.depths, depth)
	p.orphans = append(p.orphans, int32(len(p.nodes)-1))
	return true
}

func (p *infixParser) pushOperator(k nodeKind) bool {
	if !p.expectsOp {
		return false
	}
	p.expectsOp = false
	op := opByte(k)
	for len(p.ops) > 0 {
		top := p.ops[len(p.ops)-1]
		if top == '(' || precedence(op) >= precedence(top) {
			break
		}
		p.ops = p.ops[:len(p.ops)-1]
		if !p.applyOperator(top) {
			return false
		}
	}
	p.ops = append(p.ops, op)
	return true
}

func (p *infixParser) pushLeftParen() bool {
	if p.expectsOp {
		return false
	}
	p.parenLevel++
	p.ops = append(p.ops, '(')
	return true
}

func (p *infixParser) pushRightParen() bool {
	if !p.expectsOp || p.parenLevel == 0 {
		return false
	}
	p.parenLevel--
	for len(p.ops) > 0 && p.ops[len(p.ops)-1] != '(' {
		top := p.ops[len(p.ops)-1]
		p.ops = p.ops[:len(p.ops)-1]
		if !p.applyOperator(top) {
			return false
		}
	}
	if len(p.ops) == 0 {
		return false
	}
	p.ops = p.ops[:len(p.ops)-1]
	return true
}

func (p *infixParser) finalize() bool {
	if len(p.nodes) == 0 && len(p.ops) == 0 {
		return true
	}
	if !p.expectsOp || p.parenLevel != 0 {
		return false
	}
	for len(p.ops) > 0 {
		top := p.ops[len(p.ops)-1]
		p.ops = p.ops[:len(p.ops)-1]
		if top == '(' || !p.applyOperator(top) {
			return false
		}
	}
	return len(p.orphans) == 1
}

// Contains reports whether v is in the set. Contains does not allocate.
func (s VersionSpec) Contains(v Version) bool {
	if len(s.nodes) == 0 {
		return true
	}
	return s.eval(int32(len(s.nodes)-1), &v)
}

func (s VersionSpec) eval(i int32, v *Version) bool {
	for {
		n := &s.nodes[i]
		switch n.kind {
		case nodeAnd:
			if !s.eval(n.left, v) {
				return false
			}
		case nodeOr:
			if s.eval(n.left, v) {
				return true
			}
		default:
			return n.pred.contains(v)
		}
		i = n.right
	}
}

// IsAny reports whether the spec is trivially unconstrained, as for "",
// "*" or "=*". Specs that happen to match everything, like "<2|>=2", are
// not detected.
func (s VersionSpec) IsAny() bool {
	return len(s.nodes) == 0 ||
		(len(s.nodes) == 1 && s.nodes[0].kind == nodeLeaf && s.nodes[0].pred.op == opFree)
}

// expressionSize returns the number of nodes in the expression tree.
func (s VersionSpec) expressionSize() int { return len(s.nodes) }

// hasGlob reports whether any predicate is a glob such as "1.*.3".
func (s VersionSpec) hasGlob() bool {
	for i := range s.nodes {
		if s.nodes[i].kind == nodeLeaf && s.nodes[i].pred.hasGlob() {
			return true
		}
	}
	return false
}

// isClassicOperatorExpression reports whether every predicate is one of
// ==, !=, <, <=, >, >=.
func (s VersionSpec) isClassicOperatorExpression() bool {
	if len(s.nodes) == 0 {
		return false
	}
	for i := range s.nodes {
		if s.nodes[i].kind == nodeLeaf && !s.nodes[i].pred.isClassicOperator() {
			return false
		}
	}
	return true
}

// Equal reports whether s and o have the same expression tree. Different
// expressions of the same set, like "1.2" and "==1.2.0", compare equal only
// when their trees match.
func (s VersionSpec) Equal(o VersionSpec) bool {
	if len(s.nodes) != len(o.nodes) {
		return false
	}
	for i := range s.nodes {
		a, b := &s.nodes[i], &o.nodes[i]
		if a.kind != b.kind || a.left != b.left || a.right != b.right {
			return false
		}
		if a.kind == nodeLeaf && !a.pred.equal(&b.pred) {
			return false
		}
	}
	return true
}

// String returns the normalized spec, e.g. ">=1.2,<2" or "==1.7|==1.8";
// the empty spec is "=*". Sub-expressions are parenthesized, so the result
// may differ from the parsed text, but parsing it yields an equal spec.
func (s VersionSpec) String() string {
	return s.format(false)
}

// CondaBuildString is like String but writes starts-with predicates the way
// conda-build and libsolv understand them: "1.2.*" rather than "=1.2".
func (s VersionSpec) CondaBuildString() string {
	return s.format(true)
}

func (s VersionSpec) format(condaBuild bool) string {
	if len(s.nodes) == 0 {
		return "=*"
	}
	var b strings.Builder
	s.writeNode(&b, int32(len(s.nodes)-1), condaBuild)
	return b.String()
}

// writeNode writes the subtree rooted at i, recursing into left children
// and iterating along right children. Non-leaf children are parenthesized,
// except a right child with the same operator: the parser nests chains such
// as "a,b,c" to the right, so they read back unchanged (libmamba writes
// "a,(b,c)").
func (s VersionSpec) writeNode(b *strings.Builder, i int32, condaBuild bool) {
	closing := 0
	for {
		n := &s.nodes[i]
		if n.kind == nodeLeaf {
			n.pred.writeTo(b, condaBuild)
			break
		}
		s.writeChild(b, n.left, condaBuild)
		if n.kind == nodeOr {
			b.WriteByte('|')
		} else {
			b.WriteByte(',')
		}
		r := &s.nodes[n.right]
		if r.kind == nodeLeaf {
			r.pred.writeTo(b, condaBuild)
			break
		}
		if r.kind != n.kind {
			b.WriteByte('(')
			closing++
		}
		i = n.right
	}
	for ; closing > 0; closing-- {
		b.WriteByte(')')
	}
}

func (s VersionSpec) writeChild(b *strings.Builder, i int32, condaBuild bool) {
	if s.nodes[i].kind == nodeLeaf {
		s.writeNode(b, i, condaBuild)
		return
	}
	b.WriteByte('(')
	s.writeNode(b, i, condaBuild)
	b.WriteByte(')')
}
