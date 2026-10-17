package link

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// oracleBadSHA is the digest micromamba 2.2.0 wrote for every
// prefix-dependent file of the oracle environment (see checkOraclePackage).
const oracleBadSHA = "291beb4d1bc6c00cf1e98aceb4af509fbe7b344d91453bdcc86e915e1d296487"

// oracleDirs locates the micromamba-built comparison environment
// ($GOOMBA_REF_ROOT/envs/base-3.11) and its package cache
// ($GOOMBA_REF_ROOT/pkgs). Both are only ever read.
func oracleDirs(t *testing.T) (env, pkgs string) {
	t.Helper()
	root := os.Getenv("GOOMBA_REF_ROOT")
	if root == "" {
		t.Skip("set GOOMBA_REF_ROOT to a micromamba root holding envs/base-3.11 and pkgs/")
	}
	root = filepath.Clean(root)
	return filepath.Join(root, "envs", "base-3.11"), filepath.Join(root, "pkgs")
}

type oraclePackage struct {
	dist string
	meta *CondaMetaRecord
	env  string // the oracle environment
	dir  string // the extracted package in the oracle cache
}

// loadOracle returns the packages of the oracle environment in the order
// micromamba linked them (from conda-meta/history).
func loadOracle(t *testing.T) (pkgs []oraclePackage, py *PythonInfo, history []byte) {
	t.Helper()
	oracleEnv, oraclePkgs := oracleDirs(t)
	if _, err := os.Stat(oracleEnv); err != nil {
		t.Fatalf("oracle environment not available: %v", err)
	}
	recs, err := ReadCondaMeta(oracleEnv)
	if err != nil {
		t.Fatal(err)
	}
	byDist := map[string]*CondaMetaRecord{}
	for _, r := range recs {
		byDist[r.Record.Dist()] = r
		if r.Record.Name == "python" {
			if py, err = NewPythonInfo(r.Record, "linux-64"); err != nil {
				t.Fatal(err)
			}
		}
	}
	history, err = os.ReadFile(filepath.Join(oracleEnv, "conda-meta", "history"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(history), "\n") {
		if !strings.HasPrefix(line, "+") {
			continue
		}
		_, dist, ok := strings.Cut(line, "::")
		if !ok {
			t.Fatalf("bad history line %q", line)
		}
		m, ok := byDist[dist]
		if !ok {
			t.Fatalf("history names %s but conda-meta has no record", dist)
		}
		pkgs = append(pkgs, oraclePackage{dist: dist, meta: m, env: oracleEnv, dir: filepath.Join(oraclePkgs, dist)})
	}
	if len(pkgs) != 28 || len(recs) != 28 {
		t.Fatalf("expected 28 packages, history has %d and conda-meta %d", len(pkgs), len(recs))
	}
	if py == nil {
		t.Fatal("no python record")
	}
	return pkgs, py, history
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// nulTokens splits binary content at NUL bytes and drops empty tokens,
// replacing the install prefix with a marker: two binaries patched for
// different prefixes compare equal exactly when they differ only by the
// prefix and its NUL padding.
func nulTokens(data []byte, prefix string) [][]byte {
	var out [][]byte
	for _, tok := range bytes.Split(data, []byte{0}) {
		if len(tok) > 0 {
			out = append(out, bytes.ReplaceAll(tok, []byte(prefix), []byte("<PREFIX>")))
		}
	}
	return out
}

func TestOracleEnvironment(t *testing.T) {
	pkgs, py, history := loadOracle(t)
	ctx := context.Background()
	prefix := filepath.Join(t.TempDir(), "env")

	var installed []*Installed
	for _, p := range pkgs {
		rec := p.meta.Record
		opt := Options{
			Prefix:        prefix,
			Platform:      "linux-64",
			Python:        py,
			RequestedSpec: p.meta.RequestedSpec,
			CopyOnly:      true, // never create hard links to the oracle's cache
		}
		inst, err := Link(ctx, p.dir, rec, opt)
		if err != nil {
			t.Fatalf("Link %s: %v", p.dist, err)
		}
		if err := WriteCondaMeta(prefix, rec, inst, opt); err != nil {
			t.Fatalf("WriteCondaMeta %s: %v", p.dist, err)
		}
		installed = append(installed, inst)
	}

	for i, p := range pkgs {
		t.Run(p.dist, func(t *testing.T) {
			checkOraclePackage(t, p, installed[i], prefix)
		})
	}

	// ReadCondaMeta round-trips our records.
	ours, err := ReadCondaMeta(prefix)
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ := ReadCondaMeta(pkgs[0].env)
	if len(ours) != len(theirs) {
		t.Fatalf("ReadCondaMeta: %d records, want %d", len(ours), len(theirs))
	}
	for i := range ours {
		if !reflect.DeepEqual(ours[i].Record, theirs[i].Record) {
			t.Errorf("record %s differs:\n got %+v\nwant %+v", ours[i].Record.Dist(), ours[i].Record, theirs[i].Record)
		}
		if !reflect.DeepEqual(ours[i].Files, theirs[i].Files) {
			t.Errorf("files of %s differ", ours[i].Record.Dist())
		}
	}

	// The history entry matches micromamba's byte for byte.
	added := make([]*CondaMetaRecord, len(pkgs))
	for i, p := range pkgs {
		added[i] = p.meta
	}
	checkOracleHistory(t, prefix, history, added)

	// Unlinking everything leaves only conda-meta and its history.
	for i := len(pkgs) - 1; i >= 0; i-- {
		if err := Unlink(ctx, prefix, pkgs[i].meta.Record); err != nil {
			t.Fatalf("Unlink %s: %v", pkgs[i].dist, err)
		}
	}
	var left []string
	_ = filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != prefix {
			rel, _ := filepath.Rel(prefix, p)
			left = append(left, rel)
		}
		return nil
	})
	if want := []string{"conda-meta", "conda-meta/history"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("after unlinking everything the prefix holds %v, want %v", left, want)
	}
}

func checkOraclePackage(t *testing.T, p oraclePackage, inst *Installed, prefix string) {
	want := p.meta
	pkgDir := p.dir
	oracleEnv := p.env

	// The file list is micromamba's, in micromamba's order.
	if got := inst.Files(); !reflect.DeepEqual(got, want.Files) {
		t.Fatalf("files differ:\n got %d %q\nwant %d %q", len(got), head(got), len(want.Files), head(want.Files))
	}

	// conda-meta JSON: identical to micromamba's once the digests of files
	// whose content depends on the prefix are normalised.
	gotJSON, err := os.ReadFile(filepath.Join(prefix, "conda-meta", p.dist+".json"))
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := os.ReadFile(filepath.Join(oracleEnv, "conda-meta", p.dist+".json"))
	if err != nil {
		t.Fatal(err)
	}
	checkSameShape(t, gotJSON, wantJSON)
	patched := map[string]bool{}
	for i, w := range want.Paths {
		g := inst.Paths[i]
		if g.Path != w.Path || g.PathType != w.PathType || g.SHA256 != w.SHA256 || g.SizeInBytes != w.SizeInBytes || g.NoLink != w.NoLink {
			t.Errorf("paths_data[%d] = %+v, want %+v", i, g, w)
			continue
		}
		if g.SHA256InPrefix == w.SHA256InPrefix {
			continue
		}
		// micromamba 2.2.0 caches its first digest per thread
		// (validation::sha256sum's thread_local), so prefix-dependent
		// entries carry a constant bogus digest; ours must be the real one.
		if w.SHA256InPrefix != oracleBadSHA && w.SHA256 == w.SHA256InPrefix {
			t.Errorf("%s: sha256_in_prefix %s, want %s", g.Path, g.SHA256InPrefix, w.SHA256InPrefix)
			continue
		}
		patched[g.Path] = true
		resolved := filepath.Join(prefix, g.Path)
		data, err := os.ReadFile(resolved)
		switch {
		case err == nil && sha256Hex(data) != g.SHA256InPrefix:
			t.Errorf("%s: sha256_in_prefix %s does not match the file (%s)", g.Path, g.SHA256InPrefix, sha256Hex(data))
		case err != nil && g.SHA256InPrefix != emptySHA256:
			t.Errorf("%s: unreadable (%v) but sha256_in_prefix is %s", g.Path, err, g.SHA256InPrefix)
		}
	}
	norm := func(data []byte, root string) []byte {
		v, err := decodeJSONValue(data)
		if err != nil {
			t.Fatal(err)
		}
		obj := v.(map[string]any)
		for _, e := range obj["paths_data"].(map[string]any)["paths"].([]any) {
			m := e.(map[string]any)
			if patched[m["_path"].(string)] {
				m["sha256_in_prefix"] = "<prefix dependent>"
			}
		}
		out, err := marshalNlohmann(obj)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !bytes.Equal(norm(gotJSON, prefix), norm(wantJSON, oracleEnv)) {
		t.Errorf("conda-meta JSON differs from micromamba's")
	}
	// Byte-for-byte when no digest depends on the prefix.
	identical := bytes.Equal(gotJSON, wantJSON)
	if len(patched) == 0 && !identical {
		t.Errorf("conda-meta JSON is not byte-identical to micromamba's")
	}
	t.Logf("%d paths, %d with prefix-dependent digests, conda-meta byte-identical: %v", len(want.Paths), len(patched), identical)

	// Every file on disk.
	srcEntries, err := readPackagePaths(newRoot(pkgDir))
	if err != nil {
		t.Fatal(err)
	}
	placeholders := map[string]PathEntry{}
	for _, e := range srcEntries {
		if e.PrefixPlaceholder != "" {
			placeholders[e.Path] = e
		}
	}
	for i, w := range want.Paths {
		ours := filepath.Join(prefix, w.Path)
		theirs := filepath.Join(oracleEnv, w.Path)
		if w.PathType == PathPycFile {
			// Recorded, never compiled by Link.
			if _, err := os.Lstat(ours); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s: pyc file exists (%v)", w.Path, err)
			}
			continue
		}
		gfi, err := os.Lstat(ours)
		if err != nil {
			t.Errorf("%s: %v", w.Path, err)
			continue
		}
		wfi, err := os.Lstat(theirs)
		if err != nil {
			t.Errorf("oracle %s: %v", w.Path, err)
			continue
		}
		if gfi.Mode().Type() != wfi.Mode().Type() {
			t.Errorf("%s: type %v, want %v", w.Path, gfi.Mode().Type(), wfi.Mode().Type())
			continue
		}
		if gfi.Mode()&fs.ModeSymlink != 0 {
			gt, _ := os.Readlink(ours)
			wt, _ := os.Readlink(theirs)
			if gt != wt {
				t.Errorf("%s: symlink to %q, want %q", w.Path, gt, wt)
			}
			continue
		}
		got, err := os.ReadFile(ours)
		if err != nil {
			t.Error(err)
			continue
		}
		if w.PathType == PathUnixPythonEntryPoint {
			wantData, err := os.ReadFile(theirs)
			if err != nil {
				t.Fatal(err)
			}
			wantData = bytes.ReplaceAll(wantData, []byte(oracleEnv), []byte(prefix))
			if !bytes.Equal(got, wantData) {
				t.Errorf("%s: entry point differs:\n%s\nwant\n%s", w.Path, got, wantData)
			}
			if gfi.Mode().Perm() != entryPointMode {
				t.Errorf("%s: mode %v", w.Path, gfi.Mode())
			}
			continue
		}
		srcPath := inst.Paths[i].Path
		_ = srcPath
		e, hasPlaceholder := placeholderFor(placeholders, w.Path, inst, i, srcEntries)
		if !hasPlaceholder {
			if w.SHA256 != "" && sha256Hex(got) != w.SHA256 {
				t.Errorf("%s: content digest %s, want %s", w.Path, sha256Hex(got), w.SHA256)
			}
			continue
		}
		if bytes.Contains(got, []byte(e.PrefixPlaceholder)) {
			t.Errorf("%s: placeholder left in file", w.Path)
		}
		wantData, err := os.ReadFile(theirs)
		if err != nil {
			t.Fatal(err)
		}
		if e.FileMode == FileModeBinary {
			if len(got) != len(wantData) || uint64(len(got)) != e.SizeInBytes {
				t.Errorf("%s: binary length %d, want %d", w.Path, len(got), len(wantData))
			}
			if bytes.HasPrefix(got, []byte("\x7fELF")) {
				if f, err := elf.NewFile(bytes.NewReader(got)); err != nil {
					t.Errorf("%s: not ELF any more: %v", w.Path, err)
				} else {
					f.Close()
				}
			}
			if !reflect.DeepEqual(nulTokens(got, prefix), nulTokens(wantData, oracleEnv)) {
				t.Errorf("%s: binary differs beyond the prefix", w.Path)
			}
		} else {
			wantData = bytes.ReplaceAll(wantData, []byte(oracleEnv), []byte(prefix))
			if !bytes.Equal(got, wantData) {
				t.Errorf("%s: text differs after swapping the prefix", w.Path)
			}
		}
		if gfi.Mode().Perm() != wfi.Mode().Perm() {
			t.Errorf("%s: mode %v, want %v", w.Path, gfi.Mode().Perm(), wfi.Mode().Perm())
		}
	}
}

// placeholderFor finds the paths.json entry of the i-th installed path.
func placeholderFor(byPath map[string]PathEntry, dst string, inst *Installed, i int, src []PathEntry) (PathEntry, bool) {
	if i < len(src) {
		e := src[i]
		return e, e.PrefixPlaceholder != ""
	}
	e, ok := byPath[dst]
	return e, ok
}

func head(s []string) []string {
	if len(s) > 5 {
		return s[:5]
	}
	return s
}

// checkSameShape compares the keys and JSON types of two conda-meta
// documents, recursively through objects and the first array element.
func checkSameShape(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, g, w any)
	walk = func(path string, g, w any) {
		if reflect.TypeOf(g) != reflect.TypeOf(w) {
			t.Errorf("%s: type %T, want %T", path, g, w)
			return
		}
		switch w := w.(type) {
		case map[string]any:
			g := g.(map[string]any)
			for k := range w {
				if _, ok := g[k]; !ok {
					t.Errorf("%s: missing key %q", path, k)
				}
			}
			for k := range g {
				if _, ok := w[k]; !ok {
					t.Errorf("%s: extra key %q", path, k)
				}
			}
			for k := range w {
				if gv, ok := g[k]; ok {
					walk(path+"."+k, gv, w[k])
				}
			}
		case []any:
			g := g.([]any)
			if len(g) != len(w) {
				t.Errorf("%s: %d elements, want %d", path, len(g), len(w))
				return
			}
			for i := range w {
				walk(path+"[]", g[i], w[i])
			}
		}
	}
	walk("$", g, w)
}

func checkOracleHistory(t *testing.T, prefix string, want []byte, added []*CondaMetaRecord) {
	t.Helper()
	// "==> 2026-03-29 13:09:22 <=="
	line, _, _ := strings.Cut(string(want), "\n")
	stamp := strings.TrimSuffix(strings.TrimPrefix(line, "==> "), " <==")
	when, err := time.Parse(time.DateTime, stamp)
	if err != nil {
		t.Fatal(err)
	}
	var cmd string
	for _, l := range strings.Split(string(want), "\n") {
		if c, ok := strings.CutPrefix(l, "# cmd: "); ok {
			cmd = c
		}
	}
	e := HistoryEntry{Time: when, Cmd: cmd, Update: []string{"python=3.11"}}
	for _, a := range added {
		e.Added = append(e.Added, a.Record)
	}
	if err := AppendHistoryEntry(prefix, e); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "history"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("history differs:\n got %q\nwant %q", got, want)
	}
}

// TestOraclePrefixes links real packages of the oracle environment into a
// 200-character prefix and one with spaces, and checks that a prefix longer
// than the packages' placeholders fails cleanly.
func TestOraclePrefixes(t *testing.T) {
	pkgs, py, _ := loadOracle(t)
	byName := map[string]oraclePackage{}
	for _, p := range pkgs {
		byName[p.meta.Record.Name] = p
	}
	tmp := t.TempDir()
	long := tmp + "/"
	for len(long) < 200 {
		long += strings.Repeat("y", min(200-len(long)-1, 40)) + "/"
	}
	long = long[:199] + "z"
	ctx := context.Background()
	for _, prefix := range []string{long, filepath.Join(tmp, "with space", "env")} {
		t.Run(filepath.Base(prefix), func(t *testing.T) {
			for _, name := range []string{"ncurses", "python", "wheel", "pip"} {
				p := byName[name]
				inst, err := Link(ctx, p.dir, p.meta.Record, Options{Prefix: prefix, Platform: "linux-64", Python: py, CopyOnly: true})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				entries, err := readPackagePaths(newRoot(p.dir))
				if err != nil {
					t.Fatal(err)
				}
				for i, e := range entries {
					if e.PrefixPlaceholder == "" {
						continue
					}
					data, err := os.ReadFile(filepath.Join(prefix, inst.Paths[i].Path))
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(data, []byte(e.PrefixPlaceholder)) {
						t.Errorf("%s: placeholder left", inst.Paths[i].Path)
					}
					if e.FileMode == FileModeBinary {
						if uint64(len(data)) != e.SizeInBytes {
							t.Errorf("%s: length %d, want %d", inst.Paths[i].Path, len(data), e.SizeInBytes)
						}
						if bytes.HasPrefix(data, []byte("\x7fELF")) {
							f, err := elf.NewFile(bytes.NewReader(data))
							if err != nil {
								t.Errorf("%s: %v", inst.Paths[i].Path, err)
								continue
							}
							f.Close()
						}
						continue
					}
					if line, _, _ := bytes.Cut(data, []byte("\n")); bytes.HasPrefix(line, []byte("#!")) && len(line) > 127 {
						t.Errorf("%s: shebang longer than 127 bytes: %q", inst.Paths[i].Path, line)
					}
				}
			}
			wantShebang := "#!/bin/sh\n'''exec' \"" + prefix + "/bin/python3.11\" \"$0\" \"$@\" #'''\n"
			for _, ep := range []string{"bin/pip", "bin/pip3", "bin/wheel"} {
				data, err := os.ReadFile(filepath.Join(prefix, ep))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.HasPrefix(data, []byte(wantShebang)) {
					t.Errorf("%s: %q", ep, data)
				}
			}
			if prefix == long {
				// python3.11-config's shebang overflows and is rewritten.
				data, err := os.ReadFile(filepath.Join(prefix, "bin/python3.11-config"))
				if err != nil {
					t.Fatal(err)
				}
				if line, _, _ := bytes.Cut(data, []byte("\n")); string(line) != "#!/bin/sh" && !bytes.HasPrefix(line, []byte("#!/usr/bin/env ")) {
					t.Errorf("python3.11-config shebang %q", line)
				}
			}
		})
	}

	t.Run("longer than placeholder", func(t *testing.T) {
		prefix := tmp + "/" + strings.Repeat("w", 100) + "/" + strings.Repeat("w", 100) + "/" + strings.Repeat("w", 100)
		_, err := Link(ctx, byName["ncurses"].dir, byName["ncurses"].meta.Record, Options{Prefix: prefix, Platform: "linux-64", CopyOnly: true})
		if !errors.Is(err, ErrPrefixTooLong) {
			t.Fatalf("err = %v, want ErrPrefixTooLong", err)
		}
		if _, err := os.Lstat(prefix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("prefix created: %v", err)
		}
	})
}
