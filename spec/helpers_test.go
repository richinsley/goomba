package spec

import (
	"testing"
)

// ta is a test atom: libmamba's VersionPartAtom{num, lit}.
type ta struct {
	n uint64
	l string
}

// tp is a test part: libmamba's VersionPart.
type tp []ta

// mkSection appends parts as atoms, as libmamba's CommonVersion
// constructor does (implicit leading zeros off).
func mkSection(dst []atom, parts []tp) []atom {
	for _, p := range parts {
		for i, a := range p {
			lit := asciiLower(a.l)
			var flags uint8
			if i == 0 {
				flags = flagPartStart
			}
			dst = append(dst, atom{lit: lit, num: a.n, kind: literalKind(lit), flags: flags})
		}
		if len(p) == 0 {
			// An empty part: libmamba's VersionPart{} compares like {0, ""}.
			dst = append(dst, atom{kind: kindEmpty, flags: flagPartStart})
		}
	}
	return dst
}

// mkVer is libmamba's Version(epoch, version, local).
func mkVer(epoch uint64, ver []tp, local []tp) Version {
	atoms := mkSection(nil, ver)
	n := len(atoms)
	atoms = mkSection(atoms, local)
	return Version{atoms: atoms, epoch: epoch, nver: n}
}

// mkPart builds a single part's atoms, optionally with an implicit zero.
func mkPart(implicit bool, p tp) []atom {
	atoms := mkSection(nil, []tp{p})
	if implicit && len(atoms) > 0 {
		atoms[0].flags |= flagImplicitZero
	}
	return atoms
}

func mustVersion(t testing.TB, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}

func mustVersionSpec(t testing.TB, s string) VersionSpec {
	t.Helper()
	v, err := ParseVersionSpec(s)
	if err != nil {
		t.Fatalf("ParseVersionSpec(%q): %v", s, err)
	}
	return v
}

func mustMatchSpec(t testing.TB, s string) MatchSpec {
	t.Helper()
	m, err := ParseMatchSpec(s)
	if err != nil {
		t.Fatalf("ParseMatchSpec(%q): %v", s, err)
	}
	return m
}

// sameStructure reports whether two versions have the same epoch, parts
// and atoms (ignoring implicit-zero flags), a stricter check than Compare.
func sameStructure(a, b Version) bool {
	if a.epoch != b.epoch || a.nver != b.nver || len(a.atoms) != len(b.atoms) {
		return false
	}
	for i := range a.atoms {
		x, y := a.atoms[i], b.atoms[i]
		if x.num != y.num || x.lit != y.lit || x.flags&flagPartStart != y.flags&flagPartStart {
			return false
		}
	}
	return true
}
