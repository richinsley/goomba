// Entry handling ported from rattler crates/rattler_package_streaming
// src/read.rs (unpack_tar_archive_sync and helpers) and the libarchive
// ARCHIVE_EXTRACT_SECURE_* flags used by libmamba src/core/package_handling.cpp
// (BSD-3-Clause, see NOTICE), tightened to refuse rather than skip.

package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
)

// filePermMask keeps the permission bits of regular files (so the
// executable bit survives) but drops setuid, setgid, sticky and group and
// world write. It is applied with chmod, whatever the umask. Package
// archives commonly carry 0664 files, which conda and micromamba keep;
// goomba does not, so that nobody but the user can rewrite an extracted
// file, which environments hard link.
const filePermMask fs.FileMode = 0o755

// maxSymlinkHops bounds symlink chains, like Linux's MAXSYMLINKS.
const maxSymlinkHops = 40

// kind is the type of a path created during extraction.
type kind uint8

const (
	kindNone kind = iota
	kindDir
	kindFile
	kindSymlink
	kindOther // found on disk only: never created by an extraction
)

func (k kind) String() string {
	switch k {
	case kindDir:
		return "directory"
	case kindFile:
		return "file"
	case kindSymlink:
		return "symlink"
	case kindOther:
		return "special file"
	}
	return "nothing"
}

// extractor writes entries below dir, a fresh private directory, through
// fs. nodes records every path it created, keyed by its clean
// slash-separated path relative to dir; it is the authority for what
// exists, so no write ever goes through a symlink and every creation is
// exclusive.
type extractor struct {
	ctx      context.Context
	lim      Limits
	fs       treeFS
	nodes    map[string]kind
	entries  int
	inBytes  int64 // compressed bytes read
	outBytes int64 // decompressed bytes read
	buf      []byte
}

func newExtractor(ctx context.Context, lim Limits, fs treeFS) *extractor {
	return &extractor{
		ctx:   ctx,
		lim:   lim,
		fs:    fs,
		nodes: make(map[string]kind),
		buf:   make([]byte, 128<<10),
	}
}

func (x *extractor) countEntry() error {
	x.entries++
	if x.entries > x.lim.MaxEntries {
		return fmt.Errorf("%w: more than %d entries", ErrLimit, x.lim.MaxEntries)
	}
	return nil
}

// meter counts bytes read from r against max and checks ctx on every read.
type meter struct {
	ctx  context.Context
	r    io.Reader
	n    *int64
	max  int64
	what string
}

func (m *meter) Read(p []byte) (int, error) {
	if err := m.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := m.r.Read(p)
	*m.n += int64(n)
	if *m.n > m.max {
		return n, fmt.Errorf("%w: %s over %d bytes", ErrLimit, m.what, m.max)
	}
	return n, err
}

// writerOnly hides io.ReaderFrom so io.CopyBuffer uses the given buffer.
type writerOnly struct{ io.Writer }

// stream unpacks the compressed tar in, then reads both the decompressed
// stream and in to their ends so truncation and trailing garbage are
// reported.
func (x *extractor) stream(in io.Reader, format Format) error {
	cin := &meter{ctx: x.ctx, r: in, n: &x.inBytes, max: x.lim.MaxTotalSize, what: "compressed input"}
	dec, closeDec, err := decompressor(cin, format)
	if err != nil {
		return err
	}
	defer closeDec()
	out := &meter{ctx: x.ctx, r: dec, n: &x.outBytes, max: x.lim.MaxTotalSize, what: "decompressed size"}
	if err := x.untar(out); err != nil {
		return err
	}
	if _, err := io.CopyBuffer(writerOnly{io.Discard}, out, x.buf); err != nil {
		return fmt.Errorf("after end of tar: %w", err)
	}
	if _, err := io.CopyBuffer(writerOnly{io.Discard}, cin, x.buf); err != nil {
		return fmt.Errorf("after end of %s stream: %w", format, err)
	}
	return nil
}

func (x *extractor) untar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if err := x.countEntry(); err != nil {
			return err
		}
		if err := x.entry(tr, hdr); err != nil {
			return fmt.Errorf("entry %s: %w", quote(hdr.Name), boundErr(err))
		}
	}
}

func isSparse(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

func (x *extractor) entry(tr *tar.Reader, hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil // global PAX defaults; nothing to create
	}
	if isSparse(hdr) {
		return fmt.Errorf("%w: sparse file", ErrUnsupportedEntry)
	}
	typ := hdr.Typeflag
	dirName := strings.HasSuffix(hdr.Name, "/")
	switch typ {
	case tar.TypeReg:
		if dirName {
			// Pre-POSIX archives mark directories with a trailing slash
			// on a regular entry.
			if hdr.Size != 0 {
				return fmt.Errorf("%w: regular file with a directory name", ErrFormat)
			}
			typ = tar.TypeDir
		}
	case tar.TypeDir:
	case tar.TypeSymlink, tar.TypeLink:
		if dirName {
			return fmt.Errorf("%w: link with a directory name", ErrFormat)
		}
	case tar.TypeChar, tar.TypeBlock:
		return fmt.Errorf("%w: device file", ErrUnsupportedEntry)
	case tar.TypeFifo:
		return fmt.Errorf("%w: fifo", ErrUnsupportedEntry)
	default:
		return fmt.Errorf("%w: tar type %q", ErrUnsupportedEntry, typ)
	}

	rel, err := cleanName(hdr.Name, x.lim)
	if err != nil {
		return err
	}
	if rel == "" {
		if typ == tar.TypeDir {
			return nil // "./" names the destination itself
		}
		return fmt.Errorf("%w: entry names the destination itself", ErrUnsafePath)
	}
	switch typ {
	case tar.TypeDir:
		return x.mkdir(rel)
	case tar.TypeSymlink:
		return x.symlink(rel, hdr.Linkname)
	case tar.TypeLink:
		return x.hardlink(rel, hdr.Linkname)
	default:
		return x.file(rel, hdr, tr)
	}
}

// ensureParents creates the missing parent directories of rel, refusing to
// pass through anything but a directory created by this extraction. A
// directory node exists only with all its ancestors as directories, and
// never changes type, so an existing parent ends the walk at once.
func (x *extractor) ensureParents(rel string) error {
	parent, _ := splitRel(rel)
	if parent == "" || x.nodes[parent] == kindDir {
		return nil
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] != '/' {
			continue
		}
		p := rel[:i]
		switch x.nodes[p] {
		case kindDir:
		case kindNone:
			// Implicit directories count as entries, so one deep name
			// cannot create an unbounded number of directories.
			if err := x.countEntry(); err != nil {
				return err
			}
			if err := x.fs.mkdir(p); err != nil {
				return err
			}
			x.nodes[p] = kindDir
		case kindSymlink:
			return fmt.Errorf("%w: parent %s is a symlink", ErrUnsafePath, quote(p))
		default:
			return fmt.Errorf("%w: parent %s is a %s", ErrConflict, quote(p), x.nodes[p])
		}
	}
	return nil
}

// prepare makes rel ready to be created as k: its parents exist and it does
// not. A later entry of the same kind replaces an earlier one, as in tar;
// the old file is unlinked, never truncated, so earlier hard links keep
// their content.
func (x *extractor) prepare(rel string, k kind) error {
	if err := x.ensureParents(rel); err != nil {
		return err
	}
	old := x.nodes[rel]
	if old == kindNone {
		return nil
	}
	if old != k {
		return fmt.Errorf("%w: %s replaces a %s", ErrConflict, k, old)
	}
	if err := x.fs.remove(rel); err != nil {
		return err
	}
	delete(x.nodes, rel)
	return nil
}

func (x *extractor) mkdir(rel string) error {
	if err := x.ensureParents(rel); err != nil {
		return err
	}
	switch old := x.nodes[rel]; old {
	case kindDir:
		return nil
	case kindNone:
	default:
		return fmt.Errorf("%w: directory replaces a %s", ErrConflict, old)
	}
	if err := x.fs.mkdir(rel); err != nil {
		return err
	}
	x.nodes[rel] = kindDir
	return nil
}

func (x *extractor) file(rel string, hdr *tar.Header, r io.Reader) error {
	if hdr.Size < 0 {
		return fmt.Errorf("%w: negative size", ErrFormat)
	}
	if hdr.Size > x.lim.MaxFileSize {
		return fmt.Errorf("%w: file size %d over %d bytes", ErrLimit, hdr.Size, x.lim.MaxFileSize)
	}
	if err := x.prepare(rel, kindFile); err != nil {
		return err
	}
	f, err := x.fs.create(rel)
	if err != nil {
		return err
	}
	x.nodes[rel] = kindFile
	n, err := io.CopyBuffer(writerOnly{f}, io.LimitReader(r, x.lim.MaxFileSize+1), x.buf)
	if err == nil && n > x.lim.MaxFileSize {
		err = fmt.Errorf("%w: file over %d bytes", ErrLimit, x.lim.MaxFileSize)
	}
	if err == nil {
		err = f.Chmod(fs.FileMode(hdr.Mode) & filePermMask)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// Python compares source mtimes with those recorded in shipped .pyc
	// files. A filesystem that cannot store the time (exFAT before 1980)
	// is not an integrity problem, so the error is ignored, as rattler
	// does.
	_ = x.fs.setMtime(rel, hdr.ModTime)
	return nil
}

func (x *extractor) symlink(rel, target string) error {
	if err := checkLinkTarget(target, x.lim); err != nil {
		return err
	}
	if err := x.prepare(rel, kindSymlink); err != nil {
		return err
	}
	if err := x.fs.symlink(rel, target); err != nil {
		return err
	}
	x.nodes[rel] = kindSymlink
	return nil
}

func (x *extractor) hardlink(rel, linkname string) error {
	src, err := cleanName(linkname, x.lim)
	if err != nil {
		return fmt.Errorf("hard link target %s: %w", quote(linkname), err)
	}
	if x.nodes[src] != kindFile || src == "" {
		return fmt.Errorf("%w: hard link to %s, which is not a regular file extracted earlier", ErrUnsafePath, quote(linkname))
	}
	if src == rel {
		return fmt.Errorf("%w: hard link to itself", ErrUnsafePath)
	}
	if err := x.prepare(rel, kindFile); err != nil {
		return err
	}
	if err := x.fs.link(src, rel); err != nil {
		return err
	}
	x.nodes[rel] = kindFile
	return nil
}

// checkSymlinks verifies, once every entry exists, that each symlink
// resolves inside the extraction directory. Checking the final tree catches
// chains through symlinks created later in the archive.
func (x *extractor) checkSymlinks() error {
	var links []string
	for p, k := range x.nodes {
		if k == kindSymlink {
			links = append(links, p)
		}
	}
	slices.Sort(links)
	r := newResolver(x)
	for _, l := range links {
		if _, err := r.link(l); err != nil {
			return fmt.Errorf("symlink %s: %w", quote(l), boundErr(err))
		}
	}
	return nil
}

// verifyTree checks that the extraction directory holds exactly the
// entries the extraction created, each of the kind it was created as, so
// nothing planted in it during extraction survives.
func (x *extractor) verifyTree() error {
	dirs := []string{""}
	for p, k := range x.nodes {
		if k == kindDir {
			dirs = append(dirs, p)
		}
	}
	slices.Sort(dirs)
	seen := 0
	for _, d := range dirs {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		names, err := x.fs.list(d)
		if err != nil {
			return boundErr(err)
		}
		slices.Sort(names)
		for _, n := range names {
			rel := n
			if d != "" {
				rel = d + "/" + n
			}
			want, ok := x.nodes[rel]
			if !ok {
				return fmt.Errorf("%w: unexpected entry %s in the extraction directory", ErrUnsafePath, quote(rel))
			}
			got, err := x.fs.kindOf(rel)
			if err != nil {
				return boundErr(err)
			}
			if got != want {
				return fmt.Errorf("%w: %s is a %s, extracted as a %s", ErrUnsafePath, quote(rel), got, want)
			}
			seen++
		}
	}
	if seen != len(x.nodes) {
		return fmt.Errorf("%w: entries disappeared from the extraction directory", ErrUnsafePath)
	}
	return nil
}
