//go:build !unix

package archive

import "io/fs"

func nlink(fs.FileInfo) uint64 { return 1 }

func ownedByMe(fs.FileInfo) bool { return true }
