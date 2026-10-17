package repodata

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// This file is a streaming JSON reader for repodata.json. encoding/json is
// not used for index documents because it rescans unconsumed whitespace
// (quadratic time on hostile input), materializes whole values before
// decoding them, and matches object keys case-insensitively. This reader
// reads each byte once, keeps nothing it is not asked to keep, matches keys
// exactly and lets callers cap every string and list.

const (
	maxJSONWhitespace = 64 << 10 // longest whitespace run between tokens
	maxJSONNumber     = 1 << 10  // longest number literal
	maxJSONDepth      = 10000    // deepest nesting skipValue accepts (as encoding/json)
)

var errJSONWhitespace = errors.New("json: whitespace run longer than 64 KiB")

type jsonReader struct {
	r       *bufio.Reader
	off     int64 // bytes consumed, for error messages
	scratch []byte
}

func newJSONReader(r io.Reader) *jsonReader {
	return &jsonReader{r: bufio.NewReaderSize(r, 64<<10)}
}

func (j *jsonReader) syntax(msg string, args ...any) error {
	return fmt.Errorf("json: offset %d: %s", j.off, fmt.Sprintf(msg, args...))
}

func (j *jsonReader) readByte() (byte, error) {
	c, err := j.r.ReadByte()
	if err != nil {
		if err == io.EOF {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	j.off++
	return c, nil
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// peek skips whitespace and returns the next byte without consuming it.
func (j *jsonReader) peek() (byte, error) {
	run := 0
	for {
		c, err := j.readByte()
		if err != nil {
			return 0, err
		}
		if isJSONSpace(c) {
			if run++; run > maxJSONWhitespace {
				return 0, errJSONWhitespace
			}
			continue
		}
		_ = j.r.UnreadByte()
		j.off--
		return c, nil
	}
}

// expect skips whitespace and consumes c.
func (j *jsonReader) expect(c byte) error {
	got, err := j.peek()
	if err != nil {
		return err
	}
	if got != c {
		return j.syntax("expected %q, found %q", c, got)
	}
	_, _ = j.readByte()
	return nil
}

// end checks that only whitespace follows the document.
func (j *jsonReader) end() error {
	run := 0
	for {
		c, err := j.r.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		j.off++
		if !isJSONSpace(c) {
			return j.syntax("trailing data after the top-level value")
		}
		if run++; run > maxJSONWhitespace {
			return errJSONWhitespace
		}
	}
}

// readString reads a string value. It returns ok false (with the value
// consumed) when the decoded string is longer than max bytes or is not
// valid UTF-8. Escapes follow encoding/json, including its replacement of
// unpaired surrogates by U+FFFD.
func (j *jsonReader) readString(max int) (s string, ok bool, err error) {
	if err := j.expect('"'); err != nil {
		return "", false, err
	}
	buf := j.scratch[:0]
	long := false
	add := func(p ...byte) {
		if len(buf)+len(p) > max {
			long = true
			return
		}
		buf = append(buf, p...)
	}
	for {
		c, err := j.readByte()
		if err != nil {
			return "", false, err
		}
		switch {
		case c == '"':
			if cap(buf) <= maxFieldBytes {
				j.scratch = buf[:0]
			}
			if long || !utf8.Valid(buf) {
				return "", false, nil
			}
			return string(buf), true, nil
		case c == '\\':
			e, err := j.readByte()
			if err != nil {
				return "", false, err
			}
			switch e {
			case '"', '\\', '/':
				add(e)
			case 'b':
				add('\b')
			case 'f':
				add('\f')
			case 'n':
				add('\n')
			case 'r':
				add('\r')
			case 't':
				add('\t')
			case 'u':
				r, err := j.hex4()
				if err != nil {
					return "", false, err
				}
				if utf16.IsSurrogate(r) {
					r = j.lowSurrogate(r)
				}
				add(utf8.AppendRune(nil, r)...)
			default:
				return "", false, j.syntax("invalid escape \\%c", e)
			}
		case c < 0x20:
			return "", false, j.syntax("control character in string")
		default:
			add(c)
		}
	}
}

func (j *jsonReader) hex4() (rune, error) {
	var r rune
	for i := 0; i < 4; i++ {
		c, err := j.readByte()
		if err != nil {
			return 0, err
		}
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			c = c - 'A' + 10
		default:
			return 0, j.syntax("invalid \\u escape")
		}
		r = r<<4 | rune(c)
	}
	return r, nil
}

// lowSurrogate completes a surrogate pair if the next escape is its low
// half; otherwise the surrogate becomes U+FFFD and the next escape is left
// for the caller, as encoding/json does.
func (j *jsonReader) lowSurrogate(hi rune) rune {
	p, err := j.r.Peek(6)
	if err != nil || p[0] != '\\' || p[1] != 'u' {
		return utf8.RuneError
	}
	var lo rune
	for _, c := range p[2:] {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			c = c - 'A' + 10
		default:
			return utf8.RuneError
		}
		lo = lo<<4 | rune(c)
	}
	dec := utf16.DecodeRune(hi, lo)
	if dec == utf8.RuneError {
		return utf8.RuneError
	}
	_, _ = j.r.Discard(6)
	j.off += 6
	return dec
}

// readNumber reads a number literal and checks its grammar.
func (j *jsonReader) readNumber() (string, error) {
	if _, err := j.peek(); err != nil {
		return "", err
	}
	buf := j.scratch[:0]
	for {
		c, err := j.r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if !(c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E') {
			_ = j.r.UnreadByte()
			break
		}
		j.off++
		if len(buf) >= maxJSONNumber {
			return "", j.syntax("number longer than %d bytes", maxJSONNumber)
		}
		buf = append(buf, c)
	}
	j.scratch = buf[:0]
	if !validJSONNumber(buf) {
		return "", j.syntax("invalid number %q", buf)
	}
	return string(buf), nil
}

// validJSONNumber checks -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?.
func validJSONNumber(b []byte) bool {
	i := 0
	digits := func() int {
		n := 0
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
			n++
		}
		return n
	}
	if i < len(b) && b[i] == '-' {
		i++
	}
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case digits() == 0:
		return false
	}
	if i < len(b) && b[i] == '.' {
		i++
		if digits() == 0 {
			return false
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	return i == len(b)
}

// readLiteral reads true, false or null.
func (j *jsonReader) readLiteral() (string, error) {
	c, err := j.peek()
	if err != nil {
		return "", err
	}
	var want string
	switch c {
	case 't':
		want = "true"
	case 'f':
		want = "false"
	case 'n':
		want = "null"
	default:
		return "", j.syntax("unexpected %q", c)
	}
	for i := 0; i < len(want); i++ {
		c, err := j.readByte()
		if err != nil {
			return "", err
		}
		if c != want[i] {
			return "", j.syntax("invalid literal")
		}
	}
	return want, nil
}

// skipValue consumes one value of any type, checking its grammar, without
// recursion and without keeping anything.
func (j *jsonReader) skipValue() error {
	var stack []byte // open containers: '{' or '['
	for {
		// A value is expected here.
		c, err := j.peek()
		if err != nil {
			return err
		}
		switch {
		case c == '{':
			_, _ = j.readByte()
			if c, err = j.peek(); err != nil {
				return err
			}
			if c == '}' {
				_, _ = j.readByte()
				break
			}
			if len(stack) >= maxJSONDepth {
				return j.syntax("nesting deeper than %d", maxJSONDepth)
			}
			stack = append(stack, '{')
			if err := j.memberKey(); err != nil {
				return err
			}
			continue
		case c == '[':
			_, _ = j.readByte()
			if c, err = j.peek(); err != nil {
				return err
			}
			if c == ']' {
				_, _ = j.readByte()
				break
			}
			if len(stack) >= maxJSONDepth {
				return j.syntax("nesting deeper than %d", maxJSONDepth)
			}
			stack = append(stack, '[')
			continue
		case c == '"':
			if _, _, err := j.readString(0); err != nil {
				return err
			}
		case c == 't' || c == 'f' || c == 'n':
			if _, err := j.readLiteral(); err != nil {
				return err
			}
		case c == '-' || c >= '0' && c <= '9':
			if _, err := j.readNumber(); err != nil {
				return err
			}
		default:
			return j.syntax("unexpected %q", c)
		}
		// A value ended: close containers or move to the next element.
		for {
			if len(stack) == 0 {
				return nil
			}
			c, err := j.peek()
			if err != nil {
				return err
			}
			top := stack[len(stack)-1]
			if c == ',' {
				_, _ = j.readByte()
				if top == '{' {
					if err := j.memberKey(); err != nil {
						return err
					}
				}
				break
			}
			if top == '{' && c == '}' || top == '[' && c == ']' {
				_, _ = j.readByte()
				stack = stack[:len(stack)-1]
				continue
			}
			return j.syntax("unexpected %q", c)
		}
	}
}

// memberKey consumes `"key":` and discards the key.
func (j *jsonReader) memberKey() error {
	if _, _, err := j.readString(0); err != nil {
		return err
	}
	return j.expect(':')
}

// isNull consumes a null if one is next.
func (j *jsonReader) isNull() (bool, error) {
	c, err := j.peek()
	if err != nil || c != 'n' {
		return false, err
	}
	_, err = j.readLiteral()
	return err == nil, err
}

// object reads an object, calling member for each key; member must consume
// the value. Keys longer than maxKey bytes (or not UTF-8) are passed with
// ok false.
func (j *jsonReader) object(maxKey int, member func(key string, ok bool) error) error {
	if err := j.expect('{'); err != nil {
		return err
	}
	c, err := j.peek()
	if err != nil {
		return err
	}
	if c == '}' {
		_, _ = j.readByte()
		return nil
	}
	for {
		key, ok, err := j.readString(maxKey)
		if err != nil {
			return err
		}
		if err := j.expect(':'); err != nil {
			return err
		}
		if err := member(key, ok); err != nil {
			return err
		}
		c, err := j.peek()
		if err != nil {
			return err
		}
		switch c {
		case ',':
			_, _ = j.readByte()
		case '}':
			_, _ = j.readByte()
			return nil
		default:
			return j.syntax("expected ',' or '}', found %q", c)
		}
	}
}

// array reads an array, calling elem for each element; elem must consume
// the element.
func (j *jsonReader) array(elem func() error) error {
	if err := j.expect('['); err != nil {
		return err
	}
	c, err := j.peek()
	if err != nil {
		return err
	}
	if c == ']' {
		_, _ = j.readByte()
		return nil
	}
	for {
		if err := elem(); err != nil {
			return err
		}
		c, err := j.peek()
		if err != nil {
			return err
		}
		switch c {
		case ',':
			_, _ = j.readByte()
		case ']':
			_, _ = j.readByte()
			return nil
		default:
			return j.syntax("expected ',' or ']', found %q", c)
		}
	}
}
