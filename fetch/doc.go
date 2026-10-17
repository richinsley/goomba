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
// The cache directory must be writable only by its owner, on a filesystem
// with Unix ownership and permissions: the key file must belong to the
// current user and grant no access to others (checked on Unix), which FAT,
// exFAT and SMB/CIFS mounts cannot provide. The marker vouches for a tree
// as extracted; it does not protect files edited after extraction. Stop
// every goomba process using a cache before deleting its key; a process
// still holding the old key rereads the key file when a MAC fails to
// verify. On Windows, directories and read-only files cannot be flushed
// before the marker is written.
//
// Only https URLs are fetched. Redirects to anything else are refused, and
// no Referer is sent to a redirect target. Transient failures are retried;
// a digest mismatch never is.
package fetch
