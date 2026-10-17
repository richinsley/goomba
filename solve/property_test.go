package solve

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/richinsley/goomba/cdcl"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
)

// Vocabulary of the random channels of the property test and the fuzz
// target.
var (
	genNames    = []string{"a", "b", "c", "d", "e"}
	genVersions = []string{"1", "1.5", "2", "2.0.1", "3"}
	genSpecs    = []string{"", " >=2", " <2", " 1.*", " ==2", " >1,<3", " !=1.5", " 2|3", " >=1.5 b1", " * b0"}
	genVirtual  = []string{"__v", "__w"}
	genChannels = []string{"https://conda.anaconda.org/x", "https://conda.anaconda.org/y"}
)

// byteSource reads small numbers from fuzz input, yielding zeros once it
// runs out.
type byteSource struct {
	data []byte
	rng  *rand.Rand
}

func (s *byteSource) next(n int) int {
	if n <= 0 {
		return 0
	}
	if s.rng != nil {
		return s.rng.Intn(n)
	}
	if len(s.data) == 0 {
		return 0
	}
	b := s.data[0]
	s.data = s.data[1:]
	return int(b) % n
}

// raw returns up to 24 bytes of the input as a string (fuzz input only).
func (s *byteSource) raw() string {
	if s.rng != nil {
		return "a" + genSpecs[s.rng.Intn(len(genSpecs))]
	}
	n := min(s.next(25), len(s.data))
	r := string(s.data[:n])
	s.data = s.data[n:]
	return r
}

// genSpec returns a match spec on a random package.
func (s *byteSource) genSpec(withVirtual bool) string {
	switch s.next(16) {
	case 0:
		return s.raw()
	case 1:
		if withVirtual {
			return genVirtual[s.next(len(genVirtual))] + genSpecs[s.next(len(genSpecs))]
		}
	}
	n := genNames[s.next(len(genNames))]
	switch s.next(8) {
	case 0, 1, 2:
		return n // any version
	case 3:
		n = "x::" + n
	}
	return n + genSpecs[s.next(len(genSpecs))]
}

// genCase builds a small channel and a request from s.
func genCase(s *byteSource) ([]*record.PackageRecord, Request) {
	var recs []*record.PackageRecord
	seen := map[string]bool{}
	for _, n := range genNames {
		count := s.next(5)
		if s.rng != nil {
			count = 1 + s.next(4)
		}
		for i := 0; i < count; i++ {
			b := pkg(n).version(genVersions[s.next(len(genVersions))]).build(fmt.Sprintf("b%d", s.next(3))).buildNumber(uint64(s.next(3)))
			if s.rng == nil {
				// Fuzz input may also supply raw versions and build
				// strings, to reach every place a record is displayed.
				switch s.next(16) {
				case 0:
					b = b.version(s.raw())
				case 1:
					b = b.build(s.raw())
				}
			}
			b.r.Channel = genChannels[s.next(len(genChannels))]
			if s.next(6) == 0 {
				b = b.tarBz2()
			}
			if seen[b.r.Channel+b.r.Dist()] {
				continue
			}
			seen[b.r.Channel+b.r.Dist()] = true
			for d := s.next(4) - s.next(2); d > 0; d-- {
				b.r.Depends = append(b.r.Depends, s.genSpec(true))
			}
			for c := s.next(3); c > 0; c-- {
				b.r.Constrains = append(b.r.Constrains, s.genSpec(true))
			}
			switch s.next(20) {
			case 0:
				b.r.SHA256 = ""
			case 1, 2:
				b.r.TrackFeatures = []string{"tf"}
			}
			b.r.Timestamp = int64(s.next(4)) * 1000
			recs = append(recs, b.rec())
		}
	}
	var req Request
	for i := s.next(3) + 1; i > 0; i-- {
		req.Specs = append(req.Specs, s.genSpec(false))
	}
	for i := s.next(3); i > 0; i-- {
		req.Constraints = append(req.Constraints, s.genSpec(true))
	}
	for _, v := range genVirtual {
		if s.next(2) == 0 {
			req.Virtual = append(req.Virtual, virtualPkg(v, genVersions[s.next(len(genVersions))], "0"))
		}
	}
	if len(recs) > 0 {
		switch s.next(6) {
		case 0:
			req.Locked = append(req.Locked, recs[s.next(len(recs))])
		case 1:
			// A lock on a record the channel does not have (any more).
			l := *recs[s.next(len(recs))]
			l.Version = "0.1"
			l.Fn = l.Dist() + ".conda"
			l.Depends = nil
			req.Locked = append(req.Locked, &l)
		}
		if s.next(4) == 0 {
			f := recs[s.next(len(recs))]
			if len(req.Locked) == 0 || req.Locked[0].Name != f.Name {
				req.Favored = append(req.Favored, f)
			}
		}
	}
	return recs, req
}

// bruteForce reports whether any assignment of at most one record per
// package (from recs and req.Locked) is a valid solution of req, by
// checking every assignment with Validate.
func bruteForce(recs []*record.PackageRecord, req Request) ([]*record.PackageRecord, bool) {
	byName := map[string][]*record.PackageRecord{}
	for _, r := range recs {
		byName[r.Name] = append(byName[r.Name], r)
	}
	for _, r := range req.Locked {
		byName[r.Name] = append(byName[r.Name], r)
	}
	var names []string
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	choice := make([]int, len(names)) // 0: absent, i: byName[n][i-1]
	for {
		var sel []*record.PackageRecord
		for i, n := range names {
			if choice[i] > 0 {
				sel = append(sel, byName[n][choice[i]-1])
			}
		}
		if Validate(req, sel) == nil {
			return sel, true
		}
		i := 0
		for ; i < len(names); i++ {
			choice[i]++
			if choice[i] <= len(byName[names[i]]) {
				break
			}
			choice[i] = 0
		}
		if i == len(names) {
			return nil, false
		}
	}
}

// checkCase solves a generated case and checks the answer: a solution
// must pass Validate, an unsolvable answer must be confirmed by brute
// force, a malformed request must be reported as such, and nothing else
// may go wrong.
func checkCase(t *testing.T, recs []*record.PackageRecord, req Request) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sol, err := Solve(ctx, repodata.NewMemory(recs), req)
	var u *Unsolvable
	if err != nil && !isCleanText(err.Error()) {
		t.Fatalf("error text with control or bidi characters: %q", err.Error())
	}
	if errors.As(err, &u) && !isCleanText(u.Explanation) {
		t.Fatalf("explanation with control or bidi characters: %q", u.Explanation)
	}
	switch {
	case err == nil:
		if verr := Validate(req, sol.Records); verr != nil {
			t.Fatalf("invalid solution %s for %q: %v", formatRecords(sol.Records), req.Specs, verr)
		}
	case errors.Is(err, ErrInvalidRequest):
		if verr := Validate(req, nil); !errors.Is(verr, ErrInvalidRequest) {
			t.Fatalf("Solve rejects the request (%v) but Validate does not (%v)", err, verr)
		}
	case errors.As(err, &u):
		if sel, ok := bruteForce(recs, req); ok {
			t.Fatalf("unsolvable %q (constraints %q, virtual %s, locked %s, favored %s) but %s is valid; records:\n%s\nexplanation:\n%s",
				req.Specs, req.Constraints, formatRecords(req.Virtual), formatRecords(req.Locked), formatRecords(req.Favored),
				formatRecords(sel), dumpRecords(recs), u.Explanation)
		}
		if u.Explanation == "" {
			t.Fatal("empty explanation")
		}
	default:
		if errors.Is(err, cdcl.ErrInternal) {
			t.Fatalf("internal error: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
}

func dumpRecords(recs []*record.PackageRecord) string {
	var s string
	for _, r := range recs {
		s += fmt.Sprintf("  %s bn=%d depends=%q constrains=%q tf=%v sha=%v\n", displayRecord(r), r.BuildNumber, r.Depends, r.Constrains, r.TrackFeatures, r.SHA256 != "")
	}
	return s
}

func TestPropertyRandomChannels(t *testing.T) {
	n := 3000
	if testing.Short() {
		n = 300
	}
	rng := rand.New(rand.NewSource(20261017))
	solved, unsolvable := 0, 0
	for i := 0; i < n; i++ {
		recs, req := genCase(&byteSource{rng: rng})
		checkCase(t, recs, req)
		var u *Unsolvable
		switch _, err := Solve(context.Background(), repodata.NewMemory(recs), req); {
		case err == nil:
			solved++
		case errors.As(err, &u):
			unsolvable++
		}
	}
	t.Logf("%d solved, %d unsolvable", solved, unsolvable)
	if solved < n/5 || unsolvable < n/5 {
		t.Errorf("the generator is lopsided: %d solved, %d unsolvable of %d", solved, unsolvable, n)
	}
}

func TestPropertyDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		recs, req := genCase(&byteSource{rng: rng})
		want := outcome(Solve(context.Background(), repodata.NewMemory(recs), req))
		shuffled := append([]*record.PackageRecord(nil), recs...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := outcome(Solve(context.Background(), repodata.NewMemory(shuffled), req)); got != want {
			t.Fatalf("case %d: order changed the outcome:\n%s\nwant:\n%s", i, got, want)
		}
	}
}
