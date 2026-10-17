//go:build !unix

package archive

import "io/fs"

// checkOwnDir has no ownership to check on this platform.
func checkOwnDir(fs.FileInfo) error { return nil }
