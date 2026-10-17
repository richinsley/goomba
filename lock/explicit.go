// Ported from rattler crates/rattler_conda_types/src/explicit_environment_spec.rs
// (parsing), libmamba src/specs/package_info.cpp PackageInfo::from_url (hash
// fragments), micromamba src/env.cpp (export format) and libmamba
// src/core/prefix_data.cpp PrefixData::sorted_records (package order)
// (BSD-3-Clause, see NOTICE).

package lock

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/richinsley/goomba/record"
)

// ErrMissingExplicit reports an explicit file without the @EXPLICIT line,
// or with a package line before it. It is returned together with
// ErrInvalid.
var ErrMissingExplicit = errors.New("the @EXPLICIT tag is missing")

// Explicit is a parsed conda explicit environment file ("@EXPLICIT"
// format), as written by `conda list --explicit` or
// `micromamba env export --explicit`. An Explicit is a plain value with the
// same concurrency rules as Lockfile.
type Explicit struct {
	// Platform is the value of the "# platform: <p>" comment, or "".
	Platform string
	// Entries are the package lines in file order.
	Entries []ExplicitEntry
}

// ExplicitEntry is one package line of an explicit file. It is a plain
// comparable value.
type ExplicitEntry struct {
	// URL is the package URL without its "#hash" fragment.
	URL string
	// MD5 is the lowercase hex md5 from a "#<md5>" fragment, or "".
	MD5 string
	// SHA256 is the lowercase hex sha256 from a "#sha256:<hex>" or
	// "#<64 hex>" fragment, or "".
	SHA256 string
}

// ReadExplicit parses an explicit file. Lines starting with '#' are comments
// except "# platform: <p>", which must name a conda platform. Blank lines are
// skipped. Every other line must follow the @EXPLICIT line and be an
// absolute https, http or file package URL ending in .conda or .tar.bz2
// (no query or credentials), optionally followed by
// "#<32 hex md5>", "#<64 hex sha256>" or "#sha256:<64 hex>". Entries without
// a hash are accepted (rattler's test files contain them); Resolve takes
// their sha256 from the index. Errors wrap ErrInvalid. Input beyond 64 MiB
// is rejected.
func ReadExplicit(r io.Reader) (*Explicit, error) {
	data, err := readAllLimited(r)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, invalidf("explicit: input is not valid UTF-8")
	}
	e := &Explicit{}
	explicit := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxInput+1)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#"):
			rest := strings.TrimSpace(line[1:])
			p, ok := strings.CutPrefix(rest, "platform:")
			if !ok {
				continue
			}
			p = strings.TrimSpace(p)
			if !knownPlatform(p) {
				return nil, invalidf("explicit: line %d: platform %q is not a conda platform", n, p)
			}
			if e.Platform != "" && e.Platform != p {
				return nil, invalidf("explicit: line %d: platform %q conflicts with %q", n, p, e.Platform)
			}
			e.Platform = p
		case line == "@EXPLICIT":
			explicit = true
		case !explicit:
			return nil, fmt.Errorf("lock: explicit: line %d: %w: %w", n, ErrMissingExplicit, ErrInvalid)
		default:
			ent, err := parseExplicitEntry(line)
			if err != nil {
				return nil, invalidf("explicit: line %d: %q: %v", n, line, err)
			}
			if len(e.Entries) >= maxListItems {
				return nil, invalidf("explicit: more than %d entries", maxListItems)
			}
			e.Entries = append(e.Entries, ent)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, invalidf("explicit: %v", err)
	}
	if !explicit {
		return nil, fmt.Errorf("lock: explicit: %w: %w", ErrMissingExplicit, ErrInvalid)
	}
	return e, nil
}

// parseExplicitEntry parses one package line.
func parseExplicitEntry(line string) (ExplicitEntry, error) {
	if len(line) > maxURLLen+80 {
		return ExplicitEntry{}, errors.New("line too long")
	}
	for i := 0; i < len(line); i++ {
		if c := line[i]; c <= 0x20 || c >= 0x7f {
			return ExplicitEntry{}, fmt.Errorf("invalid character %q", c)
		}
	}
	raw, hash, hasHash := strings.Cut(line, "#")
	if strings.Contains(raw, "?") {
		return ExplicitEntry{}, errors.New("package URL has a query")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ExplicitEntry{}, fmt.Errorf("invalid url: %v", err)
	}
	switch {
	case u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "file",
		!strings.HasPrefix(raw, u.Scheme+":"), u.Opaque != "",
		u.Host == "" && u.Scheme != "file":
		return ExplicitEntry{}, errors.New("not an absolute https, http or file package URL")
	case u.User != nil:
		return ExplicitEntry{}, errors.New("package URL contains credentials")
	}
	ext := archiveExt(u.Path)
	if ext == "" || !strings.HasSuffix(raw, ext) {
		return ExplicitEntry{}, errors.New("package URL does not end in .conda or .tar.bz2")
	}
	// Like libmamba, require a <name>-<version>-<build> file name.
	if _, _, _, ok := splitDist(strings.TrimSuffix(path.Base(u.Path), ext)); !ok {
		return ExplicitEntry{}, errors.New("file name is not <name>-<version>-<build>")
	}
	ent := ExplicitEntry{URL: raw}
	if !hasHash {
		return ent, nil
	}
	// Hash rules from rattler's PackageArchiveHash::from_str: a "sha256:"
	// prefix or 64 characters means sha256, anything else must be an md5.
	if sha, ok := strings.CutPrefix(hash, "sha256:"); ok {
		if len(sha) != 64 || !isHex(sha) {
			return ExplicitEntry{}, errors.New("invalid sha256 hash")
		}
		ent.SHA256 = strings.ToLower(sha)
	} else if len(hash) == 64 {
		if !isHex(hash) {
			return ExplicitEntry{}, errors.New("invalid sha256 hash")
		}
		ent.SHA256 = strings.ToLower(hash)
	} else {
		if len(hash) != 32 || !isHex(hash) {
			return ExplicitEntry{}, errors.New("invalid md5 hash")
		}
		ent.MD5 = strings.ToLower(hash)
	}
	return ent, nil
}

// splitDist splits "name-version-build" at its last two dashes (names may
// contain dashes), reporting false if any part is empty.
func splitDist(stem string) (name, version, build string, ok bool) {
	i := strings.LastIndexByte(stem, '-')
	if i <= 0 {
		return "", "", "", false
	}
	j := strings.LastIndexByte(stem[:i], '-')
	if j <= 0 || j+1 == i || i+1 == len(stem) {
		return "", "", "", false
	}
	return stem[:j], stem[j+1 : i], stem[i+1:], true
}

// Resolve turns the entries into a lockfile by looking each URL up in an
// index. The index record supplies the metadata and, in particular, the
// sha256 the installer will verify; a hash given in the explicit file must
// equal the index's (ErrHashMismatch otherwise, also when the index lacks
// that hash). A URL the index does not know yields ErrNotInIndex. The
// platform is the file's "# platform:" or else the subdir of its non-noarch
// packages; channels are the package URLs' channels in order of first use.
// lookup's records are copied, never modified. The result is validated,
// so every URL must be https.
func (e *Explicit) Resolve(lookup func(url string) (*record.PackageRecord, bool)) (*Lockfile, error) {
	platform := e.Platform
	var channels []string
	seenChannel := map[string]bool{}
	recs := make([]*record.PackageRecord, 0, len(e.Entries))
	for _, ent := range e.Entries {
		rec, ok := lookup(ent.URL)
		if !ok || rec == nil {
			return nil, fmt.Errorf("lock: explicit: %q: %w", ent.URL, ErrNotInIndex)
		}
		r := *rec
		if r.URL == "" {
			r.URL = ent.URL
		} else if r.URL != ent.URL {
			return nil, invalidf("explicit: %q: index record has url %q", ent.URL, r.URL)
		}
		if ent.MD5 != "" && !strings.EqualFold(r.MD5, ent.MD5) {
			return nil, fmt.Errorf("lock: explicit: %q: md5 %s, index has %q: %w", ent.URL, ent.MD5, r.MD5, ErrHashMismatch)
		}
		if ent.SHA256 != "" && !strings.EqualFold(r.SHA256, ent.SHA256) {
			return nil, fmt.Errorf("lock: explicit: %q: sha256 %s, index has %q: %w", ent.URL, ent.SHA256, r.SHA256, ErrHashMismatch)
		}
		channel, subdir, _, err := splitArchiveURL(canonicalHost(ent.URL))
		if err != nil {
			return nil, invalidf("explicit: %q: %v", ent.URL, err)
		}
		if r.Subdir == "" {
			r.Subdir = subdir
		}
		if platform == "" && subdir != "noarch" {
			platform = subdir
		}
		if !seenChannel[channel] {
			seenChannel[channel] = true
			channels = append(channels, channel)
		}
		recs = append(recs, &r)
	}
	if platform == "" {
		return nil, invalidf("explicit: no platform: no \"# platform:\" line and no platform-specific package")
	}
	return FromRecords(platform, channels, nil, nil, recs)
}

// WriteExplicit validates l and writes it as an explicit file:
//
//	# platform: <platform>
//	@EXPLICIT
//	<url>#<md5>
//	...
//
// Packages without an md5 are written as "<url>#sha256:<sha256>". Packages
// appear in dependency order (each after the packages it depends on), using
// micromamba's `env export --explicit` algorithm so the output matches
// micromamba's line for line.
func (l *Lockfile) WriteExplicit(w io.Writer) error {
	if err := l.Validate(); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# platform: " + l.Platform + "\n@EXPLICIT\n")
	for _, p := range sortTopologically(l.Packages) {
		b.WriteString(p.URL)
		if p.MD5 != "" {
			b.WriteString("#" + p.MD5 + "\n")
		} else {
			b.WriteString("#sha256:" + p.SHA256 + "\n")
		}
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("lock: write explicit: %w", err)
	}
	return nil
}

// depName returns the package name of a match spec: the text after an
// optional "channel::" prefix, up to the first space or operator. It is
// enough to link depends entries to installed packages by name, which is
// all libmamba's sorted_records does.
func depName(spec string) string {
	s := strings.TrimSpace(spec)
	end := strings.IndexAny(s, " \t=<>!~[(,|;")
	if end < 0 {
		end = len(s)
	}
	if i := strings.LastIndex(s[:end], "::"); i >= 0 {
		s = s[i+2:]
		end -= i + 2
	}
	return s[:end]
}

// sortTopologically orders packages so that dependencies come first,
// porting libmamba's PrefixData::sorted_records: nodes are visited in name
// order and each is emitted after a depth-first visit of its installed
// dependencies (also in name order); cycles are broken where the search
// meets them, and python's dependency on pip is reversed so that python
// comes first. The input is not modified.
func sortTopologically(pkgs []Package) []Package {
	n := len(pkgs)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return pkgs[order[a]].Name < pkgs[order[b]].Name })
	node := make(map[string]int, n)
	for id, i := range order {
		node[pkgs[i].Name] = id
	}
	// preds[to] lists the installed dependencies of node to.
	preds := make([]map[int]bool, n)
	for to, i := range order {
		preds[to] = map[int]bool{}
		for _, d := range pkgs[i].Depends {
			if from, ok := node[depName(d)]; ok {
				preds[to][from] = true
			}
		}
	}
	if pip, ok := node["pip"]; ok {
		if python, ok := node["python"]; ok && preds[python][pip] {
			delete(preds[python], pip)
			preds[pip][python] = true
		}
	}
	adj := make([][]int, n)
	for id, set := range preds {
		for from := range set {
			adj[id] = append(adj[id], from)
		}
		sort.Ints(adj[id])
	}

	const (
		unvisited = iota
		ongoing
		done
	)
	status := make([]int, n)
	out := make([]Package, 0, n)
	type frame struct{ id, next int }
	for start := 0; start < n; start++ {
		if status[start] != unvisited {
			continue
		}
		status[start] = ongoing
		stack := []frame{{start, 0}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			if top.next < len(adj[top.id]) {
				child := adj[top.id][top.next]
				top.next++
				if status[child] == unvisited {
					status[child] = ongoing
					stack = append(stack, frame{child, 0})
				}
				continue
			}
			status[top.id] = done
			out = append(out, pkgs[order[top.id]])
			stack = stack[:len(stack)-1]
		}
	}
	return out
}
