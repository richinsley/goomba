package solve

import (
	"fmt"
	"sort"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/spec"
)

// installed is a package in a candidate solution.
type installed struct {
	rec     *record.PackageRecord
	version spec.Version
	virtual bool
}

// Validate checks that recs is a valid solution of req under conda's rules,
// independently of how it was found:
//
//   - every record is non-nil, has a valid lower-case package name, a
//     version that parses, a sha256 (64 hex digits) and depends and
//     constrains that parse (no conditional dependencies or extras), and is
//     not a virtual package;
//   - there is at most one record per package name;
//   - every spec of req.Specs is satisfied by an installed record or a
//     virtual package of req.Virtual;
//   - every depends of every record is satisfied by an installed record or
//     virtual package;
//   - every constrains of every record, and every req.Constraints entry,
//     holds for the package it names if that package is installed (every
//     virtual package of req.Virtual counts as installed);
//   - a package that has a record in req.Locked is installed only as that
//     record.
//
// It returns nil for a valid solution, an error wrapping ErrInvalidRequest
// for a malformed request, and otherwise an error wrapping
// ErrInvalidSolution that names the first violation (records are checked
// in name order). It does not check minimality or candidate preference.
//
// Error messages show untrusted text escaped as explanations do.
func Validate(req Request, recs []*record.PackageRecord) error {
	return sanitizeError(validate(req, recs))
}

// validate is Validate with error messages as produced.
func validate(req Request, recs []*record.PackageRecord) error {
	alias := channelAlias(req.ChannelAlias)
	if err := checkAlias(alias); err != nil {
		return err
	}
	pkgs := map[string]installed{}

	for i, r := range req.Virtual {
		if err := checkVirtual(i, r); err != nil {
			return err
		}
		if _, dup := pkgs[r.Name]; dup {
			return fmt.Errorf("%w: virtual package %q is given twice", ErrInvalidRequest, r.Name)
		}
		v, err := parseRecordVersion(r)
		if err != nil {
			return fmt.Errorf("%w: virtual package %q: %w", ErrInvalidRequest, r.Name, err)
		}
		pkgs[r.Name] = installed{rec: r, version: v, virtual: true}
	}
	locked := map[string]*record.PackageRecord{}
	if err := collectRecords("locked", req.Locked, locked); err != nil {
		return err
	}
	favored := map[string]*record.PackageRecord{}
	if err := collectRecords("favored", req.Favored, favored); err != nil {
		return err
	}
	specs := make([]parsedSpec, len(req.Specs))
	for i, s := range req.Specs {
		ps, err := parseSpec(s, alias)
		if err != nil {
			return fmt.Errorf("%w: spec %q: %w", ErrInvalidRequest, s, err)
		}
		specs[i] = ps
	}
	constraints := make([]parsedSpec, len(req.Constraints))
	for i, s := range req.Constraints {
		ps, err := parseSpec(s, alias)
		if err != nil {
			return fmt.Errorf("%w: constraint %q: %w", ErrInvalidRequest, s, err)
		}
		constraints[i] = ps
	}

	sorted := make([]*record.PackageRecord, 0, len(recs))
	for i, r := range recs {
		if r == nil {
			return fmt.Errorf("%w: record %d is nil", ErrInvalidSolution, i)
		}
		sorted = append(sorted, r)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	// Each record on its own.
	type parsedRecord struct {
		rec        *record.PackageRecord
		depends    []parsedSpec
		constrains []parsedSpec
	}
	parsed := make([]parsedRecord, 0, len(sorted))
	for _, r := range sorted {
		switch {
		case isVirtualName(r.Name):
			return fmt.Errorf("%w: %s: virtual packages come only from the target machine", ErrInvalidSolution, safeRecord(r))
		case !isPackageName(r.Name):
			return fmt.Errorf("%w: %s: invalid package name", ErrInvalidSolution, safeRecord(r))
		case r.SHA256 == "" || !isSHA256(r.SHA256):
			return fmt.Errorf("%w: %s: no valid sha256, it cannot be verified", ErrInvalidSolution, safeRecord(r))
		}
		if prev, dup := pkgs[r.Name]; dup {
			if prev.virtual {
				return fmt.Errorf("%w: %s: virtual packages come only from the target machine", ErrInvalidSolution, safeRecord(r))
			}
			return fmt.Errorf("%w: two records for %q: %s and %s", ErrInvalidSolution, r.Name, safeRecord(prev.rec), safeRecord(r))
		}
		v, err := parseRecordVersion(r)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInvalidSolution, safeRecord(r), err)
		}
		pkgs[r.Name] = installed{rec: r, version: v}
		pr := parsedRecord{rec: r}
		for _, d := range r.Depends {
			ps, err := parseSpec(d, alias)
			if err != nil {
				return fmt.Errorf("%w: %s: %s", ErrInvalidSolution, safeRecord(r), sanitize(exclusionForDependency(d, err)))
			}
			pr.depends = append(pr.depends, ps)
		}
		for _, c := range r.Constrains {
			ps, err := parseSpec(c, alias)
			if err != nil {
				return fmt.Errorf("%w: %s: %s", ErrInvalidSolution, safeRecord(r), sanitize(exclusionForDependency(c, err)))
			}
			pr.constrains = append(pr.constrains, ps)
		}
		parsed = append(parsed, pr)
	}

	// Locked packages.
	for _, pr := range parsed {
		if l, ok := locked[pr.rec.Name]; ok && !sameRecord(l, pr.rec) {
			return fmt.Errorf("%w: %s is installed but %s is locked", ErrInvalidSolution, safeRecord(pr.rec), safeRecord(l))
		}
	}

	satisfied := func(ps *parsedSpec) (installed, bool, bool) {
		inst, ok := pkgs[ps.name]
		if !ok {
			return inst, false, false
		}
		return inst, true, recordMatches(ps, inst.rec, inst.version, inst.virtual)
	}
	for i := range specs {
		ps := &specs[i]
		inst, ok, match := satisfied(ps)
		switch {
		case !ok:
			return fmt.Errorf("%w: spec %q is not satisfied: %s is not installed", ErrInvalidSolution, req.Specs[i], ps.name)
		case !match:
			return fmt.Errorf("%w: spec %q is not satisfied by %s", ErrInvalidSolution, req.Specs[i], safeRecord(inst.rec))
		}
	}
	for _, pr := range parsed {
		for i := range pr.depends {
			ps := &pr.depends[i]
			inst, ok, match := satisfied(ps)
			switch {
			case !ok:
				return fmt.Errorf("%w: %s depends on %q, but %s is not installed", ErrInvalidSolution, safeRecord(pr.rec), pr.rec.Depends[i], ps.name)
			case !match:
				return fmt.Errorf("%w: %s depends on %q, which %s does not satisfy", ErrInvalidSolution, safeRecord(pr.rec), pr.rec.Depends[i], safeRecord(inst.rec))
			}
		}
		for i := range pr.constrains {
			ps := &pr.constrains[i]
			if inst, ok, match := satisfied(ps); ok && !match {
				return fmt.Errorf("%w: %s constrains %q, which %s does not satisfy", ErrInvalidSolution, safeRecord(pr.rec), pr.rec.Constrains[i], safeRecord(inst.rec))
			}
		}
	}
	for i := range constraints {
		ps := &constraints[i]
		if inst, ok, match := satisfied(ps); ok && !match {
			return fmt.Errorf("%w: constraint %q does not hold for %s", ErrInvalidSolution, req.Constraints[i], safeRecord(inst.rec))
		}
	}
	return nil
}

// safeRecord formats a record for an error message.
func safeRecord(r *record.PackageRecord) string { return sanitize(displayRecord(r)) }
