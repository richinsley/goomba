// Command goomba creates conda environments from lockfiles without
// micromamba.
//
// Usage:
//
//	goomba create --lock FILE -p PREFIX [--pkgs-dir DIR] [--no-pyc]
//	              [--virtual-from-lock] [--virtual-override NAME=VALUE]...
//	              [--nvidia-smi PATH]
//	goomba lock --from-prefix PREFIX -o FILE
//	goomba export --explicit --lock FILE
//	goomba list -p PREFIX
//
// create installs the lockfile FILE into PREFIX, which must not exist or be
// an empty directory of the user's, verifying every package archive
// against the lockfile's sha256. Archives are cached in DIR, by default
// <user cache directory>/goomba/pkgs (os.UserCacheDir: $XDG_CACHE_HOME or
// ~/.cache on Linux, ~/Library/Caches on macOS, %LocalAppData% on Windows);
// when there is none, --pkgs-dir is required. --no-pyc skips byte-compiling
// noarch: python packages.
//
// The lockfile's dependencies on virtual packages (__glibc, __cuda, ...) are
// checked against this machine, detected as conda does and adjusted by the
// CONDA_OVERRIDE_<NAME> environment variables for the names conda knows
// (ARCHSPEC, CUDA, CUDA_ARCH, GLIBC, LINUX, OSX, WIN; CONDA_OVERRIDE_GLIBC=2.17;
// an empty value removes the package; other names are ignored with a
// warning) and by --virtual-override NAME=VALUE, which wins over them. Both
// spell names alike: case and a leading "__" do not matter. __cuda and
// __cuda_arch are detected by running nvidia-smi from the system locations
// package virtual knows, never from PATH; --nvidia-smi PATH names it instead
// (an absolute path to a regular file; anything else is a usage error). --virtual-from-lock checks against the virtual
// packages recorded in the lockfile instead (the platform's defaults
// overridden by them, then by the overrides), to prepare an environment
// for another machine. When the lockfile records virtual packages that
// differ from this machine's, create warns.
//
// lock writes a lockfile for an existing environment (FILE "-" is standard
// output); an environment of only noarch packages is locked for this
// machine's platform, and an environment whose installation did not finish
// is refused. export writes a lockfile as a conda @EXPLICIT file to
// standard output. list prints the packages installed in PREFIX.
//
// The environment is read for HTTPS_PROXY and NO_PROXY (or their lower-case
// forms; through net/http's ProxyFromEnvironment, which for the https
// downloads goomba makes uses no other variable; a value net/http would
// silently ignore or misread is an error instead), CONDA_OVERRIDE_*, and the
// variables os.UserCacheDir consults. Go's TLS stack also honours
// SSL_CERT_FILE and SSL_CERT_DIR, which choose the trusted certificate
// authorities on Linux and other Unix systems. Nothing else in the
// environment, and no configuration file, changes what goomba does.
//
// SIGINT, SIGTERM and SIGHUP cancel the running command; an interrupted
// create removes what it installed ("rolling back"). Further signals during
// the rollback are ignored. SIGPIPE is ignored, so a closed output pipe does
// not kill goomba halfway.
//
// Exit status: 0 on success, 1 on failure, 2 for a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/richinsley/goomba"
	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/virtual"
)

const usage = `usage:
  goomba create --lock FILE -p PREFIX [--pkgs-dir DIR] [--no-pyc]
                [--virtual-from-lock] [--virtual-override NAME=VALUE]...
                [--nvidia-smi PATH]
  goomba lock --from-prefix PREFIX -o FILE
  goomba export --explicit --lock FILE
  goomba list -p PREFIX
`

// Exit statuses.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	ctx, stop := signalContext(context.Background(), os.Stderr)
	c := &cli{
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		client:   func() (*http.Client, error) { return proxyClient(os.Getenv, os.Stderr) },
		environ:  os.Environ,
		cacheDir: os.UserCacheDir,
	}
	code := c.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// cli runs one goomba command. Its fields are the process's outside world,
// replaced in tests.
type cli struct {
	stdout, stderr io.Writer
	// client returns the HTTP client for downloads; nil selects fetch's
	// default.
	client func() (*http.Client, error)
	// environ lists the environment, for CONDA_OVERRIDE_*; nil means none.
	environ func() []string
	// cacheDir returns the user's cache directory; nil means none.
	cacheDir func() (string, error)
	// virtual detects this machine's virtual packages; nil selects
	// virtual.Detect.
	virtual func(context.Context, virtual.Options) ([]*record.PackageRecord, error)
}

// errUsage marks a command line error.
var errUsage = errors.New("usage error")

func (c *cli) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, usage)
		return exitUsage
	}
	var err error
	switch args[0] {
	case "create":
		err = c.create(ctx, args[1:])
	case "lock":
		err = c.lock(ctx, args[1:])
	case "export":
		err = c.export(args[1:])
	case "list":
		err = c.list(args[1:])
	case "help", "-h", "--help", "-help":
		fmt.Fprint(c.stdout, usage)
		return exitOK
	default:
		err = fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(c.stdout, usage)
		return exitOK
	case errors.Is(err, errUsage):
		fmt.Fprintf(c.stderr, "%s\n%s", message(err), usage)
		return exitUsage
	}
	fmt.Fprintln(c.stderr, message(err))
	return exitFailure
}

// message formats err for the terminal with one "goomba: " prefix (the
// library's errors carry one already).
func message(err error) string {
	m := err.Error()
	if !strings.HasPrefix(m, "goomba: ") {
		m = "goomba: " + m
	}
	return m
}

// parse parses a subcommand's flags, refusing positional arguments and
// missing required flags.
func parse(fs *flag.FlagSet, args []string, required map[string]*string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %s: %v", errUsage, fs.Name(), err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: %s: unexpected argument %q", errUsage, fs.Name(), fs.Arg(0))
	}
	for _, name := range []string{"lock", "p", "from-prefix", "o"} {
		if v, ok := required[name]; ok && *v == "" {
			return fmt.Errorf("%w: %s: -%s is required", errUsage, fs.Name(), name)
		}
	}
	return nil
}

// listFlag collects the values of a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func (c *cli) create(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	lockFile := fs.String("lock", "", "lockfile to install")
	prefix := fs.String("p", "", "environment prefix to create")
	pkgsDir := fs.String("pkgs-dir", "", "package cache (default: <user cache dir>/goomba/pkgs)")
	noPyc := fs.Bool("no-pyc", false, "do not byte-compile noarch: python packages")
	fromLock := fs.Bool("virtual-from-lock", false, "check virtual packages against the lockfile's, not this machine's")
	nvidiaSMI := fs.String("nvidia-smi", "", "absolute path of nvidia-smi, to detect __cuda and __cuda_arch")
	var overrideFlags listFlag
	fs.Var(&overrideFlags, "virtual-override", "NAME=VALUE: override a virtual package (repeatable; empty VALUE removes it)")
	if err := parse(fs, args, map[string]*string{"lock": lockFile, "p": prefix}); err != nil {
		return err
	}
	overrides, err := c.overrides(overrideFlags)
	if err != nil {
		return err
	}
	if *fromLock && *nvidiaSMI != "" {
		return fmt.Errorf("%w: create: --nvidia-smi detects this machine's __cuda; it has no use with --virtual-from-lock", errUsage)
	}
	if *pkgsDir == "" {
		dir, err := "", errors.New("no user cache directory")
		if c.cacheDir != nil {
			dir, err = c.cacheDir()
		}
		if err != nil || dir == "" {
			return fmt.Errorf("%w: create: --pkgs-dir is required: %v", errUsage, err)
		}
		*pkgsDir = filepath.Join(dir, "goomba", "pkgs")
	}
	lf, err := readLockfile(*lockFile)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(*prefix)
	if err != nil {
		return err
	}
	virt, err := c.virtualSet(ctx, lf, *fromLock, overrides, *nvidiaSMI)
	if err != nil {
		return err
	}
	var client *http.Client
	if c.client != nil {
		if client, err = c.client(); err != nil {
			return err
		}
	}
	progress := func(e goomba.Event) {
		switch e.Kind {
		case goomba.EventFetched:
			fmt.Fprintf(c.stderr, "fetched %s (%d/%d)\n", e.Package, e.Done, e.Total)
		case goomba.EventLinked:
			fmt.Fprintf(c.stderr, "linked %s (%d/%d)\n", e.Package, e.Done, e.Total)
		case goomba.EventCompiling:
			fmt.Fprintf(c.stderr, "compiling %d python files\n", e.Total)
		case goomba.EventCompiled:
			if e.Done < e.Total {
				fmt.Fprintf(c.stderr, "compiled %d of %d python files\n", e.Done, e.Total)
			}
		}
	}
	opt := goomba.InstallOptions{
		PkgsDir:    *pkgsDir,
		Client:     client,
		CompilePyc: !*noPyc,
		Virtual:    virt,
		Progress:   syncProgress(progress),
		Cmd:        "goomba create " + strings.Join(args, " "),
	}
	if err := goomba.InstallLock(ctx, abs, lf, opt); err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "created %s (%d packages)\n", abs, len(lf.Packages))
	return nil
}

// knownOverrides lists the CONDA_OVERRIDE_<NAME> names honoured, as in
// conda and libmamba (virtual_packages.cpp), plus cuda_arch.
const knownOverrides = "archspec, cuda, cuda_arch, glibc, linux, osx, win"

// isKnownOverride reports whether name (normalised) is in knownOverrides.
func isKnownOverride(name string) bool {
	switch name {
	case "archspec", "cuda", "cuda_arch", "glibc", "linux", "osx", "win":
		return true
	}
	return false
}

// overrides merges the CONDA_OVERRIDE_<NAME> environment variables and the
// --virtual-override flags (which win) into virtual.Options.Overrides form.
// Both spell a package the same way: lower-case, without a leading "__"
// (CONDA_OVERRIDE___CUDA and --virtual-override __CUDA=... both mean
// "cuda"), and values are trimmed. Environment variables for other names
// are ignored with a warning, as conda ignores them.
func (c *cli) overrides(flags []string) (map[string]string, error) {
	out := map[string]string{}
	if c.environ != nil {
		env := slices.Clone(c.environ())
		slices.Sort(env)
		for _, kv := range env {
			k, v, ok := strings.Cut(kv, "=")
			name, isOverride := strings.CutPrefix(k, "CONDA_OVERRIDE_")
			if !ok || !isOverride {
				continue
			}
			key := overrideKey(name)
			if !isKnownOverride(key) {
				fmt.Fprintf(c.stderr, "goomba: warning: ignoring %s: not a virtual package override (%s)\n", printable(k), knownOverrides)
				continue
			}
			out[key] = strings.TrimSpace(v)
		}
	}
	for _, f := range flags {
		k, v, ok := strings.Cut(f, "=")
		key := overrideKey(k)
		if !ok || key == "" {
			return nil, fmt.Errorf("%w: create: --virtual-override %q is not NAME=VALUE", errUsage, f)
		}
		out[key] = strings.TrimSpace(v)
	}
	return out, nil
}

// overrideKey normalises a virtual package name for the overrides map.
func overrideKey(name string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "__")
}

// virtualSet returns the virtual packages to check the lockfile against:
// this machine's, or with fromLock the lockfile's recorded ones (the
// platform's defaults overridden by the record), with overrides applied.
// It warns when the lockfile records a set that differs from this
// machine's.
func (c *cli) virtualSet(ctx context.Context, lf *lock.Lockfile, fromLock bool, overrides map[string]string, nvidiaSMI string) ([]*record.PackageRecord, error) {
	var recs []*record.PackageRecord
	var err error
	if fromLock {
		if recs, err = virtual.ApplyOverrides(lf.Platform, virtual.Defaults(lf.Platform), lf.Virtual); err == nil {
			recs, err = virtual.ApplyOverrides(lf.Platform, recs, overrides)
		}
	} else {
		detect := c.virtual
		if detect == nil {
			detect = virtual.Detect
		}
		recs, err = detect(ctx, virtual.Options{Platform: lf.Platform, Overrides: overrides, NvidiaSMI: nvidiaSMI})
		if errors.Is(err, virtual.ErrInvalidOption) {
			return nil, fmt.Errorf("%w: create: --nvidia-smi: %v", errUsage, err)
		}
		if err == nil && len(lf.Virtual) > 0 {
			if diff := virtualDiff(lf.Virtual, virtual.ToOverrides(recs)); diff != "" {
				fmt.Fprintf(c.stderr, "goomba: warning: the lockfile was solved for other virtual packages than this machine has (%s); use --virtual-from-lock to check against the lockfile's\n", diff)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("virtual packages: %w", err)
	}
	if recs == nil {
		recs = []*record.PackageRecord{}
	}
	return recs, nil
}

// virtualDiff describes how the recorded set locked differs from the set
// have, in name order, or returns "".
func virtualDiff(locked, have map[string]string) string {
	names := map[string]bool{}
	for k := range locked {
		names[k] = true
	}
	for k := range have {
		names[k] = true
	}
	var keys []string
	for k := range names {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		l, lok := locked[k]
		h, hok := have[k]
		switch {
		case lok && hok && l == h:
		case !hok:
			parts = append(parts, fmt.Sprintf("%s %s locked, none here", printable(k), printable(l)))
		case !lok:
			parts = append(parts, fmt.Sprintf("%s %s here, not locked", printable(k), printable(h)))
		default:
			parts = append(parts, fmt.Sprintf("%s %s locked, %s here", printable(k), printable(l), printable(h)))
		}
	}
	return strings.Join(parts, "; ")
}

func (c *cli) lock(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	prefix := fs.String("from-prefix", "", "environment prefix to lock")
	out := fs.String("o", "", "lockfile to write (- for standard output)")
	if err := parse(fs, args, map[string]*string{"from-prefix": prefix, "o": out}); err != nil {
		return err
	}
	abs, err := filepath.Abs(*prefix)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(abs, "conda-meta", ".goomba-incomplete")); err == nil {
		return fmt.Errorf("%s is incomplete: an installation into it did not finish or is still running; not locking it", abs)
	}
	// The platform is inferred from the records; an environment of only
	// noarch packages is taken to be for this machine.
	recs, err := link.ReadCondaMeta(abs)
	if err != nil {
		return err
	}
	platform := virtual.HostPlatform()
	for _, r := range recs {
		if r.Record.Subdir != "" && r.Record.Subdir != "noarch" {
			platform = ""
			break
		}
	}
	lf, err := lock.FromPrefixContext(ctx, abs, platform)
	if err != nil {
		return err
	}
	if *out == "-" {
		return lf.Write(c.stdout)
	}
	return writeFileAtomic(*out, lf.Write)
}

func (c *cli) export(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	explicit := fs.Bool("explicit", false, "write a conda @EXPLICIT file")
	lockFile := fs.String("lock", "", "lockfile to export")
	if err := parse(fs, args, map[string]*string{"lock": lockFile}); err != nil {
		return err
	}
	if !*explicit {
		return fmt.Errorf("%w: export: --explicit is required (the only export format)", errUsage)
	}
	lf, err := readLockfile(*lockFile)
	if err != nil {
		return err
	}
	return lf.WriteExplicit(c.stdout)
}

func (c *cli) list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	prefix := fs.String("p", "", "environment prefix")
	if err := parse(fs, args, map[string]*string{"p": prefix}); err != nil {
		return err
	}
	abs, err := filepath.Abs(*prefix)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Join(abs, "conda-meta")); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a conda environment (no conda-meta directory)", abs)
	}
	recs, err := link.ReadCondaMeta(abs)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(abs, "conda-meta", ".goomba-incomplete")); err == nil {
		fmt.Fprintf(c.stderr, "goomba: warning: %s is incomplete: an installation into it did not finish\n", abs)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Name\tVersion\tBuild\tChannel")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", printable(r.Record.Name), printable(r.Record.Version), printable(r.Record.Build), printable(channelName(r.Record.Channel)))
	}
	return tw.Flush()
}

// channelName shortens channels below conda.anaconda.org to their name, as
// micromamba lists them.
func channelName(ch string) string {
	if name, ok := strings.CutPrefix(ch, "https://conda.anaconda.org/"); ok && name != "" {
		return name
	}
	return ch
}

// printable replaces control characters, so that metadata read from a prefix
// cannot drive the terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return '?'
		}
		return r
	}, s)
}

// readLockfile reads and validates a goomba lockfile.
func readLockfile(name string) (*lock.Lockfile, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lf, err := lock.Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return lf, nil
}

// writeFileAtomic writes name through a temporary file in its directory.
func writeFileAtomic(name string, write func(io.Writer) error) error {
	dir, base := filepath.Split(name)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	werr := write(f)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(f.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(f.Name(), name)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
	}
	return werr
}
