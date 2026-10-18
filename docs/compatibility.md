# Conda support

goomba implements the part of conda that creating an environment needs:
reading channels, solving match specs, and installing packages into a new
prefix. This page lists what it does today. Anything not listed here is not
supported.

## Platforms

| Platform | Status |
| --- | --- |
| linux-64 | Supported and tested end to end, including file-for-file comparisons with micromamba 2.2.0 (contents compared after normalising the install path). |
| linux-aarch64 and other Linux platforms | Should work; not tested on such hardware. |
| osx-arm64 | Builds, passes the Go race-test suite, and completed a live `python=3.12 pip` install on macOS 27.0.1. The installed Python, pip entry point, SSL and SQLite ran. No file-for-file differential with micromamba has been run. Rewritten binaries are re-signed with `/usr/bin/codesign -s - -f`, as conda and mamba do. |
| osx-64 | Builds and passes `go vet`; not runtime-tested. |
| win-64 | Not supported. goomba cannot install Python entry points such as `pip.exe` (it has no launcher), and has no prefix lock without `flock`. |

On any platform, `goomba lock --platform P` (or `LockOptions.Platform`)
resolves for another platform. Its virtual packages are then conservative
defaults (`virtual.Defaults`, rattler's baseline): glibc 2.28 and Linux 4.18
for Linux, macOS 13.0 for macOS, and no GPU. A lockfile installs only on the
platform it was made for.

## Channels

- **Names** resolve against the channel alias, `https://conda.anaconda.org`
  unless `LockOptions.ChannelAlias` says otherwise (the CLI's is fixed):
  `conda-forge`, or a label such as `conda-forge/label/dev`. `pkgs/main`, `pkgs/r`, `pkgs/pro` and
  `pkgs/msys2` resolve to `https://repo.anaconda.com`, as in conda.
- **URLs** must be https, such as `https://prefix.dev/conda-forge`.
- **Priority** is strict, the only mode: every package comes from the
  first channel, in the order given, that has any package of that name.
- **Channel-qualified specs** (`conda-forge::numpy`) restrict a package to
  one channel. Unlike conda and micromamba, they do not add that channel:
  it must be one of the channels given, and the first of them that has the
  package, or the request is unsolvable and the explanation says why.

Refused, with a usage error:

- the `defaults` multichannel (list its channels instead);
- `http://` and `file://` channels;
- URLs with credentials or an anaconda.org `/t/<token>/` segment;
- platform filters such as `conda-forge/linux-64` (the platform is set
  separately);
- IPv6-literal hosts.

There is no authentication, so channels that need credentials cannot be
used.

### Network policy

Every download, both indexes and packages, follows one policy:

- **Public stays public.** A channel on a public host may only lead to https
  URLs on public hosts. A redirect or a DNS answer that points at a
  loopback, private, link-local or otherwise non-public address is refused,
  and the check is made on the address actually dialled.
- **Private mirrors need an address literal.** A mirror named by a private
  IPv4 literal (`https://10.1.2.3/conda-forge`) or a localhost name may use
  private addresses. A mirror whose DNS name resolves to a private address
  is refused. From Go, an `http.Client` whose transport is not an
  `*http.Transport` is used as is, and the policy is then the caller's.
- **Lockfile URLs are trusted as written.** A lockfile that names a private
  mirror by address literal is fetched from it; redirects from a public
  host are still held to the policy.
- **Redirects** never go from https to http.
- **Proxies** come only from the caller: the library takes an
  `*http.Client`, and the CLI builds one from `HTTPS_PROXY` and `NO_PROXY`.
  Through a proxy, only the host name as written can be checked.

## Channel indexes

- **The sharded index ([CEP 16](https://github.com/conda/ceps/blob/main/cep-0016.md))** is
  used when the channel publishes one, as conda-forge does. goomba fetches
  the shard index and then only the shards the solve reaches: about 2 MB
  for conda-forge's `python=3.12`, where the full linux-64 and noarch
  indexes are 88 MB compressed. Each
  shard is checked against the sha256 the shard index lists for it.
- **The full index**, `repodata.json` as `.zst`, `.bz2` or plain JSON, is
  used otherwise.
- **JLAP** incremental updates are not supported.
- **Caching.** Index files are cached in `RepodataDir` and revalidated with
  conditional GETs (`If-None-Match`, `If-Modified-Since`) once older than
  `MaxAge`; the CLI revalidates every time. Shards are named by their hash
  and never expire. Offline mode uses only the cache.
- **The python prefilter.** When the specs pin python to one minor series
  (`python=3.12`), records that cannot work with it are dropped while the
  index loads. If the request is then unsolvable, goomba solves again
  without the filter, as libmamba does.
- **Records without a sha256 are never installed.** They appear in
  explanations as "cannot be verified".
- **`base_url`.** A channel whose index places packages under another URL
  (CEP 15) works only if that URL ends in `/<subdir>/`. Otherwise locking
  fails with `lock.ErrInvalid`.

## Match specs

goomba parses conda's match spec syntax, ported from libmamba and checked
against conda's own corpus of versions:

```
numpy
numpy >=1.26,<2
python=3.12
python 3.12.* *_cpython
conda-forge::numpy >=1.26
numpy[version='>=1.26', build_number='>=1']
```

- **Version specs:** comparisons, `=` (starts with), `*` globs, `,` (and)
  binding tighter than `|` (or), parentheses, and `~=`.
- **Bracket keys:** `version`, `build` / `build_string`, `build_number`,
  `channel`, `url`, `subdir`, `fn` / `filename`, `md5`, `sha256`, `license`,
  `license_family`, `features`, `track_features`, `optional`, and `name` /
  `target`. An unknown key is an error; libmamba silently ignores it.
- **Build strings** are globs where only `*` is special; `^...$` is a
  regular expression.
- **Version ordering** is conda's, including epochs (`1!2.0`), local
  versions (`+local`), and pre- and post-releases.

Where conda, libmamba and libsolv disagree, goomba follows libsolv, which is
what micromamba uses to match dependencies. The full list is in the
[`spec` package docs](https://pkg.go.dev/github.com/richinsley/goomba/spec).
The differences most likely to matter:

- `==X.*` means `=X`, as in libmamba and libsolv.
- `>X.*`, `<X.*`, `<=X.*` and `~=X.*` match nothing, as in libsolv.
- `=*` and `==*` match every version.
- Version specs written as regular expressions (`^1.0$`) are not supported.

Not supported:

- conditional dependencies (`foo; if __win`, `foo[when=...]`);
- extras (`foo[extras=[bar]]`) and pip-style extras (`dask[array]`).

Records whose dependencies use these are excluded, with the reason in
explanations.

Every parser bounds its input and rejects channels carrying credentials.
Text from channels is escaped before it reaches a terminal.

## Solving

The solver is a Go port of [resolvo](https://github.com/prefix-dev/resolvo)'s
conflict-learning engine, with conda's rules from rattler's
`rattler_solve`. A solution has one record per package name, and:

- every requested spec is satisfied;
- every dependency of every chosen record is satisfied;
- every `constrains` entry holds for the packages it names when they are
  chosen;
- every dependency on a virtual package is met by the machine.

**Candidate order**, from rattler's default "highest" strategy:

1. records without `track_features`;
2. higher versions;
3. higher build numbers;
4. the candidate whose dependencies reach the highest versions;
5. newer timestamps.

Remaining ties are broken by a canonical record order, so the same request
against the same index always gives the same solution.

**Explanations.** An unsolvable request returns `*solve.Unsolvable`, whose
explanation is a tree:

```
The following packages are incompatible
├─ numpy <1.22 can be installed with any of the following options:
│  └─ numpy 1.16.5 | 1.16.6 | ... | 1.19.5 would require
│     └─ python_abi 3.6.* *_cp36m, which can be installed with any of the following options:
│        └─ python_abi 3.6
└─ python 3.12.* cannot be installed because there are no viable options:
   └─ python 3.12.0 | 3.12.1 | ... | 3.12.15, which conflicts with the versions reported above.
```

Not supported:

- flexible or disabled channel priority;
- the lowest-version strategies;
- exclude-newer;
- dependency overrides;
- pinned files;
- solving against an existing environment (install, update, remove).

## Virtual packages

goomba detects these without cgo:

| Package | Source |
| --- | --- |
| `__unix`, `__win` | the platform |
| `__linux` | the kernel release |
| `__osx` | the `kern.osproductversion` sysctl |
| `__glibc` | the version definitions in `libc.so.6`, else `getconf GNU_LIBC_VERSION`; absent on musl |
| `__archspec` | on amd64 the x86-64 level (`x86_64` to `x86_64_v4`); elsewhere the architecture (`aarch64`, `arm64`, `ppc64le`, ...) |
| `__cuda`, `__cuda_arch` | `nvidia-smi`, from fixed system locations only, never from `PATH` |

- **Overrides.** Every value can be overridden: `virtual.Options.Overrides`
  in Go; in the CLI, `--virtual-override NAME=VALUE` and the
  `CONDA_OVERRIDE_*` variables conda knows (`ARCHSPEC`, `CUDA`,
  `CUDA_ARCH`, `GLIBC`, `LINUX`, `OSX`, `WIN`). An empty value removes the
  package.
- **The lockfile records them.** It keeps the virtual packages the solve
  assumed.
- **Installs check them.** Every package's virtual dependencies are checked
  again before anything is downloaded. micromamba skips this check when it
  installs an explicit file.

`__cuda` is the highest CUDA version the driver supports, as conda reports
it. micromamba reports the toolkit version from
`/usr/local/cuda/version.json` when that file exists, so the two can
disagree.

## Packages

- **Archive formats:** `.conda` and `.tar.bz2`.
- **Package metadata:** `info/paths.json` version 1, or the legacy
  `info/files`, `info/has_prefix`, `info/no_link` and `info/no_softlink`;
  `info/link.json` for noarch python packages and their entry points.
- **Files:** hard links, symlinks and directories.
  - Files are hard linked from the package cache.
  - goomba copies instead when the file needs the prefix written into it,
    is marked `no_link`, or cannot be hard linked, for example across file
    systems.
- **Prefix replacement:**
  - **Text files:** every occurrence of the placeholder is replaced.
  - **Binary files:** the prefix is written into each null-terminated
    string and padded with nulls. A prefix longer than the placeholder is
    refused (`link.ErrPrefixTooLong`).
  - **Long shebangs** (over 127 bytes on Linux, 512 elsewhere) are
    rewritten to `#!/usr/bin/env`, as libmamba does.
- **noarch packages:**
  - **noarch: generic** packages are linked as they are.
  - **noarch: python** packages go into the interpreter's site-packages,
    including the `python_site_packages_path` of free-threaded builds.
    Their `python-scripts` go into `bin`.
- **Entry points:**
  - They must parse as `module:function` made of Python identifiers before
    anything is written.
  - Each script starts with a shebang for the environment's Python, or with
    a `/bin/sh` trampoline when that path is too long or contains a space.
- **Byte-compilation** of noarch python packages is optional:
  - It uses the new environment's interpreter and libmamba's compile
    script.
  - The interpreter runs isolated (`-I -S`), so no `.pth` file or
    `sitecustomize` from a package runs.
  - A file that fails to compile is skipped, as in micromamba.
- **conda-meta:** each package's record and `conda-meta/history` are
  written with micromamba's fields, so conda and micromamba can read a
  goomba environment, and goomba can read theirs.

### Refused or not done

- **Link scripts are never run.** A package with a pre-link, post-link or
  pre-unlink script is refused (`link.ErrLinkScript`).
  `InstallOptions.AllowScripts` lets named packages be installed with their
  scripts in place, still not run. The CLI has no option for this.
- **Two packages that install the same path** are an error (`ErrClobber`).
  micromamba warns and overwrites.
- **Unsafe packages are refused.** That covers absolute paths, `..`,
  symlinks that leave the package or the prefix, device files, and other
  hostile archive entries.
- **Menu shortcuts.** `menuinst` shortcuts are not created; `Menu/*.json`
  files are installed as ordinary files.
- **Not done at all:** environment activation and activation scripts,
  `conda run`, package signature verification, and conda's `.condarc` or
  environment variables.

## Differences from micromamba's result

For the same packages, goomba builds the environment micromamba builds,
file for file, apart from the install path and these deliberate
differences:

- **No group or other write.** No file or directory gives group or others
  write permission. Archives often ship files 0664; goomba installs them
  0644. Entry point scripts are 0755.
- **conda-meta is fuller.** Records carry the real `sha256_in_prefix` of
  files whose prefix was rewritten, and the full repodata fields, where
  micromamba 2.2.0's explicit install leaves several empty.
- **`requested_spec`** and the history's `# update specs` come from the
  lockfile's specs, and the history lists packages in goomba's link order.
- **`depends` and `constrains`** in conda-meta keep the index's text and
  order; micromamba 2.2.0 rewrites some constrains as libsolv renders them.
- **setuid, setgid and sticky bits are dropped** from package files.
- **goomba never adds pip to python.** Ask for `pip` if you want it.

## Lockfiles

A goomba lockfile is one JSON file for one platform. It lists every package
by exact URL with its sha256, and installing from it needs no solver and no
index. Here is one written by `goomba lock`, shortened to one package:

```json
{
  "version": 1,
  "platform": "linux-64",
  "channels": ["https://conda.anaconda.org/conda-forge"],
  "specs": ["python=3.12", "pip"],
  "virtual": {"__archspec": "x86_64_v4", "__glibc": "2.35", "__linux": "7.1.1", "__unix": "0"},
  "packages": [
    {
      "name": "bzip2", "version": "1.0.8", "build": "hda65f42_10", "build_number": 10,
      "subdir": "linux-64", "noarch": "",
      "url": "https://conda.anaconda.org/conda-forge/linux-64/bzip2-1.0.8-hda65f42_10.conda",
      "sha256": "1a0d382c515ebf55f8ee1f38c8b81bc95af5c2acc42ad53b66bc5df932032f96",
      "md5": "e675fabcf81499adc7edf58124fb1e01",
      "size": 257808,
      "depends": ["__glibc >=2.17,<3.0.a0", "libgcc >=14"],
      "constrains": [],
      "python_site_packages_path": ""
    }
  ]
}
```

- **Reading is strict.** The reader refuses unknown or duplicate keys,
  nulls, URLs with credentials, and anything that fails validation.
- **Bundles.** `lock.Bundle` keeps one lockfile per platform in one file.
  The CLI does not write bundles.
- **@EXPLICIT export:** `goomba export --explicit` writes conda's explicit
  format (URL and md5) in micromamba's order, so `micromamba create -f` can
  install the same set.
- **@EXPLICIT import:** `lock.ReadExplicit` and `Explicit.Resolve` turn an
  explicit file into a lockfile, with the sha256s from an index lookup the
  caller supplies. This is library-only.
- **From an environment:** `goomba lock --from-prefix` builds a lockfile
  from an environment's `conda-meta` records without solving. The
  environment can come from goomba, conda, or a micromamba solve. Every
  package needs a sha256, so micromamba's explicit installs, which record
  none, are refused.
