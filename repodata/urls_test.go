package repodata

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

const testHash = "abababababababababababababababababababababababababababababababab"

func shardURLFor(t *testing.T, indexURL, shardsBaseURL string) string {
	t.Helper()
	base, err := resolveBase(mustURL(t, indexURL), shardsBaseURL, urlPolicy{})
	if err != nil {
		t.Fatalf("resolveBase(%q, %q): %v", indexURL, shardsBaseURL, err)
	}
	var sum [32]byte
	copy(sum[:], unhex(t, testHash))
	return (&shardedSubdir{shardsBase: base}).shardURL(sum)
}

// Cases from libmamba tests/src/core/test_shards.cpp: "Shards URL
// construction", "Shards - shards_base_url edge cases" and "Shards -
// shard_url edge cases for relative_shard_path coverage". libmamba resolves
// against .../repodata.json; goomba against the shard index in the same
// directory. The exact URL is checked in addition to libmamba's assertions.
func TestShardURL(t *testing.T) {
	const anaconda = "https://anaconda.org/conda-forge/linux-64/" + shardIndexFile
	const cf = "https://conda.anaconda.org/conda-forge/linux-64/" + shardIndexFile
	cases := []struct {
		name, index, shardsBase string
		prefix, contains        string
		want                    string
	}{
		{"absolute URL", anaconda, "https://shards.example.com/conda-forge", "https://shards.example.com", "", "https://shards.example.com/conda-forge/"},
		{"relative URL", anaconda, "shards", "", "anaconda.org", "https://anaconda.org/conda-forge/linux-64/shards/"},
		{"different host", anaconda, "https://different-host.com/shards", "https://different-host.com", "", "https://different-host.com/shards/"},
		{"empty shards_base_url", anaconda, "", "https://anaconda.org/conda-forge/linux-64/", "", "https://anaconda.org/conda-forge/linux-64/"},
		{"trailing slash", anaconda, "shards/", "", "shards", "https://anaconda.org/conda-forge/linux-64/shards/"},
		{"absolute URL with different path", anaconda, "https://example.com/different/path/", "https://example.com/different/path/", "", "https://example.com/different/path/"},
		{"./ does not inherit file name", cf, "./", "https://conda.anaconda.org/conda-forge/linux-64/", "", "https://conda.anaconda.org/conda-forge/linux-64/"},
		{"absolute URL with same host", anaconda, "https://example.com/shards", "", "shards", "https://example.com/shards/"},
		{"absolute URL with different host", anaconda, "https://different-host.com/shards", "", "different-host.com", "https://different-host.com/shards/"},
		{"./shards", anaconda, "./shards", "", "shards", "https://anaconda.org/conda-forge/linux-64/shards/"},
		// libmamba appends "/shards" to the subdir; goomba follows RFC 3986
		// (host root), as rattler does.
		{"/shards", anaconda, "/shards", "", "shards", "https://anaconda.org/shards/"},
		{"../shared", cf, "../shared/", "", "", "https://conda.anaconda.org/conda-forge/shared/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shardURLFor(t, c.index, c.shardsBase)
			if !strings.HasSuffix(got, ".msgpack.zst") {
				t.Errorf("%s: no .msgpack.zst suffix", got)
			}
			if strings.Contains(got, "repodata.json/") || strings.Contains(got, shardIndexFile+"/") {
				t.Errorf("%s inherits the index file name", got)
			}
			if !strings.HasPrefix(got, c.prefix) || !strings.Contains(got, c.contains) {
				t.Errorf("%s: want prefix %q and substring %q", got, c.prefix, c.contains)
			}
			if want := c.want + testHash + ".msgpack.zst"; got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
}

// Cases from libmamba tests/src/core/test_shards.cpp "Shards - Basic
// operations" (base_url, relative sentinel base_url) plus CEP 15 relative
// base URLs.
func TestResolveBaseURL(t *testing.T) {
	cases := []struct {
		index, base, want string
		pol               urlPolicy
	}{
		{"https://anaconda.org/conda-forge/linux-64/repodata.json", "https://example.com/packages", "https://example.com/packages/", urlPolicy{}},
		{"https://conda.anaconda.org/conda-forge/linux-64/repodata.json", "/", "https://conda.anaconda.org/conda-forge/linux-64/", urlPolicy{}},
		{"https://conda.anaconda.org/conda-forge/linux-64/repodata.json", "", "https://conda.anaconda.org/conda-forge/linux-64/", urlPolicy{}},
		{"https://conda.anaconda.org/conda-forge/linux-64/repodata.json", ".", "https://conda.anaconda.org/conda-forge/linux-64/", urlPolicy{}},
		{"https://conda.anaconda.org/conda-forge/linux-64/repodata.json", "./", "https://conda.anaconda.org/conda-forge/linux-64/", urlPolicy{}},
		{"https://conda.anaconda.org/conda-forge/linux-64/repodata.json", "../pkgs/linux-64", "https://conda.anaconda.org/conda-forge/pkgs/linux-64/", urlPolicy{}},
		{"https://conda.anaconda.org/conda-forge/linux-64/repodata.json", "/mirror/cf/", "https://conda.anaconda.org/mirror/cf/", urlPolicy{}},
		{"http://127.0.0.1:8080/c/noarch/repodata.json", "", "http://127.0.0.1:8080/c/noarch/", urlPolicy{httpLoopback: true, privateHosts: true}},
		{"https://10.0.0.5/c/noarch/repodata.json", "https://10.0.0.6/pkgs/", "https://10.0.0.6/pkgs/", urlPolicy{privateHosts: true}},
	}
	for _, c := range cases {
		got, err := resolveBase(mustURL(t, c.index), c.base, c.pol)
		if err != nil {
			t.Errorf("resolveBase(%q, %q): %v", c.index, c.base, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("resolveBase(%q, %q) = %s, want %s", c.index, c.base, got, c.want)
		}
	}
}

func TestResolveBaseRefusesUnsafeURLs(t *testing.T) {
	index := mustURL(t, "https://conda.anaconda.org/conda-forge/linux-64/repodata.json")
	for _, base := range []string{
		"http://example.com/packages",
		"ftp://example.com/packages",
		"file:///etc/",
		"https://example.com/packages?token=1",
		"https://example.com/packages#frag",
		"//",
		"https://user:pw@example.com/packages",
		"https://other.example/t/tk-secret123/conda-forge/linux-64/",
		"/t/tk-secret123/x/",
		// S4: a public channel's index must not aim at local services.
		"http://127.0.0.1:2375/v1.40/containers/",
		"https://127.0.0.1:6443/api/",
		"https://localhost/x/",
		"https://[::1]/x/",
		"https://10.1.2.3/x/",
		"https://192.168.1.1/x/",
		"https://169.254.169.254/latest/meta-data/",
		"https://[fe80::1%25eth0]/x/",
		"https://0.0.0.0/x/",
		"https://[::ffff:127.0.0.1]/x/",
		"https://metadata.localhost/x/",
	} {
		if _, err := resolveBase(index, base, urlPolicy{}); !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("resolveBase(%q): err = %v", base, err)
		}
	}
	// A loopback http test channel may use other loopback servers, but not
	// remote http ones.
	local := urlPolicy{httpLoopback: true, privateHosts: true}
	lindex := mustURL(t, "http://127.0.0.1:9/c/noarch/repodata.json")
	if _, err := resolveBase(lindex, "http://localhost:10/x/", local); err != nil {
		t.Errorf("loopback base for a loopback channel: %v", err)
	}
	if _, err := resolveBase(lindex, "http://example.com/x/", local); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("remote http base: %v", err)
	}
}

func TestCanonicalChannel(t *testing.T) {
	good := []struct {
		in, want string
		pol      urlPolicy
	}{
		{"https://conda.anaconda.org/conda-forge", "https://conda.anaconda.org/conda-forge", urlPolicy{}},
		{"https://conda.anaconda.org/conda-forge///", "https://conda.anaconda.org/conda-forge", urlPolicy{}},
		{"HTTPS://Conda.Anaconda.org/conda-forge/", "https://conda.anaconda.org/conda-forge", urlPolicy{}},
		{"http://127.0.0.1:1234/chan", "http://127.0.0.1:1234/chan", urlPolicy{httpLoopback: true, privateHosts: true}},
		{"http://localhost/chan", "http://localhost/chan", urlPolicy{httpLoopback: true, privateHosts: true}},
		{"http://[::1]:99/chan", "http://[::1]:99/chan", urlPolicy{httpLoopback: true, privateHosts: true}},
		{"https://10.0.0.5/mirror", "https://10.0.0.5/mirror", urlPolicy{privateHosts: true}},
		{"https://conda.anaconda.org/t", "https://conda.anaconda.org/t", urlPolicy{}}, // a channel named "t"
	}
	for _, c := range good {
		u, pol, err := canonicalChannel(c.in)
		if err != nil {
			t.Errorf("canonicalChannel(%q): %v", c.in, err)
			continue
		}
		if u.String() != c.want || pol != c.pol {
			t.Errorf("canonicalChannel(%q) = %s %+v, want %s %+v", c.in, u, pol, c.want, c.pol)
		}
	}
	for _, in := range []string{
		"conda-forge",
		"http://conda.anaconda.org/conda-forge",
		"http://10.0.0.1/c",
		"file:///srv/channel",
		"ftp://example.com/c",
		"https://example.com/c?x=1",
		"https://example.com/c#x",
		"https:///nohost",
		"https://example.com/a/../b",
		"://bad",
	} {
		if _, _, err := canonicalChannel(in); !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("canonicalChannel(%q): err = %v", in, err)
		}
	}
}

// S3: channel URLs with credentials are refused with a fixed message that
// never repeats the URL, also when it does not parse.
func TestCanonicalChannelRefusesCredentials(t *testing.T) {
	for _, in := range []string{
		"https://user:hunter2@conda.anaconda.org/c",
		"https://tk-secret@conda.anaconda.org/c",
		"https://conda.anaconda.org/t/tk-secret/conda-forge",
		"https://conda.anaconda.org/t/tk-secret",
		"https://user:hunter2@host/t/tk-secret/c%zz",
		"http://user:hunter2@127.0.0.1/c",
	} {
		_, _, err := canonicalChannel(in)
		if !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("%q: err = %v", in, err)
			continue
		}
		for _, secret := range []string{"hunter2", "tk-secret", "conda.anaconda.org", "host/", "127.0.0.1"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%q: error %q repeats %q", in, err, secret)
			}
		}
	}
	// Every entry point refuses them.
	ctx := ctxT(t)
	const u = "https://conda.anaconda.org/t/tk-secret/conda-forge"
	if _, err := NewShardedChannel(ctx, u, testOptions(t, "noarch")); !errors.Is(err, ErrUnsupportedURL) || strings.Contains(err.Error(), "tk-secret") {
		t.Errorf("NewShardedChannel: %v", err)
	}
	if _, err := NewFullIndex(ctx, u, testOptions(t, "noarch")); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("NewFullIndex: %v", err)
	}
	if _, err := LoadRepodataJSON(strings.NewReader("{}"), "https://u:p@example.com/c", "noarch"); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("LoadRepodataJSON: %v", err)
	}
}

// S4: redirects follow the channel's policy and never downgrade to http.
func TestRedirectPolicy(t *testing.T) {
	cases := []struct {
		pol      urlPolicy
		from, to string
		ok       bool
	}{
		{urlPolicy{}, "https://conda.anaconda.org/x", "https://cdn.example.com/x", true},
		{urlPolicy{}, "https://conda.anaconda.org/x", "http://cdn.example.com/x", false},
		{urlPolicy{}, "https://conda.anaconda.org/x", "http://127.0.0.1:6379/x", false},
		{urlPolicy{}, "https://conda.anaconda.org/x", "https://127.0.0.1:6379/x", false},
		{urlPolicy{}, "https://conda.anaconda.org/x", "https://192.168.0.1/x", false},
		{urlPolicy{httpLoopback: true, privateHosts: true}, "http://127.0.0.1:1/x", "http://127.0.0.1:2/x", true},
		{urlPolicy{httpLoopback: true, privateHosts: true}, "https://example.com/x", "http://127.0.0.1:2/x", false}, // downgrade
		{urlPolicy{privateHosts: true}, "https://10.0.0.5/x", "https://10.0.0.6/x", true},
		{urlPolicy{privateHosts: true}, "https://10.0.0.5/x", "http://10.0.0.6/x", false},
	}
	for _, c := range cases {
		client := safeClient(nil, c.pol, nil)
		from, _ := http.NewRequest(http.MethodGet, c.from, nil)
		to, _ := http.NewRequest(http.MethodGet, c.to, nil)
		err := client.CheckRedirect(to, []*http.Request{from})
		if (err == nil) != c.ok {
			t.Errorf("%s -> %s: err = %v", c.from, c.to, err)
		}
	}
}
