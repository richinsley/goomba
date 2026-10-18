// Package netguard is goomba's network policy: which URLs a request may
// lead to and which addresses its connections may reach. Package repodata
// applies it to index downloads and package fetch to package downloads, so
// a hostile index, lockfile, redirect or DNS answer cannot aim goomba's
// requests at local services (SSRF).
//
// A Policy comes from the URL a download starts from (For): a URL on a
// public host may only lead to https URLs on public hosts: not a loopback,
// private, shared (CGNAT), link-local, site-local, multicast, reserved or
// NAT64 address, nor a localhost name. A URL on such a host itself (an
// internal mirror, named by an address literal or a localhost name) may
// lead to them over https. Plain http is allowed only from a URL that is
// itself http on a loopback host (test servers), and then only to loopback
// hosts. A redirect never goes from https to http. URLs with user info or
// an anaconda.org /t/<token>/ segment are refused everywhere.
//
// The policy is enforced twice: on the URL's host string, which must be in
// canonical form, and on the address each connection goes to (Transport),
// so a public-looking name that resolves to 127.0.0.1, or a host spelling a
// resolver normalizes differently, is caught at dial time. A mirror on a
// private network reached by a DNS name therefore needs a caller-supplied
// http.RoundTripper that is not an *http.Transport, which is used as is.
//
// For a request sent through a proxy, the proxy resolves the target's name
// and connects to it: the guard checks the target's host string (a
// non-public address literal or a localhost name is refused for a public
// source) but cannot check the address the proxy's DNS gives that name.
// The dial to the proxy itself is not checked: the proxy is the caller's
// choice and may sit on a private network.
//
// The package keeps no state; its functions are safe for concurrent use,
// and so are the clients and transports they return.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrUnsupportedURL is wrapped by every refusal: a URL, redirect target or
// connection address the policy does not allow, or a URL goomba does not
// use at all (not https, or http to a loopback host; user info; a token
// segment).
var ErrUnsupportedURL = errors.New("unsupported URL")

// Policy decides which URLs a download may lead to: the URL itself, base
// URLs an index names and redirect targets. The zero value is the policy
// of a URL on a public https host. A Policy is a plain comparable value.
type Policy struct {
	// HTTPLoopback permits plain http, to loopback hosts only: the
	// download started from http on a loopback host.
	HTTPLoopback bool
	// PrivateHosts permits loopback and other non-public hosts and
	// addresses: the download started from such a host.
	PrivateHosts bool
}

// For returns the policy a download starting at u implies, and whether u's
// scheme is usable at all: https with a host, or http to a loopback host.
// It does not check the rest of u; use Check.
func For(u *url.URL) (Policy, bool) {
	host := u.Hostname()
	pol := Policy{PrivateHosts: isLocalHost(host)}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return Policy{}, false
		}
		pol.HTTPLoopback = true
	default:
		return Policy{}, false
	}
	return pol, true
}

// Check enforces the policy on u. Error messages may echo u: URLs with
// credentials are refused first, without repeating them.
func (p Policy) Check(u *url.URL) error {
	if u.User != nil {
		return fmt.Errorf("%w: URL with user info", ErrUnsupportedURL)
	}
	if HasTokenSegment(u.Path) {
		return fmt.Errorf("%w: URL with a /t/<token>/ segment", ErrUnsupportedURL)
	}
	if u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("%w: %s: missing host", ErrUnsupportedURL, u)
	}
	host := u.Hostname()
	if err := canonicalHost(host); err != nil {
		return err
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if !p.PrivateHosts && isLocalHost(host) {
			return fmt.Errorf("%w: %s: loopback or private address for a public source", ErrUnsupportedURL, u)
		}
	case "http":
		if !p.HTTPLoopback || !isLoopbackHost(host) {
			return fmt.Errorf("%w: %s: plain http is only allowed from a loopback http source to loopback hosts", ErrUnsupportedURL, u)
		}
	default:
		return fmt.Errorf("%w: %s: scheme must be https", ErrUnsupportedURL, u)
	}
	return nil
}

// AllowAddr reports whether the policy lets a connection go to a.
func (p Policy) AllowAddr(a netip.Addr) bool {
	return p.PrivateHosts || isPublicAddr(a)
}

// HasTokenSegment reports an anaconda.org token path: a "t" segment
// followed by another segment.
func HasTokenSegment(p string) bool {
	segs := strings.Split(p, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "t" && segs[i+1] != "" {
			return true
		}
	}
	return false
}

// nonPublicPrefixes are address ranges a public source may not reach:
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
		netip.MustParsePrefix("192.0.2.0/24"),   // TEST-NET-1
		netip.MustParsePrefix("192.88.99.0/24"), // 6to4 relay anycast
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
		netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/96"), // unspecified, loopback, IPv4-compatible
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("100::/64"),      // discard-only
		netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments: Teredo (tunnels to anywhere), benchmarking, ORCHID
		netip.MustParsePrefix("2001:db8::/32"), // documentation
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("fec0::/10"),
		netip.MustParsePrefix("ff00::/8"),
	}
}

// isPublicAddr reports whether a is a globally routable address a public
// source may reach. IPv4-mapped IPv6 addresses are judged as IPv4, and so
// are the IPv4 addresses embedded in 6to4 (2002::/16) and SIIT
// IPv4-translated (::ffff:0:0/96) addresses; all of Teredo (2001::/32,
// within the special-purpose 2001::/23) is refused.
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
	if a.Is6() {
		b := a.As16()
		switch {
		case b[0] == 0x20 && b[1] == 0x02: // 6to4: 2002:AABB:CCDD::
			return isPublicAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		case b == [16]byte{8: 0xff, 9: 0xff, 12: b[12], 13: b[13], 14: b[14], 15: b[15]}: // ::ffff:0:a.b.c.d
			return isPublicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		}
	}
	return true
}

// isLoopbackName reports host names that conventionally resolve to
// loopback.
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

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isLocalHost reports hosts a public source may not name: loopback names
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

// LookupFunc resolves a host name to addresses. nil selects the system
// resolver; tests substitute their own.
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dialGuard enforces the policy at connect time.
type dialGuard struct {
	pol     Policy
	lookup  LookupFunc
	plain   *net.Dialer // for the proxy of a proxied request
	guarded *net.Dialer // with a Control hook re-checking the address
}

// proxyDialKey marks a request context with the address of the proxy the
// request goes through (see guardedTransport).
type proxyDialKey struct{}

func newDialGuard(pol Policy, lookup LookupFunc) *dialGuard {
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
			if !g.pol.AllowAddr(ap.Addr()) {
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
		return nil, fmt.Errorf("no address for %s", host)
	}
	dialer := g.plain
	if !proxiedTo(ctx, addr) {
		dialer = g.guarded
		for _, a := range addrs {
			if !g.pol.AllowAddr(a) {
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
// the caller supplied (which the guard cannot resolve for), closing it if
// the policy refuses it. No request bytes have been sent at that point.
func (g *dialGuard) checkConn(ctx context.Context, addr string, conn net.Conn) (net.Conn, error) {
	if proxiedTo(ctx, addr) {
		return conn, nil
	}
	ap, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil || !g.pol.AllowAddr(ap.Addr()) {
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

// Transport returns a RoundTripper over a clone of t whose connections obey
// pol. The clone's own dialing is replaced by the guard (resolving the name
// itself and refusing it unless every address is allowed, then checking
// again at connect); a caller-supplied DialContext or DialTLSContext is
// kept and its connections are checked by remote address before use. The
// dial to the proxy of a request the transport sends through a proxy is
// exempt (the proxy may sit on a private network); the target's host
// string is still checked by the URL policy, but the name is resolved by
// the proxy, so the address it stands for is not checked. t is not
// modified.
func Transport(t *http.Transport, pol Policy, lookup LookupFunc) http.RoundTripper {
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
