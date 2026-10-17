package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// maxMessageName bounds archive-supplied strings quoted in error messages.
const maxMessageName = 256

// quote formats an archive-supplied name for an error message, truncated
// so that a hostile name cannot flood logs.
func quote(s string) string {
	if len(s) > maxMessageName {
		return strconv.Quote(s[:maxMessageName]) + "..."
	}
	return strconv.Quote(s)
}

func truncate(s string) string {
	if len(s) > maxMessageName {
		return s[:maxMessageName] + "..."
	}
	return s
}

// boundErr truncates the paths carried by an *fs.PathError or
// *os.LinkError, which may come from archive-supplied names, keeping the
// underlying error for errors.Is.
func boundErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) && len(pe.Path) > maxMessageName {
		return &fs.PathError{Op: pe.Op, Path: truncate(pe.Path), Err: pe.Err}
	}
	var le *os.LinkError
	if errors.As(err, &le) && (len(le.Old) > maxMessageName || len(le.New) > maxMessageName) {
		return &os.LinkError{Op: le.Op, Old: truncate(le.Old), New: truncate(le.New), Err: le.Err}
	}
	return err
}

// onWindows is true when backslash and colon are path syntax on the host.
const onWindows = runtime.GOOS == "windows"

// hasDrive reports whether p starts with a Windows drive letter, "C:".
func hasDrive(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0] | 0x20
	return 'a' <= c && c <= 'z'
}

// checkPathLimits bounds a name or target by length and component count
// before any further work on it.
func checkPathLimits(s string, lim Limits) error {
	if len(s) > lim.MaxPathLen {
		return fmt.Errorf("%w: name of %d bytes over %d", ErrLimit, len(s), lim.MaxPathLen)
	}
	if n := strings.Count(s, "/") + 1; n > lim.MaxPathDepth {
		return fmt.Errorf("%w: name of %d components over %d", ErrLimit, n, lim.MaxPathDepth)
	}
	return nil
}

// cleanName validates an archive entry or hard link name and returns it as
// a clean slash-separated path relative to the destination; "" names the
// destination itself. It refuses names over the path limits, NUL bytes,
// absolute, drive and UNC paths, and any ".." component, also between
// backslashes. On Windows, where they are path syntax, it refuses
// backslashes and colons outright.
func cleanName(name string, lim Limits) (string, error) {
	if err := checkPathLimits(name, lim); err != nil {
		return "", err
	}
	switch {
	case name == "":
		return "", fmt.Errorf("%w: empty name", ErrUnsafePath)
	case strings.IndexByte(name, 0) >= 0:
		return "", fmt.Errorf("%w: NUL byte", ErrUnsafePath)
	case name[0] == '/' || name[0] == '\\':
		return "", fmt.Errorf("%w: absolute or UNC path", ErrUnsafePath)
	case hasDrive(name):
		return "", fmt.Errorf("%w: drive path", ErrUnsafePath)
	case onWindows && strings.ContainsAny(name, `\:`):
		return "", fmt.Errorf("%w: backslash or colon in name", ErrUnsafePath)
	}
	parts := make([]string, 0, 8)
	for p := range strings.SplitSeq(name, "/") {
		switch p {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w: %q component", ErrUnsafePath, "..")
		}
		for q := range strings.SplitSeq(p, `\`) {
			if q == ".." {
				return "", fmt.Errorf("%w: %q component", ErrUnsafePath, "..")
			}
		}
		parts = append(parts, p)
	}
	// Dropping "." components can expose a backslash or drive prefix, as
	// in "./\x" or "./C:/x".
	if len(parts) > 0 && (parts[0][0] == '\\' || hasDrive(parts[0])) {
		return "", fmt.Errorf("%w: absolute, UNC or drive path", ErrUnsafePath)
	}
	return strings.Join(parts, "/"), nil
}

// checkLinkTarget refuses symlink targets over the path limits, empty,
// containing NUL, or absolute, drive or UNC paths. Relative targets are
// resolved later against the finished tree.
func checkLinkTarget(t string, lim Limits) error {
	if err := checkPathLimits(t, lim); err != nil {
		return err
	}
	switch {
	case t == "":
		return fmt.Errorf("%w: empty symlink target", ErrUnsafePath)
	case strings.IndexByte(t, 0) >= 0:
		return fmt.Errorf("%w: NUL byte in symlink target", ErrUnsafePath)
	case t[0] == '/' || t[0] == '\\':
		return fmt.Errorf("%w: absolute symlink target %s", ErrUnsafePath, quote(t))
	case hasDrive(t):
		return fmt.Errorf("%w: drive symlink target %s", ErrUnsafePath, quote(t))
	}
	return nil
}

// splitTarget splits a symlink target into components the way the host
// resolves it.
func splitTarget(t string) []string {
	if onWindows {
		return strings.FieldsFunc(t, func(r rune) bool { return r == '/' || r == '\\' })
	}
	return strings.Split(t, "/")
}
