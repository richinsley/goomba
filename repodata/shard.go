// Ported from libmamba src/core/shards.cpp (parse_shard_msgpack,
// parse_shard_package_record) and src/core/shard_index_loader.cpp
// (parse_shard_index_map) (BSD-3-Clause, see NOTICE), cross-checked with
// rattler crates/rattler_conda_types/src/repo_data/sharded.rs.

package repodata

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/richinsley/goomba/record"
)

// shardIndex is a decoded repodata_shards.msgpack.zst (CEP 16).
type shardIndex struct {
	baseURL       string // info.base_url: where packages live
	shardsBaseURL string // info.shards_base_url: where shards live
	subdir        string // info.subdir
	shards        map[string][32]byte
	folded        map[string][]string // lowercased name -> index keys, sorted
	charged       int64               // budget charged while decoding
}

// fold builds the case-folded name index.
func (idx *shardIndex) fold() {
	idx.folded = make(map[string][]string, len(idx.shards))
	for name := range idx.shards {
		k := strings.ToLower(name)
		idx.folded[k] = append(idx.folded[k], name)
	}
	for _, names := range idx.folded {
		sort.Strings(names)
	}
}

// newZstdDecoder returns a decoder whose DecodeAll refuses output larger
// than limit bytes. A *zstd.Decoder is safe for concurrent DecodeAll calls;
// concurrency bounds how many run at once.
func newZstdDecoder(limit uint64, concurrency int) (*zstd.Decoder, error) {
	return zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(concurrency),
		zstd.WithDecoderMaxMemory(limit),
		zstd.WithDecoderLowmem(true),
	)
}

// decompressZstd decodes one zstd payload with the decoder's size limit.
func decompressZstd(d *zstd.Decoder, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("zstd: empty input")
	}
	out, err := d.DecodeAll(data, nil)
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
			return nil, fmt.Errorf("zstd: %w: %v", ErrTooLarge, err)
		}
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	return out, nil
}

// skipEntry skips the rest of a key already read and its value.
func (r *mpReader) skipEntry(k mpValue) error {
	if err := r.skipRest(k); err != nil {
		return err
	}
	return r.skip()
}

// decodeShardIndex decodes the msgpack body of a shard index:
//
//	{version: 1, info: {base_url, shards_base_url, subdir, created_at},
//	 shards: {<name>: <32-byte sha256>}}
//
// It fails closed: the top level must be a map without repeated keys,
// "shards" must be present with at most maxIndexNames entries, every hash
// must be exactly 32 raw bytes and "version", when present, must be 1.
// Non-string info values are ignored (libmamba), as are unknown keys.
func decodeShardIndex(data []byte) (*shardIndex, error) {
	r := &mpReader{b: data}
	b := fixedBudget(len(data), mpBudgetBase, mpBudgetRatio)
	idx, err := decodeShardIndexBody(r, b)
	if err != nil {
		return nil, fmt.Errorf("repodata: shard index: %w", err)
	}
	idx.charged = b.used
	if r.remaining() != 0 {
		return nil, errors.New("repodata: shard index: trailing data after the top-level map")
	}
	idx.fold()
	return idx, nil
}

func decodeShardIndexBody(r *mpReader, b *budget) (*shardIndex, error) {
	top, err := r.next()
	if err != nil {
		return nil, err
	}
	if top.typ != mpMap {
		return nil, fmt.Errorf("expected a map, got %v", top.typ)
	}
	idx := &shardIndex{}
	seen := map[string]bool{}
	for i := 0; i < top.n; i++ {
		k, err := r.next()
		if err != nil {
			return nil, err
		}
		key, ok := k.str()
		if !ok {
			if err := r.skipEntry(k); err != nil {
				return nil, err
			}
			continue
		}
		switch key {
		case "version", "info", "shards":
			if seen[key] {
				return nil, fmt.Errorf("repeated key %q", key)
			}
			seen[key] = true
		}
		switch key {
		case "version":
			v, err := r.next()
			if err != nil {
				return nil, err
			}
			if v.typ == mpNil {
				continue
			}
			if n, ok := v.uint(); !ok || n != 1 {
				if err := r.skipRest(v); err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("unsupported version (want 1)")
			}
		case "info":
			if err := decodeShardIndexInfo(r, idx); err != nil {
				return nil, fmt.Errorf("info: %w", err)
			}
		case "shards":
			if idx.shards, err = decodeShardHashes(r, b); err != nil {
				return nil, fmt.Errorf("shards: %w", err)
			}
		default:
			if err := r.skip(); err != nil {
				return nil, err
			}
		}
	}
	if !seen["shards"] {
		return nil, errors.New(`missing "shards"`)
	}
	return idx, nil
}

func decodeShardIndexInfo(r *mpReader, idx *shardIndex) error {
	v, err := r.next()
	if err != nil {
		return err
	}
	if v.typ == mpNil {
		return nil
	}
	if v.typ != mpMap {
		_ = r.skipRest(v)
		return fmt.Errorf("expected a map, got %v", v.typ)
	}
	for i := 0; i < v.n; i++ {
		k, err := r.next()
		if err != nil {
			return err
		}
		key, keyOK := k.str()
		if !keyOK {
			if err := r.skipRest(k); err != nil {
				return err
			}
		}
		val, err := r.next()
		if err != nil {
			return err
		}
		if val.typ != mpStr || !keyOK {
			if err := r.skipRest(val); err != nil {
				return err
			}
			continue // libmamba ignores non-string info values
		}
		max := maxFieldBytes
		if key == "base_url" || key == "shards_base_url" {
			max = maxBaseURL
		}
		switch key {
		case "base_url", "shards_base_url", "subdir":
			if len(val.data) > max {
				return fmt.Errorf("%q: longer than %d bytes", key, max)
			}
		}
		switch key {
		case "base_url":
			idx.baseURL = string(val.data)
		case "shards_base_url":
			idx.shardsBaseURL = string(val.data)
		case "subdir":
			idx.subdir = string(val.data)
		}
	}
	return nil
}

// decodeShardHashes reads the name -> sha256 map. The map is not presized
// from the (untrusted) entry count.
func decodeShardHashes(r *mpReader, b *budget) (map[string][32]byte, error) {
	v, err := r.next()
	if err != nil {
		return nil, err
	}
	if v.typ != mpMap {
		_ = r.skipRest(v)
		return nil, fmt.Errorf("expected a map, got %v", v.typ)
	}
	out := make(map[string][32]byte)
	for i := 0; i < v.n; i++ {
		k, err := r.next()
		if err != nil {
			return nil, err
		}
		if k.typ != mpStr || len(k.data) > maxFieldBytes {
			if err := r.skipEntry(k); err != nil {
				return nil, err
			}
			continue // libmamba skips non-string names; no lookup can reach them
		}
		h, err := r.next()
		if err != nil {
			return nil, err
		}
		if h.typ != mpBin || len(h.data) != 32 {
			_ = r.skipRest(h)
			return nil, fmt.Errorf("%q: hash must be 32 bytes of bin, got %v of length %d", k.data, h.typ, len(h.data))
		}
		name := string(k.data)
		if _, dup := out[name]; !dup {
			if len(out) >= maxIndexNames {
				return nil, fmt.Errorf("more than %d names", maxIndexNames)
			}
			if err := b.charge(costIndexEntry + sized(len(name))); err != nil {
				return nil, err
			}
		}
		var sum [32]byte
		copy(sum[:], h.data)
		out[name] = sum
	}
	return out, nil
}

// shardContents is a decoded shard before records are placed in a subdir.
type shardContents struct {
	tarBz2, conda pendingSet
	removed       map[string]struct{}
	budget        *budget // what decoding charged; placing charges the URLs
}

// place locates the valid records of both sections.
func (sc *shardContents) place(loc *locator) ([]*record.PackageRecord, error) {
	recs, err := sc.tarBz2.place(loc, make([]*record.PackageRecord, 0, len(sc.tarBz2)+len(sc.conda)), sc.budget)
	if err != nil {
		return nil, err
	}
	return sc.conda.place(loc, recs, sc.budget)
}

// decodeShard decodes the msgpack body of one shard:
//
//	{packages: {<fn>: record}, "packages.conda": {<fn>: record}, removed: [<fn>]}
//
// Records that are malformed, lack both md5 and sha256, carry a name other
// than name (when name is non-empty), have a file name other than
// name-version-build plus their section's extension, or fail the
// python-minor prefilter are dropped. A malformed document, a repeated
// top-level key or decoded data far larger than the input is an error.
// Empty lists are nil, as in LoadRepodataJSON.
func decodeShard(data []byte, name string, py *pythonMinor, in interner) (*shardContents, error) {
	r := &mpReader{b: data}
	b := fixedBudget(len(data), mpBudgetBase, mpBudgetRatio)
	sc, err := decodeShardBody(r, name, py, in, b)
	if err != nil {
		return nil, fmt.Errorf("repodata: shard %q: %w", name, err)
	}
	sc.budget = b
	if r.remaining() != 0 {
		return nil, fmt.Errorf("repodata: shard %q: trailing data after the top-level map", name)
	}
	return sc, nil
}

func decodeShardBody(r *mpReader, name string, py *pythonMinor, in interner, b *budget) (*shardContents, error) {
	top, err := r.next()
	if err != nil {
		return nil, err
	}
	if top.typ != mpMap {
		return nil, fmt.Errorf("msgpack: expected a map, got %v", top.typ)
	}
	sc := &shardContents{tarBz2: pendingSet{}, conda: pendingSet{}, removed: map[string]struct{}{}}
	seen := map[string]bool{}
	for i := 0; i < top.n; i++ {
		k, err := r.next()
		if err != nil {
			return nil, err
		}
		key, ok := k.str()
		if !ok {
			if err := r.skipEntry(k); err != nil {
				return nil, err
			}
			continue
		}
		switch key {
		case "packages", "packages.conda", "removed":
			if seen[key] {
				return nil, fmt.Errorf("repeated key %q", key)
			}
			seen[key] = true
		}
		switch key {
		case "packages":
			err = decodeShardPackages(r, sc.tarBz2, false, name, py, in, b)
		case "packages.conda":
			err = decodeShardPackages(r, sc.conda, true, name, py, in, b)
		case "removed":
			err = decodeShardRemoved(r, sc.removed, b)
		default:
			err = r.skip()
		}
		if err != nil {
			return nil, fmt.Errorf("%q: %w", key, err)
		}
	}
	return sc, nil
}

// decodeShardRemoved streams the "removed" array into the set.
func decodeShardRemoved(r *mpReader, removed map[string]struct{}, b *budget) error {
	v, err := r.next()
	if err != nil {
		return err
	}
	if v.typ == mpNil {
		return nil
	}
	if v.typ != mpArray {
		_ = r.skipRest(v)
		return fmt.Errorf("expected an array, got %v", v.typ)
	}
	if v.n > maxRemoved {
		return fmt.Errorf("more than %d entries", maxRemoved)
	}
	for i := 0; i < v.n; i++ {
		e, err := r.next()
		if err != nil {
			return err
		}
		if e.typ != mpStr && e.typ != mpBin {
			_ = r.skipRest(e)
			return fmt.Errorf("element %d: expected a string, got %v", i, e.typ)
		}
		if len(e.data) > maxFieldBytes {
			continue // cannot name a valid file
		}
		if _, dup := removed[string(e.data)]; !dup {
			if err := b.charge(costSetEntry + sized(len(e.data))); err != nil {
				return err
			}
			removed[string(e.data)] = struct{}{}
		}
	}
	return nil
}

func decodeShardPackages(r *mpReader, set pendingSet, conda bool, name string, py *pythonMinor, in interner, b *budget) error {
	v, err := r.next()
	if err != nil {
		return err
	}
	if v.typ == mpNil {
		return nil
	}
	if v.typ != mpMap {
		_ = r.skipRest(v)
		return fmt.Errorf("expected a map, got %v", v.typ)
	}
	for i := 0; i < v.n; i++ {
		k, err := r.next()
		if err != nil {
			return err
		}
		fnOK := (k.typ == mpStr || k.typ == mpBin) && len(k.data) <= maxFieldBytes
		if !fnOK {
			if err := r.skipRest(k); err != nil {
				return err
			}
		}
		rec, ok, err := decodeShardRecord(r)
		if err != nil {
			return err
		}
		if !fnOK {
			continue
		}
		fn := string(k.data)
		if _, dup := set[fn]; !dup {
			if err := b.charge(costSetEntry + sized(len(fn))); err != nil {
				return err
			}
		}
		set[fn] = nil // last occurrence wins
		if ok && validFilename(fn, conda) && fn == rec.Dist()+ext(conda) &&
			(name == "" || rec.Name == name) && py.allows(rec) {
			if err := b.charge(in.keep(rec)); err != nil {
				return err
			}
			set[fn] = rec
		}
	}
	return nil
}

// decodeShardRecord decodes one package record map. ok is false for a
// record to drop; err is set only when the stream itself is broken.
//
// Unlike libmamba, which ignores a field it cannot parse and keeps the
// record, any present field of the wrong type or over a size cap drops the
// record: silently losing a dependency or a hash would not fail closed.
func decodeShardRecord(r *mpReader) (*record.PackageRecord, bool, error) {
	v, err := r.next()
	if err != nil {
		return nil, false, err
	}
	if v.typ != mpMap {
		return nil, false, r.skipRest(v)
	}
	rec := &record.PackageRecord{}
	ok := v.n <= maxRecordKeys
	for i := 0; i < v.n; i++ {
		k, err := r.next()
		if err != nil {
			return nil, false, err
		}
		key, keyOK := k.str()
		if !keyOK || !ok {
			if err := r.skipEntry(k); err != nil {
				return nil, false, err
			}
			continue // libmamba skips non-string keys
		}
		good, err := decodeShardField(r, key, rec)
		if err != nil {
			return nil, false, err
		}
		ok = ok && good
	}
	if !ok || (rec.MD5 == "" && rec.SHA256 == "") || !checkRecord(rec) {
		// Shards must carry a checksum (libmamba parse_shard_package_record).
		return nil, false, nil
	}
	return rec, true, nil
}

// decodeShardField decodes the value of one record field into rec. It
// reports false for a value of the wrong type or over a cap; the value is
// always consumed.
func decodeShardField(r *mpReader, key string, rec *record.PackageRecord) (bool, error) {
	switch key {
	case "depends", "constrains":
		list, err := r.stringList()
		if errors.Is(err, errBadField) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if key == "depends" {
			rec.Depends = list
		} else {
			rec.Constrains = list
		}
		return true, nil
	case "track_features":
		// A string ("a,b") or an array of strings.
		if r.remaining() > 0 && isMsgpackStr(r.b[r.off]) {
			v, err := r.next()
			if err != nil {
				return false, err
			}
			if len(v.data) > maxFieldBytes {
				return false, nil
			}
			rec.TrackFeatures = splitTrackFeatures(string(v.data))
			return true, nil
		}
		list, err := r.stringList()
		if errors.Is(err, errBadField) {
			return false, nil
		}
		rec.TrackFeatures = cleanTrackFeatures(list)
		return err == nil, err
	}

	v, err := r.next()
	if err != nil {
		return false, err
	}
	if v.typ == mpNil {
		clearField(key, rec) // nil means absent (also after an earlier value)
		return true, nil
	}
	str := func(dst *string) bool {
		if (v.typ != mpStr && v.typ != mpBin) || len(v.data) > maxFieldBytes {
			return false
		}
		*dst = string(v.data)
		return true
	}
	num := func() (uint64, bool) {
		n, ok := v.uint()
		return n, ok && n <= 1<<63-1
	}
	good := true
	switch key {
	case "name":
		good = str(&rec.Name)
	case "version":
		good = str(&rec.Version)
	case "build":
		good = str(&rec.Build)
	case "build_number":
		rec.BuildNumber, good = v.uint()
	case "size":
		var n uint64
		n, good = num()
		rec.Size = int64(n)
	case "timestamp":
		var n uint64
		n, good = num()
		rec.Timestamp = record.NormalizeTimestamp(int64(n))
	case "features":
		good = str(&rec.Features)
	case "license":
		good = str(&rec.License)
	case "python_site_packages_path":
		good = str(&rec.PythonSitePackagesPath) && validSitePackagesPath(rec.PythonSitePackagesPath)
	case "sha256":
		rec.SHA256, good, err = hashValue(r, v, 32)
		return good, err
	case "md5":
		rec.MD5, good, err = hashValue(r, v, 16)
		return good, err
	case "noarch":
		switch v.typ {
		case mpBool:
			// Legacy boolean (CEP 34 wants a string): true is "generic".
			if v.b {
				rec.Noarch = "generic"
			}
		case mpStr, mpBin:
			rec.Noarch, good = noarchString(string(v.data))
		default:
			good = false
		}
	default:
		// Unknown or unused (arch, platform, subdir, license_family,
		// legacy_bz2_*, run_exports, ...): skip.
	}
	return good, r.skipRest(v)
}

// isMsgpackStr reports whether a type byte starts a str or bin value.
func isMsgpackStr(c byte) bool {
	return c >= 0xa0 && c <= 0xbf || c >= 0xc4 && c <= 0xc6 || c >= 0xd9 && c <= 0xdb
}

// hashValue decodes a digest of n bytes given as a hex str, raw bin or ext
// payload, or an array of byte values (all accepted by libmamba). The whole
// value is consumed.
func hashValue(r *mpReader, v mpValue, n int) (string, bool, error) {
	switch v.typ {
	case mpStr:
		if len(v.data) != 2*n {
			return "", false, nil
		}
		h, ok := normalizeHex(string(v.data), n)
		return h, ok, nil
	case mpBin, mpExt:
		if len(v.data) != n {
			return "", false, nil
		}
		return hex.EncodeToString(v.data), true, nil
	case mpArray:
		if v.n != n {
			return "", false, r.skipRest(v)
		}
		buf := make([]byte, 0, n)
		ok := true
		for j := 0; j < v.n; j++ {
			e, err := r.next()
			if err != nil {
				return "", false, err
			}
			b, isUint := e.uint()
			if !isUint || b > 0xff {
				if err := r.skipRest(e); err != nil {
					return "", false, err
				}
				ok = false
				continue
			}
			buf = append(buf, byte(b))
		}
		if !ok {
			return "", false, nil
		}
		return hex.EncodeToString(buf), true, nil
	}
	return "", false, r.skipRest(v)
}

// nameFromFilename returns the package name of an archive file name
// "name-version-build.ext", lowercased, or "" if it has no such shape.
func nameFromFilename(fn string) string {
	stem, ok := strings.CutSuffix(fn, extConda)
	if !ok {
		if stem, ok = strings.CutSuffix(fn, extTarBz2); !ok {
			return ""
		}
	}
	i := strings.LastIndexByte(stem, '-')
	if i <= 0 {
		return ""
	}
	j := strings.LastIndexByte(stem[:i], '-')
	if j <= 0 || !validIdent(stem[:j]) {
		return ""
	}
	return strings.ToLower(stem[:j])
}
