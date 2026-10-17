//go:build unix

package archive

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type ownerInfo struct {
	fs.FileInfo
	uid uint32
}

func (o ownerInfo) Sys() any { return &syscall.Stat_t{Uid: o.uid} }

// checkFreshDir refuses a temporary directory that another user swapped
// in, or that is not empty.
func TestCheckFreshDir(t *testing.T) {
	dir := t.TempDir()
	open := func() (*os.File, fs.FileInfo) {
		f, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return f, fi
	}
	f, fi := open()
	if err := checkFreshDir(f, fi); err != nil {
		t.Fatalf("own empty directory: %v", err)
	}
	f.Close()
	f, fi = open()
	if err := checkFreshDir(f, ownerInfo{fi, uint32(os.Getuid()) + 1}); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("another user's directory: %v", err)
	}
	f.Close()
	os.WriteFile(filepath.Join(dir, "planted"), nil, 0o644)
	f, fi = open()
	defer f.Close()
	if err := checkFreshDir(f, fi); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("non-empty directory: %v", err)
	}

}
