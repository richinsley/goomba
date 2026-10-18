package goomba

import (
	"os"

	"golang.org/x/sys/windows"
)

// openDirNoFollow opens the directory dir without following a final
// reparse point (symlink or junction: the caller's Stat then sees no
// directory) and sharing it for reading, writing and deletion, so that
// holding it open never keeps another process, or a removal of the prefix
// by path, from deleting or renaming it.
func openDirNoFollow(dir string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return os.NewFile(uintptr(h), dir), nil
}
