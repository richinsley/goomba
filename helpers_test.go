package goomba

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/richinsley/goomba/archive"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/virtual"
)

// tfile is one entry of a synthetic package.
type tfile struct {
	path        string
	data        string
	mode        fs.FileMode // 0 means 0644
	link        string      // symlink target
	dir         bool        // a "directory" path entry
	placeholder string      // prefix placeholder
	fileMode    string      // "text" or "binary"
}

// tspec describes a synthetic package.
type tspec struct {
	name, version, build string
	buildNumber          uint64
	subdir, noarch       string // subdir "" means the host platform
	depends, constrains  []string
	files                []tfile
	entryPoints          []string
	index                map[string]any    // extra info/index.json fields
	info                 map[string]string // info/ files to replace; "" removes one
}

// testPkg is a package archive the test channel serves.
type testPkg struct {
	rec  *record.PackageRecord // without URL and digests
	data []byte
	ext  string // ".conda" (the default) or ".tar.bz2"
}

// hostPlatform returns the conda platform of the test host, skipping the
// test when it has none.
func hostPlatform(t testing.TB) string {
	t.Helper()
	p := virtual.HostPlatform()
	if p == "" {
		t.Skip("host has no conda platform")
	}
	return p
}

// buildPkg builds a .conda archive for s.
func buildPkg(t testing.TB, s tspec) testPkg {
	t.Helper()
	if s.subdir == "" {
		s.subdir = hostPlatform(t)
	}
	dist := s.name + "-" + s.version + "-" + s.build
	index := map[string]any{
		"name": s.name, "version": s.version, "build": s.build, "build_number": s.buildNumber,
		"subdir": s.subdir, "depends": nonNilStrings(s.depends), "constrains": nonNilStrings(s.constrains),
		"license": "TEST", "timestamp": 1700000000123,
	}
	if s.noarch != "" {
		index["noarch"] = s.noarch
	}
	for k, v := range s.index {
		if v == nil {
			delete(index, k)
		} else {
			index[k] = v
		}
	}
	var paths []map[string]any
	var pkgTar bytes.Buffer
	tw := tar.NewWriter(&pkgTar)
	for _, f := range s.files {
		e := map[string]any{"_path": f.path}
		switch {
		case f.dir:
			e["path_type"] = "directory"
			must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: f.path + "/", Mode: 0o755}))
		case f.link != "":
			e["path_type"] = "softlink"
			must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: f.path, Linkname: f.link, Mode: 0o777}))
		default:
			mode := f.mode
			if mode == 0 {
				mode = 0o644
			}
			sum := sha256.Sum256([]byte(f.data))
			e["path_type"] = "hardlink"
			e["sha256"] = hex.EncodeToString(sum[:])
			e["size_in_bytes"] = len(f.data)
			if f.placeholder != "" {
				e["prefix_placeholder"] = f.placeholder
				e["file_mode"] = f.fileMode
			}
			must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.path, Mode: int64(mode), Size: int64(len(f.data))}))
			_, err := tw.Write([]byte(f.data))
			must(t, err)
		}
		paths = append(paths, e)
	}
	must(t, tw.Close())

	info := map[string][]byte{
		"info/index.json": mustJSON(t, index),
		"info/paths.json": mustJSON(t, map[string]any{"paths": nonNilMaps(paths), "paths_version": 1}),
	}
	if s.noarch != "" {
		lj := map[string]any{"type": s.noarch}
		if len(s.entryPoints) > 0 {
			lj["entry_points"] = s.entryPoints
		}
		info["info/link.json"] = mustJSON(t, map[string]any{"noarch": lj, "package_metadata_version": 1})
	}
	for n, data := range s.info {
		if data == "" {
			delete(info, n)
		} else {
			info[n] = []byte(data)
		}
	}
	var infoTar bytes.Buffer
	tw = tar.NewWriter(&infoTar)
	names := make([]string, 0, len(info))
	for n := range info {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: n, Mode: 0o644, Size: int64(len(info[n]))}))
		_, err := tw.Write(info[n])
		must(t, err)
	}
	must(t, tw.Close())

	enc, err := zstd.NewWriter(nil)
	must(t, err)
	defer enc.Close()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, m := range []struct {
		name string
		data []byte
	}{
		{"metadata.json", []byte(`{"conda_pkg_format_version": 2}`)},
		{"pkg-" + dist + ".tar.zst", enc.EncodeAll(pkgTar.Bytes(), nil)},
		{"info-" + dist + ".tar.zst", enc.EncodeAll(infoTar.Bytes(), nil)},
	} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Store, Modified: time.Unix(1700000000, 0)})
		must(t, err)
		_, err = w.Write(m.data)
		must(t, err)
	}
	must(t, zw.Close())
	return testPkg{
		rec: &record.PackageRecord{
			Name: s.name, Version: s.version, Build: s.build, BuildNumber: s.buildNumber,
			Subdir: s.subdir, Noarch: s.noarch, Depends: s.depends, Constrains: s.constrains,
		},
		data: out.Bytes(),
	}
}

// realPkg loads testdata/pkgs/<dist>.conda, taking its record from its own
// info/index.json.
func realPkg(t testing.TB, dist string) testPkg {
	return archivePkg(t, filepath.Join("testdata", "pkgs", dist+".conda"))
}

// archivePkg loads a package archive, taking its record from its own
// info/index.json.
func archivePkg(t testing.TB, file string) testPkg {
	t.Helper()
	data, err := os.ReadFile(file)
	must(t, err)
	dir := filepath.Join(t.TempDir(), "x")
	must(t, archive.Extract(context.Background(), file, dir, archive.Limits{}))
	raw, err := os.ReadFile(filepath.Join(dir, "info", "index.json"))
	must(t, err)
	var idx struct {
		Name, Version, Build, Subdir, Noarch string
		BuildNumber                          uint64 `json:"build_number"`
		Depends, Constrains                  []string
	}
	must(t, json.Unmarshal(raw, &idx))
	return testPkg{
		rec: &record.PackageRecord{
			Name: idx.Name, Version: idx.Version, Build: idx.Build, BuildNumber: idx.BuildNumber,
			Subdir: idx.Subdir, Noarch: idx.Noarch, Depends: idx.Depends, Constrains: idx.Constrains,
		},
		data: data,
		ext:  map[bool]string{true: ".tar.bz2", false: ".conda"}[strings.HasSuffix(file, ".tar.bz2")],
	}
}

// channel is a test conda channel served over TLS.
type channel struct {
	srv  *httptest.Server
	base string // channel URL

	mu          sync.Mutex
	files       map[string][]byte
	hits        map[string]int
	inflight    int
	maxInflight int
	delay       time.Duration
	onRequest   func(path string) // called before serving, outside mu
}

func newChannel(t testing.TB) *channel {
	t.Helper()
	c := &channel{files: map[string][]byte{}, hits: map[string]int{}}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	c.base = c.srv.URL + "/test-channel"
	return c
}

func (c *channel) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	data, ok := c.files[r.URL.Path]
	c.hits[r.URL.Path]++
	c.inflight++
	c.maxInflight = max(c.maxInflight, c.inflight)
	delay, hook := c.delay, c.onRequest
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inflight--
		c.mu.Unlock()
	}()
	if hook != nil {
		hook(r.URL.Path)
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(data)
}

// add serves p and returns its full record (URL, digests, size).
func (c *channel) add(p testPkg) *record.PackageRecord {
	r := *p.rec
	ext := p.ext
	if ext == "" {
		ext = ".conda"
	}
	r.Fn = r.Dist() + ext
	r.URL = c.base + "/" + r.Subdir + "/" + r.Fn
	r.Channel = c.base
	s := sha256.Sum256(p.data)
	m := md5.Sum(p.data)
	r.SHA256, r.MD5, r.Size = hex.EncodeToString(s[:]), hex.EncodeToString(m[:]), int64(len(p.data))
	c.mu.Lock()
	c.files["/test-channel/"+r.Subdir+"/"+r.Fn] = p.data
	c.mu.Unlock()
	return &r
}

// totalHits returns the number of requests served.
func (c *channel) totalHits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, h := range c.hits {
		n += h
	}
	return n
}

// lockfile serves pkgs and returns a validated lockfile for them.
func (c *channel) lockfile(t testing.TB, specs []string, pkgs ...testPkg) *lock.Lockfile {
	t.Helper()
	var recs []*record.PackageRecord
	for _, p := range pkgs {
		recs = append(recs, c.add(p))
	}
	lf, err := lock.FromRecords(hostPlatform(t), []string{c.base}, specs, map[string]string{}, recs)
	must(t, err)
	return lf
}

// options returns InstallOptions for the channel with a fresh package cache.
func (c *channel) options(t testing.TB) InstallOptions {
	return InstallOptions{
		PkgsDir:     filepath.Join(t.TempDir(), "pkgs"),
		Client:      c.srv.Client(),
		Concurrency: 4,
		CompilePyc:  true,
		Virtual:     testVirtual(t),
	}
}

// testVirtual returns a fixed set of virtual packages for the host
// platform: __glibc 2.28 on Linux plus the platform's defaults.
func testVirtual(t testing.TB) []*record.PackageRecord {
	recs, err := virtual.ApplyOverrides(hostPlatform(t), virtual.Defaults(hostPlatform(t)), nil)
	must(t, err)
	return recs
}

// fakePython builds a "python" package whose bin/python3.11 is a shell
// script standing in for the interpreter: it records its arguments,
// working directory, exported environment, the script it was given and its
// stdin under logDir, creates an empty .pyc for every source, and
// exits with exitCode.
func fakePython(t testing.TB, logDir string, exitCode int, depends ...string) testPkg {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake interpreter is a shell script")
	}
	script := fakePythonScript(logDir, exitCode)
	return buildPkg(t, tspec{
		name: "python", version: "3.11.9", build: "h0_cpython", depends: depends,
		files: []tfile{
			{path: "bin/python3.11", data: script, mode: 0o755},
			{path: "bin/python", link: "python3.11"},
			{path: "lib/python3.11/os.py", data: "# stdlib\n"},
		},
	})
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMaps(s []map[string]any) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	return s
}

// treeOf lists every path below root (slash-separated, sorted) with a
// short description: "d" for directories, "l <target>" for symlinks and
// "f <mode> <sha256>" for files.
func treeOf(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[rel] = "d"
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "l " + target
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[rel] = "f " + fi.Mode().Perm().String() + " " + hex.EncodeToString(sum[:8])
		}
		return nil
	})
	must(t, err)
	return out
}

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// readCondaMeta parses conda-meta/<dist>.json.
func readCondaMetaJSON(t testing.TB, prefix, dist string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(prefix, "conda-meta", dist+".json"))
	must(t, err)
	var m map[string]any
	must(t, json.Unmarshal(raw, &m))
	return m
}

// assertNoPrefix fails unless prefix is gone.
func assertNoPrefix(t testing.TB, prefix string) {
	t.Helper()
	if _, err := os.Lstat(prefix); err == nil {
		t.Errorf("prefix %s still exists: %v", prefix, sortedKeys(treeOf(t, prefix)))
	} else if !os.IsNotExist(err) {
		t.Error(err)
	}
}

// fakePythonScript is the shell script fakePython installs as its
// interpreter.
func fakePythonScript(logDir string, exitCode int) string {
	return `#!/bin/sh
log='` + logDir + `'
{
  echo "argc=$#"
  for a in "$@"; do echo "arg=$a"; done
  echo "cwd=$(pwd)"
} > "$log/info"
export -p > "$log/env"
for script in "$@"; do :; done
if [ -f "$script" ]; then /bin/cat "$script" > "$log/script"; fi
while IFS= read -r f; do
  [ -n "$f" ] || break
  echo "$f" >> "$log/stdin"
  d=${f%/*}; b=${f##*/}; b=${b%.py}
  /bin/mkdir -p "$d/__pycache__"
  : > "$d/__pycache__/$b.cpython-311.pyc"
done
exit ` + strconv.Itoa(exitCode) + `
`
}
