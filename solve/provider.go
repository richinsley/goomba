// Ported from rattler crates/rattler_solve/src/resolvo/mod.rs
// (CondaDependencyProvider) (BSD-3-Clause, see NOTICE).

package solve

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/spec"
)

// solvableInfo is what the provider knows about one candidate.
type solvableInfo struct {
	rec  *record.PackageRecord
	name cdcl.NameID
	// version is rec.Version parsed; versionOK is false when it did not
	// parse (the record is then excluded and version is the zero Version).
	version   spec.Version
	versionOK bool
	// rank orders the candidates of one package by version: equal ranks
	// for versions that compare equal, higher ranks for higher versions.
	rank    uint32
	virtual bool
	// hidden marks a placeholder for a channel a channel-qualified spec
	// names but the source did not use for the package (strict channel
	// priority). rec has only Name and Channel; it is always excluded.
	hidden bool
	hasTF  bool
	deps   cdcl.Dependencies
}

// vsInfo is an interned version set: a parsed spec and its display form.
type vsInfo struct {
	ps      parsedSpec
	display string
}

// specResult caches the outcome of parsing a dependency string.
type specResult struct {
	vs  cdcl.VersionSetID
	err error
}

// highestVersion caches find_highest_version for one version set.
type highestVersion struct {
	version spec.Version
	tf      bool
	ok      bool
}

// provider implements cdcl.DependencyProvider for conda packages. It
// loads candidates lazily from a repodata.Source, one package name at a
// time. It is used by one solve on one goroutine.
type provider struct {
	src   repodata.Source
	alias string
	pool  *cdcl.Pool[string, *record.PackageRecord]

	solvables []solvableInfo
	vsets     []vsInfo
	loaded    []*cdcl.Candidates // by NameID; nil until loaded
	specCache map[string]specResult

	virtualByName map[string]cdcl.SolvableID
	// specChannels lists, by package name, the resolved channels of the
	// channel-qualified version sets interned so far, sorted.
	specChannels map[string][]string
	locked       map[string]*record.PackageRecord
	favored      map[string]*record.PackageRecord
	prefetch     []string

	// highest caches the highest matching version per version set, for
	// candidate sorting (rattler's matchspec_to_highest_version).
	highest map[cdcl.VersionSetID]highestVersion
	// tiebreak caches dependency-based orderings of equivalent candidates
	// (rattler's DependencyTiebreakCache), keyed by the exact input order.
	tiebreak       map[string][]cdcl.SolvableID
	tiebreakStored int
}

// tiebreakCacheLimit bounds the number of solvable ids the tiebreak cache
// retains across keys and values (rattler's
// DEPENDENCY_TIEBREAK_CACHE_CANDIDATE_LIMIT).
const tiebreakCacheLimit = 100_000

// newProvider checks req and returns a provider for it and the problem to
// solve: a requirement on every virtual package (so each counts as
// installed), then one per spec, and the root constraints.
func newProvider(src repodata.Source, req Request) (*provider, cdcl.Problem, error) {
	p := &provider{
		src:           src,
		alias:         channelAlias(req.ChannelAlias),
		pool:          cdcl.NewPool[string, *record.PackageRecord](),
		specCache:     map[string]specResult{},
		virtualByName: map[string]cdcl.SolvableID{},
		specChannels:  map[string][]string{},
		locked:        map[string]*record.PackageRecord{},
		favored:       map[string]*record.PackageRecord{},
		highest:       map[cdcl.VersionSetID]highestVersion{},
		tiebreak:      map[string][]cdcl.SolvableID{},
	}
	var problem cdcl.Problem
	if err := checkAlias(p.alias); err != nil {
		return nil, problem, err
	}

	// Virtual packages come first, as in rattler, and each is required so
	// that constrains on it apply.
	for i, r := range req.Virtual {
		if err := checkVirtual(i, r); err != nil {
			return nil, problem, err
		}
		if _, dup := p.virtualByName[r.Name]; dup {
			return nil, problem, fmt.Errorf("%w: virtual package %q is given twice", ErrInvalidRequest, r.Name)
		}
		v, err := parseRecordVersion(r)
		if err != nil {
			return nil, problem, fmt.Errorf("%w: virtual package %q: %w", ErrInvalidRequest, r.Name, err)
		}
		name := p.pool.InternPackageName(r.Name)
		id := p.addSolvable(solvableInfo{rec: r, name: name, version: v, versionOK: true, virtual: true})
		p.virtualByName[r.Name] = id
		vs := p.internSpec(parsedSpec{ms: spec.MatchSpec{Name: spec.NewGlobSpec(r.Name)}, name: r.Name})
		problem.Requirements = append(problem.Requirements, cdcl.SingleRequirement(vs).Unconditional())
	}
	if err := collectRecords("locked", req.Locked, p.locked); err != nil {
		return nil, problem, err
	}
	if err := collectRecords("favored", req.Favored, p.favored); err != nil {
		return nil, problem, err
	}
	for _, m := range []map[string]*record.PackageRecord{p.locked, p.favored} {
		for name, r := range m {
			m[name] = normalizeDigests(r)
		}
	}

	seen := map[string]bool{}
	for _, s := range req.Specs {
		ps, err := parseSpec(s, p.alias)
		if err != nil {
			return nil, problem, fmt.Errorf("%w: spec %q: %w", ErrInvalidRequest, s, err)
		}
		vs := p.internSpec(ps)
		problem.Requirements = append(problem.Requirements, cdcl.SingleRequirement(vs).Unconditional())
		if !isVirtualName(ps.name) && !seen[ps.name] {
			seen[ps.name] = true
			p.prefetch = append(p.prefetch, ps.name)
		}
	}
	for _, s := range req.Constraints {
		ps, err := parseSpec(s, p.alias)
		if err != nil {
			return nil, problem, fmt.Errorf("%w: constraint %q: %w", ErrInvalidRequest, s, err)
		}
		problem.Constraints = append(problem.Constraints, p.internSpec(ps))
	}
	return p, problem, nil
}

// checkVirtual checks the i-th record of Request.Virtual.
func checkVirtual(i int, r *record.PackageRecord) error {
	switch {
	case r == nil:
		return fmt.Errorf("%w: virtual package %d is nil", ErrInvalidRequest, i)
	case !isVirtualName(r.Name):
		return fmt.Errorf("%w: %q is not a virtual package (names start with \"__\")", ErrInvalidRequest, r.Name)
	case !isPackageName(r.Name):
		return fmt.Errorf("%w: virtual package %q: invalid package name", ErrInvalidRequest, r.Name)
	}
	return nil
}

// collectRecords indexes Request.Locked or Request.Favored by name.
func collectRecords(kind string, recs []*record.PackageRecord, into map[string]*record.PackageRecord) error {
	for i, r := range recs {
		switch {
		case r == nil:
			return fmt.Errorf("%w: %s record %d is nil", ErrInvalidRequest, kind, i)
		case isVirtualName(r.Name):
			return fmt.Errorf("%w: %s record %q is a virtual package", ErrInvalidRequest, kind, r.Name)
		case !isPackageName(r.Name):
			return fmt.Errorf("%w: %s record %q: invalid package name", ErrInvalidRequest, kind, r.Name)
		}
		if _, dup := into[r.Name]; dup {
			return fmt.Errorf("%w: two %s records for %q", ErrInvalidRequest, kind, r.Name)
		}
		into[r.Name] = r
	}
	return nil
}

// isVirtualName reports whether name is reserved for virtual packages. It
// is the one predicate the package uses, and it agrees with
// record.PackageRecord.IsVirtual: "__" followed by at least one byte ("__"
// alone is an ordinary name).
func isVirtualName(name string) bool { return (&record.PackageRecord{Name: name}).IsVirtual() }

// addSolvable interns a solvable and records what the provider knows
// about it.
func (p *provider) addSolvable(info solvableInfo) cdcl.SolvableID {
	id := p.pool.InternSolvable(info.name, info.rec)
	if int(id) != len(p.solvables) {
		panic("solve: solvable ids out of step with the pool")
	}
	p.solvables = append(p.solvables, info)
	return id
}

// internSpec interns a parsed spec as a version set, keyed by specKey so
// that two specs share a version set only if they agree on every field.
func (p *provider) internSpec(ps parsedSpec) cdcl.VersionSetID {
	name := p.pool.InternPackageName(ps.name)
	id := p.pool.InternVersionSet(name, specKey(ps))
	if int(id) == len(p.vsets) {
		p.vsets = append(p.vsets, vsInfo{ps: ps, display: sanitize(displayVersionSet(ps.ms))})
		if ps.channel != "" {
			chs := p.specChannels[ps.name]
			if i := sort.SearchStrings(chs, ps.channel); i == len(chs) || chs[i] != ps.channel {
				p.specChannels[ps.name] = slices.Insert(chs, i, ps.channel)
			}
		}
	}
	return id
}

// specKey encodes every field of a parsed spec (and its resolved channel)
// as a version-set key: each field's text form, length-prefixed. It does
// not rely on MatchSpec.String being lossless.
func specKey(ps parsedSpec) string {
	ms := &ps.ms
	optional := "0"
	if ms.Optional {
		optional = "1"
	}
	var b strings.Builder
	for _, f := range [...]string{
		ms.Name.String(), ms.Version.String(), ms.Build.String(), ms.BuildNumber.String(),
		ms.Channel, ps.channel, ms.Subdir, ms.Namespace, ms.MD5, ms.SHA256, ms.Fn, ms.URL,
		ms.License, ms.LicenseFamily, ms.Features, strings.Join(ms.TrackFeatures, "\x00"), optional,
	} {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
	}
	return b.String()
}

// dependencySpec parses a dependency or constraint of a record, caching
// the result by its text.
func (p *provider) dependencySpec(dep string) (cdcl.VersionSetID, error) {
	if r, ok := p.specCache[dep]; ok {
		return r.vs, r.err
	}
	var r specResult
	ps, err := parseSpec(dep, p.alias)
	if err != nil {
		r.err = err
	} else {
		r.vs = p.internSpec(ps)
	}
	p.specCache[dep] = r
	return r.vs, r.err
}

// displayVersionSet renders a spec without its name, as rattler renders a
// NamelessMatchSpec: the version (conda-build form, "*" for any), the
// build string, then any other constrained fields in brackets.
func displayVersionSet(ms spec.MatchSpec) string {
	var b strings.Builder
	if ms.Version.IsAny() {
		b.WriteString("*")
	} else {
		b.WriteString(ms.Version.CondaBuildString())
	}
	if !ms.Build.IsAny() {
		b.WriteString(" ")
		b.WriteString(ms.Build.String())
	}
	var extra []string
	add := func(k, v string) {
		if v != "" {
			extra = append(extra, k+"="+v)
		}
	}
	add("channel", ms.Channel)
	add("subdir", ms.Subdir)
	if !ms.BuildNumber.IsAny() {
		add("build_number", ms.BuildNumber.String())
	}
	add("fn", ms.Fn)
	add("url", ms.URL)
	add("md5", ms.MD5)
	add("sha256", ms.SHA256)
	add("license", ms.License)
	add("track_features", strings.Join(ms.TrackFeatures, " "))
	if len(extra) > 0 {
		b.WriteString(" [")
		b.WriteString(strings.Join(extra, ", "))
		b.WriteString("]")
	}
	return b.String()
}

// DisplaySolvable implements cdcl.Interner: "name=version=build", as
// rattler displays a PackageRecord. Like every Display method it
// sanitizes what it renders.
func (p *provider) DisplaySolvable(s cdcl.SolvableID) string {
	info := &p.solvables[s]
	name := p.pool.ResolvePackageName(info.name)
	if info.hidden {
		return sanitize(name + " from " + info.rec.Channel)
	}
	if info.rec.Build == "" {
		return sanitize(name + " " + info.rec.Version)
	}
	return sanitize(name + "=" + info.rec.Version + "=" + info.rec.Build)
}

// DisplayMergedSolvables implements cdcl.MergedSolvablesDisplayer: the
// package name and the distinct versions in order, abbreviated around
// "..." when there are more than four, as rattler does.
func (p *provider) DisplayMergedSolvables(ss []cdcl.SolvableID) string {
	if len(ss) == 0 {
		return ""
	}
	if len(ss) == 1 && p.solvables[ss[0]].hidden {
		return p.DisplaySolvable(ss[0])
	}
	infos := make([]*solvableInfo, len(ss))
	for i, s := range ss {
		infos[i] = &p.solvables[s]
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].version.Compare(infos[j].version) < 0 })
	var versions []string
	for i, info := range infos {
		if i > 0 && info.version.Compare(infos[i-1].version) == 0 {
			continue
		}
		versions = append(versions, info.rec.Version)
	}
	const head, tail = 2, 1
	if len(versions) > head+tail+1 {
		v := append([]string{}, versions[:head]...)
		v = append(v, "...")
		versions = append(v, versions[len(versions)-tail:]...)
	}
	return sanitize(strings.TrimRight(p.pool.ResolvePackageName(p.solvables[ss[0]].name)+" "+strings.Join(versions, " | "), " "))
}

// DisplayName implements cdcl.Interner.
func (p *provider) DisplayName(n cdcl.NameID) string { return sanitize(p.pool.ResolvePackageName(n)) }

// DisplayVersionSet implements cdcl.Interner.
func (p *provider) DisplayVersionSet(vs cdcl.VersionSetID) string { return p.vsets[vs].display }

// DisplayString implements cdcl.Interner: exclusion reasons, which quote
// records and dependency strings.
func (p *provider) DisplayString(s cdcl.StringID) string { return sanitize(p.pool.ResolveString(s)) }

// VersionSetName implements cdcl.Interner.
func (p *provider) VersionSetName(vs cdcl.VersionSetID) cdcl.NameID {
	return p.pool.ResolveVersionSetPackageName(vs)
}

// SolvableName implements cdcl.Interner.
func (p *provider) SolvableName(s cdcl.SolvableID) cdcl.NameID { return p.solvables[s].name }

// VersionSetsInUnion implements cdcl.Interner. The provider never creates
// unions.
func (p *provider) VersionSetsInUnion(u cdcl.VersionSetUnionID) []cdcl.VersionSetID {
	return p.pool.ResolveVersionSetUnion(u)
}

// ResolveCondition implements cdcl.Interner. The provider never creates
// conditions.
func (p *provider) ResolveCondition(c cdcl.ConditionID) cdcl.Condition {
	return p.pool.ResolveCondition(c)
}

// FilterCandidates implements cdcl.DependencyProvider. The solver caches
// its answer per version set.
func (p *provider) FilterCandidates(candidates []cdcl.SolvableID, vs cdcl.VersionSetID, inverse bool) []cdcl.SolvableID {
	ps := &p.vsets[vs].ps
	out := make([]cdcl.SolvableID, 0, len(candidates))
	for _, c := range candidates {
		s := &p.solvables[c]
		match := s.versionOK && recordMatches(ps, s.rec, s.version, s.virtual)
		if s.hidden {
			// A placeholder stands for every record of its channel.
			match = ps.channel == s.rec.Channel
		}
		if match != inverse {
			out = append(out, c)
		}
	}
	return out
}

// GetDependencies implements cdcl.DependencyProvider. Dependencies are
// parsed when a package's candidates are loaded.
func (p *provider) GetDependencies(_ context.Context, s cdcl.SolvableID) (cdcl.Dependencies, error) {
	return p.solvables[s].deps, nil
}

// GetCandidates implements cdcl.DependencyProvider: it loads the records of
// one package from the source (a virtual package from Request.Virtual
// instead), applies the exclusion rules and interns the candidates.
func (p *provider) GetCandidates(ctx context.Context, name cdcl.NameID) (*cdcl.Candidates, error) {
	if int(name) < len(p.loaded) && p.loaded[name] != nil {
		return p.loaded[name], nil
	}
	nameStr := p.pool.ResolvePackageName(name)
	cands := &cdcl.Candidates{HintDependencies: cdcl.HintAll}
	if isVirtualName(nameStr) {
		if id, ok := p.virtualByName[nameStr]; ok {
			cands.Candidates = []cdcl.SolvableID{id}
		}
	} else {
		recs, err := p.src.Records(ctx, nameStr)
		if err != nil {
			return nil, err
		}
		if err := p.loadRecords(ctx, name, nameStr, recs, cands); err != nil {
			return nil, err
		}
		p.addHiddenChannels(name, nameStr, cands)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	p.rankVersions(cands.Candidates)
	for int(name) >= len(p.loaded) {
		p.loaded = append(p.loaded, nil)
	}
	p.loaded[name] = cands
	return cands, nil
}

// addHiddenChannels explains channel-qualified specs whose channel the
// source did not use for this package. With strict channel priority
// (repodata.Multi) only the first channel that lists a package answers
// for it, so "b::foo" finds no candidate when channel a lists foo; like
// micromamba, the explanation should say so rather than report that no
// candidates exist. For each channel named by a channel-qualified spec on
// the package (among the specs known when the package is loaded), if the
// package has candidates but none from that channel, an excluded
// placeholder candidate stands for the channel, with the channels the
// candidates do come from as the reason. A channel-qualified spec interned
// after the package was loaded gets no placeholder (see the package
// documentation).
func (p *provider) addHiddenChannels(name cdcl.NameID, nameStr string, cands *cdcl.Candidates) {
	wanted := p.specChannels[nameStr]
	if len(wanted) == 0 || len(cands.Candidates) == 0 {
		return
	}
	var from []string
	for _, id := range cands.Candidates {
		ch := p.solvables[id].rec.Channel
		if i := sort.SearchStrings(from, ch); i == len(from) || from[i] != ch {
			from = slices.Insert(from, i, ch)
		}
	}
	reason := fmt.Sprintf("the channels in use offer %s only from %s (strict channel priority)", nameStr, strings.Join(from, ", "))
	for _, ch := range wanted {
		if i := sort.SearchStrings(from, ch); i < len(from) && from[i] == ch {
			continue
		}
		reasonID := p.pool.InternString(reason)
		id := p.addSolvable(solvableInfo{
			rec:    &record.PackageRecord{Name: nameStr, Channel: ch},
			name:   name,
			hidden: true,
			deps:   cdcl.UnknownDependencies(reasonID),
		})
		cands.Candidates = append(cands.Candidates, id)
		cands.Excluded = append(cands.Excluded, cdcl.ExcludedSolvable{Solvable: id, Reason: reasonID})
	}
}

// distKey identifies a package build within one channel subdir,
// regardless of its archive format.
type distKey struct {
	channel, subdir, name, version, build string
}

// recordEntry is a record about to become a candidate.
type recordEntry struct {
	rec    *record.PackageRecord
	reason string // exclusion reason, "" if installable
}

// loadRecords turns the source's records for one package into candidates.
//
// The records are put in a canonical order first, so the answer never
// depends on the order the source returns them in. Like rattler, a build
// listed as both .conda and .tar.bz2 is kept once (the .conda unless only
// the .tar.bz2 is installable) and a file listed twice is an error. A
// locked record replaces its identical source record and excludes every
// other; a favored record is matched to its identical source record or
// added as a candidate of its own.
//
// Loading a package with very many records takes a while, so the loops
// check ctx every cancelCheckInterval records.
func (p *provider) loadRecords(ctx context.Context, name cdcl.NameID, nameStr string, recs []*record.PackageRecord, cands *cdcl.Candidates) error {
	list := make([]*record.PackageRecord, 0, len(recs))
	for i, r := range recs {
		if err := checkCancelled(ctx, i); err != nil {
			return err
		}
		if r != nil {
			list = append(list, normalizeDigests(r))
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return canonicalLess(list[i], list[j]) })
	if err := ctx.Err(); err != nil {
		return err
	}

	locked, favored := p.locked[nameStr], p.favored[nameStr]
	var entries []recordEntry
	byDist := map[distKey]int{}
	favoredAt := -1
	for i, r := range list {
		if err := checkCancelled(ctx, i); err != nil {
			return err
		}
		if locked != nil && sameRecord(r, locked) {
			continue // the locked record stands in for it
		}
		reason := p.exclusionReasonCtx(ctx, r, nameStr)
		key := distKey{r.Channel, r.Subdir, r.Name, r.Version, r.Build}
		if j, dup := byDist[key]; dup {
			prev := &entries[j]
			if archiveExt(prev.rec.Fn) == archiveExt(r.Fn) {
				return fmt.Errorf("%w: %s is listed twice", ErrDuplicateRecord, sanitize(r.Channel+"/"+r.Subdir+"/"+r.Fn))
			}
			prevOK, curOK := prev.reason == "", reason == ""
			if (curOK && !prevOK) || (curOK == prevOK && archiveExt(r.Fn) == ".conda") {
				*prev = recordEntry{rec: r, reason: reason}
			}
			continue
		}
		byDist[key] = len(entries)
		entries = append(entries, recordEntry{rec: r, reason: reason})
	}
	if favored != nil {
		for i, e := range entries {
			if sameRecord(e.rec, favored) {
				favoredAt = i
				break
			}
		}
	}

	lockedReason := ""
	if locked != nil {
		lockedReason = fmt.Sprintf("%s is locked", displayRecord(locked))
	}
	add := func(r *record.PackageRecord, reason string) cdcl.SolvableID {
		id := p.addRecord(name, r, reason)
		cands.Candidates = append(cands.Candidates, id)
		if reason == "" {
			reason = lockedReason
		}
		if reason != "" {
			cands.Excluded = append(cands.Excluded, cdcl.ExcludedSolvable{Solvable: id, Reason: p.pool.InternString(reason)})
		}
		return id
	}
	for i, e := range entries {
		if err := checkCancelled(ctx, i); err != nil {
			return err
		}
		id := add(e.rec, e.reason)
		if i == favoredAt && e.reason == "" && locked == nil {
			cands.Favored, cands.HasFavored = id, true
		}
	}
	if favored != nil && favoredAt < 0 && locked == nil {
		reason := p.exclusionReasonCtx(ctx, favored, nameStr)
		id := add(favored, reason)
		if reason == "" {
			cands.Favored, cands.HasFavored = id, true
		}
	}
	if locked != nil {
		reason := p.exclusionReasonCtx(ctx, locked, nameStr)
		id := p.addRecord(name, locked, reason)
		cands.Candidates = append(cands.Candidates, id)
		if reason != "" {
			cands.Excluded = append(cands.Excluded, cdcl.ExcludedSolvable{Solvable: id, Reason: p.pool.InternString(reason)})
		}
		cands.Locked, cands.HasLocked = id, true
	}
	return nil
}

// canonicalLess orders the records of one package independently of the
// source: by subdir and file name (the order repodata.FullIndex uses), then
// by every other field, so that only records identical in every field
// (which are then interchangeable) keep the source's order.
func canonicalLess(a, b *record.PackageRecord) bool {
	if c := compareRecords(a, b); c != 0 {
		return c < 0
	}
	return false
}

// compareRecords compares every field of two records, stopping at the
// first that differs.
func compareRecords(a, b *record.PackageRecord) int {
	if c := strings.Compare(a.Subdir, b.Subdir); c != 0 {
		return c
	}
	if c := strings.Compare(a.Fn, b.Fn); c != 0 {
		return c
	}
	for _, f := range [...][2]string{
		{a.Name, b.Name}, {a.Channel, b.Channel}, {a.URL, b.URL}, {a.SHA256, b.SHA256}, {a.MD5, b.MD5},
		{a.Version, b.Version}, {a.Build, b.Build},
	} {
		if c := strings.Compare(f[0], f[1]); c != 0 {
			return c
		}
	}
	if c := cmpUint64(a.BuildNumber, b.BuildNumber); c != 0 {
		return c
	}
	if c := cmpInt64(a.Timestamp, b.Timestamp); c != 0 {
		return c
	}
	if c := cmpInt64(a.Size, b.Size); c != 0 {
		return c
	}
	for _, f := range [...][2]string{
		{a.Noarch, b.Noarch}, {a.License, b.License}, {a.Features, b.Features},
		{a.PythonSitePackagesPath, b.PythonSitePackagesPath},
	} {
		if c := strings.Compare(f[0], f[1]); c != 0 {
			return c
		}
	}
	if c := compareStrings(a.TrackFeatures, b.TrackFeatures); c != 0 {
		return c
	}
	if c := compareStrings(a.Depends, b.Depends); c != 0 {
		return c
	}
	return compareStrings(a.Constrains, b.Constrains)
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareStrings compares two string lists element by element, then by
// length.
func compareStrings(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := strings.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

// normalizeDigests returns r, or a copy of it with its sha256 and md5 in
// lower case if either has upper-case hex digits, so that every record the
// solver hands out carries canonical digests. Records from repodata are
// already lower case; Memory sources and caller records may not be.
func normalizeDigests(r *record.PackageRecord) *record.PackageRecord {
	lowerSHA, lowerMD5 := strings.ToLower(r.SHA256), strings.ToLower(r.MD5)
	if lowerSHA == r.SHA256 && lowerMD5 == r.MD5 {
		return r
	}
	c := *r
	c.SHA256, c.MD5 = lowerSHA, lowerMD5
	return &c
}

// archiveExt returns ".conda" or ".tar.bz2" for a package file name, or ""
// for anything else.
func archiveExt(fn string) string {
	switch {
	case strings.HasSuffix(fn, ".conda"):
		return ".conda"
	case strings.HasSuffix(fn, ".tar.bz2"):
		return ".tar.bz2"
	}
	return ""
}

// displayRecord formats a record like DisplaySolvable.
func displayRecord(r *record.PackageRecord) string {
	if r.Build == "" {
		return r.Name + " " + r.Version
	}
	return r.Name + "=" + r.Version + "=" + r.Build
}

// exclusionReasonCtx returns why a record of package nameStr must not be
// installed, or "" if nothing rules it out: a name that differs from the
// package's, a version that does not parse, a missing or malformed sha256,
// or a dependency or constraint that does not parse (or uses a form goomba
// does not support).
//
// It checks ctx every cancelCheckInterval dependencies and, once ctx is
// done, returns "" early; the caller checks ctx before using the answer.
func (p *provider) exclusionReasonCtx(ctx context.Context, r *record.PackageRecord, nameStr string) string {
	if r.Name != nameStr {
		return fmt.Sprintf("its name %q is not %q", r.Name, nameStr)
	}
	if _, err := parseRecordVersion(r); err != nil {
		return fmt.Sprintf("unparsable version %q: %s", r.Version, parseErrorDetail(r.Version, err))
	}
	if r.SHA256 == "" {
		return "no sha256: cannot be verified"
	}
	if !isSHA256(r.SHA256) {
		return "malformed sha256: cannot be verified"
	}
	for i, d := range r.Depends {
		if checkCancelled(ctx, i) != nil {
			return ""
		}
		if _, err := p.dependencySpec(d); err != nil {
			return exclusionForDependency(d, err)
		}
	}
	for i, c := range r.Constrains {
		if checkCancelled(ctx, i) != nil {
			return ""
		}
		if _, err := p.dependencySpec(c); err != nil {
			return exclusionForDependency(c, err)
		}
	}
	return ""
}

// exclusionReason is exclusionReasonCtx without a deadline.
func (p *provider) exclusionReason(r *record.PackageRecord, nameStr string) string {
	return p.exclusionReasonCtx(context.Background(), r, nameStr)
}

// cancelCheckInterval is how many records or dependencies the loading
// loops handle between two checks of their context.
const cancelCheckInterval = 1024

// checkCancelled returns ctx.Err() on every cancelCheckInterval-th
// iteration i (the first included), nil otherwise.
func checkCancelled(ctx context.Context, i int) error {
	if i%cancelCheckInterval != 0 {
		return nil
	}
	return ctx.Err()
}

// isSHA256 reports whether s is 64 hex digits.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// addRecord interns a record as a solvable of package name. A record with
// an exclusion reason gets unknown dependencies with that reason, like
// rattler's records whose dependencies fail to parse.
func (p *provider) addRecord(name cdcl.NameID, r *record.PackageRecord, reason string) cdcl.SolvableID {
	v, verr := parseRecordVersion(r)
	info := solvableInfo{
		rec:       r,
		name:      name,
		version:   v,
		versionOK: verr == nil,
		hasTF:     len(r.TrackFeatures) > 0,
	}
	if reason != "" {
		info.deps = cdcl.UnknownDependencies(p.pool.InternString(reason))
	} else {
		reqs := make([]cdcl.ConditionalRequirement, 0, len(r.Depends))
		for _, d := range r.Depends {
			vs, _ := p.dependencySpec(d) // parsed without error by exclusionReason
			reqs = append(reqs, cdcl.SingleRequirement(vs).Unconditional())
		}
		var cons []cdcl.VersionSetID
		for _, c := range r.Constrains {
			vs, _ := p.dependencySpec(c)
			cons = append(cons, vs)
		}
		info.deps = cdcl.KnownDependencies(reqs, cons)
	}
	return p.addSolvable(info)
}

// rankVersions ranks the versions of all candidates of one package, as
// rattler's rank_versions does: versions that compare equal share a rank.
func (p *provider) rankVersions(ids []cdcl.SolvableID) {
	order := append([]cdcl.SolvableID(nil), ids...)
	sort.SliceStable(order, func(i, j int) bool {
		return p.solvables[order[i]].version.Compare(p.solvables[order[j]].version) < 0
	})
	for i, id := range order {
		if i == 0 {
			p.solvables[id].rank = 0
			continue
		}
		lower := &p.solvables[order[i-1]]
		p.solvables[id].rank = lower.rank
		if lower.version.Compare(p.solvables[id].version) < 0 {
			p.solvables[id].rank++
		}
	}
}
