package link

import (
	"errors"
	"fmt"
)

// Sentinel errors returned (wrapped) by this package. Test for them with
// errors.Is.
var (
	// ErrLinkScript reports a package entry named like a pre-link,
	// post-link, pre-unlink or post-unlink script (".<name>-<action>.sh" or
	// ".bat", compared after Unicode case folding) anywhere in the prefix,
	// except an allowed package's own script as a file directly in bin or
	// Scripts; or a non-ASCII name starting with "." in bin or Scripts.
	ErrLinkScript = errors.New("package contains a link script")

	// ErrExists reports a destination that already exists in the prefix.
	// Link never overwrites; the transaction layer decides about clobbering.
	// Link's error is an *ExistsError naming the destination.
	ErrExists = errors.New("destination exists")

	// ErrUnsafePath reports a path that is absolute, contains "..", control
	// characters or a reserved component, or that would resolve outside the
	// prefix or the package directory, including symlink targets that escape.
	ErrUnsafePath = errors.New("unsafe path")

	// ErrPrefixTooLong reports an install prefix longer than a binary file's
	// prefix placeholder; binary replacement must keep the file length.
	ErrPrefixTooLong = errors.New("install prefix is longer than the prefix placeholder")

	// ErrNoPython reports a noarch: python package linked without
	// Options.Python.
	ErrNoPython = errors.New("noarch python package needs python information")

	// ErrInvalidPackage reports malformed package metadata (paths.json,
	// has_prefix, link.json, index.json) or a package file whose type does
	// not match its metadata.
	ErrInvalidPackage = errors.New("invalid package")

	// ErrInvalidEntryPoint reports a noarch: python entry point that is not
	// "command = module:callable" with a simple command name and dotted
	// Python identifiers.
	ErrInvalidEntryPoint = errors.New("invalid entry point")

	// ErrUnsupported reports a feature this package does not implement, such
	// as Windows entry point launchers.
	ErrUnsupported = errors.New("unsupported")

	// ErrNotInstalled reports that Unlink found no conda-meta record for the
	// package.
	ErrNotInstalled = errors.New("package is not installed")
)

// ExistsError reports a destination of a package that already exists in
// the prefix: a path of another package, or another path of the same
// package that lands there (through a symlink the package itself
// creates). It unwraps to ErrExists. An ExistsError is immutable once
// returned.
type ExistsError struct {
	// Path is the destination as planned: prefix-relative,
	// slash-separated, after noarch: python relocation, as PlannedPaths
	// lists it. The existing file may lie behind a directory symlink on the
	// way to it.
	Path string
	// SamePackage is set when another path of the same package lands
	// there.
	SamePackage bool
}

// Error names the path.
func (e *ExistsError) Error() string {
	if e.SamePackage {
		return fmt.Sprintf("%s: %v (another path of the package lands there)", e.Path, ErrExists)
	}
	return fmt.Sprintf("%s: %v", e.Path, ErrExists)
}

// Unwrap returns ErrExists.
func (e *ExistsError) Unwrap() error { return ErrExists }
