package virtual

import (
	"errors"
	"strings"
)

// maxValueLen bounds every version or build string this package accepts.
const maxValueLen = 64

// checkVersion accepts the subset of conda version syntax a virtual package
// can carry: ASCII letters, digits and the separators '.', '_', '+' and '!',
// with no empty segment, at most one '!' (preceded by a numeric epoch) and at
// most one '+'. Package spec owns full version parsing; this only keeps
// garbage away from the solver.
func checkVersion(v string) error {
	if v == "" {
		return errors.New("empty version")
	}
	if len(v) > maxValueLen {
		return errors.New("version too long")
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !isAlnum(c) && c != '.' && c != '_' && c != '+' && c != '!' {
			return errors.New("invalid character " + quoteByte(c))
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
		sep := isVersionSep(v[i])
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

func isVersionSep(c byte) bool { return c == '.' || c == '_' || c == '+' || c == '!' }

// checkBuild accepts a conda build string: ASCII letters, digits, '_', '.'
// and '+'. A dash is rejected because conda file names split on it.
func checkBuild(b string) error {
	if b == "" {
		return errors.New("empty build string")
	}
	if len(b) > maxValueLen {
		return errors.New("build string too long")
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !isAlnum(c) && c != '_' && c != '.' && c != '+' {
			return errors.New("invalid character " + quoteByte(c))
		}
	}
	return nil
}

// checkName accepts a virtual package name: "__" followed by lower-case
// ASCII letters, digits and '_', starting with a letter or digit.
func checkName(n string) error {
	rest, ok := strings.CutPrefix(n, "__")
	if !ok || rest == "" {
		return errors.New("name must be \"__\" followed by at least one character")
	}
	if len(n) > maxValueLen {
		return errors.New("name too long")
	}
	if rest[0] == '_' {
		return errors.New("name must not start with three underscores")
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c >= 'a' && c <= 'z') && !isDigit(c) && c != '_' {
			return errors.New("invalid character " + quoteByte(c))
		}
	}
	return nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlnum(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func quoteByte(c byte) string {
	const hex = "0123456789abcdef"
	if c >= 0x20 && c < 0x7f && c != '\'' && c != '\\' {
		return "'" + string(rune(c)) + "'"
	}
	return "'\\x" + string(hex[c>>4]) + string(hex[c&15]) + "'"
}

// numericPrefix returns the longest prefix of s of the form
// digits ('.' digits){minParts-1,maxParts-1}, or "" if s does not start with
// at least minParts dot-separated numbers. It is the shape conda and rattler
// take from a kernel release or a glibc banner.
func numericPrefix(s string, minParts, maxParts int) string {
	end, parts := 0, 0
	i := 0
	for parts < maxParts {
		j := i
		if parts > 0 {
			if j >= len(s) || s[j] != '.' {
				break
			}
			j++
		}
		k := j
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k == j {
			break
		}
		parts++
		end, i = k, k
	}
	if parts < minParts || end > maxValueLen {
		return ""
	}
	return s[:end]
}
