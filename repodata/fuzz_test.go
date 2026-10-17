package repodata

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/richinsley/goomba/record"
)

// checkRecordInvariants verifies what every normalized record promises.
func checkRecordInvariants(t *testing.T, recs []*record.PackageRecord, subdir string) {
	t.Helper()
	dists := map[string]string{}
	seen := map[string]bool{}
	for i, r := range recs {
		if seen[r.Fn] {
			t.Fatalf("duplicate file %s", r.Fn)
		}
		seen[r.Fn] = true
		if !validIdent(r.Name) || !validIdent(r.Version) || !validIdent(r.Build) {
			t.Fatalf("bad identity %q %q %q", r.Name, r.Version, r.Build)
		}
		conda := strings.HasSuffix(r.Fn, extConda)
		if !validFilename(r.Fn, conda) || r.Fn != r.Dist()+ext(conda) {
			t.Fatalf("bad file name %q for %s", r.Fn, r.Dist())
		}
		if len(r.Depends) > maxListEntries || len(r.Constrains) > maxListEntries || len(r.TrackFeatures) > maxListEntries {
			t.Fatalf("list over the cap")
		}
		if r.Subdir != subdir {
			t.Fatalf("subdir %q", r.Subdir)
		}
		u, err := url.Parse(r.URL)
		if err != nil || (urlPolicy{}).check(u) != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("bad URL %q", r.URL)
		}
		if r.MD5 != "" {
			if h, ok := normalizeHex(r.MD5, 16); !ok || h != r.MD5 {
				t.Fatalf("bad md5 %q", r.MD5)
			}
		}
		if r.SHA256 != "" {
			if h, ok := normalizeHex(r.SHA256, 32); !ok || h != r.SHA256 {
				t.Fatalf("bad sha256 %q", r.SHA256)
			}
		}
		switch r.Noarch {
		case "", "python", "generic":
		default:
			t.Fatalf("bad noarch %q", r.Noarch)
		}
		if r.Size < 0 || r.Timestamp < 0 {
			t.Fatalf("negative size or timestamp")
		}
		if r.PythonSitePackagesPath != "" && !validSitePackagesPath(r.PythonSitePackagesPath) {
			t.Fatalf("bad site-packages path %q", r.PythonSitePackagesPath)
		}
		for _, tf := range r.TrackFeatures {
			if strings.TrimSpace(tf) == "" {
				t.Fatalf("empty track feature")
			}
		}
		if !validSpecs(r.Depends) || !validSpecs(r.Constrains) {
			t.Fatalf("bad specs")
		}
		if prev, ok := dists[r.Dist()]; ok && (strings.HasSuffix(prev, extConda) != conda) {
			t.Fatalf("both %s and %s kept", prev, r.Fn)
		}
		dists[r.Dist()] = r.Fn
		if i > 0 && recs[i-1].Fn > r.Fn {
			t.Fatalf("not sorted")
		}
	}
}

func FuzzLoadRepodataJSON(f *testing.F) {
	f.Add([]byte(fullDoc))
	f.Add([]byte(noarchDoc))
	f.Add([]byte(pkgDoc(`, "track_features": "a,b", "noarch": true, "timestamp": 1.6e12, "md5": "` + testMD5 + `"`)))
	f.Add([]byte(`{"info": {"subdir": "noarch", "base_url": "../pkgs/"}, "packages": {"a-1-0.tar.bz2": {"name": "a", "version": "1", "build": "0"}}, "removed": ["x"]}`))
	f.Add([]byte(`{"packages": {"a-1-0.tar.bz2": {"name": "a", "version": "1", "build": "0", "SHA256": "x", "depends": [null]}}}`))
	for _, p := range []string{
		"micromamba/test-server/noarch/repodata.json",
		"micromamba/channel_a/noarch/repodata.json",
		"micromamba/channel_b/noarch/repodata.json",
	} {
		f.Add(readTestdata(f, p))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		recs, err := LoadRepodataJSON(bytes.NewReader(data), "https://example.com/c", "noarch")
		if err != nil {
			return
		}
		checkRecordInvariants(t, recs, "noarch")
	})
}

func shardSeeds(f *testing.F) {
	f.Add(shardMP(f, omap{{"test-pkg-1.0.0-0.tar.bz2", recSpec{name: "test-pkg", version: "1.0.0", build: "0", sha256: testSHA, md5: testMD5, sha256Fmt: hashArrayBytes, md5Fmt: hashBytes, depends: []string{"python >=3.12,<3.13.0a0"}, trackFeatures: []string{"x"}}.omap(f)}}, nil))
	f.Add(shardMP(f, omap{{"test-pkg-1.0.0-0.tar.bz2", shardRec("test-pkg", "1.0.0", "0")}}, omap{{"test-pkg-1.0.0-0.conda", shardRec("test-pkg", "1.0.0", "0", "dep")}}))
	f.Add(mpEncode(f, omap{
		{"packages", omap{{"test-pkg-1-0.tar.bz2", append(shardRec("test-pkg", "1", "0"), kv{"noarch", true}, kv{"md5", mpExtVal{1, bytes.Repeat([]byte{1}, 16)}}, kv{"size", uint64(5)}, kv{"timestamp", 1.6e12}, kv{"run_exports", omap{{"weak", []any{"a"}}}})}}},
		{"removed", []string{"test-pkg-2-0.tar.bz2"}},
		{9, nil},
	}))
	f.Add([]byte{0x81, 0xa8, 'p', 'a', 'c', 'k', 'a', 'g', 'e', 's', 0xdf, 0xff, 0xff, 0xff, 0xff})
	d, err := newZstdDecoder(maxShardDecoded, 1)
	if err != nil {
		f.Fatal(err)
	}
	for _, h := range realShards() {
		raw, err := decompressZstd(d, readTestdata(f, "conda-forge/noarch/"+h+".msgpack.zst"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
}

func FuzzDecodeShard(f *testing.F) {
	shardSeeds(f)
	py := &pythonMinor{major: 3, minor: 12}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, name := range []string{"", "test-pkg"} {
			recs, err := decodeShardForTest(t, data, name, nil)
			if err != nil {
				continue
			}
			checkRecordInvariants(t, recs, "linux-64")
			for _, r := range recs {
				if name != "" && r.Name != name {
					t.Fatalf("foreign record %q in shard %q", r.Name, name)
				}
				if r.MD5 == "" && r.SHA256 == "" {
					t.Fatalf("record without checksum")
				}
			}
			filtered, err := decodeShardForTest(t, data, name, py)
			if err != nil || len(filtered) > len(recs) {
				t.Fatalf("prefilter changed the outcome: %v, %d > %d", err, len(filtered), len(recs))
			}
		}
	})
}

func FuzzDecodeShardIndex(f *testing.F) {
	f.Add(shardIndexMP(f, "https://example.com/packages", "https://shards.example.com", "linux-64", 1, map[string][]byte{"python": bytes.Repeat([]byte{0xab}, 32), "numpy": bytes.Repeat([]byte{0xcd}, 32)}))
	f.Add(shardIndexMP(f, "", "", "noarch", 1, nil))
	f.Add(mpEncode(f, omap{{"info", omap{{"base_url", uint64(1)}, {"created_at", "2026-10-17T07:09:26Z"}}}, {"shards", omap{{"a", bytes.Repeat([]byte{1}, 32)}, {7, "x"}}}, {"version", mpI(-1)}}))
	f.Add(mpEncode(f, omap{{"shards", omap{{"python", "abc"}}}}))
	f.Fuzz(func(t *testing.T, data []byte) {
		idx, err := decodeShardIndex(data)
		if err != nil {
			return
		}
		if idx.shards == nil {
			t.Fatal("nil shards map without error")
		}
		// Whatever the info says, URL resolution must not panic and must
		// enforce the policy.
		index := mustURL(t, "https://conda.anaconda.org/c/noarch/"+shardIndexFile)
		for _, ref := range []string{idx.baseURL, idx.shardsBaseURL} {
			if u, err := resolveBase(index, ref, urlPolicy{}); err == nil {
				if (urlPolicy{}).check(u) != nil || !strings.HasSuffix(u.Path, "/") {
					t.Fatalf("resolveBase(%q) = %s", ref, u)
				}
			}
		}
	})
}

func FuzzMatchesPythonMinor(f *testing.F) {
	for _, s := range []string{
		"python >=3.12,<3.13.0a0", "python 3.7.12 0_73_pypy", "python", "python_abi 3.12.* *_cp312",
		"python >= 3.8, < 3.10", "python 2.7.*|>=3.5", "python !=3.12.*", "python[version='<3']",
		"conda-forge::python 3.7", "python 3.12.0post1", "python ~=3.13", "python=3.12", "python ,,x,,",
	} {
		f.Add(s, uint8(3), uint8(12))
	}
	f.Fuzz(func(t *testing.T, dep string, major, minor uint8) {
		p := &pythonMinor{major: uint64(major), minor: uint64(minor)}
		got := p.matches(dep)
		if got != p.matches(dep) {
			t.Fatal("not deterministic")
		}
		name := specName(dep)
		if name != "python" && !got {
			t.Fatalf("non-python spec %q filtered", dep)
		}
		if strings.ContainsAny(name, "/\\: \t") {
			t.Fatalf("specName(%q) = %q", dep, name)
		}
		// The series' least element always satisfies an exact pin on it.
		if !p.matches("python ==" + strconv.FormatUint(p.major, 10) + "." + strconv.FormatUint(p.minor, 10)) {
			t.Fatal("exact pin on the requested minor dropped")
		}
	})
}

func FuzzParseCacheEntry(f *testing.F) {
	f.Add([]byte(cacheMagic + `{"url_sha256":"ab","status":200,"fetched":"2026-01-01T00:00:00Z","size":2,"sha256":"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"}` + "\nab"))
	f.Add([]byte(cacheMagic + `{"url_sha256":"x","status":404,"fetched":"2026-01-01T00:00:00Z","size":0,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}` + "\n"))
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := parseCacheEntry(data)
		if err != nil {
			return
		}
		if int64(len(e.body)) != e.meta.Size {
			t.Fatal("size not enforced")
		}
		h, err := encodeCacheHeader(e.meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseCacheEntry(append(h, e.body...)); err != nil {
			t.Fatalf("re-encoded entry rejected: %v", err)
		}
	})
}

// FuzzJSONReader checks the streaming JSON reader against encoding/json:
// both accept the same documents (except where the reader is deliberately
// stricter: long whitespace runs, long numbers), and strings decode alike.
func FuzzJSONReader(f *testing.F) {
	for _, s := range []string{
		`{}`, `[]`, `{"a": [1, -2.5e+3, true, false, null, "x\"\\\/\b\f\n\r\té😀"]}`,
		`"\ud800"`, `"\ud800A"`, `"\udc00\ud800"`, `"😀"`, `"a\u0000b"`, "\"\xff\"",
		`[1,]`, `{"a" 1}`, `{"a":1,}`, `01`, `-`, `1.`, `.5`, `1e`, `tru`, `nul`, ` {} `, `{} x`,
		fullDoc, noarchDoc, `[[[[[[[[[[]]]]]]]]]]`, "{\"a\":\n\t\r 1}",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		j := newJSONReader(bytes.NewReader(data))
		err := j.skipValue()
		if err == nil {
			err = j.end()
		}
		valid := json.Valid(data)
		if err == nil && !valid {
			t.Fatalf("accepted invalid JSON %q", data)
		}
		if err != nil && valid && !errors.Is(err, errJSONWhitespace) && !strings.Contains(err.Error(), "number longer") {
			t.Fatalf("rejected valid JSON %q: %v", data, err)
		}
		if !valid || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '"' {
			return
		}
		var want string
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := newJSONReader(bytes.NewReader(data)).readString(1 << 20)
		if err != nil {
			t.Fatalf("readString(%q): %v", data, err)
		}
		if ok && got != want {
			t.Fatalf("readString(%q) = %q, encoding/json %q", data, got, want)
		}
		if !ok && utf8.Valid(data) && len(want) <= 1<<20 {
			t.Fatalf("readString(%q) refused valid UTF-8", data)
		}
	})
}
