package repodata

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// confusionChannels publishes a private channel with mypkg 1.0 (needing
// python 3.10) and a public one with mypkg 99.0, both sharded and both as
// full indexes.
func confusionChannels(t *testing.T, private omap) *testServer {
	t.Helper()
	ts := newTestServer(t)
	pub := shardMP(t, nil, omap{{"mypkg-99.0-0.conda", shardRec("mypkg", "99.0", "0")}})
	ts.putShards("/private", "linux-64", "", "", "", map[string][]byte{"mypkg": shardMP(t, nil, private)})
	ts.putShards("/private", "noarch", "", "", "", nil)
	ts.putShards("/public", "linux-64", "", "", "", map[string][]byte{"mypkg": pub})
	ts.putShards("/public", "noarch", "", "", "", nil)
	ts.put("/private-full/noarch/repodata.json", []byte(`{}`))
	ts.put("/public-full/noarch/repodata.json", []byte(`{}`))
	ts.put("/public-full/linux-64/repodata.json", []byte(`{"packages.conda": {"mypkg-99.0-0.conda": {"name": "mypkg", "version": "99.0", "build": "0", "sha256": "`+testSHA+`"}}}`))
	return ts
}

// S2: strict channel priority is decided by whether the higher channel
// lists the name, not by whether any of its records survive filtering.
func TestMultiDependencyConfusion(t *testing.T) {
	ctx := ctxT(t)
	privateGood := omap{{"mypkg-1.0-0.conda", shardRec("mypkg", "1.0", "0", "python >=3.10,<3.11")}}
	privateNoHash := omap{{"mypkg-1.0-0.conda", omap{{"name", "mypkg"}, {"version", "1.0"}, {"build", "0"}}}}
	privateFullJSON := func(fields string) []byte {
		return []byte(`{"packages.conda": {"mypkg-1.0-0.conda": {"name": "mypkg", "version": "1.0", "build": "0"` + fields + `}}}`)
	}
	cases := []struct {
		name        string
		private     omap
		privateJSON []byte
		python      string
		want        string // version returned, "" for none
	}{
		{"no prefilter: private wins", privateGood, privateFullJSON(`, "depends": ["python >=3.10,<3.11"]`), "", "1.0"},
		{"prefilter empties the private channel", privateGood, privateFullJSON(`, "depends": ["python >=3.10,<3.11"]`), "3.12", ""},
		{"private record lacks a hash / is malformed", privateNoHash, privateFullJSON(`, "build_number": -1`), "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := confusionChannels(t, c.private)
			ts.put("/private-full/linux-64/repodata.json", c.privateJSON)
			opt := testOptions(t, "linux-64")
			opt.PythonMinor = c.python
			open := func(path string) Source {
				src, err := NewChannel(ctx, ts.url(path), opt)
				if err != nil {
					t.Fatal(err)
				}
				return src
			}
			for _, pair := range [][2]Source{
				{open("/private"), open("/public")},           // sharded
				{open("/private-full"), open("/public-full")}, // full indexes
			} {
				recs, err := NewMulti(pair[0], pair[1]).Records(ctx, "mypkg")
				if err != nil {
					t.Fatal(err)
				}
				got := ""
				if len(recs) > 0 {
					got = recs[0].Version
				}
				if got != c.want || len(recs) > 1 {
					t.Errorf("%T: got %q (%d records), want %q", pair[0], got, len(recs), c.want)
				}
			}
		})
	}
}

// A removed-only package still claims its name.
func TestFullIndexHasNameIncludesRemoved(t *testing.T) {
	ts := newTestServer(t)
	ts.put("/c/noarch/repodata.json", []byte(`{"removed": ["gone-1.0-0.conda"], "packages.conda": {
		"bad-1.0-0.conda": {"name": "bad", "version": "1.0", "build": "0", "size": -1},
		"Upper-1.0-0.conda": {"name": "Upper", "version": "1.0", "build": "0"},
		"nameless-1.0-0.conda": {"version": "1.0", "build": "0"}}}`))
	fi, err := NewFullIndex(ctxT(t), ts.url("/c"), testOptions(t, "noarch"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"gone": true, "bad": true, "upper": true, "UPPER": true, "nameless": true, "other": false} {
		if got, err := fi.HasName(ctxT(t), name); err != nil || got != want {
			t.Errorf("HasName(%q) = %v, %v", name, got, err)
		}
	}
	if recs, _ := fi.Records(ctxT(t), "upper"); len(recs) != 1 {
		t.Errorf("upper: %d records", len(recs))
	}
}

// A Source without NameLister falls back to "has records".
type plainSource struct{ recs []*record.PackageRecord }

func (p plainSource) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	var out []*record.PackageRecord
	for _, r := range p.recs {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestMultiPriority(t *testing.T) {
	ctx := ctxT(t)
	a := NewMemory([]*record.PackageRecord{{Name: "x", Version: "1"}})
	b := plainSource{[]*record.PackageRecord{{Name: "x", Version: "2"}, {Name: "y", Version: "2"}}}
	empty := NewMemory(nil)
	m := NewMulti(empty, b, a)
	for name, want := range map[string]string{"x": "2", "y": "2"} {
		recs, err := m.Records(ctx, name)
		if err != nil || len(recs) != 1 || recs[0].Version != want {
			t.Errorf("%s: %+v, %v", name, recs, err)
		}
	}
	if recs, err := m.Records(ctx, "z"); err != nil || len(recs) != 0 {
		t.Errorf("z: %+v, %v", recs, err)
	}
	if ok, err := m.HasName(ctx, "y"); !ok || err != nil {
		t.Errorf("HasName(y) = %v, %v", ok, err)
	}
	if ok, err := m.HasName(ctx, "z"); ok || err != nil {
		t.Errorf("HasName(z) = %v, %v", ok, err)
	}
	// Memory lists names regardless of records' contents.
	if ok, _ := a.HasName(ctx, "X"); !ok {
		t.Error("Memory.HasName(X) = false")
	}
}

// N10: nil and typed-nil sources make Multi fail rather than skip them.
func TestMultiRejectsNilSources(t *testing.T) {
	ctx := ctxT(t)
	var sc *ShardedChannel
	var fi *FullIndex
	for i, sources := range [][]Source{
		{nil, NewMemory(nil)},
		{sc, NewMemory(nil)},
		{NewMemory(nil), fi},
	} {
		m := NewMulti(sources...)
		if _, err := m.Records(ctx, "x"); err == nil {
			t.Errorf("case %d: Records accepted a nil source", i)
		}
		if _, err := m.HasName(ctx, "x"); err == nil {
			t.Errorf("case %d: HasName accepted a nil source", i)
		}
		m.Prefetch(ctx, []string{"x"}) // must not panic
	}
	if _, err := NewMulti().Records(ctx, "x"); err != nil {
		t.Errorf("empty Multi: %v", err)
	}
	errSrc := NewMulti(failingSource{}, NewMemory([]*record.PackageRecord{{Name: "x"}}))
	if _, err := errSrc.Records(ctx, "x"); !errors.Is(err, errFailing) {
		t.Errorf("a failing higher-priority source: %v", err)
	}
}

var errFailing = errors.New("failing source")

type failingSource struct{}

func (failingSource) Records(context.Context, string) ([]*record.PackageRecord, error) {
	return nil, errFailing
}

// SF3(a): a shard index keyed "MyPkg" answers for "mypkg", as a full index
// does, so a private channel's casing cannot let a public one in.
func TestShardIndexCaseFolded(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	ts.putShards("/public", "linux-64", "", "", "", map[string][]byte{
		"mypkg": shardMP(t, nil, omap{{"mypkg-99.0-0.conda", shardRec("mypkg", "99.0", "0")}}),
	})
	ts.putShards("/public", "noarch", "", "", "", nil)
	ts.putShards("/private", "linux-64", "", "", "", map[string][]byte{
		"MyPkg": shardMP(t, nil, omap{{"MyPkg-1.0-0.conda", shardRec("MyPkg", "1.0", "0")}}),
		"other": shardMP(t, nil, omap{{"other-1.0-0.conda", shardRec("other", "1.0", "0")}}),
		"Other": shardMP(t, nil, omap{{"Other-2.0-0.conda", shardRec("Other", "2.0", "0")}}),
	})
	ts.putShards("/private", "noarch", "", "", "", nil)
	opt := testOptions(t, "linux-64")
	priv, err := NewShardedChannel(ctx, ts.url("/private"), opt)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := NewShardedChannel(ctx, ts.url("/public"), opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"mypkg", "MyPkg", "MYPKG"} {
		if ok, err := priv.HasName(ctx, q); !ok || err != nil {
			t.Errorf("HasName(%q) = %v, %v", q, ok, err)
		}
		recs, err := NewMulti(priv, pub).Records(ctx, q)
		if err != nil || len(recs) != 1 || recs[0].Version != "1.0" {
			t.Errorf("Multi.Records(%q) = %+v, %v", q, recs, err)
		}
	}
	// An exact key wins; otherwise every case-insensitive match answers.
	for q, want := range map[string][]string{
		"other": {"other-1.0-0.conda"},
		"Other": {"Other-2.0-0.conda"},
		"OTHER": {"Other-2.0-0.conda", "other-1.0-0.conda"},
	} {
		recs, err := priv.Records(ctx, q)
		if got := fns(recs); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Records(%q) = %q, %v", q, got, err)
		}
	}
}

// SF3(b): a full index lists a name both from the record and from its file
// name, so neither spelling falls through to a lower-priority channel.
func TestFullIndexListsFileAndRecordNames(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	long := strings.Repeat("x", maxFieldBytes) + "-1.0-0.conda"
	ts.put("/private/noarch/repodata.json", []byte(`{"packages.conda": {
		"mypkg-1.0-0.conda": {"name": "my_pkg", "version": "1.0", "build": "0", "sha256": "`+testSHA+`"},
		"`+long+`": {"name": "longpkg", "version": "1.0", "build": "0"}}}`))
	ts.put("/public/noarch/repodata.json", []byte(`{"packages.conda": {
		"mypkg-99.0-0.conda": {"name": "mypkg", "version": "99.0", "build": "0"},
		"my_pkg-99.0-0.conda": {"name": "my_pkg", "version": "99.0", "build": "0"},
		"longpkg-99.0-0.conda": {"name": "longpkg", "version": "99.0", "build": "0"}}}`))
	opt := testOptions(t, "noarch")
	priv, err := NewFullIndex(ctx, ts.url("/private"), opt)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := NewFullIndex(ctx, ts.url("/public"), opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"mypkg", "MyPkg", "my_pkg", "longpkg"} {
		if ok, _ := priv.HasName(ctx, q); !ok {
			t.Errorf("HasName(%q) = false", q)
		}
		if recs, err := NewMulti(priv, pub).Records(ctx, q); err != nil || len(recs) != 0 {
			t.Errorf("Multi.Records(%q) = %+v, %v (fell through)", q, recs, err)
		}
	}
}
