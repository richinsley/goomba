// Package virtual detects virtual packages (__glibc, __linux, __unix, __osx,
// __win, __archspec, __cuda) for the current machine. Every value can be
// overridden by the caller.
//
// Detect probes the host without cgo; Defaults describes any platform
// without probing, for locking environments for other machines. Records
// are fresh on every call, and every function is safe for concurrent use.
package virtual
