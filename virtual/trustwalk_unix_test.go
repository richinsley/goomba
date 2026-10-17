//go:build unix

package virtual

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// This file checks trustedUnixExecutable's path resolution against the
// kernel's. The walk resolves a path one component at a time with lstat and
// readlink, and runs the path it resolved, so it must land on the file the
// kernel would open: the test builds random trees of directories, files and
// symbolic links, resolves random paths in them both ways and compares.

const (
	// trustWalkSeed fixes the trees and the paths resolved in them, so the
	// test is deterministic. Tree i is generated from (trustWalkSeed, i).
	trustWalkSeed = 0x5c7a_570d
	// trustWalkTrees is the number of trees checked by default.
	// GOOMBA_TRUSTWALK_TREES sets another number.
	trustWalkTrees = 300
	// trustWalkShortTrees is the number of trees checked with -short.
	trustWalkShortTrees = 100
	// trustWalkQueries is the number of paths resolved in each tree.
	trustWalkQueries = 100
)

// kernelFS is the real file system with every entry presented as root's and
// not writable by group or others, so that trustedUnixExecutable's
// ownership checks always pass and only its path resolution is under test.
// With noPrecheck, stat reports every path as a regular file, which takes
// the walk's opening stat out of play and leaves the outcome to the walk.
type kernelFS struct {
	noPrecheck bool
	targets    []string // the link targets read, in order
}

func asRoots(fi fs.FileInfo) fileFacts {
	return fileFacts{mode: fi.Mode() &^ 0o022, modTime: fi.ModTime()}
}

func (k *kernelFS) stat(name string) (fileFacts, error) {
	if k.noPrecheck {
		return fileFacts{mode: 0o755}, nil
	}
	fi, err := os.Stat(name)
	if err != nil {
		return fileFacts{}, err
	}
	return asRoots(fi), nil
}

func (k *kernelFS) lstat(name string) (fileFacts, error) {
	fi, err := os.Lstat(name)
	if err != nil {
		return fileFacts{}, err
	}
	return asRoots(fi), nil
}

func (k *kernelFS) readlink(name string) (string, error) {
	target, err := os.Readlink(name)
	if err == nil {
		k.targets = append(k.targets, target)
	}
	return target, err
}

func (k *kernelFS) readDirNames(string, int) ([]string, error) {
	return nil, errors.New("kernelFS: readDirNames is not part of the walk")
}

// Kinds of tree entries.
const (
	twDir  = 'd'
	twFile = 'f'
	twFIFO = 'p'
	twLink = 'l'
)

// twChainPrefix starts the names of the links of long chains, and no name
// of the pool.
const twChainPrefix = "z"

// twNode is one entry of a generated tree.
type twNode struct {
	rel    string // slash-separated path below the tree root
	kind   byte   // twDir, twFile, twFIFO or twLink
	target string // of a link
}

// twTree is a generated tree and the absolute paths to resolve in it.
type twTree struct {
	root    string   // absolute, clean and without symbolic links
	nodes   []twNode // parents before their entries
	queries []string
}

// twGen generates a tree from a random source.
type twGen struct {
	r         *rand.Rand
	rootParts []string // components of the tree root
	names     []string // the names of entries
	nodes     []twNode
	dirs      []string // the directories, as paths below the root ("" is the root)
	taken     map[string]bool
	heads     []string // the first links of long chains
	// The tree as planned, which guides most queries: the kind of each
	// entry, the names in each directory (but the links of long chains),
	// and where each link was meant to lead, where that is an entry, a
	// name in a directory or the root. Mutated targets may lead elsewhere.
	kinds   map[string]byte
	kids    map[string][]string
	planned map[string]string
}

// genTrustWalkTree generates tree number seed, to be created at root.
//
// Entries take their names from a pool of ten, so that random paths often
// name entries that exist; the pool holds names that only look special
// ("...", "..x", ".x"). Link targets are relative or absolute (inside the
// tree), name entries, the root or missing names (dangling links), climb
// with ".." past their common ancestor and sometimes past the root and back
// down, and may hold "." components, "//", detours through other entries
// ("x/.." where x may be a link, a file or missing) and trailing "/" or
// "/.". Half the trees hold a chain of 38 to 42 links, around the 40-link
// limit, and a third a loop: a link to itself, two links to each other, or
// a link to an ancestor directory. Other links may lead into the chains, so
// longer walks occur too.
func genTrustWalkTree(seed uint64, root string) *twTree {
	g := &twGen{
		r:         rand.New(rand.NewPCG(trustWalkSeed, seed)),
		rootParts: splitUnixPath(root),
		names:     []string{"a", "b", "c", "d", "e", "f", "g", "...", "..x", ".x"},
		dirs:      []string{""},
		taken:     map[string]bool{"": true},
		planned:   map[string]string{},
	}
	for n := 2 + g.r.IntN(8); n > 0; n-- {
		if i, ok := g.newNode(twDir); ok {
			g.dirs = append(g.dirs, g.nodes[i].rel)
		}
	}
	for n := 1 + g.r.IntN(6); n > 0; n-- {
		kind := byte(twFile)
		if g.r.IntN(8) == 0 {
			kind = twFIFO
		}
		g.newNode(kind)
	}
	var links []int // links whose targets are drawn once every entry exists
	for n := 2 + g.r.IntN(12); n > 0; n-- {
		if i, ok := g.newNode(twLink); ok {
			links = append(links, i)
		}
	}
	if g.r.IntN(2) == 0 {
		g.chain()
	}
	if g.r.IntN(3) == 0 {
		g.loop()
	}
	for _, i := range links {
		g.nodes[i].target = g.target(g.nodes[i].rel)
	}
	g.kinds, g.kids = map[string]byte{}, map[string][]string{}
	for _, n := range g.nodes {
		g.kinds[n.rel] = n.kind
		if dir, name := parentRel(n.rel), path.Base(n.rel); !strings.HasPrefix(name, twChainPrefix) {
			g.kids[dir] = append(g.kids[dir], name)
		}
	}
	t := &twTree{root: root, nodes: g.nodes}
	for range trustWalkQueries {
		t.queries = append(t.queries, g.query())
	}
	return t
}

// newNode adds an entry of kind under a random directory and returns its
// index, unless the names tried are taken.
func (g *twGen) newNode(kind byte) (int, bool) {
	dir := g.dirs[g.r.IntN(len(g.dirs))]
	for range 4 {
		rel := joinRel(dir, g.name())
		if !g.taken[rel] {
			g.taken[rel] = true
			g.nodes = append(g.nodes, twNode{rel: rel, kind: kind})
			return len(g.nodes) - 1, true
		}
	}
	return 0, false
}

func (g *twGen) name() string { return g.names[g.r.IntN(len(g.names))] }

// comps returns n random path components: names, "..", "." and "".
func (g *twGen) comps(n int) []string {
	out := make([]string, n)
	for i := range out {
		switch k := g.r.IntN(20); {
		case k < 14:
			out[i] = g.name()
		case k < 17:
			out[i] = ".."
		case k < 19:
			out[i] = "."
		default:
			out[i] = ""
		}
	}
	return out
}

// entry returns the components of a random entry, or of the root.
func (g *twGen) entry() []string {
	if i := g.r.IntN(len(g.nodes) + 1); i < len(g.nodes) {
		return strings.Split(g.nodes[i].rel, "/")
	}
	return nil
}

// target returns a random target for the link rel.
func (g *twGen) target(rel string) string {
	switch k := g.r.IntN(10); {
	case k < 6: // an entry or the root
		return g.linkTo(rel, g.entry())
	case k < 9: // a name in a directory, which may not exist
		return g.linkTo(rel, append(splitUnixPath(g.dirs[g.r.IntN(len(g.dirs))]), g.name()))
	}
	return g.render(g.mutate(g.comps(1+g.r.IntN(4)), false), false)
}

// linkTo returns a target, absolute or relative and mutated, for the link
// rel that leads to dest, and records the plan.
func (g *twGen) linkTo(rel string, dest []string) string {
	g.planned[rel] = strings.Join(dest, "/")
	if g.r.IntN(3) == 0 {
		return g.absolute(dest)
	}
	return g.render(g.mutate(g.relComps(parentRel(rel), dest), false), false)
}

// absolute returns the absolute path of comps, components below the root,
// mutated; the part that names the root sometimes gets a "." or empty
// component but nothing that could lead out of it.
func (g *twGen) absolute(comps []string) string {
	parts := append(slices.Clone(g.rootParts), g.mutate(comps, true)...)
	if g.r.IntN(10) == 0 {
		parts = slices.Insert(parts, g.r.IntN(len(g.rootParts)+1), []string{".", ""}[g.r.IntN(2)])
	}
	return "/" + strings.Join(parts, "/")
}

// relComps returns a relative route from the directory from to dest (both
// below the root): up past their common ancestor, sometimes higher, even
// above the root, and back down.
func (g *twGen) relComps(from string, dest []string) []string {
	f := splitUnixPath(from)
	c := 0
	for c < len(f) && c < len(dest) && f[c] == dest[c] {
		c++
	}
	up := len(f) - c
	if g.r.IntN(4) == 0 {
		up += 1 + g.r.IntN(c+2)
	}
	var out []string
	for range up {
		out = append(out, "..")
	}
	if over := up - len(f); over > 0 { // above the root: come back down to it
		out = append(out, g.rootParts[len(g.rootParts)-over:]...)
		out = append(out, dest...)
	} else {
		out = append(out, dest[len(f)-up:]...)
	}
	if len(out) == 0 {
		out = []string{"."}
	}
	return out
}

// mutate inserts "." and empty components (which render as "//"), a detour
// through a random name and back, and a trailing "/" or "/.", each
// sometimes. A relative path keeps a non-empty first component; abs marks
// components that follow the root.
func (g *twGen) mutate(comps []string, abs bool) []string {
	first := 1
	if abs {
		first = 0
	}
	insert := func(c ...string) {
		at := first + g.r.IntN(len(comps)-first+1)
		comps = slices.Insert(comps, at, c...)
	}
	if g.r.IntN(4) == 0 {
		insert(".")
	}
	if g.r.IntN(4) == 0 {
		insert("")
	}
	if g.r.IntN(6) == 0 {
		insert(g.name(), "..")
	}
	switch g.r.IntN(12) {
	case 0:
		comps = append(comps, "")
	case 1:
		comps = append(comps, ".")
	}
	return comps
}

// render joins components into a target. A relative one never starts with
// "/" (from an empty first component): every absolute target names the
// tree root.
func (g *twGen) render(comps []string, abs bool) string {
	s := strings.Join(comps, "/")
	switch {
	case abs:
		return "/" + s
	case s == "" || s[0] == '/':
		return "." + s
	}
	return s
}

// chain adds a chain of links around the kernel's limit, spread over the
// directories, ending at a file or directory (never another link) so that
// resolving the first link follows exactly as many links as the chain has.
func (g *twGen) chain() {
	n := maxLinkHops - 2 + g.r.IntN(5)
	id := len(g.heads)
	first := len(g.nodes)
	for k := range n {
		dir := g.dirs[g.r.IntN(len(g.dirs))]
		name := fmt.Sprintf("%s%d.%d", twChainPrefix, id, k)
		g.nodes = append(g.nodes, twNode{rel: joinRel(dir, name), kind: twLink})
	}
	var ends []string
	for _, nd := range g.nodes[:first] {
		if nd.kind == twFile || (nd.kind == twDir && g.r.IntN(4) == 0) {
			ends = append(ends, nd.rel)
		}
	}
	end := ""
	if len(ends) > 0 {
		end = ends[g.r.IntN(len(ends))]
	}
	for k := range n {
		next := end
		if k < n-1 {
			next = g.nodes[first+k+1].rel
		}
		from := parentRel(g.nodes[first+k].rel)
		g.planned[g.nodes[first+k].rel] = next
		abs := g.r.IntN(3) == 0
		var comps []string
		if abs {
			comps = append(slices.Clone(g.rootParts), splitUnixPath(next)...)
		} else {
			comps = g.relComps(from, splitUnixPath(next))
		}
		if len(comps) > 1 && g.r.IntN(4) == 0 {
			// "." or "//" before the last component: no link, no detour
			at := 1 + g.r.IntN(len(comps)-1)
			comps = slices.Insert(comps, at, []string{".", ""}[g.r.IntN(2)])
		}
		g.nodes[first+k].target = g.render(comps, abs)
	}
	g.heads = append(g.heads, g.nodes[first].rel)
}

// loop adds a link to itself, two links to each other, or a link to an
// ancestor of its directory (an endless tree of directories).
func (g *twGen) loop() {
	i, ok := g.newNode(twLink)
	if !ok {
		return
	}
	rel := g.nodes[i].rel
	dest := splitUnixPath(rel) // itself
	switch g.r.IntN(3) {
	case 1: // a second link, which leads back to the first
		if j, ok := g.newNode(twLink); ok {
			g.nodes[j].target = g.linkTo(g.nodes[j].rel, dest)
			dest = splitUnixPath(g.nodes[j].rel)
		}
	case 2:
		f := splitUnixPath(parentRel(rel))
		dest = f[:g.r.IntN(len(f)+1)]
	}
	g.nodes[i].target = g.linkTo(rel, dest)
}

// follow returns where the entry rel leads as planned: rel itself, unless
// it is a link whose plan is known, which is followed in turn.
func (g *twGen) follow(rel string) (string, bool) {
	for range 2 * maxLinkHops {
		if g.kinds[rel] != twLink {
			return rel, true
		}
		dest, ok := g.planned[rel]
		if !ok {
			return "", false
		}
		rel = dest
	}
	return "", false
}

// walk returns the components of a random walk from the root through the
// tree as planned: mostly a name in the directory reached, following links
// to where they were meant to lead, and sometimes "..", ".", "" or a random
// name. It stops at anything but a directory.
func (g *twGen) walk() []string {
	var out []string
	cur := ""
	for n := 1 + g.r.IntN(8); n > 0; n-- {
		switch k := g.r.IntN(20); {
		case k < 15:
			kids := g.kids[cur]
			if len(kids) == 0 {
				return append(out, g.name())
			}
			name := kids[g.r.IntN(len(kids))]
			out = append(out, name)
			next, ok := g.follow(joinRel(cur, name))
			if !ok || (next != "" && g.kinds[next] != twDir) {
				return out
			}
			cur = next
		case k < 17:
			out = append(out, "..")
			if cur == "" { // above the root, and back down
				out = append(out, g.rootParts[len(g.rootParts)-1])
			} else {
				cur = parentRel(cur)
			}
		case k < 19:
			out = append(out, []string{".", ""}[g.r.IntN(2)])
		default:
			return append(out, g.name())
		}
	}
	return out
}

// query returns a random absolute path into the tree: a walk through the
// tree as planned, an entry followed by a few random components, random
// components alone, or the head of a long chain, mutated as link targets
// are; sometimes with "//" or "/./" in the part that names the root.
func (g *twGen) query() string {
	var comps []string
	switch k := g.r.IntN(40); {
	case k < 2 && len(g.heads) > 0:
		comps = strings.Split(g.heads[g.r.IntN(len(g.heads))], "/")
		if g.r.IntN(2) == 0 {
			comps = append(comps, g.comps(1)...)
		}
	case k < 7:
		comps = append(g.entry(), g.comps(g.r.IntN(3))...)
	case k < 12:
		comps = g.comps(1 + g.r.IntN(6))
	default:
		comps = g.walk()
	}
	return g.absolute(comps)
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func parentRel(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// create makes the tree. Every entry is made in a directory made before it,
// so nothing is written through a link, and files are made exclusively.
func (t *twTree) create() error {
	if err := os.MkdirAll(t.root, 0o755); err != nil {
		return err
	}
	for _, n := range t.nodes {
		p := t.root + "/" + n.rel
		var err error
		switch n.kind {
		case twDir:
			err = os.Mkdir(p, 0o755)
		case twFile:
			var f *os.File
			if f, err = os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
				err = f.Close()
			}
		case twFIFO:
			err = unix.Mkfifo(p, 0o644)
		case twLink:
			err = os.Symlink(n.target, p)
		}
		if err != nil {
			return fmt.Errorf("create %c %s: %w", n.kind, n.rel, err)
		}
	}
	return nil
}

func (t *twTree) String() string {
	nodes := slices.Clone(t.nodes)
	slices.SortFunc(nodes, func(a, b twNode) int { return strings.Compare(a.rel, b.rel) })
	var b strings.Builder
	fmt.Fprintf(&b, "tree at %s:", t.root)
	for _, n := range nodes {
		fmt.Fprintf(&b, "\n\t%c %s", n.kind, n.rel)
		if n.kind == twLink {
			fmt.Fprintf(&b, " -> %s", n.target)
		}
	}
	return b.String()
}

// twStats counts what the queries exercised.
type twStats struct {
	trees, queries, mismatches int
	// The kernel's verdicts: a regular file, a directory, something else,
	// or an error.
	regular, dirs, others, enoent, enotdir, eloop, otherErrs int
	// Accepted walks: through a link, an absolute target, a target with a
	// ".." component, a target with a trailing slash; the most links
	// followed, and how many followed exactly maxLinkHops.
	viaLink, viaAbs, viaDotDot, viaTrailing, maxHops, atLimit int
	// Paths the walk alone (without its opening stat) accepts and the
	// kernel refuses because a file is followed by "/" or "/.".
	aloneOnly int
	// Paths both refuse at link maxLinkHops+1, after maxLinkHops different
	// links (not a loop), which the walk refuses without reading.
	pastLimit int
}

func (s *twStats) String() string {
	return fmt.Sprintf("%d trees, %d queries, %d mismatches; kernel: %d regular files, "+
		"%d directories, %d other files, %d ENOENT, %d ENOTDIR, %d ELOOP, %d other errors; "+
		"accepted walks: %d through links, %d through absolute targets, %d through targets with \"..\", "+
		"%d through targets with a trailing slash, %d with exactly %d links (most %d); "+
		"%d trailing-slash paths left to the opening stat; %d refused at link %d after different links",
		s.trees, s.queries, s.mismatches, s.regular, s.dirs, s.others, s.enoent, s.enotdir, s.eloop,
		s.otherErrs, s.viaLink, s.viaAbs, s.viaDotDot, s.viaTrailing, s.atLimit, maxLinkHops, s.maxHops,
		s.aloneOnly, s.pastLimit, maxLinkHops+1)
}

// kernelLinkLimit is the most symbolic links the kernel follows in one
// lookup, where known: maxLinkHops copies Linux's.
func kernelLinkLimit() int {
	switch runtime.GOOS {
	case "linux", "android":
		return maxLinkHops
	}
	return 0
}

// checkTrustWalkQuery resolves q with the kernel and with the walk, both
// with and without the walk's opening stat, and returns every disagreement.
func checkTrustWalkQuery(q string, st *twStats) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	kfi, kerr := os.Stat(q)
	kernelOK := kerr == nil && kfi.Mode().IsRegular()
	switch {
	case kernelOK:
		st.regular++
	case kerr == nil && kfi.IsDir():
		st.dirs++
	case kerr == nil:
		st.others++
	case errors.Is(kerr, syscall.ENOENT):
		st.enoent++
	case errors.Is(kerr, syscall.ENOTDIR):
		st.enotdir++
	case errors.Is(kerr, syscall.ELOOP):
		st.eloop++
	default:
		st.otherErrs++
	}
	kernel := "error " + fmt.Sprint(kerr)
	if kerr == nil {
		kernel = "mode " + kfi.Mode().String()
	}

	k := &kernelFS{}
	got, ok := trustedUnixExecutable(k, q)
	if ok != kernelOK {
		bad("walk: %s; kernel: %s", walkVerdict(got, ok), kernel)
	}
	if ok {
		problems = append(problems, checkResolved(got, kfi)...)
		if n := len(k.targets); n > 0 {
			st.viaLink++
			st.maxHops = max(st.maxHops, n)
			if n == maxLinkHops {
				st.atLimit++
			}
		}
		var abs, dotdot, trailing bool
		for _, target := range k.targets {
			abs = abs || strings.HasPrefix(target, "/")
			dotdot = dotdot || slices.Contains(strings.Split(target, "/"), "..")
			trailing = trailing || strings.HasSuffix(target, "/")
		}
		st.viaAbs += b2i(abs)
		st.viaDotDot += b2i(dotdot)
		st.viaTrailing += b2i(trailing)
	}

	// Where the kernel refuses, the walk alone must refuse too, except that
	// it does not see a "/" or "/." after a file, which the kernel refuses
	// (ENOTDIR) and the opening stat catches; and that a kernel with a lower
	// link limit than Linux's refuses longer chains (ELOOP). Where the
	// kernel accepts, the walk alone is the walk.
	if !kernelOK {
		a := &kernelFS{noPrecheck: true}
		alone, aok := trustedUnixExecutable(a, q)
		switch {
		case aok && (errors.Is(kerr, syscall.ENOTDIR) || (errors.Is(kerr, syscall.ELOOP) && kernelLinkLimit() == 0)):
			st.aloneOnly++
			problems = append(problems, checkResolved(alone, nil)...)
		case aok:
			bad("walk alone: %s; kernel: %s", walkVerdict(alone, aok), kernel)
		case errors.Is(kerr, syscall.ELOOP) && len(a.targets) == maxLinkHops &&
			len(slices.Compact(slices.Sorted(slices.Values(a.targets)))) == maxLinkHops:
			st.pastLimit++
		}
	}
	if len(problems) > 0 {
		st.mismatches++
	}
	return problems
}

// checkResolved checks a path the walk accepted: absolute, clean, without
// a symbolic link in any component, a regular file, and the file the kernel
// resolved, if it did.
func checkResolved(p string, kernel fs.FileInfo) []string {
	var problems []string
	if !filepath.IsAbs(p) || p != filepath.Clean(p) {
		problems = append(problems, fmt.Sprintf("resolved path %q is not absolute and clean", p))
	}
	for i := 1; i <= len(p); i++ {
		if i < len(p) && p[i] != '/' {
			continue
		}
		fi, err := os.Lstat(p[:i])
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("resolved path %q: %v", p, err))
		case fi.Mode()&fs.ModeSymlink != 0:
			problems = append(problems, fmt.Sprintf("resolved path %q has a link at %q", p, p[:i]))
		case i == len(p) && !fi.Mode().IsRegular():
			problems = append(problems, fmt.Sprintf("resolved path %q is a %v", p, fi.Mode().Type()))
		}
	}
	if kernel != nil {
		if fi, err := os.Stat(p); err != nil || !os.SameFile(fi, kernel) {
			problems = append(problems, fmt.Sprintf("resolved path %q is not the kernel's file (%v)", p, err))
		}
	}
	return problems
}

func walkVerdict(p string, ok bool) string {
	if ok {
		return "accepts " + p
	}
	return "refuses"
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// trustWalkBase returns a directory for the trees: absolute and without
// symbolic links, so that link counts are those the generator planned.
func trustWalkBase(t testing.TB) string {
	dir, err := filepath.Abs(t.TempDir())
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkTrustWalkTree creates tree number seed under base, resolves its
// queries and removes it. Its root lies three directories below base,
// named from the pool, so paths that climb past the root mostly stay
// inside base, and some come back down by name.
func checkTrustWalkTree(t testing.TB, base string, seed uint64, st *twStats) {
	top := filepath.Join(base, "a")
	tree := genTrustWalkTree(seed, filepath.Join(top, "b", "c"))
	defer os.RemoveAll(top)
	if err := tree.create(); err != nil {
		t.Fatalf("tree %d: %v", seed, err)
	}
	st.trees++
	reported := false
	for _, q := range tree.queries {
		st.queries++
		problems := checkTrustWalkQuery(q, st)
		if len(problems) == 0 || st.mismatches > 20 {
			continue
		}
		if !reported {
			t.Errorf("tree %d (seed %#x): %v", seed, trustWalkSeed, tree)
			reported = true
		}
		t.Errorf("tree %d: %s:\n\t%s", seed, q, strings.Join(problems, "\n\t"))
	}
}

// TestTrustWalkMatchesKernel compares trustedUnixExecutable with the kernel
// on random trees, through a file system that presents every entry as
// root's, so that only path resolution is under test: the walk accepts a
// path exactly when stat succeeds and finds a regular file, and the path it
// returns is clean, holds no link and is the kernel's file.
func TestTrustWalkMatchesKernel(t *testing.T) {
	trees := trustWalkTrees
	if testing.Short() {
		trees = trustWalkShortTrees
	}
	if s := os.Getenv("GOOMBA_TRUSTWALK_TREES"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("GOOMBA_TRUSTWALK_TREES=%q: want a positive number", s)
		}
		trees = n
	}
	base := trustWalkBase(t)
	var st twStats
	for i := range trees {
		checkTrustWalkTree(t, base, uint64(i), &st)
	}
	t.Log(&st)
	if trees < trustWalkShortTrees {
		return
	}
	// The generator must keep exercising what the walk has to get right.
	for _, c := range []struct {
		what string
		n    int
	}{
		{"regular files", st.regular}, {"directories", st.dirs}, {"FIFOs", st.others},
		{"missing entries", st.enoent}, {"files used as directories", st.enotdir},
		{"link loops", st.eloop}, {"accepted walks through links", st.viaLink},
		{"accepted walks through absolute targets", st.viaAbs},
		{"accepted walks through targets with ..", st.viaDotDot},
		{"accepted walks through targets with a trailing slash", st.viaTrailing},
		{"paths only the opening stat refuses", st.aloneOnly},
	} {
		if c.n == 0 {
			t.Errorf("no queries reached %s", c.what)
		}
	}
	if kernelLinkLimit() == maxLinkHops && (st.atLimit == 0 || st.pastLimit == 0) {
		t.Errorf("%d walks accepted after exactly %d links, %d refused at link %d; want some of each",
			st.atLimit, maxLinkHops, st.pastLimit, maxLinkHops+1)
	}
}

// FuzzTrustWalk runs the comparison of TestTrustWalkMatchesKernel on the
// tree generated from each input.
func FuzzTrustWalk(f *testing.F) {
	for _, seed := range []uint64{0, 1, 2, 3, trustWalkTrees} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		var st twStats
		checkTrustWalkTree(t, trustWalkBase(t), seed, &st)
	})
}
