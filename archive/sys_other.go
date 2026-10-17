//go:build !linux

package archive

// renameNoReplace renames src to dst, failing if dst exists.
func renameNoReplace(src, dst string) error { return renameChecked(src, dst) }
