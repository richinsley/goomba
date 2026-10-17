// Package lock reads and writes goomba lockfiles, imports and exports conda
// @EXPLICIT files, and builds a lockfile from an existing environment's
// conda-meta records.
//
// A lockfile is the only input the installer trusts, so every reader here
// treats its input as hostile: parsing is strict and everything read is
// validated before it is returned. Lockfiles refuse URLs with credentials,
// and error messages never quote a URL's user name or password: they show
// "https://***@host/..." instead. The functions in this package are safe
// for concurrent use; the types are plain values (see each type).
package lock
