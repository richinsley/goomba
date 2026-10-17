package lock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/virtual"
)

// Version is the lockfile format version this package reads and writes.
const Version = 1

// maxInput bounds how much Read, ReadBundle and ReadExplicit consume.
const maxInput = 64 << 20

// Sentinel errors returned (wrapped) by this package.
var (
	// ErrInvalid reports a lockfile, bundle or explicit file that is
	// malformed or fails validation. Every error from Read, ReadBundle,
	// ReadExplicit and Validate that is not an I/O error wraps it.
	ErrInvalid = errors.New("invalid lockfile")
	// ErrUnsupportedVersion reports a lockfile whose "version" is not
	// Version. It is returned together with ErrInvalid.
	ErrUnsupportedVersion = errors.New("unsupported lockfile version")
	// ErrNotInIndex reports an explicit-file entry the index does not know.
	ErrNotInIndex = errors.New("package not in index")
	// ErrHashMismatch reports an explicit-file hash that differs from the
	// index.
	ErrHashMismatch = errors.New("hash mismatch")
)

// Lockfile is a fully resolved environment for one platform: the only input
// the installer trusts. Its JSON form is
//
//	{
//	  "version": 1,
//	  "platform": "linux-64",
//	  "channels": ["https://conda.anaconda.org/conda-forge"],
//	  "specs": ["python=3.11"],
//	  "virtual": {"__glibc": "2.17", "__linux": "0", "__unix": "0"},
//	  "packages": [ {Package}, ... ]
//	}
//
// A Lockfile is a plain value: it is not safe for concurrent mutation, and
// concurrent reads are safe.
type Lockfile struct {
	// Version is the format version; Validate accepts only Version.
	Version int `json:"version"`
	// Platform is the subdir the environment is for, e.g. "linux-64".
	Platform string `json:"platform"`
	// Channels are the channel base URLs the packages were resolved from,
	// in priority order, e.g. "https://conda.anaconda.org/conda-forge".
	// Every package URL lies under one of them.
	Channels []string `json:"channels"`
	// Specs are the match specs the environment was solved for. They are
	// informational: installing never re-solves.
	Specs []string `json:"specs"`
	// Virtual records the virtual packages assumed by the solve, as
	// virtual.ToOverrides writes them: name to version, except "__archspec",
	// which maps to the microarchitecture (its build string). Turn it back
	// into records with virtual.ApplyOverrides(Platform, nil, Virtual), which
	// reproduces the set exactly; passing it to virtual.Detect as overrides
	// would add the host's own packages.
	Virtual map[string]string `json:"virtual"`
	// Packages are the packages to install, one per name.
	Packages []Package `json:"packages"`
}

// Package is one locked package archive. A Package is a plain value with
// the same concurrency rules as Lockfile.
type Package struct {
	// Name is the package name, e.g. "python".
	Name string `json:"name"`
	// Version is the conda version string, e.g. "3.11.15".
	Version string `json:"version"`
	// Build is the build string, e.g. "hd63d673_0_cpython".
	Build string `json:"build"`
	// BuildNumber is the build number from the package metadata.
	BuildNumber uint64 `json:"build_number"`
	// Subdir is the lockfile's platform or "noarch".
	Subdir string `json:"subdir"`
	// Noarch is "", "generic" or "python".
	Noarch string `json:"noarch"`
	// URL is the https download URL
	// <channel>/<subdir>/<name>-<version>-<build><ext>.
	URL string `json:"url"`
	// SHA256 is the lowercase hex sha256 of the archive; always set.
	SHA256 string `json:"sha256"`
	// MD5 is the lowercase hex md5 of the archive, or "".
	MD5 string `json:"md5"`
	// Size is the archive size in bytes, or 0 if unknown.
	Size int64 `json:"size"`
	// Depends are the package's match specs, kept for re-solving and
	// install ordering; the installer does not re-check them.
	Depends []string `json:"depends"`
	// Constrains are match specs that apply only to packages that are also
	// installed.
	Constrains []string `json:"constrains"`
	// PythonSitePackagesPath is "" or a clean relative path such as
	// "lib/python3.13t/site-packages".
	PythonSitePackagesPath string `json:"python_site_packages_path"`
}

// Read parses and validates a lockfile. Parsing is strict: unknown, duplicate
// or wrongly-cased keys, null values, values of the wrong type, invalid
// UTF-8 and trailing data are errors, and the result must pass Validate.
// Input beyond 64 MiB is rejected.
func Read(r io.Reader) (*Lockfile, error) {
	data, err := readAllLimited(r)
	if err != nil {
		return nil, err
	}
	d, err := newStrictDecoder(data)
	if err != nil {
		return nil, err
	}
	l, err := decodeLockfile(d, "lockfile")
	if err != nil {
		return nil, err
	}
	if err := d.end(); err != nil {
		return nil, err
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

func readAllLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return nil, fmt.Errorf("lock: read: %w", err)
	}
	if len(data) > maxInput {
		return nil, fmt.Errorf("lock: input larger than %d bytes: %w", maxInput, ErrInvalid)
	}
	return data, nil
}

// Write validates l and writes it as canonical JSON: packages sorted by
// name, keys in a fixed order, 2-space indent, no HTML escaping, empty lists
// as [] and a trailing newline. Equal lockfiles produce identical bytes. l
// is not modified.
func (l *Lockfile) Write(w io.Writer) error {
	if err := l.Validate(); err != nil {
		return err
	}
	return writeJSON(w, l.canonical())
}

func writeJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("lock: encode: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("lock: write: %w", err)
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// canonical returns a copy of l in Write's form: packages sorted by name and
// no nil slices or maps, so that empty lists encode as [] and {}.
func (l *Lockfile) canonical() *Lockfile {
	c := &Lockfile{
		Version:  l.Version,
		Platform: l.Platform,
		Channels: nonNil(l.Channels),
		Specs:    nonNil(l.Specs),
		Virtual:  l.Virtual,
		Packages: make([]Package, len(l.Packages)),
	}
	if c.Virtual == nil {
		c.Virtual = map[string]string{}
	}
	for i, p := range l.Packages {
		p.Depends = nonNil(p.Depends)
		p.Constrains = nonNil(p.Constrains)
		c.Packages[i] = p
	}
	sort.SliceStable(c.Packages, func(i, j int) bool { return c.Packages[i].Name < c.Packages[j].Name })
	return c
}

// invalidf returns an error wrapping ErrInvalid with a "lock: " prefix.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("lock: "+format+": %w", append(args, ErrInvalid)...)
}

// Validate checks everything the installer relies on and returns the first
// problem found, wrapping ErrInvalid:
//   - version is Version (otherwise ErrUnsupportedVersion too) and platform
//     is a known conda platform other than noarch;
//   - channels are canonical https URLs without a trailing slash, with no
//     duplicates; specs are non-empty one-line strings;
//   - virtual is a recorded set that virtual.CheckRecorded accepts for the
//     platform: full lower-case names, non-empty versions (a lower-case build
//     string for __archspec), only packages the platform can carry, and the
//     virtual package's length limits;
//   - package names are valid and unique (ignoring case) and not virtual;
//     versions and builds are well-formed; subdir is the platform or
//     "noarch"; noarch is "", "generic" or "python" (in any subdir: conda
//     channels hold noarch: python packages under linux-64 and packages
//     without noarch under noarch, and the installer takes the linking
//     mode from the archive's own metadata);
//   - url is canonical https <channel>/<subdir>/<name>-<version>-<build>
//     followed by .conda or .tar.bz2, its subdir is the package's subdir and
//     its channel is listed in channels;
//   - sha256 is 64 lowercase hex digits; md5 is empty or 32 lowercase hex
//     digits; size is not negative;
//   - depends and constrains entries are non-empty one-line strings, and
//     python_site_packages_path is empty or a clean relative path.
func (l *Lockfile) Validate() error {
	if l == nil {
		return invalidf("nil lockfile")
	}
	if l.Version != Version {
		return fmt.Errorf("lock: version %d (want %d): %w: %w", l.Version, Version, ErrUnsupportedVersion, ErrInvalid)
	}
	if !knownPlatform(l.Platform) {
		return invalidf("platform %q is not a conda platform", l.Platform)
	}
	channels := make(map[string]bool, len(l.Channels))
	for _, c := range l.Channels {
		if err := checkChannel(c); err != nil {
			return invalidf("channel %q: %v", redactURL(c), err)
		}
		if channels[c] {
			return invalidf("channel %q listed twice", redactURL(c))
		}
		channels[c] = true
	}
	for _, s := range l.Specs {
		if err := checkText(s, maxSpecLen); err != nil {
			return invalidf("spec %q: %v", s, err)
		}
	}
	if err := virtual.CheckRecorded(l.Platform, l.Virtual); err != nil {
		return invalidf("%v", err)
	}
	seen := make(map[string]string, len(l.Packages))
	for i := range l.Packages {
		p := &l.Packages[i]
		if err := p.validate(l.Platform, channels); err != nil {
			return err
		}
		key := strings.ToLower(p.Name)
		if prev, dup := seen[key]; dup {
			return invalidf("package %q: name also used by %q", p.Name, prev)
		}
		seen[key] = p.Name
	}
	return nil
}

// validate checks one package of a lockfile for platform whose channels are
// listed in channels.
func (p *Package) validate(platform string, channels map[string]bool) error {
	if err := checkPackageName(p.Name); err != nil {
		return invalidf("package %q: %v", p.Name, err)
	}
	if err := checkVersion(p.Version); err != nil {
		return invalidf("package %q: version %q: %v", p.Name, p.Version, err)
	}
	if err := checkBuild(p.Build); err != nil {
		return invalidf("package %q: build %q: %v", p.Name, p.Build, err)
	}
	if p.Subdir != platform && p.Subdir != "noarch" {
		return invalidf("package %q: subdir %q is neither %q nor \"noarch\"", p.Name, p.Subdir, platform)
	}
	switch p.Noarch {
	case "", "generic", "python":
	default:
		return invalidf("package %q: noarch %q is not \"\", \"generic\" or \"python\"", p.Name, p.Noarch)
	}
	channel, subdir, fn, err := splitArchiveURL(p.URL)
	if err != nil {
		return invalidf("package %q: url %q: %v", p.Name, redactURL(p.URL), err)
	}
	ext := archiveExt(fn)
	if ext == "" {
		return invalidf("package %q: url %q: file is not .conda or .tar.bz2", p.Name, redactURL(p.URL))
	}
	if want := p.Name + "-" + p.Version + "-" + p.Build + ext; fn != want {
		return invalidf("package %q: url %q: file name is not %q", p.Name, redactURL(p.URL), want)
	}
	if subdir != p.Subdir {
		return invalidf("package %q: url %q: subdir %q is not %q", p.Name, redactURL(p.URL), subdir, p.Subdir)
	}
	if !channels[channel] {
		return invalidf("package %q: url %q: channel %q is not listed in channels", p.Name, redactURL(p.URL), redactURL(channel))
	}
	if !isLowerHex(p.SHA256, 64) {
		return invalidf("package %q: sha256 %q is not 64 lowercase hex digits", p.Name, p.SHA256)
	}
	if p.MD5 != "" && !isLowerHex(p.MD5, 32) {
		return invalidf("package %q: md5 %q is not 32 lowercase hex digits", p.Name, p.MD5)
	}
	if p.Size < 0 {
		return invalidf("package %q: size %d is negative", p.Name, p.Size)
	}
	if len(p.Depends) > maxListItems || len(p.Constrains) > maxListItems {
		return invalidf("package %q: too many depends or constrains", p.Name)
	}
	for _, d := range p.Depends {
		if err := checkText(d, maxSpecLen); err != nil {
			return invalidf("package %q: depends %q: %v", p.Name, d, err)
		}
	}
	for _, c := range p.Constrains {
		if err := checkText(c, maxSpecLen); err != nil {
			return invalidf("package %q: constrains %q: %v", p.Name, c, err)
		}
	}
	if err := checkSitePackagesPath(p.PythonSitePackagesPath); err != nil {
		return invalidf("package %q: python_site_packages_path %q: %v", p.Name, p.PythonSitePackagesPath, err)
	}
	return nil
}

// Records converts the packages to fresh PackageRecords in l.Packages order,
// filling Fn and Channel from the URL. Call it only on a validated
// lockfile; for a package whose URL does not split, Fn and Channel are
// left empty.
func (l *Lockfile) Records() []*record.PackageRecord {
	out := make([]*record.PackageRecord, len(l.Packages))
	for i, p := range l.Packages {
		channel, _, fn, err := splitArchiveURL(p.URL)
		if err != nil {
			channel, fn = "", ""
		}
		out[i] = &record.PackageRecord{
			Name:                   p.Name,
			Version:                p.Version,
			Build:                  p.Build,
			BuildNumber:            p.BuildNumber,
			Depends:                cloneStrings(p.Depends),
			Constrains:             cloneStrings(p.Constrains),
			Noarch:                 p.Noarch,
			Subdir:                 p.Subdir,
			MD5:                    p.MD5,
			SHA256:                 p.SHA256,
			Size:                   p.Size,
			PythonSitePackagesPath: p.PythonSitePackagesPath,
			Fn:                     fn,
			URL:                    p.URL,
			Channel:                channel,
		}
	}
	return out
}

func cloneStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return append([]string(nil), s...)
}

// FromRecords builds and validates a lockfile from resolved records.
// Virtual packages in recs (names starting with "__") are skipped: they
// belong in virt (see Lockfile.Virtual). Hash digests are lower-cased,
// channel and package URLs get a canonical authority (lower-case scheme and
// host, no trailing dot, no explicit :443), channel URLs lose a trailing
// slash, and packages are sorted by name. A record whose Fn
// is set must match its URL's file name. The inputs are not modified or
// retained.
func FromRecords(platform string, channels, specs []string, virt map[string]string, recs []*record.PackageRecord) (*Lockfile, error) {
	l := &Lockfile{
		Version:  Version,
		Platform: platform,
		Channels: make([]string, 0, len(channels)),
		Specs:    cloneStrings(specs),
		Virtual:  make(map[string]string, len(virt)),
		Packages: make([]Package, 0, len(recs)),
	}
	for _, c := range channels {
		l.Channels = append(l.Channels, canonicalHost(strings.TrimRight(c, "/")))
	}
	for k, v := range virt {
		l.Virtual[k] = v
	}
	for i, r := range recs {
		if r == nil {
			return nil, invalidf("record %d is nil", i)
		}
		if r.IsVirtual() {
			continue
		}
		if r.Fn != "" {
			if _, _, fn, err := splitArchiveURL(r.URL); err == nil && fn != r.Fn {
				return nil, invalidf("package %q: fn %q does not match url %q", r.Name, r.Fn, redactURL(r.URL))
			}
		}
		l.Packages = append(l.Packages, packageFromRecord(r))
	}
	sort.SliceStable(l.Packages, func(i, j int) bool { return l.Packages[i].Name < l.Packages[j].Name })
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

func packageFromRecord(r *record.PackageRecord) Package {
	return Package{
		Name:                   r.Name,
		Version:                r.Version,
		Build:                  r.Build,
		BuildNumber:            r.BuildNumber,
		Subdir:                 r.Subdir,
		Noarch:                 r.Noarch,
		URL:                    canonicalHost(r.URL),
		SHA256:                 strings.ToLower(r.SHA256),
		MD5:                    strings.ToLower(r.MD5),
		Size:                   r.Size,
		Depends:                cloneStrings(r.Depends),
		Constrains:             cloneStrings(r.Constrains),
		PythonSitePackagesPath: r.PythonSitePackagesPath,
	}
}
