package goomba

import (
	"fmt"

	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/spec"
)

// hostVirtual is a validated set of the host's virtual packages.
type hostVirtual struct {
	byName  map[string]*record.PackageRecord
	version map[string]spec.Version
}

// newHostVirtual checks recs (one record per virtual package name, each with
// a parsable version) and indexes them by name.
func newHostVirtual(recs []*record.PackageRecord) (*hostVirtual, error) {
	h := &hostVirtual{byName: map[string]*record.PackageRecord{}, version: map[string]spec.Version{}}
	for _, r := range recs {
		if r == nil || !r.IsVirtual() {
			return nil, fmt.Errorf("goomba: %w: virtual package list holds a record that is not a virtual package", ErrInvalidOptions)
		}
		if _, dup := h.byName[r.Name]; dup {
			return nil, fmt.Errorf("goomba: %w: virtual package %s listed twice", ErrInvalidOptions, r.Name)
		}
		v, err := spec.ParseVersion(r.Version)
		if err != nil {
			return nil, fmt.Errorf("goomba: %w: virtual package %s: %w", ErrInvalidOptions, r.Name, err)
		}
		h.byName[r.Name] = r
		h.version[r.Name] = v
	}
	return h, nil
}

// check returns a *VirtualError for the first entry of virt, in package and
// then list order, that the host does not satisfy: a dependency on a virtual
// package the host lacks or has in a version or build the spec excludes, or
// a constraint on a virtual package the host has in an excluded version or
// build. A constraint on a virtual package the host lacks is satisfied, as
// constraints are.
func (h *hostVirtual) check(recs []*record.PackageRecord, sc *specScan) error {
	for _, v := range sc.virt {
		ps := sc.vspecs[v.spec]
		name := ps.ms.Name.String()
		have, ok := h.byName[name]
		switch {
		case !ok && v.constrain:
		case !ok:
			return &VirtualError{Package: recs[v.pkg].Dist(), Spec: ps.text}
		case !ps.ms.MatchesVersion(have, h.version[name]):
			return &VirtualError{Package: recs[v.pkg].Dist(), Spec: ps.text, Have: have.String(), Constrains: v.constrain}
		}
	}
	return nil
}
