# WP1 — `spec`: versions, version specs, globs, build numbers, match specs

Read `AGENTS.md` first. You own `spec/` only.

## Goal

Package `spec` reproduces conda's rules for comparing versions and matching
packages, exactly. The resolver (WP7) calls it millions of times, so the hot
paths (`Version.Compare`, `VersionSpec.Contains`, `MatchSpec.MatchesVersion`)
must not allocate.

Primary reference: libmamba `src/specs/` (`version.cpp`, `version_spec.cpp`,
`glob_spec.cpp`, `build_number_spec.cpp`, `match_spec.cpp`, `platform.cpp`,
`chimera_string_spec.cpp`, `regex_spec.cpp`). Cross-check against rattler
`crates/rattler_conda_types/src/{version,version_spec,match_spec}`.

## Required API (WP7 codes against this; you may add more)

```go
type Version struct{ /* parsed, comparable */ }
func ParseVersion(s string) (Version, error)
func (v Version) Compare(o Version) int        // conda VersionOrder; -1/0/1
func (v Version) String() string               // normalized form
func (v Version) StartsWith(prefix Version) bool // for "3.11.*" and "=3.11"
func (v Version) CompatibleWith(o Version) bool  // for "~=1.2"

type VersionSpec struct{ /* tree of and/or over predicates */ }
func ParseVersionSpec(s string) (VersionSpec, error)
func (s VersionSpec) Contains(v Version) bool
func (s VersionSpec) IsAny() bool
func (s VersionSpec) String() string

type BuildNumberSpec struct{ ... }   // "3", ">=2", "!=1"
type GlobSpec struct{ ... }          // "*_cp311", "py*", exact strings
type StringMatcher = ...              // build field: glob, exact or ^regex$ (chimera)

type MatchSpec struct {
    Name        GlobSpec        // usually exact; "*" allowed
    Version     VersionSpec     // IsAny() when absent
    Build       StringMatcher
    BuildNumber BuildNumberSpec
    Channel     string          // canonical base URL or name as written; "" = any
    Subdir      string
    MD5, SHA256 string
    Fn, URL     string
    // plus any bracket keys libmamba supports (license, track_features, ...)
}
func ParseMatchSpec(s string) (MatchSpec, error)
func (m MatchSpec) String() string
// MatchesVersion tests a record whose version is already parsed (hot path).
func (m MatchSpec) MatchesVersion(r *record.PackageRecord, v Version) bool
// Matches parses r.Version itself (convenience).
func (m MatchSpec) Matches(r *record.PackageRecord) bool
```

Also `Platform` helpers: known subdirs (`linux-64`, `linux-aarch64`,
`linux-ppc64le`, `osx-64`, `osx-arm64`, `win-64`, `win-arm64`, `noarch`, ...),
`HostPlatform()` from `runtime.GOOS/GOARCH`, and `IsUnix/IsLinux/IsOSX/IsWin`.

Channels: `ParseChannel(s)` turning `conda-forge`, `conda-forge/label/x`,
`https://...` into a canonical base URL (default alias
`https://conda.anaconda.org`). Keep it minimal: no auth tokens, no `file://`
except as a URL.

## Semantics to get exactly right

Everything in libmamba's tests. In particular: epochs (`1!2.0`), local
versions (`1.0+local`), case-insensitivity, `_` and `-` as separators, special
ordering of `dev` < `_` < letters < digits < `post`, `*` and `.*` suffixes,
`=1.2` (fuzzy, starts-with) versus `==1.2`, `~=`, `!=`, `|` and `,` precedence
with parentheses, and match-spec forms such as `numpy 1.2.* py39*`,
`conda-forge::numpy>=1.2`, `numpy[version='>=1.2',build=py*]`,
`python=3.11`, `python_abi 3.11.* *_cp311`, `__glibc >=2.17,<3.0.a0`.

## Tests to port

- libmamba `tests/src/specs/`: `test_version.cpp`, `test_version_spec.cpp`,
  `test_glob_spec.cpp`, `test_build_number_spec.cpp`, `test_match_spec.cpp`
  (108 cases), `test_platform.cpp`, `test_chimera_string_spec.cpp`,
  `test_regex_spec.cpp`; the parts of `test_channel.cpp` that apply.
- rattler `test-data/parsed_versions.txt` (28,557 lines of versions and how
  conda parsed them): a test that parses every line; read its header for the
  format. Check that sorting a representative sample matches.
- Real-world: every `depends` and `constrains` string in
  `$GOOMBA_REF_ROOT/envs/base-3.11/conda-meta/*.json` parses.

Fuzz targets: `FuzzParseVersion` (no panic; `ParseVersion(v.String())`
compares equal to `v`), `FuzzVersionOrder` (antisymmetry and transitivity on
three parsed inputs), `FuzzParseVersionSpec`, `FuzzParseMatchSpec`.

Add benchmarks for `Compare`, `Contains` and `MatchesVersion` and report
ns/op and allocs/op.
