// The transaction is modelled on libmamba src/core/transaction.cpp
// (MTransaction::execute, fetch_extract_packages) and rattler
// crates/rattler/src/install/driver.rs and installer/ (BSD-3-Clause, see
// NOTICE).

package goomba

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richinsley/goomba/fetch"
	"github.com/richinsley/goomba/internal/safedir"
	"github.com/richinsley/goomba/link"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/virtual"
)

// maxConcurrency caps InstallOptions.Concurrency.
const maxConcurrency = 64

// InstallOptions configures InstallLock. InstallLock only reads it, so one
// value may be shared by concurrent calls as long as nobody modifies it.
type InstallOptions struct {
	// PkgsDir is the package cache directory (fetch.Cache.Dir); required,
	// and not inside the prefix. It may be shared by any number of goomba
	// processes.
	PkgsDir string
	// Client performs downloads. nil selects fetch's default client, which
	// uses no proxy: proxies come from the caller's Client, never from the
	// environment.
	Client *http.Client
	// Concurrency bounds the downloads and the links in flight. 0 selects
	// runtime.NumCPU(); values above 64 are capped at 64. It is also the
	// number of byte-compilation workers.
	Concurrency int
	// CompilePyc byte-compiles the sources of noarch: python packages with
	// the environment's interpreter after linking, as micromamba does by
	// default. The zero value skips it; the .pyc files are recorded in
	// conda-meta either way, as in micromamba.
	CompilePyc bool
	// AllowScripts lists package names whose own link scripts may be
	// installed (never run); passed to link.Options.AllowScripts.
	AllowScripts []string
	// Virtual are the host's virtual packages, checked against the
	// lockfile's virtual dependencies. nil selects virtual.Detect for the
	// lockfile's platform.
	Virtual []*record.PackageRecord
	// Progress, if set, receives events as the transaction advances. It may
	// be called from any goroutine, concurrently; it must not block for
	// long. A panic in it cancels the transaction, which is rolled back
	// (unless EventDone was being reported), and is then raised again on
	// the goroutine that called InstallLock.
	Progress func(Event)
	// Cmd is the command line recorded in conda-meta/history ("# cmd:").
	// Empty records "goomba create -p <prefix>". It must be one line.
	Cmd string
}

// EventKind tells what an Event reports.
type EventKind int

// Event kinds, in the order an installation emits them.
const (
	// EventFetched: a package archive is in the cache, verified and
	// extracted. Done counts the packages fetched so far, Total all of
	// them.
	EventFetched EventKind = iota + 1
	// EventLinked: a package was linked into the prefix. Done counts the
	// packages linked so far, Total all of them.
	EventLinked
	// EventCompiling: byte-compilation of Total source files starts; Done
	// is 0.
	EventCompiling
	// EventCompiled: byte-compilation finished; Done is the number of .pyc
	// files present afterwards, Total the number of sources. Done < Total
	// means some sources did not compile (micromamba ignores that too).
	EventCompiled
	// EventDone: the environment is complete. Done and Total are the number
	// of packages.
	EventDone

	// The kinds below report Lock (and the resolve step of Create), which
	// runs before an installation: EventRepodata for each channel, then
	// EventSolving, again both for a retry, then EventSolved.

	// EventRepodata: the index of a channel is open (its shard indexes, or
	// its full repodata.json, for the platform and noarch). Channel is its
	// URL and Prefilter the python-minor prefilter it was loaded with;
	// Done counts the channels open so far in this attempt, Total all of
	// them.
	EventRepodata
	// EventSolving: a solve starts. Prefilter is the python minor series
	// ("3.11") whose incompatible records were dropped while loading, or
	// "" for none; Done is the attempt, 1, or 2 for the retry without the
	// prefilter after the first was unsolvable; Total is the number of
	// attempts the request may take (2 with a prefilter, else 1).
	EventSolving
	// EventSolved: the solve succeeded. Prefilter tells which attempt it
	// was (as for EventSolving); Done and Total are the number of packages
	// in the solution.
	EventSolved
)

// String returns the kind's name, e.g. "fetched".
func (k EventKind) String() string {
	switch k {
	case EventFetched:
		return "fetched"
	case EventLinked:
		return "linked"
	case EventCompiling:
		return "compiling"
	case EventCompiled:
		return "compiled"
	case EventDone:
		return "done"
	case EventRepodata:
		return "repodata"
	case EventSolving:
		return "solving"
	case EventSolved:
		return "solved"
	}
	return fmt.Sprintf("EventKind(%d)", int(k))
}

// Event reports progress of InstallLock, Lock or Create. It is a plain
// value.
type Event struct {
	// Kind is what happened.
	Kind EventKind
	// Package is the package concerned, "name-version-build", or "" for
	// events about the whole transaction.
	Package string
	// Channel is the channel URL of an EventRepodata, otherwise "".
	Channel string
	// Prefilter is the python-minor prefilter of an EventRepodata,
	// EventSolving or EventSolved ("3.11", or "" for none), otherwise "".
	Prefilter string
	// Done and Total measure progress; their unit depends on Kind.
	Done, Total int64
}

// InstallLock creates prefix (which must not exist, or be an empty
// directory) from lf. On any error or cancellation the prefix is removed
// again if this call created it; the package cache is kept.
//
// The steps, in order:
//
//  1. lf must pass lock.Lockfile.Validate and be for the host platform
//     (virtual.HostPlatform).
//  2. Every dependency of a locked package on a virtual package (__glibc,
//     __osx, __cuda, ...) must be satisfied by opt.Virtual, and every
//     constraint on a virtual package that opt.Virtual holds must match
//     it; otherwise a *VirtualError names the package and the spec, before
//     any download.
//  3. The link order is planned (see below); a depends or constrains entry
//     that does not parse as a match spec is an error.
//  4. Every package is fetched, verified against its sha256 and extracted
//     into opt.PkgsDir, opt.Concurrency at a time; the first failure
//     cancels the rest.
//  5. Before anything is written, the paths every package will create are
//     compared: two packages installing the same path, or one installing
//     below the other's file or symlink, is a *ClobberError naming both
//     (micromamba warns and lets the later one win).
//  6. The prefix, resolved once at the start, one component at a time
//     (package internal/safedir: symlinks on the way to it are followed
//     then and never again, and only from directories nobody else can
//     change; in a sticky shared directory such as /tmp, only the user's or
//     root's symlinks; the resolved path is what is installed into and
//     written into files), is created (missing parents 0755), locked for
//     the rest of the transaction (flock on Unix), checked to belong to the
//     user and not be writable by others, as are the directories on the way
//     to it unless sticky (ErrUnsafePrefix), and
//     conda-meta/.goomba-incomplete is written into it. The prefix
//     directory stays 0700 until the transaction ends, so nobody else can
//     reach what it holds while it is filled; then it becomes 0755 (an
//     existing directory gets its own mode back, also after a rollback).
//  7. Packages are linked with link.Link layer by layer, the packages of a
//     layer concurrently. A link.ErrExists that the check of step 5 did not
//     predict (a path reached through another package's directory symlink,
//     or a case-insensitive file system) also becomes a *ClobberError when
//     the other package can be found. Then every symlink in the prefix must
//     resolve inside it, and the digests of symlinks are recorded from the
//     finished tree.
//  8. With opt.CompilePyc, the sources of noarch: python packages are
//     byte-compiled by the environment's interpreter (see below).
//  9. Each package's conda-meta record is written, its timestamp, license,
//     track_features and features taken from its own info/index.json
//     (lockfiles do not carry them), its requested_spec from the last of
//     lf.Specs naming it; then one conda-meta/history revision with
//     lf.Specs as "# update specs". The prefix directory gets its final
//     mode, and removing .goomba-incomplete, the last change, completes
//     the transaction.
//
// Link order: dependencies link before their dependents. Dependency cycles
// are broken without regard to the order of the input: a platform
// package's dependency on a noarch package of its cycle is dropped first,
// so noarch packages link after the platform packages of their cycle
// (python before pip, as in rattler); cycles left after that (only
// platform or only noarch packages, which real channels contain) are
// broken where a depth-first search in name order closes them.
//
// Byte-compilation is the only process goomba runs: the interpreter of the
// environment's python package, checked just before to be the file linked
// from the verified package, as "<prefix>/bin/pythonX.Y -I -S -Wi -u
// compile_pyc.py" with libmamba's script, so that neither .pth files nor
// sitecustomize of a locked package run; its working directory is a
// private directory in conda-meta, its environment holds only
// MAMBA_EXTRACT_THREADS, and the sources' absolute paths come on stdin.
// It is killed on cancellation.
//
// On failure the prefix is emptied through the locked directory handle
// (openat/unlinkat on Unix), so renaming the prefix or a directory above
// it cannot redirect the removal to other data, never entering a mount
// point inside it (package internal/rmtree), and in an order that keeps
// .goomba-incomplete until nothing else is left: a process killed at any
// moment, while installing or rolling back, leaves a prefix CheckComplete
// reports as ErrIncomplete (RemoveIncomplete removes it), or nothing.
//
// Errors come in this order, so callers that branch on them see the first
// condition that applies: a nil or invalid lockfile (lock.ErrInvalid); a
// lockfile for another platform (ErrPlatform); unusable options
// (ErrInvalidOptions: no PkgsDir, a negative Concurrency, a line break in
// Cmd); the prefix's resolution (ErrUnsafePrefix for an unsafe directory on
// the way to it, the existing prefix directory included when it belongs to
// another user than root and the current user, or I/O errors such as
// ENOTDIR or, for a dangling symlink, EEXIST); a PkgsDir inside the prefix
// (ErrInvalidOptions); unmet virtual packages (*VirtualError) or a
// malformed virtual record (ErrInvalidOptions); an unparsable dependency;
// a prefix that is not an empty directory (ErrPrefixNotEmpty); the
// downloads (fetch's errors, fetch.ErrUnsafeCache for an unsafe package
// cache); clobbering paths (*ClobberError); then, while taking the prefix:
// ErrUnsafePrefix (an existing empty prefix that is not the user's own
// private directory, or a path that changed), ErrPrefixLocked,
// ErrPrefixNotEmpty (filled meanwhile). An existing, non-empty prefix of
// another user is therefore ErrUnsafePrefix, not ErrPrefixNotEmpty.
// ValidateInstall checks the conditions that need no lockfile, in the same
// order, without doing any work.
//
// InstallLock is safe for concurrent use with different prefixes; calls
// for the same prefix exclude each other through the prefix lock (the
// second fails with ErrPrefixLocked or ErrPrefixNotEmpty). A CheckComplete
// that holds the lock shared when InstallLock takes it delays it for
// moments (InstallLock tries again for about 50 ms) but does not make it
// fail.
func InstallLock(ctx context.Context, prefix string, lf *lock.Lockfile, opt InstallOptions) (err error) {
	if lf == nil {
		return fmt.Errorf("goomba: %w: nil lockfile", lock.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := lf.Validate(); err != nil {
		return fmt.Errorf("goomba: %w", err)
	}
	if host := virtual.HostPlatform(); lf.Platform != host {
		return fmt.Errorf("goomba: %w: lockfile is for %s, host is %q", ErrPlatform, lf.Platform, host)
	}
	prefix, cmd, err := checkInstall(prefix, opt)
	if err != nil {
		return err
	}
	conc := opt.Concurrency
	if conc == 0 {
		conc = runtime.NumCPU()
	}
	conc = min(conc, maxConcurrency)

	// A panic in Progress, on whatever goroutine it is called, cancels the
	// transaction; once it is rolled back the panic is raised again here.
	// The re-raising defer is registered first, so it runs last.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var panicked atomic.Pointer[progressPanic]
	defer func() {
		if p := panicked.Load(); p != nil {
			panic(p.value)
		}
	}()
	emit := func(e Event) {
		if opt.Progress == nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				panicked.CompareAndSwap(nil, &progressPanic{value: r})
				cancel()
			}
		}()
		opt.Progress(e)
	}

	recs := lf.Records()
	slices.SortFunc(recs, func(a, b *record.PackageRecord) int { return cmp.Compare(a.Name, b.Name) })
	sc, err := scanSpecs(recs)
	if err != nil {
		return err
	}

	// 2. Virtual packages.
	virt := opt.Virtual
	if virt == nil {
		if virt, err = virtual.Detect(ctx, virtual.Options{Platform: lf.Platform}); err != nil {
			return fmt.Errorf("goomba: detect virtual packages: %w", err)
		}
	}
	hv, err := newHostVirtual(virt)
	if err != nil {
		return err
	}
	if err := hv.check(recs, sc); err != nil {
		return err
	}

	// 3. Plan.
	plan, err := planInstall(recs, sc)
	if err != nil {
		return err
	}
	sc = nil
	// The interpreter's layout, needed only by noarch: python packages.
	var py *link.PythonInfo
	var pyErr error
	for _, r := range recs {
		if r.Name == "python" {
			if py, pyErr = link.NewPythonInfo(r, lf.Platform); pyErr != nil {
				pyErr = fmt.Errorf("goomba: %w", pyErr)
			}
		}
	}
	if err := checkPrefixFree(prefix); err != nil {
		return err
	}
	dists := make([]string, len(recs))
	for i, r := range recs {
		dists[i] = r.Dist()
	}
	total := int64(len(recs))

	user := newUserSpecs(lf.Specs)

	// 4. Fetch and extract.
	cache := &fetch.Cache{Dir: opt.PkgsDir, Client: opt.Client}
	dirs := make([]string, len(recs))
	var fetched atomic.Int64
	err = runBounded(ctx, len(recs), conc, func(ctx context.Context, i int) error {
		d, err := cache.Extracted(ctx, recs[i])
		if err != nil {
			return fmt.Errorf("goomba: fetch %s: %w", dists[i], err)
		}
		dirs[i] = d
		emit(Event{Kind: EventFetched, Package: dists[i], Done: fetched.Add(1), Total: total})
		return nil
	})
	if err != nil {
		return err
	}

	// 5. Clobber check, on the paths link will create (link.PlannedPaths
	// plans and validates them as link.Link does).
	linkOptions := func(i int) link.Options {
		return link.Options{
			Prefix:        prefix,
			Platform:      lf.Platform,
			Python:        py,
			AllowScripts:  opt.AllowScripts,
			RequestedSpec: user.requested(recs[i].Name),
		}
	}
	dests := make([][]dest, len(recs))
	cl := claims{}
	for i, r := range recs {
		planned, err := link.PlannedPaths(ctx, dirs[i], r, linkOptions(i))
		if err != nil {
			if errors.Is(err, link.ErrNoPython) {
				if pyErr != nil {
					return pyErr
				}
				if py == nil {
					return fmt.Errorf("goomba: %w: python is not in the lockfile", err)
				}
			}
			return fmt.Errorf("goomba: %w", err)
		}
		dests[i] = plannedDests(planned)
		if err := cl.add(i, dests[i], dists); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 6. Prefix.
	tx, err := openPrefix(prefix)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed { // an error, or a panic unwinding
			tx.rollback()
		}
	}()

	// 7. Link.
	insts := make([]*link.Installed, len(recs))
	var order []int
	var linked atomic.Int64
	for _, layer := range plan.layers {
		err = runBounded(ctx, len(layer), conc, func(ctx context.Context, k int) error {
			i := layer[k]
			inst, err := link.Link(ctx, dirs[i], recs[i], linkOptions(i))
			if err != nil {
				if errors.Is(err, link.ErrExists) {
					if ce := cl.attributeExistsError(prefix, i, err, dests[i], dists); ce != nil {
						return fmt.Errorf("goomba: link %s: %w", dists[i], ce)
					}
				}
				return fmt.Errorf("goomba: %w", err)
			}
			insts[i] = inst
			emit(Event{Kind: EventLinked, Package: dists[i], Done: linked.Add(1), Total: total})
			return nil
		})
		if err != nil {
			return err
		}
		order = append(order, layer...)
	}
	if err = verifySymlinks(ctx, prefix); err != nil {
		return err
	}
	// Symlink digests once every package is in place, so they do not
	// depend on which package of a layer was linked first.
	if err = rehashSymlinks(ctx, prefix, insts); err != nil {
		return err
	}

	// 8. Byte-compile.
	if opt.CompilePyc && py != nil {
		var sources []string
		for _, i := range order {
			sources = append(sources, insts[i].PythonSources...)
		}
		if len(sources) > 0 {
			emit(Event{Kind: EventCompiling, Total: int64(len(sources))})
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = verifyInterpreter(prefix, py, dirs, insts); err != nil {
				return err
			}
			res, err := compilePyc(ctx, prefix, py, sources, conc)
			if err != nil {
				return err
			}
			emit(Event{Kind: EventCompiled, Done: int64(res.compiled), Total: int64(res.total)})
		}
	}

	// 9. conda-meta and history.
	added := make([]*record.PackageRecord, 0, len(order))
	for _, i := range order {
		if err = ctx.Err(); err != nil {
			return err
		}
		r := recs[i]
		if err = fillFromIndex(r, insts[i].Index); err != nil {
			return fmt.Errorf("goomba: %s: %w", dists[i], err)
		}
		if r.Size == 0 {
			// The archive sits beside the extracted tree, in the resolved cache.
			if fi, serr := os.Lstat(filepath.Join(filepath.Dir(dirs[i]), r.Fn)); serr == nil && fi.Mode().IsRegular() {
				r.Size = fi.Size()
			}
		}
		if err = link.WriteCondaMeta(prefix, r, insts[i], link.Options{Prefix: prefix, Platform: lf.Platform, RequestedSpec: user.requested(r.Name)}); err != nil {
			return fmt.Errorf("goomba: %w", err)
		}
		if err = dropGroupWrite(filepath.Join(prefix, "conda-meta", dists[i]+".json")); err != nil {
			return err
		}
		added = append(added, r)
	}
	if err = link.AppendHistoryEntry(prefix, link.HistoryEntry{Time: time.Now(), Cmd: cmd, Added: added, Update: user.text}); err != nil {
		return fmt.Errorf("goomba: %w", err)
	}
	if err = dropGroupWrite(filepath.Join(prefix, "conda-meta", "history")); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.commit(); err != nil {
		return err
	}
	committed = true
	emit(Event{Kind: EventDone, Done: total, Total: total})
	return nil
}

// checkInstall validates opt for an installation into prefix, which it
// resolves once (safedir.ResolvePath: symlinks are followed only from
// directories nobody else can change, and every directory on the way must
// be safe), and returns the resolved prefix and the history command. It
// changes nothing.
func checkInstall(prefix string, opt InstallOptions) (resolved, cmd string, err error) {
	if opt.PkgsDir == "" {
		return "", "", fmt.Errorf("goomba: %w: PkgsDir is required", ErrInvalidOptions)
	}
	if opt.Concurrency < 0 {
		return "", "", fmt.Errorf("goomba: %w: negative Concurrency %d", ErrInvalidOptions, opt.Concurrency)
	}
	if strings.ContainsAny(opt.Cmd, "\r\n") {
		return "", "", fmt.Errorf("goomba: %w: Cmd contains a line break", ErrInvalidOptions)
	}
	if prefix == "" {
		return "", "", fmt.Errorf("goomba: %w: empty prefix", ErrInvalidOptions)
	}
	if resolved, err = safedir.ResolvePath(prefix); err != nil {
		return "", "", prefixError(prefix, err)
	}
	if inside, err := within(opt.PkgsDir, resolved); err != nil {
		return "", "", fmt.Errorf("goomba: %w", err)
	} else if inside {
		return "", "", fmt.Errorf("goomba: %w: package cache %s is inside the prefix %s", ErrInvalidOptions, opt.PkgsDir, resolved)
	}
	cmd = opt.Cmd
	if cmd == "" {
		cmd = "goomba create -p " + resolved
	}
	if strings.ContainsAny(cmd, "\r\n") {
		return "", "", fmt.Errorf("goomba: prefix %q contains a line break", resolved)
	}
	return resolved, cmd, nil
}

// prefixError wraps an error about the prefix p: a safedir refusal as
// ErrUnsafePrefix.
func prefixError(p string, err error) error {
	if errors.Is(err, safedir.ErrUnsafe) {
		return fmt.Errorf("goomba: %w: %w", ErrUnsafePrefix, err)
	}
	return fmt.Errorf("goomba: prefix %s: %w", p, err)
}

// ValidateInstall reports the errors InstallLock (and Create) would return
// for prefix and opt before doing any work, in this order:
//
//  1. ErrInvalidOptions for unusable options: no PkgsDir, a negative
//     Concurrency, a line break in Cmd;
//  2. the prefix's resolution (see InstallLock): ErrUnsafePrefix when a
//     directory on the way to it, or the existing prefix directory itself,
//     belongs to another user than root and the current user or may be
//     written by group or others without the sticky bit, or when a symlink
//     on the way is in a directory others may write; other errors (ENOTDIR,
//     ENOENT, EEXIST for a dangling symlink) for a path that cannot be a
//     prefix;
//  3. ErrInvalidOptions for a PkgsDir inside the prefix;
//  4. fetch.ErrUnsafeCache for a package cache (PkgsDir) that another user
//     could change, or that a directory on the way to it would let them
//     redirect (a missing PkgsDir is fine);
//  5. ErrInvalidOptions for a malformed opt.Virtual record;
//  6. ErrPrefixNotEmpty for a prefix that is not an empty directory;
//  7. ErrUnsafePrefix for an existing empty prefix that is not the user's
//     own, private directory (one made by root, or mode 1777).
//
// It reads the file system and changes nothing. Callers may use it to fail
// fast before slow preparation, such as detecting virtual packages; success
// does not guarantee that a later InstallLock succeeds, since the file
// system may change meanwhile. It is safe for concurrent use.
func ValidateInstall(prefix string, opt InstallOptions) error {
	resolved, _, err := checkInstall(prefix, opt)
	if err != nil {
		return err
	}
	if err := checkPkgsDir(opt.PkgsDir); err != nil {
		return err
	}
	if opt.Virtual != nil {
		if _, err := newHostVirtual(opt.Virtual); err != nil {
			return err
		}
	}
	if err := checkPrefixFree(resolved); err != nil {
		return err
	}
	// An existing (empty) prefix must be the user's own directory, as
	// openPrefix requires.
	if fi, err := os.Lstat(resolved); err == nil {
		if err := safedir.CheckOwnDir(resolved, fi); err != nil {
			return prefixError(resolved, err)
		}
	}
	return nil
}

// checkPkgsDir checks the package cache directory as package fetch will,
// without creating it: it and the directories on the way to it must be
// safe (safedir), and an existing one must be the user's own directory.
func checkPkgsDir(dir string) error {
	real, err := safedir.ResolvePath(dir)
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Lstat(real); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("goomba: package cache %s: not a directory", real)
			}
			err = safedir.CheckOwnDir(real, fi)
		}
	}
	if errors.Is(err, safedir.ErrUnsafe) {
		return fmt.Errorf("goomba: package cache: %w: %w", fetch.ErrUnsafeCache, err)
	}
	if err != nil {
		return fmt.Errorf("goomba: package cache %s: %w", dir, err)
	}
	return nil
}

// progressPanic holds a value a Progress callback panicked with.
type progressPanic struct{ value any }

// errAborted is the cancellation cause runBounded gives the tasks still
// running after one failed.
var errAborted = errors.New("aborted after another task failed")

// runBounded runs fn(ctx, i) for every i in [0, n), at most limit at a time.
// The first failure cancels the context of the others and stops launching
// new ones. It returns ctx's error if ctx was cancelled, and otherwise the
// error of the lowest i that failed for a reason of its own (not the
// cancellation), so that the error reported does not depend on timing.
func runBounded(ctx context.Context, n, limit int, fn func(ctx context.Context, i int) error) error {
	inner, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errs := make([]error, n)
	sem := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
launch:
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-inner.Done():
			break launch
		}
		if inner.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(inner, i); err != nil {
				errs[i] = err
				cancel(errAborted)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	var first error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if first == nil {
			first = err
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, errAborted) {
			return err
		}
	}
	return first
}

// dropGroupWrite takes group and other write permission off the regular
// file p, which link created as the umask allowed (0664 under umask 002):
// nothing in an environment goomba makes is writable by others.
func dropGroupWrite(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("goomba: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 == 0 {
		return nil
	}
	if err := os.Chmod(p, fi.Mode().Perm()&^0o022); err != nil {
		return fmt.Errorf("goomba: %w", err)
	}
	return nil
}
