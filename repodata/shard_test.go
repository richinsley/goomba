package repodata

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// oneShard wraps one record under "packages" with the conventional file name.
func oneShard(t testing.TB, r omap, fn string) []byte {
	return shardMP(t, omap{{fn, r}}, nil)
}

func decodeOne(t *testing.T, data []byte) []*record.PackageRecord {
	t.Helper()
	recs, err := decodeShardForTest(t, data, "", nil)
	if err != nil {
		t.Fatalf("decodeShard: %v", err)
	}
	return recs
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shard parsing -
// Package record parsing". libmamba only checks that its helper produced a
// msgpack map; here each record is decoded and its fields checked.
func TestShardRecordParsing(t *testing.T) {
	const sha, md5 = testSHA, testMD5
	cases := []struct {
		name string
		spec recSpec
		keep bool
		want record.PackageRecord
	}{
		{
			"extended optional fields",
			recSpec{name: "ext-pkg", version: "1.0.0", build: "0", buildNumber: 1, sha256: sha, md5: md5, depends: []string{"dep1"}, constrains: []string{"constr"}, noarch: "python"},
			true,
			record.PackageRecord{Name: "ext-pkg", Version: "1.0.0", Build: "0", BuildNumber: 1, SHA256: sha, MD5: md5, Depends: []string{"dep1"}, Constrains: []string{"constr"}, Noarch: "python"},
		},
		{
			"all fields",
			recSpec{name: "test-pkg", version: "1.2.3", build: "build123", buildNumber: 42, sha256: sha, md5: md5, depends: []string{"dep1", "dep2"}, constrains: []string{"constraint1"}, noarch: "python"},
			true,
			record.PackageRecord{Name: "test-pkg", Version: "1.2.3", Build: "build123", BuildNumber: 42, SHA256: sha, MD5: md5, Depends: []string{"dep1", "dep2"}, Constrains: []string{"constraint1"}, Noarch: "python"},
		},
		{
			"sha256 as bytes",
			recSpec{name: "test-pkg", version: "1.0.0", build: "0", sha256: testHash, sha256Fmt: hashBytes},
			true,
			record.PackageRecord{Name: "test-pkg", Version: "1.0.0", Build: "0", SHA256: testHash},
		},
		{
			"md5 as bytes",
			recSpec{name: "test-pkg", version: "1.0.0", build: "0", md5: md5, md5Fmt: hashBytes},
			true,
			record.PackageRecord{Name: "test-pkg", Version: "1.0.0", Build: "0", MD5: md5},
		},
		{
			// No checksum: libmamba's parse_shard_package_record rejects it.
			"minimal fields",
			recSpec{name: "minimal-pkg", version: "1.0.0", build: "0"},
			false, record.PackageRecord{},
		},
		{
			"sha256 as array of bytes",
			recSpec{name: "test-pkg", version: "1.0.0", build: "0", sha256: sha, sha256Fmt: hashArrayBytes},
			true,
			record.PackageRecord{Name: "test-pkg", Version: "1.0.0", Build: "0", SHA256: sha},
		},
		{
			"md5 as array of bytes",
			recSpec{name: "test-pkg", version: "1.0.0", build: "0", md5: md5, md5Fmt: hashArrayBytes},
			true,
			record.PackageRecord{Name: "test-pkg", Version: "1.0.0", Build: "0", MD5: md5},
		},
		{
			"both checksums as arrays",
			recSpec{name: "test-pkg", version: "1.0.0", build: "0", sha256: sha, md5: md5, sha256Fmt: hashArrayBytes, md5Fmt: hashArrayBytes},
			true,
			record.PackageRecord{Name: "test-pkg", Version: "1.0.0", Build: "0", SHA256: sha, MD5: md5},
		},
		{
			"mixed hash formats",
			recSpec{name: "test-pkg", version: "1.0.0", build: "0", sha256: sha, md5: md5, sha256Fmt: hashArrayBytes, md5Fmt: hashBytes},
			true,
			record.PackageRecord{Name: "test-pkg", Version: "1.0.0", Build: "0", SHA256: sha, MD5: md5},
		},
		{
			"track features list",
			recSpec{name: "libblas", version: "3.11.0", build: "7_netlib", buildNumber: 7, sha256: sha, trackFeatures: []string{"blas_netlib", "blas_netlib_2"}},
			true,
			record.PackageRecord{Name: "libblas", Version: "3.11.0", Build: "7_netlib", BuildNumber: 7, SHA256: sha, TrackFeatures: []string{"blas_netlib", "blas_netlib_2"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn := c.spec.name + "-" + c.spec.version + "-" + c.spec.build + ".tar.bz2"
			recs := decodeOne(t, oneShard(t, c.spec.omap(t), fn))
			if !c.keep {
				if len(recs) != 0 {
					t.Fatalf("record kept: %+v", *recs[0])
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("got %d records", len(recs))
			}
			got := *recs[0]
			want := c.want
			want.Fn, want.URL, want.Channel, want.Subdir = got.Fn, got.URL, got.Channel, got.Subdir
			if got.Fn != fn {
				t.Errorf("Fn = %q", got.Fn)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shard parsing -
// ShardDict parsing". The libmamba fixtures carry no checksum, so their
// records are dropped (as libmamba's parser does); a variant with a
// checksum shows the structure is read.
func TestShardDictParsing(t *testing.T) {
	minimal := omap{{"name", "test-pkg"}, {"version", "1.0.0"}, {"build", "0"}, {"depends", []string{"dep1"}}}
	withSHA := append(append(omap{}, minimal...), kv{"sha256", testSHA})
	cases := []struct {
		name      string
		packages  omap
		conda     omap
		wantFiles []string
	}{
		{"packages", omap{{"test-pkg-1.0.0-0.tar.bz2", minimal}}, nil, []string{}},
		{"packages.conda", nil, omap{{"test-pkg-1.0.0-0.conda", omap{{"name", "test-pkg"}, {"version", "1.0.0"}, {"build", "0"}}}}, []string{}},
		{"both packages and packages.conda", omap{{"test-pkg-1.0.0-0.tar.bz2", minimal}}, omap{{"test-pkg-1.0.0-0.conda", minimal}}, []string{}},
		{"with checksums", omap{{"test-pkg-1.0.0-0.tar.bz2", withSHA}, {"test-pkg-0.9-0.tar.bz2", withSHA}}, omap{{"test-pkg-1.0.0-1.conda", withSHA}},
			// Only the file named after its record (test-pkg-1.0.0-0) is kept.
			[]string{"test-pkg-1.0.0-0.tar.bz2"}},
	}
	for _, c := range cases {
		recs := decodeOne(t, shardMP(t, c.packages, c.conda))
		got := fns(recs)
		if !reflect.DeepEqual(got, c.wantFiles) && !(len(got) == 0 && len(c.wantFiles) == 0) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.wantFiles)
		}
	}
}

// Same name-version-build in both sections: only .conda survives (here the
// two .tar.bz2 names with the dist of the .conda record are dropped).
func TestShardPrefersConda(t *testing.T) {
	r := omap{{"name", "bzip2"}, {"version", "1.0.8"}, {"build", "hd590300_5"}, {"sha256", testSHA}}
	recs := decodeOne(t, shardMP(t,
		omap{{"bzip2-1.0.8-hd590300_5.tar.bz2", r}},
		omap{{"bzip2-1.0.8-hd590300_5.conda", r}}))
	if got := fns(recs); !reflect.DeepEqual(got, []string{"bzip2-1.0.8-hd590300_5.conda"}) {
		t.Errorf("got %q", got)
	}
}

func TestShardRemoved(t *testing.T) {
	r := func(v string) omap {
		return omap{{"name", "pkg"}, {"version", v}, {"build", "0"}, {"sha256", testSHA}}
	}
	data := mpEncode(t, omap{
		{"packages", omap{{"pkg-1-0.tar.bz2", r("1")}, {"pkg-2-0.tar.bz2", r("2")}}},
		{"packages.conda", omap{{"pkg-3-0.conda", r("3")}}},
		{"removed", []string{"pkg-2-0.tar.bz2", "pkg-3-0.conda", "other-1-0.conda"}},
	})
	if got := fns(decodeOne(t, data)); !reflect.DeepEqual(got, []string{"pkg-1-0.tar.bz2"}) {
		t.Errorf("got %q", got)
	}
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shard parsing - Hash
// format edge cases". Deviation: a present hash that does not decode drops
// the record (libmamba ignores the field and keeps the record when md5 is
// present). Losing the sha256 silently would not fail closed.
func TestShardHashFormats(t *testing.T) {
	base := func(extra ...kv) omap {
		return append(omap{{"name", "test-pkg"}, {"version", "1.0.0"}, {"build", "0"}}, extra...)
	}
	abab := bytes.Repeat([]byte{0xab}, 32)
	cases := []struct {
		name   string
		rec    omap
		keep   bool
		sha256 string
		md5    string
	}{
		{"sha256 as EXT", base(kv{"sha256", mpExtVal{0, abab}}), true, hex.EncodeToString(abab), ""},
		{"sha256 as NIL, md5 present", base(kv{"sha256", nil}, kv{"md5", testMD5}), true, "", testMD5},
		{"sha256 as array with negative integers", base(kv{"sha256", []any{mpI(-1), mpI(-2)}}), false, "", ""},
		{"sha256 as array with negative integers, md5 present", base(kv{"sha256", []any{mpI(-1), mpI(-2)}}, kv{"md5", testMD5}), false, "", ""},
		{"sha256 as array with invalid element types", base(kv{"sha256", []any{"ab"}}, kv{"md5", testMD5}), false, "", ""},
		{"sha256 as empty array", base(kv{"sha256", []any{}}, kv{"md5", testMD5}), false, "", ""},
		{"sha256 array element above 255", base(kv{"sha256", append(make([]any, 31), uint64(256))}), false, "", ""},
		{"sha256 bin of wrong length", base(kv{"sha256", abab[:31]}), false, "", ""},
		{"sha256 hex string of wrong length", base(kv{"sha256", testSHA[:63]}), false, "", ""},
		{"sha256 as integer", base(kv{"sha256", 5}), false, "", ""},
		{"md5 as 16-byte EXT", base(kv{"md5", mpExtVal{3, abab[:16]}}), true, "", hex.EncodeToString(abab[:16])},
		{"uppercase hex string", base(kv{"sha256", strings.ToUpper(testSHA)}), true, testSHA, ""},
	}
	for _, c := range cases {
		recs := decodeOne(t, oneShard(t, c.rec, "test-pkg-1.0.0-0.tar.bz2"))
		if !c.keep {
			if len(recs) != 0 {
				t.Errorf("%s: record kept: %+v", c.name, *recs[0])
			}
			continue
		}
		if len(recs) != 1 {
			t.Errorf("%s: got %d records", c.name, len(recs))
			continue
		}
		if recs[0].SHA256 != c.sha256 || recs[0].MD5 != c.md5 {
			t.Errorf("%s: sha256 %q md5 %q", c.name, recs[0].SHA256, recs[0].MD5)
		}
	}
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shard parsing -
// Package record error handling".
func TestShardRecordErrors(t *testing.T) {
	ok := omap{{"name", "test-pkg"}, {"version", "1.0.0"}, {"build", "0"}, {"sha256", testSHA}}
	cases := []struct {
		name string
		rec  omap
		keep bool
	}{
		{"missing checksums", omap{{"name", "test-pkg"}, {"version", "1.0.0"}, {"build", "0"}}, false},
		{"invalid key type only", omap{{42, "value"}}, false},
		{"invalid key type is skipped", append(omap{{42, "value"}, {uint64(7), omap{{"x", 1}}}}, ok...), true},
		// Deviation: libmamba keeps a record whose name is nil (empty name).
		{"nil required field", omap{{"name", nil}, {"version", "1.0.0"}, {"build", "0"}, {"sha256", testSHA}}, false},
		{"size field", append(append(omap{}, ok...), kv{"size", uint64(12345)}), true},
		{"negative size", append(append(omap{}, ok...), kv{"size", mpI(-5)}), false},
		{"negative build_number", append(append(omap{}, ok...), kv{"build_number", mpI(-1)}), false},
		{"string build_number", append(append(omap{}, ok...), kv{"build_number", "1"}), false},
		{"depends not an array", append(append(omap{}, ok...), kv{"depends", "python"}), false},
		{"depends with an integer", append(append(omap{}, ok...), kv{"depends", []any{"python", 3}}), false},
		{"depends with nil", append(append(omap{}, ok...), kv{"depends", []any{nil}}), false},
		{"depends nil", append(append(omap{}, ok...), kv{"depends", nil}), true},
		{"constrains map", append(append(omap{}, ok...), kv{"constrains", omap{{"a", "b"}}}), false},
		{"noarch unknown", append(append(omap{}, ok...), kv{"noarch", "weird"}), false},
		{"noarch integer", append(append(omap{}, ok...), kv{"noarch", 1}), false},
		{"track_features integer", append(append(omap{}, ok...), kv{"track_features", 1}), false},
		{"track_features list with int", append(append(omap{}, ok...), kv{"track_features", []any{"a", 1, "b"}}), false},
		{"python_site_packages_path escape", append(append(omap{}, ok...), kv{"python_site_packages_path", "../x"}), false},
		{"name with slash", omap{{"name", "a/b"}, {"version", "1"}, {"build", "0"}, {"sha256", testSHA}}, false},
		{"unknown nested fields skipped", append(append(omap{}, ok...), kv{"run_exports", omap{{"weak", []any{"a", omap{{"b", []any{1, 2}}}}}}}), true},
		{"float timestamp", append(append(omap{}, ok...), kv{"timestamp", 1.6e12}), true},
		{"fractional timestamp", append(append(omap{}, ok...), kv{"timestamp", 1.5}), false},
	}
	for _, c := range cases {
		recs := decodeOne(t, oneShard(t, c.rec, "test-pkg-1.0.0-0.tar.bz2"))
		if got := len(recs) == 1; got != c.keep {
			t.Errorf("%s: kept = %v, want %v", c.name, got, c.keep)
		}
		if c.name == "size field" && len(recs) == 1 && recs[0].Size != 12345 {
			t.Errorf("size = %d", recs[0].Size)
		}
	}
	// A record that is not a map, and a non-string file name.
	data := shardMP(t, omap{{"a-1-0.tar.bz2", "x"}, {5, ok}, {"test-pkg-1.0.0-0.tar.bz2", ok}}, nil)
	if got := fns(decodeOne(t, data)); !reflect.DeepEqual(got, []string{"test-pkg-1.0.0-0.tar.bz2"}) {
		t.Errorf("got %q", got)
	}
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shards - parse legacy
// boolean noarch field".
func TestShardLegacyBooleanNoarch(t *testing.T) {
	for _, c := range []struct {
		noarch bool
		want   string
	}{{true, "generic"}, {false, ""}} {
		r := omap{{"name", "tensorboard"}, {"version", "2.1.1"}, {"build", "py38_0"}, {"sha256", testSHA}, {"noarch", c.noarch}}
		recs := decodeOne(t, oneShard(t, r, "tensorboard-2.1.1-py38_0.tar.bz2"))
		if len(recs) != 1 || recs[0].Noarch != c.want {
			t.Errorf("noarch %v: got %+v", c.noarch, recs)
		}
	}
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shards -
// process_downloaded_shard": a compressed shard through decompression and
// decoding. ("No cache path found" and "file not found" concern libmamba's
// downloader plumbing and have no equivalent.)
func TestDecodeShardFile(t *testing.T) {
	d, err := newZstdDecoder(maxShardDecoded, 1)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := resolveBase(mustURL(t, "https://anaconda.org/conda-forge/linux-64/x"), "", urlPolicy{})
	loc := &locator{channel: "https://anaconda.org/conda-forge", subdir: "linux-64", base: base}
	shard := func(version, build string, deps ...string) []byte {
		spec := recSpec{name: "test-pkg", version: version, build: build, sha256: testSHA, depends: deps}
		return zst(t, oneShard(t, spec.omap(t), "test-pkg-"+version+"-"+build+".tar.bz2"))
	}
	for _, c := range []struct {
		version, build string
		deps           []string
	}{{"1.0.0", "0", []string{"dep1"}}, {"2.0.0", "1", []string{"dep-a", "dep-b"}}} {
		recs, _, err := decodeShardFile(d, shard(c.version, c.build, c.deps...), "test-pkg", loc, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 1 || recs[0].Fn != "test-pkg-"+c.version+"-"+c.build+".tar.bz2" ||
			recs[0].Name != "test-pkg" || recs[0].Version != c.version || recs[0].Build != c.build ||
			!reflect.DeepEqual(recs[0].Depends, c.deps) {
			t.Errorf("got %+v", recs)
		}
	}
	// Corrupted zstd data (libmamba create_corrupted_zstd_data).
	_, _, err = decodeShardFile(d, []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF}, "test-pkg", loc, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "zstd") {
		t.Errorf("corrupted zstd: %v", err)
	}
	// Valid zstd of invalid msgpack (libmamba create_invalid_msgpack_data).
	_, _, err = decodeShardFile(d, zst(t, []byte{0xFF, 0xFF, 0xFF, 0xFF}), "test-pkg", loc, nil)
	if err == nil || !strings.Contains(err.Error(), "msgpack") {
		t.Errorf("invalid msgpack: %v", err)
	}
	// Records named after another package are not this shard's.
	other := zst(t, oneShard(t, recSpec{name: "other", version: "1", build: "0", sha256: testSHA}.omap(t), "other-1-0.tar.bz2"))
	if recs, _, err := decodeShardFile(d, other, "test-pkg", loc, nil); err != nil || len(recs) != 0 {
		t.Errorf("foreign record: %v %+v", err, recs)
	}
	// Empty input and decompression bombs.
	if _, _, err := decodeShardFile(d, nil, "x", loc, nil); err == nil {
		t.Error("empty shard accepted")
	}
	small, _ := newZstdDecoder(1<<10, 1)
	if _, _, err := decodeShardFile(small, zst(t, make([]byte, 1<<16)), "x", loc, nil); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("bomb: %v", err)
	}
}

func TestShardDocumentErrors(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":              {},
		"not a map":          mpEncode(t, []any{"packages"}),
		"truncated":          mpEncode(t, omap{{"packages", omap{{"a", omap{{"name", "a"}}}}}})[:10],
		"trailing bytes":     append(mpEncode(t, omap{}), 0xc0),
		"packages array":     mpEncode(t, omap{{"packages", []any{}}}),
		"removed map":        mpEncode(t, omap{{"removed", omap{}}}),
		"removed ints":       mpEncode(t, omap{{"removed", []any{1}}}),
		"invalid byte":       {0x81, 0xc1},
		"huge array claim":   {0x81, 0xa8, 'p', 'a', 'c', 'k', 'a', 'g', 'e', 's', 0xdf, 0xff, 0xff, 0xff, 0xff},
		"deep nesting":       append(append([]byte{0x81, 0xa1, 'x'}, bytes.Repeat([]byte{0x91}, 100000)...), 0xc0),
		"deep nesting trunc": append([]byte{0x81, 0xa1, 'x'}, bytes.Repeat([]byte{0x91}, 100000)...),
	} {
		_, err := decodeShard(data, "", nil, interner{})
		if name == "deep nesting" {
			if err != nil {
				t.Errorf("%s: %v", name, err) // skipped iteratively, no stack growth
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// nil sections and unknown keys are fine.
	if _, err := decodeShard(mpEncode(t, omap{{"packages", nil}, {"packages.conda", nil}, {"removed", nil}, {"v3", omap{{"whl", omap{}}}}, {7, 8}}), "", nil, interner{}); err != nil {
		t.Errorf("nil sections: %v", err)
	}
}

// Cases from libmamba tests/src/core/test_shard_index_loader.cpp.
func TestDecodeShardIndex(t *testing.T) {
	hash1 := bytes.Repeat([]byte{0xAB}, 32)
	hash2 := bytes.Repeat([]byte{0xCD}, 32)

	t.Run("valid index with version field", func(t *testing.T) {
		data := shardIndexMP(t, "https://example.com/packages", "https://shards.example.com", "linux-64", 1, map[string][]byte{"python": hash1, "numpy": hash2})
		idx, err := decodeShardIndex(data)
		if err != nil {
			t.Fatal(err)
		}
		if idx.baseURL != "https://example.com/packages" || idx.shardsBaseURL != "https://shards.example.com" || idx.subdir != "linux-64" {
			t.Errorf("info = %+v", idx)
		}
		py, np := idx.shards["python"], idx.shards["numpy"]
		if len(idx.shards) != 2 || !bytes.Equal(py[:], hash1) || !bytes.Equal(np[:], hash2) {
			t.Errorf("shards = %v", idx.shards)
		}
	})
	t.Run("repodata_version field", func(t *testing.T) {
		data := mpEncode(t, omap{
			{"info", omap{{"base_url", "https://example.com/packages"}, {"shards_base_url", "https://shards.example.com"}, {"subdir", "noarch"}}},
			{"repodata_version", uint64(2)},
			{"shards", omap{{"test-pkg", bytes.Repeat([]byte{0xEF}, 32)}}},
		})
		idx, err := decodeShardIndex(data)
		if err != nil || idx.subdir != "noarch" || len(idx.shards) != 1 {
			t.Fatalf("got %+v, %v", idx, err)
		}
	})
	t.Run("missing info", func(t *testing.T) {
		idx, err := decodeShardIndex(mpEncode(t, omap{{"version", uint64(1)}, {"shards", omap{}}}))
		if err != nil || idx.baseURL != "" {
			t.Fatalf("got %+v, %v", idx, err)
		}
	})
	t.Run("large index", func(t *testing.T) {
		shards := map[string][]byte{}
		for i := 0; i < 1000; i++ {
			shards[fmt.Sprintf("pkg-%d", i)] = bytes.Repeat([]byte{byte(i % 256)}, 32)
		}
		idx, err := decodeShardIndex(shardIndexMP(t, "https://example.com/packages", "https://shards.example.com", "linux-64", 1, shards))
		if err != nil || len(idx.shards) != 1000 {
			t.Fatalf("got %d shards, %v", len(idx.shards), err)
		}
		if _, ok := idx.shards["pkg-0"]; !ok {
			t.Error("pkg-0 missing")
		}
		if _, ok := idx.shards["pkg-999"]; !ok {
			t.Error("pkg-999 missing")
		}
	})
	t.Run("non-string info values are ignored", func(t *testing.T) {
		data := mpEncode(t, omap{
			{"info", omap{{"base_url", uint64(0x4141414141)}, {"shards_base_url", []any{}}, {"subdir", "linux-64"}, {3, "x"}}},
			{"version", uint64(1)},
			{"shards", omap{}},
		})
		idx, err := decodeShardIndex(data)
		if err != nil || idx.baseURL != "" || idx.shardsBaseURL != "" || idx.subdir != "linux-64" {
			t.Fatalf("got %+v, %v", idx, err)
		}
	})
	t.Run("created_at and unknown keys", func(t *testing.T) {
		data := mpEncode(t, omap{
			{"version", uint64(1)},
			{"info", omap{{"base_url", ""}, {"shards_base_url", ""}, {"created_at", "2026-10-17T07:09:26Z"}, {"subdir", "noarch"}, {"repodata_revisions", omap{{"v3", omap{}}}}}},
			{"shards", omap{{"six", hash1}, {7, hash2}}},
			{"future", []any{1, 2}},
		})
		idx, err := decodeShardIndex(data)
		if err != nil || len(idx.shards) != 1 {
			t.Fatalf("got %+v, %v", idx, err)
		}
	})
	t.Run("over-long info strings", func(t *testing.T) {
		long := strings.Repeat("x", maxFieldBytes+1)
		for _, key := range []string{"base_url", "shards_base_url", "subdir"} {
			if _, err := decodeShardIndex(mpEncode(t, omap{{"info", omap{{key, long}}}, {"shards", omap{}}})); err == nil {
				t.Errorf("%s: accepted", key)
			}
		}
		if _, err := decodeShardIndex(mpEncode(t, omap{{"info", omap{{"created_at", long}}}, {"shards", omap{}}})); err != nil {
			t.Errorf("unused info key: %v", err)
		}
	})
	// Deviations from libmamba, which accepts all of these: goomba refuses an
	// index it cannot fully trust (fail closed).
	for name, data := range map[string][]byte{
		"negative integer version":        mpEncode(t, omap{{"info", omap{}}, {"version", mpI(-1)}, {"shards", omap{}}}),
		"version 2":                       mpEncode(t, omap{{"version", uint64(2)}, {"shards", omap{}}}),
		"missing shards field":            mpEncode(t, omap{{"info", omap{{"subdir", "linux-64"}}}, {"version", uint64(1)}}),
		"not a map":                       mpEncode(t, []any{"info", uint64(1), omap{}}),
		"hex string hash with odd length": mpEncode(t, omap{{"shards", omap{{"python", "abc"}}}}),
		"hex string hash":                 mpEncode(t, omap{{"shards", omap{{"python", hex.EncodeToString(hash1)}}}}),
		"short hash":                      mpEncode(t, omap{{"shards", omap{{"python", hash1[:31]}}}}),
		"shards not a map":                mpEncode(t, omap{{"shards", []any{}}}),
		"info not a map":                  mpEncode(t, omap{{"info", "x"}, {"shards", omap{}}}),
		"empty":                           {},
		"invalid msgpack":                 {0xFF, 0xFF, 0xFF, 0xFF},
		"trailing data":                   append(mpEncode(t, omap{{"shards", omap{}}}), 0x00),
	} {
		if _, err := decodeShardIndex(data); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// Corrupted or empty zstd (the file-level cases).
	d, _ := newZstdDecoder(maxShardDecoded, 1)
	for name, data := range map[string][]byte{
		"empty file":     {},
		"corrupted zstd": {0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF},
		"invalid data":   {0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00},
	} {
		if _, err := decompressZstd(d, data); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// realShards returns the conda-forge noarch shards in testdata, by package.
func realShards() map[string]string {
	return map[string]string{
		"six":       "0f32d05fecdecd0d1814a2b8da90560944fc991288670a062c3d9648da820225",
		"wheel":     "6dc058e3e933b5078ed23010ed83925281a6f4efb24468a91c20983c953775fc",
		"packaging": "b0a1c7d2d9b20982c4f9d3b4dda91e967852d5172642219d194c62269362fa1e",
		"pip":       "00fbfaeb1ddb0776af2112243b78a5590f1dac35dcb2906605ba76181db82d0d",
	}
}

// The real conda-forge noarch shard index and shards in testdata.
func TestRealCondaForgeShards(t *testing.T) {
	d, err := newZstdDecoder(maxShardDecoded, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decompressZstd(d, readTestdata(t, "conda-forge/noarch/"+shardIndexFile))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := decodeShardIndex(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.shards) != 23942 || idx.subdir != "noarch" || idx.baseURL != "" || idx.shardsBaseURL != "" {
		t.Errorf("index: %d shards, info %q %q %q", len(idx.shards), idx.subdir, idx.baseURL, idx.shardsBaseURL)
	}
	base, _ := resolveBase(mustURL(t, cfChannel+"/noarch/"+shardIndexFile), idx.baseURL, urlPolicy{})
	loc := &locator{channel: cfChannel, subdir: "noarch", base: base}
	for name, h := range realShards() {
		sum := idx.shards[name]
		if hex.EncodeToString(sum[:]) != h {
			t.Errorf("%s: index hash %x", name, sum)
		}
		data := readTestdata(t, "conda-forge/noarch/"+h+".msgpack.zst")
		if got := sha256.Sum256(data); got != sum {
			t.Errorf("%s: shard bytes hash to %x (the hash is of the compressed bytes)", name, got)
		}
		if name == "pip" && len(data) != 6326 {
			t.Errorf("pip shard is %d bytes, want 6326", len(data))
		}
		recs, _, err := decodeShardFile(d, data, name, loc, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(recs) == 0 {
			t.Errorf("%s: no records", name)
		}
		for _, r := range recs {
			if r.Name != name || r.Subdir != "noarch" || len(r.SHA256) != 64 || len(r.MD5) != 32 || r.Channel != cfChannel ||
				r.URL != cfChannel+"/noarch/"+r.Fn || r.Noarch != "python" {
				t.Errorf("%s: bad record %+v", name, *r)
			}
		}
	}
	data := readTestdata(t, "conda-forge/noarch/"+realShards()["six"]+".msgpack.zst")
	recs, _, _ := decodeShardFile(d, data, "six", loc, nil)
	got := byFn(recs)["six-1.17.0-pyhd8ed1ab_0.conda"]
	want := &record.PackageRecord{
		Name: "six", Version: "1.17.0", Build: "pyhd8ed1ab_0", BuildNumber: 0,
		Depends: []string{"python >=3.9"}, License: "MIT", Noarch: "python", Subdir: "noarch",
		MD5:       "a451d576819089b0d672f18768be0f65",
		SHA256:    "41db0180680cc67c3fa76544ffd48d6a5679d96f4b71d7498a759e94edc9a2db",
		Size:      0x4001,
		Timestamp: 0x193958f8b3e,
		Fn:        "six-1.17.0-pyhd8ed1ab_0.conda",
		URL:       cfChannel + "/noarch/six-1.17.0-pyhd8ed1ab_0.conda",
		Channel:   cfChannel,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("six 1.17.0:\n got %+v\nwant %+v", got, want)
	}
}

func TestShardDuplicateKeys(t *testing.T) {
	ff := strings.Repeat("f", 64)
	data := mpEncode(t, omap{
		{"packages", omap{
			{"a-1-0.tar.bz2", shardRec("a", "1", "0")},
			{"a-1-0.tar.bz2", omap{{"name", "a"}, {"version", "1"}, {"build", "0"}, {"sha256", ff}}},
			{"b-1-0.tar.bz2", shardRec("b", "1", "0")},
			{"b-1-0.tar.bz2", omap{{"name", "b"}, {"version", "1"}, {"build", "0"}}}, // no checksum
			{"c-1-0.tar.bz2", append(shardRec("c", "1", "0"), kv{"name", nil})},      // nil resets
		}},
	})
	recs := decodeOne(t, data)
	if got := fns(recs); !reflect.DeepEqual(got, []string{"a-1-0.tar.bz2"}) || recs[0].SHA256 != ff {
		t.Errorf("got %q", got)
	}
	for _, key := range []string{"packages", "packages.conda", "removed"} {
		if _, err := decodeShard(mpEncode(t, omap{{key, nil}, {key, nil}}), "", nil, interner{}); err == nil || !strings.Contains(err.Error(), "repeated") {
			t.Errorf("repeated %q: err = %v", key, err)
		}
	}
	for _, key := range []string{"shards", "info", "version"} {
		if _, err := decodeShardIndex(mpEncode(t, omap{{"shards", omap{}}, {key, nil}, {key, nil}})); err == nil || !strings.Contains(err.Error(), "repeated") {
			t.Errorf("index: repeated %q: err = %v", key, err)
		}
	}
}

// N2: sections are separate; a misplaced extension is dropped without
// touching the other section.
func TestShardMisplacedExtension(t *testing.T) {
	r := shardRec("a", "1", "0")
	recs := decodeOne(t, shardMP(t, omap{{"a-1-0.conda", r}}, omap{{"a-1-0.conda", r}}))
	if got := fns(recs); !reflect.DeepEqual(got, []string{"a-1-0.conda"}) {
		t.Errorf("got %q", got)
	}
}
