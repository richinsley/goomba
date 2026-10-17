package lock

import (
	"errors"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// TestErrorsRedactCredentials: no error message quotes the user name or
// password of a URL (review N1).
func TestErrorsRedactCredentials(t *testing.T) {
	const secret = "s3cr3t"
	cred := "https://alice:" + secret + "@conda.example.com"
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error", what)
			return
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "alice") {
			t.Errorf("%s: message leaks credentials: %v", what, err)
		}
		if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotInIndex) {
			t.Errorf("%s: %v does not wrap ErrInvalid", what, err)
		}
	}
	l := validLockfile()
	l.Channels = []string{cred + "/conda-forge"}
	check("channel", l.Validate())
	l = validLockfile()
	l.Channels = []string{cf, cf}
	l.Channels[1] = cred + "/x"
	check("second channel", l.Validate())
	for _, u := range []string{
		cred + "/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda",
		"https://alice:" + secret + "@[bad/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda",
		"https://alice:" + secret + "@conda.example.com/%zz/linux-64/python-3.11.15-hd63d673_0_cpython.conda",
	} {
		l = validLockfile()
		l.Packages[0].URL = u
		check("package url "+redactURL(u), l.Validate())
	}
	if _, err := FromRecords("linux-64", []string{cf}, nil, nil, []*record.PackageRecord{{
		Name: "python", Version: "3.11.15", Build: "hd63d673_0_cpython", Subdir: "linux-64", SHA256: pythonSHA,
		Fn: "other.conda", URL: cred + "/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda",
	}}); err != nil {
		check("FromRecords", err)
	}
	// Explicit files: malformed URLs and index records.
	_, err := ReadExplicit(strings.NewReader("@EXPLICIT\nhttps://alice:" + secret + "@[::1/x/linux-64/a-1-0.conda\n"))
	check("explicit parse", err)
	e, err := ReadExplicit(strings.NewReader("@EXPLICIT\n" + pythonURL + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Resolve(func(string) (*record.PackageRecord, bool) {
		return &record.PackageRecord{Name: "python", Version: "3.11.15", Build: "hd63d673_0_cpython", Subdir: "linux-64",
			SHA256: pythonSHA, URL: cred + "/conda-forge/linux-64/python-3.11.15-hd63d673_0_cpython.conda"}, true
	})
	check("explicit resolve", err)
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@host/x":           "https://***@host/x",
		"https://u@host":               "https://***@host",
		"https://user:se/cret@host/x":  "https://***@host/x",
		"https://user:se?cr#et@host/x": "https://***@host/x",
		"https://host/a@b":             "https://***@b",
		"https://u:p@h@host/x":         "https://***@host/x",
		"no url":                       "no url",
		"file:///x@y":                  "file://***@y",
		"x https://u:p@h/y z@w":        "x https://***@h/y z@w",
		"https://u:p@h/y\thttps://a@b": "https://***@h/y\thttps://a@b",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzRedactURL(f *testing.F) {
	for _, s := range []string{"https://u:p@host/x", "https://host/a@b", "https://user:se/cret@host/x", "https://u:p?q#r@h", "no url", "file:///x@y", "://@", "a b://c@d e"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := redactURL(s)
		i := strings.Index(s, "://")
		if i < 0 {
			if got != s {
				t.Fatalf("changed a string without a scheme: %q -> %q", s, got)
			}
			return
		}
		token := s[i+3:]
		if j := strings.IndexAny(token, " \t\r\n"); j >= 0 {
			token = token[:j]
		}
		at := strings.LastIndexByte(token, '@')
		if at < 0 {
			if got != s {
				t.Fatalf("changed a URL without '@': %q -> %q", s, got)
			}
			return
		}
		// Nothing between "://" and the last '@' survives.
		if want := s[:i+3] + "***" + s[i+3+at:]; got != want {
			t.Fatalf("redactURL(%q) = %q, want %q", s, got, want)
		}
	})
}
