// URL resolution is ported from libmamba src/core/shards.cpp (Shards::base_url,
// Shards::shards_base_url) and src/core/shard_types.cpp (to_package_info)
// (BSD-3-Clause, see NOTICE), cross-checked with rattler
// crates/rattler_repodata_gateway/src/gateway/sharded_subdir.

package repodata

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// urlPolicy decides which URLs a channel may lead to: the channel URL
// itself, base URLs from its indexes and redirect targets.
//
// A channel on a public host may only lead to https URLs on public hosts:
// not a loopback, private, shared (CGNAT), link-local, site-local,
// multicast, reserved or NAT64 address, nor a localhost name, so a hostile
// index or redirect cannot aim requests at local services. A channel on
// such a host itself (an internal mirror) may lead to them over https.
// Plain http is allowed only for a channel that is itself http on a
// loopback host (test servers), and then only to loopback hosts. A
// redirect never goes from https to http. URLs with user info or an
// anaconda.org /t/<token>/ segment are refused everywhere.
//
// The policy is enforced twice: on the URL's host string, which must be in
// canonical form (canonicalHost), and on the address each connection goes
// to (guardTransport), so a name that resolves to a private address is
// caught too.
type urlPolicy struct {
	httpLoopback bool // the channel is http on a loopback host
	privateHosts bool // the channel is on a loopback or private host
}

// check enforces the policy on u. Error messages may echo u: URLs with
// credentials are refused before anything is fetched.
func (p urlPolicy) check(u *url.URL) error {
	if u.User != nil {
		return fmt.Errorf("%w: URL with user info", ErrUnsupportedURL)
	}
	if hasTokenSegment(u.Path) {
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
		if !p.privateHosts && isLocalHost(host) {
			return fmt.Errorf("%w: %s: loopback or private address for a public channel", ErrUnsupportedURL, u)
		}
	case "http":
		if !p.httpLoopback || !isLoopbackHost(host) {
			return fmt.Errorf("%w: %s: plain http is only allowed from a loopback http channel to loopback hosts", ErrUnsupportedURL, u)
		}
	default:
		return fmt.Errorf("%w: %s: scheme must be https", ErrUnsupportedURL, u)
	}
	return nil
}

// redact returns u as a string for messages, with any password masked.
// Channel URLs with credentials are refused, so this is defense in depth.
func redact(u *url.URL) string {
	return u.Redacted()
}

// redactString redacts a URL string, if it parses.
func redactString(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "<invalid URL>"
	}
	return redact(u)
}

// Fixed channel-URL errors. They never echo the URL, which may hold a
// secret.
var (
	errChannelParse  = fmt.Errorf("repodata: channel URL: %w: cannot parse", ErrUnsupportedURL)
	errChannelCreds  = fmt.Errorf("repodata: channel URL: %w: credentials (user info or a /t/<token>/ segment) are not allowed", ErrUnsupportedURL)
	errChannelScheme = fmt.Errorf("repodata: channel URL: %w: scheme must be https (or http to a loopback host)", ErrUnsupportedURL)
	errChannelHost   = fmt.Errorf("repodata: channel URL: %w: missing host", ErrUnsupportedURL)
	errChannelQuery  = fmt.Errorf("repodata: channel URL: %w: query or fragment not allowed", ErrUnsupportedURL)
	errChannelDots   = fmt.Errorf("repodata: channel URL: %w: dot segment in path", ErrUnsupportedURL)
)

// canonicalChannel validates a channel URL and returns it without trailing
// slashes (e.g. "https://conda.anaconda.org/conda-forge") together with
// the URL policy it implies. Channel URLs carrying credentials (user info
// or an anaconda.org /t/<token>/ segment) are refused, with an error that
// does not repeat the URL.
func canonicalChannel(channelURL string) (*url.URL, urlPolicy, error) {
	u, err := url.Parse(channelURL)
	if err != nil {
		return nil, urlPolicy{}, errChannelParse
	}
	if u.User != nil || hasTokenSegment(u.Path) {
		return nil, urlPolicy{}, errChannelCreds
	}
	pol := urlPolicy{privateHosts: isLocalHost(u.Hostname())}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return nil, urlPolicy{}, errChannelScheme
		}
		pol.httpLoopback = true
	default:
		return nil, urlPolicy{}, errChannelScheme
	}
	if u.Host == "" || u.Opaque != "" {
		return nil, urlPolicy{}, errChannelHost
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, urlPolicy{}, errChannelQuery
	}
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	c.Host = strings.ToLower(c.Host)
	c.Path = strings.TrimRight(c.Path, "/")
	c.RawPath = ""
	for _, seg := range strings.Split(c.Path, "/") {
		if seg == ".." || seg == "." {
			return nil, urlPolicy{}, errChannelDots
		}
	}
	if err := pol.check(&c); err != nil {
		return nil, urlPolicy{}, errChannelScheme
	}
	return &c, pol, nil
}

// hasTokenSegment reports an anaconda.org token path: a "t" segment
// followed by another segment.
func hasTokenSegment(p string) bool {
	segs := strings.Split(p, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "t" && segs[i+1] != "" {
			return true
		}
	}
	return false
}

// subdirURL returns "<channel>/<subdir>/" for a canonical channel.
func subdirURL(channel *url.URL, subdir string) *url.URL {
	u := *channel
	u.Path = channel.Path + "/" + subdir + "/"
	u.RawPath = ""
	return &u
}

// resolveBase resolves a base_url or shards_base_url found in an index
// against the index file URL and returns it with a trailing slash.
//
// An empty value, ".", "./" and "/" all mean the index's own directory: the
// first three by RFC 3986, "/" as a sentinel some shard indices use
// (libmamba Shards::base_url). Other values follow RFC 3986: absolute URLs
// are used as is and "/x" is relative to the host root. The result must pass
// the channel's URL policy and carry no query or fragment.
func resolveBase(indexURL *url.URL, ref string, pol urlPolicy) (*url.URL, error) {
	var out *url.URL
	switch ref {
	case "", ".", "./", "/":
		d := *indexURL
		if i := strings.LastIndexByte(d.Path, '/'); i >= 0 {
			d.Path = d.Path[:i+1]
		} else {
			d.Path = "/"
		}
		d.RawPath = ""
		d.RawQuery = ""
		d.Fragment = ""
		out = &d
	default:
		r, err := url.Parse(ref)
		if err != nil {
			return nil, fmt.Errorf("%w: base URL does not parse", ErrUnsupportedURL)
		}
		if r.Scheme == "" && r.Host == "" && strings.HasPrefix(ref, "//") {
			return nil, fmt.Errorf("%w: base URL %q: empty host", ErrUnsupportedURL, ref)
		}
		out = indexURL.ResolveReference(r)
	}
	if err := pol.check(out); err != nil {
		return nil, err
	}
	if out.RawQuery != "" || out.ForceQuery || out.Fragment != "" {
		return nil, fmt.Errorf("%w: base URL %s: query or fragment not allowed", ErrUnsupportedURL, redact(out))
	}
	if !strings.HasSuffix(out.Path, "/") {
		o := *out
		o.Path += "/"
		o.RawPath = ""
		out = &o
	}
	return out, nil
}

// joinFile appends an escaped file name to a base URL ending in '/'.
// Mirrors libmamba to_package_info: url_concat(base_url, "/", encode_percent(filename)).
func joinFile(base *url.URL, fn string) string {
	return base.String() + url.PathEscape(fn)
}

// safeClient copies c (or builds a default client) and wraps CheckRedirect
// so that a redirect to a URL the policy refuses, or from https to http,
// fails the request.
//
// The transport is guarded so that connections obey the policy too: goomba's
// default transport, a caller's *http.Transport (cloned) and the default
// transport behind a client with a nil Transport all are. Any other
// http.RoundTripper is used as is: its owner is responsible for network
// policy.
func safeClient(c *http.Client, pol urlPolicy, lookup lookupFunc) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	}
	switch t := out.Transport.(type) {
	case nil:
		if c == nil {
			out.Transport = guardTransport(defaultTransport(), pol, lookup)
		} else if dt := defaultTransportClone(); dt != nil {
			out.Transport = guardTransport(dt, pol, lookup)
		}
	case *http.Transport:
		out.Transport = guardTransport(t, pol, lookup)
	}
	inner := out.CheckRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := pol.check(req.URL); err != nil {
			return fmt.Errorf("repodata: refusing redirect: %w", err)
		}
		for _, v := range via {
			if strings.EqualFold(v.URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return fmt.Errorf("repodata: refusing redirect: %w: https to %s", ErrUnsupportedURL, req.URL.Scheme)
			}
		}
		if inner != nil {
			return inner(req, via)
		}
		if len(via) >= 10 {
			return errors.New("repodata: stopped after 10 redirects")
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

func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // library code never reads the environment (HTTPS_PROXY); pass Options.Client to use a proxy
		// No DialContext: guardTransport installs its resolving dialer.
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   DefaultConcurrency,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
