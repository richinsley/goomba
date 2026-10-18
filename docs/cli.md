# The goomba command

`goomba` creates conda environments from match specs or from lockfiles,
writes lockfiles, and lists environments. It has no configuration files.

```sh
go install github.com/richinsley/goomba/cmd/goomba@latest
```

goomba needs Go 1.24 or later to build. It runs on Linux; on Apple Silicon,
a live `python=3.12 pip` install has passed, but broader macOS differential
testing has not been done. See
[compatibility.md](compatibility.md#platforms).

## Commands

```
goomba create -p PREFIX [-c CHANNEL]... [--offline] [--repodata-dir DIR]
              [--pkgs-dir DIR] [--no-pyc] [--virtual-override NAME=VALUE]...
              [--nvidia-smi PATH] [--timeout DURATION] SPEC...
goomba create --lock FILE -p PREFIX [--pkgs-dir DIR] [--no-pyc]
              [--virtual-from-lock] [--virtual-override NAME=VALUE]...
              [--nvidia-smi PATH] [--timeout DURATION]
goomba lock [-c CHANNEL]... [--platform P] [--offline] [--repodata-dir DIR]
            [--virtual-override NAME=VALUE]... [--nvidia-smi PATH]
            [--timeout DURATION] -o FILE SPEC...
goomba lock --from-prefix PREFIX -o FILE
goomba export --explicit --lock FILE
goomba list -p PREFIX
```

- **Flag order.** Flags and SPECs may come in any order, and `--` ends the
  flags. Flags take one or two dashes (`-p` or `--p`).
- **Quoting.** Quote a SPEC that contains spaces or shell characters:
  `'numpy >=1.26'`.
- **Output.** Progress and errors go to standard error. Lockfiles written to
  `-o -` and the output of `export` and `list` go to standard output.

## Create an environment from specs

```sh
goomba create -p ./py312 python=3.12 pip
```

```
goomba: no channel given (-c); using conda-forge
loaded https://conda.anaconda.org/conda-forge (1/1)
solving (skipping packages for other python than 3.12)
solved: 26 packages
fetched zstd-1.5.7-hb78ec9c_7 (1/26)
...
linked pip-26.2.1-pyh8b19718_0 (26/26)
compiling 665 python files
created /work/py312 (26 packages)
```

This is `micromamba create -p ./py312 -c conda-forge python=3.12 pip`
without micromamba.

- **The prefix** must not exist, or must be an empty directory of yours.
  goomba never installs into an existing environment.
- **Channels** are given with `-c`, highest priority first. Priority is
  strict. Without `-c`, goomba uses conda-forge and says so.
- **pip is not added.** micromamba adds pip to python by default; goomba
  installs only what you ask for.
- **Running it.** The environment is not activated. Run its programs by
  path: `./py312/bin/python`.

```sh
./py312/bin/python -c 'import ssl, sqlite3; print("ok")'
goomba create -p ./sci -c conda-forge python=3.12 'numpy>=2' pandas
```

If the request has no solution, goomba prints why and exits 1:

```sh
goomba create -p ./old python=3.12 'numpy<1.22'
```

```
goomba: the request cannot be satisfied:
The following packages are incompatible
├─ numpy <1.22 can be installed with any of the following options:
│  └─ numpy 1.16.5 | 1.16.6 | ... | 1.19.5 would require
│     └─ python_abi 3.6.* *_cp36m, which can be installed with any of the following options:
│        └─ python_abi 3.6
└─ python 3.12.* cannot be installed because there are no viable options:
   └─ python 3.12.0 | 3.12.1 | ... | 3.12.15, which conflicts with the versions reported above.
```

## Lock, then create from the lockfile

A lockfile pins every package by URL and sha256. Make it once, review it,
and install it anywhere with the same platform. No solving happens at
install time.

```sh
goomba lock -o py312.lock.json python=3.12 pip
goomba create --lock py312.lock.json -p ./env-a
```

- **`-o -`** writes the lockfile to standard output.
- **`--platform P`** locks for another platform. Its virtual packages are
  conservative defaults, adjusted by any overrides: glibc 2.28 and Linux
  4.18 for Linux, macOS 13.0 for macOS (rattler's baseline), and never a
  GPU.

  ```sh
  goomba lock --platform osx-arm64 -o py312-osx-arm64.lock.json python=3.12 pip
  ```

- **`create --lock`** verifies every archive against the lockfile's sha256
  before unpacking it. SPECs and `--lock` cannot be combined, and the
  resolve flags (`-c`, `--offline`, `--repodata-dir`) are a usage error with
  `--lock`.

## Lock an existing environment

```sh
goomba lock --from-prefix ./py312 -o py312-from-env.lock.json
```

This reads the environment's `conda-meta` records, without solving. It works
for environments made by goomba or conda, and by micromamba when it solved
them. Every package needs a sha256, and micromamba's explicit installs
(`micromamba create -f`) record none, so those are refused.

- **Platform.** An environment of only noarch packages is locked for this
  machine's platform.
- **Unfinished environments** are refused.

## Export to conda's explicit format

```sh
goomba export --explicit --lock py312.lock.json > py312.explicit.txt
```

This writes `@EXPLICIT` lines (URL and md5) in the order micromamba uses, so
micromamba or conda can install the same packages:
`micromamba create -p PREFIX -f py312.explicit.txt`.

## List an environment

```sh
goomba list -p ./py312
```

```
Name              Version    Build                 Channel
_openmp_mutex     4.5        20_gnu                conda-forge
bzip2             1.0.8      hda65f42_10           conda-forge
...
```

`list` warns when the prefix is incomplete, and says whether a goomba
process is installing or removing it right now, or an installation did not
finish.

## Caches

| Cache | Flag | Default |
| --- | --- | --- |
| Package archives and extracted packages | `--pkgs-dir DIR` | `<user cache dir>/goomba/pkgs` |
| Channel indexes | `--repodata-dir DIR` | `<user cache dir>/goomba/repodata` |

- **Where the defaults live.** The user cache directory is
  `$XDG_CACHE_HOME` or `~/.cache` on Linux, and `~/Library/Caches` on
  macOS. Without one, the flags are required.
- **Privacy.** Both caches, and every directory above them, must be
  private to you: goomba refuses a cache that another user could change.
- **Inside the prefix.** The package cache cannot be inside the prefix.
- **Sharing.** Any number of goomba processes can share the caches. Do not
  share the package cache with micromamba, whose layout differs. goomba
  builds that mark extracted packages differently re-extract each other's
  packages when they share a cache; give them separate caches.

**`--offline`** solves from the cached indexes only, whatever their age.
`create --offline` still downloads package archives the package cache does
not have:

```sh
goomba create --offline -p ./env-b python=3.12 pip
```

## Virtual packages

Packages depend on facts about the machine, such as `__glibc >=2.17` or
`__cuda >=12`. goomba detects them the way conda does:

- **`__cuda` and `__cuda_arch`** come from `nvidia-smi`, found only at fixed
  system locations, never through `PATH`. `--nvidia-smi PATH` names it
  instead; the path must be an absolute path to a regular file. It is a
  usage error with `--virtual-from-lock`, or with `--platform` for another
  platform, where this machine's GPU does not matter.
- **Overrides.** Adjust the detected values with `CONDA_OVERRIDE_<NAME>`
  for the names conda knows (`ARCHSPEC`, `CUDA`, `CUDA_ARCH`, `GLIBC`,
  `LINUX`, `OSX`, `WIN`), or with `--virtual-override NAME=VALUE`, which
  wins. Case and a leading `__` do not matter, and an empty value removes
  the package.
- **The lockfile records them.** `lock` stores the virtual packages it
  solved with.
- **`create --lock` checks them.** It checks the lockfile's packages
  against this machine. It warns if the lockfile was solved for different
  virtual packages, unless they are exactly the platform's conservative
  defaults (`virtual.Defaults`, which `lock --platform` records for another
  platform when no override is given), meant for any machine.
- **Preparing for another machine.** `--virtual-from-lock` checks against
  the lockfile's recorded virtual packages instead of this machine's, so
  this machine's own are not checked at all.

```sh
# Lock without the GPU this machine has:
goomba lock --virtual-override cuda= -o cpu.lock.json python=3.12 pip

# Lock as if for an older glibc:
CONDA_OVERRIDE_GLIBC=2.28 goomba lock -o glibc228.lock.json python=3.12 pip

# Install for the machine the lockfile describes:
goomba create --lock cpu.lock.json --virtual-from-lock -p ./env-c
```

## Time limits and interruption

- **`--timeout`.** `create` (with SPECs or `--lock`) and `lock` with SPECs
  must finish within `--timeout`, 10 minutes by default. It covers
  everything: detecting virtual packages, resolving, downloading and
  installing. A hostile channel could otherwise keep the solver busy for
  very long. Raise it for large environments on slow links.
- **Signals.** SIGINT, SIGTERM and SIGHUP stop the command, and an
  interrupted `create` removes what it installed:

  ```
  goomba: interrupt: stopping, rolling back…
  goomba: context canceled
  ```

  Further signals during the rollback are ignored. SIGPIPE is ignored too,
  so a closed output pipe cannot kill goomba halfway through.
- **A killed goomba** (SIGKILL, OOM) leaves an incomplete prefix. `list`
  warns about it, and `lock --from-prefix` refuses it. Make sure no goomba
  process, and nothing it started, is still working on it, then delete it.
  See [failures.md](failures.md).

## Environment variables

The CLI reads:

- `HTTPS_PROXY` and `NO_PROXY`, or their lower-case forms. An
  `HTTPS_PROXY` value Go's `net/http` would silently ignore or misread is an
  error instead. `NO_PROXY` is read only when `HTTPS_PROXY` is set, and an
  entry that can match no host draws a warning.
- `CONDA_OVERRIDE_*`, as described above. Other names are ignored with a
  warning.
- The variables that locate the user cache directory, such as
  `XDG_CACHE_HOME`.
- `SSL_CERT_FILE` and `SSL_CERT_DIR`, read by Go's TLS stack on Linux and
  other Unix systems to choose trusted certificate authorities.

Nothing else in the environment changes what goomba does: no `.condarc`, no
`MAMBA_*`, no `CONDA_*` beyond the overrides.

## Exit status

| Status | Meaning |
| --- | --- |
| 0 | success |
| 1 | failure, including an unsolvable request |
| 2 | usage error |

When the reader of standard output stops early, as `head` does in
`goomba list -p ./py312 | head -3`, goomba may exit 1, without a message.
(When the whole output fits in the pipe before the reader stops, it exits
0.)

Usage errors are found before anything is detected or downloaded. They
include:

- a spec that does not parse;
- a refused channel (`file://`, `http://`, `defaults`, credentials);
- a duplicate channel;
- a package cache inside the prefix;
- a missing required flag.

Text that comes from channels, lockfiles or prefixes is printed with
control and bidirectional-formatting characters escaped, and credentials in
URLs are never printed.
