// Ported from libmamba src/core/link.cpp (LinkPackage::link_path,
// replace_long_shebang) and src/core/link.hpp (shebang_regex,
// MAX_SHEBANG_LENGTH) (BSD-3-Clause, see NOTICE).

package link

import (
	"bytes"
	"fmt"
	"strings"
)

// matchShebang matches a shebang line against libmamba's shebang_regex
//
//	^(#!(?:[ ]*)(/(?:\\ |[^ \n\r\t])*)(.*))$
//
// and returns the executable path (group 2, which may contain escaped
// spaces) and the rest of the line (group 3). std::regex's ECMAScript "."
// matches neither "\n" nor "\r", so a line containing either does not match.
// The greedy match never needs to backtrack, so a scan gives the same groups.
func matchShebang(s string) (exe, rest string, ok bool) {
	if !strings.HasPrefix(s, "#!") {
		return "", "", false
	}
	i := 2
	for i < len(s) && s[i] == ' ' {
		i++
	}
	if i >= len(s) || s[i] != '/' {
		return "", "", false
	}
	j := i
	for j < len(s) {
		if s[j] == '\\' && j+1 < len(s) && s[j+1] == ' ' {
			j += 2
			continue
		}
		if c := s[j]; c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			break
		}
		j++
	}
	if strings.ContainsAny(s[j:], "\n\r") {
		return "", "", false
	}
	return s[i:j], s[j:], true
}

// maxShebangLength returns libmamba's MAX_SHEBANG_LENGTH for the target
// platform: 127 on Linux, 512 elsewhere.
func maxShebangLength(platform string) int {
	if strings.HasPrefix(platform, "linux-") || platform == "" {
		return 127
	}
	return 512
}

func isWindows(platform string) bool { return strings.HasPrefix(platform, "win-") }
func isOSX(platform string) bool     { return strings.HasPrefix(platform, "osx-") }

// ReplaceLongShebang returns shebang unchanged if it is at most the
// platform's maximum shebang length (127 bytes on Linux, 512 on macOS and
// other targets); otherwise it rewrites "#!/long/path/to/exe args" to
// "#!/usr/bin/env exe args". A shebang the regular expression does not match
// (for example one containing "\r") is returned unchanged, as in libmamba.
func ReplaceLongShebang(shebang, platform string) string {
	if len(shebang) <= maxShebangLength(platform) {
		return shebang
	}
	exe, rest, ok := matchShebang(shebang)
	if !ok {
		return shebang
	}
	if i := strings.LastIndexByte(exe, '/'); i >= 0 {
		exe = exe[i+1:]
	}
	return "#!/usr/bin/env " + exe + rest
}

// ReplaceTextPrefix replaces every occurrence of placeholder in data with
// prefix, as libmamba does for text-mode files. On non-Windows targets a
// first line starting with "#!" that ends up longer than the maximum shebang
// length is rewritten with ReplaceLongShebang. On Windows targets backslashes
// in prefix are written as forward slashes. data is not modified.
func ReplaceTextPrefix(data []byte, placeholder, prefix, platform string) []byte {
	if isWindows(platform) {
		prefix = strings.ReplaceAll(prefix, `\`, "/")
	}
	var out []byte
	if placeholder == "" {
		out = bytes.Clone(data)
	} else {
		out = bytes.ReplaceAll(data, []byte(placeholder), []byte(prefix))
	}
	if isWindows(platform) || !bytes.HasPrefix(out, []byte("#!")) {
		return out
	}
	end := bytes.IndexByte(out, '\n')
	if end < 0 {
		end = len(out)
	}
	first := string(out[:end])
	if len(first) <= maxShebangLength(platform) {
		return out
	}
	repl := ReplaceLongShebang(first, platform)
	if repl == first {
		return out
	}
	res := make([]byte, 0, len(out)-len(first)+len(repl))
	res = append(res, repl...)
	return append(res, out[end:]...)
}

// ReplaceBinaryPrefix replaces placeholder with prefix inside each
// NUL-terminated string of data, padding the string with NULs so that the
// output has exactly the length of data. It returns the new contents and the
// number of occurrences replaced. It fails with ErrPrefixTooLong when prefix
// is longer than placeholder, whether or not placeholder occurs, and with
// ErrInvalidPackage when placeholder contains a NUL byte. An empty
// placeholder replaces nothing. data is not modified.
//
// The result is libmamba's: for each occurrence, the bytes from the end of
// the placeholder up to the next NUL move left to follow the new prefix, and
// the freed bytes become NULs in front of that NUL. Because the search
// resumes after each inserted prefix, that is the same as replacing the
// non-overlapping occurrences of each NUL-terminated string and padding the
// string to its old length, which this implementation does in linear time
// (libmamba's in-place shifting is quadratic in the number of occurrences).
func ReplaceBinaryPrefix(data []byte, placeholder, prefix string) ([]byte, int, error) {
	if len(prefix) > len(placeholder) {
		return nil, 0, fmt.Errorf("link: replace binary prefix: %w (%d > %d bytes)", ErrPrefixTooLong, len(prefix), len(placeholder))
	}
	if strings.IndexByte(placeholder, 0) >= 0 {
		return nil, 0, fmt.Errorf("link: replace binary prefix: %w: placeholder contains a NUL byte", ErrInvalidPackage)
	}
	if placeholder == "" || !bytes.Contains(data, []byte(placeholder)) {
		return bytes.Clone(data), 0, nil
	}
	ph, repl := []byte(placeholder), []byte(prefix)
	out := make([]byte, 0, len(data))
	n := 0
	for rest := data; len(rest) > 0; {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			end = len(rest)
		}
		seg := rest[:end]
		if k := bytes.Count(seg, ph); k > 0 {
			n += k
			before := len(out)
			out = append(out, bytes.ReplaceAll(seg, ph, repl)...)
			for len(out)-before < len(seg) {
				out = append(out, 0)
			}
		} else {
			out = append(out, seg...)
		}
		if end < len(rest) {
			out = append(out, 0)
			end++
		}
		rest = rest[end:]
	}
	return out, n, nil
}
