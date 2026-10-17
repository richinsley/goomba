package solve

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// sanitize makes untrusted text (package names, versions, build strings,
// dependency strings, channels and URLs from repodata or from the caller)
// safe to show in an explanation or an error message: every rune that is
// not printable is written as a Go escape, as are invalid UTF-8 bytes.
// Backslashes are kept, so text that strconv.Quote already escaped (parse
// errors quote their input) is not escaped twice; an escape such as \x1b
// in the output may therefore also be literal text, but it never acts on
// a terminal. Non-printable means everything unicode.IsPrint
// rejects except the ASCII space: C0 and C1 controls (so no terminal escape
// sequence and no newline can forge a line of the explanation tree), DEL,
// the line and paragraph separators U+2028 and U+2029, the bidi controls
// U+202A-U+202E and U+2066-U+2069 and every other format character, and
// spaces other than U+0020.
//
// Printable characters that show as nothing are escaped too: the Unicode
// default-ignorable code points (soft hyphen, zero-width characters,
// variation selectors, the Hangul fillers U+115F, U+1160, U+3164 and U+FFA0,
// tag characters, ...) and the braille blank U+2800, as is every combining
// mark after the first maxCombiningMarks of a run, so stacked marks cannot
// smear over neighbouring lines.
//
// It is the one function this package uses for untrusted text; it is
// applied once, where text is rendered.
func sanitize(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	marks := 0 // length of the current run of combining marks
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.Is(unicode.M, r) {
			marks++
		} else {
			marks = 0
		}
		switch {
		case r == utf8.RuneError && size == 1:
			writeEscape(&b, `\x`, uint32(s[i]), 2)
		case visible(r) && marks <= maxCombiningMarks:
			b.WriteString(s[i : i+size])
		case r < 0x80:
			writeEscape(&b, `\x`, uint32(r), 2)
		case r <= 0xffff:
			writeEscape(&b, `\u`, uint32(r), 4)
		default:
			writeEscape(&b, `\U`, uint32(r), 8)
		}
		i += size
	}
	return b.String()
}

// cleanError carries a sanitized message for an error whose own message
// shows untrusted text; it unwraps to the original error.
type cleanError struct {
	msg string
	err error
}

func (e *cleanError) Error() string { return e.msg }
func (e *cleanError) Unwrap() error { return e.err }

// sanitizeError returns err unchanged if its message is clean text, and
// otherwise an error with the message sanitized that unwraps to err (so
// errors.Is and errors.As still see err and what it wraps). An
// *Unsolvable is returned as it is: its explanation is sanitized where it
// is built, and its newlines are part of it.
func sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*Unsolvable); ok {
		return err
	}
	msg := err.Error()
	if clean := sanitize(msg); clean != msg {
		return &cleanError{msg: clean, err: err}
	}
	return err
}

// writeEscape writes prefix and v in lower-case hex with digits digits.
func writeEscape(b *strings.Builder, prefix string, v uint32, digits int) {
	const hex = "0123456789abcdef"
	b.WriteString(prefix)
	for shift := 4 * (digits - 1); shift >= 0; shift -= 4 {
		b.WriteByte(hex[(v>>uint(shift))&0xf])
	}
}

// maxCombiningMarks is the longest run of combining marks sanitize keeps;
// real text stacks at most two or three.
const maxCombiningMarks = 3

// visible reports whether r shows as itself: printable (or the ASCII
// space) and neither default-ignorable nor the braille blank.
func visible(r rune) bool {
	return (r == ' ' || unicode.IsPrint(r)) && !defaultIgnorable(r) && r != 0x2800
}

// defaultIgnorable reports whether r has the Unicode property
// Default_Ignorable_Code_Point (DerivedCoreProperties.txt).
func defaultIgnorable(r rune) bool {
	switch {
	case r < 0xad:
		return false
	case r == 0xad, r == 0x34f, r == 0x61c, r == 0x115f, r == 0x1160,
		r == 0x17b4, r == 0x17b5, r == 0x3164, r == 0xfeff, r == 0xffa0:
		return true
	case r >= 0x180b && r <= 0x180f, r >= 0x200b && r <= 0x200f,
		r >= 0x202a && r <= 0x202e, r >= 0x2060 && r <= 0x206f,
		r >= 0xfe00 && r <= 0xfe0f, r >= 0xfff0 && r <= 0xfff8,
		r >= 0x1bca0 && r <= 0x1bca3, r >= 0x1d173 && r <= 0x1d17a,
		r >= 0xe0000 && r <= 0xe0fff:
		return true
	}
	return false
}

// isCleanText reports whether s contains nothing sanitize would escape
// other than newlines: what every explanation must satisfy.
func isCleanText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	marks := 0
	for _, r := range s {
		if unicode.Is(unicode.M, r) {
			marks++
		} else {
			marks = 0
		}
		if r != '\n' && (!visible(r) || marks > maxCombiningMarks) {
			return false
		}
	}
	return true
}
