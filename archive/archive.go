// Behaviour ported from rattler crates/rattler_package_streaming src/read.rs
// and libmamba src/core/package_handling.cpp (BSD-3-Clause, see NOTICE).

package archive

import (
	"compress/bzip2"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/richinsley/goomba/internal/rmtree"
)

// Default limits applied when a Limits field is zero or negative.
const (
	DefaultMaxFileSize     int64 = 4 << 30   // 4 GiB per entry
	DefaultMaxTotalSize    int64 = 16 << 30  // 16 GiB per archive
	DefaultMaxEntries            = 1_000_000 // entries per archive
	DefaultMaxPathLen            = 4096      // bytes per entry name or link target
	DefaultMaxPathDepth          = 256       // components per entry name or link target
	DefaultMaxSymlinkSteps       = 1_000_000 // symlink resolution steps per archive
)

// Limits bounds the resources one extraction may consume. Limits are
// enforced on bytes actually decompressed, not on what headers claim. A zero
// or negative field selects the corresponding Default value. Limits is a
// plain value, safe to share between goroutines.
type Limits struct {
	// MaxFileSize is the largest regular file an entry may produce.
	MaxFileSize int64
	// MaxTotalSize bounds the decompressed bytes of the archive's tar
	// streams (headers, padding and trailing data included) and, separately,
	// the compressed bytes read for them.
	MaxTotalSize int64
	// MaxEntries bounds the number of tar entries, .conda zip members and
	// directories created implicitly as parents of entries.
	MaxEntries int
	// MaxPathLen bounds the length in bytes of each entry name, hard link
	// name and symlink target.
	MaxPathLen int
	// MaxPathDepth bounds the number of components of each entry name, hard
	// link name and symlink target.
	MaxPathDepth int
	// MaxSymlinkSteps bounds the work of checking that symlinks stay
	// inside the destination: path components looked up, plus components
	// reused from symlinks already resolved, over the whole archive.
	MaxSymlinkSteps int
}

func (l Limits) withDefaults() Limits {
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = DefaultMaxFileSize
	}
	if l.MaxTotalSize <= 0 {
		l.MaxTotalSize = DefaultMaxTotalSize
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	if l.MaxPathLen <= 0 {
		l.MaxPathLen = DefaultMaxPathLen
	}
	if l.MaxPathDepth <= 0 {
		l.MaxPathDepth = DefaultMaxPathDepth
	}
	if l.MaxSymlinkSteps <= 0 {
		l.MaxSymlinkSteps = DefaultMaxSymlinkSteps
	}
	return l
}

// Sentinel errors. Extraction errors wrap one of these (or a context, I/O or
// decompression error) so callers can branch with errors.Is.
var (
	// ErrUnsafePath reports an entry name, link name or symlink target that
	// could reach outside the destination: absolute, "..", drive or UNC
	// paths, NUL bytes, writes through a symlink, escaping symlinks and hard
	// links to anything but an earlier regular file. It also reports a
	// temporary directory replaced during extraction.
	ErrUnsafePath = errors.New("unsafe path")
	// ErrUnsupportedEntry reports device files, FIFOs, sparse files and
	// unknown tar entry types.
	ErrUnsupportedEntry = errors.New("unsupported entry")
	// ErrConflict reports a path that appears twice with different types,
	// for example a file and later a directory of the same name.
	ErrConflict = errors.New("conflicting duplicate entry")
	// ErrLimit reports that a Limits bound was exceeded.
	ErrLimit = errors.New("limit exceeded")
	// ErrFormat reports a structurally invalid package archive, such as a
	// .conda without its pkg or info tar.
	ErrFormat = errors.New("malformed package archive")
	// ErrUnknownFormat reports an archive name that ends in neither ".conda"
	// nor ".tar.bz2", or an unknown Format.
	ErrUnknownFormat = errors.New("unknown archive format")
)

// Format names the compression of a tar stream passed to ExtractReader. It
// is a plain value, safe for concurrent use.
type Format int

// Stream formats accepted by ExtractReader.
const (
	// TarBz2 is a bzip2-compressed tar, the legacy .tar.bz2 package format.
	TarBz2 Format = iota + 1
	// TarZst is a zstd-compressed tar, the format of the pkg and info
	// members inside a .conda archive.
	TarZst
)

// String returns "tar.bz2" or "tar.zst".
func (f Format) String() string {
	switch f {
	case TarBz2:
		return "tar.bz2"
	case TarZst:
		return "tar.zst"
	}
	return fmt.Sprintf("Format(%d)", int(f))
}

// Extract unpacks a .conda or .tar.bz2 archive (chosen by file name suffix)
// into destDir, which must not exist; missing parents are created (and
// removed again on error). It extracts into a sibling temp dir and renames
// on success, so destDir appears complete or not at all. On any error,
// including cancellation of ctx, nothing is left behind.
//
// The parent of destDir must not be writable by other users: Extract
// detects its temporary directory being replaced before the final rename,
// but cannot stop another user from replacing destDir afterwards. On
// case-insensitive filesystems an archive holding two paths that differ
// only in case is refused, because the second would alias the first.
// Extract does not sync file data to disk; callers that need durability
// across a crash (package fetch does) sync before recording the result.
func Extract(ctx context.Context, archivePath, destDir string, lim Limits) error {
	if _, err := archiveFormat(archivePath); err != nil {
		return fmt.Errorf("archive: extract %q: %w", archivePath, err)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("archive: extract: %w", boundErr(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("archive: extract: %w", boundErr(err))
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("archive: extract %q: %w: not a regular file", archivePath, ErrFormat)
	}
	if err := extractAt(ctx, f, fi.Size(), archivePath, destDir, lim); err != nil {
		return fmt.Errorf("archive: extract %q: %w", archivePath, err)
	}
	return nil
}

// ExtractReaderAt is Extract for an archive already open as r with the
// given size, for example the very *os.File whose digest a caller
// verified. name selects the format by suffix (".conda" or ".tar.bz2") and
// appears in error messages; r is only read with ReadAt.
func ExtractReaderAt(ctx context.Context, r io.ReaderAt, size int64, name, destDir string, lim Limits) error {
	if err := extractAt(ctx, r, size, name, destDir, lim); err != nil {
		return fmt.Errorf("archive: extract %q: %w", name, err)
	}
	return nil
}

// archiveFormat maps a file name to "conda" or "tar.bz2".
func archiveFormat(name string) (string, error) {
	switch {
	case strings.HasSuffix(name, ".conda"):
		return "conda", nil
	case strings.HasSuffix(name, ".tar.bz2"):
		return "tar.bz2", nil
	}
	return "", fmt.Errorf("%w: want a .conda or .tar.bz2 name", ErrUnknownFormat)
}

func extractAt(ctx context.Context, r io.ReaderAt, size int64, name, destDir string, lim Limits) error {
	kind, err := archiveFormat(name)
	if err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("%w: negative size", ErrFormat)
	}
	return extractInto(ctx, destDir, lim, func(x *extractor) error {
		if kind == "conda" {
			return x.conda(r, size)
		}
		return x.stream(io.NewSectionReader(r, 0, size), TarBz2)
	})
}

// ExtractReader is the streaming form of Extract for .tar.bz2 and tar.zst
// content: it unpacks the tar stream r, compressed as format, into destDir
// under the same rules and guarantees as Extract. The whole of r is consumed
// (and counted against lim) so a stream cut short is reported. A .conda
// archive needs random access; use Extract or ExtractReaderAt for it.
func ExtractReader(ctx context.Context, r io.Reader, format Format, destDir string, lim Limits) error {
	if format != TarBz2 && format != TarZst {
		return fmt.Errorf("archive: extract to %q: %w: %v", destDir, ErrUnknownFormat, format)
	}
	err := extractInto(ctx, destDir, lim, func(x *extractor) error {
		return x.stream(r, format)
	})
	if err != nil {
		return fmt.Errorf("archive: extract %s to %q: %w", format, destDir, err)
	}
	return nil
}

// extractInto runs fill against a fresh temporary sibling of destDir, checks
// every symlink, and renames the result to destDir. The temporary directory,
// and any parent directories created for destDir, are removed on error.
func extractInto(ctx context.Context, destDir string, lim Limits, fill func(*extractor) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if destDir == "" {
		return errors.New("empty destination")
	}
	if _, err := os.Lstat(destDir); err == nil {
		return fmt.Errorf("destination %q: %w", destDir, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	clean := filepath.Clean(destDir)
	parent, base := filepath.Dir(clean), filepath.Base(clean)
	created, err := mkdirParents(parent)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			removeEmpty(created)
		}
	}()
	tmp, err := mkdirTemp(parent, "."+base+".tmp-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// If tmp was replaced by something else, this removes that
			// entry (a symlink is removed, not followed), never our tree's
			// new location; a mount point inside it is not entered.
			_ = rmtree.RemoveAll(tmp)
		}
	}()
	// Pin the directory just created before writing anything: open it
	// without following a symlink and check that it is ours and empty.
	rootDir, err := openDirNoFollow(tmp)
	if err != nil {
		return err
	}
	defer rootDir.Close()
	id, err := rootDir.Stat()
	if err != nil {
		return err
	}
	if err := checkFreshDir(rootDir, id); err != nil {
		return err
	}
	var tfs treeFS
	if haveAt {
		tfs = newAtFS(rootDir)
	} else {
		root, err := os.OpenRoot(tmp)
		if err != nil {
			return err
		}
		defer root.Close()
		rd, err := root.Open(".")
		if err != nil {
			return err
		}
		rfi, err := rd.Stat()
		rd.Close()
		if err != nil {
			return err
		}
		if !os.SameFile(rfi, id) {
			return fmt.Errorf("%w: temporary directory %q was replaced during extraction", ErrUnsafePath, tmp)
		}
		tfs = &rootFS{root: root, dir: tmp}
	}
	defer tfs.close()

	x := newExtractor(ctx, lim.withDefaults(), tfs)
	if err := fill(x); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := x.checkSymlinks(); err != nil {
		return err
	}
	if err := x.verifyTree(); err != nil {
		return err
	}
	tfs.close()
	if !haveAt {
		if err := sameEntry(tmp, id); err != nil {
			return err
		}
	}
	if err := tfs.chmodRoot(0o755); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sameEntry(tmp, id); err != nil {
		return err
	}
	if err := renameNoReplace(tmp, destDir); err != nil {
		return err
	}
	if err := sameEntry(destDir, id); err != nil {
		// Replaced between the check and the rename: whatever was renamed
		// is not ours. Remove it if it is a symlink; leave anything else.
		if fi, lerr := os.Lstat(destDir); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			_ = os.Remove(destDir)
		}
		return err
	}
	syncDir(parent)
	return nil
}

// checkFreshDir checks that the directory open as d, described by id, is
// the one just created: owned by the current user and empty.
func checkFreshDir(d *os.File, id fs.FileInfo) error {
	if !id.IsDir() {
		return fmt.Errorf("%w: temporary directory is not a directory", ErrUnsafePath)
	}
	if err := checkOwnDir(id); err != nil {
		return err
	}
	names, err := d.Readdirnames(1)
	if len(names) > 0 || (err != nil && err != io.EOF) {
		return fmt.Errorf("%w: temporary directory is not empty", ErrUnsafePath)
	}
	return nil
}

// sameEntry fails unless path names the directory described by id.
func sameEntry(path string, id fs.FileInfo) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(fi, id) {
		return fmt.Errorf("%w: temporary directory %q was replaced during extraction", ErrUnsafePath, path)
	}
	return nil
}

// mkdirParents creates dir and its missing ancestors and returns those it
// created, deepest first.
func mkdirParents(dir string) ([]string, error) {
	var missing []string
	for p := dir; ; {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, p)
		next := filepath.Dir(p)
		if next == p {
			break
		}
		p = next
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		removeEmpty(missing)
		return nil, err
	}
	return missing, nil
}

// removeEmpty removes the given directories, deepest first, stopping at the
// first that cannot be removed (it is no longer empty).
func removeEmpty(dirs []string) {
	for _, d := range dirs {
		if os.Remove(d) != nil {
			return
		}
	}
}

// mkdirTemp creates a new private directory named prefix plus 16 random hex
// digits inside dir. Unlike os.MkdirTemp it gives "*" in prefix no meaning.
func mkdirTemp(dir, prefix string) (string, error) {
	var b [8]byte
	for range 16 {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		p := filepath.Join(dir, prefix+hex.EncodeToString(b[:]))
		err := os.Mkdir(p, 0o700)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("create temporary directory in %q: too many collisions", dir)
}

// decompressor wraps r in the decoder for format. The returned close
// function releases decoder resources.
func decompressor(r io.Reader, format Format) (io.Reader, func(), error) {
	switch format {
	case TarBz2:
		return bzip2.NewReader(r), func() {}, nil
	case TarZst:
		d, err := newZstdReader(r)
		if err != nil {
			return nil, nil, err
		}
		return d, d.Close, nil
	}
	return nil, nil, ErrUnknownFormat
}
