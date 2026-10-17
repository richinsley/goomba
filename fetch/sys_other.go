//go:build !linux

package fetch

// renameNoReplace renames src to dst, failing if dst exists.
func renameNoReplace(src, dst string) error { return renameChecked(src, dst) }
