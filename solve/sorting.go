// Ported from rattler crates/rattler_solve/src/resolvo/conda_sorting.rs
// (BSD-3-Clause, see NOTICE), default (highest) strategy.

package solve

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/spec"
)

// SortCandidates implements cdcl.DependencyProvider. It orders the
// candidates of one package best first, in two steps:
//
//  1. Records without track_features first, then higher versions, then
//     higher build numbers (a stable sort, so ties keep their order).
//  2. Within each run of candidates equal under step 1, the candidate
//     whose dependencies, shared by every candidate of the run, can reach
//     the highest versions comes first; newer timestamps break the
//     remaining ties.
func (p *provider) SortCandidates(ctx context.Context, cache *cdcl.SolverCache, ss []cdcl.SolvableID) error {
	if len(ss) < 2 {
		return nil
	}
	sort.SliceStable(ss, func(i, j int) bool { return p.simpleCompare(ss[i], ss[j]) < 0 })
	return p.sortByHighestDependencyVersions(ctx, cache, ss)
}

// simpleCompare orders two candidates of the same package by tracked
// features, version and build number (rattler's simple_compare).
func (p *provider) simpleCompare(a, b cdcl.SolvableID) int {
	ra, rb := &p.solvables[a], &p.solvables[b]
	if ra.hasTF != rb.hasTF {
		if ra.hasTF {
			return 1
		}
		return -1
	}
	if ra.rank != rb.rank {
		if ra.rank > rb.rank {
			return -1
		}
		return 1
	}
	return cmpUint64(buildNumber(rb), buildNumber(ra))
}

// buildNumber is the build number used for sorting (0 for virtual
// packages, as in rattler).
func buildNumber(s *solvableInfo) uint64 {
	if s.virtual {
		return 0
	}
	return s.rec.BuildNumber
}

func cmpUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// sortByHighestDependencyVersions sorts every run of candidates that
// simpleCompare finds equal by their dependencies.
func (p *provider) sortByHighestDependencyVersions(ctx context.Context, cache *cdcl.SolverCache, ss []cdcl.SolvableID) error {
	for start := 0; start < len(ss); {
		end := start + 1
		for end < len(ss) && p.simpleCompare(ss[start], ss[end]) == 0 {
			end++
		}
		sub := ss[start:end]
		start = end
		if len(sub) < 2 {
			continue
		}
		key := idsKey(sub)
		if cached, ok := p.tiebreak[key]; ok {
			copy(sub, cached)
			continue
		}
		cacheable := p.tiebreakStored+2*len(sub) <= tiebreakCacheLimit
		if err := p.sortSubsetByHighestDependencyVersions(ctx, cache, sub); err != nil {
			return err
		}
		if cacheable {
			p.tiebreak[key] = append([]cdcl.SolvableID(nil), sub...)
			p.tiebreakStored += 2 * len(sub)
		}
	}
	return nil
}

// idsKey encodes a list of solvable ids as a map key.
func idsKey(ids []cdcl.SolvableID) string {
	b := make([]byte, 4*len(ids))
	for i, id := range ids {
		binary.LittleEndian.PutUint32(b[4*i:], uint32(id))
	}
	return string(b)
}

// trackedVersion couples a version with whether the record carrying it has
// track_features (rattler's TrackedFeatureVersion).
type trackedVersion struct {
	version spec.Version
	tf      bool
}

// compareTracked orders two tracked versions best first: a version without
// tracked features before one with, then the higher version first.
func compareTracked(a, b trackedVersion) int {
	switch {
	case a.tf && !b.tf:
		return 1
	case !a.tf && b.tf:
		return -1
	}
	return b.version.Compare(a.version)
}

// depKey identifies the requirements of one candidate on one package.
type depKey struct {
	solvable cdcl.SolvableID
	name     cdcl.NameID
}

// score is a lazily computed dependency score.
type score struct {
	done bool
	ok   bool
	v    trackedVersion
}

// sortSubsetByHighestDependencyVersions orders candidates that are equal by
// tracked features, version and build number (rattler's
// sort_subset_by_highest_dependency_versions):
//
//  1. Candidates with unknown dependencies move to the end.
//  2. For every package that all remaining candidates depend on, a
//     candidate scores the best version it can end up with: of each of its
//     requirements on that package, the highest matching version, and of
//     those the lowest (only versions matching all of them can be
//     selected).
//  3. Candidates are sorted by their scores, package by package in name
//     order, then by newer timestamp.
func (p *provider) sortSubsetByHighestDependencyVersions(ctx context.Context, cache *cdcl.SolverCache, ss []cdcl.SolvableID) error {
	deps := make(map[cdcl.SolvableID]*cdcl.Dependencies, len(ss))
	for _, id := range ss {
		d, err := cache.Dependencies(ctx, id)
		if err != nil {
			return err
		}
		deps[id] = d
	}

	idAndDeps := map[depKey][]cdcl.VersionSetID{}
	nameCount := map[cdcl.NameID]int{}
	known := len(ss)
	for idx := 0; idx < known; {
		id := ss[idx]
		d := deps[id]
		if d.Unknown {
			// Swap to the end and look at the swapped-in candidate next.
			known--
			ss[idx], ss[known] = ss[known], ss[idx]
			continue
		}
		for _, req := range d.Requirements {
			vs, ok := req.Requirement.VersionSet()
			if !ok {
				return fmt.Errorf("solve: %w: union requirement in candidate sorting", cdcl.ErrInternal)
			}
			name := p.VersionSetName(vs)
			if count, seen := nameCount[name]; seen {
				if count+1 < idx {
					// Some earlier candidate lacks this package.
					continue
				}
			} else if idx != 0 {
				continue
			} else {
				nameCount[name] = 0
			}
			k := depKey{solvable: id, name: name}
			if list, ok := idAndDeps[k]; ok {
				idAndDeps[k] = append(list, vs)
			} else {
				idAndDeps[k] = []cdcl.VersionSetID{vs}
				nameCount[name]++
			}
		}
		idx++
	}
	ss = ss[:known]

	var names []cdcl.NameID
	for name, count := range nameCount {
		if count == known {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return p.DisplayName(names[i]) < p.DisplayName(names[j]) })

	var firstErr error
	best := func(id cdcl.SolvableID, name cdcl.NameID) (trackedVersion, bool) {
		vss, ok := idAndDeps[depKey{solvable: id, name: name}]
		if !ok || firstErr != nil {
			// After an error (the source failed, or ctx was cancelled) the
			// order no longer matters: the solve stops.
			return trackedVersion{}, false
		}
		var acc trackedVersion
		have := false
		for _, vs := range vss {
			hv, err := p.findHighestVersion(ctx, cache, vs)
			if err != nil {
				firstErr = err
				return trackedVersion{}, false
			}
			if !hv.ok {
				continue
			}
			cur := trackedVersion{version: hv.version, tf: hv.tf}
			// Keep the more restrictive (worse) of the two maxima.
			if !have || compareTracked(acc, cur) <= 0 {
				acc = cur
			}
			have = true
		}
		return acc, have
	}

	scores := make([]score, len(ss)*len(names))
	pos := make(map[cdcl.SolvableID]int, len(ss))
	for i, id := range ss {
		pos[id] = i
	}
	scoreOf := func(id cdcl.SolvableID, n int) *score {
		s := &scores[pos[id]*len(names)+n]
		if !s.done {
			s.v, s.ok = best(id, names[n])
			s.done = true
		}
		return s
	}
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		for n := range names {
			sa, sb := scoreOf(a, n), scoreOf(b, n)
			var c int
			switch {
			case sa.ok && sb.ok:
				c = compareTracked(sa.v, sb.v)
			case sa.ok:
				c = -1
			case sb.ok:
				c = 1
			}
			if c != 0 {
				return c < 0
			}
		}
		return timestampCompare(&p.solvables[b], &p.solvables[a]) < 0
	})
	return firstErr
}

// timestampCompare compares the timestamps of two candidates; a missing
// timestamp (0, or a virtual package) sorts below any other.
func timestampCompare(a, b *solvableInfo) int {
	ta, tb := int64(0), int64(0)
	hasA, hasB := !a.virtual && a.rec.Timestamp != 0, !b.virtual && b.rec.Timestamp != 0
	if hasA {
		ta = a.rec.Timestamp
	}
	if hasB {
		tb = b.rec.Timestamp
	}
	switch {
	case hasA != hasB:
		if hasA {
			return 1
		}
		return -1
	case ta < tb:
		return -1
	case ta > tb:
		return 1
	}
	return 0
}

// findHighestVersion returns the highest version among the candidates
// matching vs (excluded ones included, as in rattler), and whether the
// record carrying it has track_features. The answer is cached per version
// set.
func (p *provider) findHighestVersion(ctx context.Context, cache *cdcl.SolverCache, vs cdcl.VersionSetID) (highestVersion, error) {
	if hv, ok := p.highest[vs]; ok {
		return hv, nil
	}
	cands, err := cache.MatchingCandidates(ctx, vs)
	if err != nil {
		return highestVersion{}, err
	}
	var hv highestVersion
	for _, c := range cands {
		s := &p.solvables[c]
		if s.hidden {
			continue // not a record
		}
		tf := s.hasTF && !s.virtual
		if !hv.ok || s.version.Compare(hv.version) > 0 {
			hv = highestVersion{version: s.version, tf: tf, ok: true}
		}
	}
	p.highest[vs] = hv
	return hv, nil
}
