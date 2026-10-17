//go:build !unix

package fetch

import "io/fs"

// checkCacheDir checks nothing on this platform: ownership and modes are
// not examined (on Windows, a cache under the user's profile is protected
// by its default ACLs; a cache elsewhere must be protected by the caller).
func checkCacheDir(string) error { return nil }

// checkPrivate checks nothing on this platform; see checkCacheDir.
func checkPrivate(string, fs.FileInfo) error { return nil }
