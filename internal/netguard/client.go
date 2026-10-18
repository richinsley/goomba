package netguard

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxRedirects is the redirect limit of a Client whose caller set no
// CheckRedirect.
const maxRedirects = 10

// Client copies c (or, for nil, builds a client over DefaultTransport) and
// wraps its CheckRedirect so that a redirect to a URL pol refuses, or from
// https to http, fails the request; c's own CheckRedirect then runs, or a
// limit of 10 redirects.
//
// The transport is guarded too (Transport): goomba's default transport, a
// caller's *http.Transport (cloned) and the default transport behind a
// client with a nil Transport (http.DefaultTransport, cloned without its
// dialer) all are. Any other http.RoundTripper is used as is: its owner is
// responsible for network policy. Requests sent through a proxy have their
// target's name resolved by the proxy, which the guard cannot check (see
// Transport). c is not modified.
func Client(c *http.Client, pol Policy, lookup LookupFunc) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	}
	switch t := out.Transport.(type) {
	case nil:
		if c == nil {
			out.Transport = Transport(DefaultTransport(), pol, lookup)
		} else if dt := defaultTransportClone(); dt != nil {
			out.Transport = Transport(dt, pol, lookup)
		}
	case *http.Transport:
		out.Transport = Transport(t, pol, lookup)
	}
	inner := out.CheckRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := pol.Check(req.URL); err != nil {
			return fmt.Errorf("refusing redirect: %w", err)
		}
		for _, v := range via {
			if strings.EqualFold(v.URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return fmt.Errorf("refusing redirect: %w: https to %s", ErrUnsupportedURL, req.URL.Scheme)
			}
		}
		if inner != nil {
			return inner(req, via)
		}
		if len(via) >= maxRedirects {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &out
}

// defaultTransportClone clones http.DefaultTransport (what a client with
// a nil Transport uses) without its stock dialer, which is not the
// caller's: the guard then resolves and checks before connecting. It
// returns nil if DefaultTransport is not an *http.Transport.
func defaultTransportClone() *http.Transport {
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil
	}
	dt = dt.Clone()
	dt.DialContext = nil
	return dt
}

// DefaultTransport returns a new transport for goomba's downloads: no
// proxy (library code never reads HTTPS_PROXY; callers pass a client to
// use one), TLS 1.2 or later, and connect (set by Transport's dialer),
// TLS-handshake and response-header timeouts. It has no DialContext: wrap
// it with Transport, which installs the resolving, checking dialer.
func DefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}
