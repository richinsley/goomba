package repodata

import (
	"bytes"
	"compress/bzip2"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fullDoc = `{
	"info": {"subdir": "linux-64"},
	"packages": {
		"bzip2-1.0.8-hd590300_5.tar.bz2": {"name": "bzip2", "version": "1.0.8", "build": "hd590300_5", "build_number": 5, "sha256": "` + testSHA + `"},
		"python-3.11.9-0.tar.bz2": {"name": "python", "version": "3.11.9", "build": "0", "depends": ["bzip2 >=1.0.8"]},
		"old-1.0-0.tar.bz2": {"name": "old", "version": "1.0", "build": "0"}
	},
	"packages.conda": {
		"bzip2-1.0.8-hd590300_5.conda": {"name": "bzip2", "version": "1.0.8", "build": "hd590300_5", "build_number": 5, "sha256": "` + testSHA + `"},
		"numpy-1.26.4-py311_0.conda": {"name": "numpy", "version": "1.26.4", "build": "py311_0", "depends": ["python >=3.11,<3.12.0a0"]},
		"numpy-1.26.4-py312_0.conda": {"name": "numpy", "version": "1.26.4", "build": "py312_0", "depends": ["python >=3.12,<3.13.0a0"]}
	},
	"removed": ["old-1.0-0.tar.bz2"]
}`

const noarchDoc = `{"info": {"subdir": "noarch"}, "packages.conda": {
	"tzdata-2024a-h0c530f3_0.conda": {"name": "tzdata", "version": "2024a", "build": "h0c530f3_0", "noarch": "generic"},
	"Numpy-1.0-0.conda": {"name": "Numpy", "version": "1.0", "build": "0", "noarch": "python"}
}}`

func bz2Of(t *testing.T, path string) []byte {
	t.Helper()
	// compress/bzip2 cannot write; use the bz2 fixture for that variant.
	return readTestdata(t, path)
}

func TestFullIndexVariants(t *testing.T) {
	ctx := ctxT(t)
	for _, variant := range []string{"repodata.json.zst", "repodata.json", "repodata.json.bz2"} {
		t.Run(variant, func(t *testing.T) {
			ts := newTestServer(t)
			switch variant {
			case "repodata.json.zst":
				ts.put("/c/linux-64/repodata.json.zst", zst(t, []byte(fullDoc)))
				ts.put("/c/noarch/repodata.json.zst", zst(t, []byte(noarchDoc)))
			case "repodata.json":
				ts.put("/c/linux-64/repodata.json", []byte(fullDoc))
				ts.put("/c/noarch/repodata.json", []byte(noarchDoc))
			default:
				// linux-64 absent; noarch only as the micromamba bz2 fixture.
				ts.put("/c/noarch/repodata.json.bz2", bz2Of(t, "micromamba/test-server/noarch/repodata.json.bz2"))
			}
			fi, err := NewFullIndex(ctx, ts.url("/c"), testOptions(t, "linux-64"))
			if err != nil {
				t.Fatal(err)
			}
			if variant == "repodata.json.bz2" {
				recs, err := fi.Records(ctx, "test-package")
				if err != nil || len(recs) != 1 || recs[0].URL != ts.url("/c/noarch/test-package-0.1-0.tar.bz2") {
					t.Fatalf("got %+v, %v", recs, err)
				}
				return
			}
			if got := fi.Names(); !reflect.DeepEqual(got, []string{"bzip2", "numpy", "python", "tzdata"}) {
				t.Errorf("Names = %q", got)
			}
			recs, _ := fi.Records(ctx, "bzip2")
			if got := fns(recs); !reflect.DeepEqual(got, []string{"bzip2-1.0.8-hd590300_5.conda"}) {
				t.Errorf("bzip2: %q", got)
			}
			if recs, _ := fi.Records(ctx, "old"); len(recs) != 0 {
				t.Error("removed record returned")
			}
			// Names are case-insensitive; both subdirs are merged, sorted.
			recs, _ = fi.Records(ctx, "NUMPY")
			if got := fns(recs); !reflect.DeepEqual(got, []string{"numpy-1.26.4-py311_0.conda", "numpy-1.26.4-py312_0.conda", "Numpy-1.0-0.conda"}) {
				t.Errorf("numpy: %q", got)
			}
			if recs[2].Subdir != "noarch" || recs[2].URL != ts.url("/c/noarch/Numpy-1.0-0.conda") {
				t.Errorf("noarch record %+v", *recs[2])
			}
		})
	}
}

func TestFullIndexPythonPrefilter(t *testing.T) {
	ts := newTestServer(t)
	ts.put("/c/linux-64/repodata.json", []byte(fullDoc))
	ts.put("/c/noarch/repodata.json", []byte(noarchDoc))
	opt := testOptions(t, "linux-64")
	opt.PythonMinor = "3.12"
	fi, err := NewFullIndex(ctxT(t), ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := fi.Records(ctxT(t), "numpy")
	if got := fns(recs); !reflect.DeepEqual(got, []string{"numpy-1.26.4-py312_0.conda", "Numpy-1.0-0.conda"}) {
		t.Errorf("numpy: %q", got)
	}
}

// Cases from libmamba tests/src/core/test_subdir_index.cpp
// "SubdirIndexLoader", against an httptest server instead of
// conda.anaconda.org/quantstack and a file:// channel.
func TestFullIndexSubdirLoading(t *testing.T) {
	ctx := ctxT(t)
	serve := func(t *testing.T) *testServer {
		ts := newTestServer(t)
		ts.put("/quantstack/linux-64/repodata.json", []byte(fullDoc))
		ts.put("/quantstack/noarch/repodata.json", []byte(noarchDoc))
		return ts
	}
	t.Run("Create a subdir loader", func(t *testing.T) {
		u := subdirURL(mustURL(t, "https://conda.anaconda.org/quantstack"), "mamba-128").JoinPath("foo.json")
		if u.String() != "https://conda.anaconda.org/quantstack/mamba-128/foo.json" {
			t.Errorf("repodata URL %s", u)
		}
	})
	t.Run("Download indexes", func(t *testing.T) {
		ts := serve(t)
		opt := testOptions(t, "linux-64")
		if _, err := NewFullIndex(ctx, ts.url("/quantstack"), opt); err != nil {
			t.Fatal(err)
		}
		entries, _ := filepath.Glob(filepath.Join(opt.CacheDir, "index", "*"))
		var bodies int
		for _, e := range entries {
			data, _ := os.ReadFile(e)
			if bytes.Contains(data, []byte(`"packages`)) {
				bodies++
			}
		}
		if bodies != 2 {
			t.Errorf("%d cached index bodies among %q", bodies, entries)
		}
	})
	t.Run("No download offline", func(t *testing.T) {
		ts := serve(t)
		opt := testOptions(t, "linux-64")
		opt.Offline = true
		if _, err := NewFullIndex(ctx, ts.url("/quantstack"), opt); !errors.Is(err, ErrNotCached) {
			t.Errorf("err = %v", err)
		}
		if n := ts.totalRequests("/"); n != 0 {
			t.Errorf("%d requests offline", n)
		}
	})
	t.Run("Local noarch-only repo offline", func(t *testing.T) {
		ts := newTestServer(t)
		ts.put("/repo/noarch/repodata.json", readTestdata(t, "micromamba/test-server/noarch/repodata.json"))
		opt := testOptions(t, "linux-64")
		if _, err := NewFullIndex(ctx, ts.url("/repo"), opt); err != nil {
			t.Fatal(err)
		}
		// The absence of linux-64 is cached too, so offline mode works.
		opt.Offline = true
		fi, err := NewFullIndex(ctx, ts.url("/repo"), opt)
		if err != nil {
			t.Fatal(err)
		}
		if recs, _ := fi.Records(ctx, "test-package"); len(recs) != 1 {
			t.Errorf("got %d records", len(recs))
		}
	})
	t.Run("Download indexes repodata ttl", func(t *testing.T) {
		ts := serve(t)
		opt := testOptions(t, "linux-64") // MaxAge 0: always revalidate
		if _, err := NewFullIndex(ctx, ts.url("/quantstack"), opt); err != nil {
			t.Fatal(err)
		}
		// Reloading: the cache is expired, so each index is revalidated with
		// a conditional GET answered 304.
		if _, err := NewFullIndex(ctx, ts.url("/quantstack"), opt); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"/quantstack/linux-64/repodata.json", "/quantstack/noarch/repodata.json"} {
			if ts.notModified(p) != 1 {
				t.Errorf("%s: %d conditional hits", p, ts.notModified(p))
			}
		}
		// With a long MaxAge the cache is used without asking.
		opt.MaxAge = time.Hour
		before := ts.totalRequests("/")
		if _, err := NewFullIndex(ctx, ts.url("/quantstack"), opt); err != nil {
			t.Fatal(err)
		}
		if ts.totalRequests("/") != before {
			t.Error("fresh cache revalidated")
		}
	})
}

func TestFullIndexMissingNoarch(t *testing.T) {
	ts := newTestServer(t)
	ts.put("/c/linux-64/repodata.json", []byte(fullDoc))
	_, err := NewFullIndex(ctxT(t), ts.url("/c"), testOptions(t, "linux-64"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v, want an HTTPError 404 too", err)
	}
}

func TestFullIndexUpdatedOnServer(t *testing.T) {
	ctx := ctxT(t)
	ts := newTestServer(t)
	ts.put("/c/noarch/repodata.json", []byte(noarchDoc))
	opt := testOptions(t, "noarch")
	if _, err := NewFullIndex(ctx, ts.url("/c"), opt); err != nil {
		t.Fatal(err)
	}
	ts.put("/c/noarch/repodata.json", []byte(`{"packages.conda": {"new-1-0.conda": {"name": "new", "version": "1", "build": "0"}}}`))
	fi, err := NewFullIndex(ctx, ts.url("/c"), opt)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Names(); !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("Names = %q", got)
	}
}

func TestFullIndexCancelled(t *testing.T) {
	ts := newTestServer(t)
	ts.put("/c/noarch/repodata.json", []byte(noarchDoc))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFullIndex(ctx, ts.url("/c"), testOptions(t, "noarch")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	fi := &FullIndex{byName: nil}
	if _, err := fi.Records(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Records err = %v", err)
	}
	// The parse itself checks ctx between reads.
	r := &ctxReader{ctx: ctx, r: strings.NewReader("{}")}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Errorf("ctxReader err = %v", err)
	}
}

func TestLimitedReader(t *testing.T) {
	r := &limitedReader{r: strings.NewReader("0123456789"), left: 10}
	if b, err := io.ReadAll(r); err != nil || string(b) != "0123456789" {
		t.Errorf("exact limit: %q %v", b, err)
	}
	r = &limitedReader{r: strings.NewReader("0123456789x"), left: 10}
	if _, err := io.ReadAll(r); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over limit: %v", err)
	}
	// bz2 and zstd decompression feed the same reader.
	for _, name := range []string{"repodata.json.bz2", "repodata.json.zst", "repodata.json"} {
		body := []byte(`{}`)
		switch name {
		case "repodata.json.bz2":
			body = readTestdata(t, "micromamba/test-server/noarch/repodata.json.bz2")
		case "repodata.json.zst":
			body = zst(t, body)
		}
		rd, err := decompressIndex(name, body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(rd); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		rd.Close()
	}
	if _, err := io.ReadAll(bzip2.NewReader(strings.NewReader("garbage"))); err == nil {
		t.Error("bzip2 garbage accepted")
	}
}
