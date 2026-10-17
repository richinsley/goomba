package repodata

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

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
	} {
		if isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s is public", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.0", "172.32.0.1", "2606:4700::1111", "::ffff:8.8.8.8"} {
		if !isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s is not public", s)
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

func publicFetcher(t *testing.T, client *http.Client, lookup lookupFunc) *fetcher {
	t.Helper()
	s, err := Options{CacheDir: t.TempDir(), Platform: "noarch"}.settings()
	if err != nil {
		t.Fatal(err)
	}
	s.lookup = lookup
	_, pol, err := canonicalChannel(cfChannel)
	if err != nil {
		t.Fatal(err)
	}
	return newFetcher(s, client, pol)
}

func get(f *fetcher, raw string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	_, _, err = f.do(req, 1<<20)
	return err
}

// SF2: every spelling of a loopback or non-public host that a resolver or
// dialer might accept is refused for a public channel, and nothing reaches
// the loopback listener.
func TestPublicChannelNeverReachesLoopback(t *testing.T) {
	port, hits := countingListener(t)
	f := publicFetcher(t, nil, nil)
	for _, host := range []string{
		"127.0.0.1", "localhost", "localhost.", "LOCALHOST.", "127.0.0.1.", "127.1", "2130706433", "0x7f000001",
		"0177.0.0.1", "0x7f.0.0.1", "[::ffff:127.0.0.1]", "[::ffff:7f00:1]", "[0:0:0:0:0:ffff:7f00:1]",
		"ｌｏｃａｌｈｏｓｔ", "１２７．０．０．１", "127。0。0。1", "ⓛⓞⓒⓐⓛⓗⓞⓢⓣ", "foo.localhost",
		"localhost.localdomain", "ip6-localhost", "ip6-loopback", "[::127.0.0.1]", "[::1]",
		"100.100.100.200", "[fec0::1]", "[64:ff9b::7f00:1]", "0.0.0.1", "[::]", "0.0.0.0",
	} {
		raw := "https://" + host + ":" + port + "/admin/delete?id=1"
		if u, err := url.Parse(raw); err == nil {
			if err := f.pol.check(u); !errors.Is(err, ErrUnsupportedURL) {
				t.Errorf("%s: policy check = %v", host, err)
			}
		}
		if err := get(f, raw); err == nil {
			t.Errorf("%s: request succeeded", host)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listener", n)
	}
}

// SF2: a public-looking name that resolves to loopback is refused at dial
// time; a private mirror channel may connect.
func TestDialGuardChecksResolvedAddress(t *testing.T) {
	port, hits := countingListener(t)
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "public.example.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "mixed.example.test":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	f := publicFetcher(t, nil, lookup)
	for _, host := range []string{"public.example.test", "mixed.example.test"} {
		err := get(f, "https://"+host+":"+port+"/x")
		if !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("%s: err = %v", host, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listener", n)
	}
	// The private-mirror exception holds at dial time too.
	s, _ := Options{CacheDir: t.TempDir(), Platform: "noarch"}.settings()
	s.lookup = lookup
	mirror := newFetcher(s, nil, urlPolicy{privateHosts: true})
	if err := get(mirror, "https://public.example.test:"+port+"/x"); errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("private mirror refused: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.Load() == 0 {
		t.Error("private mirror did not connect")
	}
}

// tlsServer serves h over TLS on 127.0.0.1 with a certificate for names,
// and returns a transport trusting it.
func tlsServer(t *testing.T, h http.Handler, names ...string) (*httptest.Server, *http.Transport) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: names, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return srv, &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
}

// SF2: a caller's *http.Transport is guarded too: a dev TLS server on
// localhost is unreachable from a public channel, through the default dial
// and through a caller-supplied DialContext.
func TestCallerTransportIsGuarded(t *testing.T) {
	var served atomic.Int64
	srv, tr := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		_, _ = w.Write([]byte("secret"))
	}), "localhost", "public.example.test")
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	f := publicFetcher(t, &http.Client{Transport: tr}, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	for _, host := range []string{"localhost", "localhost.", "ｌｏｃａｌｈｏｓｔ", "127.1", "public.example.test"} {
		if err := get(f, "https://"+host+":"+port+"/admin/delete?id=1"); err == nil {
			t.Errorf("%s: request served", host)
		}
	}
	// A caller DialContext that always connects to the local server: the
	// connection is checked by its remote address and dropped.
	custom := tr.Clone()
	custom.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	f2 := publicFetcher(t, &http.Client{Transport: custom}, nil)
	if err := get(f2, "https://public.example.test:"+port+"/admin"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("caller dialer: err = %v", err)
	}
	if n := served.Load(); n != 0 {
		t.Errorf("the local server served %d requests", n)
	}
	// Sanity: a private-mirror policy with the same transport gets through.
	s, _ := Options{CacheDir: t.TempDir(), Platform: "noarch"}.settings()
	mirror := newFetcher(s, &http.Client{Transport: tr}, urlPolicy{privateHosts: true})
	if err := get(mirror, "https://localhost:"+port+"/ok"); err != nil || served.Load() != 1 {
		t.Errorf("private mirror: %v (%d served)", err, served.Load())
	}
}

// SF2: a proxy may sit on a private network: dials to exactly the proxy's
// address are exempt, while the target host is still checked.
func TestProxyAddressExempt(t *testing.T) {
	backend, tr := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("via proxy"))
	}), "public.example.test")
	var tunnels atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		tunnels.Add(1)
		up, err := net.Dial("tcp", backend.Listener.Addr().String()) // the proxy resolves the target
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			up.Close()
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { _, _ = io.Copy(up, conn); up.Close() }()
		_, _ = io.Copy(conn, up)
		conn.Close()
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	tr.Proxy = http.ProxyURL(proxyURL)
	f := publicFetcher(t, &http.Client{Transport: tr}, func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no direct resolution through a proxy")
	})
	req, _ := http.NewRequestWithContext(ctxT(t), http.MethodGet, "https://public.example.test/x", nil)
	resp, body, err := f.do(req, 1<<20)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "via proxy" || tunnels.Load() != 1 {
		t.Fatalf("through the proxy: %v %q (%d tunnels)", err, body, tunnels.Load())
	}
	// The target's host string is still checked.
	if err := get(f, "https://127.0.0.1/x"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("loopback target through the proxy: %v", err)
	}
	// Without the proxy, the same transport cannot reach the loopback host.
	tr.Proxy = nil
	direct := publicFetcher(t, &http.Client{Transport: tr}, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	if err := get(direct, "https://public.example.test:"+proxyURL.Port()+"/x"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("proxy address dialled directly: %v", err)
	}
}

// Other RoundTrippers are used as given: their owner handles network policy.
type stubRT struct{}

func (stubRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("stub") }

func TestCustomRoundTripperKept(t *testing.T) {
	c := safeClient(&http.Client{Transport: stubRT{}}, urlPolicy{}, nil)
	if _, ok := c.Transport.(stubRT); !ok {
		t.Errorf("transport %T", c.Transport)
	}
	c = safeClient(&http.Client{}, urlPolicy{}, nil)
	if g, ok := c.Transport.(*guardedTransport); !ok || g.t == http.DefaultTransport {
		t.Errorf("nil Transport not replaced by a guarded clone: %T", c.Transport)
	}
}

// Nit: a client with a nil Transport uses a DefaultTransport clone without
// its stock dialer, so the guard resolves before connecting (the live test
// TestLiveLoopbackNamesNeverConnect checks this end to end).
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
	g := guardTransport(dt, urlPolicy{}, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	gt := g.(*guardedTransport)
	gt.proxy = nil // ignore any proxy configured in the environment
	gt.t.Proxy = nil
	f := publicFetcher(t, &http.Client{Transport: g}, nil)
	if err := get(f, "https://public.example.test:"+port+"/x"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("err = %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listener", n)
	}
}

// Nit: the proxy exemption covers only the dial of a request that is being
// proxied. After a proxied request, a direct request from a public channel
// to the proxy's own host:port is still refused (the reviewer's repro).
func TestProxyExemptionIsPerRequest(t *testing.T) {
	proxyPort, proxyHits := countingListener(t)
	proxyHost := "proxy.example.test:" + proxyPort
	tr := &http.Transport{Proxy: func(r *http.Request) (*url.URL, error) {
		if r.URL.Hostname() == "only-this.example.test" {
			return url.Parse("http://" + proxyHost)
		}
		return nil, nil // everything else direct, as with NO_PROXY
	}}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "proxy.example.test" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	f := publicFetcher(t, &http.Client{Transport: tr}, lookup)
	for round := 1; round <= 2; round++ {
		before := proxyHits.Load()
		_ = get(f, "https://only-this.example.test/x") // the listener hangs up; the dial is what counts
		time.Sleep(30 * time.Millisecond)
		if proxyHits.Load() == before {
			t.Fatalf("round %d: the proxied request did not reach the proxy", round)
		}
		before = proxyHits.Load()
		err := get(f, "https://"+proxyHost+"/admin")
		time.Sleep(30 * time.Millisecond)
		if !errors.Is(err, ErrUnsupportedURL) || proxyHits.Load() != before {
			t.Errorf("round %d: direct request to the proxy's address: %v, %d connections", round, err, proxyHits.Load()-before)
		}
	}
}
