package goomba

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// fuzzRecords decodes data into a small package graph: the first byte picks
// the number of packages and their noarch kinds, each following pair of
// bytes adds a dependency (with a version constraint, a virtual package or
// an unknown name now and then).
func fuzzRecords(data []byte) []*record.PackageRecord {
	if len(data) == 0 {
		return nil
	}
	n := 1 + int(data[0])%10
	recs := make([]*record.PackageRecord, n)
	for i := range recs {
		recs[i] = &record.PackageRecord{Name: fmt.Sprintf("p%d", i), Version: "1", Build: "0"}
		if len(data) > 1 && data[1]>>(i%8)&1 == 1 {
			recs[i].Noarch = "python"
		}
	}
	for k := 2; k+1 < len(data) && k < 80; k += 2 {
		from, to := int(data[k])%n, int(data[k+1])
		dep := fmt.Sprintf("p%d", to%n)
		switch to / n % 5 {
		case 1:
			dep += " >=1"
		case 2:
			dep = "__glibc >=2.17"
		case 3:
			dep = "unknown"
		case 4:
			dep = strings.ToUpper(dep)
		}
		if to/n%7 == 6 {
			recs[from].Constrains = append(recs[from].Constrains, fmt.Sprintf("p%d <2", to%n))
			continue
		}
		recs[from].Depends = append(recs[from].Depends, dep)
	}
	return recs
}

func FuzzPlan(f *testing.F) {
	f.Add([]byte{3, 0b010, 0, 1, 1, 0})                           // python <-> pip
	f.Add([]byte{4, 0, 0, 1, 1, 2, 2, 3, 3, 0})                   // platform cycle
	f.Add([]byte{5, 0b11010, 0, 1, 1, 3, 3, 4, 4, 1, 2, 0, 0, 2}) // mixed
	f.Add([]byte{9, 0xff, 1, 2, 3, 4, 5, 6, 7, 8, 8, 1, 6, 66})
	f.Fuzz(func(t *testing.T, data []byte) {
		recs := fuzzRecords(data)
		if recs == nil {
			return
		}
		sorted, p := mustPlan(t, recs)
		checkPlan(t, sorted, p)
		want := fmt.Sprint(linkOrder(sorted, p), p.moved, p.graph.Layers(dimConstrains))
		// The plan depends neither on the package order nor on the order
		// of depends entries.
		rev := make([]*record.PackageRecord, len(recs))
		for i, r := range recs {
			c := *r
			c.Depends = slices.Clone(r.Depends)
			slices.Reverse(c.Depends)
			rev[len(recs)-1-i] = &c
		}
		sorted2, p2 := mustPlan(t, rev)
		if got := fmt.Sprint(linkOrder(sorted2, p2), p2.moved, p2.graph.Layers(dimConstrains)); got != want {
			t.Fatalf("plan depends on input order:\n%s\n%s", want, got)
		}
	})
}

func FuzzFillFromIndex(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"timestamp": 1700000000123, "license": "MIT", "track_features": "a,b c", "features": "f"}`,
		`{"timestamp": 1.7e12}`, `{"track_features": ["x", " ", "y"]}`, `{"timestamp": "soon"}`,
		`{"timestamp": 1e300}`, `{"license": null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var index map[string]json.RawMessage
		if json.Unmarshal(data, &index) != nil {
			return
		}
		var r record.PackageRecord
		if err := fillFromIndex(&r, index); err != nil {
			return
		}
		for _, tf := range r.TrackFeatures {
			if tf == "" || strings.TrimSpace(tf) != tf {
				t.Errorf("track feature %q", tf)
			}
		}
		if r.Timestamp < 0 && index["timestamp"] != nil && !strings.HasPrefix(strings.TrimSpace(string(index["timestamp"])), "-") {
			t.Errorf("timestamp %d from %s", r.Timestamp, index["timestamp"])
		}
	})
}

func FuzzClaims(f *testing.F) {
	f.Add([]byte{0, 1, 1, 1, 0x80, 2})
	f.Add([]byte{0, 0, 1, 3, 2, 3, 0x81, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		paths := []string{"a", "a/b", "a/b/c", "d", "a/e", "d/f"}
		dists := []string{"x-1-0", "y-1-0", "z-1-0"}
		dests := make([][]dest, 3)
		for k := 0; k+1 < len(data) && k < 60; k += 2 {
			pkg := int(data[k]) % 3
			kind := claimFile
			if data[k+1]&0x80 != 0 {
				kind = claimDir
			}
			dests[pkg] = append(dests[pkg], dest{path: paths[int(data[k+1]&0x7f)%len(paths)], kind: kind})
		}
		c := claims{}
		for pkg := range dests {
			err := c.add(pkg, dests[pkg], dists)
			if err == nil {
				continue
			}
			var ce *ClobberError
			if !errors.As(err, &ce) || ce.Packages[0] == ce.Packages[1] || ce.Path == "" {
				t.Fatalf("add: %v", err)
			}
			return
		}
		// No conflict reported: no non-directory path is wanted by two
		// packages, and no package's file is a parent of another's path.
		for i := range dests {
			for j := range dests {
				if i == j {
					continue
				}
				for _, a := range dests[i] {
					for _, b := range dests[j] {
						if a.path == b.path && (a.kind != claimDir || b.kind != claimDir) {
							t.Fatalf("%s wanted by %s and %s", a.path, dists[i], dists[j])
						}
						if a.kind == claimFile && strings.HasPrefix(b.path, a.path+"/") {
							t.Fatalf("%s's file %s is a parent of %s's %s", dists[i], a.path, dists[j], b.path)
						}
					}
				}
			}
		}
	})
}

func FuzzResolveInside(f *testing.F) {
	f.Add("file", "../file", "d", "x/../..")
	f.Add("../outside", "../../x", "/etc", "l1")
	f.Add("l2", "l1", "d/e/../../..", "missing/../../x")
	f.Fuzz(func(t *testing.T, t1, t2, t3, t4 string) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "d", "e"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		names := []string{"l1", "d/l2", "d/e/l3", "l4"}
		var made []string
		for i, target := range []string{t1, t2, t3, t4} {
			if target == "" || strings.ContainsRune(target, 0) || len(target) > 200 {
				continue
			}
			if os.Symlink(target, filepath.Join(root, filepath.FromSlash(names[i]))) == nil {
				made = append(made, names[i])
			}
		}
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range made {
			verdict := resolveInside(root, n)
			// Whatever the OS resolves the link to must be inside root
			// unless resolveInside refused it.
			if real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(n))); err == nil && verdict == nil {
				if rel, err := filepath.Rel(realRoot, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					t.Fatalf("%s resolves to %s outside %s, accepted", n, real, realRoot)
				}
			}
		}
	})
}
