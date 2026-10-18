// The python-minor prefilter and the retry of an unsolvable request follow
// libmamba src/api/utils.cpp (the prefilter choice in the solver context
// setup, handle_unsolvable_with_retry) (BSD-3-Clause, see NOTICE). Unlike
// libmamba, whose retry keeps the prefilter of an explicitly requested
// python minor, the retry here always drops it, so the prefilter can never
// be the reason a request is unsolvable.

package goomba

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richinsley/goomba/internal/netguard"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/solve"
	"github.com/richinsley/goomba/spec"
	"github.com/richinsley/goomba/virtual"
)

// LockOptions describes what Lock resolves and where from. Lock only reads
// it, so one value may be shared by concurrent calls as long as nobody
// modifies it.
//
// Memory: each channel bounds the index data it keeps with repodata's
// default MaxRetained (repodata.DefaultMaxRetained, 2 GiB, which
// conda-forge's whole full index stays under); there is no bound across
// channels, so n channels may keep up to n times that. Sharded channels
// keep only the shards the solve reaches (about 2 MB for conda-forge's
// python=3.12).
type LockOptions struct {
	// Specs are the match specs to resolve, e.g. "python=3.11" or
	// "numpy>=1.26"; at least one. They become the lockfile's specs.
	Specs []string
	// Constraints are root constraints: a package they name must match
	// them if it is installed, but nothing requires it to be. They are not
	// recorded in the lockfile.
	Constraints []string
	// Channels are channel names ("conda-forge", "conda-forge/label/dev")
	// or https URLs, highest priority first; at least one. Priority is
	// strict: every package comes from the first channel that lists its
	// name. Platform filters ("conda-forge/linux-64") are refused: the
	// platform is Platform.
	Channels []string
	// ChannelAlias resolves channel names, here and in channel-qualified
	// specs ("conda-forge::numpy"); "" means https://conda.anaconda.org.
	ChannelAlias string
	// Platform is the subdir to resolve for, e.g. "linux-64"; "" means
	// virtual.HostPlatform(). The noarch subdir is always used too.
	Platform string
	// Virtual are the virtual packages (__glibc, __cuda, ...) of the
	// machine the environment is for. nil selects virtual.Detect for the
	// host; for another Platform it is required (conda's defaults for a
	// platform p are virtual.Defaults(p)). An empty, non-nil slice means
	// none. The lockfile records them (see lock.Lockfile.Virtual).
	Virtual []*record.PackageRecord
	// RepodataDir is the index cache directory (repodata.Options.CacheDir);
	// required. It must be private to the user (repodata.ErrUnsafeCache).
	RepodataDir string
	// Client performs the index downloads. nil selects repodata's default
	// client, which uses no proxy: proxies come from the caller's Client,
	// never from the environment.
	Client *http.Client
	// Offline uses only the index cache, whatever its age; a channel the
	// cache lacks is an error wrapping repodata.ErrNotCached.
	Offline bool
	// MaxAge is how long a cached index file is used without asking the
	// server (repodata.Options.MaxAge); zero revalidates every time.
	// Shards never expire.
	MaxAge time.Duration
	// Progress, if set, receives EventRepodata, EventSolving and
	// EventSolved events, always on the goroutine that called Lock or
	// Create; a panic in it propagates to that caller.
	Progress func(Event)
}

// maxLockAttempts is how many solves Lock may run: with the python-minor
// prefilter, then without it.
const maxLockAttempts = 2

// Lock resolves opt.Specs against opt.Channels into a lockfile for
// opt.Platform, without micromamba:
//
//  1. The channels are resolved with spec.ParseChannelAlias against
//     opt.ChannelAlias and opened with repodata.NewChannel (the sharded
//     index when the channel publishes one, else the full repodata.json)
//     for the platform and noarch, concurrently, and combined with
//     repodata.NewMulti in the given order: strict channel priority.
//  2. When the specs pin python to one minor series (see
//     solve.RequestedPythonMinor), records whose python dependency excludes
//     that series are dropped while the indexes load
//     (repodata.Options.PythonMinor). If the request is then unsolvable,
//     the channels are opened again without the prefilter and the request
//     solved once more, as libmamba does; the cached index makes the retry
//     cheap. EventSolving and EventSolved tell which attempt ran and
//     succeeded.
//  3. solve.Solve resolves the specs and constraints with the virtual
//     packages and the channel alias. Lock imposes no deadline: solving is
//     NP-complete and a hostile channel can make it take very long, so
//     callers should pass a ctx with one.
//  4. lock.FromRecords builds the lockfile: the platform; the channels'
//     canonical URLs in priority order (followed by the channel URL of any
//     package whose index placed it under another URL, through a base_url,
//     in package name order); the specs; the virtual packages as
//     virtual.ToOverrides records them; and the solved packages. A
//     lockfile holds package URLs of the form <channel>/<subdir>/<file>
//     only, so a package whose index base_url does not end in /<subdir>/
//     (which CEP 15 allows) cannot be locked: Lock fails closed with an
//     error wrapping lock.ErrInvalid.
//
// The same channels, index cache state and options give a byte-identical
// lockfile. An unsolvable request returns an error that errors.As matches
// as *solve.Unsolvable, whose explanation is safe to print (solve escapes
// untrusted text). Invalid options are ErrInvalidOptions errors, raised
// before any network access; errors of the packages Lock builds on are
// wrapped unchanged (repodata.ErrUnsafeCache, repodata.ErrNotCached,
// solve.ErrInvalidRequest, lock.ErrInvalid, context.Canceled, ...).
//
// Lock is safe for concurrent use; concurrent calls may share RepodataDir.
func Lock(ctx context.Context, opt LockOptions) (*lock.Lockfile, error) {
	lf, _, err := lockSpecs(ctx, opt)
	return lf, err
}

// Create resolves opt with Lock, then installs the lockfile into prefix
// with InstallLock(ctx, prefix, lf, inst), and returns the lockfile it
// installed. inst.Virtual, when nil, becomes the virtual packages the solve
// used, so the install checks the lockfile against the same machine.
// Before anything is resolved or downloaded, opt.Platform must be the host
// platform (ErrPlatform) and prefix and inst must pass ValidateInstall
// (invalid options, an unsafe prefix or package cache, a prefix that is
// not empty, in the order ValidateInstall documents); then come Lock's
// errors (ErrInvalidOptions for the specs and channels, the repodata
// errors, *solve.Unsolvable), then InstallLock's.
// opt.Progress receives the resolve's events and inst.Progress the
// install's. On any error the installation is rolled back (see
// InstallLock, also for a rollback that cannot finish).
//
// Create is safe for concurrent use with different prefixes.
func Create(ctx context.Context, prefix string, opt LockOptions, inst InstallOptions) (*lock.Lockfile, error) {
	host := virtual.HostPlatform()
	if opt.Platform != "" && opt.Platform != host {
		return nil, fmt.Errorf("goomba: %w: cannot create an environment for %s on %q", ErrPlatform, opt.Platform, host)
	}
	if err := ValidateInstall(prefix, inst); err != nil {
		return nil, err
	}
	lf, virt, err := lockSpecs(ctx, opt)
	if err != nil {
		return nil, err
	}
	if inst.Virtual == nil {
		inst.Virtual = virt
	}
	if err := InstallLock(ctx, prefix, lf, inst); err != nil {
		return nil, err
	}
	return lf, nil
}

// ValidateLock reports the ErrInvalidOptions error Lock would return for
// opt (no specs, a spec or constraint that does not parse, no channels, a
// channel or alias that does not resolve to a usable https URL or carries
// credentials, no RepodataDir, a negative MaxAge, an unknown platform,
// another platform without Virtual, a record in Virtual that is not a
// virtual package), without any I/O. A nil error does not mean Lock will
// succeed. It is safe for concurrent use.
func ValidateLock(opt LockOptions) error {
	_, err := newLockRequest(opt)
	return err
}

// lockRequest is validated LockOptions.
type lockRequest struct {
	opt      LockOptions
	platform string
	alias    string   // channel alias, never ""
	channels []string // canonical channel URLs, in priority order
	python   string   // python-minor prefilter, or ""
}

// newLockRequest validates opt without any I/O.
func newLockRequest(opt LockOptions) (*lockRequest, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("goomba: %w: "+format, append([]any{ErrInvalidOptions}, args...)...)
	}
	r := &lockRequest{opt: opt, platform: opt.Platform, alias: opt.ChannelAlias}
	if len(opt.Specs) == 0 {
		return nil, bad("no specs to resolve")
	}
	for _, list := range []struct {
		what  string
		specs []string
	}{{"spec", opt.Specs}, {"constraint", opt.Constraints}} {
		for _, s := range list.specs {
			if _, err := spec.ParseMatchSpec(s); err != nil {
				return nil, bad("%s %q: %v", list.what, s, err)
			}
		}
	}
	if len(opt.Channels) == 0 {
		return nil, bad("no channels")
	}
	if opt.RepodataDir == "" {
		return nil, bad("RepodataDir is required")
	}
	if opt.MaxAge < 0 {
		return nil, bad("negative MaxAge %v", opt.MaxAge)
	}
	if r.alias == "" {
		r.alias = spec.DefaultChannelAlias
	}
	if shown := redactChannelArg(r.alias); shown != r.alias || strings.ContainsRune(r.alias, '@') {
		return nil, bad("channel alias %s: credentials in channel URLs are not supported", quoteArg(shown))
	}
	host := virtual.HostPlatform()
	if r.platform == "" {
		if host == "" {
			return nil, bad("this machine has no conda platform; set Platform")
		}
		r.platform = host
	}
	if !virtual.KnownPlatform(r.platform) {
		return nil, bad("platform %q is not a conda platform", r.platform)
	}
	if opt.Virtual == nil && r.platform != host {
		return nil, bad("Virtual is required to lock for %s on %q", r.platform, host)
	}
	for i, v := range opt.Virtual {
		if v == nil || !v.IsVirtual() {
			return nil, bad("Virtual[%d] is not a virtual package record", i)
		}
	}
	chans, err := parseChannels(opt.Channels, r.alias)
	if err != nil {
		return nil, fmt.Errorf("goomba: %w: %w", ErrInvalidOptions, err)
	}
	r.channels = chans
	r.python = solve.RequestedPythonMinor(opt.Specs)
	return r, nil
}

// parseChannels resolves channel arguments to canonical https URLs, in
// order. A channel with credentials (an '@', a token segment), with
// platform filters, with %-escapes or that is not https (a lockfile holds
// only https URLs without them), and one given twice, are errors, which
// never show credentials.
func parseChannels(args []string, alias string) ([]string, error) {
	out := make([]string, 0, len(args))
	seen := map[string]string{}
	for _, a := range args {
		if shown := redactChannelArg(a); shown != a || strings.ContainsRune(a, '@') {
			return nil, fmt.Errorf("channel %s: %w: credentials in channel URLs are not supported", quoteArg(shown), spec.ErrChannel)
		}
		ch, err := spec.ParseChannelAlias(a, alias)
		if err != nil {
			return nil, channelParseError(a, err)
		}
		if len(ch.Platforms) > 0 {
			return nil, fmt.Errorf("channel %s: platform filters are not supported (the platform is set separately)", quoteArg(a))
		}
		u, err := url.Parse(ch.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("channel %s: %s is not an https URL", quoteArg(a), quoteArg(ch.URL))
		}
		if strings.ContainsRune(ch.URL, '%') {
			return nil, fmt.Errorf("channel %s: %w: %%-escapes are not supported (lockfiles do not hold them)", quoteArg(redactChannelArg(ch.URL)), spec.ErrChannel)
		}
		if u.User != nil || netguard.HasTokenSegment(u.Path) {
			return nil, fmt.Errorf("channel resolves to %s: %w: credentials in channel URLs are not supported", quoteArg(redactChannelArg(ch.URL)), spec.ErrChannel)
		}
		key := channelKey(ch.URL)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("channel %s: the same channel as %s", quoteArg(a), quoteArg(prev))
		}
		seen[key] = a
		out = append(out, ch.URL)
	}
	return out, nil
}

// channelKey normalises a channel URL for comparison: lower-case scheme and
// host, no default port, no trailing dot or slash.
func channelKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && !(p == "443" && strings.EqualFold(u.Scheme, "https")) {
		host += ":" + p
	}
	return strings.ToLower(u.Scheme) + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
}

// lockSpecs is Lock, also returning the virtual packages the solve used.
func lockSpecs(ctx context.Context, opt LockOptions) (*lock.Lockfile, []*record.PackageRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r, err := newLockRequest(opt)
	if err != nil {
		return nil, nil, err
	}
	virt := opt.Virtual
	if virt == nil {
		if virt, err = virtual.Detect(ctx, virtual.Options{Platform: r.platform}); err != nil {
			return nil, nil, fmt.Errorf("goomba: detect virtual packages: %w", err)
		}
		if virt == nil {
			virt = []*record.PackageRecord{}
		}
	}
	emit := func(e Event) {
		if opt.Progress != nil {
			opt.Progress(e)
		}
	}
	attempts := 1
	if r.python != "" {
		attempts = maxLockAttempts
	}
	req := solve.Request{Specs: opt.Specs, Constraints: opt.Constraints, Virtual: virt, ChannelAlias: opt.ChannelAlias}
	var sol *solve.Solution
	prefilter := r.python
	for attempt := 1; ; attempt++ {
		src, err := r.openChannels(ctx, prefilter, emit)
		if err != nil {
			return nil, nil, err
		}
		emit(Event{Kind: EventSolving, Prefilter: prefilter, Done: int64(attempt), Total: int64(attempts)})
		sol, err = solve.Solve(ctx, src, req)
		var u *solve.Unsolvable
		if errors.As(err, &u) && prefilter != "" && ctx.Err() == nil {
			prefilter = "" // libmamba's retry without the prefilter
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("goomba: %w", err)
		}
		break
	}
	emit(Event{Kind: EventSolved, Prefilter: prefilter, Done: int64(len(sol.Records)), Total: int64(len(sol.Records))})
	lf, err := lock.FromRecords(r.platform, lockChannels(r.channels, sol.Records), opt.Specs, virtual.ToOverrides(virt), sol.Records)
	if err != nil {
		return nil, nil, fmt.Errorf("goomba: %w", err)
	}
	return lf, virt, nil
}

// openChannels opens every channel concurrently with the python-minor
// prefilter py ("" for none) and combines them in priority order. It
// reports each channel, in order, once all are open; the first failure in
// channel order is the error.
func (r *lockRequest) openChannels(ctx context.Context, py string, emit func(Event)) (repodata.Source, error) {
	ro := repodata.Options{
		Client:      r.opt.Client,
		CacheDir:    r.opt.RepodataDir,
		Offline:     r.opt.Offline,
		MaxAge:      r.opt.MaxAge,
		Platform:    r.platform,
		PythonMinor: py,
	}
	n := len(r.channels)
	sources := make([]repodata.Source, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, ch := range r.channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sources[i], errs[i] = repodata.NewChannel(ctx, ch, ro)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("goomba: channel %s: %w", r.channels[i], err)
		}
	}
	for i, ch := range r.channels {
		emit(Event{Kind: EventRepodata, Channel: ch, Prefilter: py, Done: int64(i + 1), Total: int64(n)})
	}
	return repodata.NewMulti(sources...), nil
}

// lockChannels returns the lockfile's channel list: the requested channels
// in priority order, then the channel URL of every package whose URL lies
// under none of them (an index base_url pointing elsewhere), in the order
// of recs, which Solve sorts by name.
func lockChannels(channels []string, recs []*record.PackageRecord) []string {
	out := append([]string(nil), channels...)
	known := make(map[string]bool, len(channels))
	for _, c := range channels {
		known[channelKey(c)] = true
	}
	for _, r := range recs {
		suffix := "/" + r.Subdir + "/" + r.Fn
		if r.Subdir == "" || r.Fn == "" || !strings.HasSuffix(r.URL, suffix) {
			continue // lock.FromRecords reports the URL
		}
		ch := strings.TrimSuffix(r.URL, suffix)
		if k := channelKey(ch); !known[k] {
			known[k] = true
			out = append(out, ch)
		}
	}
	return out
}

// redactChannelArg hides what may be credentials in a channel argument,
// whatever its spelling: everything before the last '@' (keeping only a
// leading "scheme://" of scheme characters), as user information, and the
// segment after a "t" segment, as an anaconda.org token (the "t" compared
// after decoding %-escapes and trimming white space, so "%74" counts).
// Arguments without either come back unchanged.
func redactChannelArg(a string) string {
	segs := strings.Split(a, "/")
	var mask []int
	for i := 0; i+1 < len(segs); i++ {
		if isTokenMarker(segs[i]) && segs[i+1] != "" {
			mask = append(mask, i+1)
		}
	}
	for _, i := range mask {
		segs[i] = "***"
	}
	a = strings.Join(segs, "/")
	at := strings.LastIndexByte(a, '@')
	if at < 0 {
		return a
	}
	keep := ""
	if i := strings.Index(a, "://"); i >= 0 && i < at && isSchemeName(a[:i]) {
		keep = a[:i+3]
	}
	return keep + "***" + a[at:]
}

// isTokenMarker reports whether a path segment is "t", possibly %-encoded
// or surrounded by white space.
func isTokenMarker(seg string) bool {
	if dec, err := url.PathUnescape(seg); err == nil {
		seg = dec
	}
	return trimSpace(seg) == "t"
}

// isSchemeName reports whether s is a URL scheme: a letter followed by
// letters, digits, '+', '-' or '.'.
func isSchemeName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// channelParseError reports a channel argument that spec refuses, showing
// the argument redacted and only spec's innermost reason: spec's own
// message would repeat the argument as given.
func channelParseError(a string, err error) error {
	reason := "cannot be parsed"
	var pe *spec.ParseError
	if errors.As(err, &pe) {
		for {
			var inner *spec.ParseError
			if pe.Err == nil || !errors.As(pe.Err, &inner) {
				break
			}
			pe = inner
		}
		if pe.Msg != "" {
			reason = pe.Msg
		}
	}
	return fmt.Errorf("channel %s: %w: %s", quoteArg(redactChannelArg(a)), spec.ErrChannel, reason)
}

// maxQuoted bounds an argument quoted in an error message.
const maxQuoted = 256

// quoteArg quotes an argument for an error message, Go-escaped (so no
// control character reaches a terminal) and truncated.
func quoteArg(s string) string {
	if len(s) > maxQuoted {
		return strconv.Quote(s[:maxQuoted]) + "..."
	}
	return strconv.Quote(s)
}

// trimSpace trims ASCII white space and control characters, as spec
// strips channel arguments.
func trimSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f })
}
