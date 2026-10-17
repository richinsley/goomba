package spec

import (
	"errors"
	"strconv"
)

// Sentinel errors identifying what kind of input failed to parse. Every
// parse error returned by this package is a *ParseError whose Is method
// matches exactly one of these, so callers can branch with errors.Is.
// A *ParseError that wraps another *ParseError (for example a match spec
// whose version spec is invalid) matches both kinds.
var (
	// ErrVersion reports an invalid version, e.g. "1..2".
	ErrVersion = errors.New("invalid version")
	// ErrVersionSpec reports an invalid version spec, e.g. ">=1,,2".
	ErrVersionSpec = errors.New("invalid version spec")
	// ErrBuildNumberSpec reports an invalid build number spec, e.g. "<2.4".
	ErrBuildNumberSpec = errors.New("invalid build number spec")
	// ErrStringMatcher reports an invalid build string matcher, i.e. a
	// pattern that looks like a regular expression but does not compile.
	ErrStringMatcher = errors.New("invalid string matcher")
	// ErrMatchSpec reports an invalid match spec, e.g. ">=1.0" (no name).
	ErrMatchSpec = errors.New("invalid match spec")
	// ErrChannel reports an invalid or unsupported channel, e.g. a local
	// path or a URL carrying credentials.
	ErrChannel = errors.New("invalid channel")
)

// ParseError describes an input string that could not be parsed.
//
// ParseError values are immutable and safe for concurrent use.
type ParseError struct {
	// Kind is one of the package's sentinel errors (ErrVersion, ...).
	Kind error
	// Input is the string that failed to parse, as given.
	Input string
	// Msg describes the problem, e.g. "empty part".
	Msg string
	// Err is the underlying cause, often another *ParseError; may be nil.
	Err error
}

// Error returns a message of the form
// `spec: parse version "1..2": empty part`.
func (e *ParseError) Error() string {
	return "spec: " + e.detail()
}

func (e *ParseError) detail() string {
	s := "parse " + kindName(e.Kind) + " " + strconv.Quote(e.Input)
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	if e.Err != nil {
		var inner *ParseError
		if errors.As(e.Err, &inner) {
			s += ": " + inner.detail()
		} else {
			s += ": " + e.Err.Error()
		}
	}
	return s
}

// Unwrap returns the underlying cause.
func (e *ParseError) Unwrap() error { return e.Err }

// Is reports whether target is the sentinel naming this error's kind.
func (e *ParseError) Is(target error) bool { return target == e.Kind }

func kindName(kind error) string {
	switch kind {
	case ErrVersion:
		return "version"
	case ErrVersionSpec:
		return "version spec"
	case ErrBuildNumberSpec:
		return "build number spec"
	case ErrStringMatcher:
		return "string matcher"
	case ErrMatchSpec:
		return "match spec"
	case ErrChannel:
		return "channel"
	}
	return "input"
}

func parseErr(kind error, input, msg string) *ParseError {
	return &ParseError{Kind: kind, Input: input, Msg: msg}
}

func wrapErr(kind error, input, msg string, err error) *ParseError {
	return &ParseError{Kind: kind, Input: input, Msg: msg, Err: err}
}
