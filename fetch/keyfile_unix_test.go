//go:build unix

package fetch

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is a FileInfo with chosen mode and owner.
type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return keyName }
func (f fakeInfo) Size() int64        { return 65 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func TestCheckKeyFile(t *testing.T) {
	me := uint32(os.Getuid())
	if err := checkKeyFile(fakeInfo{0o600, me}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []fakeInfo{
		{0o600, me + 1},              // another user's key
		{0o640, me},                  // group-readable
		{0o602, me},                  // world-writable
		{fs.ModeSymlink | 0o777, me}, // not a regular file
		{fs.ModeDir | 0o700, me},
	} {
		if err := checkKeyFile(bad); !errors.Is(err, ErrUnsafeCache) {
			t.Errorf("checkKeyFile(%v uid %d) = %v", bad.mode, bad.uid, err)
		}
	}
}
