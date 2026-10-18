//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package goomba

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/richinsley/goomba/internal/rmtree"
	"golang.org/x/sys/unix"
)

// havePrefixLock reports that this platform has the prefix lock (flock),
// which RemoveIncomplete requires.
const havePrefixLock = true

// lockDir opens the directory dir, refusing a symlink there, and takes an
// exclusive, non-blocking flock on it (see lockOpenDir). The lock belongs
// to the open file, so it conflicts with any other open of the directory
// that locks it, in this process or another, and it leaves nothing behind
// in the prefix.
func lockDir(dir string) (*os.File, error) {
	d, err := openDirNoFollow(dir)
	if err != nil {
		return nil, err
	}
	if err := lockOpenDir(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// lockOpenDir takes an exclusive, non-blocking flock on the open directory
// d. When the lock is held, it tries for a shared lock: if that succeeds,
// only status checks (CheckComplete, tryLockShared) hold it, each for a
// moment, so it lets go and tries again, every millisecond for up to
// lockRetries times (about 50 ms). An exclusive holder makes it fail with
// ErrPrefixLocked at once; so do checks that hold the lock without a break
// for that long (a caller may try again). It never blocks indefinitely.
func lockOpenDir(d *os.File) error {
	for attempt := 0; ; attempt++ {
		err := flockNB(d, unix.LOCK_EX)
		if err == nil {
			return nil
		}
		if !isWouldBlock(err) {
			return fmt.Errorf("lock: %w", &os.PathError{Op: "flock", Path: d.Name(), Err: err})
		}
		err = flockNB(d, unix.LOCK_SH)
		switch {
		case isWouldBlock(err):
			return ErrPrefixLocked
		case err != nil:
			return fmt.Errorf("lock: %w", &os.PathError{Op: "flock", Path: d.Name(), Err: err})
		}
		_ = unix.Flock(int(d.Fd()), unix.LOCK_UN)
		if attempt >= lockRetries {
			return ErrPrefixLocked
		}
		time.Sleep(time.Millisecond)
	}
}

// lockRetries bounds how often lockOpenDir tries again while only shared
// holders hold the lock.
const lockRetries = 50

// isWouldBlock reports a non-blocking flock that found the lock held.
func isWouldBlock(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

// flockNB takes the flock how (LOCK_EX or LOCK_SH) on d without blocking,
// retrying on EINTR.
func flockNB(d *os.File, how int) error {
	for {
		err := unix.Flock(int(d.Fd()), how|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// openDirNoFollow opens the directory dir as lockDir does, refusing a
// symlink there, without locking it.
func openDirNoFollow(dir string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// tryLockShared takes a shared, non-blocking flock on the open directory d:
// ErrPrefixLocked while another open file holds lockDir's exclusive lock
// (an InstallLock or a RemoveIncomplete), in this process or another.
// Shared locks do not exclude each other, so concurrent status checks never
// see one another, and lockOpenDir waits for them briefly. unlockDir
// releases it.
func tryLockShared(d *os.File) error {
	err := flockNB(d, unix.LOCK_SH)
	switch {
	case err == nil:
		return nil
	case isWouldBlock(err):
		return ErrPrefixLocked
	}
	return fmt.Errorf("lock: %w", &os.PathError{Op: "flock", Path: d.Name(), Err: err})
}

// readState reads the state of the open prefix directory d, looking names
// up relative to it and following no symlink. A conda-meta that is not a
// directory (a symlink, a file) is an error. The directories it lists are
// opened with O_NOATIME where the system allows, so that a status check
// does not even change their access times.
func readState(d *os.File) (prefixState, error) {
	var st prefixState
	fd := int(d.Fd())
	n, err := countNamesAt(fd, ".", 2)
	if err != nil {
		return st, err
	}
	st.entries = n
	cfd, err := unix.Openat(fd, "conda-meta", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return st, nil
	case errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP):
		return st, fmt.Errorf("conda-meta: %w", errNotADir)
	case err != nil:
		return st, &os.PathError{Op: "open", Path: "conda-meta", Err: err}
	}
	defer unix.Close(cfd)
	st.meta = true
	if st.metaEntries, err = countNamesAt(cfd, ".", 1); err != nil {
		return st, err
	}
	exists := func(name string) (bool, error) {
		var s unix.Stat_t
		err := unix.Fstatat(cfd, name, &s, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		if err != nil {
			return false, &os.PathError{Op: "stat", Path: "conda-meta/" + name, Err: err}
		}
		return true, nil
	}
	if st.marker, err = exists(incompleteMarker); err != nil {
		return st, err
	}
	st.history, err = exists("history")
	return st, err
}

// countNamesAt counts the entries of the directory name below fd, up to
// max, through a fresh handle opened with O_NOATIME (without it where the
// system refuses it: a directory of another owner).
func countNamesAt(fd int, name string, max int) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	rfd, err := unix.Openat(fd, name, flags|oNoatime, 0)
	if errors.Is(err, unix.EPERM) && oNoatime != 0 {
		rfd, err = unix.Openat(fd, name, flags, 0)
	}
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: name, Err: err}
	}
	rd := os.NewFile(uintptr(rfd), name)
	defer rd.Close()
	names, err := rd.Readdirnames(max)
	if err == io.EOF {
		err = nil
	}
	return len(names), err
}

// unlockDir releases the lock and closes f.
func unlockDir(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}

// emptyDir empties the prefix directory open as d with emptyPrefix,
// working relative to its handle (openat, unlinkat) with package rmtree:
// never following a symlink, never entering a mount point below the prefix
// (the removal stops there, keeping the marker), so that a directory
// renamed or replaced meanwhile, above or inside the prefix, or a volume
// mounted inside it, cannot redirect the removal elsewhere. Read-only
// directories are made writable first, once they are known to be no mount
// point.
func emptyDir(d *os.File) error {
	return emptyPrefix(fdPrefixFS{int(d.Fd())})
}

// fdPrefixFS is the prefixFS of an open prefix directory.
type fdPrefixFS struct{ fd int }

// meta opens conda-meta without following a symlink or entering a mount
// point, writable by its owner (the removal only).
func (p fdPrefixFS) meta() (int, error) {
	cfd, err := rmtree.OpenDir(p.fd, "conda-meta")
	if errors.Is(err, rmtree.ErrMountPoint) {
		return -1, fmt.Errorf("conda-meta: %w", err)
	}
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: "conda-meta", Err: err}
	}
	_ = unix.Fchmod(cfd, 0o700)
	return cfd, nil
}

func (p fdPrefixFS) names(dir string) ([]string, error) {
	if dir == "" {
		return rmtree.Names(p.fd)
	}
	cfd, err := p.meta()
	if err != nil {
		return nil, err
	}
	defer unix.Close(cfd)
	return rmtree.Names(cfd)
}

func (p fdPrefixFS) removeAll(rel string) error {
	name, ok := strings.CutPrefix(rel, "conda-meta/")
	if !ok {
		return rmtree.RemoveAt(p.fd, rel, rel)
	}
	cfd, err := p.meta()
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(cfd)
	return rmtree.RemoveAt(cfd, name, rel)
}

func (p fdPrefixFS) ensureMarker() error {
	if err := unix.Mkdirat(p.fd, "conda-meta", 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
		return &os.PathError{Op: "mkdir", Path: "conda-meta", Err: err}
	}
	cfd, err := p.meta()
	if err != nil {
		return err
	}
	defer unix.Close(cfd)
	var st unix.Stat_t
	if err := unix.Fstatat(cfd, incompleteMarker, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return nil
	}
	mfd, err := unix.Openat(cfd, incompleteMarker, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return &os.PathError{Op: "create", Path: "conda-meta/" + incompleteMarker, Err: err}
	}
	return unix.Close(mfd)
}

// rmdirSame removes the directory path, which must still be the directory
// described by id: it opens path's parent without following a symlink,
// compares the entry with id and removes it with rmdir semantics
// (unlinkat AT_REMOVEDIR), so a path swapped meanwhile can at most cost an
// empty directory, never a file. A mount point gives EBUSY, a directory
// that is not empty ENOTEMPTY; an entry that is no longer id is left alone
// (nil).
func rmdirSame(path string, id fs.FileInfo) error {
	want, ok := id.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("no file identity")
	}
	pfd, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: filepath.Dir(path), Err: err}
	}
	defer unix.Close(pfd)
	base := filepath.Base(path)
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if uint64(st.Dev) != uint64(want.Dev) || uint64(st.Ino) != uint64(want.Ino) {
		return nil
	}
	if err := unix.Unlinkat(pfd, base, unix.AT_REMOVEDIR); err != nil {
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}

// isNotADir reports an error for something that should be a directory and
// is not (a file, or a symlink that was not followed).
func isNotADir(err error) bool {
	return errors.Is(err, errNotADir) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP)
}

// rmdir removes the empty directory path (rmdir semantics: never a file).
func rmdir(path string) error {
	if err := unix.Rmdir(path); err != nil {
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}

// setDirMode sets the mode of the open directory d (fchmod), whatever the
// umask.
func setDirMode(d *os.File, mode fs.FileMode) error { return d.Chmod(mode) }

// isBusy reports EBUSY: a mount point.
func isBusy(err error) bool { return errors.Is(err, unix.EBUSY) }

// isNotEmpty reports a directory that is not empty.
func isNotEmpty(err error) bool { return errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) }

// markIncomplete makes sure the prefix open as d has conda-meta and the
// incomplete marker in it.
func markIncomplete(d *os.File) error { return fdPrefixFS{int(d.Fd())}.ensureMarker() }
