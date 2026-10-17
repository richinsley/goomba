package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richinsley/goomba"
)

// proxyClient returns the HTTP client for downloads: nil (fetch's default,
// which uses no proxy) unless HTTPS_PROXY or https_proxy is set, and then a
// client whose transport takes its proxy from net/http's
// ProxyFromEnvironment, which for the https URLs goomba fetches uses only
// HTTPS_PROXY and NO_PROXY (and their lower-case forms), with Go's own
// semantics for both. Its timeouts are fetch's.
//
// ProxyFromEnvironment silently ignores a proxy setting it cannot parse, and
// goomba would then connect directly; so the setting Go will use (the first
// non-empty of HTTPS_PROXY and https_proxy) is checked first
// (checkProxyURL), and one that does not parse cleanly is an error, quoted
// with its credentials hidden. What Go accepts is never refused: a proxy URL
// with a path, and NO_PROXY entries that can match nothing, draw a warning
// on warn.
func proxyClient(getenv func(string) string, warn io.Writer) (*http.Client, error) {
	name, v := firstSet(getenv, "HTTPS_PROXY", "https_proxy")
	if v == "" {
		return nil, nil
	}
	note, err := checkProxyURL(v)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL in %s: %q: %w", name, redactProxy(v), err)
	}
	if note != "" {
		fmt.Fprintf(warn, "goomba: warning: %s %q: %s\n", name, redactProxy(v), note)
	}
	npName, np := firstSet(getenv, "NO_PROXY", "no_proxy")
	for _, note := range checkNoProxy(np) {
		fmt.Fprintf(warn, "goomba: warning: %s: %s\n", npName, note)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}}, nil
}

// syncProgress serialises calls to f: InstallLock reports progress from
// several goroutines.
func syncProgress(f func(goomba.Event)) func(goomba.Event) {
	var mu sync.Mutex
	return func(e goomba.Event) {
		mu.Lock()
		defer mu.Unlock()
		f(e)
	}
}

// firstSet returns the first of the named variables that is set to a
// non-empty value, as net/http picks between HTTPS_PROXY and https_proxy.
func firstSet(getenv func(string) string, names ...string) (string, string) {
	for _, n := range names {
		if v := getenv(n); v != "" {
			return n, v
		}
	}
	return "", ""
}

// errProxyURL is the error for a proxy setting that does not parse. It says
// no more: url.Parse's own messages quote pieces of the value, which may be
// pieces of a password.
var errProxyURL = errors.New("not a URL with a scheme and host (a password must have '/', '?', '#', '@' and '%' %-encoded)")

// checkProxyURL accepts what golang.org/x/net/http/httpproxy (and so
// ProxyFromEnvironment) parses into a usable proxy: a URL with scheme
// http, https, socks5 or socks5h and a host, or, as httpproxy reads it, a
// bare host[:port] taken as http. It refuses what httpproxy would drop or
// misread: an unparsable URL (a bad %-escape, a space in the host), a value
// without a host, an unsupported scheme or a bad port. A path, query or
// fragment, which Go ignores, comes back as a note for a warning.
func checkProxyURL(v string) (note string, err error) {
	raw := v
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || strings.ContainsAny(u.Hostname(), " \t") {
		return "", errProxyURL
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("bad port %q", p)
		}
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "its path, query or fragment is ignored", nil
	}
	return "", nil
}

// checkNoProxy returns notes on NO_PROXY entries that net/http accepts but
// that can match no host: one holding white space, a scheme
// ("https://host") or credentials. It never refuses a list.
func checkNoProxy(v string) []string {
	var notes []string
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if strings.ContainsAny(e, " \t\r\n@") || strings.Contains(e, "://") {
			notes = append(notes, fmt.Sprintf("entry %q matches no host", redactProxy(e)))
		}
	}
	return notes
}

// redactProxy hides the user information of a proxy setting: everything
// between "://" (or the start, for a bare host) and the last '@'.
func redactProxy(v string) string {
	start := 0
	if i := strings.Index(v, "://"); i >= 0 {
		start = i + 3
	}
	if at := strings.LastIndexByte(v[start:], '@'); at >= 0 {
		return v[:start] + "***" + v[start+at:]
	}
	return v
}
