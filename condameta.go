package goomba

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/spec"
)

// fillFromIndex completes a locked record with the fields lockfiles do not
// carry, from the package's own info/index.json (already verified with the
// archive): timestamp, license, track_features and features. Repodata takes
// them from the same file, so the conda-meta record then matches the one
// micromamba writes after a solve. Absent or null fields are left empty; a
// field of the wrong type is an error.
func fillFromIndex(rec *record.PackageRecord, index map[string]json.RawMessage) error {
	bad := func(key string, raw json.RawMessage) error {
		return fmt.Errorf("%w: index.json %s %.64s is not usable", link.ErrInvalidPackage, key, raw)
	}
	isNull := func(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }
	if raw, ok := index["timestamp"]; ok && !isNull(raw) {
		ts, err := jsonInt(raw)
		if err != nil {
			return bad("timestamp", raw)
		}
		rec.Timestamp = record.NormalizeTimestamp(ts)
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"license", &rec.License}, {"features", &rec.Features}} {
		if raw, ok := index[f.key]; ok && !isNull(raw) {
			if err := json.Unmarshal(raw, f.dst); err != nil {
				return bad(f.key, raw)
			}
		}
	}
	if raw, ok := index["track_features"]; ok && !isNull(raw) {
		var list []string
		var s string
		switch {
		case json.Unmarshal(raw, &s) == nil:
			list = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
		case json.Unmarshal(raw, &list) == nil:
		default:
			return bad("track_features", raw)
		}
		rec.TrackFeatures = nil
		for _, f := range list {
			if f = strings.TrimSpace(f); f != "" {
				rec.TrackFeatures = append(rec.TrackFeatures, f)
			}
		}
	}
	return nil
}

// jsonInt reads a JSON number as an int64; a float must be integral and in
// range.
func jsonInt(raw json.RawMessage) (int64, error) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return i, nil
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<62 {
		return 0, fmt.Errorf("not an integer: %s", n)
	}
	return int64(f), nil
}

// userSpecs holds the lockfile's specs: parsed for matching, and in the
// canonical form libmamba records in conda-meta and history.
type userSpecs struct {
	parsed []spec.MatchSpec
	ok     []bool
	text   []string
}

// newUserSpecs parses specs. One that does not parse is kept verbatim for
// the history and matches no package.
func newUserSpecs(specs []string) userSpecs {
	u := userSpecs{parsed: make([]spec.MatchSpec, len(specs)), ok: make([]bool, len(specs)), text: make([]string, len(specs))}
	for i, s := range specs {
		u.text[i] = s
		if ms, err := spec.ParseMatchSpec(s); err == nil {
			u.parsed[i], u.ok[i] = ms, true
			u.text[i] = ms.String()
		}
	}
	return u
}

// requested returns the requested_spec libmamba records for a package: the
// last spec whose name matches it, in canonical form, or "".
func (u userSpecs) requested(name string) string {
	out := ""
	for i, ms := range u.parsed {
		if u.ok[i] && ms.Name.Contains(name) {
			out = u.text[i]
		}
	}
	return out
}
