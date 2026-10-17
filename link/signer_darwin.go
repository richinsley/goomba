//go:build darwin

package link

import (
	"context"
	"fmt"
	"os/exec"
)

// defaultSigner ad-hoc signs a binary with "/usr/bin/codesign -s - -f",
// as libmamba's codesign does (src/core/util_os.cpp). The process is
// killed when ctx is cancelled.
func defaultSigner(ctx context.Context) func(string) error {
	return func(path string) error {
		out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-s", "-", "-f", path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("codesign %s: %w: %s", path, err, out)
		}
		return nil
	}
}
