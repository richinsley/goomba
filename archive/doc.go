// Package archive unpacks .conda and .tar.bz2 package archives, refusing any
// entry that would land outside the destination directory.
//
// Package archives are hostile input. Extraction refuses (with an error, and
// with nothing left on disk) absolute paths, ".." components, Windows drive
// and UNC paths, NUL bytes, names and link targets over the path limits,
// symlinks whose target resolves outside the destination, hard links to
// anything that is not a regular file extracted earlier from the same
// archive, entries written through a symlink, device files, FIFOs, sparse
// files, duplicate paths of conflicting types, and data before or after a
// .conda's zip structure. Size, entry-count and path limits are enforced on
// the bytes actually decompressed.
//
// Extraction happens in a private temporary sibling of the destination,
// opened without following symlinks and checked to be the caller's own,
// empty directory before anything is written. It is renamed into place only
// after every entry was written, every symlink resolved inside it (each
// once, within a step budget) and the whole tree checked to hold exactly
// the entries the extraction created, so the destination appears complete
// or not at all. On Linux, macOS, FreeBSD, NetBSD and OpenBSD every
// operation runs relative to a cached directory handle, each level opened
// with O_NOFOLLOW relative to its parent's, so a path swapped during
// extraction cannot redirect it. Elsewhere (Windows among them) creation
// goes through an os.Root and links, times and lookups use paths inside the
// temporary directory. Everywhere the directory's identity is checked
// before and after the final rename. The destination's parent must not be
// writable by other users.
//
// Extracted files keep their permission bits except setuid, setgid, sticky
// and group and other write (mode & 0755, applied whatever the umask), and
// directories are created 0755: nobody but the extracting user can change
// the tree, which environments hard link. conda and micromamba keep the
// archive's group write (0664 files are common).
//
// On case-insensitive filesystems an archive holding two paths that differ
// only in case (or in Unicode normalization) is refused: every creation is
// exclusive, so the second path fails instead of aliasing the first.
//
// All functions are safe for concurrent use as long as callers use distinct
// destination directories.
package archive
