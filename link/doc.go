// Package link installs one unpacked package into an environment prefix:
// files and symlinks, install-path rewriting, noarch python, entry points,
// and the conda-meta record.
//
// The behaviour is a port of libmamba's LinkPackage and UnlinkPackage
// (src/core/link.cpp), cross-checked against rattler's install module. The
// result matches the environment micromamba builds file for file, except for
// the install path itself.
//
// Link writes the package files and returns an Installed description;
// WriteCondaMeta then records it under conda-meta/. Unlink reverses both.
// Link never runs package scripts: a package that ships a pre-link,
// post-link or pre-unlink script is refused with ErrLinkScript unless the
// caller allows it, and even then the script is only installed, and only
// directly in bin or Scripts. Script names elsewhere, and bin or Scripts as
// symlinks, are always refused, so that no directory holding a script can
// later be made into bin.
//
// Every input is treated as hostile. Destinations are confined to the prefix
// (symlinks are followed only while they stay inside it), sources are
// confined to the package directory, entry points are validated before
// anything is written, and a failed Link removes everything it created.
//
// The functions in this package keep no shared state. Link may run
// concurrently for different packages in one prefix: shared directories are
// created race-free and files are created exclusively, so two packages that
// claim the same path fail with ErrExists rather than overwrite each other.
// A symlink's digest is taken from whatever its target holds at the time, so
// packages whose symlinks point into each other should be linked in
// dependency order. WriteCondaMeta may also run concurrently for different
// packages. Unlink must not run concurrently with Link on the same prefix,
// and AppendHistory calls on one prefix must be serialised.
package link
