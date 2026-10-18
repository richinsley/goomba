// Ported from libmamba src/core/link.cpp (LinkPackage::execute,
// LinkPackage::link_path, LinkPackage::create_python_entry_point,
// check_path_within_prefix) (BSD-3-Clause, see NOTICE).

package link

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/richinsley/goomba/record"
)

// emptySHA256 is the digest of no bytes; libmamba records it for symlinks
// that do not resolve to a file (MAMBA_EMPTY_SHA).
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// entryPointMode is the permission of generated entry point scripts.
// libmamba's make_executable uses 0775; goomba does not make them group
// writable.
const entryPointMode = 0o755

// Options configures Link and WriteCondaMeta. An Options value is not
// modified by this package and may be shared between goroutines.
type Options struct {
	// Prefix is the absolute install path. It is written into files that
	// carry a prefix placeholder and into entry point shebangs.
	Prefix string
	// Platform is the target subdir, e.g. "linux-64". It selects the shebang
	// length limit (127 bytes for linux-* and "", 512 otherwise), Windows
	// behaviour (win-*) and osx-arm64 re-signing.
	Platform string
	// Python describes the environment's interpreter. It is required for
	// noarch: python packages and ignored otherwise.
	Python *PythonInfo
	// AllowScripts lists package names whose own link scripts may be
	// installed. Scripts are never run. Other packages that ship one are
	// refused with ErrLinkScript.
	AllowScripts []string
	// RequestedSpec is stored as requested_spec in conda-meta.
	RequestedSpec string
	// Signer re-signs a rewritten binary on osx-arm64 targets. nil selects
	// the default: "/usr/bin/codesign -s - -f <path>" when running on macOS
	// (killed if Link's context is cancelled), nothing elsewhere. A custom
	// Signer gets no context; Link checks for cancellation after each file.
	Signer func(path string) error
	// CopyOnly copies every file instead of hard linking it.
	CopyOnly bool

	// beforeOpen, if set, runs between the checks on a resolved destination
	// and the opening of its directory. Tests use it to change the prefix
	// at the worst moment.
	beforeOpen func(dst string)
}

// PathRecord is one entry of conda-meta's paths_data.paths. It is a plain
// value, safe for concurrent reads.
type PathRecord struct {
	// Path is the prefix-relative, slash-separated destination.
	Path string
	// PathType is the package path type, or PathPycFile or
	// PathUnixPythonEntryPoint for paths created by linking.
	PathType PathType
	// SHA256 is the digest recorded in paths.json (empty for symlinks).
	SHA256 string
	// SHA256InPrefix is the digest of the installed file, after prefix
	// replacement; for symlinks, of the file they resolve to. Empty for
	// pyc files and entry points, as in micromamba.
	SHA256InPrefix string
	// SizeInBytes is the size recorded in paths.json (before replacement).
	SizeInBytes uint64
	// NoLink is set when paths.json marked the file no_link.
	NoLink bool
}

// Installed describes what Link put into the prefix. It is the input of
// WriteCondaMeta. An Installed value is not safe for concurrent
// modification.
type Installed struct {
	// Prefix is the install prefix.
	Prefix string
	// PackageDir is the absolute extracted package directory.
	PackageDir string
	// Noarch is the effective noarch kind: "", "generic" or "python".
	Noarch string
	// Paths lists every installed path in conda-meta order: package files
	// in paths.json order, then the .pyc files a byte-compilation step would
	// create (noarch: python only; they are recorded, not created), then
	// entry points.
	Paths []PathRecord
	// PythonSources lists the prefix-relative .py files whose .pyc files are
	// recorded in Paths, for a caller that byte-compiles them.
	PythonSources []string
	// Index holds the fields of the package's info/index.json; WriteCondaMeta
	// adds those the record does not provide (arch, platform, ...).
	Index map[string]json.RawMessage
}

// Files returns the prefix-relative paths of Paths, the conda-meta "files"
// list.
func (in *Installed) Files() []string {
	out := make([]string, len(in.Paths))
	for i, p := range in.Paths {
		out[i] = p.Path
	}
	return out
}

type plannedPath struct {
	entry      PathEntry
	dst        string
	src        string // absolute source path, directory resolved in the package
	srcMode    fs.FileMode
	linkTarget string // softlinks: the symlink text
	copyFrom   string // no_link softlinks: the resolved target file to copy
}

type plannedEntryPoint struct {
	ep  EntryPoint
	dst string
}

// undo records what Link created, in creation order, so a failure can
// remove it. Each entry keeps the identity of what was created: rollback
// removes only entries that are still the same file, so it never deletes
// something another process put in their place.
type undo struct {
	entries []undoEntry
}

type undoEntry struct {
	path string
	info fs.FileInfo
}

func (u *undo) add(path string) {
	if fi, err := os.Lstat(path); err == nil {
		u.entries = append(u.entries, undoEntry{path: path, info: fi})
	}
}

func (u *undo) addAll(paths []string) {
	for _, p := range paths {
		u.add(p)
	}
}

func (u *undo) rollback() {
	for i := len(u.entries) - 1; i >= 0; i-- {
		e := u.entries[i]
		if fi, err := os.Lstat(e.path); err == nil && os.SameFile(fi, e.info) {
			_ = os.Remove(e.path) // directories go only when empty
		}
	}
}

// Link installs the extracted package in pkgDir, described by rec, into
// opt.Prefix exactly as micromamba does: package files are hard linked (or
// copied when they need prefix replacement, are marked no_link, cannot be
// hard linked, or opt.CopyOnly is set), symlinks are recreated, noarch:
// python files are relocated and entry points written. It does not write the
// conda-meta record; call WriteCondaMeta with the result.
//
// Link first creates the prefix and its conda-meta directory if they are
// missing, so that conda-meta can be recognised by identity; both stay,
// even empty, if Link fails. Everything else is validated before the first
// write, by resolving every destination
// as if the package's earlier entries already existed: paths; where they
// really land (never in conda-meta); link-script names (refused anywhere
// except an allowed package's own script, as a file directly in bin or
// Scripts), also for directories created on the way; bin and Scripts
// themselves (never symlinks); symlink targets (after dropping "." and
// empty components: leading ".." only, no component naming conda-meta,
// never climbing above the prefix from the symlink's physical directory,
// never leading into conda-meta; the target is written as the package has
// it); entry points; placeholder
// lengths; existing destinations; and, for Windows targets or hosts, names
// that Win32 would read as another name (trailing dots or spaces, ':' and
// 8.3 short names of conda-meta or Scripts). Names are compared after
// dropping invisible code points and Unicode case folding. The checks on
// resolved locations run again when each entry is created. If anything
// fails, Link removes what it created, including directories (but not
// conda-meta), before returning the error.
//
// Like libmamba, Link does not verify file contents against the sha256 in
// paths.json; the archive's digest is checked when it is fetched. For files
// that are hard linked or copied unchanged, sha256_in_prefix is the
// paths.json digest. Files with setuid, setgid or sticky bits are copied
// without those bits instead of being hard linked.
func Link(ctx context.Context, pkgDir string, rec *record.PackageRecord, opt Options) (inst *Installed, err error) {
	if rec == nil {
		return nil, errors.New("link: nil package record")
	}
	dist := rec.Dist()
	fail := func(err error) error { return fmt.Errorf("link: %s: %w", dist, err) }
	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}
	pp, err := planPackage(pkgDir, rec, opt)
	if err != nil {
		return nil, fail(err)
	}
	prefix, pkgDir, py, plan, eps, windows := pp.prefix, pp.pkgDir, pp.py, pp.paths, pp.eps, pp.windows

	// Create the prefix and its conda-meta directory, so that conda-meta
	// can be recognised by identity from now on. Everything else Link
	// creates is removed again on failure. conda-meta itself (and so the
	// prefix holding it) stays even when empty: removing it could pull it
	// from under a concurrent Link or WriteCondaMeta, and an empty
	// conda-meta is harmless.
	tx := &undo{}
	defer func() {
		if err != nil {
			tx.rollback()
			inst = nil
		}
	}()
	var made []string
	err = mkdirAllRecorded(filepath.Join(prefix, "conda-meta"), &made)
	if err != nil {
		return nil, fail(err)
	}

	// Dry run: resolve every destination as if the earlier ones existed,
	// and check where it really lands. Nothing is written. Directories
	// created on the way are checked like entries.

	pr := newRoot(prefix)
	signer := opt.Signer
	if signer == nil {
		signer = defaultSigner(ctx)
	}
	l := &linker{root: pr, guard: newGuard(pr), prefix: prefix, name: rec.Name, opt: opt, signer: signer, tx: tx, windows: windows}
	pr.mkdirCheck = func(parent []string, name string) error {
		if err := l.checkResolved(parent, name, kindDir, "", nil); err != nil {
			return fmt.Errorf("directory %s: %w", strings.Join(append(parent[:len(parent):len(parent)], name), "/"), err)
		}
		return nil
	}
	ov := overlay{}
	for _, p := range plan {
		kind := kindFile
		switch {
		case p.entry.PathType == PathDirectory:
			kind = kindDir
		case p.entry.PathType == PathSoftlink && p.copyFrom == "":
			kind = kindSymlink
		}
		if err := l.planDest(ov, p.dst, kind, p.linkTarget); err != nil {
			return nil, fail(err)
		}
	}
	for _, e := range eps {
		if err := l.planDest(ov, e.dst, kindFile, ""); err != nil {
			return nil, fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}

	// Write.
	inst = &Installed{Prefix: prefix, PackageDir: pkgDir, Noarch: pp.noarch, Index: pp.index}
	for _, p := range plan {
		if err := ctx.Err(); err != nil {
			return nil, fail(err)
		}
		r, err := l.linkPath(p)
		if err != nil {
			return nil, fail(atPath(p.dst, err))
		}
		inst.Paths = append(inst.Paths, r)
	}
	if err := l.hashSymlinks(inst.Paths, plan); err != nil {
		return nil, fail(err)
	}
	if py != nil {
		for _, pyc := range pp.pyc {
			inst.PythonSources = append(inst.PythonSources, pyc.src)
			inst.Paths = append(inst.Paths, PathRecord{Path: pyc.dst, PathType: PathPycFile})
		}
		shebang := PythonShebang(prefix+"/"+py.Executable, opt.Platform)
		for _, e := range eps {
			if err := ctx.Err(); err != nil {
				return nil, fail(err)
			}
			if err := l.writeEntryPoint(e.dst, entryPointScript(shebang, e.ep)); err != nil {
				return nil, fail(atPath(e.dst, err))
			}
			inst.Paths = append(inst.Paths, PathRecord{Path: e.dst, PathType: PathUnixPythonEntryPoint})
		}
	}
	return inst, nil
}

// pkgPlan is what Link plans from the package and the options alone,
// before it looks at the prefix: every destination, validated.
type pkgPlan struct {
	prefix  string // cleaned absolute prefix
	pkgDir  string // absolute package directory
	index   map[string]json.RawMessage
	noarch  string
	py      *PythonInfo // set for noarch: python packages only
	paths   []plannedPath
	pyc     []plannedPyc
	eps     []plannedEntryPoint
	windows bool // the target or the host is Windows
}

// plannedPyc is a .pyc file recorded for a noarch: python source.
type plannedPyc struct {
	src, dst string // prefix-relative source and .pyc paths
}

// planPackage reads and validates the package metadata and plans every
// destination: the package entries (relocated for noarch: python), the
// .pyc files of noarch: python sources and the entry points, each checked
// as a clean relative path outside conda-meta, not listed twice, not a
// link script (unless allowed), and, for Windows targets or hosts, not a
// name Win32 would read as another. It does not look at the prefix.
func planPackage(pkgDir string, rec *record.PackageRecord, opt Options) (*pkgPlan, error) {
	if err := checkRecord(rec); err != nil {
		return nil, err
	}
	prefix, err := checkPrefix(opt.Prefix)
	if err != nil {
		return nil, err
	}
	if pkgDir, err = filepath.Abs(pkgDir); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(pkgDir); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidPackage, pkgDir)
	}
	pkg := newRoot(pkgDir)

	index, indexNoarch, err := readIndex(pkg, rec)
	if err != nil {
		return nil, err
	}
	pp := &pkgPlan{prefix: prefix, pkgDir: pkgDir, index: index, noarch: rec.Noarch}
	if pp.noarch == "" {
		pp.noarch = indexNoarch
	}
	if pp.noarch == "python" {
		if opt.Python == nil {
			return nil, ErrNoPython
		}
		if err := opt.Python.validate(); err != nil {
			return nil, err
		}
		pp.py = opt.Python
	}

	entries, err := readPackagePaths(pkg)
	if err != nil {
		return nil, err
	}

	// Plan and validate every destination before writing anything.
	seen := make(map[string]bool, len(entries))
	pp.windows = isWindows(opt.Platform) || runtime.GOOS == "windows"
	checkDst := func(dst string) error {
		if err := validRelPath(dst); err != nil {
			return err
		}
		first, _, _ := strings.Cut(dst, "/")
		if foldName(first) == "conda-meta" {
			return fmt.Errorf("%w: %q is inside conda-meta", ErrUnsafePath, dst)
		}
		if pp.windows {
			for c := range strings.SplitSeq(dst, "/") {
				if windowsAlias(c) {
					return fmt.Errorf("%w: %q has a component Windows would read as another name", ErrUnsafePath, dst)
				}
			}
		}
		if seen[dst] {
			return fmt.Errorf("%w: %q is listed twice", ErrInvalidPackage, dst)
		}
		seen[dst] = true
		dir, leaf := path.Split(dst)
		inScriptDir := strings.Count(dst, "/") == 1 && isScriptDirName(strings.TrimSuffix(dir, "/"))
		if !checkScriptName(rec.Name, opt.AllowScripts, leaf, kindFile, inScriptDir, inScriptDir) {
			return fmt.Errorf("%w: %s", ErrLinkScript, dst)
		}
		return nil
	}
	pp.paths = make([]plannedPath, 0, len(entries))
	for _, e := range entries {
		p, err := planPath(pkg, e, pp.py, prefix, opt.Platform)
		if err != nil {
			return nil, err
		}
		if err := checkDst(p.dst); err != nil {
			return nil, err
		}
		pp.paths = append(pp.paths, p)
	}
	if pp.py != nil {
		for _, p := range pp.paths {
			if isPycCandidate(p.entry.Path) {
				pp.pyc = append(pp.pyc, plannedPyc{src: p.dst, dst: PycPath(p.dst, pp.py.Version)})
			}
		}
		defs, err := readEntryPoints(pkg)
		if err != nil {
			return nil, err
		}
		if len(defs) > 0 && isWindows(opt.Platform) {
			return nil, fmt.Errorf("%w: windows entry point launchers", ErrUnsupported)
		}
		for _, def := range defs {
			ep, err := ParseEntryPoint(def)
			if err != nil {
				return nil, err
			}
			dst := pp.py.BinDir + "/" + ep.Command
			if err := checkDst(dst); err != nil {
				return nil, err
			}
			pp.eps = append(pp.eps, plannedEntryPoint{ep: ep, dst: dst})
		}
	}
	return pp, nil
}

// planned lists the destinations in the order of Installed.Paths.
func (pp *pkgPlan) planned() []PlannedPath {
	out := make([]PlannedPath, 0, len(pp.paths)+len(pp.pyc)+len(pp.eps))
	for _, p := range pp.paths {
		out = append(out, PlannedPath{Path: p.dst, PathType: p.entry.PathType})
	}
	for _, p := range pp.pyc {
		out = append(out, PlannedPath{Path: p.dst, PathType: PathPycFile})
	}
	for _, e := range pp.eps {
		out = append(out, PlannedPath{Path: e.dst, PathType: PathUnixPythonEntryPoint})
	}
	return out
}

// PlannedPath is one path Link creates in the prefix, or, for a .pyc file,
// records for a later byte-compilation. It is a plain value.
type PlannedPath struct {
	// Path is the prefix-relative, slash-separated destination, after
	// noarch: python relocation.
	Path string
	// PathType is the package entry's type (PathHardlink, PathSoftlink or
	// PathDirectory), PathPycFile or PathUnixPythonEntryPoint: the type
	// the path has in Installed.Paths.
	PathType PathType
}

// PlannedPaths returns the paths Link(ctx, pkgDir, rec, opt) creates (or
// records, for .pyc files), exactly as Installed.Paths will list them and
// in the same order: the package's entries from info/paths.json or the
// legacy info/files (relocated for noarch: python), then the .pyc files of
// noarch: python sources, then the entry points. It reads and validates
// the package metadata as Link's planning does (record fields, index.json,
// paths, link-script names, placeholder lengths against opt.Prefix, entry
// points, Windows names) and fails with the same errors, so a package
// PlannedPaths accepts fails Link only because of the prefix: a
// destination that exists, or a symlink in the prefix that leads a path
// somewhere it may not go. It does not look at the prefix, which need not
// exist; opt.Prefix is still required (an absolute path), as for Link.
func PlannedPaths(ctx context.Context, pkgDir string, rec *record.PackageRecord, opt Options) ([]PlannedPath, error) {
	if rec == nil {
		return nil, errors.New("link: nil package record")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("link: %s: %w", rec.Dist(), err)
	}
	pp, err := planPackage(pkgDir, rec, opt)
	if err != nil {
		return nil, fmt.Errorf("link: %s: %w", rec.Dist(), err)
	}
	return pp.planned(), nil
}

// atPath prefixes err with the destination dst, unless err is an
// *ExistsError for dst, which names it already.
func atPath(dst string, err error) error {
	var ee *ExistsError
	if errors.As(err, &ee) && ee.Path == dst {
		return err
	}
	return fmt.Errorf("%s: %w", dst, err)
}

// checkRecord validates the parts of rec that become file names.
func checkRecord(rec *record.PackageRecord) error {
	for _, f := range []struct{ name, val string }{{"name", rec.Name}, {"version", rec.Version}, {"build", rec.Build}} {
		if f.val == "" {
			return fmt.Errorf("%w: empty %s", ErrInvalidPackage, f.name)
		}
		if strings.ContainsAny(f.val, `/\:`) || strings.HasPrefix(f.val, ".") || strings.IndexFunc(f.val, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return fmt.Errorf("%w: %s %q is not usable in a file name", ErrUnsafePath, f.name, f.val)
		}
	}
	return nil
}

// checkPrefix returns the cleaned absolute prefix.
func checkPrefix(prefix string) (string, error) {
	if prefix == "" || !filepath.IsAbs(prefix) {
		return "", fmt.Errorf("%w: prefix %q is not absolute", ErrUnsafePath, prefix)
	}
	if strings.IndexFunc(prefix, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", fmt.Errorf("%w: prefix %q contains a control character", ErrUnsafePath, prefix)
	}
	return filepath.Clean(prefix), nil
}

// readIndex reads info/index.json if present, checks that it describes rec
// and returns its fields and its noarch kind.
func readIndex(pkg *root, rec *record.PackageRecord) (map[string]json.RawMessage, string, error) {
	data, err := pkg.readFile("info/index.json", maxMetadataSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read index.json: %w", err)
	}
	var index map[string]json.RawMessage
	if err := json.Unmarshal(data, &index); err != nil || index == nil {
		return nil, "", fmt.Errorf("parse index.json: %w: not a JSON object", ErrInvalidPackage)
	}
	for _, f := range []struct{ key, want string }{{"name", rec.Name}, {"version", rec.Version}, {"build", rec.Build}} {
		raw, ok := index[f.key]
		if !ok {
			continue
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil || got != f.want {
			return nil, "", fmt.Errorf("%w: index.json %s %s does not match the record's %q", ErrInvalidPackage, f.key, raw, f.want)
		}
	}
	noarch := ""
	if raw, ok := index["noarch"]; ok {
		var s string
		var b bool
		switch {
		case json.Unmarshal(raw, &s) == nil:
			noarch = s
		case json.Unmarshal(raw, &b) == nil:
			if b {
				noarch = "generic"
			}
		}
	}
	return index, noarch, nil
}

// readEntryPoints returns noarch.entry_points from info/link.json.
func readEntryPoints(pkg *root) ([]string, error) {
	data, err := pkg.readFile("info/link.json", maxMetadataSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read link.json: %w", err)
	}
	return parseLinkJSON(data)
}

func parseLinkJSON(data []byte) ([]string, error) {
	var lj struct {
		Noarch *struct {
			EntryPoints []string `json:"entry_points"`
		} `json:"noarch"`
	}
	if err := json.Unmarshal(data, &lj); err != nil {
		return nil, fmt.Errorf("parse link.json: %w: %v", ErrInvalidPackage, err)
	}
	if lj.Noarch == nil {
		return nil, nil
	}
	return lj.Noarch.EntryPoints, nil
}

// planPath validates one package entry against its source file and computes
// its destination.
func planPath(pkg *root, e PathEntry, py *PythonInfo, prefix, platform string) (plannedPath, error) {
	p := plannedPath{entry: e, dst: e.Path}
	if py != nil {
		p.dst = NoarchTargetPath(e.Path, py.SitePackagesPath, py.BinDir)
	}
	if e.PrefixPlaceholder != "" && e.PathType != PathHardlink {
		return p, fmt.Errorf("%w: %s: prefix placeholder on a %s", ErrInvalidPackage, e.Path, e.PathType)
	}
	if e.PrefixPlaceholder != "" && e.FileMode == FileModeBinary && !isWindows(platform) && len(prefix) > len(e.PrefixPlaceholder) {
		return p, fmt.Errorf("%s: %w (%d > %d bytes)", e.Path, ErrPrefixTooLong, len(prefix), len(e.PrefixPlaceholder))
	}
	src, err := pkg.path(e.Path)
	if err != nil {
		return p, fmt.Errorf("source %s: %w", e.Path, err)
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return p, fmt.Errorf("source %s: %w", e.Path, err)
	}
	p.src, p.srcMode = src, fi.Mode()
	switch e.PathType {
	case PathHardlink:
		if !fi.Mode().IsRegular() {
			return p, fmt.Errorf("%w: %s is listed as a file but is %s", ErrInvalidPackage, e.Path, fileKind(fi.Mode()))
		}
	case PathDirectory:
		if !fi.IsDir() {
			return p, fmt.Errorf("%w: %s is listed as a directory but is %s", ErrInvalidPackage, e.Path, fileKind(fi.Mode()))
		}
	case PathSoftlink:
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, fmt.Errorf("%w: %s is listed as a symlink but is %s", ErrInvalidPackage, e.Path, fileKind(fi.Mode()))
		}
		target, err := os.Readlink(src)
		if err != nil {
			return p, fmt.Errorf("source %s: %w", e.Path, err)
		}
		if err := checkSymlinkTarget(p.dst, target); err != nil {
			return p, err
		}
		p.linkTarget = target
		if e.NoLink {
			// libmamba and rattler copy the file a no_link symlink points to.
			abs, _, err := pkg.resolve(e.Path, resolveFollow, nil, nil)
			if err != nil {
				return p, fmt.Errorf("source %s: %w", e.Path, err)
			}
			tfi, err := os.Lstat(abs)
			if err != nil {
				return p, fmt.Errorf("source %s: %w", e.Path, err)
			}
			if !tfi.Mode().IsRegular() {
				return p, fmt.Errorf("%w: no_link symlink %s does not point to a file", ErrInvalidPackage, e.Path)
			}
			p.copyFrom, p.srcMode = abs, tfi.Mode()
		}
	}
	return p, nil
}

// fileKind names the type of a file mode for error messages.
func fileKind(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "a regular file"
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSymlink != 0:
		return "a symlink"
	}
	return "a special file"
}

// checkSymlinkTarget checks a symlink target against the destination as
// written: the target must have the shape symlinkTargetShape accepts, and
// its normalised form may not climb above the prefix from the symlink's
// directory, nor point into conda-meta. checkResolved repeats the depth
// check on the symlink's physical directory.
func checkSymlinkTarget(dst, target string) error {
	ups, names, err := symlinkTargetShape(target)
	if err != nil {
		return fmt.Errorf("symlink %s: %w", dst, err)
	}
	dir := path.Dir(dst)
	depth := 0
	if dir != "." {
		depth = strings.Count(dir, "/") + 1
	}
	if ups > depth {
		return fmt.Errorf("%w: symlink %s points outside the prefix (%q)", ErrUnsafePath, dst, target)
	}
	if ups == depth && len(names) > 0 && foldName(names[0]) == "conda-meta" ||
		ups < depth && foldName(strings.SplitN(dir, "/", 2)[0]) == "conda-meta" {
		return fmt.Errorf("%w: symlink %s points into conda-meta (%q)", ErrUnsafePath, dst, target)
	}
	return nil
}

type linker struct {
	root    *root
	guard   *guard
	prefix  string
	name    string // package name, for link-script checks
	opt     Options
	signer  func(string) error
	tx      *undo
	windows bool // the target or the host is Windows
}

// mkdirAllRecorded creates dir and any missing parents, appending each
// directory it creates to *created, outermost first.
func mkdirAllRecorded(dir string, created *[]string) error {
	var missing []string
	for d := filepath.Clean(dir); ; {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		up := filepath.Dir(d)
		if up == d {
			break
		}
		d = up
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o777); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return err
		}
		*created = append(*created, missing[i])
	}
	return nil
}

// checkResolved checks a destination at its resolved location: parent is
// the resolved parent directory (components below the prefix) and leaf the
// final name. Nothing may land in conda-meta; a link-script name is only
// accepted for an allowed package's own script, as a file directly in the
// top-level bin or Scripts; bin and Scripts may not be symlinks; a symlink's
// target must stay inside the prefix when read from the symlink's physical
// directory, and must not lead into conda-meta when resolved through the
// file system and the entries planned in ov (nil when creating).
func (l *linker) checkResolved(parent []string, leaf string, kind entryKind, target string, ov overlay) error {
	full := append(parent[:len(parent):len(parent)], leaf)
	where := strings.Join(full, "/")
	if l.guard.inCondaMeta(full) {
		return fmt.Errorf("%w: resolves into conda-meta (%s)", ErrUnsafePath, where)
	}
	topScriptDir := len(parent) == 1 && isScriptDirName(parent[0])
	if !checkScriptName(l.name, l.opt.AllowScripts, leaf, kind, topScriptDir, topScriptDir || l.guard.isScriptDir(parent)) {
		return fmt.Errorf("%w: resolves to %s", ErrLinkScript, where)
	}
	if l.windows && windowsAlias(leaf) {
		return fmt.Errorf("%w: %s is a name Windows would read as another", ErrUnsafePath, where)
	}
	if kind != kindSymlink {
		return nil
	}
	if len(parent) == 0 && isScriptDirName(leaf) {
		return fmt.Errorf("%w: %s may not be a symlink", ErrUnsafePath, leaf)
	}
	ups, names, err := symlinkTargetShape(target)
	if err != nil {
		return err
	}
	if ups > len(parent) {
		return fmt.Errorf("%w: symlink target %q climbs %d levels from /%s, %d below the prefix", ErrUnsafePath, target, ups, strings.Join(parent, "/"), len(parent))
	}
	if l.windows {
		for _, n := range names {
			if windowsAlias(n) {
				return fmt.Errorf("%w: symlink target %q has a component Windows would read as another name", ErrUnsafePath, target)
			}
		}
	}
	dest := append(parent[:len(parent)-ups:len(parent)-ups], names...)
	rel := strings.Join(append(parent[:len(parent):len(parent)], target), "/")
	if _, resolved, err := l.root.resolve(rel, resolveLenient, nil, ov); err == nil {
		dest = resolved
	} else if errors.Is(err, ErrUnsafePath) {
		return fmt.Errorf("symlink target %q: %w", target, err)
	}
	if l.guard.inCondaMeta(dest) {
		return fmt.Errorf("%w: symlink target %q leads into conda-meta", ErrUnsafePath, target)
	}
	return nil
}

// planDest resolves dst through the file system and the entries planned so
// far, checks it, and records it in ov. It writes nothing.
func (l *linker) planDest(ov overlay, dst string, kind entryKind, target string) error {
	_, parent, leaf, err := l.root.parentResolved(dst, resolveCreate, nil, ov)
	if err != nil {
		return fmt.Errorf("%s: %w", dst, err)
	}
	if err := l.checkResolved(parent, leaf, kind, target, ov); err != nil {
		return fmt.Errorf("%s: %w", dst, err)
	}
	full := append(parent[:len(parent):len(parent)], leaf)
	if e, ok := ov.get(full); ok {
		if kind == kindDir && e.kind == kindDir {
			return nil
		}
		return &ExistsError{Path: dst, SamePackage: true}
	}
	fi, err := os.Lstat(l.root.abs(full))
	switch {
	case err == nil:
		if kind != kindDir || !fi.IsDir() {
			return &ExistsError{Path: dst}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s: %w", dst, err)
	}
	ov.put(full, overlayEntry{kind: kind, target: target})
	return nil
}

// place resolves the directory of dst inside the prefix, creating missing
// directories, checks the resolved destination again (the prefix may have
// changed since planning), opens the resolved directory without following
// symlinks, and runs op in it. When a concurrent Link rolls back and removes
// a directory, or a directory is replaced between resolution and opening,
// the whole step is retried; a destination is never reached through a path
// that differs from the one checked.
func (l *linker) place(dst string, kind entryKind, target string, op func(d dirHandle, leaf string) error) (string, error) {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var dir, leaf string
		var parent []string
		var made []string
		dir, parent, leaf, err = l.root.parentResolved(dst, resolveCreate, &made, nil)
		l.tx.addAll(made)
		if err != nil {
			if retryable(err) {
				continue
			}
			return "", err
		}
		if err := l.checkResolved(parent, leaf, kind, target, nil); err != nil {
			return "", err
		}
		if l.opt.beforeOpen != nil {
			l.opt.beforeOpen(dst)
		}
		var d dirHandle
		if d, err = openDir(l.root.dir, parent); err != nil {
			if retryable(err) {
				continue
			}
			return "", err
		}
		err = op(d, leaf)
		d.close()
		if err == nil || !retryable(err) {
			return filepath.Join(dir, leaf), err
		}
	}
	return "", err
}

// create opens a new file at dst for writing, refusing to replace anything.
func (l *linker) create(dst string) (*os.File, error) {
	var f *os.File
	abs, err := l.place(dst, kindFile, "", func(d dirHandle, leaf string) (err error) {
		f, err = d.create(leaf)
		return err
	})
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, &ExistsError{Path: dst}
		}
		return nil, err
	}
	l.tx.add(abs)
	return f, nil
}

// writeFile creates dst with the given contents and permission bits.
func (l *linker) writeFile(dst string, data []byte, perm fs.FileMode) (string, error) {
	f, err := l.create(dst)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(perm)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return f.Name(), werr
}

func (l *linker) linkPath(p plannedPath) (PathRecord, error) {
	e := p.entry
	r := PathRecord{Path: p.dst, PathType: e.PathType, SizeInBytes: e.SizeInBytes, NoLink: e.NoLink}
	if e.PathType != PathSoftlink {
		// libmamba's read_paths drops sha256 for symlinks.
		r.SHA256 = e.SHA256
	}
	perm := p.srcMode.Perm()

	switch {
	case e.PathType == PathDirectory:
		created := false
		abs, err := l.place(p.dst, kindDir, "", func(d dirHandle, leaf string) (err error) {
			created, err = d.mkdir(leaf)
			return err
		})
		if err != nil {
			return r, err
		}
		if created {
			l.tx.add(abs)
		}
		r.SHA256InPrefix = emptySHA256

	case e.PrefixPlaceholder != "":
		data, err := os.ReadFile(p.src)
		if err != nil {
			return r, err
		}
		var out []byte
		changed := false
		if e.FileMode == FileModeBinary {
			if isWindows(l.opt.Platform) {
				// conda and libmamba do not patch binaries on Windows.
				out = data
			} else {
				var n int
				if out, n, err = ReplaceBinaryPrefix(data, e.PrefixPlaceholder, l.prefix); err != nil {
					return r, err
				}
				changed = n > 0
			}
		} else {
			out = ReplaceTextPrefix(data, e.PrefixPlaceholder, l.prefix, l.opt.Platform)
		}
		abs, err := l.writeFile(p.dst, out, perm)
		if err != nil {
			return r, err
		}
		sum := sha256.Sum256(out)
		r.SHA256InPrefix = hex.EncodeToString(sum[:])
		if changed && l.opt.Platform == "osx-arm64" && l.signer != nil {
			err := l.signer(abs)
			l.tx.add(abs) // signing may replace the file
			if err != nil {
				return r, fmt.Errorf("re-sign: %w", err)
			}
			if r.SHA256InPrefix, err = hashFile(abs); err != nil {
				return r, err
			}
		}

	case e.PathType == PathHardlink:
		linked := false
		// A hard link shares the cached inode's mode, so files with
		// setuid, setgid or sticky bits are copied without them.
		special := p.srcMode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0
		if !e.NoLink && !l.opt.CopyOnly && !special {
			abs, err := l.place(p.dst, kindFile, "", func(d dirHandle, leaf string) error { return d.link(p.src, leaf) })
			switch {
			case err == nil:
				l.tx.add(abs)
				linked = true
			case errors.Is(err, fs.ErrExist):
				return r, &ExistsError{Path: p.dst}
			case errors.Is(err, ErrUnsafePath):
				return r, err
			}
			// Any other error (EXDEV, EPERM, EMLINK, ...) falls back to a copy.
		}
		if !linked {
			if err := l.copyFile(p.src, p.dst, perm); err != nil {
				return r, err
			}
		}
		r.SHA256InPrefix = e.SHA256
		if r.SHA256InPrefix == "" {
			abs, err := l.root.path(p.dst)
			if err != nil {
				return r, err
			}
			if r.SHA256InPrefix, err = hashFile(abs); err != nil {
				return r, err
			}
		}

	case e.PathType == PathSoftlink && p.copyFrom != "":
		if err := l.copyFile(p.copyFrom, p.dst, perm); err != nil {
			return r, err
		}

	case e.PathType == PathSoftlink:
		abs, err := l.place(p.dst, kindSymlink, p.linkTarget, func(d dirHandle, leaf string) error { return d.symlink(p.linkTarget, leaf) })
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return r, &ExistsError{Path: p.dst}
			}
			return r, err
		}
		l.tx.add(abs)
	}
	return r, nil
}

// copyFile copies the regular file src to the new file dst with perm.
func (l *linker) copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInvalidPackage, src)
	}
	out, err := l.create(dst)
	if err != nil {
		return err
	}
	_, werr := io.Copy(out, in)
	if werr == nil {
		werr = out.Chmod(perm)
	}
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// hashSymlinks fills SHA256InPrefix for symlinks once every file is in
// place: the digest of the file the link resolves to, reusing digests
// already computed for this package, or the empty digest for links that
// do not resolve to a regular file.
func (l *linker) hashSymlinks(recs []PathRecord, plan []plannedPath) error {
	byPath := make(map[string]int, len(recs))
	for i, r := range recs {
		if r.PathType != PathSoftlink {
			byPath[r.Path] = i
		}
	}
	for i := range recs {
		if recs[i].PathType != PathSoftlink {
			continue
		}
		if plan[i].copyFrom != "" {
			abs, err := l.root.path(recs[i].Path)
			if err != nil {
				return err
			}
			if recs[i].SHA256InPrefix, err = hashFile(abs); err != nil {
				return err
			}
			continue
		}
		abs, comps, err := l.root.resolve(recs[i].Path, resolveFollow, nil, nil)
		if errors.Is(err, ErrUnsafePath) {
			return fmt.Errorf("%s: %w", recs[i].Path, err)
		}
		if err != nil {
			recs[i].SHA256InPrefix = emptySHA256
			continue
		}
		if j, ok := byPath[strings.Join(comps, "/")]; ok && recs[j].SHA256InPrefix != "" && recs[j].PathType == PathHardlink {
			recs[i].SHA256InPrefix = recs[j].SHA256InPrefix
			continue
		}
		fi, err := os.Lstat(abs)
		if err != nil || !fi.Mode().IsRegular() {
			recs[i].SHA256InPrefix = emptySHA256
			continue
		}
		if recs[i].SHA256InPrefix, err = hashFile(abs); err != nil {
			return fmt.Errorf("%s: %w", recs[i].Path, err)
		}
	}
	return nil
}

// writeEntryPoint writes an executable entry point script.
func (l *linker) writeEntryPoint(dst, script string) error {
	_, err := l.writeFile(dst, []byte(script), entryPointMode)
	return err
}

func hashFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
