package repodata

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// This file enforces the URL policy where it matters: on the address a
// connection actually goes to. Host strings are checked first (canonical
// form only), but the dialer resolves names itself, so a public-looking
// name that resolves to 127.0.0.1, or a host spelling the resolver
// normalizes differently, is caught at dial time.

// nonPublicPrefixes are address ranges a public channel may not reach:
// "this network", private (RFC 1918, ULA), shared CGNAT space, loopback,
// link-local, site-local, IETF protocol assignments, benchmarking,
// multicast, reserved, IPv4-compatible and NAT64 forms.
func nonPublicPrefixes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/96"), // unspecified, loopback, IPv4-compatible
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("fec0::/10"),
		netip.MustParsePrefix("ff00::/8"),
	}
}

// isPublicAddr reports whether a is a globally routable address a public
// channel may reach. IPv4-mapped IPv6 addresses are judged as IPv4.
func isPublicAddr(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	a = a.Unmap()
	for _, p := range nonPublicPrefixes() {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// loopbackNames are host names that conventionally resolve to loopback.
func isLoopbackName(h string) bool {
	h = strings.ToLower(h)
	switch h {
	case "localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback":
		return true
	}
	return strings.HasSuffix(h, ".localhost")
}

// canonicalHost checks a URL host (without port or brackets) is spelled in
// the one form every resolver agrees on: ASCII, no trailing dot, IPv6
// without a zone, and IPv4 only as canonical dotted decimal (no "127.1",
// "2130706433", "0x7f000001" or "0177.0.0.1"), so the checks on the string
// see what the dialer will see.
func canonicalHost(h string) error {
	if h == "" {
		return fmt.Errorf("%w: missing host", ErrUnsupportedURL)
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if c >= 0x80 || c <= ' ' || c == 0x7f {
			return fmt.Errorf("%w: host %q is not plain ASCII", ErrUnsupportedURL, h)
		}
	}
	if strings.HasSuffix(h, ".") {
		return fmt.Errorf("%w: host %q ends with a dot", ErrUnsupportedURL, h)
	}
	if strings.Contains(h, ":") { // IPv6 literal
		a, err := netip.ParseAddr(h)
		if err != nil || a.Zone() != "" {
			return fmt.Errorf("%w: host %q is not a plain IPv6 address", ErrUnsupportedURL, h)
		}
		return nil
	}
	// A name whose last label is numeric (or hexadecimal) is an IPv4
	// address to some resolver; accept it only in canonical form.
	last := h[strings.LastIndexByte(h, '.')+1:]
	if allDigits(last) || strings.HasPrefix(strings.ToLower(last), "0x") {
		if a, err := netip.ParseAddr(h); err != nil || !a.Is4() {
			return fmt.Errorf("%w: host %q is not a canonical IPv4 address", ErrUnsupportedURL, h)
		}
	}
	return nil
}

// isLocalHost reports hosts a public channel may not name: loopback names
// and non-public address literals.
func isLocalHost(h string) bool {
	if isLoopbackName(h) {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && !isPublicAddr(a)
}

func isLoopbackHost(h string) bool {
	if isLoopbackName(h) {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}

// allowAddr reports whether the policy lets a connection go to a.
func (p urlPolicy) allowAddr(a netip.Addr) bool {
	return p.privateHosts || isPublicAddr(a)
}

// lookupFunc resolves a host name to addresses.
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dialGuard enforces the policy at connect time.
type dialGuard struct {
	pol     urlPolicy
	lookup  lookupFunc
	plain   *net.Dialer // for the proxy of a proxied request
	guarded *net.Dialer // with a Control hook re-checking the address
}

// proxyDialKey marks a request context with the address of the proxy the
// request goes through (see guardedTransport).
type proxyDialKey struct{}

func newDialGuard(pol urlPolicy, lookup lookupFunc) *dialGuard {
	g := &dialGuard{pol: pol, lookup: lookup}
	if g.lookup == nil {
		g.lookup = defaultLookup
	}
	g.plain = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	g.guarded = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: cannot check address %q", ErrUnsupportedURL, address)
			}
			if !g.pol.allowAddr(ap.Addr()) {
				return fmt.Errorf("%w: connection to non-public address %s", ErrUnsupportedURL, ap.Addr())
			}
			return nil
		}}
	return g
}

// proxiedTo reports whether ctx belongs to a request being proxied
// through exactly addr: only that dial is exempt from the address check (a
// proxy may legitimately sit on a private network; the target's host is
// still checked by the URL policy).
func proxiedTo(ctx context.Context, addr string) bool {
	v, _ := ctx.Value(proxyDialKey{}).(string)
	return v != "" && v == addr
}

// proxyAddr is the address net/http dials for a proxy URL.
func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// dial resolves addr itself, refuses it if any address is outside the
// policy, and connects to an allowed address with the guarded dialer.
func (g *dialGuard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else if addrs, err = g.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("repodata: no address for %s", host)
	}
	dialer := g.plain
	if !proxiedTo(ctx, addr) {
		dialer = g.guarded
		for _, a := range addrs {
			if !g.pol.allowAddr(a) {
				return nil, fmt.Errorf("%w: %s resolves to non-public address %s", ErrUnsupportedURL, host, a.Unmap())
			}
		}
	}
	var lastErr error
	for _, a := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// checkConn verifies the remote address of a connection made by a dialer
// the caller supplied (which goomba cannot resolve for), closing it if the
// policy refuses it. No request bytes have been sent at that point.
func (g *dialGuard) checkConn(ctx context.Context, addr string, conn net.Conn) (net.Conn, error) {
	if proxiedTo(ctx, addr) {
		return conn, nil
	}
	ap, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil || !g.pol.allowAddr(ap.Addr()) {
		conn.Close()
		return nil, fmt.Errorf("%w: connection to non-public address %s", ErrUnsupportedURL, conn.RemoteAddr())
	}
	return conn, nil
}

func (g *dialGuard) wrap(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return g.checkConn(ctx, addr, conn)
	}
}

// guardedTransport marks each request that its transport will send through
// a proxy with that proxy's address, so the dialer exempts exactly that
// dial. A direct request to the same host:port gets no mark and is
// checked; net/http pools proxied and direct connections apart.
type guardedTransport struct {
	t     *http.Transport
	proxy func(*http.Request) (*url.URL, error) // t.Proxy
}

// RoundTrip implements http.RoundTripper.
func (g *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if g.proxy != nil {
		u, err := g.proxy(req)
		if err != nil {
			return nil, err
		}
		if u != nil {
			req = req.WithContext(context.WithValue(req.Context(), proxyDialKey{}, proxyAddr(u)))
		}
	}
	return g.t.RoundTrip(req)
}

// CloseIdleConnections closes the transport's idle connections.
func (g *guardedTransport) CloseIdleConnections() { g.t.CloseIdleConnections() }

// guardTransport returns a RoundTripper over a clone of t whose
// connections obey the policy. The clone's own dialing is replaced by the
// guard (pre-resolving, then checking at connect); a caller-supplied
// DialContext or DialTLSContext is kept and its connections are checked by
// remote address before use.
func guardTransport(t *http.Transport, pol urlPolicy, lookup lookupFunc) http.RoundTripper {
	t = t.Clone()
	g := newDialGuard(pol, lookup)
	switch {
	case t.DialContext != nil:
		t.DialContext = g.wrap(t.DialContext)
	case t.Dial != nil: //nolint:staticcheck // honour a caller's legacy Dial
		dial := t.Dial //nolint:staticcheck
		t.Dial = nil   //nolint:staticcheck
		t.DialContext = g.wrap(func(_ context.Context, network, addr string) (net.Conn, error) { return dial(network, addr) })
	default:
		t.DialContext = g.dial
	}
	switch {
	case t.DialTLSContext != nil:
		t.DialTLSContext = g.wrap(t.DialTLSContext)
		t.DialTLS = nil //nolint:staticcheck // DialTLSContext takes precedence anyway
	case t.DialTLS != nil: //nolint:staticcheck
		dial := t.DialTLS //nolint:staticcheck
		t.DialTLS = nil   //nolint:staticcheck
		t.DialTLSContext = g.wrap(func(_ context.Context, network, addr string) (net.Conn, error) { return dial(network, addr) })
	}
	return &guardedTransport{t: t, proxy: t.Proxy}
}
