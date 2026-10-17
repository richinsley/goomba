//go:build unix

package fetch

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openNoFollow opens p for reading, refusing a symlink at p and never
// blocking on a FIFO planted there.
func openNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

// fileStamp returns the inode number and status-change time (ns) of f, or
// zeros if unknown. Unlike the mtime, an unprivileged process cannot set
// the change time back after modifying a file, so together with size and
// mtime it reveals any change.
func fileStamp(f *os.File) (ino uint64, ctime int64) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, 0
	}
	return uint64(st.Ino), int64(st.Ctim.Sec)*1e9 + int64(st.Ctim.Nsec)
}

// noUnixPerms explains a key that cannot be protected.
const noUnixPerms = "the cache must be on a filesystem with Unix ownership and permissions (FAT, exFAT, SMB/CIFS and similar mounts cannot protect it)"

// checkKeyFile refuses a key file that is not a regular file owned by the
// current user with no permissions for group or others.
func checkKeyFile(fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", ErrUnsafeCache)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: no owner information", ErrUnsafeCache)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: owned by uid %d, not the current user (uid %d); %s", ErrUnsafeCache, st.Uid, os.Geteuid(), noUnixPerms)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: mode %v grants access to others; %s", ErrUnsafeCache, fi.Mode().Perm(), noUnixPerms)
	}
	return nil
}
