// Ported from resolvo tests/solver/bundle_box/parser.rs, spec.rs,
// conditional_spec.rs and pack.rs (BSD-3-Clause, see NOTICE).

package bundlebox

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/richinsley/goomba/cdcl"
)

// Pack is a version in the BundleBox packaging system. The flags let tests
// simulate unknown dependencies and cancellation. It is a plain value, safe
// for concurrent use.
type Pack struct {
	// Version is the version number.
	Version uint32
	// UnknownDeps makes the provider report unknown dependencies.
	UnknownDeps bool
	// CancelDuringGetDependencies makes the provider cancel the solve when
	// the dependencies are requested.
	CancelDuringGetDependencies bool
}

// NewPack returns a plain pack with the given version.
func NewPack(v uint32) Pack { return Pack{Version: v} }

// WithUnknownDeps returns a copy whose dependencies are unknown.
func (p Pack) WithUnknownDeps() Pack { p.UnknownDeps = true; return p }

// WithCancelDuringGetDependencies returns a copy that cancels the solve when
// its dependencies are requested.
func (p Pack) WithCancelDuringGetDependencies() Pack {
	p.CancelDuringGetDependencies = true
	return p
}

// String returns the version number.
func (p Pack) String() string { return fmt.Sprint(p.Version) }

// Compare orders packs by version, then by the flags (false before true),
// like the derived Ord of the Rust struct.
func (p Pack) Compare(o Pack) int {
	switch {
	case p.Version != o.Version:
		if p.Version < o.Version {
			return -1
		}
		return 1
	case p.UnknownDeps != o.UnknownDeps:
		if !p.UnknownDeps {
			return -1
		}
		return 1
	case p.CancelDuringGetDependencies != o.CancelDuringGetDependencies:
		if !p.CancelDuringGetDependencies {
			return -1
		}
		return 1
	}
	return 0
}

type rangesKind uint8

const (
	rangesEmpty rangesKind = iota
	rangesFull
	rangesBetween
)

// Ranges is the subset of version_ranges::Ranges<Pack> that the BundleBox
// parser produces: empty, full, or a half-open interval [lo, hi). It is a
// plain value, safe for concurrent use.
type Ranges struct {
	kind   rangesKind
	lo, hi Pack
}

// Full returns the range containing every pack.
func Full() Ranges { return Ranges{kind: rangesFull} }

// Between returns [lo, hi), or the empty range if lo >= hi.
func Between(lo, hi Pack) Ranges {
	if lo.Compare(hi) >= 0 {
		return Ranges{}
	}
	return Ranges{kind: rangesBetween, lo: lo, hi: hi}
}

// Contains reports whether p is in the range.
func (r Ranges) Contains(p Pack) bool {
	switch r.kind {
	case rangesFull:
		return true
	case rangesBetween:
		return r.lo.Compare(p) <= 0 && p.Compare(r.hi) < 0
	}
	return false
}

// String formats the range like version_ranges.
func (r Ranges) String() string {
	switch r.kind {
	case rangesFull:
		return "*"
	case rangesBetween:
		return fmt.Sprintf(">=%s, <%s", r.lo, r.hi)
	}
	return "∅"
}

// Spec is a package name with a range of versions, e.g. "foo 1..3". It is a
// plain value, safe for concurrent use.
type Spec struct {
	// Name is the package name.
	Name string
	// Versions is the version range.
	Versions Ranges
}

// LogicalOperator aliases cdcl.LogicalOperator.
type LogicalOperator = cdcl.LogicalOperator

// SpecCondition is a condition tree: either a Spec or a binary operation.
// It is plain data: safe for concurrent reads.
type SpecCondition struct {
	// Op combines Left and Right when Requirement is nil.
	Op LogicalOperator
	// Left and Right are the operands of a binary condition.
	Left, Right *SpecCondition
	// Requirement is set for a leaf condition.
	Requirement *Spec
}

// key returns a canonical string identifying the condition structurally.
func (c *SpecCondition) key() string {
	if c.Requirement != nil {
		return fmt.Sprintf("(%s %d %s %s)", c.Requirement.Name, c.Requirement.Versions.kind, c.Requirement.Versions.lo.key(), c.Requirement.Versions.hi.key())
	}
	return fmt.Sprintf("(%s %s %s)", c.Left.key(), c.Op, c.Right.key())
}

func (p Pack) key() string {
	return fmt.Sprintf("%d/%t/%t", p.Version, p.UnknownDeps, p.CancelDuringGetDependencies)
}

// ConditionalSpec is a union of specs with an optional condition, e.g.
// "foo 1 | bar; if baz and qux 2". It is plain data: safe for concurrent
// reads.
type ConditionalSpec struct {
	// Condition gates the requirement, or is nil.
	Condition *SpecCondition
	// Specs are the members of the union; there is at least one.
	Specs []Spec
}

// ErrSyntax is returned (wrapped) for malformed specs.
var ErrSyntax = errors.New("bundlebox: syntax error")

type parser struct {
	s   string
	pos int
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("%w: parse %q at offset %d: %s", ErrSyntax, p.s, p.pos, fmt.Sprintf(format, args...))
}

func (p *parser) skipWS() {
	for p.pos < len(p.s) {
		r, n := utf8.DecodeRuneInString(p.s[p.pos:])
		if !unicode.IsSpace(r) {
			return
		}
		p.pos += n
	}
}

func (p *parser) eof() bool { return p.pos >= len(p.s) }

// name parses an identifier: an ASCII letter or '_' followed by ASCII
// letters, digits, '_' or '-'.
func (p *parser) name() (string, error) {
	start := p.pos
	if p.eof() {
		return "", p.errorf("expected a name")
	}
	c := p.s[p.pos]
	if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return "", p.errorf("expected a name")
	}
	p.pos++
	for !p.eof() {
		c := p.s[p.pos]
		if c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
			continue
		}
		break
	}
	return p.s[start:p.pos], nil
}

// integer parses chumsky's text::int(10): "0" or a non-zero digit followed
// by digits.
func (p *parser) integer() (uint32, bool, error) {
	if p.eof() || p.s[p.pos] < '0' || p.s[p.pos] > '9' {
		return 0, false, nil
	}
	start := p.pos
	if p.s[p.pos] == '0' {
		p.pos++
	} else {
		for !p.eof() && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
			p.pos++
		}
	}
	var v uint64
	for _, c := range p.s[start:p.pos] {
		v = v*10 + uint64(c-'0')
		if v > math.MaxUint32 {
			return 0, false, p.errorf("version number out of range")
		}
	}
	return uint32(v), true, nil
}

// ranges parses "N" or "N..M" (whitespace allowed around ".." and after M).
func (p *parser) ranges() (Ranges, bool, error) {
	left, ok, err := p.integer()
	if err != nil || !ok {
		return Ranges{}, false, err
	}
	right := uint64(left) + 1
	save := p.pos
	p.skipWS()
	if strings.HasPrefix(p.s[p.pos:], "..") {
		p.pos += 2
		p.skipWS()
		r, ok, err := p.integer()
		if err != nil {
			return Ranges{}, false, err
		}
		if !ok {
			p.pos = save
		} else {
			right = uint64(r)
			p.skipWS()
		}
	} else {
		p.pos = save
	}
	if right > math.MaxUint32 {
		return Ranges{}, false, p.errorf("version number out of range")
	}
	return Between(NewPack(left), NewPack(uint32(right))), true, nil
}

// spec parses a name with optional ranges, padded with whitespace.
func (p *parser) spec() (Spec, error) {
	p.skipWS()
	name, err := p.name()
	if err != nil {
		return Spec{}, err
	}
	p.skipWS()
	r, ok, err := p.ranges()
	if err != nil {
		return Spec{}, err
	}
	if !ok {
		r = Full()
	}
	return Spec{Name: name, Versions: r}, nil
}

// keyword matches a padded keyword, restoring the position on failure.
func (p *parser) keyword(kw string) bool {
	save := p.pos
	p.skipWS()
	if strings.HasPrefix(p.s[p.pos:], kw) {
		p.pos += len(kw)
		p.skipWS()
		return true
	}
	p.pos = save
	return false
}

// condition parses specs joined by "and"/"or", left-associative with equal
// precedence.
func (p *parser) condition() (*SpecCondition, error) {
	first, err := p.spec()
	if err != nil {
		return nil, err
	}
	lhs := &SpecCondition{Requirement: &first}
	for {
		var op LogicalOperator
		switch {
		case p.keyword("and"):
			op = cdcl.And
		case p.keyword("or"):
			op = cdcl.Or
		default:
			return lhs, nil
		}
		rs, err := p.spec()
		if err != nil {
			return nil, err
		}
		lhs = &SpecCondition{Op: op, Left: lhs, Right: &SpecCondition{Requirement: &rs}}
	}
}

// ParseSpec parses a single spec such as "foo", "foo 3" or "foo 1..5".
func ParseSpec(s string) (Spec, error) {
	p := &parser{s: s}
	sp, err := p.spec()
	if err != nil {
		return Spec{}, err
	}
	if !p.eof() {
		return Spec{}, p.errorf("unexpected trailing input")
	}
	return sp, nil
}

// ParseConditionalSpec parses a union of specs separated by "|", optionally
// followed by "; if" and a condition.
func ParseConditionalSpec(s string) (ConditionalSpec, error) {
	p := &parser{s: s}
	var out ConditionalSpec
	for {
		sp, err := p.spec()
		if err != nil {
			return ConditionalSpec{}, err
		}
		out.Specs = append(out.Specs, sp)
		if !p.keyword("|") {
			break
		}
	}
	if p.keyword("; if") {
		c, err := p.condition()
		if err != nil {
			return ConditionalSpec{}, err
		}
		out.Condition = c
	}
	if !p.eof() {
		return ConditionalSpec{}, p.errorf("unexpected trailing input")
	}
	return out, nil
}
