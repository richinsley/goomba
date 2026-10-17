// Package record defines PackageRecord, the one type every goomba package
// shares. It lives alone so that packages built in parallel never depend on
// each other's in-progress code.
//
// PackageRecord is goomba's in-memory form of a conda package's metadata. It
// is not a wire format: repodata, shards, conda-meta and lockfiles each have
// their own codec in the package that owns that format.
package record

import "strconv"

// PackageRecord describes one conda package build.
type PackageRecord struct {
	// Name is the package name, e.g. "python".
	Name string
	// Version is the raw conda version string, e.g. "3.11.15". Parse it with
	// package spec; never compare raw strings.
	Version string
	// Build is the build string, e.g. "hd63d673_0_cpython".
	Build string
	// BuildNumber is the build number, e.g. 0.
	BuildNumber uint64

	// Depends are match specs this package requires, e.g. "openssl >=3.5,<4.0a0".
	Depends []string
	// Constrains are match specs that apply only if the named package is also
	// installed, e.g. "python_abi 3.11.* *_cp311".
	Constrains []string
	// TrackFeatures lowers this record's priority in the solver. Repodata
	// stores it as a comma- or space-separated string or as a list; codecs
	// normalize it to a list with no empty entries.
	TrackFeatures []string
	// Features is the legacy "features" field, kept verbatim.
	Features string

	// Noarch is "", "generic" or "python". Codecs map the legacy boolean
	// noarch=true to "generic".
	Noarch string
	// Subdir is the platform subdirectory, e.g. "linux-64" or "noarch".
	Subdir string
	// Timestamp is the build time in milliseconds since the Unix epoch.
	// Codecs normalize second-based timestamps with NormalizeTimestamp.
	Timestamp int64
	// License is the SPDX-ish license string, informational only.
	License string

	// MD5 and SHA256 are lowercase hex digests of the package archive.
	MD5    string
	SHA256 string
	// Size is the archive size in bytes, or 0 if unknown.
	Size int64

	// PythonSitePackagesPath is set on python records whose site-packages
	// directory is not lib/pythonX.Y/site-packages (e.g. free-threaded
	// builds use lib/python3.13t/site-packages).
	PythonSitePackagesPath string

	// Fn is the archive file name, e.g. "python-3.11.15-hd63d673_0_cpython.conda".
	Fn string
	// URL is the absolute download URL of the archive.
	URL string
	// Channel is the canonical channel base URL without the subdir, e.g.
	// "https://conda.anaconda.org/conda-forge". Virtual packages use "@".
	Channel string
}

// Dist returns "name-version-build", the conventional conda distribution
// name used for package directories and conda-meta file names.
func (r *PackageRecord) Dist() string {
	return r.Name + "-" + r.Version + "-" + r.Build
}

// String returns "name version build" for logs and messages.
func (r *PackageRecord) String() string {
	return r.Name + " " + r.Version + " " + r.Build
}

// IsVirtual reports whether the record is a virtual package such as __glibc.
func (r *PackageRecord) IsVirtual() bool {
	return len(r.Name) > 2 && r.Name[0] == '_' && r.Name[1] == '_'
}

// BuildNumberString returns the build number in decimal.
func (r *PackageRecord) BuildNumberString() string {
	return strconv.FormatUint(r.BuildNumber, 10)
}

// NormalizeTimestamp converts a repodata timestamp to milliseconds. Conda
// repodata mixes seconds and milliseconds; any value small enough to be a
// date in seconds before the year 9999 is treated as seconds.
func NormalizeTimestamp(ts int64) int64 {
	const maxSeconds = 253402300799 // 9999-12-31T23:59:59Z
	if ts > 0 && ts <= maxSeconds {
		return ts * 1000
	}
	return ts
}
