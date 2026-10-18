//go:build darwin || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package rmtree

import "golang.org/x/sys/unix"

// openSubdir opens the directory name below fd without following a
// symlink and refuses it if it is on another device than fd. A bind (or
// nullfs) mount of the same file system is not detected.
func openSubdir(fd int, name string) (int, error) {
	cfd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var sp, sc unix.Stat_t
	err = unix.Fstat(fd, &sp)
	if err == nil {
		err = unix.Fstat(cfd, &sc)
	}
	if err == nil && sp.Dev != sc.Dev {
		err = ErrMountPoint
	}
	if err != nil {
		unix.Close(cfd)
		return -1, err
	}
	return cfd, nil
}
