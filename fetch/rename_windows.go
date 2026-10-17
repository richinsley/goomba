//go:build windows

package fetch

import (
	"fmt"
	"os"
)

// renameOpen renames the file open as f from tmp to final. Windows cannot
// rename a file Go opened, so f is closed, renamed and reopened, and the
// reopened file must have the size and mtime f had: a weaker guarantee than
// keeping the verified handle, as on other platforms.
func renameOpen(f *os.File, tmp, final string) (*os.File, error) {
	before, err := f.Stat()
	if err != nil {
		return f, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	g, err := os.Open(final)
	if err != nil {
		return nil, err
	}
	after, err := g.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		g.Close()
		return nil, fmt.Errorf("%w: %s changed while it was renamed", ErrUnsafeCache, final)
	}
	return g, nil
}
