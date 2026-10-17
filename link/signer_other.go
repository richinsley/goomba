//go:build !darwin

package link

import "context"

// defaultSigner returns nil: binaries are only re-signed when running on
// macOS.
func defaultSigner(context.Context) func(string) error { return nil }
