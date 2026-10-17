// Ported from libmamba src/core/link.cpp (LinkPackage::execute, the
// conda-meta writer), src/core/package_fetcher.cpp (write_repodata_record)
// and src/specs/package_info.cpp (to_json, from_json) (BSD-3-Clause, see
// NOTICE).

package link

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/richinsley/goomba/record"
)

// defaultChannelAlias is micromamba's default channel_alias; channels below
// it are written to conda-meta by name ("conda-forge").
const defaultChannelAlias = "https://conda.anaconda.org/"

// maxCondaMetaSize bounds a conda-meta record read into memory.
const maxCondaMetaSize = 256 << 20

// CondaMetaRecord is one conda-meta/<dist>.json record of an installed
// package. It is a plain value, safe for concurrent reads.
type CondaMetaRecord struct {
	// Record is the package metadata. Timestamp is in milliseconds and
	// Channel is a channel URL, as everywhere in goomba.
	Record *record.PackageRecord
	// Files is the "files" list: prefix-relative paths.
	Files []string
	// Paths is paths_data.paths.
	Paths []PathRecord
	// RequestedSpec is the requested_spec field.
	RequestedSpec string
	// PackageDir is the extracted_package_dir field.
	PackageDir string
	// FileName is the record's file name inside conda-meta.
	FileName string
}

// WriteCondaMeta writes conda-meta/<name>-<version>-<build>.json in prefix
// for a package that Link installed. The JSON has the keys, value types and
// layout micromamba writes: the record's repodata fields (timestamp in
// seconds, track_features as a comma-separated string), the remaining
// fields of the package's info/index.json, then files, paths_data,
// requested_spec, extracted_package_dir, package_tarball_full_path and
// link. The file is written to a temporary name and renamed into place.
func WriteCondaMeta(prefix string, rec *record.PackageRecord, inst *Installed, opt Options) error {
	if rec == nil || inst == nil {
		return errors.New("link: write conda-meta: nil record or install result")
	}
	dist := rec.Dist()
	fail := func(err error) error { return fmt.Errorf("link: write conda-meta for %s: %w", dist, err) }
	if err := checkRecord(rec); err != nil {
		return fail(err)
	}
	prefix, err := checkPrefix(prefix)
	if err != nil {
		return fail(err)
	}
	obj, err := condaMetaObject(rec, inst, opt)
	if err != nil {
		return fail(err)
	}
	data, err := marshalNlohmann(obj)
	if err != nil {
		return fail(err)
	}
	dir, _, err := newRoot(prefix).resolve("conda-meta", resolveCreate, nil, nil)
	if err != nil {
		return fail(err)
	}
	if err := writeFileAtomic(dir, dist+".json", data); err != nil {
		return fail(err)
	}
	return nil
}

func condaMetaObject(rec *record.PackageRecord, inst *Installed, opt Options) (jsonObject, error) {
	obj := jsonObject{
		"name":           rec.Name,
		"version":        rec.Version,
		"channel":        channelName(rec),
		"url":            rec.URL,
		"subdir":         rec.Subdir,
		"fn":             rec.Fn,
		"size":           rec.Size,
		"timestamp":      rec.Timestamp / 1000,
		"build":          rec.Build,
		"build_string":   rec.Build,
		"build_number":   rec.BuildNumber,
		"license":        rec.License,
		"track_features": strings.Join(rec.TrackFeatures, ","),
		"depends":        nonNil(rec.Depends),
		"constrains":     nonNil(rec.Constrains),
	}
	if rec.Noarch != "" {
		obj["noarch"] = rec.Noarch
	}
	if rec.MD5 != "" {
		obj["md5"] = rec.MD5
	}
	if rec.SHA256 != "" {
		obj["sha256"] = rec.SHA256
	}
	if rec.PythonSitePackagesPath != "" {
		obj["python_site_packages_path"] = rec.PythonSitePackagesPath
	}
	// Like write_repodata_record: index.json only adds missing keys.
	keys := make([]string, 0, len(inst.Index))
	for k := range inst.Index {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if _, ok := obj[k]; ok {
			continue
		}
		v, err := decodeJSONValue(inst.Index[k])
		if err != nil {
			return nil, fmt.Errorf("index.json field %q: %w", k, err)
		}
		obj[k] = v
	}

	paths := make([]any, len(inst.Paths))
	for i, p := range inst.Paths {
		e := jsonObject{"_path": p.Path, "path_type": string(p.PathType)}
		if p.SHA256InPrefix != "" {
			e["sha256_in_prefix"] = p.SHA256InPrefix
		}
		if p.SHA256 != "" {
			e["sha256"] = p.SHA256
		}
		if p.NoLink {
			e["no_link"] = true
		}
		if p.SizeInBytes != 0 {
			e["size_in_bytes"] = p.SizeInBytes
		}
		paths[i] = e
	}
	obj["paths_data"] = jsonObject{"paths": paths, "paths_version": pathsJSONVersion}
	obj["files"] = inst.Files()
	obj["requested_spec"] = opt.RequestedSpec
	obj["package_tarball_full_path"] = inst.PackageDir + ".tar.bz2"
	obj["extracted_package_dir"] = inst.PackageDir
	obj["link"] = jsonObject{"source": inst.PackageDir, "type": 1}
	return obj, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// channelName returns the channel as micromamba writes it to conda-meta:
// the name for channels below the default channel alias, the URL otherwise.
func channelName(rec *record.PackageRecord) string {
	c := rec.Channel
	if c == "" {
		c = channelFromURL(rec.URL, rec.Subdir, rec.Fn)
	}
	c = strings.TrimSuffix(c, "/")
	if name, ok := strings.CutPrefix(c, defaultChannelAlias); ok && name != "" {
		return name
	}
	return c
}

// channelFromURL strips "/<subdir>/<fn>" from a package URL.
func channelFromURL(url, subdir, fn string) string {
	if url == "" || subdir == "" || fn == "" {
		return ""
	}
	base, ok := strings.CutSuffix(url, "/"+subdir+"/"+fn)
	if !ok {
		return ""
	}
	return base
}

// writeFileAtomic writes data to dir/name through a temporary file in dir.
func writeFileAtomic(dir, name string, data []byte) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+"."+hex.EncodeToString(rnd[:])+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(dir, name))
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// ReadCondaMeta reads every conda-meta/*.json record in prefix and returns
// them sorted by package name (then version, build and file name). A
// prefix without conda-meta has no records.
//
// ReadCondaMeta fails closed: a record that cannot be parsed, or whose file
// name is not <name>-<version>-<build>.json for the package it describes,
// makes the whole call fail with an error naming the file, because an
// environment whose metadata cannot be trusted should not be acted on.
// Remove or repair the file to proceed.
func ReadCondaMeta(prefix string) ([]*CondaMetaRecord, error) {
	prefix, err := checkPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("link: read conda-meta: %w", err)
	}
	dir, _, err := newRoot(prefix).resolve("conda-meta", resolveExisting, nil, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("link: read conda-meta: %w", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("link: read conda-meta: %w", err)
	}
	var out []*CondaMetaRecord
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") || !e.Type().IsRegular() {
			continue
		}
		r, err := readCondaMetaFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if want := r.Record.Dist() + ".json"; name != want {
			return nil, fmt.Errorf("link: read conda-meta %s: %w: the record describes %s", name, ErrInvalidPackage, want)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *CondaMetaRecord) int {
		for _, c := range [][2]string{
			{a.Record.Name, b.Record.Name},
			{a.Record.Version, b.Record.Version},
			{a.Record.Build, b.Record.Build},
			{a.FileName, b.FileName},
		} {
			if n := strings.Compare(c[0], c[1]); n != 0 {
				return n
			}
		}
		return 0
	})
	return out, nil
}

type rawPathRecord struct {
	Path           string  `json:"_path"`
	PathType       string  `json:"path_type"`
	SHA256         string  `json:"sha256"`
	SHA256InPrefix string  `json:"sha256_in_prefix"`
	SizeInBytes    *uint64 `json:"size_in_bytes"`
	NoLink         bool    `json:"no_link"`
}

type rawCondaMeta struct {
	Name                   string          `json:"name"`
	Version                string          `json:"version"`
	Build                  *string         `json:"build"`
	BuildString            string          `json:"build_string"`
	BuildNumber            json.Number     `json:"build_number"`
	Depends                []string        `json:"depends"`
	Constrains             []string        `json:"constrains"`
	TrackFeatures          json.RawMessage `json:"track_features"`
	Features               json.RawMessage `json:"features"`
	Noarch                 json.RawMessage `json:"noarch"`
	Subdir                 string          `json:"subdir"`
	Timestamp              json.Number     `json:"timestamp"`
	License                string          `json:"license"`
	MD5                    string          `json:"md5"`
	SHA256                 string          `json:"sha256"`
	Size                   json.Number     `json:"size"`
	PythonSitePackagesPath string          `json:"python_site_packages_path"`
	Fn                     string          `json:"fn"`
	URL                    string          `json:"url"`
	Channel                string          `json:"channel"`
	Files                  []string        `json:"files"`
	PathsData              *struct {
		Paths []rawPathRecord `json:"paths"`
	} `json:"paths_data"`
	RequestedSpec       string `json:"requested_spec"`
	ExtractedPackageDir string `json:"extracted_package_dir"`
}

func readCondaMetaFile(name string) (*CondaMetaRecord, error) {
	fail := func(err error) error { return fmt.Errorf("link: read conda-meta %s: %w", filepath.Base(name), err) }
	fi, err := os.Lstat(name)
	if err != nil {
		return nil, fail(err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxCondaMetaSize {
		return nil, fail(errors.New("not a regular file of acceptable size"))
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fail(err)
	}
	r, err := parseCondaMeta(data)
	if err != nil {
		return nil, fail(err)
	}
	r.FileName = filepath.Base(name)
	return r, nil
}

func parseCondaMeta(data []byte) (*CondaMetaRecord, error) {
	var raw rawCondaMeta
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	rec := &record.PackageRecord{
		Name:                   raw.Name,
		Version:                raw.Version,
		Build:                  raw.BuildString,
		Depends:                nilIfEmpty(raw.Depends),
		Constrains:             nilIfEmpty(raw.Constrains),
		Subdir:                 raw.Subdir,
		License:                raw.License,
		MD5:                    raw.MD5,
		SHA256:                 raw.SHA256,
		PythonSitePackagesPath: raw.PythonSitePackagesPath,
		Fn:                     raw.Fn,
		URL:                    raw.URL,
	}
	if raw.Build != nil {
		rec.Build = *raw.Build
	}
	var err error
	if rec.BuildNumber, err = parseUintNumber(raw.BuildNumber); err != nil {
		return nil, fmt.Errorf("build_number: %w", err)
	}
	ts, err := parseIntNumber(raw.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}
	rec.Timestamp = record.NormalizeTimestamp(ts)
	if rec.Size, err = parseIntNumber(raw.Size); err != nil {
		return nil, fmt.Errorf("size: %w", err)
	}
	if rec.TrackFeatures, err = parseTrackFeatures(raw.TrackFeatures); err != nil {
		return nil, fmt.Errorf("track_features: %w", err)
	}
	if len(raw.Features) > 0 && string(raw.Features) != "null" {
		if err := json.Unmarshal(raw.Features, &rec.Features); err != nil {
			return nil, fmt.Errorf("features: %w", err)
		}
	}
	if rec.Noarch, err = parseNoarch(raw.Noarch); err != nil {
		return nil, fmt.Errorf("noarch: %w", err)
	}
	rec.Channel = canonicalChannel(raw.Channel, raw.URL, raw.Subdir, raw.Fn)

	out := &CondaMetaRecord{
		Record:        rec,
		Files:         raw.Files,
		RequestedSpec: raw.RequestedSpec,
		PackageDir:    raw.ExtractedPackageDir,
	}
	if raw.PathsData != nil {
		for _, p := range raw.PathsData.Paths {
			pr := PathRecord{
				Path:           p.Path,
				PathType:       PathType(p.PathType),
				SHA256:         p.SHA256,
				SHA256InPrefix: p.SHA256InPrefix,
				NoLink:         p.NoLink,
			}
			if p.SizeInBytes != nil {
				pr.SizeInBytes = *p.SizeInBytes
			}
			out.Paths = append(out.Paths, pr)
		}
	}
	return out, nil
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func parseIntNumber(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		return i, nil
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || f != f || f > 1<<62 || f < -(1<<62) {
		return 0, fmt.Errorf("bad number %q", string(n))
	}
	return int64(f), nil
}

func parseUintNumber(n json.Number) (uint64, error) {
	i, err := parseIntNumber(n)
	if err != nil {
		return 0, err
	}
	if i < 0 {
		return 0, fmt.Errorf("negative number %q", string(n))
	}
	return uint64(i), nil
}

func parseTrackFeatures(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	var s string
	switch {
	case json.Unmarshal(raw, &s) == nil:
		list = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	case json.Unmarshal(raw, &list) == nil:
	default:
		return nil, fmt.Errorf("neither a string nor a list of strings")
	}
	var out []string
	for _, f := range list {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

func parseNoarch(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return "generic", nil
		}
		return "", nil
	}
	return "", fmt.Errorf("neither a string nor a boolean")
}

// canonicalChannel turns conda-meta's channel field into a channel URL:
// the package URL without "/<subdir>/<fn>" when that works, else the
// channel URL as written, else the channel name below the default alias.
func canonicalChannel(channel, url, subdir, fn string) string {
	if c := channelFromURL(url, subdir, fn); c != "" {
		return c
	}
	channel = strings.TrimSuffix(channel, "/")
	if channel == "" || strings.Contains(channel, "://") || channel == "@" {
		return channel
	}
	return defaultChannelAlias + channel
}
