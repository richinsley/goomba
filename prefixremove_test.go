package goomba

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// fakePrefix is an in-memory prefixFS: a set of slash paths below the
// prefix, directories ending in "/". names lists the incomplete marker and
// conda-meta first, as tmpfs, btrfs and XFS short-form directories do,
// so that an unordered removal would drop the marker first. After every
// change it records whether the prefix, as a kill at that moment would
// leave it, still counts as interrupted or is empty.
type fakePrefix struct {
	t         *testing.T
	paths     map[string]bool
	ops       int
	failOn    string            // removeAll of this path fails
	markerErr error             // ensureMarker fails with this
	appear    map[string]string // when this path is removed, create that one (a straggling writer)
	invalid   []string          // states that were neither interrupted nor empty
}

func newFakePrefix(t *testing.T, paths ...string) *fakePrefix {
	f := &fakePrefix{t: t, paths: map[string]bool{}, appear: map[string]string{}}
	for _, p := range paths {
		f.paths[p] = true
	}
	return f
}

func (f *fakePrefix) children(dir string) []string {
	var out []string
	for p := range f.paths {
		rest, ok := strings.CutPrefix(p, dir)
		if dir != "" && !ok {
			continue
		}
		if dir == "" {
			rest = p
		}
		name, _, _ := strings.Cut(strings.TrimSuffix(rest, "/"), "/")
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	// Marker and conda-meta first, the rest in reverse order.
	slices.SortFunc(out, func(a, b string) int {
		rank := func(s string) int {
			if s == incompleteMarker || s == "conda-meta" {
				return 0
			}
			return 1
		}
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(b, a)
	})
	return out
}

func (f *fakePrefix) names(dir string) ([]string, error) {
	if dir != "" {
		if !f.paths[dir+"/"] {
			return nil, fs.ErrNotExist
		}
		dir += "/"
	}
	return f.children(dir), nil
}

func (f *fakePrefix) removeAll(rel string) error {
	if rel == f.failOn {
		return errors.New("injected failure")
	}
	// Remove the deepest entries first, one per step, as the real removal
	// does, checking the state after each.
	var under []string
	for p := range f.paths {
		if p == rel || p == rel+"/" || strings.HasPrefix(p, rel+"/") {
			under = append(under, p)
		}
	}
	slices.SortFunc(under, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	for _, p := range under {
		delete(f.paths, p)
		f.step()
	}
	if p, ok := f.appear[rel]; ok {
		delete(f.appear, rel)
		f.paths[p] = true
		f.step()
	}
	return nil
}

func (f *fakePrefix) ensureMarker() error {
	if f.markerErr != nil {
		return f.markerErr
	}
	if !f.paths["conda-meta/"] {
		for p := range f.paths {
			if p == "conda-meta" {
				return fmt.Errorf("conda-meta: %w", errNotADir)
			}
		}
		f.paths["conda-meta/"] = true
		f.step()
	}
	if !f.paths["conda-meta/"+incompleteMarker] {
		f.paths["conda-meta/"+incompleteMarker] = true
		f.step()
	}
	return nil
}

// state is the prefixState of the fake prefix.
func (f *fakePrefix) state() prefixState {
	st := prefixState{entries: min(len(f.children("")), 2)}
	if f.paths["conda-meta/"] {
		st.meta = true
		st.metaEntries = min(len(f.children("conda-meta/")), 1)
		st.marker = f.paths["conda-meta/"+incompleteMarker]
		st.history = f.paths["conda-meta/history"]
	}
	return st
}

func (f *fakePrefix) step() {
	f.ops++
	if st := f.state(); st.entries != 0 && !st.interrupted() {
		f.invalid = append(f.invalid, fmt.Sprintf("after step %d: %+v %q", f.ops, st, sortedKeys(f.paths)))
	}
}

// installed is a prefix as a transaction leaves it once it has linked
// everything and written the history.
func installed(extra ...string) []string {
	p := []string{
		"bin/", "bin/python3.12", "lib/", "lib/python3.12/", "lib/python3.12/site-packages/",
		"lib/python3.12/site-packages/a.py", "lib/libz.so", "share/", "share/terminfo/", "share/terminfo/x",
		"conda-meta/", "conda-meta/history", "conda-meta/python-3.12-0.json", "conda-meta/zlib-1-0.json",
	}
	return append(p, extra...)
}

// TestEmptyPrefixOrder checks emptyPrefix's order at every step, on a fake
// file system that lists the marker and conda-meta first: a kill at any
// moment leaves a prefix that is still interrupted, or nothing.
func TestEmptyPrefixOrder(t *testing.T) {
	for _, c := range []struct {
		name  string
		paths []string
		setup func(f *fakePrefix)
	}{
		{"marked", installed("conda-meta/" + incompleteMarker), nil},
		{"marker missing (failed commit)", installed(), nil},
		// openPrefix creates conda-meta before anything else, so a
		// prefix without it is empty (and stays so through the removal).
		{"empty, no conda-meta yet", nil, nil},
		{"only conda-meta", []string{"conda-meta/"}, nil},
		{"straggling writer", installed("conda-meta/" + incompleteMarker), func(f *fakePrefix) {
			f.appear["lib"] = "lib/python3.12/site-packages/late.pyc"
			f.appear["conda-meta/zlib-1-0.json"] = "share/late"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakePrefix(t, c.paths...)
			if c.setup != nil {
				c.setup(f)
			}
			if err := emptyPrefix(f); err != nil {
				t.Fatal(err)
			}
			if len(f.paths) != 0 {
				t.Errorf("left %q", sortedKeys(f.paths))
			}
			for _, s := range f.invalid {
				t.Errorf("a kill would leave files without the marker: %s", s)
			}
			t.Logf("%d steps", f.ops)
		})
	}
	t.Run("a removal fails", func(t *testing.T) {
		for _, fail := range []string{"lib", "share", "conda-meta/zlib-1-0.json", "conda-meta/history"} {
			f := newFakePrefix(t, installed("conda-meta/"+incompleteMarker)...)
			f.failOn = fail
			if err := emptyPrefix(f); err == nil {
				t.Errorf("%s: no error", fail)
			}
			if !f.state().interrupted() {
				t.Errorf("%s: the marker went although %q is left", fail, sortedKeys(f.paths))
			}
			for _, s := range f.invalid {
				t.Errorf("%s: %s", fail, s)
			}
		}
	})
	t.Run("conda-meta cannot be removed after the marker", func(t *testing.T) {
		// Something appears in conda-meta once the marker is gone, and
		// conda-meta then cannot be removed: the prefix is marked again.
		f := newFakePrefix(t, installed("conda-meta/"+incompleteMarker)...)
		f.failOn = "conda-meta"
		f.appear["conda-meta/"+incompleteMarker] = "conda-meta/late.json"
		if err := emptyPrefix(f); err == nil {
			t.Error("no error")
		}
		if !f.state().interrupted() {
			t.Errorf("not marked again: %q", sortedKeys(f.paths))
		}
	})
	t.Run("the marker cannot be made", func(t *testing.T) {
		// Out of descriptors, an I/O error, a full disk, conda-meta a
		// mount point: nothing is removed.
		f := newFakePrefix(t, installed()...)
		f.markerErr = errors.New("injected EIO")
		if err := emptyPrefix(f); err == nil {
			t.Error("no error")
		}
		if f.ops != 0 {
			t.Errorf("%d changes although the marker could not be made", f.ops)
		}
	})
	t.Run("conda-meta is not a directory", func(t *testing.T) {
		f := newFakePrefix(t, "conda-meta", "lib/", "lib/a")
		if err := emptyPrefix(f); err != nil {
			t.Fatal(err)
		}
		if len(f.paths) != 0 {
			t.Errorf("left %q", sortedKeys(f.paths))
		}
	})
}

// TestPrefixStateVerdict checks the classification CheckComplete and
// RemoveIncomplete share.
func TestPrefixStateVerdict(t *testing.T) {
	for _, c := range []struct {
		st          prefixState
		interrupted bool
		want        error // nil, ErrIncomplete or fs.ErrNotExist
	}{
		{prefixState{}, false, fs.ErrNotExist},
		{prefixState{entries: 2, meta: true, metaEntries: 1, history: true}, false, nil},
		{prefixState{entries: 1, meta: true, metaEntries: 1, history: true}, false, nil},
		{prefixState{entries: 2, meta: true, metaEntries: 1, marker: true, history: true}, true, ErrIncomplete},
		{prefixState{entries: 1, meta: true, metaEntries: 1, marker: true}, true, ErrIncomplete},
		{prefixState{entries: 1, meta: true}, true, ErrIncomplete}, // only an empty conda-meta
		{prefixState{entries: 2, meta: true}, false, fs.ErrNotExist},
		{prefixState{entries: 2, meta: true, metaEntries: 1}, false, fs.ErrNotExist},
		{prefixState{entries: 1}, false, fs.ErrNotExist},
	} {
		if got := c.st.interrupted(); got != c.interrupted {
			t.Errorf("%+v: interrupted %v", c.st, got)
		}
		err := c.st.verdict()
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%+v: verdict %v, want %v", c.st, err, c.want)
		}
	}
}
