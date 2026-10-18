package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/richinsley/goomba"
	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/record"
	"github.com/richinsley/goomba/virtual"
)

// specChannel serves a conda channel at <srv.URL>/ch over TLS with full
// indexes: noarch lists the testdata ca-certificates archive (served) and
// two records that cannot be installed; the platform subdirs are empty.
// block, if set, makes every request wait for its context to end.
type specChannel struct {
	srv   *httptest.Server
	url   string
	block bool
}

func newSpecChannel(t *testing.T, platforms ...string) *specChannel {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ca-certificates needs __unix")
	}
	const dist = "ca-certificates-2026.2.25-hbd8a1cb_0"
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pkgs", dist+".conda"))
	if err != nil {
		t.Fatal(err)
	}
	s, m := sha256.Sum256(data), md5.Sum(data)
	entry := func(name, version string, depends []string, sha string) map[string]any {
		return map[string]any{"name": name, "version": version, "build": "0", "build_number": 0, "depends": depends, "subdir": "noarch", "sha256": sha}
	}
	ca := entry("ca-certificates", "2026.2.25", []string{"__unix"}, hex.EncodeToString(s[:]))
	ca["build"], ca["md5"], ca["size"], ca["noarch"] = "hbd8a1cb_0", hex.EncodeToString(m[:]), len(data), "generic"
	fake := strings.Repeat("ab", 32)
	noarch, err := json.Marshal(map[string]any{
		"info": map[string]any{"subdir": "noarch"},
		"packages.conda": map[string]any{
			dist + ".conda":     ca,
			"evil-1.0-0.conda":  entry("evil", "1.0", []string{"missing \x1b[2J\x1b]0;pwned\x07 >=1"}, fake),
			"needy-1.0-0.conda": entry("needy", "1.0", []string{"ca-certificates >=2099"}, fake),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"/ch/noarch/repodata.json":      noarch,
		"/ch/noarch/" + dist + ".conda": data,
	}
	for _, p := range append(platforms, virtual.HostPlatform()) {
		files["/ch/"+p+"/repodata.json"] = []byte(`{"info": {"subdir": "` + p + `"}, "packages.conda": {}}`)
	}
	c := &specChannel{}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.block {
			<-r.Context().Done()
			return
		}
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(c.srv.Close)
	c.url = c.srv.URL + "/ch"
	return c
}

// cli returns a CLI using the channel's client, a private cache directory
// and the platform's default virtual packages.
func (c *specChannel) cli(t *testing.T) (*cli, string) {
	cache := t.TempDir()
	return &cli{
		client:   func() (*http.Client, error) { return c.srv.Client(), nil },
		cacheDir: func() (string, error) { return cache, nil },
		virtual:  fakeDetect,
	}, cache
}

// TestCreateFromSpecs: create resolves SPECs against -c channels and
// installs the result, caching the index under the user cache directory.
func TestCreateFromSpecs(t *testing.T) {
	ch := newSpecChannel(t)
	c, cache := ch.cli(t)
	prefix := filepath.Join(t.TempDir(), "env")
	code, _, stderr := runCLI(t, c, "create", "-p", prefix, "ca-certificates", "-c", ch.url, "--no-pyc")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"loaded " + ch.url + " (1/1)", "solved: 1 packages", "linked ca-certificates-2026.2.25-hbd8a1cb_0 (1/1)", "created " + prefix + " (1 packages)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("no %q in:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "no channel given") {
		t.Error("default channel notice with -c")
	}
	if _, err := os.Stat(filepath.Join(prefix, "ssl", "cacert.pem")); err != nil {
		t.Error(err)
	}
	for _, d := range []string{"repodata", "pkgs"} {
		if _, err := os.Stat(filepath.Join(cache, "goomba", d)); err != nil {
			t.Errorf("default %s directory: %v", d, err)
		}
	}
	hist, err := os.ReadFile(filepath.Join(prefix, "conda-meta", "history"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# cmd: goomba create -p " + prefix + " ca-certificates -c " + ch.url + " --no-pyc\n", `# update specs: ["ca-certificates"]`} {
		if !strings.Contains(string(hist), want) {
			t.Errorf("no %q in history:\n%s", want, hist)
		}
	}
	// Explicit cache directories.
	pkgs, rd := filepath.Join(t.TempDir(), "p"), filepath.Join(t.TempDir(), "r")
	code, _, stderr = runCLI(t, c, "create", "--repodata-dir", rd, "--pkgs-dir", pkgs, "-c", ch.url, "-p", filepath.Join(t.TempDir(), "env2"), "ca-certificates")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, d := range []string{pkgs, filepath.Join(rd, "index")} {
		if _, err := os.Stat(d); err != nil {
			t.Error(err)
		}
	}
}

// TestLockFromSpecs: lock resolves SPECs (flags and SPECs in any order)
// to a lockfile, for this machine or another platform.
func TestLockFromSpecs(t *testing.T) {
	other := "osx-arm64"
	if virtual.HostPlatform() == other {
		other = "linux-64"
	}
	ch := newSpecChannel(t, other)
	c, _ := ch.cli(t)
	code, stdout, stderr := runCLI(t, c, "lock", "ca-certificates", "-c", ch.url, "-o", "-")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lf, err := lock.Read(strings.NewReader(stdout))
	if err != nil {
		t.Fatal(err)
	}
	if len(lf.Packages) != 1 || !slices.Equal(lf.Channels, []string{ch.url}) || !slices.Equal(lf.Specs, []string{"ca-certificates"}) || lf.Platform != virtual.HostPlatform() {
		t.Errorf("lockfile %+v", lf)
	}
	out := filepath.Join(t.TempDir(), "x.lock.json")
	code, _, stderr = runCLI(t, c, "lock", "--platform", other, "-c", ch.url, "-o", out, "--virtual-override", "osx=14.0", "ca-certificates")
	if code != exitOK {
		t.Fatalf("--platform: exit %d: %s", code, stderr)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lf, err = lock.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if lf.Platform != other || len(lf.Packages) != 1 {
		t.Errorf("--platform lockfile %+v", lf)
	}
	if other == "osx-arm64" && lf.Virtual["__osx"] != "14.0" {
		t.Errorf("override not applied: %v", lf.Virtual)
	}
	// "--" ends the flags.
	code, stdout, stderr = runCLI(t, c, "lock", "-c", ch.url, "-o", "-", "--", "ca-certificates")
	if code != exitOK || !strings.Contains(stdout, `"ca-certificates"`) {
		t.Errorf("after --: exit %d: %s", code, stderr)
	}
}

// TestDefaultChannel: with no -c, conda-forge is used and a notice says so
// (offline with an empty cache, so nothing is downloaded).
func TestDefaultChannel(t *testing.T) {
	c := &cli{cacheDir: func() (string, error) { return t.TempDir(), nil }, virtual: fakeDetect}
	code, _, stderr := runCLI(t, c, "lock", "--offline", "-o", "-", "python")
	if code != exitFailure || !strings.Contains(stderr, "no channel given (-c); using conda-forge") || !strings.Contains(stderr, "https://conda.anaconda.org/conda-forge") || !strings.Contains(stderr, "not in cache") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

// TestUnsolvableCLI: an unsolvable request prints the explanation, with
// the index's control sequences escaped, and exits 1; create leaves no
// prefix.
func TestUnsolvableCLI(t *testing.T) {
	ch := newSpecChannel(t)
	c, _ := ch.cli(t)
	for _, specs := range [][]string{{"nonexistent"}, {"evil"}, {"needy"}} {
		args := append([]string{"lock", "-c", ch.url, "-o", "-"}, specs...)
		code, stdout, stderr := runCLI(t, c, args...)
		if code != exitFailure || !strings.Contains(stderr, "goomba: the request cannot be satisfied:\n") || stdout != "" {
			t.Errorf("%q: exit %d: %s", specs, code, stderr)
		}
		if strings.ContainsAny(stderr, "\x1b\x07") || !utf8.ValidString(stderr) {
			t.Errorf("%q: raw control characters: %q", specs, stderr)
		}
		if specs[0] != "nonexistent" && !strings.Contains(stderr, specs[0]+" 1.0") {
			t.Errorf("%q: no explanation:\n%s", specs, stderr)
		}
	}
	prefix := filepath.Join(t.TempDir(), "env")
	code, _, stderr := runCLI(t, c, "create", "-p", prefix, "-c", ch.url, "needy")
	if code != exitFailure || !strings.Contains(stderr, "cannot be satisfied") {
		t.Errorf("create: exit %d: %s", code, stderr)
	}
	if _, err := os.Lstat(prefix); !os.IsNotExist(err) {
		t.Errorf("prefix left: %v", err)
	}
}

// TestTimeoutFlag: --timeout bounds resolving; the error says so.
func TestTimeoutFlag(t *testing.T) {
	ch := newSpecChannel(t)
	ch.block = true
	c, _ := ch.cli(t)
	for _, args := range [][]string{
		{"lock", "-c", ch.url, "-o", "-", "--timeout", "200ms", "ca-certificates"},
		{"create", "-p", filepath.Join(t.TempDir(), "env"), "-c", ch.url, "--timeout", "200ms", "ca-certificates"},
	} {
		code, _, stderr := runCLI(t, c, args...)
		if code != exitFailure || !strings.Contains(stderr, "timed out after 200ms (--timeout)") {
			t.Errorf("%s: exit %d: %s", args[0], code, stderr)
		}
	}
}

// TestResolveUsage: combinations that make no sense are usage errors,
// before anything is resolved or installed.
func TestResolveUsage(t *testing.T) {
	ch := newSpecChannel(t)
	c, _ := ch.cli(t)
	p := filepath.Join(t.TempDir(), "env")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "-p", p, "--lock", "x.json", "python"}, "mutually exclusive"},
		{[]string{"create", "-p", p}, "--lock FILE or SPECs"},
		{[]string{"create", "python"}, "-p is required"},
		{[]string{"create", "-p", p, "--virtual-from-lock", "python"}, "needs --lock"},
		{[]string{"create", "-p", p, "--lock", "x.json", "-c", "conda-forge"}, "no use with --lock"},
		{[]string{"create", "-p", p, "--lock", "x.json", "--offline"}, "no use with --lock"},
		{[]string{"create", "-p", p, "--timeout", "0", "python"}, "--timeout must be positive"},
		{[]string{"create", "-p", p, "--timeout", "soon", "python"}, "invalid value"},
		{[]string{"lock", "python"}, "-o is required"},
		{[]string{"lock", "-o", "-"}, "SPECs to resolve or --from-prefix"},
		{[]string{"lock", "-o", "-", "--from-prefix", p, "python"}, "takes no SPECs"},
		{[]string{"lock", "-o", "-", "--from-prefix", p, "-c", "x"}, "takes no SPECs"},
		{[]string{"lock", "-o", "-", "--platform", "linux-65", "python"}, "not a conda platform"},
		{[]string{"lock", "-o", "-", "--platform", "zos-z", "--nvidia-smi", "/x", "-c", ch.url, "python"}, "no use with --platform"},
		{[]string{"lock", "-o", "-", "--virtual-override", "glibc", "python"}, "is not NAME=VALUE"},
	} {
		code, _, stderr := runCLI(t, c, tc.args...)
		if code != exitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: exit %d: %s", tc.args, code, stderr)
		}
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("prefix created: %v", err)
	}
}

// TestRepodataDirRequired: without a user cache directory, resolving SPECs
// needs --repodata-dir (and create --pkgs-dir).
func TestRepodataDirRequired(t *testing.T) {
	c := &cli{virtual: fakeDetect}
	code, _, stderr := runCLI(t, c, "lock", "-o", "-", "-c", "https://example.invalid/c", "python")
	if code != exitUsage || !strings.Contains(stderr, "--repodata-dir is required") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

func TestTerminalSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain text\nline two\tcol":    "plain text\nline two\tcol",
		"a\x1b[2Jb":                    `a\x1b[2Jb`,
		"bell\x07 del\x7f":             `bell\x07 del\x7f`,
		"c1\u009bx":                    `c1\x9bx`,
		"bidi\u202eevil\u2066":         `bidi\u202eevil\u2066`,
		"sep\u2028x\u2029":             `sep\u2028x\u2029`,
		"bad\xffutf8":                  `bad\xffutf8`,
		"cr\rlf":                       `cr\x0dlf`,
		`already \x1b escaped \\ ok`:   `already \x1b escaped \\ ok`,
		"ünïcödé and emoji 🐍 are fine": "ünïcödé and emoji 🐍 are fine",
	} {
		if got := terminalSafe(in); got != want {
			t.Errorf("terminalSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMixed(t *testing.T) {
	for _, tc := range []struct {
		args []string
		pos  []string
		o    string
		v    bool
	}{
		{[]string{"a", "-o", "x", "b"}, []string{"a", "b"}, "x", false},
		{[]string{"-v", "a", "-o", "x"}, []string{"a"}, "x", true},
		{[]string{"a", "--", "-o", "b"}, []string{"a", "-o", "b"}, "", false},
		{[]string{"-o", "--", "a", "-v"}, []string{"a"}, "--", true},
		{[]string{"-v", "--", "-v"}, []string{"-v"}, "", true},
		{[]string{"--o=--", "--", "x"}, []string{"x"}, "--", false},
		// Review round 1, N6: a value spelled like a flag is a value, and
		// everything after the "--" is positional.
		{[]string{"-c", "-o", "--", "-o", "x"}, []string{"-o", "x"}, "", false},
		{[]string{"-c", "-o", "--", "a", "-v"}, []string{"a", "-v"}, "", false},
		{[]string{"-c", "--", "a", "-v"}, []string{"a"}, "", true},
		{[]string{"-", "-o", "-"}, []string{"-"}, "-", false},
		{nil, nil, "", false},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		o := fs.String("o", "", "")
		v := fs.Bool("v", false, "")
		var c listFlag
		fs.Var(&c, "c", "")
		pos, err := parseMixed(fs, tc.args)
		if err != nil || !slices.Equal(pos, tc.pos) || *o != tc.o || *v != tc.v {
			t.Errorf("%q: %q o=%q v=%v, %v", tc.args, pos, *o, *v, err)
		}
	}
}

// FuzzTerminalSafe: the output never holds a control character other than
// newline and tab, a format or separator character, or invalid UTF-8, and
// text without them passes unchanged.
func FuzzTerminalSafe(f *testing.F) {
	for _, s := range []string{"", "plain", "a\x1b[2Jb", "\u202e", "\xff\xfe", "x\u2028y", "tab\there\nnew", "🐍"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := terminalSafe(s)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 out of %q", s)
		}
		clean := true
		for _, r := range s {
			if r == utf8.RuneError || r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r >= 0x80 && r < 0xa0 || r == 0x2028 || r == 0x2029 || isFormat(r) {
				clean = false
			}
		}
		for _, r := range out {
			if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r >= 0x80 && r < 0xa0 || r == 0x2028 || r == 0x2029 || isFormat(r) {
				t.Fatalf("%q -> %q keeps %U", s, out, r)
			}
		}
		if clean && out != s {
			t.Fatalf("clean %q changed to %q", s, out)
		}
		if terminalSafe(out) != out {
			t.Fatalf("not idempotent: %q -> %q", out, terminalSafe(out))
		}
	})
}

// FuzzParseMixed: interleaved arguments never panic, and the positional
// arguments come back in order, as a subsequence of the input.
func FuzzParseMixed(f *testing.F) {
	for _, s := range []string{"a\x00-o\x00x\x00b", "-v\x00--\x00-v", "-o\x00--\x00a", "--o=--\x00--\x00x", "-h", "-x\x00a", "--\x00--\x00--"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		args := strings.Split(s, "\x00")
		if len(args) > 64 {
			return
		}
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("o", "", "")
		fs.Bool("v", false, "")
		var l listFlag
		fs.Var(&l, "c", "")
		pos, err := parseMixed(fs, args)
		if err != nil {
			return
		}
		i := 0
		for _, p := range pos {
			for i < len(args) && args[i] != p {
				i++
			}
			if i == len(args) {
				t.Fatalf("positional %q of %q is not in order", p, args)
			}
			i++
		}
	})
}

func isFormat(r rune) bool { return unicode.Is(unicode.Cf, r) }

// TestRetryNoticeOrder (review round 1, N12): the retry without the python
// prefilter is announced before the channels load again.
func TestRetryNoticeOrder(t *testing.T) {
	var buf strings.Builder
	c := &cli{stderr: &buf}
	p := c.lockProgress()
	for _, e := range []goomba.Event{
		{Kind: goomba.EventRepodata, Channel: "https://x/c", Prefilter: "3.11", Done: 1, Total: 1},
		{Kind: goomba.EventSolving, Prefilter: "3.11", Done: 1, Total: 2},
		{Kind: goomba.EventRepodata, Channel: "https://x/c", Done: 1, Total: 1},
		{Kind: goomba.EventSolving, Done: 2, Total: 2},
		{Kind: goomba.EventSolved, Done: 3, Total: 3},
	} {
		p(e)
	}
	want := "loaded https://x/c (1/1)\n" +
		"solving (skipping packages for other python than 3.11)\n" +
		"no solution with the python 3.11 prefilter; solving again without it\n" +
		"loaded https://x/c (1/1)\n" +
		"solving\n" +
		"solved: 3 packages\n"
	if buf.String() != want {
		t.Errorf("got\n%s\nwant\n%s", buf.String(), want)
	}
}

// TestChannelUsageErrors (review round 1, N1, N5): unusable -c values are
// usage errors (exit 2) that never show credentials, before anything is
// detected or downloaded.
func TestChannelUsageErrors(t *testing.T) {
	ch := newSpecChannel(t)
	c, _ := ch.cli(t)
	detected := 0
	c.virtual = func(ctx context.Context, o virtual.Options) ([]*record.PackageRecord, error) {
		detected++
		return fakeDetect(ctx, o)
	}
	for _, tc := range []struct {
		channels []string
		want     string
	}{
		{[]string{"file:///tmp/c"}, "not an https URL"},
		{[]string{ch.url, ch.url + "/"}, "the same channel"},
		{[]string{"conda-forge/linux-64"}, "platform filters"},
		{[]string{"defaults"}, "defaults"},
		{[]string{"https://user:se/cret@example.invalid/c"}, "credentials"},
		{[]string{"https://example.invalid/%74/secret/c"}, "credentials"},
		{[]string{"https:/user:secret@example.invalid/c"}, "credentials"},
		{[]string{"user:secret@example.invalid/c"}, "credentials"},
	} {
		args := []string{"lock", "-o", "-"}
		for _, a := range tc.channels {
			args = append(args, "-c", a)
		}
		code, out, stderr := runCLI(t, c, append(args, "ca-certificates")...)
		if code != exitUsage || !strings.Contains(stderr, tc.want) || !strings.Contains(stderr, "usage:") || out != "" {
			t.Errorf("%q: exit %d: %s", tc.channels, code, stderr)
		}
		if strings.Contains(stderr, "cret") {
			t.Errorf("%q: printed with its secret: %s", tc.channels, stderr)
		}
	}
	if detected != 0 {
		t.Errorf("virtual packages detected %d times", detected)
	}
}

// TestCreateChecksBeforeDetect (review round 1, N4): create refuses a
// prefix or options that cannot work before detecting virtual packages
// and before the default-channel notice.
func TestCreateChecksBeforeDetect(t *testing.T) {
	ch := newSpecChannel(t)
	c, _ := ch.cli(t)
	detected := 0
	c.virtual = func(ctx context.Context, o virtual.Options) ([]*record.PackageRecord, error) {
		detected++
		return fakeDetect(ctx, o)
	}
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(t.TempDir(), "env")
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"create", "-p", full, "ca-certificates"}, exitFailure, "not an empty directory"},
		{[]string{"create", "-p", env, "--pkgs-dir", filepath.Join(env, "pkgs"), "ca-certificates"}, exitUsage, "inside the prefix"},
		{[]string{"create", "-p", env, "ca-certificates\n# injected"}, exitUsage, `SPEC "ca-certificates\n# injected" contains a line break`},
		{[]string{"create", "-p", env, "--lock", "a\nb.json"}, exitUsage, "the value of -lock contains a line break"},
		{[]string{"create", "-p", env, "-c", "https://u:pw\n@example.invalid/c", "x"}, exitUsage, "the value of -c contains a line break"},
	} {
		code, _, stderr := runCLI(t, c, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) || strings.Contains(stderr, "no channel given") || strings.Contains(stderr, "pw") || strings.Contains(stderr, "Cmd") {
			t.Errorf("%q: exit %d: %s", tc.args, code, stderr)
		}
	}
	if detected != 0 {
		t.Errorf("virtual packages detected %d times", detected)
	}
	if _, err := os.Lstat(env); !os.IsNotExist(err) {
		t.Errorf("prefix created: %v", err)
	}
}
