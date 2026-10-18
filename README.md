# goomba

goomba creates conda environments in pure Go, without micromamba or conda.
It resolves match specs against conda channels into a lockfile, and installs
a lockfile into a new environment, verifying every package against its
sha256. It works as a Go library and as a command.

```sh
goomba create -p ./py312 python=3.12 pip
./py312/bin/python -c 'import ssl, sqlite3; print("ok")'
```

goomba was built so that Go programs can provision Python without
downloading and running a package manager. The resolver and installer are
compiled into the program:

- **No executable is fetched.** goomba downloads channel indexes and package
  archives only. Besides the new environment's own Python (to
  byte-compile), it runs only `nvidia-smi` and `getconf` from fixed system
  locations (or the `nvidia-smi` you name) to detect the machine, and
  `/usr/bin/codesign` on Apple Silicon.
- **No host settings.** The library reads no conda config files and no
  environment variables of its own.
- **Verified packages.** Every archive is checked against its sha256 before
  it is unpacked.
- **No package scripts.** goomba never runs them.

[jumpboot](https://github.com/richinsley/jumpboot) uses goomba as one of its
two backends, next to micromamba.

## Status

goomba creates new environments from conda-forge and other https channels
on Linux. linux-64 is tested end to end, including file-for-file
comparisons with micromamba 2.2.0. macOS builds but has not been run on a
Mac. Windows is not supported.

It does not:

- install into, update or remove packages from an existing environment;
- activate environments;
- run package scripts;
- install pip packages.

[docs/compatibility.md](docs/compatibility.md) lists exactly what is
supported.

## Install

The command:

```sh
go install github.com/richinsley/goomba/cmd/goomba@latest
```

The library:

```sh
go get github.com/richinsley/goomba
```

Both need Go 1.24 or later. Besides the standard library, goomba depends on
`github.com/klauspost/compress`, `github.com/vmihailenco/msgpack/v5` (with
its `github.com/vmihailenco/tagparser/v2`) and `golang.org/x/sys`.

## Command line

```sh
goomba create -p ./demo python=3.12 pip             # solve and install
goomba lock -o py312.lock.json python=3.12 pip      # solve to a lockfile
goomba create --lock py312.lock.json -p ./env-a     # install a lockfile
goomba list -p ./env-a                              # list the packages
```

- **Channels** default to conda-forge. `-c CHANNEL`, repeatable and highest
  priority first, replaces it; priority is strict.
- **Caches** go under your user cache directory, `goomba/pkgs` and
  `goomba/repodata`.

See [docs/cli.md](docs/cli.md) for every command and flag.

## Go

```go
	lf, err := goomba.Create(ctx, prefix,
		goomba.LockOptions{
			Specs:       specs,
			Channels:    []string{"conda-forge"},
			RepodataDir: filepath.Join(cache, "goomba", "repodata"),
			Progress:    progress,
		},
		goomba.InstallOptions{
			PkgsDir:    filepath.Join(cache, "goomba", "pkgs"),
			CompilePyc: true,
			Progress:   progress,
		})
```

On an error the installation is rolled back. If the process is killed
instead, or the rollback cannot finish, `goomba.CheckComplete` reports the
prefix as incomplete, and `goomba.RemoveIncomplete` cleans it up.

[docs/library.md](docs/library.md) walks through the API with complete
programs from [examples/](examples).

## Documentation

| Document | Contents |
| --- | --- |
| [docs/cli.md](docs/cli.md) | The `goomba` command |
| [docs/library.md](docs/library.md) | Using goomba from Go, with runnable examples |
| [docs/compatibility.md](docs/compatibility.md) | Which part of conda goomba supports, and how it differs from micromamba |
| [docs/transactions.md](docs/transactions.md) | What an installation guarantees, and where the guarantees stop |
| [docs/failures.md](docs/failures.md) | Errors, what stays locked, what rolls back, retrying, and pip |
| [docs/architecture.md](docs/architecture.md) | Packages, the resolver, the installer and the security model |
| [pkg.go.dev](https://pkg.go.dev/github.com/richinsley/goomba) | API reference |

`AGENTS.md` and [docs/briefs](docs/briefs) record how goomba was built: the
rules every work package followed, and each package's brief.

## License

goomba is released under the MIT license; see [LICENSE](LICENSE). It
includes code ported from mamba, resolvo and rattler, under the BSD-3-Clause
license; see [NOTICE](NOTICE).
