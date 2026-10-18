package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/richinsley/goomba/archive"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/virtual"
)

func runCLI(t *testing.T, c *cli, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	c.stdout, c.stderr = &out, &errOut
	code := c.run(context.Background(), args)
	return code, out.String(), errOut.String()
}

func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, exitUsage},
		{[]string{"bogus"}, exitUsage},
		{[]string{"create"}, exitUsage},
		{[]string{"create", "--lock", "x"}, exitUsage},
		{[]string{"create", "-p", "x"}, exitUsage},
		{[]string{"create", "--lock", "x", "-p", "y", "extra"}, exitUsage},
		{[]string{"create", "--bogus"}, exitUsage},
		{[]string{"create", "--no-pyc=maybe", "--lock", "x", "-p", "y"}, exitUsage},
		{[]string{"lock", "--from-prefix", "x"}, exitUsage},
		{[]string{"lock", "-o", "x"}, exitUsage},
		{[]string{"export", "--lock", "x"}, exitUsage},
		{[]string{"export", "--explicit"}, exitUsage},
		{[]string{"list"}, exitUsage},
		{[]string{"list", "-p"}, exitUsage},
		{[]string{"help"}, exitOK},
		{[]string{"--help"}, exitOK},
		{[]string{"create", "-h"}, exitOK},
		{[]string{"create", "--lock", "/nonexistent/lock.json", "-p", "y", "--pkgs-dir", "z"}, exitFailure},
		{[]string{"export", "--explicit", "--lock", "/nonexistent/lock.json"}, exitFailure},
		{[]string{"list", "-p", "/nonexistent/prefix"}, exitFailure},
		{[]string{"lock", "--from-prefix", "/nonexistent/prefix", "-o", "-"}, exitFailure},
	} {
		code, _, stderr := runCLI(t, &cli{}, tc.args...)
		if code != tc.want {
			t.Errorf("%q: exit %d, want %d (%s)", tc.args, code, tc.want, stderr)
		}
		if tc.want == exitUsage && !strings.Contains(stderr, "usage:") {
			t.Errorf("%q: no usage text: %s", tc.args, stderr)
		}
	}
}

// testLockfile serves the root package's testdata archive dist over TLS and
// returns a lockfile file for it.
func testLockfile(t *testing.T, dist string) (string, *httptest.Server, *lock.Lockfile) {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "pkgs", dist+".conda")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "x")
	if err := archive.Extract(context.Background(), src, dir, archive.Limits{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "info", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Name, Version, Build, Subdir, Noarch string
		BuildNumber                          uint64 `json:"build_number"`
		Depends                              []string
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ch/"+idx.Subdir+"/"+dist+".conda" {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	s, m := sha256.Sum256(data), md5.Sum(data)
	rec := &record.PackageRecord{
		Name: idx.Name, Version: idx.Version, Build: idx.Build, BuildNumber: idx.BuildNumber,
		Subdir: idx.Subdir, Noarch: idx.Noarch, Depends: idx.Depends,
		URL: srv.URL + "/ch/" + idx.Subdir + "/" + dist + ".conda", SHA256: hex.EncodeToString(s[:]), MD5: hex.EncodeToString(m[:]), Size: int64(len(data)),
	}
	plat := virtual.HostPlatform()
	if plat == "" {
		t.Skip("host has no conda platform")
	}
	lf, err := lock.FromRecords(plat, []string{srv.URL + "/ch"}, []string{"ca-certificates"}, map[string]string{}, []*record.PackageRecord{rec})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "env.lock.json")
	var buf bytes.Buffer
	if err := lf.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return file, srv, lf
}

func TestCommands(t *testing.T) {
	lockFile, srv, lf := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	cache := t.TempDir()
	c := &cli{
		client:   func() (*http.Client, error) { return srv.Client(), nil },
		cacheDir: func() (string, error) { return cache, nil },
	}
	envs := t.TempDir()
	prefix := filepath.Join(envs, "env")

	code, _, stderr := runCLI(t, c, "create", "--lock", lockFile, "-p", prefix, "--no-pyc")
	if code != exitOK {
		t.Fatalf("create: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "linked ca-certificates-2026.2.25-hbd8a1cb_0 (1/1)") {
		t.Errorf("create output:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(prefix, "ssl", "cacert.pem")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "goomba", "pkgs")); err != nil {
		t.Errorf("default package cache: %v", err)
	}
	hist, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "history"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hist), "# cmd: goomba create --lock "+lockFile+" -p "+prefix+" --no-pyc\n") {
		t.Errorf("history:\n%s", hist)
	}

	code, stdout, stderr := runCLI(t, c, "list", "-p", prefix)
	if code != exitOK {
		t.Fatalf("list: exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || strings.Fields(lines[0])[0] != "Name" ||
		strings.Join(strings.Fields(lines[1]), " ") != "ca-certificates 2026.2.25 hbd8a1cb_0 "+srv.URL+"/ch" {
		t.Errorf("list:\n%s", stdout)
	}

	// An unfinished installation is flagged.
	marker := filepath.Join(prefix, "conda-meta", ".goomba-incomplete")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, c, "list", "-p", prefix); code != exitOK || !strings.Contains(stderr, "incomplete") {
		t.Errorf("list of an incomplete prefix: exit %d: %s", code, stderr)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "back.lock.json")
	if code, _, stderr := runCLI(t, c, "lock", "--from-prefix", prefix, "-o", out); code != exitOK {
		t.Fatalf("lock: exit %d: %s", code, stderr)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	back, err := lock.Read(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Packages) != 1 || back.Packages[0].URL != lf.Packages[0].URL || back.Packages[0].SHA256 != lf.Packages[0].SHA256 {
		t.Errorf("lock round trip: %+v", back.Packages)
	}
	if strings.Join(back.Specs, ",") != "ca-certificates" {
		t.Errorf("lock specs %v", back.Specs)
	}
	code, stdout, _ = runCLI(t, c, "lock", "--from-prefix", prefix, "-o", "-")
	if code != exitOK || !strings.HasPrefix(stdout, "{\n") {
		t.Errorf("lock to stdout: exit %d\n%s", code, stdout)
	}

	code, stdout, stderr = runCLI(t, c, "export", "--explicit", "--lock", out)
	if code != exitOK {
		t.Fatalf("export: exit %d: %s", code, stderr)
	}
	want := "# platform: " + lf.Platform + "\n@EXPLICIT\n" + lf.Packages[0].URL + "#" + lf.Packages[0].MD5 + "\n"
	if stdout != want {
		t.Errorf("export:\n%s\nwant\n%s", stdout, want)
	}

	// A second create into the same prefix fails.
	code, _, stderr = runCLI(t, c, "create", "--lock", lockFile, "-p", prefix)
	if code != exitFailure || !strings.Contains(stderr, "not an empty directory") {
		t.Errorf("second create: exit %d: %s", code, stderr)
	}
}

// TestProxyClient: without HTTPS_PROXY the default client (no proxy) is
// used; with it, net/http's own ProxyFromEnvironment decides, NO_PROXY
// included (review N3). net/http reads the environment once per process,
// so each case runs in a child process.
func TestProxyClient(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if c, err := proxyClient(env(nil), io.Discard); c != nil || err != nil {
		t.Errorf("no proxy: %v %v", c, err)
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
		c, err := proxyClient(env(map[string]string{k: "http://proxy.invalid:3128"}), io.Discard)
		if err != nil || c == nil || c.Transport.(*http.Transport).Proxy == nil {
			t.Errorf("%s: %v %v", k, c, err)
		}
	}
	for _, tc := range []struct {
		noProxy string
		proxied map[string]bool
	}{
		{"", map[string]bool{"https://conda.anaconda.org/x": true, "https://localhost/x": false, "https://127.0.0.1:8443/x": false}},
		{"*", map[string]bool{"https://conda.anaconda.org/x": false}},
		{".corp,10.0.0.0/8,example.com", map[string]bool{
			"https://x.corp/x": false, "https://corp/x": true, "https://10.1.2.3/x": false, "https://11.0.0.1/x": true,
			"https://example.com/x": false, "https://www.example.com/x": false, "https://notexample.com/x": true,
		}},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProxyChild$", "-test.count=1", "-test.v")
		cmd.Env = []string{"GOOMBA_TEST_CHILD=proxy", "HTTPS_PROXY=http://proxy.invalid:3128", "NO_PROXY=" + tc.noProxy}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		for raw, want := range tc.proxied {
			line := "PROXY " + raw + " " + map[bool]string{true: "proxied", false: "direct"}[want]
			if !strings.Contains(string(out), line+"\n") {
				t.Errorf("NO_PROXY=%q: want %q in\n%s", tc.noProxy, line, out)
			}
		}
	}
}

// TestProxyChild runs in a child process for TestProxyClient: it asks the
// CLI's client how it would reach each URL.
func TestProxyChild(t *testing.T) {
	if os.Getenv("GOOMBA_TEST_CHILD") != "proxy" {
		t.Skip("child process only")
	}
	c, err := proxyClient(os.Getenv, io.Discard)
	if err != nil || c == nil {
		t.Fatalf("proxyClient: %v %v", c, err)
	}
	for _, raw := range []string{
		"https://conda.anaconda.org/x", "https://localhost/x", "https://127.0.0.1:8443/x", "https://x.corp/x", "https://corp/x",
		"https://10.1.2.3/x", "https://11.0.0.1/x", "https://example.com/x", "https://www.example.com/x", "https://notexample.com/x",
	} {
		u, _ := url.Parse(raw)
		p, err := c.Transport.(*http.Transport).Proxy(&http.Request{URL: u})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("PROXY %s %s\n", raw, map[bool]string{true: "proxied", false: "direct"}[p != nil])
	}
}

func TestPrintable(t *testing.T) {
	if got := printable("a\x1b[2Jb\u009bc\td"); got != "a?[2Jb?c?d" {
		t.Errorf("printable = %q", got)
	}
	if channelName("https://conda.anaconda.org/conda-forge") != "conda-forge" || channelName("https://x/y") != "https://x/y" {
		t.Error("channelName")
	}
}

// TestBadProxyRefused (review rounds 2 and 3): the proxy setting Go will
// use (the first non-empty of HTTPS_PROXY and https_proxy) must parse; one
// net/http would drop or misread is an error naming the variable, with
// credentials hidden and none of url.Parse's own words. What Go accepts
// (a path on the proxy URL, odd NO_PROXY entries, an unused bad
// https_proxy) only warns, or passes.
func TestBadProxyRefused(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		env    map[string]string
		ok     bool
		inErr  string // in the error, or in the warnings when ok
		secret string
	}{
		{map[string]string{"HTTPS_PROXY": "http://user:pa%zzss@proxy.corp:3128"}, false, "invalid proxy URL in HTTPS_PROXY", "pa%zzss"},
		{map[string]string{"HTTPS_PROXY": "http://user:se/cret@proxy.corp:3128"}, false, "invalid proxy URL in HTTPS_PROXY", "se"},
		{map[string]string{"HTTPS_PROXY": "http://user:se%2Fcret@proxy.corp:3128"}, true, "", ""},
		{map[string]string{"https_proxy": "http://proxy corp:3128"}, false, "https_proxy", ""},
		{map[string]string{"HTTPS_PROXY": "http://"}, false, "HTTPS_PROXY", ""},
		{map[string]string{"HTTPS_PROXY": "ftp://proxy:21"}, false, "scheme", ""},
		{map[string]string{"HTTPS_PROXY": "http://proxy:99999"}, false, "port", ""},
		{map[string]string{"HTTPS_PROXY": "http://proxy:3128/pac.pac"}, true, "ignored", ""},
		{map[string]string{"HTTPS_PROXY": "proxy.corp:3128"}, true, "", ""},
		{map[string]string{"HTTPS_PROXY": "socks5://proxy.corp:1080"}, true, "", ""},
		{map[string]string{"HTTPS_PROXY": "http://proxy.corp:3128", "https_proxy": "http://x y"}, true, "", ""},
		{map[string]string{"HTTPS_PROXY": "http://u:p@proxy.corp:3128", "NO_PROXY": "https://internal.corp"}, true, "matches no host", ""},
		{map[string]string{"HTTPS_PROXY": "http://proxy.corp:3128", "NO_PROXY": "[2001:db8::]/32,10.0.0.0/33", "no_proxy": "https://x"}, true, "", ""},
		{map[string]string{"HTTPS_PROXY": "http://proxy.corp:3128", "NO_PROXY": " .corp, 10.0.0.0/8 ,localhost:443,*"}, true, "", ""},
	} {
		var warn bytes.Buffer
		c, err := proxyClient(env(tc.env), &warn)
		if tc.ok {
			if err != nil || c == nil || tc.inErr != "" && !strings.Contains(warn.String(), tc.inErr) || tc.inErr == "" && warn.Len() > 0 {
				t.Errorf("%v: client %v, err %v, warnings %q", tc.env, c != nil, err, warn.String())
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.inErr) {
			t.Errorf("%v: err = %v, want one mentioning %q", tc.env, err, tc.inErr)
		}
		if err != nil && tc.secret != "" && strings.Contains(strings.ReplaceAll(err.Error(), "***", ""), ":"+tc.secret) {
			t.Errorf("%v: error leaks the password: %v", tc.env, err)
		}
		if err != nil && strings.Contains(err.Error(), "invalid port") {
			t.Errorf("%v: url.Parse's message shown: %v", tc.env, err)
		}
	}
	// Through the CLI: exit 1, before anything is installed.
	lockFile, _, _ := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	c := &cli{
		client: func() (*http.Client, error) {
			return proxyClient(env(map[string]string{"HTTPS_PROXY": "http://user:pa%zzss@proxy.corp:3128"}), io.Discard)
		},
		cacheDir: func() (string, error) { return t.TempDir(), nil },
		virtual:  fakeDetect,
	}
	prefix := filepath.Join(t.TempDir(), "env")
	code, _, stderr := runCLI(t, c, "create", "--lock", lockFile, "-p", prefix)
	if code != exitFailure || !strings.Contains(stderr, "HTTPS_PROXY") || strings.Contains(stderr, "pa%zzss") {
		t.Errorf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(prefix); !os.IsNotExist(err) {
		t.Errorf("prefix created: %v", err)
	}
}

func TestRedactProxy(t *testing.T) {
	for in, want := range map[string]string{
		"http://u:p@h:1":        "http://***@h:1",
		"http://u:se/c?r@h:1":   "http://***@h:1",
		"u:p@h:1":               "***@h:1",
		"http://h:1":            "http://h:1",
		"socks5://u@a@b@h:1080": "socks5://***@h:1080",
	} {
		if got := redactProxy(in); got != want {
			t.Errorf("redactProxy(%q) = %q, want %q", in, got, want)
		}
	}
}

// epipeWriter fails every write as a pipe whose reader has gone does.
type epipeWriter struct{}

func (epipeWriter) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "pipe", Err: syscall.EPIPE}
}

// TestClosedStdout: when the reader of standard output goes away (goomba
// list | head), goomba exits 1 without a message; an EPIPE that did not
// come from standard output is still reported.
func TestClosedStdout(t *testing.T) {
	lockFile, srv, _ := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	cache := t.TempDir()
	c := &cli{
		client:   func() (*http.Client, error) { return srv.Client(), nil },
		cacheDir: func() (string, error) { return cache, nil },
	}
	prefix := filepath.Join(t.TempDir(), "env")
	if code, _, stderr := runCLI(t, c, "create", "--lock", lockFile, "-p", prefix, "--no-pyc"); code != exitOK {
		t.Fatalf("create: exit %d: %s", code, stderr)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	for _, args := range [][]string{
		{"list", "-p", prefix},
		{"export", "--explicit", "--lock", lockFile},
		{"lock", "--from-prefix", prefix, "-o", "-"},
	} {
		for name, out := range map[string]io.Writer{"pipe": stdoutWriter{w}, "fake": stdoutWriter{epipeWriter{}}} {
			var errOut bytes.Buffer
			c := &cli{stdout: out, stderr: &errOut}
			if code := c.run(context.Background(), args); code != exitFailure || errOut.Len() != 0 {
				t.Errorf("%s to a closed %s: exit %d, stderr %q; want 1 and no message", args[0], name, code, errOut.String())
			}
		}
	}
	var errOut bytes.Buffer
	c = &cli{stdout: epipeWriter{}, stderr: &errOut}
	if code := c.run(context.Background(), []string{"list", "-p", prefix}); code != exitFailure || !strings.Contains(errOut.String(), "broken pipe") {
		t.Errorf("EPIPE not from standard output: exit %d, stderr %q; want it reported", code, errOut.String())
	}
}
