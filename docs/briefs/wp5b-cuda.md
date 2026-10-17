# WP5b — `virtual`: detect `__cuda` and `__cuda_arch` without cgo

Read `AGENTS.md` first. You own `virtual/` only.

## Goal

`virtual.Detect` reports `__cuda` today only when an override sets it, so
goomba never selects CUDA builds on a GPU machine unless the caller supplies
the version by hand. Detect the driver's CUDA version and the GPUs' minimum
compute capability by running `nvidia-smi`, as conda and rattler do,
without cgo and without reading environment variables.

References: rattler `crates/rattler_virtual_packages/src/cuda.rs`
(`detect_cuda_version_via_nvidia_smi`, `parse_nvidia_smi_cuda_version`,
`detect_cuda_arch_via_nvidia_smi`, the compute-capability parser,
`is_valid_cuda_version_format`, and their tests) and `lib.rs` (`Cuda`,
`CudaArch`, the rule that `__cuda_arch` is absent when `__cuda` is);
libmamba `src/core/virtual_packages.cpp` (`cuda_version`, including its
Windows search of `DriverStore\FileRepository\nv*\nvidia-smi.exe`).

## Behaviour

- On `linux-*` and `win-*` hosts, Detect adds:
  - `__cuda`: version is `<cuda_version>` from `nvidia-smi --query -u -x`,
    the highest CUDA version the installed driver supports, normalised to
    `major.minor`; build `"0"`.
  - `__cuda_arch`: version is the minimum compute capability across all GPUs
    from `nvidia-smi --query-gpu=compute_cap --format=csv,noheader`, as
    `major.minor` with sub-architecture letters dropped (as rattler does);
    build `"0"`. Absent whenever `__cuda` is absent.
  - Neither appears on macOS or in `Defaults` (no platform is assumed to have
    a GPU).
- Driver version, not toolkit version: libmamba first reads
  `/usr/local/cuda/version.json` (the toolkit). goomba follows conda and
  rattler and reports what the driver supports. Document this as a known
  difference from micromamba.
- Overrides keep their current meaning: an overridden package is not probed,
  and an empty value removes it. Removing `__cuda` (`"__cuda": ""`) also
  disables the `nvidia-smi` run and drops `__cuda_arch` unless that is
  overridden too. If neither package needs probing, nothing is executed.
- Unparsable, missing, `N/A` or out-of-range values mean the package is
  absent, never an error. A non-zero exit status with parsable output still
  counts (rattler does this: degraded multi-GPU systems exit non-zero).

## Finding and running `nvidia-smi` (security-relevant)

Follow the existing `glibcFromGetconf` pattern: goomba never reads `PATH` or
any other environment variable.

- `Options.NvidiaSMI string`: an absolute path the caller chose (the CLI may
  take it from a flag). A relative path is `ErrInvalidOverride` (or a new
  sentinel, documented). When set, only that path is tried.
- Otherwise, try fixed absolute locations in order, for example:
  - Linux: `/usr/bin/nvidia-smi`, `/usr/lib/wsl/lib/nvidia-smi` (WSL2),
    `/usr/local/nvidia/bin/nvidia-smi` (common in GPU containers),
    `/run/current-system/sw/bin/nvidia-smi` (NixOS).
  - Windows: `<system directory>\nvidia-smi.exe`, with the directory from
    `windows.GetSystemDirectory`, not `%SystemRoot%`. Then libmamba's
    bounded `DriverStore\FileRepository\nv*\nvidia-smi.exe` search, then
    `<Program Files>\NVIDIA Corporation\NVSMI\nvidia-smi.exe` via
    `windows.KnownFolderPath`.
  Justify the list in a comment.
- On Unix, a built-in location is used only if it resolves to a regular file
  that is owned by root and not group- or world-writable. Treat any other
  file as absent. A caller-chosen path needs only to be an absolute regular
  file.
- Run with an empty environment on Unix. On Windows pass only
  `SystemRoot=<windows directory from the API>`, because many Windows
  programs fail without it.
- Use a timeout derived from ctx: about 10 s per command, since nvidia-smi
  can take seconds to initialise without persistence mode. Cap stdout at
  64 KiB and discard stderr. Honour ctx cancellation: return `ctx.Err()`
  from Detect, as today.
- Run the two queries only when a candidate path exists, so machines without
  NVIDIA tools pay one `stat` per location and no exec.

## Tests

- Port rattler's parser tests for both outputs, and add hostile cases: huge
  or binary output, several `<cuda_version>` elements, whitespace,
  `N/A`/`[N/A]`, `12`, `12.4.1`, `99999.1`, negative numbers, `8.6a`, `9.0a`,
  mixed valid and invalid lines, and thousands of lines.
- Extend the fake `probe` so that every Detect rule above is covered without
  a GPU, including overrides and the `__cuda_arch`-needs-`__cuda` rule.
- Windows search logic must be testable on Linux (inject the file system or
  the directory list).
- Live test, skipped unless the GPU tool is present at a built-in location
  and `GOOMBA_LIVE=1`: Detect reports `__cuda` and `__cuda_arch` that match
  the tool's own output, parsed independently in the test.
- Fuzz the two parsers. Keep the existing gates green (`scripts/gate.sh
  ./virtual/...`, `CGO_ENABLED=0` build, `GOOS=windows`/`darwin` vet).

## Out of scope

AMD GPUs (`__amdgpu`, `__amdgpu_arch`), NVML/libcuda loading (needs cgo or
dlopen), and caching detection results across calls.
