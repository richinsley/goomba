package goomba

import (
	"errors"
	"fmt"

	"github.com/richinsley/goomba/link"
)

// Sentinel errors returned (wrapped) by InstallLock. Test for them with
// errors.Is. Errors from the packages InstallLock builds on (lock, fetch,
// archive, link, virtual) are wrapped unchanged, so their sentinels work
// too: lock.ErrInvalid for a lockfile that fails validation, the fetch
// error types for downloads, link.ErrLinkScript and link.ErrUnsafePath for
// refused package contents, context.Canceled after cancellation.
var (
	// ErrInvalidOptions reports unusable InstallOptions: no PkgsDir, a
	// PkgsDir inside the prefix, a negative Concurrency, a malformed virtual
	// package record or a history command with a line break.
	ErrInvalidOptions = errors.New("invalid install options")

	// ErrPlatform reports a lockfile made for another platform than the
	// host's.
	ErrPlatform = errors.New("lockfile platform is not the host platform")

	// ErrVirtual reports a locked package whose dependency on, or
	// constraint of, a virtual package (__glibc, __osx, __cuda, ...) the
	// host does not satisfy. The error is a *VirtualError.
	ErrVirtual = errors.New("virtual package requirement not met")

	// ErrPrefixNotEmpty reports a prefix that exists and is not an empty
	// directory.
	ErrPrefixNotEmpty = errors.New("prefix exists and is not an empty directory")

	// ErrUnsafePrefix reports an existing prefix directory that another
	// user could change: one that does not belong to the current user or
	// that group or others may write (on Unix).
	ErrUnsafePrefix = errors.New("prefix is not private to the current user")

	// ErrPrefixLocked reports a prefix locked by another installation, in
	// this process or another.
	ErrPrefixLocked = errors.New("prefix is locked by another process")

	// ErrClobber reports two packages of one transaction that install the
	// same path. The error is a *ClobberError.
	ErrClobber = errors.New("packages install the same path")

	// ErrCompilePyc reports that the environment's Python interpreter could
	// not be run to byte-compile noarch: python packages, or died abnormally.
	// A compilation that merely fails for some files is not an error, as in
	// micromamba.
	ErrCompilePyc = errors.New("python byte-compilation failed")
)

// VirtualError reports a locked package that depends on a virtual package
// the host lacks or has in a version the dependency excludes, or that
// constrains a virtual package the host has to versions it does not have.
// It unwraps to ErrVirtual. A VirtualError is immutable once returned.
type VirtualError struct {
	// Package is the dependent package, "name-version-build".
	Package string
	// Spec is the unmet dependency as written in the lockfile, e.g.
	// "__glibc >=2.28,<3.0.a0".
	Spec string
	// Have is the host's virtual package of that name, "name version build",
	// or "" when the host has none.
	Have string
	// Constrains is set when Spec is one of the package's constrains
	// rather than depends entries: the host has the virtual package in a
	// version the package excludes.
	Constrains bool
}

// Error names the package, the unmet spec and what the host has.
func (e *VirtualError) Error() string {
	have := "none"
	if e.Have != "" {
		have = e.Have
	}
	verb := "requires"
	if e.Constrains {
		verb = "constrains"
	}
	return fmt.Sprintf("goomba: %s %s %q, host has %s: %v", e.Package, verb, e.Spec, have, ErrVirtual)
}

// Unwrap returns ErrVirtual.
func (e *VirtualError) Unwrap() error { return ErrVirtual }

// ClobberError reports a path that two packages of one transaction both
// install. micromamba warns and lets the package linked later win; goomba
// refuses the transaction. It unwraps to both ErrClobber and link.ErrExists.
// A ClobberError is immutable once returned.
type ClobberError struct {
	// Path is the prefix-relative, slash-separated path.
	Path string
	// Packages are the two packages, "name-version-build": the one that
	// claims Path first in name order (or that was linked first), then the
	// other.
	Packages [2]string
}

// Error names the path and both packages.
func (e *ClobberError) Error() string {
	return fmt.Sprintf("goomba: %s and %s both install %q: %v", e.Packages[0], e.Packages[1], e.Path, ErrClobber)
}

// Unwrap returns ErrClobber and link.ErrExists.
func (e *ClobberError) Unwrap() []error { return []error{ErrClobber, link.ErrExists} }
