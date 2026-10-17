//go:build !amd64

package virtual

import "runtime"

// hostArchspec returns this machine's __archspec build string.
func hostArchspec() string {
	return hostArchspecFor(runtime.GOARCH, HostPlatform(), func() x86Features { return x86Features{} })
}
