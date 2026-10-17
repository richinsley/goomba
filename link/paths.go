// Ported from libmamba src/core/package_paths.cpp and
// include/mamba/core/package_paths.hpp (BSD-3-Clause, see NOTICE).

package link

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"
)

// PathType is the kind of a path in paths.json or in a conda-meta record.
type PathType string

// Path types. The first three appear in packages; the others only in
// conda-meta records written after installation. libmamba rejects
// PathDirectory entries at link time ("path type not implemented"); goomba
// creates the directory, as rattler does.
const (
	PathHardlink                      PathType = "hardlink"
	PathSoftlink                      PathType = "softlink"
	PathDirectory                     PathType = "directory"
	PathPycFile                       PathType = "pyc_file"
	PathUnixPythonEntryPoint          PathType = "unix_python_entry_point"
	PathWindowsPythonEntryPointScript PathType = "windows_python_entry_point_script"
	PathWindowsPythonEntryPointExe    PathType = "windows_python_entry_point_exe"
	PathLinkedPackageRecord           PathType = "linked_package_record"
)

const (
	// defaultPrefixPlaceholder is conda's legacy placeholder, split so that
	// a prefix replacement run over this source leaves it unchanged.
	defaultPrefixPlaceholder = "/opt/anaconda1anaconda2" + "anaconda3"
	pathsJSONVersion         = 1
	maxPathLength            = 4096
)

// FileMode tells how a file's prefix placeholder is replaced.
type FileMode string

// File modes. An empty FileMode behaves like FileModeText, as in libmamba.
const (
	FileModeText   FileMode = "text"
	FileModeBinary FileMode = "binary"
)

// PathEntry is one entry of a package's info/paths.json (or of the legacy
// info/files, info/has_prefix and info/no_link files). It is a plain value,
// safe for concurrent reads.
type PathEntry struct {
	// Path is the slash-separated path relative to the package root.
	Path string
	// PathType is PathHardlink, PathSoftlink or PathDirectory.
	PathType PathType
	// FileMode is the placeholder replacement mode; empty means text.
	FileMode FileMode
	// PrefixPlaceholder is the build prefix to replace, or "" for none.
	PrefixPlaceholder string
	// SHA256 is the lowercase or uppercase hex digest from paths.json, or "".
	SHA256 string
	// SizeInBytes is the size before prefix replacement, or 0 if unknown.
	SizeInBytes uint64
	// NoLink forces a copy instead of a hard link.
	NoLink bool
}

type rawPathEntry struct {
	Path              *string `json:"_path"`
	PathType          *string `json:"path_type"`
	FileMode          *string `json:"file_mode"`
	PrefixPlaceholder *string `json:"prefix_placeholder"`
	SHA256            *string `json:"sha256"`
	SizeInBytes       *uint64 `json:"size_in_bytes"`
	NoLink            *bool   `json:"no_link"`
}

type rawPathsJSON struct {
	Paths        *[]rawPathEntry `json:"paths"`
	PathsVersion *int64          `json:"paths_version"`
}

// ParsePathsJSON parses the contents of info/paths.json. It requires
// paths_version 1 and a valid relative path and known path_type for every
// entry. Like libmamba, a file_mode starting with 't' is text, one starting
// with 'b' is binary, and anything else is left empty (treated as text).
func ParsePathsJSON(data []byte) ([]PathEntry, error) {
	var raw rawPathsJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("link: parse paths.json: %w: %v", ErrInvalidPackage, err)
	}
	if raw.PathsVersion == nil || *raw.PathsVersion != pathsJSONVersion {
		return nil, fmt.Errorf("link: parse paths.json: %w: paths_version must be 1", ErrInvalidPackage)
	}
	if raw.Paths == nil {
		return nil, fmt.Errorf("link: parse paths.json: %w: missing paths", ErrInvalidPackage)
	}
	out := make([]PathEntry, 0, len(*raw.Paths))
	for i, r := range *raw.Paths {
		if r.Path == nil {
			return nil, fmt.Errorf("link: parse paths.json: %w: entry %d has no _path", ErrInvalidPackage, i)
		}
		e := PathEntry{Path: *r.Path}
		if err := validRelPath(e.Path); err != nil {
			return nil, fmt.Errorf("link: parse paths.json: entry %d: %w", i, err)
		}
		if r.PathType == nil {
			return nil, fmt.Errorf("link: parse paths.json: %w: %q has no path_type", ErrInvalidPackage, e.Path)
		}
		switch pt := PathType(*r.PathType); pt {
		case PathHardlink, PathSoftlink, PathDirectory:
			e.PathType = pt
		default:
			return nil, fmt.Errorf("link: parse paths.json: %w: %q has unknown path_type %q", ErrInvalidPackage, e.Path, *r.PathType)
		}
		if r.FileMode != nil {
			e.FileMode = parseFileMode(*r.FileMode)
		}
		if r.PrefixPlaceholder != nil {
			if strings.IndexByte(*r.PrefixPlaceholder, 0) >= 0 {
				return nil, fmt.Errorf("link: parse paths.json: %w: %q has a NUL byte in its prefix_placeholder", ErrInvalidPackage, e.Path)
			}
			e.PrefixPlaceholder = *r.PrefixPlaceholder
		}
		if r.SHA256 != nil {
			if *r.SHA256 != "" && !isHexDigest(*r.SHA256, 64) {
				return nil, fmt.Errorf("link: parse paths.json: %w: %q has malformed sha256", ErrInvalidPackage, e.Path)
			}
			e.SHA256 = *r.SHA256
		}
		if r.SizeInBytes != nil {
			e.SizeInBytes = *r.SizeInBytes
		}
		if r.NoLink != nil {
			e.NoLink = *r.NoLink
		}
		out = append(out, e)
	}
	return out, nil
}

// parseFileMode mirrors libmamba's read_paths: only the first character
// counts.
func parseFileMode(s string) FileMode {
	switch {
	case strings.HasPrefix(s, "t"):
		return FileModeText
	case strings.HasPrefix(s, "b"):
		return FileModeBinary
	}
	return ""
}

func isHexDigest(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// validRelPath checks that p is a clean, slash-separated relative path that
// cannot leave the directory it is joined to.
func validRelPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty path", ErrUnsafePath)
	case len(p) > maxPathLength:
		return fmt.Errorf("%w: path longer than %d bytes", ErrUnsafePath, maxPathLength)
	case !utf8.ValidString(p):
		return fmt.Errorf("%w: %q is not valid UTF-8", ErrUnsafePath, p)
	case p[0] == '/':
		return fmt.Errorf("%w: %q is absolute", ErrUnsafePath, p)
	case strings.ContainsRune(p, '\\'):
		return fmt.Errorf("%w: %q contains a backslash", ErrUnsafePath, p)
	case hasDriveLetter(p):
		return fmt.Errorf("%w: %q starts with a drive letter", ErrUnsafePath, p)
	}
	for i := 0; i < len(p); i++ {
		if isControl(p[i]) {
			return fmt.Errorf("%w: %q contains a control character", ErrUnsafePath, p)
		}
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("%w: %q is not a clean relative path", ErrUnsafePath, p)
		}
	}
	return nil
}

func hasDriveLetter(p string) bool {
	return len(p) >= 2 && p[1] == ':' && ('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z')
}

// isControl mirrors std::iscntrl in the C locale.
func isControl(c byte) bool { return c < 0x20 || c == 0x7f }

// readLines mirrors libmamba's read_lines: it drops a trailing "\r", strips
// ASCII whitespace and skips empty lines.
func readLines(data []byte) []string {
	var out []string
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		s := strings.Trim(string(line), " \t\n\v\f\r")
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

type prefixFile struct {
	placeholder string
	mode        FileMode
}

// parseHasPrefix parses the legacy info/has_prefix file: each line is either
// "path" (default placeholder, text mode) or "placeholder mode path". As in
// libmamba, fields are separated by single spaces, so quoted paths with
// spaces are not supported.
func parseHasPrefix(data []byte) (map[string]prefixFile, error) {
	res := make(map[string]prefixFile)
	for _, l := range readLines(data) {
		s := strings.Split(l, " ")
		switch len(s) {
		case 1:
			res[s[0]] = prefixFile{placeholder: defaultPrefixPlaceholder, mode: FileModeText}
		case 3:
			if strings.IndexByte(s[0], 0) >= 0 {
				return nil, fmt.Errorf("link: parse has_prefix line %q: %w: NUL byte in placeholder", l, ErrInvalidPackage)
			}
			mode := FileModeBinary
			if strings.HasPrefix(s[1], "t") {
				mode = FileModeText
			}
			res[s[2]] = prefixFile{placeholder: s[0], mode: mode}
		default:
			return nil, fmt.Errorf("link: parse has_prefix line %q: %w: expected 1 or 3 fields", l, ErrInvalidPackage)
		}
	}
	return res, nil
}

// maxMetadataSize bounds the package metadata files read into memory.
const maxMetadataSize = 256 << 20

// readPackagePaths reads info/paths.json from the package, falling back to
// the legacy info/files, info/has_prefix, info/no_link and info/no_softlink
// files as libmamba's read_paths does.
func readPackagePaths(pkg *root) ([]PathEntry, error) {
	data, err := pkg.readFile("info/paths.json", maxMetadataSize)
	if err == nil {
		return ParsePathsJSON(data)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("link: read paths.json: %w", err)
	}

	files, err := pkg.readFile("info/files", maxMetadataSize)
	if err != nil {
		return nil, fmt.Errorf("link: read info/files: %w", err)
	}
	hasPrefix := map[string]prefixFile{}
	if data, err := pkg.readFile("info/has_prefix", maxMetadataSize); err == nil {
		if hasPrefix, err = parseHasPrefix(data); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("link: read info/has_prefix: %w", err)
	}
	noLink := map[string]bool{}
	for _, name := range []string{"no_link", "no_softlink"} {
		data, err := pkg.readFile("info/"+name, maxMetadataSize)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("link: read info/%s: %w", name, err)
		}
		for _, l := range readLines(data) {
			noLink[l] = true
		}
	}

	var out []PathEntry
	for _, f := range readLines(files) {
		if err := validRelPath(f); err != nil {
			return nil, fmt.Errorf("link: parse info/files: %w", err)
		}
		e := PathEntry{Path: f, PathType: PathHardlink, NoLink: noLink[f]}
		if pf, ok := hasPrefix[f]; ok {
			e.PrefixPlaceholder = pf.placeholder
			e.FileMode = pf.mode
		}
		if fi, err := pkg.lstat(f); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			e.PathType = PathSoftlink
		}
		out = append(out, e)
	}
	return out, nil
}
