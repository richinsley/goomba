package goomba

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/richinsley/goomba/record"
)

// specServer serves conda channels over TLS for Lock and Create: each
// channel at /<name>, with package archives and a full or sharded index
// built from the packages added to it.
type specServer struct {
	srv *httptest.Server

	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
	// hook, if set, runs before each request is served, outside mu; it
	// may block (on the request's context).
	hook func(r *http.Request)
}

func newSpecServer(t testing.TB) *specServer {
	t.Helper()
	s := &specServer{files: map[string][]byte{}, hits: map[string]int{}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *specServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	hook := s.hook
	data, ok := s.files[r.URL.Path]
	s.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

func (s *specServer) put(path string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = data
}

func (s *specServer) setHook(h func(r *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = h
}

// requests counts the requests served so far.
func (s *specServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.hits {
		n += c
	}
	return n
}

func (s *specServer) client() *http.Client { return s.srv.Client() }

// specChannel is one channel of a specServer.
type specChannel struct {
	s    *specServer
	name string
	url  string
	// recs are the records the index lists, by subdir.
	recs map[string][]*record.PackageRecord
	// baseURL, if set, is the base_url of the platform subdir's index.
	baseURL string
}

func (s *specServer) channel(name string) *specChannel {
	return &specChannel{s: s, name: name, url: s.srv.URL + "/" + name, recs: map[string][]*record.PackageRecord{}}
}

// add serves the archive of p and lists it in the index; it returns the
// full record.
func (c *specChannel) add(t testing.TB, p testPkg) *record.PackageRecord {
	t.Helper()
	r := *p.rec
	ext := p.ext
	if ext == "" {
		ext = ".conda"
	}
	r.Fn = r.Dist() + ext
	sum, m := sha256.Sum256(p.data), md5.Sum(p.data)
	r.SHA256, r.MD5, r.Size = hex.EncodeToString(sum[:]), hex.EncodeToString(m[:]), int64(len(p.data))
	r.Timestamp = 1700000000123
	r.License = "TEST"
	c.s.put("/"+c.name+"/"+r.Subdir+"/"+r.Fn, p.data)
	c.recs[r.Subdir] = append(c.recs[r.Subdir], &r)
	return &r
}

// list lists a record in the index without an archive (for solve-only
// tests); its digest is of its file name.
func (c *specChannel) list(subdir, name, version, build string, depends ...string) *record.PackageRecord {
	r := &record.PackageRecord{Name: name, Version: version, Build: build, Subdir: subdir, Depends: depends}
	r.Fn = r.Dist() + ".conda"
	sum := sha256.Sum256([]byte(r.Fn))
	r.SHA256 = hex.EncodeToString(sum[:])
	c.recs[subdir] = append(c.recs[subdir], r)
	return r
}

// indexEntry is a record as repodata lists it.
func indexEntry(r *record.PackageRecord, binaryDigests bool) map[string]any {
	e := map[string]any{
		"name": r.Name, "version": r.Version, "build": r.Build, "build_number": r.BuildNumber,
		"depends": nonNilStrings(r.Depends), "constrains": nonNilStrings(r.Constrains), "subdir": r.Subdir,
	}
	if r.Noarch != "" {
		e["noarch"] = r.Noarch
	}
	if r.Size != 0 {
		e["size"] = r.Size
	}
	if r.Timestamp != 0 {
		e["timestamp"] = r.Timestamp
	}
	if r.License != "" {
		e["license"] = r.License
	}
	if binaryDigests {
		s, _ := hex.DecodeString(r.SHA256)
		e["sha256"] = s
		if r.MD5 != "" {
			m, _ := hex.DecodeString(r.MD5)
			e["md5"] = m
		}
	} else {
		e["sha256"] = r.SHA256
		if r.MD5 != "" {
			e["md5"] = r.MD5
		}
	}
	return e
}

// subdirs returns the platform's subdir and noarch.
func (c *specChannel) subdirs(platform string) []string { return []string{platform, "noarch"} }

// publishFull writes repodata.json for the platform and noarch.
func (c *specChannel) publishFull(t testing.TB, platform string) {
	t.Helper()
	for _, sd := range c.subdirs(platform) {
		pkgs := map[string]any{}
		for _, r := range c.recs[sd] {
			pkgs[r.Fn] = indexEntry(r, false)
		}
		info := map[string]any{"subdir": sd}
		if c.baseURL != "" && sd == platform {
			info["base_url"] = c.baseURL
		}
		c.s.put("/"+c.name+"/"+sd+"/repodata.json", mustJSON(t, map[string]any{
			"info": info, "packages": map[string]any{}, "packages.conda": pkgs, "repodata_version": 1,
		}))
	}
}

// publishSharded writes the CEP 16 shard index and one shard per package
// name for the platform and noarch.
func (c *specChannel) publishSharded(t testing.TB, platform string) {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	must(t, err)
	defer enc.Close()
	for _, sd := range c.subdirs(platform) {
		byName := map[string]map[string]any{}
		for _, r := range c.recs[sd] {
			if byName[r.Name] == nil {
				byName[r.Name] = map[string]any{}
			}
			byName[r.Name][r.Fn] = indexEntry(r, true)
		}
		shards := map[string]any{}
		for _, name := range sortedKeys(byName) {
			body := enc.EncodeAll(mpBytes(t, map[string]any{
				"packages": map[string]any{}, "packages.conda": byName[name], "removed": []string{},
			}), nil)
			sum := sha256.Sum256(body)
			shards[name] = sum[:]
			c.s.put("/"+c.name+"/"+sd+"/shards/"+hex.EncodeToString(sum[:])+".msgpack.zst", body)
		}
		info := map[string]any{"subdir": sd, "base_url": "", "shards_base_url": "./shards/"}
		if c.baseURL != "" && sd == platform {
			info["base_url"] = c.baseURL
		}
		c.s.put("/"+c.name+"/"+sd+"/repodata_shards.msgpack.zst", enc.EncodeAll(mpBytes(t, map[string]any{
			"version": 1, "info": info, "shards": shards,
		}), nil))
	}
}

// mpBytes encodes v as msgpack with sorted map keys.
func mpBytes(t testing.TB, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.SetSortMapKeys(true)
	enc.UseCompactInts(true)
	must(t, enc.Encode(v))
	return buf.Bytes()
}

// lockOptions returns LockOptions for channels of s with a fresh index
// cache and the test machine's virtual packages.
func (s *specServer) lockOptions(t testing.TB, specs []string, channels ...*specChannel) LockOptions {
	o := LockOptions{
		Specs:       specs,
		Platform:    hostPlatform(t),
		Virtual:     testVirtual(t),
		RepodataDir: t.TempDir(),
		Client:      s.client(),
	}
	for _, c := range channels {
		o.Channels = append(o.Channels, c.url)
	}
	return o
}

// eventLog records events, for tests.
type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) add(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) of(kinds ...EventKind) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if slices.Contains(kinds, e.Kind) {
			out = append(out, e)
		}
	}
	return out
}

// lockedNames lists "name version build" of a lockfile's packages.
func lockedNames(recs []*record.PackageRecord) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Name+" "+r.Version+" "+r.Build)
	}
	sort.Strings(out)
	return out
}

// ctxT returns a context cancelled when the test ends.
func ctxT(t testing.TB) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// joinLines joins with newlines, for messages.
func joinLines(s []string) string { return strings.Join(s, "\n") }
