package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCanonicalHost and TestIsPublicAddr moved here from package repodata
// (netguard_test.go there) with the code they test.

func TestCanonicalHost(t *testing.T) {
	for _, h := range []string{"conda.anaconda.org", "127.0.0.1", "::1", "1e100.net", "a-1.b", "10.0.0.5", "fd00::1"} {
		if err := canonicalHost(h); err != nil {
			t.Errorf("canonicalHost(%q): %v", h, err)
		}
	}
	for _, h := range []string{
		"", "localhost.", "LOCALHOST.", "127.0.0.1.", "127.1", "2130706433", "0x7f000001", "0177.0.0.1",
		"0x7f.0.0.1", "127.0.0.01", "ｌｏｃａｌｈｏｓｔ", "１２７．０．０．１", "127。0。0。1", "ⓛⓞⓒⓐⓛⓗⓞⓢⓣ",
		"fe80::1%eth0", "a b", "a\x00b", "1.2.3", "host.123",
	} {
		if err := canonicalHost(h); !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("canonicalHost(%q) = %v", h, err)
		}
	}
}

func TestIsPublicAddr(t *testing.T) {
	for _, s := range []string{
		"0.0.0.0", "0.0.0.1", "10.1.2.3", "100.64.0.1", "100.100.100.200", "100.127.255.255", "127.0.0.1",
		"127.255.255.254", "169.254.169.254", "172.16.0.1", "172.31.255.255", "192.0.0.8", "192.168.1.1",
		"198.18.0.1", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::127.0.0.1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1", "64:ff9b:1::1",
		"fc00::1", "fd00:ec2::254", "fe80::1", "fec0::1", "ff02::1",
		// Review round 1, N7: embedded non-public IPv4 addresses, Teredo,
		// documentation and discard ranges.
		"2002:7f00:1::", "2002:a9fe:a9fe::", "2002:a00:1::", "2002:c0a8:101::1",
		"2001:0:4136:e378:8000:63bf:80ff:fffe", "2001::1", "2001:0:808:808::",
		"::ffff:0:7f00:1", "::ffff:0:a9fe:a9fe", "::ffff:0:a00:1",
		"2001:db8::1", "2001:10::1", "2001:2::1", "100::1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "192.88.99.1",
	} {
		if isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s is public", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.0", "172.32.0.1", "2606:4700::1111", "::ffff:8.8.8.8",
		"2002:808:808::1", "::ffff:0:808:808", "2001:4860:4860::8888", "2a00:1450:4001::1"} {
		if !isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s is not public", s)
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestFor: the policy a starting URL implies.
func TestFor(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Policy
		ok   bool
	}{
		{"https://conda.anaconda.org/conda-forge", Policy{}, true},
		{"HTTPS://Conda.Anaconda.org/x", Policy{}, true},
		{"https://10.0.0.5/mirror", Policy{PrivateHosts: true}, true},
		{"https://localhost:8443/c", Policy{PrivateHosts: true}, true},
		{"https://[fd00::1]/c", Policy{PrivateHosts: true}, true},
		{"http://127.0.0.1:1234/c", Policy{HTTPLoopback: true, PrivateHosts: true}, true},
		{"http://localhost/c", Policy{HTTPLoopback: true, PrivateHosts: true}, true},
		{"http://[::1]:99/c", Policy{HTTPLoopback: true, PrivateHosts: true}, true},
		{"http://10.0.0.5/c", Policy{}, false},
		{"http://conda.anaconda.org/c", Policy{}, false},
		{"ftp://127.0.0.1/c", Policy{}, false},
		{"file:///tmp/c", Policy{}, false},
	} {
		got, ok := For(mustURL(t, tc.in))
		if got != tc.want || ok != tc.ok {
			t.Errorf("For(%s) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCheck: what each policy lets a download lead to.
func TestCheck(t *testing.T) {
	public, private, loop := Policy{}, Policy{PrivateHosts: true}, Policy{HTTPLoopback: true, PrivateHosts: true}
	for _, tc := range []struct {
		pol Policy
		u   string
		ok  bool
	}{
		{public, "https://conda.anaconda.org/x", true},
		{public, "https://cdn.example.com/x.conda", true},
		{public, "http://cdn.example.com/x", false},
		{public, "https://127.0.0.1/x", false},
		{public, "https://localhost/x", false},
		{public, "https://169.254.169.254/latest/meta-data", false},
		{public, "https://[fe80::1]/x", false},
		{public, "https://10.0.0.1/x", false},
		{public, "https://127.1/x", false},
		{public, "https://user:pw@conda.anaconda.org/x", false},
		{public, "https://conda.anaconda.org/t/tk-123/conda-forge/x", false},
		{public, "ftp://conda.anaconda.org/x", false},
		{private, "https://10.0.0.6/x", true},
		{private, "https://127.0.0.1/x", true},
		{private, "http://127.0.0.1/x", false},
		{loop, "http://127.0.0.1:2/x", true},
		{loop, "http://10.0.0.1/x", false},
		{loop, "https://8.8.8.8/x", true},
	} {
		err := tc.pol.Check(mustURL(t, tc.u))
		if (err == nil) != tc.ok || err != nil && !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("%+v.Check(%s) = %v", tc.pol, tc.u, err)
		}
	}
}

func TestHasTokenSegment(t *testing.T) {
	for p, want := range map[string]bool{
		"/t/tk-1/conda-forge": true, "/t/x": true, "/t": false, "/t/": false, "/conda-forge/t": false, "/a/t/b/c": true,
	} {
		if got := HasTokenSegment(p); got != want {
			t.Errorf("HasTokenSegment(%q) = %v", p, got)
		}
	}
}

// countingListener accepts and closes TCP connections, counting them: any
// count means a dial reached the loopback service.
func countingListener(t *testing.T) (string, *atomic.Int64) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	hits := &atomic.Int64{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port, hits
}

func get(c *http.Client, raw string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// TestDialChecksResolvedAddress: a public-looking name that resolves to a
// non-public address (or to a mix) is refused before connecting.
func TestDialChecksResolvedAddress(t *testing.T) {
	port, hits := countingListener(t)
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "public.example.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "linklocal.example.test":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		case "mixed.example.test":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	c := Client(nil, Policy{}, lookup)
	for _, host := range []string{"public.example.test", "linklocal.example.test", "mixed.example.test"} {
		if err := get(c, "https://"+host+":"+port+"/x"); !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("%s: err = %v", host, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listener", n)
	}
	// A private mirror's policy connects.
	if err := get(Client(nil, Policy{PrivateHosts: true}, lookup), "https://public.example.test:"+port+"/x"); errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("private mirror refused: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.Load() == 0 {
		t.Error("private mirror did not connect")
	}
}

// Other RoundTrippers are used as given: their owner handles network policy.
type stubRT struct{}

func (stubRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("stub") }

func TestCustomRoundTripperKept(t *testing.T) {
	c := Client(&http.Client{Transport: stubRT{}}, Policy{}, nil)
	if _, ok := c.Transport.(stubRT); !ok {
		t.Errorf("transport %T", c.Transport)
	}
	c = Client(&http.Client{}, Policy{}, nil)
	if g, ok := c.Transport.(*guardedTransport); !ok || g.t == http.DefaultTransport {
		t.Errorf("nil Transport not replaced by a guarded clone: %T", c.Transport)
	}
	c = Client(nil, Policy{}, nil)
	if g, ok := c.Transport.(*guardedTransport); !ok || g.t.Proxy != nil {
		t.Errorf("nil client: not a guarded default transport without proxy: %T", c.Transport)
	}
}

// Nit: a client with a nil Transport uses a DefaultTransport clone without
// its stock dialer, so the guard resolves before connecting (the live test
// TestLiveLoopbackNamesNeverConnect in package repodata checks this end to
// end).
func TestDefaultTransportCloneDropsStockDialer(t *testing.T) {
	dt := defaultTransportClone()
	if dt == nil || dt.DialContext != nil || dt == http.DefaultTransport {
		t.Fatalf("clone %+v", dt)
	}
	if http.DefaultTransport.(*http.Transport).DialContext == nil {
		t.Error("the shared DefaultTransport was modified")
	}
	// Behind the guard, the clone's dials go through the resolving dialer:
	// with a resolver mapping a public name to loopback, nothing connects.
	port, hits := countingListener(t)
	g := Transport(dt, Policy{}, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	gt := g.(*guardedTransport)
	gt.proxy = nil // ignore any proxy configured in the environment
	gt.t.Proxy = nil
	if err := get(Client(&http.Client{Transport: g}, Policy{}, nil), "https://public.example.test:"+port+"/x"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("err = %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listener", n)
	}
}

// TestRedirectChecks: a redirect must pass the policy and never downgrade
// to http; the caller's CheckRedirect runs after those checks.
func TestRedirectChecks(t *testing.T) {
	calls := 0
	inner := func(*http.Request, []*http.Request) error { calls++; return nil }
	for _, tc := range []struct {
		pol      Policy
		from, to string
		ok       bool
	}{
		{Policy{}, "https://conda.anaconda.org/x", "https://cdn.example.com/x", true},
		{Policy{}, "https://conda.anaconda.org/x", "https://127.0.0.1/x", false},
		{Policy{}, "https://conda.anaconda.org/x", "https://169.254.169.254/x", false},
		{Policy{}, "https://conda.anaconda.org/x", "https://[fe80::1]/x", false},
		{Policy{HTTPLoopback: true, PrivateHosts: true}, "https://127.0.0.1/x", "http://127.0.0.1/x", false},
	} {
		c := Client(&http.Client{CheckRedirect: inner}, tc.pol, nil)
		from, _ := http.NewRequest(http.MethodGet, tc.from, nil)
		to, _ := http.NewRequest(http.MethodGet, tc.to, nil)
		before := calls
		err := c.CheckRedirect(to, []*http.Request{from})
		if (err == nil) != tc.ok {
			t.Errorf("%s -> %s: %v", tc.from, tc.to, err)
		}
		if !tc.ok && calls != before {
			t.Errorf("%s -> %s: the caller's CheckRedirect ran for a refused redirect", tc.from, tc.to)
		}
	}
	// Without a caller's CheckRedirect, 10 redirects are the limit.
	c := Client(nil, Policy{}, nil)
	to, _ := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = to
	}
	if err := c.CheckRedirect(to, via); err == nil {
		t.Error("an 11th redirect was allowed")
	}
}

// FuzzCheck: URLs never panic the policy; a URL a public policy accepts is
// https on a host in canonical form that is neither a loopback name nor a
// non-public address literal, without credentials; one the policy of For
// accepts was usable to For.
func FuzzCheck(f *testing.F) {
	for _, s := range []string{
		"https://conda.anaconda.org/conda-forge", "http://127.0.0.1:8000/c", "https://127.1/x", "https://[::ffff:127.0.0.1]/x",
		"https://ｌｏｃａｌｈｏｓｔ/x", "https://user:pw@example.com/x", "https://example.com/t/tk/x", "https://10.0.0.1/x",
		"https://example.com./x", "https://0x7f000001/x", "https://[fe80::1%25eth0]/x", "ftp://example.com/x", "https:///x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		if err := (Policy{}).Check(u); err == nil {
			h := u.Hostname()
			if !strings.EqualFold(u.Scheme, "https") || u.User != nil || HasTokenSegment(u.Path) || isLocalHost(h) || canonicalHost(h) != nil {
				t.Fatalf("public policy accepted %q", raw)
			}
			if a, err := netip.ParseAddr(h); err == nil && !isPublicAddr(a) {
				t.Fatalf("public policy accepted the address %s", a)
			}
		} else if !errors.Is(err, ErrUnsupportedURL) {
			t.Fatalf("%q: %v", raw, err)
		}
		pol, ok := For(u)
		if !ok && (pol != Policy{}) {
			t.Fatalf("For(%q) = %+v, false", raw, pol)
		}
		if pol.HTTPLoopback && !pol.PrivateHosts {
			t.Fatalf("For(%q) = %+v", raw, pol)
		}
	})
}
