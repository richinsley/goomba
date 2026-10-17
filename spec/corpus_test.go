package spec

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// condaElem is one element of conda's VersionOrder.version lists: an int,
// a string, or float("inf") for "post".
type condaElem struct {
	str   string
	num   uint64
	isStr bool
	inf   bool
}

// parseCondaRepr parses Python's repr of VersionOrder.version, e.g.
// "[[0], [1], [0, 'DEV', 2], [0, inf]]".
func parseCondaRepr(s string) ([][]condaElem, error) {
	if !strings.HasPrefix(s, "[[") || !strings.HasSuffix(s, "]]") {
		return nil, fmt.Errorf("bad repr %q", s)
	}
	var out [][]condaElem
	for _, comp := range strings.Split(s[2:len(s)-2], "], [") {
		var elems []condaElem
		for _, e := range strings.Split(comp, ", ") {
			switch {
			case e == "inf":
				elems = append(elems, condaElem{inf: true})
			case strings.HasPrefix(e, "'"):
				elems = append(elems, condaElem{isStr: true, str: strings.Trim(e, "'")})
			default:
				n, err := strconv.ParseUint(e, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("bad element %q in %q", e, s)
				}
				elems = append(elems, condaElem{num: n})
			}
		}
		out = append(out, elems)
	}
	return out, nil
}

// condaRepr formats v as conda's VersionOrder.version repr.
func condaRepr(v Version) string {
	var b strings.Builder
	b.WriteString("[[" + strconv.FormatUint(v.epoch, 10) + "]")
	sec := v.ver()
	for i := 0; i < len(sec); {
		ni := nextPart(sec, i)
		b.WriteString(", [")
		for k, a := range sec[i:ni] {
			if k > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.FormatUint(a.num, 10))
			switch a.lit {
			case "":
			case "post":
				b.WriteString(", inf")
			case "dev":
				b.WriteString(", 'DEV'")
			default:
				b.WriteString(", '" + a.lit + "'")
			}
		}
		b.WriteString("]")
		i = ni
	}
	b.WriteString("]")
	return b.String()
}

// condaCompare is conda's VersionOrder comparison (models/version.py) on
// the version lists: missing components are [] and missing elements 0;
// strings sort before numbers; inf after every number.
func condaCompare(a, b [][]condaElem) int {
	zero := condaElem{}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y []condaElem
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		for k := 0; k < len(x) || k < len(y); k++ {
			c1, c2 := zero, zero
			if k < len(x) {
				c1 = x[k]
			}
			if k < len(y) {
				c2 = y[k]
			}
			if c1 == c2 {
				continue
			}
			switch {
			case c1.isStr && !c2.isStr:
				return -1
			case !c1.isStr && c2.isStr:
				return 1
			case c1.isStr:
				return strings.Compare(c1.str, c2.str)
			case c1.inf:
				return 1
			case c2.inf:
				return -1
			}
			return cmpUint(c1.num, c2.num)
		}
	}
	return 0
}

// TestParsedVersionsCorpus parses every version of
// testdata/parsed_versions.txt (copied from rattler test-data, generated with
// conda's VersionOrder; see testdata/README.md) and checks the result
// against conda's parse and conda's ordering.
func TestParsedVersionsCorpus(t *testing.T) {
	f, err := os.Open("testdata/parsed_versions.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	type entry struct {
		raw   string
		v     Version
		conda [][]condaElem
	}
	var entries []entry
	failures := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.TrimSpace(line) == "" {
			continue
		}
		raw, repr, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		v, err := ParseVersion(raw)
		if err != nil {
			failures++
			t.Errorf("ParseVersion(%q): %v", raw, err)
			continue
		}
		if got := condaRepr(v); got != repr {
			failures++
			t.Errorf("ParseVersion(%q) = %s, conda parsed %s", raw, got, repr)
			continue
		}
		conda, err := parseCondaRepr(repr)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{raw, v, conda})
		if failures > 20 {
			t.Fatal("too many failures")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(entries) < 28000 {
		t.Fatalf("only %d versions parsed", len(entries))
	}

	// Sort everything with Compare and check neighbours with conda's rules.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].v.Compare(entries[j].v) < 0 })
	for i := 1; i < len(entries); i++ {
		a, b := entries[i-1], entries[i]
		if c := condaCompare(a.conda, b.conda); c > 0 {
			t.Errorf("sorted %q before %q, conda orders them the other way", a.raw, b.raw)
		}
	}
	// Check a deterministic sample of pairs for the exact sign.
	for i := 0; i < len(entries); i++ {
		j := int((int64(i)*104729 + 7) % int64(len(entries)))
		a, b := entries[i], entries[j]
		want := condaCompare(a.conda, b.conda)
		got, _ := cmpSection(a.v.ver(), b.v.ver())
		if a.v.epoch != b.v.epoch {
			got = cmpUint(a.v.epoch, b.v.epoch)
		}
		if got != want {
			t.Errorf("compare(%q, %q) = %d, conda %d", a.raw, b.raw, got, want)
		}
	}
}
