# WP4 — `link`: install one unpacked package into a prefix

Read `AGENTS.md` first. You own `link/`.

## Goal

Given an extracted package directory and its record, create exactly the files
micromamba would create in the prefix, then write the `conda-meta` record.
The result must match micromamba's environment file for file, except for the
install path itself.

Primary reference: libmamba `src/core/link.cpp` (whole file; entry points at
lines ~60–440, prefix replacement ~990–1150, codesign call ~1149, noarch
python ~1290–1510) and `src/core/util_os.cpp` (codesign ~771). Cross-check with
rattler `crates/rattler/src/install/` (`link.rs`, `entry_point.rs`,
`python.rs`, `apple_codesign.rs`, `prefix_replacement/`, `unlink.rs`,
`clobber_registry.rs`).

## Required API

```go
type PythonInfo struct {
    Version          string // "3.11" (major.minor)
    SitePackagesPath string // "lib/python3.11/site-packages" or the record's python_site_packages_path
    BinDir           string // "bin" (unix) or "Scripts" (win)
    Executable       string // "bin/python3.11" relative to prefix
}
type Options struct {
    Prefix          string      // absolute install path, written into files
    Platform        string      // target subdir, e.g. "linux-64"
    Python          *PythonInfo // required for noarch: python packages
    AllowScripts    []string    // package names whose link scripts may exist; others are refused
    RequestedSpec   string      // stored in conda-meta
    Signer          func(path string) error // osx-arm64 re-signing; nil = default (/usr/bin/codesign -s - -f) on darwin, none elsewhere
    CopyOnly        bool        // never hard link
}
type Installed struct { /* files written, paths_data, link info */ }
func Link(ctx context.Context, pkgDir string, rec *record.PackageRecord, opt Options) (*Installed, error)
func Unlink(ctx context.Context, prefix string, rec *record.PackageRecord) error
func WriteCondaMeta(prefix string, rec *record.PackageRecord, inst *Installed, opt Options) error
func ReadCondaMeta(prefix string) ([]*CondaMetaRecord, error) // record + files, sorted by name
func AppendHistory(prefix string, cmd string, added, removed []*record.PackageRecord, now time.Time) error
```

`os/exec` is allowed only for `/usr/bin/codesign` inside the default signer.

## Behaviour

- Read `info/paths.json` (version 1: `_path`, `path_type`
  (`hardlink`/`softlink`/`directory`), `file_mode` (`text`/`binary`),
  `prefix_placeholder`, `sha256`, `size_in_bytes`, `no_link`). Fall back to
  `info/files` + `info/has_prefix` + `info/no_link` for old packages, as
  libmamba does.
- Every destination must stay inside the prefix (port
  `check_path_within_prefix`). An existing destination file is an error
  (the transaction layer decides about clobbering).
- Hard link from `pkgDir` when nothing needs rewriting; fall back to copy on
  `EXDEV`/`EPERM`/`CopyOnly`; `no_link` files are copied; softlinks are
  recreated as symlinks with the same target.
- Prefix rewriting (files with `prefix_placeholder`), exactly as libmamba:
  text mode replaces every occurrence; binary mode replaces inside each
  NUL-terminated string and pads with NULs to keep the length; refuse when the
  prefix is longer than the placeholder. Shebang handling: libmamba
  `replace_long_shebang` (> 127 chars → `#!/usr/bin/env <name><args>`).
  Preserve the file mode. On osx-arm64 targets call `Signer` on every
  rewritten binary.
- `noarch: python` (from `info/link.json` and `info/index.json`): files under
  `site-packages/` go to `Python.SitePackagesPath`, `python-scripts/` to
  `Python.BinDir`. Entry points `name = module:func`: validate with the
  ports of `check_entry_point_command` / `check_python_identifier_chain`
  before writing anything; write libmamba's template exactly, behind
  `python_shebang` (`#!<prefix>/<Executable>`, or the `/bin/sh` trampoline when
  longer than 125 chars or containing spaces); mode 0755. The real result to
  match is `$GOOMBA_REF_ROOT/envs/base-3.11/bin/pip`.
- Link scripts (`bin/.<name>-pre-link.sh`, `-post-link.sh`, `-pre-unlink.sh`,
  Windows `.bat`) are never run. A package that contains one is refused with
  `ErrLinkScript` unless its name is in `AllowScripts`; then the script file
  is still not run, only installed.
- `conda-meta/<dist>.json`: match the fields and JSON types micromamba writes;
  inspect `$GOOMBA_REF_ROOT/envs/base-3.11/conda-meta/*.json` (for
  example `track_features` is a string there, `timestamp` is in seconds,
  `paths_data` has `paths_version: 1`). Paths in `files` use the prefix-relative
  form micromamba uses.

## Tests

- For each of the 28 extracted packages in `$GOOMBA_REF_ROOT/pkgs/`
  that belong to `envs/base-3.11` (read their names from its `conda-meta`), link
  into a temp prefix (`Python` from the python record) and check: the file set
  equals micromamba's for that package; text files equal micromamba's after
  replacing micromamba's prefix with yours; binaries keep their length and
  still parse as ELF (`debug/elf`); no placeholder string remains; symlink
  targets equal; entry points equal byte for byte after the prefix swap; your
  `conda-meta` JSON has the same keys and types as micromamba's. Read the
  micromamba environment, never write to it.
- rattler `test-data/link-json/*`, `test-data/shebang_test.txt`,
  `test-data/link-scripts` (must be refused), and the cases in rattler
  `install/` tests that apply.
- Prefixes: short, 200 characters, with spaces, longer than the placeholder
  (must fail cleanly and leave nothing behind for that package).
- `Unlink` removes exactly what `Link` created, including now-empty
  directories, and the `conda-meta` file.

Fuzz: `FuzzParsePathsJSON`, `FuzzParseEntryPoint`, `FuzzReplaceBinaryPrefix`
(the output length always equals the input length).
