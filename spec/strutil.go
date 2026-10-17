// Ported from libmamba src/util/string.cpp (BSD-3-Clause, see NOTICE).

package spec

// Byte-level string helpers mirroring libmamba's util/string.hpp. All of them
// work on ASCII only: conda specs are ASCII, and treating other bytes as
// opaque keeps the parsers from being tricked by Unicode case folding (for
// example U+212A KELVIN SIGN lower-casing to 'k').

const whitespace = " \t\n\v\f\r"

func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }
func isLower(c byte) bool { return 'a' <= c && c <= 'z' }
func isUpper(c byte) bool { return 'A' <= c && c <= 'Z' }
func isAlpha(c byte) bool { return isLower(c) || isUpper(c) }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

// isGraphic mirrors C isgraph in the "C" locale: printable, non-space ASCII.
func isGraphic(c byte) bool { return 0x21 <= c && c <= 0x7e }

func lstrip(s string) string {
	for len(s) > 0 && isSpace(s[0]) {
		s = s[1:]
	}
	return s
}

func rstrip(s string) string {
	for len(s) > 0 && isSpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

func strip(s string) string { return rstrip(lstrip(s)) }

// lstripChars removes leading bytes contained in set.
func lstripChars(s, set string) string {
	for len(s) > 0 && indexByte(set, s[0]) >= 0 {
		s = s[1:]
	}
	return s
}

// rstripChars removes trailing bytes contained in set.
func rstripChars(s, set string) string {
	for len(s) > 0 && indexByte(set, s[len(s)-1]) >= 0 {
		s = s[:len(s)-1]
	}
	return s
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// indexAny returns the index of the first byte of s contained in set, or -1.
func indexAny(s, set string) int {
	for i := 0; i < len(s); i++ {
		if indexByte(set, s[i]) >= 0 {
			return i
		}
	}
	return -1
}

// lastIndexAny returns the index of the last byte of s contained in set, or -1.
func lastIndexAny(s, set string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if indexByte(set, s[i]) >= 0 {
			return i
		}
	}
	return -1
}

// asciiLower lower-cases ASCII letters only. It returns s itself, without
// allocating, when there is nothing to change.
func asciiLower(s string) string {
	i := 0
	for i < len(s) && !isUpper(s[i]) {
		i++
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if isUpper(b[i]) {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
