package link

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf8"
)

// jsonObject is a JSON object whose keys are written in byte order, like
// nlohmann::json's std::map-backed objects.
type jsonObject map[string]any

// marshalNlohmann renders v the way nlohmann::json::dump(4) does, which is
// how micromamba writes conda-meta records: four-space indentation, ": "
// after keys, sorted keys, "[]" and "{}" for empty containers, UTF-8 left
// unescaped, control characters as lowercase \u00xx, no trailing newline.
// Invalid UTF-8 is an error, as with nlohmann's strict error handler.
func marshalNlohmann(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeNlohmann(&buf, v, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeIndent(buf *bytes.Buffer, n int) {
	for i := 0; i < n; i++ {
		buf.WriteByte(' ')
	}
}

func writeNlohmann(buf *bytes.Buffer, v any, indent int) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case string:
		return writeNlohmannString(buf, x)
	case int:
		buf.WriteString(strconv.Itoa(x))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
	case json.Number:
		if _, err := strconv.ParseFloat(string(x), 64); err != nil {
			return fmt.Errorf("invalid JSON number %q", string(x))
		}
		buf.WriteString(string(x))
	case []string:
		arr := make([]any, len(x))
		for i, s := range x {
			arr[i] = s
		}
		return writeNlohmann(buf, arr, indent)
	case []any:
		if len(x) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteString("[\n")
		for i, e := range x {
			writeIndent(buf, indent+4)
			if err := writeNlohmann(buf, e, indent+4); err != nil {
				return err
			}
			if i < len(x)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		writeIndent(buf, indent)
		buf.WriteByte(']')
	case map[string]any:
		return writeNlohmann(buf, jsonObject(x), indent)
	case jsonObject:
		if len(x) == 0 {
			buf.WriteString("{}")
			return nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		buf.WriteString("{\n")
		for i, k := range keys {
			writeIndent(buf, indent+4)
			if err := writeNlohmannString(buf, k); err != nil {
				return err
			}
			buf.WriteString(": ")
			if err := writeNlohmann(buf, x[k], indent+4); err != nil {
				return err
			}
			if i < len(keys)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		writeIndent(buf, indent)
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value of type %T", v)
	}
	return nil
}

func writeNlohmannString(buf *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("invalid UTF-8 in JSON string %q", s)
	}
	const hexDigits = "0123456789abcdef"
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if c < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[c>>4])
				buf.WriteByte(hexDigits[c&0xf])
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
	return nil
}

// decodeJSONValue decodes raw into the generic values writeNlohmann accepts,
// keeping numbers as json.Number.
func decodeJSONValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
