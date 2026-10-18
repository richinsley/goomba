package rmtree

import (
	"errors"

	"golang.org/x/sys/unix"
)

// openSubdir opens the directory name below fd with openat2, refusing
// symlinks and any mount crossing; without openat2 it falls back to
// openSubdirCompare.
func openSubdir(fd int, name string) (int, error) {
	cfd, err := unix.Openat2(fd, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS,
	})
	switch {
	case err == nil:
		return cfd, nil
	case errors.Is(err, unix.EXDEV):
		return -1, ErrMountPoint
	case errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM):
		// No openat2 (Linux < 5.6), or a seccomp filter refusing it.
		return openSubdirCompare(fd, name)
	}
	return -1, err
}

// openSubdirCompare opens the directory name below fd without following a
// symlink and refuses it if it lies on another mount than fd: by mount ID
// (statx STATX_MNT_ID) where the kernel reports it, else by device number,
// which cannot tell a bind mount of the same file system.
func openSubdirCompare(fd int, name string) (int, error) {
	cfd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	same, err := sameMount(fd, cfd)
	if err == nil && !same {
		err = ErrMountPoint
	}
	if err != nil {
		unix.Close(cfd)
		return -1, err
	}
	return cfd, nil
}

// sameMount reports whether the open files a and b are on the same mount.
func sameMount(a, b int) (bool, error) {
	var sa, sb unix.Statx_t
	ea := unix.Statx(a, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &sa)
	eb := unix.Statx(b, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &sb)
	if ea == nil && eb == nil && sa.Mask&unix.STATX_MNT_ID != 0 && sb.Mask&unix.STATX_MNT_ID != 0 {
		return sa.Mnt_id == sb.Mnt_id, nil
	}
	return sameDevice(a, b)
}

// sameDevice reports whether the open files a and b are on the same
// device.
func sameDevice(a, b int) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Fstat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Fstat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}
