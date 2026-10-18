# WP9 — jumpboot: goomba as a second environment backend

You work in the **jumpboot** repository (module `github.com/richinsley/jumpboot`),
on the branch and worktree named in your prompt, not in goomba. Read goomba's
`AGENTS.md` for the working rules; they apply here too (no machine paths in
anything committed, resource limits, one heavy run at a time). Do not edit
goomba. If you need something from it, report it.

## Goal

jumpboot creates conda environments by downloading a micromamba binary and
running `micromamba create`. Add goomba, called as a Go library, as a second
backend that callers choose explicitly. **micromamba stays.**
`CreateEnvironmentMamba` and every other existing function keep their
signatures and behaviour. Programs that never call the goomba functions must
behave exactly as before.

The consumers are poppi and jb-mesh. They call
`CreateEnvironmentMamba(name, envHome, python, "conda-forge", cb)` to build
`base-<python>`, then use venv and pip on top of it. poppi currently tells a
micromamba environment apart by `env.MicromambaPath != ""`.

## API

```go
// Backend says which tool manages a conda environment.
type Backend string

const (
    BackendNone       Backend = ""           // system Python, an executable, a venv
    BackendMicromamba Backend = "micromamba"
    BackendGoomba     Backend = "goomba"
)

// BaseEnvironment gains:
//     Backend Backend
// CreateEnvironmentMamba sets BackendMicromamba; this is the one change to it.

// GoombaOptions configures environment creation with goomba.
type GoombaOptions struct {
    PythonVersion    string            // "" = "3.10", as CreateEnvironmentMamba
    Channels         []string          // strict priority; nil = ["conda-forge"]
    Specs            []string          // extra match specs, solved together with python
    ChannelAlias     string            // "" = https://conda.anaconda.org
    Timeout          time.Duration     // resolve + install; 0 = 10 minutes
    Client           *http.Client      // nil = goomba's default (no proxy from the environment)
    VirtualOverrides map[string]string // CONDA_OVERRIDE_* names: "cuda", "glibc", ...
    NvidiaSMI        string            // absolute path; "" = goomba's built-in locations
    NoPyc            bool              // skip byte-compiling noarch: python packages
}

// Same arguments, layout and reuse rule as CreateEnvironmentMamba.
func CreateEnvironmentGoomba(envName, rootDir, pythonVersion, channel string,
    progressCallback ProgressCallback) (*PythonEnvironment, error)

func CreateEnvironmentGoombaWithOptions(ctx context.Context, envName, rootDir string,
    opts GoombaOptions, progressCallback ProgressCallback) (*PythonEnvironment, error)

// JSON restore with goomba (see below).
func CreateEnvironmentFromJSONFileGoomba(ctx context.Context, filePath, rootDir string,
    ropts RestoreOptions, gopts GoombaOptions,
    progressCallback ProgressCallback) (*PythonEnvironment, error)
```

Adjust names if the code suggests better ones, and report the final API.
`ctx` is the caller's. `Timeout` applies on top of it.

## Behaviour

- **Layout.** The environment is `rootDir/envs/NAME`, as with micromamba, and
  every path field of `PythonEnvironment` is filled exactly as
  `CreateEnvironmentMamba` fills it. Move that code into a shared helper
  rather than copying it. A goomba environment and a micromamba environment
  of the same name are interchangeable.
- **Caches.** Use `rootDir/goomba/pkgs` (`InstallOptions.PkgsDir`) and
  `rootDir/goomba/repodata` (`LockOptions.RepodataDir`). Never share
  micromamba's `rootDir/pkgs`, because the layouts differ. goomba refuses
  cache directories that other users can write (`fetch.ErrUnsafeCache`,
  `repodata.ErrUnsafeCache`). Return those errors wrapped, and create the
  directories 0700.
- **Specs.**
  - `python=<PythonVersion>` plus `opts.Specs`.
  - Also `pip`, unless a spec already names pip. micromamba adds pip to
    python by default (`add_pip_as_python_dependency`). goomba never does,
    and jumpboot requires pip.
- **Reuse.** If `rootDir/envs/NAME` exists, reuse it (`IsNew` false), as
  `CreateEnvironmentMamba` does, with one exception: if
  `conda-meta/.goomba-incomplete` exists, return an error and remove
  nothing. That marker means a goomba install was interrupted without
  rollback.
- **Create.** Call `goomba.Create`. Map goomba's `Event`s to
  `ProgressCallback`: downloads with byte counts, then linking, with -1 when
  the total is unknown. Then run the checks `CreateEnvironmentMamba` runs:
  python and pip versions, and python at least the requested version.
- **Errors.**
  - `*solve.Unsolvable` reaches the caller through `errors.As`. Its
    explanation is already safe to print.
  - So do `goomba.ErrPrefixLocked` (a concurrent create of the same
    environment) and the cache errors.
- **No micromamba.** The goomba functions never download micromamba, never
  run it, and never read `MAMBA_ROOT_PREFIX` or any `.condarc`. A goomba
  environment has an empty `MicromambaPath`.
- **Existing methods on a goomba environment.**
  - `MicromambaInstallPackage` returns a clear error. goomba does not
    install into an existing environment. Use JSON restore or recreate.
  - `FreezeToFile` records the conda packages from `conda-meta`
    (`lock.FromPrefix`) instead of running `micromamba list`. Write
    `Packages` with `Source: "conda"`, version, build and `sha256`, plus
    `Channels`. Keep pip freeze as it is.
- **Environment variables.** jumpboot passes `os.Environ()` to micromamba,
  so proxies reach it. goomba reads no environment, so the goomba path uses
  a proxy only through `GoombaOptions.Client`. Document this.

## JSON restore with goomba

The micromamba restore creates the base environment, then installs each
conda package separately, trying each channel in turn. goomba cannot install
into an existing environment, so it instead resolves python, pip and every
conda package of the spec **in one solve** at create time. That is more
correct, because the packages are solved together.

- **Channels:** the spec's `Channels`, else `gopts.Channels`, else
  conda-forge, all in strict priority. Document that the micromamba restore
  falls back from one channel to the next for each package, and goomba does
  not.
- **Python version:** the spec's `PythonVersion` overrides
  `gopts.PythonVersion`. The name comes from the spec.
- **`Packages` with `Source: "conda"`:**
  - Pin each one exactly: version equal, build equal when given.
  - Choose a spec form that `spec.ParseMatchSpec` reads as exact equality,
    not conda's fuzzy `=1.2` prefix match. Prove it with a test.
- **Legacy `CondaPackages`:** pass them through as match specs.
- **Checksums:** check `RestoreOptions` before installing anything. Call
  `goomba.Lock`, then `goomba.InstallLock`.
  - With `VerifyChecksums`, every conda package that has `SHA256` must equal
    the solved record's sha256.
  - With `Strict`, keep the existing rule that every package needs a
    checksum, and every conda package of the spec must also be in the
    solution.
  - Any mismatch fails before the prefix is created.
  - The micromamba restore only checks that checksums are present. Note the
    difference.
- **Pip packages:** install them afterwards exactly as today.

## Tests

- **Unit, offline:**
  - path filling equal to `CreateEnvironmentMamba`'s for the same
    name/root/version on linux, darwin and windows (refactor the path code
    so it can be called per GOOS);
  - `Backend` values;
  - reuse;
  - refusal of the incomplete marker;
  - option defaults;
  - the pip rule;
  - the JSON-spec-to-match-spec translation (exactness via
    `spec.ParseMatchSpec`);
  - checksum verification against a hand-made `lock.Lockfile`;
  - `MicromambaInstallPackage` on a goomba environment;
  - freeze of a fake `conda-meta`.
- **End to end, offline (skip unless `GOOMBA_REF_ROOT` is set):**
  - Serve a channel from `httptest`, using frozen conda-forge repodata and
    package files copied from `$GOOMBA_REF_ROOT/pkgs` into scratch, as
    goomba's own create differential does. Copy the approach; you cannot
    import goomba's test helpers.
  - Run `CreateEnvironmentGoombaWithOptions` for python 3.11 (pass the
    server's URL as the channel) and check python, pip, `SitePackagesPath`
    and `PythonLibPath`.
  - Run a JSON restore of a spec with `Packages` and checksums: one that
    passes, and one with a wrong sha256 that fails with nothing created.
- **Differential (skip unless `GOOMBA_MICROMAMBA` and `GOOMBA_LIVE=1`):**
  - Put `$GOOMBA_MICROMAMBA` into a scratch root's `bin/`, so
    `CreateEnvironmentMamba` downloads nothing.
  - Create `base-3.12` with each backend under separate scratch roots.
  - Compare the `PythonEnvironment` fields (paths equal modulo the root), the
    python and pip versions, and the conda package sets from `conda-meta`.
    Explain every difference.
  - Run micromamba with a scratch HOME and an explicit `MAMBA_ROOT_PREFIX`.
    The developer shell may export a stray one.
- **Existing jumpboot tests stay green.** Run the package tests and the
  examples' builds. A `CGO_ENABLED=0` build must still work for the files
  that built without cgo before. Run vet with `GOOS=windows` and
  `GOOS=darwin`.

## Dependency

- Require goomba at the commit named in your prompt (a pseudo-version of
  goomba's pushed `main`).
- Never commit a `replace` directive or a `go.work` file.
- goomba adds `github.com/klauspost/compress` to jumpboot's module graph;
  report any other additions.
- Keep jumpboot's `go` directive (1.24.0).

## Docs

- Update `doc.go` and `README.md` with a short "Choosing a backend"
  section: what each backend needs, its defaults and the differences listed
  above.
- Add one example (`examples/goomba/`) that mirrors the simplest existing
  micromamba example.

## Out of scope

- Changing poppi or jb-mesh. Report what they would change, such as
  `baseKind` using `Backend`.
- Installing into an existing goomba environment, and removing packages.
- Removing or deprecating anything micromamba-related.
