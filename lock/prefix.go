package lock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/richinsley/goomba/record"
)

// maxPrefixRecord bounds one conda-meta/*.json file. Records list every
// file of a package, so large packages produce records of several MB.
const maxPrefixRecord = 128 << 20

// FromPrefix builds a lockfile from the conda-meta/*.json records of an
// installed environment. It is FromPrefixContext with
// context.Background().
func FromPrefix(prefix, platform string) (*Lockfile, error) {
	return FromPrefixContext(context.Background(), prefix, platform)
}

// FromPrefixContext builds a lockfile from the conda-meta/*.json records of
// the environment at prefix. platform is the environment's platform; ""
// infers it from the records' subdirs. Every record must carry an https URL
// and a sha256 (conda and micromamba record both); URLs are normalized as
// FromRecords does, the lockfile's channels are the records' channels in
// sorted order and its specs are the records' non-empty requested_spec
// values, sorted. Virtual packages are unknown and
// left empty. A record file must be named after the record
// (<name>-<version>-<build>.json) and be a regular file. The prefix is only
// read.
func FromPrefixContext(ctx context.Context, prefix, platform string) (*Lockfile, error) {
	dir := filepath.Join(prefix, "conda-meta")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("lock: read prefix %q: %w", prefix, err)
	}
	var recs []*record.PackageRecord
	specs := map[string]bool{}
	channels := map[string]bool{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || e.IsDir() {
			continue
		}
		if !e.Type().IsRegular() {
			// Never open a FIFO, device or link: opening a FIFO blocks.
			return nil, invalidf("conda-meta %q: not a regular file", name)
		}
		path := filepath.Join(dir, name)
		pr, err := readPrefixRecordFile(path)
		if err != nil {
			return nil, err
		}
		if want := pr.rec.Dist() + ".json"; name != want {
			return nil, invalidf("conda-meta %q: holds record %q", name, pr.rec.Dist())
		}
		if pr.requestedSpec != "" {
			specs[pr.requestedSpec] = true
		}
		if ch, _, _, err := splitArchiveURL(canonicalHost(pr.rec.URL)); err == nil {
			channels[ch] = true
		}
		recs = append(recs, pr.rec)
	}
	if platform == "" {
		platform, err = inferPlatform(recs)
		if err != nil {
			return nil, fmt.Errorf("lock: prefix %q: %w", prefix, err)
		}
	}
	l, err := FromRecords(platform, sortedKeys(channels), sortedKeys(specs), map[string]string{}, recs)
	if err != nil {
		return nil, fmt.Errorf("lock: prefix %q: %w", prefix, err)
	}
	return l, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inferPlatform returns the one non-noarch subdir among recs.
func inferPlatform(recs []*record.PackageRecord) (string, error) {
	platform := ""
	for _, r := range recs {
		switch {
		case r.Subdir == "noarch":
		case platform == "":
			platform = r.Subdir
		case r.Subdir != platform:
			return "", invalidf("records for both %q and %q", platform, r.Subdir)
		}
	}
	if platform == "" {
		return "", invalidf("cannot infer the platform: no platform-specific records")
	}
	return platform, nil
}

// prefixRecord is what goomba needs from a conda-meta record.
type prefixRecord struct {
	rec           *record.PackageRecord
	requestedSpec string
}

// readPrefixRecordFile reads one conda-meta record, which must be a regular
// file and not a symbolic link. openRegular never blocks on a FIFO and, on
// unix, never follows a link; afterwards the opened file must still be the
// file at path (os.SameFile on the Fstat and Lstat results), so swapping a
// link in while the record is read is caught on every OS.
func readPrefixRecordFile(path string) (prefixRecord, error) {
	name := filepath.Base(path)
	f, err := openRegular(path)
	if err != nil {
		return prefixRecord{}, err
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil {
		return prefixRecord{}, fmt.Errorf("lock: %w", err)
	}
	if !fst.Mode().IsRegular() {
		return prefixRecord{}, invalidf("conda-meta %q: not a regular file", name)
	}
	lst, err := os.Lstat(path)
	if err != nil || !lst.Mode().IsRegular() || !os.SameFile(fst, lst) {
		return prefixRecord{}, invalidf("conda-meta %q: replaced or linked while being read", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPrefixRecord+1))
	if err != nil {
		return prefixRecord{}, fmt.Errorf("lock: read %q: %w", path, err)
	}
	if len(data) > maxPrefixRecord {
		return prefixRecord{}, invalidf("conda-meta %q: larger than %d bytes", filepath.Base(path), maxPrefixRecord)
	}
	return parsePrefixRecord(data, "conda-meta "+strconv.Quote(filepath.Base(path)))
}

// parsePrefixRecord decodes a conda-meta record. Unlike Read it tolerates
// what conda and micromamba write: unknown keys (files, paths_data, link,
// ...) are skipped, null means absent, "build_string" stands in for a
// missing "build" (when both are present they must agree), and "noarch" may
// be a string, a boolean (true is "generic") or an object with a "type".
// Keys are still matched exactly and duplicates rejected, so the record
// cannot say two things at once.
func parsePrefixRecord(data []byte, what string) (prefixRecord, error) {
	d, err := newStrictDecoder(data)
	if err != nil {
		return prefixRecord{}, err
	}
	r := &record.PackageRecord{}
	var pr prefixRecord
	var build, buildString, fn string
	err = d.object(what, func(key string) error {
		at := what + "." + key
		switch key {
		case "name":
			return d.optStr(at, &r.Name)
		case "version":
			return d.optStr(at, &r.Version)
		case "build":
			return d.optStr(at, &build)
		case "build_string":
			return d.optStr(at, &buildString)
		case "build_number":
			return d.optUint64(at, &r.BuildNumber)
		case "subdir":
			return d.optStr(at, &r.Subdir)
		case "noarch":
			return d.noarch(at, &r.Noarch)
		case "url":
			return d.optStr(at, &r.URL)
		case "sha256":
			return d.optStr(at, &r.SHA256)
		case "md5":
			return d.optStr(at, &r.MD5)
		case "size":
			return d.optInt64(at, &r.Size)
		case "depends":
			return d.optStrings(at, &r.Depends)
		case "constrains":
			return d.optStrings(at, &r.Constrains)
		case "python_site_packages_path":
			return d.optStr(at, &r.PythonSitePackagesPath)
		case "fn":
			return d.optStr(at, &fn)
		case "requested_spec":
			return d.optStr(at, &pr.requestedSpec)
		}
		return d.skip(at)
	})
	if err != nil {
		return prefixRecord{}, err
	}
	if err := d.end(); err != nil {
		return prefixRecord{}, err
	}
	r.Build = build
	switch {
	case r.Build == "":
		r.Build = buildString
	case buildString != "" && buildString != build:
		return prefixRecord{}, invalidf("%s: build %q and build_string %q differ", what, build, buildString)
	}
	r.Fn = fn
	if r.Name == "" || r.Version == "" || r.Build == "" {
		return prefixRecord{}, invalidf("%s: name, version or build missing", what)
	}
	pr.rec = r
	return pr, nil
}

// skip consumes one value of any type.
func (d *strictDecoder) skip(what string) error {
	var raw json.RawMessage
	if err := d.dec.Decode(&raw); err != nil {
		return invalidf("%s: %v", what, err)
	}
	return nil
}

func (d *strictDecoder) optStr(what string, dst *string) error {
	tok, err := d.token(what)
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case nil:
		*dst = ""
	case string:
		*dst = t
	default:
		return invalidf("%s: expected a string, found %s", what, describe(tok))
	}
	return nil
}

func (d *strictDecoder) optNumber(what string) (json.Number, bool, error) {
	tok, err := d.token(what)
	if err != nil {
		return "", false, err
	}
	switch t := tok.(type) {
	case nil:
		return "", false, nil
	case json.Number:
		return t, true, nil
	}
	return "", false, invalidf("%s: expected a number, found %s", what, describe(tok))
}

func (d *strictDecoder) optUint64(what string, dst *uint64) error {
	n, ok, err := d.optNumber(what)
	if err != nil || !ok {
		return err
	}
	v, err := strconv.ParseUint(string(n), 10, 64)
	if err != nil {
		return invalidf("%s: %s is not a non-negative integer", what, n)
	}
	*dst = v
	return nil
}

func (d *strictDecoder) optInt64(what string, dst *int64) error {
	n, ok, err := d.optNumber(what)
	if err != nil || !ok {
		return err
	}
	v, err := n.Int64()
	if err != nil {
		return invalidf("%s: %s is not an integer", what, n)
	}
	*dst = v
	return nil
}

func (d *strictDecoder) optStrings(what string, dst *[]string) error {
	tok, err := d.token(what)
	if err != nil {
		return err
	}
	switch tok {
	case nil:
		*dst = []string{}
		return nil
	case json.Delim('['):
	default:
		return invalidf("%s: expected an array, found %s", what, describe(tok))
	}
	out := []string{}
	for i := 0; d.dec.More(); i++ {
		if i >= maxListItems {
			return invalidf("%s: more than %d elements", what, maxListItems)
		}
		var s string
		if err := d.str(fmt.Sprintf("%s[%d]", what, i), &s); err != nil {
			return err
		}
		out = append(out, s)
	}
	if tok, err := d.token(what); err != nil {
		return err
	} else if tok != json.Delim(']') {
		return invalidf("%s: expected ']', found %s", what, describe(tok))
	}
	*dst = out
	return nil
}

// noarch reads conda's noarch field in any of its historical forms.
func (d *strictDecoder) noarch(what string, dst *string) error {
	tok, err := d.token(what)
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case nil:
		*dst = ""
	case bool:
		*dst = ""
		if t {
			*dst = "generic"
		}
	case string:
		*dst = t
	case json.Delim:
		if t != '{' {
			return invalidf("%s: expected a string or object, found %s", what, describe(tok))
		}
		*dst = ""
		return d.objectBody(what, func(key string) error {
			if key == "type" {
				return d.optStr(what+".type", dst)
			}
			return d.skip(what + "." + key)
		})
	default:
		return invalidf("%s: expected a string, found %s", what, describe(tok))
	}
	return nil
}
