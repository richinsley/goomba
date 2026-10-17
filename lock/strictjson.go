package lock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"
)

// strictDecoder walks a JSON document token by token so that every key is
// matched exactly (encoding/json matches struct fields case-insensitively),
// duplicate keys and nulls are rejected, and nothing follows the top-level
// value.
type strictDecoder struct {
	dec *json.Decoder
}

func newStrictDecoder(data []byte) (*strictDecoder, error) {
	if !utf8.Valid(data) {
		return nil, invalidf("input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return &strictDecoder{dec: dec}, nil
}

func describe(tok json.Token) string {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return "object"
		case '[':
			return "array"
		}
		return "'" + t.String() + "'"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", tok)
}

// token reads the next token, turning io.EOF and syntax errors into
// ErrInvalid errors that name what was being read.
func (d *strictDecoder) token(what string) (json.Token, error) {
	tok, err := d.dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, invalidf("%s: unexpected end of input", what)
		}
		return nil, invalidf("%s: %v", what, err)
	}
	return tok, nil
}

// object reads an object, calling field for each key; field must consume
// the value. Duplicate keys are errors.
func (d *strictDecoder) object(what string, field func(key string) error) error {
	tok, err := d.token(what)
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return invalidf("%s: expected an object, found %s", what, describe(tok))
	}
	return d.objectBody(what, field)
}

// objectBody reads the rest of an object whose '{' has been consumed.
func (d *strictDecoder) objectBody(what string, field func(key string) error) error {
	seen := map[string]bool{}
	for d.dec.More() {
		tok, err := d.token(what)
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return invalidf("%s: expected a key, found %s", what, describe(tok))
		}
		if seen[key] {
			return invalidf("%s: duplicate key %q", what, key)
		}
		seen[key] = true
		if err := field(key); err != nil {
			return err
		}
	}
	if tok, err := d.token(what); err != nil {
		return err
	} else if tok != json.Delim('}') {
		return invalidf("%s: expected '}', found %s", what, describe(tok))
	}
	return nil
}

// array reads an array, calling elem for each element; elem must consume
// the element.
func (d *strictDecoder) array(what string, elem func(i int) error) error {
	tok, err := d.token(what)
	if err != nil {
		return err
	}
	if tok != json.Delim('[') {
		return invalidf("%s: expected an array, found %s", what, describe(tok))
	}
	for i := 0; d.dec.More(); i++ {
		if i >= maxListItems {
			return invalidf("%s: more than %d elements", what, maxListItems)
		}
		if err := elem(i); err != nil {
			return err
		}
	}
	if tok, err := d.token(what); err != nil {
		return err
	} else if tok != json.Delim(']') {
		return invalidf("%s: expected ']', found %s", what, describe(tok))
	}
	return nil
}

func (d *strictDecoder) str(what string, dst *string) error {
	tok, err := d.token(what)
	if err != nil {
		return err
	}
	s, ok := tok.(string)
	if !ok {
		return invalidf("%s: expected a string, found %s", what, describe(tok))
	}
	*dst = s
	return nil
}

func (d *strictDecoder) number(what string) (json.Number, error) {
	tok, err := d.token(what)
	if err != nil {
		return "", err
	}
	n, ok := tok.(json.Number)
	if !ok {
		return "", invalidf("%s: expected a number, found %s", what, describe(tok))
	}
	return n, nil
}

func (d *strictDecoder) uint64(what string, dst *uint64) error {
	n, err := d.number(what)
	if err != nil {
		return err
	}
	v, err := strconv.ParseUint(string(n), 10, 64)
	if err != nil {
		return invalidf("%s: %s is not a non-negative integer", what, n)
	}
	*dst = v
	return nil
}

func (d *strictDecoder) int64(what string, dst *int64) error {
	n, err := d.number(what)
	if err != nil {
		return err
	}
	v, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		return invalidf("%s: %s is not an integer", what, n)
	}
	*dst = v
	return nil
}

func (d *strictDecoder) int(what string, dst *int) error {
	var v int64
	if err := d.int64(what, &v); err != nil {
		return err
	}
	if v < math.MinInt32 || v > math.MaxInt32 {
		return invalidf("%s: %d is out of range", what, v)
	}
	*dst = int(v)
	return nil
}

// strings reads an array of strings into a non-nil slice.
func (d *strictDecoder) strings(what string, dst *[]string) error {
	out := []string{}
	err := d.array(what, func(i int) error {
		var s string
		if err := d.str(fmt.Sprintf("%s[%d]", what, i), &s); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return err
	}
	*dst = out
	return nil
}

// stringMap reads an object whose values are strings into a non-nil map.
func (d *strictDecoder) stringMap(what string, dst *map[string]string) error {
	out := map[string]string{}
	err := d.object(what, func(key string) error {
		if len(out) >= maxListItems {
			return invalidf("%s: more than %d keys", what, maxListItems)
		}
		var s string
		if err := d.str(fmt.Sprintf("%s[%q]", what, key), &s); err != nil {
			return err
		}
		out[key] = s
		return nil
	})
	if err != nil {
		return err
	}
	*dst = out
	return nil
}

// end checks that only whitespace follows the top-level value.
func (d *strictDecoder) end() error {
	if _, err := d.dec.Token(); !errors.Is(err, io.EOF) {
		return invalidf("trailing data after the top-level value")
	}
	return nil
}

// decodeLockfile reads one lockfile object. what names it in errors.
func decodeLockfile(d *strictDecoder, what string) (*Lockfile, error) {
	l := &Lockfile{}
	err := d.object(what, func(key string) error {
		at := what + "." + key
		switch key {
		case "version":
			return d.int(at, &l.Version)
		case "platform":
			return d.str(at, &l.Platform)
		case "channels":
			return d.strings(at, &l.Channels)
		case "specs":
			return d.strings(at, &l.Specs)
		case "virtual":
			return d.stringMap(at, &l.Virtual)
		case "packages":
			l.Packages = []Package{}
			return d.array(at, func(i int) error {
				p, err := decodePackage(d, fmt.Sprintf("%s[%d]", at, i))
				if err != nil {
					return err
				}
				l.Packages = append(l.Packages, p)
				return nil
			})
		}
		return invalidf("%s: unknown key %q", what, key)
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

func decodePackage(d *strictDecoder, what string) (Package, error) {
	var p Package
	err := d.object(what, func(key string) error {
		at := what + "." + key
		switch key {
		case "name":
			return d.str(at, &p.Name)
		case "version":
			return d.str(at, &p.Version)
		case "build":
			return d.str(at, &p.Build)
		case "build_number":
			return d.uint64(at, &p.BuildNumber)
		case "subdir":
			return d.str(at, &p.Subdir)
		case "noarch":
			return d.str(at, &p.Noarch)
		case "url":
			return d.str(at, &p.URL)
		case "sha256":
			return d.str(at, &p.SHA256)
		case "md5":
			return d.str(at, &p.MD5)
		case "size":
			return d.int64(at, &p.Size)
		case "depends":
			return d.strings(at, &p.Depends)
		case "constrains":
			return d.strings(at, &p.Constrains)
		case "python_site_packages_path":
			return d.str(at, &p.PythonSitePackagesPath)
		}
		return invalidf("%s: unknown key %q", what, key)
	})
	return p, err
}
