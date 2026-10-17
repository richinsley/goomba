package link

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// shebangRegexp is libmamba's shebang_regex in Go syntax; std::regex's "."
// excludes "\r" as well as "\n". matchShebang must agree with it.
var shebangRegexp = regexp.MustCompile(`^(#!(?:[ ]*)(/(?:\\ |[^ \n\r\t])*)([^\n\r]*))$`)

func matchShebangRegexp(s string) (string, string, bool) {
	m := shebangRegexp.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[2], m[3], true
}

// Cases from libmamba tests/src/core/test_cpp.cpp, TEST_CASE("replace_long_shebang").
func TestReplaceLongShebangLibmamba(t *testing.T) {
	long := "#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooong/python -o test -x"
	aaa := "#!/" + strings.Repeat("a", 500) + "/bin/python -o test 123 -x"
	escaped := []byte(aaa)
	escaped[299], escaped[300] = '\\', ' '
	cases := []struct {
		platform, in, want string
	}{
		{"linux-64", long, "#!/usr/bin/env python -o test -x"},
		{"osx-64", long, long},
		{"linux-64", `#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooo\ oooooo\ oooooo\ oooooooooooooooooooooooooooooooooooong/python -o test -x`, "#!/usr/bin/env python -o test -x"},
		{"linux-64", `#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooo\ oooooo\ oooooo\ oooooooooooooooooooooooooooooooooooong/pyt hon -o test -x`, "#!/usr/bin/env pyt hon -o test -x"},
		{"linux-64", `#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooo\ oooooo\ oooooo\ oooooooooooooooooooooooooooooooooooong/pyt\ hon -o test -x`, `#!/usr/bin/env pyt\ hon -o test -x`},
		{"linux-64", `#! /this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooo\ oooooo\ oooooo\ oooooooooooooooooooooooooooooooooooong/pyt\ hon -o test -x`, `#!/usr/bin/env pyt\ hon -o test -x`},
		{"linux-64", `#!    /this/is/looooooooooooooooooooooooooooooooooooooooooooo\ \ ooooooo\ oooooo\ oooooo\ ooooooooooooooooo\ ooooooooooooooooooong/pyt\ hon -o "te  st" -x`, `#!/usr/bin/env pyt\ hon -o "te  st" -x`},
		{"linux-64", aaa, "#!/usr/bin/env python -o test 123 -x"},
		{"osx-arm64", aaa, "#!/usr/bin/env python -o test 123 -x"},
		{"linux-64", string(escaped), "#!/usr/bin/env python -o test 123 -x"},
	}
	for _, c := range cases {
		if got := ReplaceLongShebang(c.in, c.platform); got != c.want {
			t.Errorf("ReplaceLongShebang(%q, %s) = %q, want %q", c.in, c.platform, got, c.want)
		}
	}
}

// Cases from libmamba tests/src/core/test_cpp.cpp, TEST_CASE("shebang_regex_matches").
func TestShebangRegexp(t *testing.T) {
	cases := []struct{ in, whole, exe, args string }{
		{"#!/simple/shebang", "#!/simple/shebang", "/simple/shebang", ""},
		{"#!    /simple/shebang", "#!    /simple/shebang", "/simple/shebang", ""},
		{`#!/simple/shebang/escaped\ space --and --flags -x`, `#!/simple/shebang/escaped\ space --and --flags -x`, `/simple/shebang/escaped\ space`, " --and --flags -x"},
	}
	for _, c := range cases {
		for _, match := range []func(string) (string, string, bool){matchShebang, matchShebangRegexp} {
			exe, args, ok := match(c.in)
			if !ok || c.whole != c.in || exe != c.exe || args != c.args {
				t.Errorf("%q: %q %q %v", c.in, exe, args, ok)
			}
		}
	}
	// std::regex's "." does not match "\r", so CRLF shebangs are left alone.
	for _, in := range []string{"#!/bin/sh\r", "#!/bin/sh -x\r", "#!bin/sh", "#! ", "#!", "/bin/sh"} {
		if _, _, ok := matchShebang(in); ok {
			t.Errorf("matched %q", in)
		}
	}
}

// Cases from rattler crates/rattler/src/install/prefix_replacement/tests.rs,
// test_replace_long_shebang, as far as libmamba agrees. rattler's
// replace_shebang also substitutes the prefix and converts short shebangs
// whose new prefix contains a space; libmamba does neither, so those
// expectations differ (see TestReplaceTextPrefixSpacesLibmamba).
func TestReplaceLongShebangRattler(t *testing.T) {
	cases := []struct {
		platform, in, want string
	}{
		{"linux-64", "#!/path/to/executable -x 123", "#!/path/to/executable -x 123"},
		{"linux-64", "#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooong/executable -o test -x", "#!/usr/bin/env executable -o test -x"},
		{"osx-64", "#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooong/executable -o test -x", "#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooong/executable -o test -x"},
		{"linux-64", `#!/this/is/loooooooooooooooooooooooooooooooooooooooooooooooooooo\ oooooo\ oooooo\ oooooooooooooooooooooooooooooooooooong/exe\ cutable -o test -x`, `#!/usr/bin/env exe\ cutable -o test -x`},
		{"linux-64", `#!    /this/is/looooooooooooooooooooooooooooooooooooooooooooo\ \ ooooooo\ oooooo\ oooooo\ ooooooooooooooooo\ ooooooooooooooooooong/exe\ cutable -o "te  st" -x`, `#!/usr/bin/env exe\ cutable -o "te  st" -x`},
		{"linux-64", "#!/usr/bin/env perl", "#!/usr/bin/env perl"},
	}
	for _, c := range cases {
		if got := ReplaceLongShebang(c.in, c.platform); got != c.want {
			t.Errorf("ReplaceLongShebang(%q, %s) = %q, want %q", c.in, c.platform, got, c.want)
		}
	}
}

// Cases from rattler prefix_replacement/tests.rs: test_replace_shebang,
// replace_python_shebang and the "/with space" cases of
// test_replace_long_shebang. libmamba (primary) does not rewrite short
// shebangs when the new prefix contains a space, so the prefix is simply
// substituted.
func TestReplaceTextPrefixSpacesLibmamba(t *testing.T) {
	cases := []struct{ in, placeholder, prefix, want string }{
		{"#!/path/placeholder/executable -o test -x", "placeholder", "with space", "#!/path/with space/executable -o test -x"},
		{"#!/placeholder/perl", "/placeholder", "/with space", "#!/with space/perl"},
		{"#!/usr/bin/env perl", "/placeholder", "/with space", "#!/usr/bin/env perl"},
		{"#!/path/to/python3.12", "/path/to", "/new/prefix/with spaces/bin", "#!/new/prefix/with spaces/bin/python3.12"},
		{"#!/path/to/python3.12 -x 123", "/path/to", "/new/prefix/with spaces/bin", "#!/new/prefix/with spaces/bin/python3.12 -x 123"},
	}
	for _, c := range cases {
		if got := string(ReplaceTextPrefix([]byte(c.in), c.placeholder, c.prefix, "linux-64")); got != c.want {
			t.Errorf("ReplaceTextPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Cases from rattler prefix_replacement/tests.rs,
// test_copy_and_replace_textual_placeholder and
// test_replace_long_prefix_in_text_file.
func TestReplaceTextPrefix(t *testing.T) {
	cases := []struct{ in, placeholder, prefix, want string }{
		{"Hello, cruel world!", "cruel", "fabulous", "Hello, fabulous world!"},
		{"prefix_placeholder", "prefix_placeholder", "target_prefix", "target_prefix"},
		// Text mode replaces every occurrence and may change the length.
		{"a/p b/p\x00c/p", "/p", "/prefix", "a/prefix b/prefix\x00c/prefix"},
		// A short shebang is kept even when it contains the prefix.
		{"#!/PH/bin/python\nimport /PH\n", "/PH", "/x", "#!/x/bin/python\nimport /x\n"},
		// A file that is only a long shebang line.
		{"#!/p/bin/tool -x", "/p", "/" + strings.Repeat("d", 130), "#!/usr/bin/env tool -x"},
	}
	for _, c := range cases {
		if got := string(ReplaceTextPrefix([]byte(c.in), c.placeholder, c.prefix, "linux-64")); got != c.want {
			t.Errorf("ReplaceTextPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	input, err := os.ReadFile("testdata/shebang_test.txt")
	if err != nil {
		t.Fatal(err)
	}
	target := "/super/long/" + strings.Repeat("verylongstring/", 15)
	want := "#!/usr/bin/env executable -m 123123\n\n# just a comment\n\nimport sys\nimport re\n\nmain(...)\n"
	if got := string(ReplaceTextPrefix(input, "/this/is/placeholder", target, "linux-64")); got != want {
		t.Errorf("shebang_test.txt:\n%s\nwant\n%s", got, want)
	}
	// macOS allows 512-byte shebangs, so the long prefix stays.
	if got := string(ReplaceTextPrefix(input, "/this/is/placeholder", target, "osx-arm64")); !strings.HasPrefix(got, "#!"+target+"/executable -m 123123\n") {
		t.Errorf("osx shebang rewritten: %q", got)
	}
	// Windows: no shebang handling, forward slashes in the prefix.
	if got := string(ReplaceTextPrefix([]byte("#!P/x\nP"), "P", `C:\very\long`+strings.Repeat("x", 200), "win-64")); got != "#!C:/very/long"+strings.Repeat("x", 200)+"/x\nC:/very/long"+strings.Repeat("x", 200) {
		t.Errorf("windows replacement: %q", got)
	}
}

// Cases from rattler prefix_replacement/tests.rs:
// test_copy_and_replace_binary_placeholder, test_shorter_binary_placeholder
// and replace_binary_path_var. The offset-based and UTF-16/UTF-32 cases are
// not ported: libmamba (primary) only searches for the UTF-8 placeholder.
func TestReplaceBinaryPrefix(t *testing.T) {
	cases := []struct {
		in, placeholder, prefix, want string
		n                             int
	}{
		{"12345Hello, fabulous world!\x006789", "fabulous", "cruel", "12345Hello, cruel world!\x00\x00\x00\x006789", 1},
		{"beginrandomdataPATH=/placeholder/etc/share:/placeholder/bin/:\x00somemoretext", "/placeholder", "/target",
			"beginrandomdataPATH=/target/etc/share:/target/bin/:\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00somemoretext", 2},
		// Unterminated final string: padding goes at the end of the file.
		{"xx/placeholder/lib", "/placeholder", "/t", "xx/t/lib\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00", 1},
		// Same length: no padding.
		{"/aaaa\x00/aaaa", "/aaaa", "/bbbb", "/bbbb\x00/bbbb", 2},
		// No occurrence: unchanged.
		{"nothing here\x00", "/placeholder", "/t", "nothing here\x00", 0},
		// Empty placeholder replaces nothing.
		{"abc", "", "", "abc", 0},
	}
	for _, c := range cases {
		got, n, err := ReplaceBinaryPrefix([]byte(c.in), c.placeholder, c.prefix)
		if err != nil {
			t.Errorf("ReplaceBinaryPrefix(%q): %v", c.in, err)
			continue
		}
		if string(got) != c.want || n != c.n {
			t.Errorf("ReplaceBinaryPrefix(%q) = %q, %d; want %q, %d", c.in, got, n, c.want, c.n)
		}
		if len(got) != len(c.in) {
			t.Errorf("length changed: %d -> %d", len(c.in), len(got))
		}
	}

	for _, c := range []struct{ in, placeholder, prefix string }{
		{"short\x00", "short", "verylong"},
		{"short1234\x00", "short", "verylong"},
		{"no occurrence\x00", "short", "verylong"},
	} {
		in := []byte(c.in)
		if _, _, err := ReplaceBinaryPrefix(in, c.placeholder, c.prefix); !errors.Is(err, ErrPrefixTooLong) {
			t.Errorf("ReplaceBinaryPrefix(%q, %q, %q) error = %v, want ErrPrefixTooLong", c.in, c.placeholder, c.prefix, err)
		}
		if !bytes.Equal(in, []byte(c.in)) {
			t.Errorf("input modified")
		}
	}

	// A placeholder with a NUL byte made libmamba's search loop forever
	// (found by FuzzReplaceBinaryPrefix); it is refused.
	if _, _, err := ReplaceBinaryPrefix([]byte("\x00\x00000"), "\x00\x00", ""); !errors.Is(err, ErrInvalidPackage) {
		t.Errorf("NUL placeholder: %v", err)
	}
	// Many occurrences in one long string take linear time.
	big := bytes.Repeat([]byte("/ph/"), 1<<20)
	out, n, err := ReplaceBinaryPrefix(big, "/ph/", "/")
	if err != nil || n != 1<<20 || len(out) != len(big) || !bytes.HasPrefix(out, bytes.Repeat([]byte("/"), 1<<20)) || bytes.Count(out, []byte{0}) != 3<<20 {
		t.Errorf("long string: n=%d err=%v", n, err)
	}
}
