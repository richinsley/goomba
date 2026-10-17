# WP5 — `virtual` and `lock`: machine facts and lockfiles

Read `AGENTS.md` first. You own `virtual/` and `lock/`.

## `virtual`

Virtual packages are `record.PackageRecord`s with `Channel: "@"`, no URL and no
hashes, e.g. `__glibc 2.35 0`, `__linux 6.8.0 0`, `__unix 0 0`,
`__archspec 1 x86_64_v3`, `__osx 14.5 0`, `__win 0 0`, `__cuda 12.4 0`.

References: libmamba `src/core/virtual_packages.cpp`; rattler
`crates/rattler_virtual_packages/src/` (`libc.rs`, `linux.rs`, `osx.rs`,
`win.rs`, `cuda.rs`, `lib.rs`).

```go
type Options struct {
    Platform  string            // target subdir; "" = host
    Overrides map[string]string // name → version; "" removes the package
}
// Detect returns the virtual packages of this machine for opt.Platform
// (which must be the host platform), with Overrides applied.
func Detect(ctx context.Context, opt Options) ([]*record.PackageRecord, error)
// Defaults returns conservative virtual packages for any platform, for
// building lockfiles for platforms other than the host.
func Defaults(platform string) []*record.PackageRecord
```

Detection without cgo:
- `__linux`: kernel release via `golang.org/x/sys/unix.Uname`, trimmed to the
  numeric part.
- `__glibc`: read the `GLIBC_x.y` version definitions from the system
  `libc.so.6` with `debug/elf` (find it via `/lib*/`, `/usr/lib*/`
  multiarch dirs); fall back to `getconf GNU_LIBC_VERSION` (the only
  `os/exec` allowed here). musl systems get no `__glibc`.
- `__osx`: `unix.Sysctl("kern.osproductversion")`.
- `__archspec`: version `1`, build = microarchitecture name, from
  `golang.org/x/sys/cpu` features, using rattler's / archspec's naming
  (`x86_64`, `x86_64_v2`, `x86_64_v3`, `x86_64_v4`, `aarch64`, `arm64`, ...).
- `__cuda`: only if `Overrides` sets it (detection is a later phase).
- `__unix` / `__win` from the platform.
Document the defaults you choose in `Defaults` and why (pixi's defaults are a
good guide: linux `__glibc 2.28`, `__linux 4.18`; check rattler).

Tests: build strings and versions for each platform; overrides add, change and
remove; `Detect` on this Linux machine returns `__glibc`, `__linux`, `__unix`,
`__archspec` with plausible values (compare `getconf GNU_LIBC_VERSION`).

## `lock`

The lockfile is the only input the installer trusts. Design from the doc:

```json
{
  "version": 1,
  "platform": "linux-64",
  "channels": ["https://conda.anaconda.org/conda-forge"],
  "specs": ["python=3.11"],
  "virtual": {"__glibc": "2.17", "__unix": "0", "__linux": "0"},
  "packages": [
    {"name": "python", "version": "3.11.15", "build": "hd63d673_0_cpython",
     "build_number": 0, "subdir": "linux-64", "noarch": "",
     "url": "https://conda.anaconda.org/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda",
     "sha256": "bf6a32c69889d38482436a786bea32276756cedf0e9805cc856ffd088e8d00f0",
     "md5": "a5ebcefec0c12a333bcd6d7bf3bddc1f", "size": 30949404,
     "depends": ["..."], "constrains": ["python_abi 3.11.* *_cp311"],
     "python_site_packages_path": ""}
  ]
}
```

```go
type Lockfile struct { ... }          // the JSON above
type Package struct { ... }
type Bundle struct { Platforms map[string]*Lockfile }
func Read(r io.Reader) (*Lockfile, error)        // strict: unknown fields are errors
func (l *Lockfile) Write(w io.Writer) error       // deterministic: packages sorted by name, 2-space indent, trailing newline
func (l *Lockfile) Validate() error               // unique names; url https; sha256 64 lowercase hex; md5 32 hex if set; subdir is platform or noarch; url ends with the record's fn
func (l *Lockfile) Records() []*record.PackageRecord
func FromRecords(platform string, channels, specs []string, virtual map[string]string, recs []*record.PackageRecord) (*Lockfile, error)
func ReadBundle(r io.Reader) (*Bundle, error); func (b *Bundle) Write(w io.Writer) error
// @EXPLICIT interop
func (l *Lockfile) WriteExplicit(w io.Writer) error  // "# platform: <p>\n@EXPLICIT\n<url>#<md5>\n..." sorted like conda
func ReadExplicit(r io.Reader) (*Explicit, error)    // entries: url plus md5 or sha256 (`#sha256:<hex>`)
func (e *Explicit) Resolve(lookup func(url string) (*record.PackageRecord, bool)) (*Lockfile, error) // fill sha256 from an index; md5 must match
// Existing environments
func FromPrefix(prefix, platform string) (*Lockfile, error) // reads conda-meta/*.json (url, sha256, md5, size, depends, constrains, ...)
```

`FromPrefix` needs only a small private conda-meta reader (package `link`
owns the full codec; WP8 will merge them). Test it on
`$GOOMBA_REF_ROOT/envs/base-3.11` (read-only): 28 packages, all with
https URLs and sha256.

Tests: round-trip (read → write → read equal, byte-identical writes);
`Validate` rejects each kind of bad entry; explicit files from rattler
`test-data/explicit-envs/*` parse; `WriteExplicit` output installs with
`micromamba create --dry-run -f` when `GOOMBA_MICROMAMBA` is set (scratch root,
explicit `MAMBA_ROOT_PREFIX`, `--offline` is fine to skip).

Fuzz: `FuzzReadLockfile`, `FuzzReadExplicit`.
