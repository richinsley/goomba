package lock

import (
	"io"
	"sort"
	"strconv"
)

// Bundle holds one lockfile per platform, for projects locked for several
// platforms at once. Its JSON form is
//
//	{
//	  "version": 1,
//	  "platforms": {
//	    "linux-64": {Lockfile},
//	    "osx-arm64": {Lockfile}
//	  }
//	}
//
// where each key equals its lockfile's "platform". A Bundle is a plain value
// with the same concurrency rules as Lockfile.
type Bundle struct {
	// Platforms maps a platform to its lockfile.
	Platforms map[string]*Lockfile
}

// wireBundle is the JSON form of a Bundle.
type wireBundle struct {
	Version   int                  `json:"version"`
	Platforms map[string]*Lockfile `json:"platforms"`
}

// ReadBundle parses and validates a bundle with the same strictness as
// Read. The bundle must hold at least one platform.
func ReadBundle(r io.Reader) (*Bundle, error) {
	data, err := readAllLimited(r)
	if err != nil {
		return nil, err
	}
	d, err := newStrictDecoder(data)
	if err != nil {
		return nil, err
	}
	b := &Bundle{}
	version := 0
	err = d.object("bundle", func(key string) error {
		switch key {
		case "version":
			return d.int("bundle.version", &version)
		case "platforms":
			b.Platforms = map[string]*Lockfile{}
			return d.object("bundle.platforms", func(platform string) error {
				l, err := decodeLockfile(d, "bundle.platforms["+strconv.Quote(platform)+"]")
				if err != nil {
					return err
				}
				b.Platforms[platform] = l
				return nil
			})
		}
		return invalidf("bundle: unknown key %q", key)
	})
	if err != nil {
		return nil, err
	}
	if err := d.end(); err != nil {
		return nil, err
	}
	if version != Version {
		return nil, invalidf("bundle: version %d (want %d): %w", version, Version, ErrUnsupportedVersion)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// Validate checks that the bundle holds at least one platform, that every
// key equals its lockfile's platform, and that every lockfile is valid.
func (b *Bundle) Validate() error {
	if b == nil || len(b.Platforms) == 0 {
		return invalidf("bundle: no platforms")
	}
	keys := make([]string, 0, len(b.Platforms))
	for k := range b.Platforms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		l := b.Platforms[k]
		if l == nil {
			return invalidf("bundle: platform %q: nil lockfile", k)
		}
		if l.Platform != k {
			return invalidf("bundle: platform %q holds a lockfile for %q", k, l.Platform)
		}
		if err := l.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Write validates b and writes it as canonical JSON (platforms sorted, each
// lockfile in Write's form, 2-space indent, trailing newline).
func (b *Bundle) Write(w io.Writer) error {
	if err := b.Validate(); err != nil {
		return err
	}
	wb := wireBundle{Version: Version, Platforms: make(map[string]*Lockfile, len(b.Platforms))}
	for k, l := range b.Platforms {
		wb.Platforms[k] = l.canonical()
	}
	return writeJSON(w, wb)
}
