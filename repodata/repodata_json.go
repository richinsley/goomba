// Ported from libmamba src/specs/repo_data.cpp (BSD-3-Clause, see NOTICE),
// cross-checked with rattler crates/rattler_conda_types/src/repo_data.

package repodata

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/richinsley/goomba/record"
)

// LoadRepodataJSON parses an uncompressed repodata.json for one subdir of a
// channel and returns its records, normalized:
//
//   - Fn, URL, Channel (channelURL without trailing slash) and Subdir are
//     filled; URL uses info.base_url (CEP 15) when present, resolved against
//     <channelURL>/<subdir>/repodata.json, else the subdir URL.
//   - track_features becomes a list without empty entries; noarch true
//     becomes "generic"; timestamps are in milliseconds; hashes are
//     lowercase hex. Empty lists (depends, constrains, track_features) are
//     nil, from either index format.
//   - Records listed in "removed" are dropped, and a .tar.bz2 record is
//     dropped when the same name-version-build exists as .conda.
//   - Malformed records (missing name, version or build, wrong field types,
//     bad hashes, a file name other than name-version-build plus the
//     extension of its section, values over the size caps) are dropped. A
//     malformed document, a repeated top-level key or decoded data far
//     larger than the input is an error.
//
// subdir may be empty, in which case info.subdir is used; when both are set
// they must agree. The input is read as a stream. Version strings are not
// parsed. channelURL must pass the same checks as NewShardedChannel's.
func LoadRepodataJSON(r io.Reader, channelURL, subdir string) ([]*record.PackageRecord, error) {
	ch, pol, err := canonicalChannel(channelURL)
	if err != nil {
		return nil, err
	}
	recs, _, err := loadRepodataJSON(r, ch, pol, subdir, nil)
	return recs, err
}

// pendingSet holds decoded records by file name until they can be placed
// (the subdir and base URL come from "info", which may follow "packages").
// A nil value marks a file name whose last occurrence was invalid: like
// Python's json, a repeated key means its last value, and an invalid last
// value drops the record (fail closed). Each index section has its own set.
type pendingSet map[string]*record.PackageRecord

// place locates the valid records, charging each record's URL to b.
func (p pendingSet) place(loc *locator, out []*record.PackageRecord, b *budget) ([]*record.PackageRecord, error) {
	for fn, r := range p {
		if r != nil {
			loc.place(r, fn)
			if err := b.charge(costString + sized(len(r.URL))); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// countingReader counts the bytes read, for the decode budget.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// loadRepodataJSON parses a repodata.json. It also returns every package
// name the document lists, before any filtering (see NameLister).
func loadRepodataJSON(r io.Reader, ch *url.URL, pol urlPolicy, subdir string, py *pythonMinor) ([]*record.PackageRecord, map[string]struct{}, error) {
	return loadRepodataJSONBudget(r, ch, pol, subdir, py, nil)
}

// loadRepodataJSONBudget is loadRepodataJSON with the budget made by mk
// (nil: base jsonBudgetBase plus jsonBudgetRatio per input byte).
func loadRepodataJSONBudget(r io.Reader, ch *url.URL, pol urlPolicy, subdir string, py *pythonMinor, mk func(input func() int64) *budget) ([]*record.PackageRecord, map[string]struct{}, error) {
	if subdir != "" && checkPlatform(subdir) != nil {
		return nil, nil, fmt.Errorf("repodata: invalid subdir %q", subdir)
	}
	fail := func(err error) ([]*record.PackageRecord, map[string]struct{}, error) {
		return nil, nil, fmt.Errorf("repodata: parse repodata.json for %s/%s: %w", redact(ch), subdir, err)
	}
	cr := &countingReader{r: r}
	j := newJSONReader(cr)
	d := &jsonIndexDecoder{
		j:       j,
		py:      py,
		in:      interner{},
		names:   map[string]struct{}{},
		removed: map[string]struct{}{},
		tarBz2:  pendingSet{},
		conda:   pendingSet{},
	}
	input := func() int64 { return cr.n }
	if mk == nil {
		d.budget = &budget{base: jsonBudgetBase, ratio: jsonBudgetRatio, input: input}
	} else {
		d.budget = mk(input)
	}
	var infoSubdir, baseURL string
	seen := map[string]bool{}
	err := j.object(maxKeyBytes, func(key string, ok bool) error {
		switch key {
		case "info", "packages", "packages.conda", "removed":
			if seen[key] {
				return fmt.Errorf("repeated top-level key %q", key)
			}
			seen[key] = true
		}
		var err error
		switch {
		case !ok:
			err = j.skipValue()
		case key == "info":
			infoSubdir, baseURL, err = d.info()
		case key == "packages":
			err = d.packages(d.tarBz2, false)
		case key == "packages.conda":
			err = d.packages(d.conda, true)
		case key == "removed":
			err = d.removedList()
		default:
			err = j.skipValue()
		}
		if err != nil {
			return fmt.Errorf("%q: %w", key, err)
		}
		return nil
	})
	if err == nil {
		err = j.end()
	}
	if err != nil {
		return fail(err)
	}

	switch {
	case subdir == "" && infoSubdir == "":
		return fail(errors.New("no subdir given and info.subdir is missing"))
	case subdir == "":
		if checkPlatform(infoSubdir) != nil {
			return fail(fmt.Errorf("invalid info.subdir %q", infoSubdir))
		}
		subdir = infoSubdir
	case infoSubdir != "" && infoSubdir != subdir:
		return fail(fmt.Errorf("info.subdir %q does not match subdir %q", infoSubdir, subdir))
	}
	indexURL := subdirURL(ch, subdir).JoinPath("repodata.json")
	base, err := resolveBase(indexURL, baseURL, pol)
	if err != nil {
		return fail(fmt.Errorf("info.base_url: %w", err))
	}
	loc := &locator{channel: ch.String(), subdir: subdir, base: base}
	recs, err := d.tarBz2.place(loc, make([]*record.PackageRecord, 0, len(d.tarBz2)+len(d.conda)), d.budget)
	if err == nil {
		recs, err = d.conda.place(loc, recs, d.budget)
	}
	if err != nil {
		return fail(err)
	}
	for fn := range d.removed {
		if n := nameFromFilename(fn); n != "" {
			d.names[n] = struct{}{}
		}
	}
	return finalize(recs, d.removed), d.names, nil
}

// jsonIndexDecoder holds the state of one repodata.json parse.
type jsonIndexDecoder struct {
	j       *jsonReader
	py      *pythonMinor
	in      interner
	names   map[string]struct{} // every name listed, valid record or not
	removed map[string]struct{}
	tarBz2  pendingSet
	conda   pendingSet
	budget  *budget
}

// info reads the "info" object. Only subdir and base_url matter; both must
// be strings (or null) when present.
func (d *jsonIndexDecoder) info() (subdir, baseURL string, err error) {
	if null, err := d.j.isNull(); null || err != nil {
		return "", "", err
	}
	keys := 0
	err = d.j.object(maxKeyBytes, func(key string, ok bool) error {
		if keys++; keys > maxRecordKeys {
			return errors.New("too many keys")
		}
		if !ok || (key != "subdir" && key != "base_url") {
			return d.j.skipValue()
		}
		if null, err := d.j.isNull(); null || err != nil {
			return err
		}
		max := maxFieldBytes
		if key == "base_url" {
			max = maxBaseURL
		}
		s, good, err := d.j.readString(max)
		if err != nil {
			return err
		}
		if !good {
			return fmt.Errorf("info.%s: longer than %d bytes or not UTF-8", key, max)
		}
		if key == "subdir" {
			subdir = s
		} else {
			baseURL = s
		}
		return nil
	})
	return subdir, baseURL, err
}

// removedList streams the "removed" array into the set.
func (d *jsonIndexDecoder) removedList() error {
	if null, err := d.j.isNull(); null || err != nil {
		return err
	}
	n := 0
	return d.j.array(func() error {
		if n++; n > maxRemoved {
			return fmt.Errorf("more than %d entries", maxRemoved)
		}
		s, ok, err := d.j.readString(maxFieldBytes)
		if err != nil {
			return err
		}
		if !ok {
			return nil // cannot name a valid file
		}
		if _, dup := d.removed[s]; !dup {
			if err := d.budget.charge(costSetEntry + sized(len(s))); err != nil {
				return err
			}
			d.removed[s] = struct{}{}
		}
		return nil
	})
}

// packages reads a "packages" or "packages.conda" object into set.
func (d *jsonIndexDecoder) packages(set pendingSet, conda bool) error {
	if null, err := d.j.isNull(); null || err != nil {
		return err
	}
	return d.j.object(maxFieldBytes, func(fn string, fnOK bool) error {
		rec, ok, err := d.record()
		if err != nil {
			return err
		}
		// List the name both from the record and from the file name, so
		// that strict channel priority holds whatever the record says.
		if rec != nil && validIdent(rec.Name) {
			if err := d.list(strings.ToLower(rec.Name)); err != nil {
				return err
			}
		}
		if !fnOK {
			return nil
		}
		if err := d.list(nameFromFilename(fn)); err != nil {
			return err
		}
		if _, dup := set[fn]; !dup {
			if err := d.budget.charge(costSetEntry + sized(len(fn))); err != nil {
				return err
			}
		}
		set[fn] = nil
		if ok && validFilename(fn, conda) && fn == rec.Dist()+ext(conda) && d.py.allows(rec) {
			if err := d.budget.charge(d.in.keep(rec)); err != nil {
				return err
			}
			set[fn] = rec
		}
		return nil
	})
}

// list records a package name the document lists (see NameLister).
func (d *jsonIndexDecoder) list(name string) error {
	if _, dup := d.names[name]; name == "" || dup {
		return nil
	}
	if err := d.budget.charge(costSetEntry + sized(len(name))); err != nil {
		return err
	}
	d.names[name] = struct{}{}
	return nil
}

func ext(conda bool) string {
	if conda {
		return extConda
	}
	return extTarBz2
}

// record reads one package record. ok is false for a record to drop (rec
// may still carry the name, for NameLister); err is set only when the
// document itself is broken. Unlike libmamba, which ignores a field it
// cannot read, any present field of the wrong type or over a size cap drops
// the record: silently losing a dependency or a hash would not fail closed.
func (d *jsonIndexDecoder) record() (*record.PackageRecord, bool, error) {
	j := d.j
	c, err := j.peek()
	if err != nil {
		return nil, false, err
	}
	if c != '{' {
		return nil, false, j.skipValue()
	}
	r := &record.PackageRecord{}
	ok := true
	keys := 0
	err = j.object(maxKeyBytes, func(key string, keyOK bool) error {
		if keys++; keys > maxRecordKeys {
			ok = false
		}
		if !keyOK || !ok {
			return j.skipValue()
		}
		good, err := d.field(key, r)
		ok = ok && good
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if !ok || !checkRecord(r) {
		return r, false, nil
	}
	return r, true, nil
}

// field reads the value of one record key into r. It reports false for a
// value of the wrong type or over a cap; the value is always consumed.
func (d *jsonIndexDecoder) field(key string, r *record.PackageRecord) (bool, error) {
	j := d.j
	var dst *string
	switch key {
	case "name":
		dst = &r.Name
	case "version":
		dst = &r.Version
	case "build":
		dst = &r.Build
	case "features":
		dst = &r.Features
	case "license":
		dst = &r.License
	case "python_site_packages_path":
		dst = &r.PythonSitePackagesPath
	case "md5", "sha256", "noarch", "track_features",
		"depends", "constrains", "build_number", "size", "timestamp":
	default:
		return true, j.skipValue() // unknown or unused field
	}
	if null, err := j.isNull(); null || err != nil {
		clearField(key, r) // null means absent (also after an earlier value)
		return true, err
	}
	c, err := j.peek()
	if err != nil {
		return false, err
	}
	switch key {
	case "md5", "sha256":
		if c != '"' {
			return false, j.skipValue()
		}
		s, ok, err := j.readString(maxFieldBytes)
		if err != nil || !ok || s == "" {
			return ok, err // "" means absent
		}
		if key == "md5" {
			r.MD5, ok = normalizeHex(s, 16)
		} else {
			r.SHA256, ok = normalizeHex(s, 32)
		}
		return ok, nil
	case "noarch":
		switch c {
		case 't', 'f':
			lit, err := j.readLiteral()
			if lit == "true" {
				r.Noarch = "generic" // legacy noarch: true
			}
			return true, err
		case '"':
			s, ok, err := j.readString(maxFieldBytes)
			if err != nil || !ok {
				return false, err
			}
			r.Noarch, ok = noarchString(s)
			return ok, nil
		}
		return false, j.skipValue()
	case "track_features":
		if c == '"' {
			s, ok, err := j.readString(maxFieldBytes)
			r.TrackFeatures = splitTrackFeatures(s)
			return ok, err
		}
		l, ok, err := d.stringList(c)
		r.TrackFeatures = cleanTrackFeatures(l)
		return ok, err
	case "depends", "constrains":
		l, ok, err := d.stringList(c)
		if key == "depends" {
			r.Depends = l
		} else {
			r.Constrains = l
		}
		return ok, err
	case "build_number", "size", "timestamp":
		if c != '-' && (c < '0' || c > '9') {
			return false, j.skipValue()
		}
		lit, err := j.readNumber()
		if err != nil {
			return false, err
		}
		n, ok := jsonUint(lit)
		switch key {
		case "build_number":
			r.BuildNumber = n
		case "size":
			r.Size = int64(n)
		default:
			r.Timestamp = record.NormalizeTimestamp(int64(n))
		}
		return ok && (key == "build_number" || n <= math.MaxInt64), nil
	}
	// The plain string fields.
	if c != '"' {
		return false, j.skipValue()
	}
	s, ok, err := j.readString(maxFieldBytes)
	*dst = s
	if ok && key == "python_site_packages_path" && s != "" {
		ok = validSitePackagesPath(s)
	}
	return ok, err
}

// clearField resets a record field to its absent value.
func clearField(key string, r *record.PackageRecord) {
	switch key {
	case "name":
		r.Name = ""
	case "version":
		r.Version = ""
	case "build":
		r.Build = ""
	case "features":
		r.Features = ""
	case "license":
		r.License = ""
	case "python_site_packages_path":
		r.PythonSitePackagesPath = ""
	case "md5":
		r.MD5 = ""
	case "sha256":
		r.SHA256 = ""
	case "noarch":
		r.Noarch = ""
	case "track_features":
		r.TrackFeatures = nil
	case "depends":
		r.Depends = nil
	case "constrains":
		r.Constrains = nil
	case "build_number":
		r.BuildNumber = 0
	case "size":
		r.Size = 0
	case "timestamp":
		r.Timestamp = 0
	}
}

// stringList reads an array of strings with at most maxListEntries entries
// of at most maxFieldBytes each. ok is false otherwise (all consumed).
func (d *jsonIndexDecoder) stringList(first byte) ([]string, bool, error) {
	if first != '[' {
		return nil, false, d.j.skipValue()
	}
	var out []string
	ok := true
	err := d.j.array(func() error {
		c, err := d.j.peek()
		if err != nil {
			return err
		}
		if c != '"' || !ok || len(out) >= maxListEntries {
			ok = false
			return d.j.skipValue()
		}
		s, good, err := d.j.readString(maxFieldBytes)
		if err != nil {
			return err
		}
		if !good {
			ok = false
			return nil
		}
		out = append(out, s)
		return nil
	})
	if !ok {
		out = nil
	}
	return out, ok, err
}

// jsonUint accepts a non-negative integer literal, also written as an
// integral float ("1.6e12"), as some old indexes do for timestamps.
func jsonUint(lit string) (uint64, bool) {
	if v, err := strconv.ParseUint(lit, 10, 64); err == nil {
		return v, true
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil || f < 0 || f != math.Trunc(f) || f >= 1<<63 {
		return 0, false
	}
	return uint64(f), true
}
