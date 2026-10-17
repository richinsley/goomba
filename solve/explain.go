package solve

import (
	"sort"
	"strings"

	"github.com/richinsley/goomba/spec"
)

// conflictLeafSuffix ends cdcl's line for a candidate that conflicts with
// a choice reported higher up the tree.
const conflictLeafSuffix = ", which conflicts with the versions reported above."

// foldConflictLeaves makes cdcl's explanation shorter where it is most
// repetitive: cdcl prints one line per candidate that "conflicts with the
// versions reported above", so a package with many builds (python has
// hundreds) fills screens with lines that differ only in the version.
// Consecutive sibling lines of that kind for the same package are folded
// into one, listing the distinct versions in order as rattler lists merged
// candidates ("python 3.10.0 | 3.10.1 | ... | 3.10.20, which conflicts
// ..."). Every other line is kept as it is.
func foldConflictLeaves(text string) string {
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		leaf, ok := parseConflictLeaf(lines[i])
		if !ok {
			b.WriteString(lines[i])
			i++
			continue
		}
		versions := leaf.versions
		last := leaf
		j := i + 1
		for ; j < len(lines) && last.connector == "├─ "; j++ {
			next, ok := parseConflictLeaf(lines[j])
			if !ok || next.prefix != leaf.prefix || next.name != leaf.name {
				break
			}
			versions = append(versions, next.versions...)
			last = next
		}
		if j == i+1 {
			b.WriteString(lines[i])
			i++
			continue
		}
		b.WriteString(leaf.prefix + last.connector + leaf.name + " " + strings.Join(mergeVersions(versions), " | ") + conflictLeafSuffix + "\n")
		i = j
	}
	return b.String()
}

// conflictLeaf is a parsed "<prefix><connector><name> <versions>, which
// conflicts with the versions reported above." line.
type conflictLeaf struct {
	prefix, connector, name string
	versions                []leafVersion
}

// leafVersion is a version as displayed and parsed.
type leafVersion struct {
	raw    string
	parsed spec.Version
}

// parseConflictLeaf parses one line of a cdcl explanation (with its
// newline). It rejects lines whose versions were already abbreviated with
// "...".
func parseConflictLeaf(line string) (conflictLeaf, bool) {
	body, ok := strings.CutSuffix(line, conflictLeafSuffix+"\n")
	if !ok {
		return conflictLeaf{}, false
	}
	var l conflictLeaf
	for {
		switch {
		case strings.HasPrefix(body, "│  "):
			l.prefix += "│  "
			body = body[len("│  "):]
			continue
		case strings.HasPrefix(body, "   "):
			l.prefix += "   "
			body = body[len("   "):]
			continue
		}
		break
	}
	switch {
	case strings.HasPrefix(body, "├─ "):
		l.connector = "├─ "
	case strings.HasPrefix(body, "└─ "):
		l.connector = "└─ "
	default:
		return conflictLeaf{}, false
	}
	body = body[len(l.connector):]
	name, versions, ok := strings.Cut(body, " ")
	if !ok || name == "" {
		return conflictLeaf{}, false
	}
	l.name = name
	for _, v := range strings.Split(versions, " | ") {
		parsed, err := spec.ParseVersion(v)
		if err != nil || v == "..." {
			return conflictLeaf{}, false
		}
		l.versions = append(l.versions, leafVersion{raw: v, parsed: parsed})
	}
	return l, true
}

// mergeVersions returns the distinct versions in order, abbreviated around
// "..." when there are more than four, like DisplayMergedSolvables.
func mergeVersions(vs []leafVersion) []string {
	sorted := append([]leafVersion(nil), vs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].parsed.Compare(sorted[j].parsed) < 0 })
	var out []string
	for i, v := range sorted {
		if i > 0 && v.parsed.Compare(sorted[i-1].parsed) == 0 {
			continue
		}
		out = append(out, v.raw)
	}
	const head, tail = 2, 1
	if len(out) > head+tail+1 {
		abbreviated := append([]string{}, out[:head]...)
		abbreviated = append(abbreviated, "...")
		out = append(abbreviated, out[len(out)-tail:]...)
	}
	return out
}
