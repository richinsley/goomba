package fetch

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

	"github.com/richinsley/goomba/internal/netguard"
)

// Package downloads follow the same network policy as index downloads
// (internal/netguard): a package URL on a public host never leads to a
// loopback, private or link-local address, through a redirect or a DNS
// answer.

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// loopbackListener accepts and closes TCP connections, counting them: any
// count means a dial reached the loopback service.
func loopbackListener(t *testing.T) (string, *atomic.Int64) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	must(t, err)
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

// TestFetchDNSAnswerToLoopback: a public package URL whose name resolves to
// a loopback or link-local address is refused before any connection, and
// not retried.
func TestFetchDNSAnswerToLoopback(t *testing.T) {
	port, hits := loopbackListener(t)
	lookups := atomic.Int64{}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups.Add(1)
		switch host {
		case "public.example.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "metadata.example.test":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		case "v6.example.test":
			return []netip.Addr{netip.MustParseAddr("fe80::1"), netip.MustParseAddr("::1")}, nil
		}
		return nil, errors.New("no such host")
	}
	for _, host := range []string{"public.example.test", "metadata.example.test", "v6.example.test"} {
		lookups.Store(0)
		c := &Cache{Dir: t.TempDir(), lookup: lookup, backoff0: int64(time.Millisecond)}
		u := "https://" + host + ":" + port + "/conda-forge/noarch/" + wheelFn
		_, err := c.Fetch(context.Background(), u, wheelSHA, wheelSize)
		if !errors.Is(err, ErrInsecureURL) || !errors.Is(err, netguard.ErrUnsupportedURL) {
			t.Errorf("%s: err = %v, want ErrInsecureURL", host, err)
		}
		if n := lookups.Load(); n != 1 {
			t.Errorf("%s: %d lookups: a refused address was retried", host, n)
		}
		assertClean(t, c.Dir)
	}
	time.Sleep(50 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listener", n)
	}
}

// TestFetchRedirectToNonPublic: a public package URL that redirects to a
// loopback, link-local or private address (literally, or by a name that
// resolves to one), or to http, is refused, and nothing reaches the
// loopback service. (A URL naming a non-public host itself is an internal
// mirror and may reach such hosts: the other tests fetch from
// https://127.0.0.1.)
func TestFetchRedirectToNonPublic(t *testing.T) {
	port, hits := loopbackListener(t)
	targets := map[string]string{
		"/loopback/" + wheelFn:   "https://127.0.0.1:" + port + "/x/" + wheelFn,
		"/localhost/" + wheelFn:  "https://localhost:" + port + "/x/" + wheelFn,
		"/linklocal/" + wheelFn:  "https://169.254.169.254/latest/" + wheelFn,
		"/v6/" + wheelFn:         "https://[fe80::1]/x/" + wheelFn,
		"/private/" + wheelFn:    "https://10.1.2.3/x/" + wheelFn,
		"/rebinding/" + wheelFn:  "https://evil.example.test:" + port + "/x/" + wheelFn,
		"/downgrade/" + wheelFn:  "http://cdn.example.test/x/" + wheelFn,
		"/okredirect/" + wheelFn: "/real/" + wheelFn,
	}
	backend, tr := publicTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to, ok := targets[r.URL.Path]; ok {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		if r.URL.Path == "/real/"+wheelFn {
			_, _ = w.Write(wheelBytes(t))
			return
		}
		http.NotFound(w, r)
	}), "public.example.test")
	// The test cannot reach a public address, so requests to
	// public.example.test go through a proxy on loopback (a dial netguard
	// exempts) that tunnels to the backend; every other host is direct.
	proxyURL := tunnelProxy(t, backend.Listener.Addr().String())
	tr.Proxy = func(r *http.Request) (*url.URL, error) {
		if r.URL.Hostname() == "public.example.test" {
			return proxyURL, nil
		}
		return nil, nil
	}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "evil.example.test" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	newC := func() *Cache {
		return &Cache{Dir: t.TempDir(), Client: &http.Client{Transport: tr}, lookup: lookup, backoff0: int64(time.Millisecond)}
	}
	// Sanity: a redirect within the public host works through the proxy.
	if _, err := newC().Fetch(context.Background(), "https://public.example.test/okredirect/"+wheelFn, wheelSHA, wheelSize); err != nil {
		t.Fatalf("public redirect: %v", err)
	}
	for path := range targets {
		if path == "/okredirect/"+wheelFn {
			continue
		}
		c := newC()
		_, err := c.Fetch(context.Background(), "https://public.example.test"+path, wheelSHA, wheelSize)
		if !errors.Is(err, ErrInsecureURL) {
			t.Errorf("%s: err = %v, want ErrInsecureURL", path, err)
		}
		assertClean(t, c.Dir)
	}
	time.Sleep(50 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d connections reached the loopback listener", n)
	}
}

// publicTLSServer serves h over TLS on 127.0.0.1 with a certificate for
// names, and returns a transport trusting it.
func publicTLSServer(t *testing.T, h http.Handler, names ...string) (*httptest.Server, *http.Transport) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: names, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	cert, _ := x509.ParseCertificate(der)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return srv, &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
}

// tunnelProxy runs an HTTP CONNECT proxy on loopback that tunnels every
// request to backend, and returns its URL.
func tunnelProxy(t *testing.T, backend string) *url.URL {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		up, err := net.Dial("tcp", backend)
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
	u, err := url.Parse(proxy.URL)
	must(t, err)
	return u
}
