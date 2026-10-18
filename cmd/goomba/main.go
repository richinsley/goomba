// Command goomba creates conda environments without micromamba: from match
// specs, resolved against conda channels, or from lockfiles.
//
// Usage:
//
//	goomba create -p PREFIX [-c CHANNEL]... [--offline] [--repodata-dir DIR]
//	              [--pkgs-dir DIR] [--no-pyc] [--virtual-override NAME=VALUE]...
//	              [--nvidia-smi PATH] [--timeout DURATION] SPEC...
//	goomba create --lock FILE -p PREFIX [--pkgs-dir DIR] [--no-pyc]
//	              [--virtual-from-lock] [--virtual-override NAME=VALUE]...
//	              [--nvidia-smi PATH] [--timeout DURATION]
//	goomba lock [-c CHANNEL]... [--platform P] [--offline] [--repodata-dir DIR]
//	            [--virtual-override NAME=VALUE]... [--nvidia-smi PATH]
//	            [--timeout DURATION] -o FILE SPEC...
//	goomba lock --from-prefix PREFIX -o FILE
//	goomba export --explicit --lock FILE
//	goomba list -p PREFIX
//
// create with SPECs (match specs such as python=3.11 or "numpy >=1.26")
// resolves them against the channels and installs the result into PREFIX,
// which must not exist or be an empty directory of the user's: the
// equivalent of micromamba create -p PREFIX -c CHANNEL SPEC.... Channels
// are names (conda-forge, resolved against https://conda.anaconda.org) or
// https URLs, highest priority first, with strict channel priority; with
// no -c, goomba uses conda-forge and says so. goomba has no configuration
// files. Channel indexes (sharded where the channel publishes one) are
// cached in the repodata directory, by default <user cache
// directory>/goomba/repodata, which must be private to the user; --offline
// uses only that cache for the indexes, but create still downloads the
// package archives the package cache lacks. Flags and specs may come in
// any order; "--" ends the flags.
//
// create --lock installs the lockfile FILE instead, verifying every package
// archive against the lockfile's sha256. SPECs and --lock are mutually
// exclusive. Archives are cached in DIR, by default <user cache
// directory>/goomba/pkgs. The user cache directory is os.UserCacheDir:
// $XDG_CACHE_HOME or ~/.cache on Linux, ~/Library/Caches on macOS,
// %LocalAppData% on Windows; when there is none, --pkgs-dir (and, to
// resolve specs, --repodata-dir) is required. --no-pyc skips
// byte-compiling noarch: python packages.
//
// The packages' dependencies on virtual packages (__glibc, __cuda, ...)
// are resolved and checked against this machine, detected as conda does
// and adjusted by the CONDA_OVERRIDE_<NAME> environment variables for the
// names conda knows (ARCHSPEC, CUDA, CUDA_ARCH, GLIBC, LINUX, OSX, WIN;
// CONDA_OVERRIDE_GLIBC=2.17; an empty value removes the package; other
// names are ignored with a warning) and by --virtual-override NAME=VALUE,
// which wins over them. Both spell names alike: case and a leading "__" do
// not matter. __cuda and __cuda_arch are detected by running nvidia-smi
// from the system locations package virtual knows, never from PATH;
// --nvidia-smi PATH names it instead (an absolute path to a regular file;
// anything else is a usage error). --virtual-from-lock (with --lock only)
// checks against the virtual packages recorded in the lockfile instead
// (the platform's defaults overridden by them, then by the overrides), to
// prepare an environment for another machine. When the lockfile records
// virtual packages that differ from this machine's, create warns.
//
// lock with SPECs resolves them like create and writes the lockfile to FILE
// ("-" is standard output) without installing anything. --platform P
// resolves for another platform, whose virtual packages are then conda's
// defaults for P adjusted by the overrides. lock --from-prefix writes a
// lockfile for an existing environment; an environment of only noarch
// packages is locked for this machine's platform, and an environment whose
// installation did not finish is refused. export writes a lockfile as a
// conda @EXPLICIT file to standard output. list prints the packages
// installed in PREFIX.
//
// Resolving and installing (create, and lock with SPECs) must finish within
// --timeout, 10m by default: a hostile channel can make the solver run for
// very long. A request that has no solution prints why, as a tree, and
// exits 1. Specs, channels or paths that cannot be used (a spec that does
// not parse, a file:// or duplicate channel, credentials in a channel, a
// package cache inside the prefix) are usage errors, checked before
// anything is detected or downloaded.
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
// Text from channels, lockfiles and prefixes is printed with control and
// bidirectional-formatting characters escaped, and URL credentials are
// never printed.
//
// SIGINT, SIGTERM and SIGHUP cancel the running command; an interrupted
// create removes what it installed ("rolling back"). Further signals during
// the rollback are ignored. SIGPIPE is ignored, so a closed output pipe does
// not kill goomba halfway. When the reader of standard output stops early
// (goomba list -p PREFIX | head), goomba exits 1 without a message.
//
// Exit status: 0 on success, 1 on failure, 2 for a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/richinsley/goomba"
	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/solve"
	"github.com/richinsley/goomba/virtual"
)

const usage = `usage:
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
`

// defaultChannel is the channel used when no -c is given.
const defaultChannel = "conda-forge"

// defaultTimeout bounds resolving plus installing.
const defaultTimeout = 10 * time.Minute

// Exit statuses.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	ctx, stop := signalContext(context.Background(), os.Stderr)
	c := &cli{
		stdout:   stdoutWriter{os.Stdout},
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

// errOutputClosed marks a write to standard output that failed because its
// reader went away, as when "goomba list | head" stops reading.
var errOutputClosed = errors.New("standard output closed")

// stdoutWriter is the standard output of the commands that print results.
// A write its reader refused (EPIPE) returns an error wrapping
// errOutputClosed, so that run can tell it from an EPIPE elsewhere, such as
// on a network connection.
type stdoutWriter struct{ w io.Writer }

func (s stdoutWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if errors.Is(err, syscall.EPIPE) {
		err = fmt.Errorf("%w: %w", errOutputClosed, err)
	}
	return n, err
}

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
	var unsolvable *solve.Unsolvable
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errOutputClosed):
		// Whoever read the output stopped early; there is nobody to tell.
		return exitFailure
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(c.stdout, usage)
		return exitOK
	case errors.Is(err, errUsage), errors.Is(err, goomba.ErrInvalidOptions):
		// ErrInvalidOptions: the specs, channels, platform or paths given
		// on the command line are unusable.
		fmt.Fprintf(c.stderr, "%s\n%s", message(err), usage)
		return exitUsage
	case errors.As(err, &unsolvable):
		// solve escapes untrusted text in explanations already; the tree's
		// own line breaks stay.
		fmt.Fprintf(c.stderr, "goomba: the request cannot be satisfied:\n%s\n", terminalSafe(strings.TrimRight(unsolvable.Explanation, "\n")))
		return exitFailure
	}
	fmt.Fprintln(c.stderr, message(err))
	return exitFailure
}

// message formats err for the terminal with one "goomba: " prefix (the
// library's errors carry one already), with anything that could drive the
// terminal escaped.
func message(err error) string {
	m := err.Error()
	if !strings.HasPrefix(m, "goomba: ") {
		m = "goomba: " + m
	}
	return terminalSafe(m)
}

// terminalSafe escapes what could change how a terminal shows text: control
// characters other than newline and tab, DEL and the C1 controls (as
// \xNN), Unicode format characters such as the bidirectional overrides,
// and the line and paragraph separators (as \uNNNN), and bytes that are
// not UTF-8 (as \xNN). Everything else is kept, so text solve or lock
// escaped already passes unchanged.
func terminalSafe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
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
	return checkRequired(fs, required)
}

// checkRequired reports the first of the required flags that is empty.
func checkRequired(fs *flag.FlagSet, required map[string]*string) error {
	for _, name := range []string{"lock", "p", "from-prefix", "o"} {
		if v, ok := required[name]; ok && *v == "" {
			return fmt.Errorf("%w: %s: -%s is required", errUsage, fs.Name(), name)
		}
	}
	return nil
}

// parseMixed parses fs's flags from args, in which flags and positional
// arguments may be interleaved (goomba lock python -c conda-forge -o -),
// and returns the positional arguments in order. It splits args as package
// flag would: a known flag that takes a value takes the next argument,
// whatever it looks like ("-c -o" sets -c to "-o"); "-" alone is
// positional; and everything after a "--" that is not a flag's value is
// positional, flags or not.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name, hasValue := strings.CutPrefix(a[1:], "-")
		if !hasValue {
			name = a[1:]
		}
		name, _, hasValue = strings.Cut(name, "=")
		if hasValue {
			continue
		}
		// An unknown or malformed flag is left to fs.Parse to report.
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s: %v", errUsage, fs.Name(), err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: %s: unexpected argument %q", errUsage, fs.Name(), fs.Arg(0))
	}
	return pos, nil
}

// isBoolFlag reports whether f takes no value.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
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
	rf := addResolveFlags(fs)
	specs, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	if err := checkRequired(fs, map[string]*string{"p": prefix}); err != nil {
		return err
	}
	if err := checkLineBreaks(fs, specs); err != nil {
		return err
	}
	switch {
	case *lockFile != "" && len(specs) > 0:
		return fmt.Errorf("%w: create: SPECs and --lock are mutually exclusive", errUsage)
	case *lockFile == "" && len(specs) == 0:
		return fmt.Errorf("%w: create: --lock FILE or SPECs to resolve are required", errUsage)
	case *lockFile == "" && *fromLock:
		return fmt.Errorf("%w: create: --virtual-from-lock checks against a lockfile's virtual packages; it needs --lock", errUsage)
	case *lockFile != "" && rf.used():
		return fmt.Errorf("%w: create: -c, --offline and --repodata-dir resolve SPECs; they have no use with --lock", errUsage)
	}
	if err := rf.check("create"); err != nil {
		return err
	}
	overrides, err := c.overrides(overrideFlags)
	if err != nil {
		return err
	}
	if *fromLock && *nvidiaSMI != "" {
		return fmt.Errorf("%w: create: --nvidia-smi detects this machine's __cuda; it has no use with --virtual-from-lock", errUsage)
	}
	if *pkgsDir, err = c.defaultDir(*pkgsDir, "pkgs", "create", "--pkgs-dir"); err != nil {
		return err
	}
	abs, err := filepath.Abs(*prefix)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *rf.timeout)
	defer cancel()
	var client *http.Client
	if c.client != nil {
		if client, err = c.client(); err != nil {
			return err
		}
	}
	opt := goomba.InstallOptions{
		PkgsDir:    *pkgsDir,
		Client:     client,
		CompilePyc: !*noPyc,
		Progress:   syncProgress(c.installProgress),
		Cmd:        "goomba create " + strings.Join(args, " "),
	}
	// Refuse a prefix or options that cannot work before detecting virtual
	// packages (nvidia-smi) or saying anything about channels.
	if err := goomba.ValidateInstall(abs, opt); err != nil {
		return err
	}
	n := 0
	if *lockFile != "" {
		lf, err := readLockfile(*lockFile)
		if err != nil {
			return err
		}
		if opt.Virtual, err = c.virtualSet(ctx, lf, *fromLock, overrides, *nvidiaSMI); err != nil {
			return timedOut(err, *rf.timeout)
		}
		if err := goomba.InstallLock(ctx, abs, lf, opt); err != nil {
			return timedOut(err, *rf.timeout)
		}
		n = len(lf.Packages)
	} else {
		lo, err := c.lockOptions(ctx, rf, specs, "", overrides, *nvidiaSMI, client)
		if err != nil {
			return timedOut(err, *rf.timeout)
		}
		opt.Virtual = lo.Virtual
		lf, err := goomba.Create(ctx, abs, lo, opt)
		if err != nil {
			return timedOut(err, *rf.timeout)
		}
		n = len(lf.Packages)
	}
	fmt.Fprintf(c.stderr, "created %s (%d packages)\n", terminalSafe(abs), n)
	return nil
}

// checkLineBreaks refuses arguments holding a line break, which the
// environment's history (one "# cmd:" line) cannot record, naming the
// SPEC, or only the flag of a flag's value (a channel URL could hold a
// secret).
func checkLineBreaks(fs *flag.FlagSet, specs []string) error {
	for _, s := range specs {
		if strings.ContainsAny(s, "\r\n") {
			return fmt.Errorf("%w: %s: SPEC %q contains a line break", errUsage, fs.Name(), s)
		}
	}
	var bad string
	fs.Visit(func(f *flag.Flag) {
		if bad == "" && strings.ContainsAny(f.Value.String(), "\r\n") {
			bad = f.Name
		}
	})
	if bad != "" {
		return fmt.Errorf("%w: %s: the value of -%s contains a line break", errUsage, fs.Name(), bad)
	}
	return nil
}

// resolveFlags are the flags of create and lock that resolve SPECs.
type resolveFlags struct {
	channels    listFlag
	offline     *bool
	repodataDir *string
	timeout     *time.Duration
}

func addResolveFlags(fs *flag.FlagSet) *resolveFlags {
	rf := &resolveFlags{}
	fs.Var(&rf.channels, "c", "channel name or https URL, highest priority first (repeatable; default conda-forge)")
	rf.offline = fs.Bool("offline", false, "use only the cached channel indexes (create still downloads package archives it does not have)")
	rf.repodataDir = fs.String("repodata-dir", "", "channel index cache (default: <user cache dir>/goomba/repodata)")
	rf.timeout = fs.Duration("timeout", defaultTimeout, "time limit for resolving and installing")
	return rf
}

// used reports whether a flag that only matters for resolving was given.
func (rf *resolveFlags) used() bool {
	return len(rf.channels) > 0 || *rf.offline || *rf.repodataDir != ""
}

func (rf *resolveFlags) check(cmd string) error {
	if *rf.timeout <= 0 {
		return fmt.Errorf("%w: %s: --timeout must be positive", errUsage, cmd)
	}
	return nil
}

// defaultDir returns dir, or <user cache dir>/goomba/<name> when dir is
// empty; with no user cache directory, flag is required.
func (c *cli) defaultDir(dir, name, cmd, flag string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	base, err := "", errors.New("no user cache directory")
	if c.cacheDir != nil {
		base, err = c.cacheDir()
	}
	if err != nil || base == "" {
		return "", fmt.Errorf("%w: %s: %s is required: %v", errUsage, cmd, flag, err)
	}
	return filepath.Join(base, "goomba", name), nil
}

// lockOptions builds the resolve options of create and lock: the channels
// (conda-forge, with a notice, when none is given), the index cache, and
// the virtual packages of platform ("" for this machine): detected with
// the overrides for this machine, conda's defaults with the overrides for
// another platform.
func (c *cli) lockOptions(ctx context.Context, rf *resolveFlags, specs []string, platform string, overrides map[string]string, nvidiaSMI string, client *http.Client) (goomba.LockOptions, error) {
	cmd := "create"
	if platform != "" {
		cmd = "lock"
	}
	dir, err := c.defaultDir(*rf.repodataDir, "repodata", cmd, "--repodata-dir")
	if err != nil {
		return goomba.LockOptions{}, err
	}
	channels := []string(rf.channels)
	if len(channels) == 0 {
		channels = []string{defaultChannel}
	}
	lo := goomba.LockOptions{
		Specs:       specs,
		Channels:    channels,
		Platform:    platform,
		Virtual:     []*record.PackageRecord{}, // set below
		RepodataDir: dir,
		Client:      client,
		Offline:     *rf.offline,
		Progress:    c.lockProgress(),
	}
	// Refuse unusable specs and channels before detecting virtual
	// packages or saying anything about channels.
	if err := goomba.ValidateLock(lo); err != nil {
		return goomba.LockOptions{}, err
	}
	if len(rf.channels) == 0 {
		fmt.Fprintf(c.stderr, "goomba: no channel given (-c); using %s\n", defaultChannel)
	}
	host := virtual.HostPlatform()
	var virt []*record.PackageRecord
	if platform == "" || platform == host {
		detect := c.virtual
		if detect == nil {
			detect = virtual.Detect
		}
		virt, err = detect(ctx, virtual.Options{Platform: host, Overrides: overrides, NvidiaSMI: nvidiaSMI})
		if errors.Is(err, virtual.ErrInvalidOption) {
			return goomba.LockOptions{}, fmt.Errorf("%w: %s: --nvidia-smi: %v", errUsage, cmd, err)
		}
	} else {
		if nvidiaSMI != "" {
			return goomba.LockOptions{}, fmt.Errorf("%w: %s: --nvidia-smi detects this machine's __cuda; it has no use with --platform %s", errUsage, cmd, platform)
		}
		virt, err = virtual.ApplyOverrides(platform, virtual.Defaults(platform), overrides)
	}
	if err != nil {
		return goomba.LockOptions{}, fmt.Errorf("virtual packages: %w", err)
	}
	if virt != nil {
		lo.Virtual = virt
	}
	return lo, nil
}

// lockProgress returns a printer of the resolve's progress. When a
// prefiltered solve found no solution, the retry is announced before the
// channels load again.
func (c *cli) lockProgress() func(goomba.Event) {
	prefilter, announced := "", false
	announce := func() {
		if prefilter != "" && !announced {
			announced = true
			fmt.Fprintf(c.stderr, "no solution with the python %s prefilter; solving again without it\n", terminalSafe(prefilter))
		}
	}
	return func(e goomba.Event) {
		switch e.Kind {
		case goomba.EventRepodata:
			if e.Prefilter == "" {
				announce()
			}
			fmt.Fprintf(c.stderr, "loaded %s (%d/%d)\n", terminalSafe(e.Channel), e.Done, e.Total)
		case goomba.EventSolving:
			switch {
			case e.Done > 1:
				announce()
				fmt.Fprintf(c.stderr, "solving\n")
			case e.Prefilter != "":
				prefilter = e.Prefilter
				fmt.Fprintf(c.stderr, "solving (skipping packages for other python than %s)\n", terminalSafe(e.Prefilter))
			default:
				fmt.Fprintf(c.stderr, "solving\n")
			}
		case goomba.EventSolved:
			fmt.Fprintf(c.stderr, "solved: %d packages\n", e.Total)
		}
	}
}

// installProgress prints an installation's progress.
func (c *cli) installProgress(e goomba.Event) {
	switch e.Kind {
	case goomba.EventFetched:
		fmt.Fprintf(c.stderr, "fetched %s (%d/%d)\n", terminalSafe(e.Package), e.Done, e.Total)
	case goomba.EventLinked:
		fmt.Fprintf(c.stderr, "linked %s (%d/%d)\n", terminalSafe(e.Package), e.Done, e.Total)
	case goomba.EventCompiling:
		fmt.Fprintf(c.stderr, "compiling %d python files\n", e.Total)
	case goomba.EventCompiled:
		if e.Done < e.Total {
			fmt.Fprintf(c.stderr, "compiled %d of %d python files\n", e.Done, e.Total)
		}
	}
}

// timedOut explains an error caused by the --timeout deadline.
func timedOut(err error, d time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %v (--timeout): %w", d, err)
	}
	return err
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
		// A lockfile solved for a platform's conservative defaults
		// (virtual.Defaults) is meant for any machine of the platform, so it
		// draws no warning; either way the packages are checked against
		// this machine below.
		if err == nil && len(lf.Virtual) > 0 && !maps.Equal(lf.Virtual, virtual.ToOverrides(virtual.Defaults(lf.Platform))) {
			if diff := virtualDiff(lf.Virtual, virtual.ToOverrides(recs)); diff != "" {
				fmt.Fprintf(c.stderr, "goomba: warning: the lockfile was solved for other virtual packages than this machine has (%s); its packages are checked against this machine's (--virtual-from-lock checks against the lockfile's instead, to prepare an environment for another machine)\n", diff)
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
	platform := fs.String("platform", "", "platform to resolve for (default: this machine's)")
	nvidiaSMI := fs.String("nvidia-smi", "", "absolute path of nvidia-smi, to detect __cuda and __cuda_arch")
	var overrideFlags listFlag
	fs.Var(&overrideFlags, "virtual-override", "NAME=VALUE: override a virtual package (repeatable; empty VALUE removes it)")
	rf := addResolveFlags(fs)
	specs, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	if err := checkRequired(fs, map[string]*string{"o": out}); err != nil {
		return err
	}
	if *prefix != "" {
		if len(specs) > 0 || rf.used() || *platform != "" || len(overrideFlags) > 0 || *nvidiaSMI != "" {
			return fmt.Errorf("%w: lock: --from-prefix locks an existing environment; it takes no SPECs or resolve flags", errUsage)
		}
		return c.lockPrefix(ctx, *prefix, *out)
	}
	if len(specs) == 0 {
		return fmt.Errorf("%w: lock: SPECs to resolve or --from-prefix PREFIX are required", errUsage)
	}
	if err := rf.check("lock"); err != nil {
		return err
	}
	host := virtual.HostPlatform()
	if *platform == "" {
		if host == "" {
			return fmt.Errorf("%w: lock: this machine has no conda platform; --platform is required", errUsage)
		}
		*platform = host
	}
	if !virtual.KnownPlatform(*platform) {
		return fmt.Errorf("%w: lock: --platform %q is not a conda platform", errUsage, *platform)
	}
	overrides, err := c.overrides(overrideFlags)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *rf.timeout)
	defer cancel()
	var client *http.Client
	if c.client != nil {
		if client, err = c.client(); err != nil {
			return err
		}
	}
	opt, err := c.lockOptions(ctx, rf, specs, *platform, overrides, *nvidiaSMI, client)
	if err != nil {
		return timedOut(err, *rf.timeout)
	}
	lf, err := goomba.Lock(ctx, opt)
	if err != nil {
		return timedOut(err, *rf.timeout)
	}
	if *out == "-" {
		return lf.Write(c.stdout)
	}
	return writeFileAtomic(*out, lf.Write)
}

// lockPrefix writes a lockfile for the environment in prefix.
func (c *cli) lockPrefix(ctx context.Context, prefix, out string) error {
	abs, err := filepath.Abs(prefix)
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
	if out == "-" {
		return lf.Write(c.stdout)
	}
	return writeFileAtomic(out, lf.Write)
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
		if errors.Is(goomba.CheckComplete(abs), goomba.ErrPrefixLocked) {
			fmt.Fprintf(c.stderr, "goomba: warning: %s is incomplete: a goomba process is installing or removing it right now\n", terminalSafe(abs))
		} else {
			fmt.Fprintf(c.stderr, "goomba: warning: %s is incomplete: an installation into it did not finish\n", terminalSafe(abs))
		}
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Name\tVersion\tBuild\tChannel")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cell(r.Record.Name), cell(r.Record.Version), cell(r.Record.Build), cell(channelName(r.Record.Channel)))
	}
	return tw.Flush()
}

// cell escapes a value read from a prefix for one cell of list's table:
// as terminalSafe does, and tabs and line breaks too, which would break
// the table.
func cell(s string) string {
	return strings.NewReplacer("\t", `\t`, "\n", `\n`).Replace(terminalSafe(s))
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
