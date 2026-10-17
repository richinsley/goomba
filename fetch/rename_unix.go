//go:build !windows

package fetch

import "os"

// renameOpen renames the file open as f from tmp to final and returns f,
// which keeps referring to the same, verified file.
func renameOpen(f *os.File, tmp, final string) (*os.File, error) {
	return f, os.Rename(tmp, final)
}
