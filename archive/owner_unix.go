//go:build unix

package archive

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkOwnDir refuses a temporary directory that is not owned by the
// current user: another user replaced it before it was opened.
func checkOwnDir(fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: temporary directory is not owned by the current user", ErrUnsafePath)
	}
	return nil
}
