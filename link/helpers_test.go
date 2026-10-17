package link

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// pkgFile is one file of a synthetic test package.
type pkgFile struct {
	path        string
	data        string
	mode        fs.FileMode // default 0644
	symlink     string      // creates a symlink with this target
	dir         bool        // creates an (empty) directory entry
	placeholder string
	fileMode    FileMode
	noLink      bool
	unlisted    bool   // present in the package but not in paths.json
	pathType    string // overrides the path_type written to paths.json
}

// pkgSpec describes a synthetic extracted package.
type pkgSpec struct {
	name, version, build string
	noarch               string
	files                []pkgFile
	entryPoints          []string
	legacy               bool              // write info/files + info/has_prefix instead of paths.json
	info                 map[string]string // extra info/ files
	index                map[string]any    // overrides for index.json
}

func (s pkgSpec) dist() string { return s.name + "-" + s.version + "-" + s.build }

// buildPkg creates the package below root and returns its directory and
// record.
func buildPkg(t *testing.T, root string, s pkgSpec) (string, *record.PackageRecord) {
	t.Helper()
	if s.version == "" {
		s.version = "1.0"
	}
	if s.build == "" {
		s.build = "h0_0"
	}
	dir := filepath.Join(root, s.dist())
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "info"), 0o755))
	type entry struct {
		Path        string `json:"_path"`
		PathType    string `json:"path_type"`
		FileMode    string `json:"file_mode,omitempty"`
		Placeholder string `json:"prefix_placeholder,omitempty"`
		SHA256      string `json:"sha256,omitempty"`
		Size        int    `json:"size_in_bytes,omitempty"`
		NoLink      bool   `json:"no_link,omitempty"`
	}
	entries := []entry{}
	var files, hasPrefix, noLink []string
	for _, f := range s.files {
		p := filepath.Join(dir, filepath.FromSlash(f.path))
		must(os.MkdirAll(filepath.Dir(p), 0o755))
		e := entry{Path: f.path, FileMode: string(f.fileMode), Placeholder: f.placeholder, NoLink: f.noLink}
		switch {
		case f.symlink != "":
			must(os.Symlink(f.symlink, p))
			e.PathType = "softlink"
		case f.dir:
			must(os.MkdirAll(p, 0o755))
			e.PathType = "directory"
		default:
			mode := f.mode
			if mode == 0 {
				mode = 0o644
			}
			must(os.WriteFile(p, []byte(f.data), mode))
			must(os.Chmod(p, mode))
			e.PathType = "hardlink"
			e.SHA256 = sha256Hex([]byte(f.data))
			e.Size = len(f.data)
		}
		if f.pathType != "" {
			e.PathType = f.pathType
		}
		if f.unlisted {
			continue
		}
		entries = append(entries, e)
		files = append(files, f.path)
		if f.placeholder != "" {
			hasPrefix = append(hasPrefix, f.placeholder+" "+string(f.fileMode)+" "+f.path)
		}
		if f.noLink {
			noLink = append(noLink, f.path)
		}
	}
	writeJSON := func(name string, v any) {
		data, err := json.MarshalIndent(v, "", "  ")
		must(err)
		must(os.WriteFile(filepath.Join(dir, "info", name), data, 0o644))
	}
	if s.legacy {
		must(os.WriteFile(filepath.Join(dir, "info", "files"), []byte(strings.Join(files, "\n")+"\n"), 0o644))
		if len(hasPrefix) > 0 {
			must(os.WriteFile(filepath.Join(dir, "info", "has_prefix"), []byte(strings.Join(hasPrefix, "\n")+"\n"), 0o644))
		}
		if len(noLink) > 0 {
			must(os.WriteFile(filepath.Join(dir, "info", "no_link"), []byte(strings.Join(noLink, "\n")+"\n"), 0o644))
		}
	} else {
		writeJSON("paths.json", map[string]any{"paths": entries, "paths_version": 1})
	}
	index := map[string]any{
		"name": s.name, "version": s.version, "build": s.build, "build_number": 0,
		"depends": []string{}, "license": "MIT", "subdir": "linux-64",
		"timestamp": 1700000000123, "arch": "x86_64", "platform": "linux",
	}
	if s.noarch != "" {
		index["noarch"] = s.noarch
		index["subdir"] = "noarch"
		index["arch"], index["platform"] = nil, nil
	}
	for k, v := range s.index {
		if v == nil {
			delete(index, k)
		} else {
			index[k] = v
		}
	}
	writeJSON("index.json", index)
	if s.noarch == "python" {
		lj := map[string]any{"noarch": map[string]any{"type": "python"}, "package_metadata_version": 1}
		if len(s.entryPoints) > 0 {
			lj["noarch"].(map[string]any)["entry_points"] = s.entryPoints
		}
		writeJSON("link.json", lj)
	}
	for name, data := range s.info {
		must(os.WriteFile(filepath.Join(dir, "info", name), []byte(data), 0o644))
	}
	subdir := "linux-64"
	if s.noarch != "" {
		subdir = "noarch"
	}
	rec := &record.PackageRecord{
		Name: s.name, Version: s.version, Build: s.build, Noarch: s.noarch, Subdir: subdir,
		Depends: []string{"python >=3.8"}, Timestamp: 1700000000000, License: "MIT",
		SHA256: strings.Repeat("ab", 32), MD5: strings.Repeat("cd", 16), Size: 1234,
		Fn: s.dist() + ".conda", Channel: "https://conda.anaconda.org/conda-forge",
		URL: "https://conda.anaconda.org/conda-forge/" + subdir + "/" + s.dist() + ".conda",
	}
	return dir, rec
}

// pythonInfo returns the PythonInfo of a Python 3.11 environment.
func pythonInfo() *PythonInfo {
	return &PythonInfo{
		Version:          "3.11",
		SitePackagesPath: "lib/python3.11/site-packages",
		BinDir:           "bin",
		Executable:       "bin/python3.11",
	}
}

// tree lists everything below dir as sorted slash paths, with "@target"
// for symlinks and "/" for directories. An empty top-level conda-meta is
// left out: Link creates it and keeps it even when it fails.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == dir {
				return filepath.SkipAll
			}
			return err
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "conda-meta" && d.IsDir() {
			if ents, err := os.ReadDir(p); err == nil && len(ents) == 0 {
				return nil
			}
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			rel += "@" + target
		case d.IsDir():
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func readString(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
