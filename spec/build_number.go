// Ported from libmamba src/specs/build_number_spec.cpp (BSD-3-Clause, see
// NOTICE).

package spec

import "strconv"

// BuildNumberSpec is a condition on a build number: "3" or "=3", "!=3",
// ">3", ">=3", "<3", "<=3", or "*" for any.
//
// The zero BuildNumberSpec matches every build number. A BuildNumberSpec is
// immutable and safe for concurrent use.
type BuildNumberSpec struct {
	num uint64
	op  predOp // opFree, opEqual, opNotEqual, opGreater, ...
}

// ParseBuildNumberSpec parses a build number spec.
func ParseBuildNumberSpec(s string) (BuildNumberSpec, error) {
	str := strip(s)
	if isFreeVersionStr(str) {
		return BuildNumberSpec{}, nil
	}
	i := 0
	for i < len(str) && !isDigit(str[i]) {
		i++
	}
	opStr, numStr := strip(str[:i]), str[i:]
	num, ok := parseUint(numStr)
	if !ok {
		return BuildNumberSpec{}, parseErr(ErrBuildNumberSpec, s, "expected a number")
	}
	var op predOp
	switch opStr {
	case "", "=":
		op = opEqual
	case "!=":
		op = opNotEqual
	case ">":
		op = opGreater
	case ">=":
		op = opGreaterEqual
	case "<":
		op = opLess
	case "<=":
		op = opLessEqual
	default:
		return BuildNumberSpec{}, parseErr(ErrBuildNumberSpec, s, "invalid operator "+strconv.Quote(opStr))
	}
	return BuildNumberSpec{num: num, op: op}, nil
}

// Contains reports whether n satisfies the spec.
func (b BuildNumberSpec) Contains(n uint64) bool {
	switch b.op {
	case opEqual:
		return n == b.num
	case opNotEqual:
		return n != b.num
	case opGreater:
		return n > b.num
	case opGreaterEqual:
		return n >= b.num
	case opLess:
		return n < b.num
	case opLessEqual:
		return n <= b.num
	}
	return true
}

// IsAny reports whether the spec matches every build number.
func (b BuildNumberSpec) IsAny() bool { return b.op == opFree }

// String returns the normalized spec, e.g. "=3", ">=2" or "=*".
func (b BuildNumberSpec) String() string {
	var op string
	switch b.op {
	case opFree:
		return "=*"
	case opEqual:
		op = "="
	case opNotEqual:
		op = "!="
	case opGreater:
		op = ">"
	case opGreaterEqual:
		op = ">="
	case opLess:
		op = "<"
	case opLessEqual:
		op = "<="
	}
	return op + strconv.FormatUint(b.num, 10)
}
