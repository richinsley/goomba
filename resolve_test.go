package goomba

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/repodata"
	"github.com/richinsley/goomba/solve"
	"github.com/richinsley/goomba/virtual"
)

// lockBytes writes lf in its JSON form.
func lockBytes(t testing.TB, lf *lock.Lockfile) []byte {
	t.Helper()
	var buf bytes.Buffer
	must(t, lf.Write(&buf))
	return buf.Bytes()
}

// lfNames lists "name version build" of a lockfile's packages, sorted.
func lfNames(lf *lock.Lockfile) []string {
	return lockedNames(lf.Records())
}

// TestLockShardedAndFull: Lock resolves against a sharded channel and,
// when the channel publishes no shard index, against its repodata.json,
// and records the request in the lockfile.
func TestLockShardedAndFull(t *testing.T) {
	plat := hostPlatform(t)
	for _, sharded := range []bool{true, false} {
		t.Run(fmt.Sprintf("sharded=%v", sharded), func(t *testing.T) {
			s := newSpecServer(t)
			ch := s.channel("main")
			ch.list(plat, "foo", "1.0", "h0_0", "bar >=1")
			ch.list(plat, "foo", "2.0", "h0_0", "bar >=2")
			ch.list("noarch", "bar", "1.5", "0")
			ch.list("noarch", "bar", "2.1", "0")
			ch.list("noarch", "unrelated", "1", "0")
			if sharded {
				ch.publishSharded(t, plat)
			} else {
				ch.publishFull(t, plat)
			}
			var log eventLog
			opt := s.lockOptions(t, []string{"foo"}, ch)
			opt.Progress = log.add
			lf, err := Lock(ctxT(t), opt)
			must(t, err)
			if got, want := lfNames(lf), []string{"bar 2.1 0", "foo 2.0 h0_0"}; !slices.Equal(got, want) {
				t.Errorf("packages %q, want %q", got, want)
			}
			if !slices.Equal(lf.Channels, []string{ch.url}) || !slices.Equal(lf.Specs, []string{"foo"}) || lf.Platform != plat {
				t.Errorf("lockfile header %q %q %q", lf.Channels, lf.Specs, lf.Platform)
			}
			wantVirt := virtual.ToOverrides(testVirtual(t))
			if fmt.Sprint(lf.Virtual) != fmt.Sprint(wantVirt) {
				t.Errorf("virtual %v, want %v", lf.Virtual, wantVirt)
			}
			for _, p := range lf.Packages {
				if want := ch.url + "/" + p.Subdir + "/" + p.Name + "-" + p.Version + "-" + p.Build + ".conda"; p.URL != want {
					t.Errorf("url %s, want %s", p.URL, want)
				}
			}
			s.mu.Lock()
			shardIdx, full := s.hits["/main/"+plat+"/repodata_shards.msgpack.zst"], s.hits["/main/"+plat+"/repodata.json"]
			s.mu.Unlock()
			if shardIdx != 1 || (full == 1) == sharded {
				t.Errorf("shard index requested %d times, repodata.json %d times", shardIdx, full)
			}
			ev := log.of(EventRepodata, EventSolving, EventSolved)
			want := []Event{
				{Kind: EventRepodata, Channel: ch.url, Done: 1, Total: 1},
				{Kind: EventSolving, Done: 1, Total: 1},
				{Kind: EventSolved, Done: 2, Total: 2},
			}
			if !slices.Equal(ev, want) {
				t.Errorf("events %+v, want %+v", ev, want)
			}
		})
	}
}

// TestLockChannelPriority: strict channel priority, in the order given; the
// lockfile lists the channels in that order; names resolve against the
// channel alias, as do channel-qualified specs.
func TestLockChannelPriority(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	a, b := s.channel("a"), s.channel("b")
	a.list(plat, "foo", "1.0", "0")
	b.list(plat, "foo", "2.0", "0")
	b.list(plat, "bar", "1.0", "0", "foo")
	a.publishSharded(t, plat)
	b.publishFull(t, plat)
	ctx := ctxT(t)

	opt := s.lockOptions(t, []string{"bar"}, a, b)
	lf, err := Lock(ctx, opt)
	must(t, err)
	if got := lfNames(lf); !slices.Equal(got, []string{"bar 1.0 0", "foo 1.0 0"}) {
		t.Errorf("a, b: %q", got)
	}
	if !slices.Equal(lf.Channels, []string{a.url, b.url}) {
		t.Errorf("channels %q", lf.Channels)
	}
	for _, p := range lf.Packages {
		if want := map[string]string{"foo": a.url, "bar": b.url}[p.Name]; !strings.HasPrefix(p.URL, want+"/") {
			t.Errorf("%s from %s, want %s", p.Name, p.URL, want)
		}
	}
	opt = s.lockOptions(t, []string{"bar"}, b, a)
	lf, err = Lock(ctx, opt)
	must(t, err)
	if got := lfNames(lf); !slices.Equal(got, []string{"bar 1.0 0", "foo 2.0 0"}) || !slices.Equal(lf.Channels, []string{b.url, a.url}) {
		t.Errorf("b, a: %q from %q", got, lf.Channels)
	}

	// Channel names through the alias, and channel-qualified specs.
	opt = s.lockOptions(t, []string{"a::foo"})
	opt.Channels, opt.ChannelAlias = []string{"a", "b"}, s.srv.URL
	lf, err = Lock(ctx, opt)
	must(t, err)
	if got := lfNames(lf); !slices.Equal(got, []string{"foo 1.0 0"}) || !slices.Equal(lf.Channels, []string{a.url, b.url}) {
		t.Errorf("alias: %q from %q", got, lf.Channels)
	}
	opt.Specs = []string{"b::foo"}
	_, err = Lock(ctx, opt)
	var u *solve.Unsolvable
	if !errors.As(err, &u) || !strings.Contains(u.Explanation, "strict channel priority") {
		t.Errorf("b::foo against a, b: %v", err)
	}
}

// pythonChannel publishes python 3.11.0dev0 and pkg 1.0 depending on
// python <3.11.0a0: with the python 3.11 prefilter pkg is dropped, though
// the development build satisfies both python=3.11 and pkg.
func pythonChannel(t *testing.T, s *specServer, plat string) *specChannel {
	ch := s.channel("py")
	ch.list(plat, "python", "3.11.0dev0", "0")
	ch.list(plat, "python", "3.11.5", "0")
	ch.list(plat, "pkg", "1.0", "0", "python <3.11.0a0")
	ch.list(plat, "good", "1.0", "0", "python >=3.11,<3.12.0a0")
	ch.list(plat, "old", "1.0", "0", "python >=3.8,<3.9.0a0")
	ch.publishSharded(t, plat)
	return ch
}

// TestLockPrefilterRetry: an unsolvable request with the python-minor
// prefilter on is solved again without it, from the cached index, and the
// events say which attempt succeeded.
func TestLockPrefilterRetry(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := pythonChannel(t, s, plat)
	ctx := ctxT(t)

	// Solvable with the prefilter: one attempt.
	var log eventLog
	opt := s.lockOptions(t, []string{"python=3.11", "good"}, ch)
	opt.Progress = log.add
	lf, err := Lock(ctx, opt)
	must(t, err)
	if got := lfNames(lf); !slices.Equal(got, []string{"good 1.0 0", "python 3.11.5 0"}) {
		t.Errorf("packages %q", got)
	}
	want := []Event{
		{Kind: EventSolving, Prefilter: "3.11", Done: 1, Total: 2},
		{Kind: EventSolved, Prefilter: "3.11", Done: 2, Total: 2},
	}
	if got := log.of(EventSolving, EventSolved); !slices.Equal(got, want) {
		t.Errorf("events %+v", got)
	}

	// Unsolvable with it: the retry without the prefilter finds pkg, and
	// downloads nothing (the index is fresh, the shards are cached).
	log = eventLog{}
	opt = s.lockOptions(t, []string{"python=3.11", "pkg"}, ch)
	opt.MaxAge = time.Hour
	opt.Progress = log.add
	before := s.requests()
	lf, err = Lock(ctx, opt)
	must(t, err)
	if got := lfNames(lf); !slices.Equal(got, []string{"pkg 1.0 0", "python 3.11.0dev0 0"}) {
		t.Errorf("packages %q", got)
	}
	want = []Event{
		{Kind: EventRepodata, Channel: ch.url, Prefilter: "3.11", Done: 1, Total: 1},
		{Kind: EventSolving, Prefilter: "3.11", Done: 1, Total: 2},
		{Kind: EventRepodata, Channel: ch.url, Done: 1, Total: 1},
		{Kind: EventSolving, Done: 2, Total: 2},
		{Kind: EventSolved, Done: 2, Total: 2},
	}
	if got := log.of(EventRepodata, EventSolving, EventSolved); !slices.Equal(got, want) {
		t.Errorf("events\n%+v\nwant\n%+v", got, want)
	}
	// The first attempt fetched the two shard indexes and the shards of
	// python and pkg; the retry fetched nothing.
	if n := s.requests() - before; n != 4 {
		s.mu.Lock()
		t.Errorf("%d requests, want 4: %v", n, s.hits)
		s.mu.Unlock()
	}

	// Unsolvable either way: two attempts, the explanation of the second.
	opt = s.lockOptions(t, []string{"python=3.11", "old"}, ch)
	_, err = Lock(ctx, opt)
	var u *solve.Unsolvable
	if !errors.As(err, &u) || !strings.Contains(u.Explanation, "old") {
		t.Errorf("err = %v", err)
	}
}

// TestLockUnsolvable: an unsolvable request is a *solve.Unsolvable whose
// text carries no terminal control sequences from the index, and Create
// leaves nothing behind.
func TestLockUnsolvable(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.list(plat, "foo", "2.0", "0")
	ch.list(plat, "evil", "1.0", "0", "foo >=3", "bar \x1b[2J\x1b]0;pwned\x07 >=1")
	ch.list(plat, "evil", "2.0", "0", "foo\u202e >=3")
	ch.publishSharded(t, plat)
	ctx := ctxT(t)
	for _, specs := range [][]string{{"foo >=3"}, {"evil"}, {"foo=1", "evil"}} {
		_, err := Lock(ctx, s.lockOptions(t, specs, ch))
		var u *solve.Unsolvable
		if !errors.As(err, &u) {
			t.Fatalf("%q: err = %v, want *solve.Unsolvable", specs, err)
		}
		if strings.ContainsAny(err.Error(), "\x1b\x07\u202e") {
			t.Errorf("%q: explanation carries control characters: %q", specs, err.Error())
		}
		t.Logf("%q:\n%s", specs, u.Explanation)
	}
	prefix := filepath.Join(t.TempDir(), "env")
	_, err := Create(ctx, prefix, s.lockOptions(t, []string{"foo >=3"}, ch), InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()})
	var u *solve.Unsolvable
	if !errors.As(err, &u) {
		t.Errorf("Create: %v", err)
	}
	assertNoPrefix(t, prefix)
}

// TestLockDeterministic: the same channels, cache state and request give a
// byte-identical lockfile, from a cold cache, a warm one and offline.
func TestLockDeterministic(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	a, b := s.channel("a"), s.channel("b")
	for i := range 30 {
		name := fmt.Sprintf("pkg%02d", i)
		deps := []string{}
		if i > 0 {
			deps = append(deps, fmt.Sprintf("pkg%02d >=1", i-1), "python >=3.11,<3.12.0a0")
		}
		for v := 1; v <= 3; v++ {
			(map[bool]*specChannel{true: a, false: b})[i%3 == 0].list(plat, name, fmt.Sprint(v), "0", deps...)
		}
	}
	b.list(plat, "python", "3.11.9", "0")
	b.list(plat, "python", "3.12.1", "0")
	a.publishSharded(t, plat)
	b.publishFull(t, plat)
	ctx := ctxT(t)
	opt := s.lockOptions(t, []string{"pkg29", "python=3.11"}, a, b)
	first, err := Lock(ctx, opt)
	must(t, err)
	want := lockBytes(t, first)
	for _, tc := range []struct {
		name   string
		mutate func(*LockOptions)
	}{
		{"warm cache", func(*LockOptions) {}},
		{"other cold cache", func(o *LockOptions) { o.RepodataDir = t.TempDir() }},
		{"offline", func(o *LockOptions) { o.Offline = true }},
	} {
		o := opt
		tc.mutate(&o)
		lf, err := Lock(ctx, o)
		must(t, err)
		if got := lockBytes(t, lf); !bytes.Equal(got, want) {
			t.Errorf("%s: lockfile differs:\n%s\nwant\n%s", tc.name, got, want)
		}
	}
	if len(first.Packages) != 31 {
		t.Errorf("%d packages", len(first.Packages))
	}
}

// TestLockOffline: offline, Lock uses the cache only, whatever its age,
// for sharded and full channels; a cold cache is ErrNotCached.
func TestLockOffline(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	a, b := s.channel("a"), s.channel("b")
	a.list(plat, "foo", "1.0", "0", "bar")
	b.list("noarch", "bar", "1.0", "0")
	a.publishSharded(t, plat)
	b.publishFull(t, plat)
	ctx := ctxT(t)
	opt := s.lockOptions(t, []string{"foo"}, a, b)
	online, err := Lock(ctx, opt)
	must(t, err)
	before := s.requests()
	opt.Offline = true
	offline, err := Lock(ctx, opt)
	must(t, err)
	if !bytes.Equal(lockBytes(t, online), lockBytes(t, offline)) {
		t.Error("offline lockfile differs")
	}
	if n := s.requests() - before; n != 0 {
		t.Errorf("%d requests offline", n)
	}
	opt.RepodataDir = t.TempDir()
	if _, err := Lock(ctx, opt); !errors.Is(err, repodata.ErrNotCached) {
		t.Errorf("cold cache offline: %v", err)
	}
	if n := s.requests() - before; n != 0 {
		t.Errorf("%d requests offline", n)
	}
}

// TestLockCancelLoading: cancelling ctx while the index downloads stops
// Lock promptly with context.Canceled.
func TestLockCancelLoading(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.publishSharded(t, plat)
	arrived := make(chan struct{}, 8)
	s.setHook(func(r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done() // never answers
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-arrived
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := Lock(ctx, s.lockOptions(t, []string{"foo"}, ch))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Lock did not return after cancellation")
	}
}

// pigeonholeChannel lists n+1 pigeon packages p<i>, each in n versions
// (version j: in hole j, which needs h<j> == i), and n hole packages h<j>
// with one version per pigeon: n+1 pigeons cannot share n holes, which a
// conflict-driven solver takes exponential time to prove.
func pigeonholeChannel(t *testing.T, s *specServer, plat string, n int) (*specChannel, []string) {
	ch := s.channel("pigeons")
	var specs []string
	for i := 0; i <= n; i++ {
		for j := 1; j <= n; j++ {
			ch.list(plat, fmt.Sprintf("p%02d", i), fmt.Sprint(j), "0", fmt.Sprintf("h%02d ==%d", j, i))
		}
		specs = append(specs, fmt.Sprintf("p%02d", i))
	}
	for j := 1; j <= n; j++ {
		for i := 0; i <= n; i++ {
			ch.list(plat, fmt.Sprintf("h%02d", j), fmt.Sprint(i), "0")
		}
	}
	ch.publishSharded(t, plat)
	return ch, specs
}

// TestLockCancelSolving: cancelling ctx during the solve stops Lock with
// context.Canceled: at once when cancelled as the solve starts, and
// promptly in the middle of a hard solve.
func TestLockCancelSolving(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch, specs := pigeonholeChannel(t, s, plat, 11)

	ctx, cancel := context.WithCancel(context.Background())
	opt := s.lockOptions(t, specs, ch)
	opt.Progress = func(e Event) {
		if e.Kind == EventSolving {
			cancel()
		}
	}
	if _, err := Lock(ctx, opt); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled at EventSolving: %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	started := make(chan time.Time, 1)
	opt.Progress = func(e Event) {
		if e.Kind == EventSolving {
			started <- time.Now()
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := Lock(ctx, opt)
		done <- err
	}()
	start := <-started
	time.Sleep(300 * time.Millisecond)
	cancel()
	cancelled := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled (the solve took %v)", err, time.Since(start))
		}
		t.Logf("returned %v after cancellation", time.Since(cancelled))
	case <-time.After(10 * time.Second):
		t.Fatal("Lock did not return after cancellation")
	}
}

// TestLockInvalidOptions: unusable options are ErrInvalidOptions errors,
// before any request.
func TestLockInvalidOptions(t *testing.T) {
	plat := hostPlatform(t)
	other := "osx-arm64"
	if plat == other {
		other = "linux-64"
	}
	s := newSpecServer(t)
	ch := s.channel("main")
	for _, tc := range []struct {
		name   string
		mutate func(*LockOptions)
	}{
		{"no specs", func(o *LockOptions) { o.Specs = nil }},
		{"bad spec", func(o *LockOptions) { o.Specs = []string{"foo >=>1"} }},
		{"bad constraint", func(o *LockOptions) { o.Constraints = []string{"(("} }},
		{"no channels", func(o *LockOptions) { o.Channels = nil }},
		{"defaults", func(o *LockOptions) { o.Channels = []string{"defaults"} }},
		{"platform filter", func(o *LockOptions) { o.Channels = []string{"conda-forge/linux-64"} }},
		{"platform list", func(o *LockOptions) { o.Channels = []string{"conda-forge[linux-64,noarch]"} }},
		{"http channel", func(o *LockOptions) { o.Channels = []string{"http://127.0.0.1:1/c"} }},
		{"file channel", func(o *LockOptions) { o.Channels = []string{"file:///tmp/c"} }},
		{"local path", func(o *LockOptions) { o.Channels = []string{"/tmp/c"} }},
		{"credentials", func(o *LockOptions) { o.Channels = []string{"https://user:pw@example.com/c"} }},
		{"token", func(o *LockOptions) { o.Channels = []string{"https://conda.anaconda.org/t/pw-1/c"} }},
		{"token without scheme", func(o *LockOptions) { o.Channels = []string{"conda.anaconda.org/t/pw-1/c"} }},
		{"same channel twice", func(o *LockOptions) { o.Channels = []string{ch.url, strings.ToUpper(ch.url[:5]) + ch.url[5:] + "/"} }},
		{"bad alias", func(o *LockOptions) { o.Channels, o.ChannelAlias = []string{"c"}, "https://a b" }},
		{"alias with credentials", func(o *LockOptions) { o.Channels, o.ChannelAlias = []string{"c"}, "https://u:pw@example.com" }},
		{"alias with token", func(o *LockOptions) { o.Channels, o.ChannelAlias = []string{"c"}, "https://example.com/t/pw-1" }},
		{"no repodata dir", func(o *LockOptions) { o.RepodataDir = "" }},
		{"negative max age", func(o *LockOptions) { o.MaxAge = -time.Second }},
		{"unknown platform", func(o *LockOptions) { o.Platform = "linux-65" }},
		{"noarch platform", func(o *LockOptions) { o.Platform = "noarch" }},
		{"other platform without virtual", func(o *LockOptions) { o.Platform, o.Virtual = other, nil }},
		{"non-virtual record", func(o *LockOptions) { o.Virtual = []*record.PackageRecord{{Name: "glibc", Version: "2", Build: "0"}} }},
		{"nil virtual record", func(o *LockOptions) { o.Virtual = []*record.PackageRecord{nil} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := s.lockOptions(t, []string{"foo"}, ch)
			tc.mutate(&opt)
			_, err := Lock(ctxT(t), opt)
			if !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("err = %v, want ErrInvalidOptions", err)
			}
			if verr := ValidateLock(opt); fmt.Sprint(verr) != fmt.Sprint(err) {
				t.Errorf("ValidateLock: %v, Lock: %v", verr, err)
			}
			if strings.Contains(fmt.Sprint(err), "pw") {
				t.Errorf("error shows the password: %v", err)
			}
		})
	}
	if n := s.requests(); n != 0 {
		t.Errorf("%d requests", n)
	}
}

// TestLockOtherPlatform: locking for another platform takes its index and
// the given virtual packages.
func TestLockOtherPlatform(t *testing.T) {
	plat := hostPlatform(t)
	other := "osx-arm64"
	if plat == other {
		other = "linux-64"
	}
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.list(other, "foo", "2.0", "0", "__osx >=11")
	ch.list(other, "foo", "3.0", "0", "__osx >=99")
	if other == "linux-64" {
		ch.list(other, "foo", "2.5", "0", "__glibc >=2.17")
	}
	ch.publishSharded(t, plat)
	ch.publishSharded(t, other)
	opt := s.lockOptions(t, []string{"foo"}, ch)
	opt.Platform = other
	opt.Virtual = virtual.Defaults(other)
	lf, err := Lock(ctxT(t), opt)
	must(t, err)
	if lf.Platform != other || len(lf.Packages) != 1 || lf.Packages[0].Subdir != other {
		t.Errorf("lockfile %+v", lf)
	}
	if fmt.Sprint(lf.Virtual) != fmt.Sprint(virtual.ToOverrides(virtual.Defaults(other))) {
		t.Errorf("virtual %v", lf.Virtual)
	}
	// Create refuses another platform before any request.
	before := s.requests()
	if _, err := Create(ctxT(t), filepath.Join(t.TempDir(), "env"), opt, InstallOptions{PkgsDir: t.TempDir()}); !errors.Is(err, ErrPlatform) {
		t.Errorf("Create for %s: %v", other, err)
	}
	if s.requests() != before {
		t.Error("Create made requests")
	}
}

// TestLockBaseURL: a package whose index places it under another URL (a
// base_url) is locked with that channel URL listed after the requested
// ones, and Create installs it from there.
func TestLockBaseURL(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	mirror := s.channel("mirror")
	p := buildPkg(t, tspec{name: "foo", version: "1.0", build: "0", files: []tfile{{path: "share/foo", data: "foo"}}})
	r := mirror.add(t, p) // the archive lives on the mirror
	ch.recs[plat] = append(ch.recs[plat], r)
	ch.baseURL = mirror.url + "/" + plat + "/"
	ch.publishSharded(t, plat)
	opt := s.lockOptions(t, []string{"foo"}, ch)
	lf, err := Lock(ctxT(t), opt)
	must(t, err)
	if !slices.Equal(lf.Channels, []string{ch.url, mirror.url}) || lf.Packages[0].URL != mirror.url+"/"+plat+"/"+r.Fn {
		t.Errorf("channels %q, url %s", lf.Channels, lf.Packages[0].URL)
	}
	prefix := filepath.Join(t.TempDir(), "env")
	_, err = Create(ctxT(t), prefix, opt, InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()})
	must(t, err)
	if data, err := os.ReadFile(filepath.Join(prefix, "share", "foo")); err != nil || string(data) != "foo" {
		t.Errorf("installed file: %q, %v", data, err)
	}
}

// TestCreate: Create resolves specs and installs the result, with the
// virtual packages of the solve, real and synthetic archives, a noarch:
// python package with an entry point, and byte-compilation.
func TestCreate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake interpreter is a shell script")
	}
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("cf")
	logDir := t.TempDir()
	ch.add(t, fakePython(t, logDir, 0))
	specs := []string{"python=3.11", "wheel", "ca-certificates"}
	for _, dist := range []string{"packaging-26.0-pyhcf101f3_0", "wheel-0.46.3-pyhd8ed1ab_0", "ca-certificates-2026.2.25-hbd8a1cb_0"} {
		ch.add(t, realPkg(t, dist))
	}
	if plat == "linux-64" {
		ch.add(t, realPkg(t, "libzlib-1.3.2-h25fd6f3_2"))
		specs = append(specs, "libzlib")
	}
	ch.publishSharded(t, plat)
	ctx := ctxT(t)
	opt := s.lockOptions(t, specs, ch)
	var lockLog, instLog eventLog
	opt.Progress = lockLog.add
	prefix := filepath.Join(t.TempDir(), "env")
	inst := InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client(), CompilePyc: true, Progress: instLog.add}
	lf, err := Create(ctx, prefix, opt, inst)
	must(t, err)
	if len(lockLog.of(EventSolved)) != 1 || len(lockLog.of(EventFetched, EventLinked, EventDone)) != 0 {
		t.Error("resolve events went elsewhere")
	}
	if len(instLog.of(EventDone)) != 1 || len(instLog.of(EventRepodata, EventSolving, EventSolved)) != 0 {
		t.Error("install events went elsewhere")
	}
	if len(instLog.of(EventCompiled)) != 1 {
		t.Error("no byte-compilation")
	}
	opt.Progress = nil
	locked, err := Lock(ctx, opt)
	must(t, err)
	if !bytes.Equal(lockBytes(t, lf), lockBytes(t, locked)) {
		t.Error("Create's lockfile differs from Lock's")
	}
	wantPkgs := 5
	if plat != "linux-64" {
		wantPkgs = 4
	}
	if len(lf.Packages) != wantPkgs {
		t.Errorf("packages %q", lfNames(lf))
	}
	for _, p := range lf.Packages {
		if _, err := os.Stat(filepath.Join(prefix, "conda-meta", p.Name+"-"+p.Version+"-"+p.Build+".json")); err != nil {
			t.Error(err)
		}
	}
	if fi, err := os.Stat(filepath.Join(prefix, "bin", "wheel")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("entry point: %v", err)
	}
	hist, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "history"))
	must(t, err)
	if !strings.Contains(string(hist), `# update specs: ["python=3.11", "wheel", "ca-certificates"`) {
		t.Errorf("history:\n%s", hist)
	}

	// A prefix that is not empty is refused before anything is resolved.
	before := s.requests()
	if _, err := Create(ctx, prefix, opt, inst); !errors.Is(err, ErrPrefixNotEmpty) {
		t.Errorf("non-empty prefix: %v", err)
	}
	inst.PkgsDir = ""
	if _, err := Create(ctx, filepath.Join(t.TempDir(), "x"), opt, inst); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("no PkgsDir: %v", err)
	}
	if s.requests() != before {
		t.Error("refused Create calls made requests")
	}
}

// TestCreateVirtual: without InstallOptions.Virtual, Create checks the
// lockfile against the virtual packages the solve used; with it, against
// those.
func TestCreateVirtual(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.add(t, buildPkg(t, tspec{name: "gpu", version: "1.0", build: "0", depends: []string{"__cuda >=12"}, files: []tfile{{path: "share/gpu", data: "x"}}}))
	ch.publishSharded(t, plat)
	opt := s.lockOptions(t, []string{"gpu"}, ch)
	withCuda, err := virtual.ApplyOverrides(plat, testVirtual(t), map[string]string{"cuda": "12.4"})
	must(t, err)
	opt.Virtual = withCuda
	ctx := ctxT(t)
	if _, err := Create(ctx, filepath.Join(t.TempDir(), "env"), opt, InstallOptions{PkgsDir: t.TempDir(), Client: s.client()}); err != nil {
		t.Errorf("solve's virtual packages: %v", err)
	}
	prefix := filepath.Join(t.TempDir(), "env")
	_, err = Create(ctx, prefix, opt, InstallOptions{PkgsDir: t.TempDir(), Client: s.client(), Virtual: testVirtual(t)})
	if !errors.Is(err, ErrVirtual) {
		t.Errorf("install's virtual packages without __cuda: %v", err)
	}
	assertNoPrefix(t, prefix)
}

// TestLockHostVirtual: with Virtual nil, Lock detects this machine's
// virtual packages and records them.
func TestLockHostVirtual(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.publishSharded(t, plat)
	opt := s.lockOptions(t, []string{"foo"}, ch)
	opt.Virtual, opt.Platform = nil, ""
	lf, err := Lock(ctxT(t), opt)
	must(t, err)
	host, err := virtual.Detect(ctxT(t), virtual.Options{})
	must(t, err)
	if fmt.Sprint(lf.Virtual) != fmt.Sprint(virtual.ToOverrides(host)) || lf.Platform != plat {
		t.Errorf("virtual %v, detected %v", lf.Virtual, virtual.ToOverrides(host))
	}
}

func TestChannelKey(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"https://Example.COM/c", "https://example.com/c/"},
		{"https://example.com:443/c", "https://example.com/c"},
		{"https://example.com./c", "https://example.com/c"},
		{"https://[FD00::1]:8443/c", "https://[fd00::1]:8443/c"},
	} {
		if channelKey(tc.a) != channelKey(tc.b) {
			t.Errorf("%s and %s differ: %s, %s", tc.a, tc.b, channelKey(tc.a), channelKey(tc.b))
		}
	}
	for _, tc := range []struct{ a, b string }{
		{"https://example.com/c", "https://example.com/C"},
		{"https://example.com:8443/c", "https://example.com/c"},
		{"https://[fd00::1]:8443/c", "https://[fd00::1:8443]/c"},
	} {
		if channelKey(tc.a) == channelKey(tc.b) {
			t.Errorf("%s and %s are the same: %s", tc.a, tc.b, channelKey(tc.a))
		}
	}
}

// TestLockChannelsOrder: concurrent channel loading reports the
// channels in priority order, whatever finishes first.
func TestLockChannelsOrder(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	var chans []*specChannel
	for i := range 4 {
		c := s.channel(fmt.Sprintf("c%d", i))
		c.list(plat, fmt.Sprintf("p%d", i), "1", "0")
		c.publishSharded(t, plat)
		chans = append(chans, c)
	}
	// The first channel answers last.
	s.setHook(func(r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/c0/") {
			time.Sleep(50 * time.Millisecond)
		}
	})
	var log eventLog
	opt := s.lockOptions(t, []string{"p0", "p3"}, chans...)
	opt.Progress = log.add
	lf, err := Lock(ctxT(t), opt)
	must(t, err)
	var got []string
	for _, e := range log.of(EventRepodata) {
		got = append(got, e.Channel)
	}
	var want []string
	for _, c := range chans {
		want = append(want, c.url)
	}
	if !slices.Equal(got, want) || !slices.Equal(lf.Channels, want) {
		t.Errorf("events %q, channels %q, want %q", got, lf.Channels, want)
	}
}

// TestChannelArgRedaction (review round 1, N1): credentials in channel
// arguments, in any spelling, never reach an error message or a channel
// URL: passwords holding '/', '?', '#', ' ' or bad %-escapes, user
// information after a mistyped scheme or with no scheme, %-encoded token
// segments.
func TestChannelArgRedaction(t *testing.T) {
	for _, a := range []string{
		"https://user:se/cret@example.com/c",
		"https://user:se?cret@example.com/c",
		"https://user:se#cret@example.com/c",
		"https://user:se cret@example.com/c",
		"https://user:se%zzcret@example.com/c",
		"user:secret@example.com/c",
		"https:/user:secret@example.com/c",
		"https:user:secret@example.com/c",
		"HTTPS://user:secret@example.com/c",
		"https://example.com/t/secret",
		"https://example.com/%74/secret/c",
		"https://example.com/c/t/secret/",
		"example.com/t/secret",
		"t/secret/c",
		" t/secret",
		" https://user:secret@example.com/c",
		"https://user:secret@example.com:99999/c",
		"https://[user:secret@example.com/c",
		"https:///user:secret@example.com/c",
		"https://user%3Asecret@example.com/c",
		"https://user:secret\x00@example.com/c",
		"https://example.com/t/t/secret/c",
	} {
		chans, err := parseChannels([]string{a}, "https://conda.anaconda.org")
		msg := fmt.Sprint(err)
		if strings.Contains(msg, "secret") || strings.Contains(msg, "cret") {
			t.Errorf("%q: the error shows the secret: %s", a, msg)
		}
		for _, c := range chans {
			if strings.Contains(c, "secret") {
				t.Errorf("%q accepted as %q", a, c)
			}
		}
		// The same through the alias.
		_, err = newLockRequest(LockOptions{Specs: []string{"x"}, Channels: []string{"c"}, ChannelAlias: a, RepodataDir: "d", Platform: "linux-64", Virtual: []*record.PackageRecord{}})
		if msg := fmt.Sprint(err); strings.Contains(msg, "secret") || strings.Contains(msg, "cret") {
			t.Errorf("alias %q: the error shows the secret: %s", a, msg)
		}
	}
	// A doubly encoded "t" is no token segment ("%74" to the server), but
	// escapes are refused anyway: lockfiles cannot hold them.
	if _, err := parseChannels([]string{"https://example.com/%2574/x/c"}, "https://conda.anaconda.org"); err == nil {
		t.Error("a channel with an escape was accepted")
	}
}

// TestCreateValidatesFirst (review round 1, N4): Create refuses what
// InstallLock would refuse in its options and prefix before resolving
// anything, with zero requests; ValidateInstall reports the same.
func TestCreateValidatesFirst(t *testing.T) {
	plat := hostPlatform(t)
	s := newSpecServer(t)
	ch := s.channel("main")
	ch.list(plat, "foo", "1.0", "0")
	ch.publishSharded(t, plat)
	full := t.TempDir()
	must(t, os.WriteFile(filepath.Join(full, "x"), nil, 0o644))
	file := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(file, nil, 0o644))
	for _, tc := range []struct {
		name   string
		prefix func() string
		mutate func(*InstallOptions, string)
		want   error
	}{
		{"no PkgsDir", nil, func(o *InstallOptions, _ string) { o.PkgsDir = "" }, ErrInvalidOptions},
		{"PkgsDir inside the prefix", nil, func(o *InstallOptions, p string) { o.PkgsDir = filepath.Join(p, "pkgs") }, ErrInvalidOptions},
		{"negative Concurrency", nil, func(o *InstallOptions, _ string) { o.Concurrency = -1 }, ErrInvalidOptions},
		{"line break in Cmd", nil, func(o *InstallOptions, _ string) { o.Cmd = "goomba create\n# injected" }, ErrInvalidOptions},
		{"malformed virtual record", nil, func(o *InstallOptions, _ string) {
			o.Virtual = []*record.PackageRecord{{Name: "glibc", Version: "2", Build: "0"}}
		}, ErrInvalidOptions},
		{"non-empty prefix", func() string { return full }, nil, ErrPrefixNotEmpty},
		{"prefix is a file", func() string { return file }, nil, ErrPrefixNotEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "env")
			if tc.prefix != nil {
				prefix = tc.prefix()
			}
			inst := InstallOptions{PkgsDir: filepath.Join(t.TempDir(), "pkgs"), Client: s.client()}
			if tc.mutate != nil {
				tc.mutate(&inst, prefix)
			}
			if err := ValidateInstall(prefix, inst); !errors.Is(err, tc.want) {
				t.Errorf("ValidateInstall: %v, want %v", err, tc.want)
			}
			before := s.requests()
			if _, err := Create(ctxT(t), prefix, s.lockOptions(t, []string{"foo"}, ch), inst); !errors.Is(err, tc.want) {
				t.Errorf("Create: %v, want %v", err, tc.want)
			}
			if n := s.requests() - before; n != 0 {
				t.Errorf("%d requests", n)
			}
		})
	}
	if err := ValidateInstall(filepath.Join(t.TempDir(), "env"), InstallOptions{PkgsDir: t.TempDir()}); err != nil {
		t.Errorf("a good call: %v", err)
	}
}
