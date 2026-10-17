package solve

import (
	"errors"
	"fmt"
	"strings"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/spec"
)

// checkAlias checks a Request's channel alias up front, so a bad alias is
// reported as such rather than as specs that do not resolve.
func checkAlias(alias string) error {
	if _, err := spec.ParseChannelAlias("conda-forge", alias); err != nil {
		return fmt.Errorf("%w: channel alias %q: %w", ErrInvalidRequest, alias, err)
	}
	return nil
}

// parsedSpec is a match spec ready for the solver: one exact package name,
// its channel resolved to a canonical URL. It is plain data.
type parsedSpec struct {
	ms spec.MatchSpec
	// name is the exact, lower-case package name.
	name string
	// channel is the canonical channel URL the spec requires, or "".
	channel string
}

// errUnsupported marks dependency forms goomba does not support yet
// (conditional dependencies and extras), so their exclusion reason says so
// instead of calling them unparsable.
type errUnsupported struct{ what string }

func (e errUnsupported) Error() string { return e.what + " are not supported" }

// parseSpec parses s for the solver, resolving a channel with alias. It
// rejects specs whose name is a pattern rather than one package, and the
// conditional ("; if") and extras forms conda's newest repodata may use.
func parseSpec(s, alias string) (parsedSpec, error) {
	ms, err := spec.ParseMatchSpec(s)
	if err != nil {
		if u, ok := unsupportedForm(s, err); ok {
			return parsedSpec{}, u
		}
		return parsedSpec{}, err
	}
	if !ms.Name.IsExact() {
		return parsedSpec{}, fmt.Errorf("the name %q is a pattern, not a package name", ms.Name.String())
	}
	ch, err := ms.ChannelURL(alias)
	if err != nil {
		return parsedSpec{}, err
	}
	return parsedSpec{ms: ms, name: ms.Name.String(), channel: ch}, nil
}

// unsupportedForm recognises a spec that failed to parse (with err)
// because it uses a form goomba does not support yet:
//
//   - a conditional dependency "<spec>; if <condition>", recognised only
//     when the part before ";" parses as a match spec and a condition
//     follows "if";
//   - the bracket keys when= (a conditional dependency) and extras=, which
//     package spec rejects as unknown keys.
//
// Anything else that fails to parse is reported as unparsable.
func unsupportedForm(s string, err error) (errUnsupported, bool) {
	if before, after, ok := strings.Cut(s, ";"); ok {
		cond, isIf := strings.CutPrefix(strings.TrimLeft(after, " "), "if ")
		if isIf && strings.TrimSpace(cond) != "" {
			if _, perr := spec.ParseMatchSpec(before); perr == nil {
				return errUnsupported{"conditional dependencies"}, true
			}
		}
		return errUnsupported{}, false
	}
	var pe *spec.ParseError
	if errors.As(err, &pe) {
		switch pe.Msg {
		case `unknown key "when"`:
			return errUnsupported{"conditional dependencies"}, true
		case `unknown key "extras"`:
			return errUnsupported{"extras"}, true
		}
	}
	return errUnsupported{}, false
}

// exclusionForDependency returns the reason a record whose dependency or
// constraint dep fails to parse is excluded. The dependency is quoted once
// (strconv.Quote, which also escapes control characters); the parse
// error's own copy of it is left out.
func exclusionForDependency(dep string, err error) string {
	if u, ok := err.(errUnsupported); ok {
		return fmt.Sprintf("its dependency %q uses %s, which goomba does not support yet", dep, u.what)
	}
	return fmt.Sprintf("unparsable dependency %q: %s", dep, parseErrorDetail(dep, err))
}

// parseErrorDetail describes err, a failure to parse input, without
// repeating input when err is a *spec.ParseError that quotes it.
func parseErrorDetail(input string, err error) string {
	var pe *spec.ParseError
	if errors.As(err, &pe) && pe.Input == input {
		var parts []string
		if pe.Msg != "" {
			parts = append(parts, pe.Msg)
		}
		if pe.Err != nil {
			parts = append(parts, pe.Err.Error())
		}
		if len(parts) > 0 {
			return strings.Join(parts, ": ")
		}
	}
	return err.Error()
}

// recordMatches reports whether a record satisfies a parsed spec under
// conda's rules: a virtual package (from the target machine) is matched
// on its version and build string only, as rattler does; any other record
// on every field spec.MatchSpec checks, its channel when the spec names
// one, and its URL when the spec is a package URL. A channel-qualified
// spec never matches a virtual package. version is rec.Version parsed.
func recordMatches(ps *parsedSpec, rec *record.PackageRecord, version spec.Version, virtual bool) bool {
	if virtual {
		return ps.channel == "" &&
			ps.ms.Name.Contains(rec.Name) &&
			ps.ms.Version.Contains(version) &&
			ps.ms.Build.Contains(rec.Build)
	}
	if ps.channel != "" && rec.Channel != ps.channel {
		return false
	}
	if ps.ms.URL != "" && rec.URL != ps.ms.URL {
		return false
	}
	return ps.ms.MatchesVersion(rec, version)
}

// parseRecordVersion parses a record's version; conda treats an empty
// version as "0".
func parseRecordVersion(rec *record.PackageRecord) (spec.Version, error) {
	v := rec.Version
	if v == "" {
		v = "0"
	}
	return spec.ParseVersion(v)
}

// isPackageName reports whether name is a normalized conda package name:
// non-empty, at most 256 bytes, lower-case letters, digits, '.', '_' and
// '-' only.
func isPackageName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// sameRecord reports whether a and b describe the same package file.
func sameRecord(a, b *record.PackageRecord) bool {
	return a == b || (a.Name == b.Name && a.Version == b.Version && a.Build == b.Build &&
		a.BuildNumber == b.BuildNumber && a.Channel == b.Channel && a.Subdir == b.Subdir &&
		a.Fn == b.Fn && a.URL == b.URL && strings.EqualFold(a.SHA256, b.SHA256))
}
