// Package fetch downloads package archives into a content-addressed package
// cache, verifying sha256 and size before an archive becomes visible, and
// extracts them, under file locks.
//
// An archive with sha256 S lives at <Dir>/<S>/<fn> and its extracted tree
// at <Dir>/<S>/pkg, so a path returned once never holds other bytes later
// and packages differing only in digest never share a path. Extraction
// reads the very file handle whose digest was verified (or that the
// download wrote), under the lock for S, and marks the tree with an
// HMAC-keyed .goomba-extracted marker written last, after the tree was
// synced to disk. Sidecars that let an unchanged archive skip re-hashing
// carry the same kind of MAC, so neither can be forged without the cache's
// key.
//
// A Cache resolves the symlinks of its directory once, on first use, and
// works in the resolved directory from then on; the checks below apply to
// that path, so a symlink on the way to it that someone re-points later
// changes nothing.
//
// The cache directory must be writable only by its owner, on a filesystem
// with Unix ownership and permissions: the key file must belong to the
// current user and grant no access to others (checked on Unix), which FAT,
// exFAT and SMB/CIFS mounts cannot provide. On Unix every use also checks,
// in the manner of OpenSSH's StrictModes, that the cache directory belongs
// to the effective user and that neither it nor any directory above it can
// be changed by another user (each must belong to root or the user and not
// be writable by group or others unless sticky, as /tmp is; a directory of
// the user's own private group, gid equal to the uid and to the effective
// gid, counts as the user's, which is wrong only where an administrator
// shared that group with another account), and that content
// directories, the lock directory, extracted trees and their markers belong
// to the user and are not writable by group or others; anything else is an
// ErrUnsafeCache error. Directories are created 0755 and markers and
// sidecars 0600. Without these checks a user able to write the cache, or a
// directory above it, could rename into it a tree of their own holding a
// copy of a (readable) marker. On Windows ownership is not checked: keep
// the cache under the user's profile, whose ACLs exclude other users. The marker vouches for a tree
// as extracted; it does not protect files edited after extraction. Stop
// every goomba process using a cache before deleting its key; a process
// still holding the old key rereads the key file when a MAC fails to
// verify. On Windows, directories and read-only files cannot be flushed
// before the marker is written.
//
// The marker carries a format version (now 3: trees whose files have no
// group or other write). A tree with another version's marker is extracted
// again, so goomba builds with different marker versions sharing one cache
// re-extract each other's trees whenever they use a package; give them
// separate caches to avoid that.
//
// Only https URLs are fetched. Redirects to anything else are refused, and
// no Referer is sent to a redirect target. Transient failures are retried;
// a digest mismatch never is.
package fetch
