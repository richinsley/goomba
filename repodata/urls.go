// URL resolution is ported from libmamba src/core/shards.cpp (Shards::base_url,
// Shards::shards_base_url) and src/core/shard_types.cpp (to_package_info)
// (BSD-3-Clause, see NOTICE), cross-checked with rattler
// crates/rattler_repodata_gateway/src/gateway/sharded_subdir.

package repodata

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/richinsley/goomba/internal/netguard"
)

// urlPolicy decides which URLs a channel may lead to: the channel URL
// itself, base URLs from its indexes and redirect targets. It is the
// channel's internal/netguard policy (see that package for the rules):
// a channel on a public host may only lead to https URLs on public hosts,
// a channel on a loopback or private host may also lead to such hosts over
// https, and plain http is allowed only from an http channel on a loopback
// host, to loopback hosts. The policy is enforced on the URL's host string
// and on the address each connection goes to.
type urlPolicy struct {
	httpLoopback bool // the channel is http on a loopback host
	privateHosts bool // the channel is on a loopback or private host
}

// guard returns the netguard form of p.
func (p urlPolicy) guard() netguard.Policy {
	return netguard.Policy{HTTPLoopback: p.httpLoopback, PrivateHosts: p.privateHosts}
}

// check enforces the policy on u. Error messages may echo u: URLs with
// credentials are refused before anything is fetched.
func (p urlPolicy) check(u *url.URL) error { return p.guard().Check(u) }

// lookupFunc resolves a host name to addresses (nil: the system resolver).
type lookupFunc = netguard.LookupFunc

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
	if u.User != nil || netguard.HasTokenSegment(u.Path) {
		return nil, urlPolicy{}, errChannelCreds
	}
	gp, ok := netguard.For(u)
	if !ok {
		return nil, urlPolicy{}, errChannelScheme
	}
	pol := urlPolicy{httpLoopback: gp.HTTPLoopback, privateHosts: gp.PrivateHosts}
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

// safeClient copies c (or builds a default client) with the channel's
// policy applied to redirects and to connections (netguard.Client): a
// redirect to a URL the policy refuses, or from https to http, fails the
// request, and goomba's default transport, a caller's *http.Transport and
// the default transport behind a client with a nil Transport only connect
// to allowed addresses. Any other http.RoundTripper is used as is: its
// owner is responsible for network policy.
func safeClient(c *http.Client, pol urlPolicy, lookup lookupFunc) *http.Client {
	return netguard.Client(c, pol.guard(), lookup)
}
