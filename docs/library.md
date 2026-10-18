# Using goomba from Go

```sh
go get github.com/richinsley/goomba
```

goomba needs Go 1.24 or later. The API reference is on
[pkg.go.dev](https://pkg.go.dev/github.com/richinsley/goomba). This page
shows how the pieces fit together. Every example here is a complete program
in [examples/](../examples), and was run against the release.

## The API in brief

| Function | What it does |
| --- | --- |
| `Lock(ctx, LockOptions)` | Resolves match specs against channels into a `*lock.Lockfile`. Touches no prefix. |
| `InstallLock(ctx, prefix, lf, InstallOptions)` | Creates a new environment from a lockfile, as one transaction. |
| `Create(ctx, prefix, LockOptions, InstallOptions)` | `Lock`, then `InstallLock`. Returns the lockfile it installed. |
| `CheckComplete(prefix)` | Tells whether a prefix holds a finished environment, one being installed, or one a killed installation left behind. |
| `RemoveIncomplete(prefix)` | Removes what a killed installation left behind. |
| `ValidateLock(LockOptions)`, `ValidateInstall(prefix, InstallOptions)` | Report the option and prefix errors `Lock` and `InstallLock` would return, without doing any work. |

Some things hold for the whole API:

- **No environment variables of its own.** The library reads no
  environment variables, no config files and nothing from the home
  directory. Every setting comes through `LockOptions` and
  `InstallOptions`, including the cache directories. Go's TLS stack still
  uses the system's trusted certificates, which `SSL_CERT_FILE` and
  `SSL_CERT_DIR` can change on Linux.
- **No proxy unless you pass one.** The default HTTP client uses no proxy.
  To use one, pass an `*http.Client` whose `Transport` sets `Proxy`.
- **Concurrency.** Every function is safe for concurrent use. Calls for
  different prefixes run independently; calls for the same prefix exclude
  each other through the prefix lock.
- **Contexts.** Everything that does I/O takes a `context.Context` and stops
  promptly when it is cancelled.

## Create an environment from specs

[examples/create](../examples/create/main.go) does what
`goomba create -p PREFIX SPEC...` does:

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

	var unsolvable *solve.Unsolvable
	switch {
	case errors.As(err, &unsolvable):
		fmt.Fprintf(os.Stderr, "no solution:\n%s\n", unsolvable.Explanation)
		os.Exit(1)
	case errors.Is(err, goomba.ErrPrefixNotEmpty):
		log.Fatalf("%s already exists; choose a new prefix", prefix)
	case err != nil:
		// The installation was rolled back (see docs/failures.md).
		log.Fatal(err)
	}
	fmt.Printf("created %s with %d packages\n", prefix, len(lf.Packages))
```

```sh
go run ./examples/create ./py312 python=3.12 pip
```

The options in this example:

- **Caches.** `RepodataDir` (channel indexes) and `PkgsDir` (package
  archives and their extracted trees) are required. They must be private
  to the user, and any number of processes can share them. The example
  uses the same directories as the CLI.
- **Defaults.** `Channels` lists channel names or https URLs, highest
  priority first. `Platform` defaults to this machine's, and `Virtual` to
  this machine's detected virtual packages.
- **Byte-compilation.** `CompilePyc` byte-compiles noarch python packages,
  as micromamba does by default. The zero value skips it.
- **The deadline.** `ctx` carries a deadline. Solving is NP-complete, and
  `Lock` imposes no limit of its own.

### Progress

`Progress` receives `Event`s. For `Lock` they arrive on the calling
goroutine. For `InstallLock` they may arrive concurrently from several
goroutines, so the example guards its output with a mutex:

```go
	// Progress may be called from several goroutines at once.
	var mu sync.Mutex
	progress := func(e goomba.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Kind {
		case goomba.EventSolved:
			fmt.Fprintf(os.Stderr, "solved: %d packages\n", e.Total)
		case goomba.EventLinked:
			fmt.Fprintf(os.Stderr, "linked %s (%d/%d)\n", e.Package, e.Done, e.Total)
		}
	}
```

The event kinds, in order:

- **Resolving:** `EventRepodata` (a channel's index is open), then
  `EventSolving` and `EventSolved`.
- **Installing:** `EventFetched` and `EventLinked` (once per package),
  `EventCompiling` and `EventCompiled`, `EventBeforeCommit`, then
  `EventDone`.

A panic in an install's `Progress` cancels the transaction, rolls it back
(unless it was reporting `EventDone`) and is raised again on the goroutine
that called `InstallLock`.

## Lock at development time

A lockfile fixes every package by URL and sha256, so it can be reviewed
before it ships, and installing it needs no solver.
[examples/lock](../examples/lock/main.go) writes one lockfile per platform:

```go
	for _, platform := range []string{"linux-64", "linux-aarch64", "osx-64", "osx-arm64"} {
		lf, err := goomba.Lock(ctx, goomba.LockOptions{
			Specs:       specs,
			Channels:    []string{"conda-forge"},
			Platform:    platform,
			Virtual:     virtual.Defaults(platform),
			RepodataDir: filepath.Join(cache, "goomba", "repodata"),
		})
```

```sh
go run ./examples/lock ./locks python=3.12 pip
```

The options in this example:

- **Other platforms need `Virtual`.** `Lock` will not guess another
  machine's virtual packages. `virtual.Defaults` gives conservative ones:
  glibc 2.28 and Linux 4.18 for Linux, macOS 13.0, and no GPU.
- **Stable output.** `lf.Write` writes canonical JSON, so the same solution
  gives the same bytes.
- **Bundles.** `lock.Bundle` holds one lockfile per platform in a single
  file.

## Provision at runtime

[examples/provision](../examples/provision/main.go) is the pattern for a
program that brings its own Python, as
[jumpboot](https://github.com/richinsley/jumpboot) does. It:

- reads a lockfile;
- reuses the environment if it is already there (without comparing it with
  the lockfile: give each lockfile its own prefix);
- recovers from a killed installation;
- waits while another process installs;
- otherwise installs, running pip inside goomba's transaction.

Reading the lockfile:

```go
	lf, err := lock.Read(f) // strict: validates everything it returns
```

`lock.Read` takes any `io.Reader`, so a lockfile embedded with `go:embed`
works the same way.

### The caller's step: BeforeCommit

```go
	opt := goomba.InstallOptions{
		PkgsDir:    filepath.Join(cache, "goomba", "pkgs"),
		CompilePyc: true,
		// The caller's last step, still inside the transaction.
		BeforeCommit: func(ctx context.Context, prefix string) error {
			return pipInstall(ctx, prefix, pipSpecs)
		},
	}
```

```go
// pipInstall installs specs into the environment at prefix with its own pip.
// It runs inside the transaction: if it fails, or ctx ends, goomba rolls the
// whole environment back.
func pipInstall(ctx context.Context, prefix string, specs []string) error {
	if len(specs) == 0 {
		return nil
	}
	// -I and --isolated keep Python and pip from reading PYTHON* and PIP_*
	// variables, the user's site directory and the user's pip configuration;
	// PIP_CONFIG_FILE=/dev/null keeps pip from reading any configuration
	// file, the system's and the environment's too.
	args := append([]string{"-I", "-m", "pip", "install", "--isolated", "--no-input", "--disable-pip-version-check"}, specs...)
	cmd := exec.CommandContext(ctx, filepath.Join(prefix, "bin", "python"), args...)
	cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE="+os.DevNull)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// CommandContext kills pip when ctx ends, but not the processes pip
	// started; this hook must not return while they still run (see
	// failures.md).
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pip install: %w", err)
	}
	return nil
}
```

`BeforeCommit` runs after every package is linked, compiled and recorded,
and before goomba commits:

- **The environment is not finished yet.** It is locked, so callers that
  ask `CheckComplete` cannot use it until pip has succeeded.
- **Failure rolls back.** If pip fails, or the context ends, the whole
  environment is rolled back.

Killing pip does not kill the build processes it started; jumpboot runs pip
in its own process group for that reason. [failures.md](failures.md#pip-and-other-finishing-work)
lists what else a hook has to handle.

```sh
goomba lock -o env.lock.json python=3.12 pip
go run ./examples/provision env.lock.json ./env six==1.17.0
```

### Reuse and recovery

`ensure` in the same example loops over `CheckComplete` until the prefix
holds a finished environment:

| `CheckComplete` returns | `ensure` |
| --- | --- |
| `nil` | reuses the environment |
| `ErrPrefixLocked` | waits a second and looks again |
| `ErrIncomplete` | calls `RemoveIncomplete`, then looks again; another caller removing or reinstalling it first is fine |
| an error wrapping `fs.ErrNotExist` | calls `InstallLock`. On `ErrPrefixLocked` another installer won the race: it waits and looks again. On `ErrPrefixNotEmpty` it looks again, and returns the error if the prefix is a directory of other files. |
| anything else | returns the error |

The loop is printed in full in
[failures.md](failures.md#a-retry-loop).

## Other tasks

- **List an environment.** `link.ReadCondaMeta(prefix)` returns its
  installed packages. It works on environments from goomba, micromamba or
  conda.
- **Lock an existing environment.** `lock.FromPrefixContext(ctx, prefix,
  platform)` does this without solving. Every package must have a sha256,
  so micromamba's explicit installs, which record none, are refused.
- **Import @EXPLICIT.** `lock.ReadExplicit` reads conda's explicit files,
  and `Explicit.Resolve` turns one into a lockfile.
- **Export @EXPLICIT.** `(*lock.Lockfile).WriteExplicit` writes one.
- **Detect virtual packages yourself.** `virtual.Detect` does this, for
  example to show the user or to adjust them before calling `Lock` with
  `Virtual` set.

`link.ReadCondaMeta` and `lock.FromPrefixContext` do not check that the
environment is finished: call `CheckComplete` first. The CLI refuses
(`lock --from-prefix`) or warns (`list`) when the incomplete marker is
present.

## Errors

Branch on errors with `errors.Is` and `errors.As`; goomba wraps the errors
of the packages it builds on unchanged. The main ones are:

- **`*solve.Unsolvable`:** an explained conflict.
- **Prefix states:** `ErrPrefixNotEmpty`, `ErrPrefixLocked`,
  `ErrUnsafePrefix` and `ErrIncomplete`.
- **Recovery:** from `RemoveIncomplete`, `ErrNotIncomplete` (nothing to
  remove) and `ErrMountPoint` (something is mounted inside the prefix).
- **Unsafe paths:** `*UnsafePathError`, found with `errors.As` behind
  `ErrUnsafePrefix` and the cache errors. Its `Path` names the directory
  another user could change.
- **The machine:** `*VirtualError` for unmet virtual packages, and
  `ErrPlatform` for a lockfile made for another platform.
- **Bad input:** `ErrInvalidOptions`.

[failures.md](failures.md) lists the errors callers branch on, with the
state each leaves and what to do.
