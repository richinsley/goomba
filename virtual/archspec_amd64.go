//go:build amd64

package virtual

import (
	"runtime"

	"golang.org/x/sys/cpu"
)

// hostArchspec returns this machine's __archspec build string.
func hostArchspec() string {
	return hostArchspecFor(runtime.GOARCH, HostPlatform(), func() x86Features {
		return x86Features{
			AVX512F:  cpu.X86.HasAVX512F,
			AVX512BW: cpu.X86.HasAVX512BW,
			AVX512CD: cpu.X86.HasAVX512CD,
			AVX512DQ: cpu.X86.HasAVX512DQ,
			AVX512VL: cpu.X86.HasAVX512VL,
			AVX:      cpu.X86.HasAVX,
			AVX2:     cpu.X86.HasAVX2,
			BMI1:     cpu.X86.HasBMI1,
			BMI2:     cpu.X86.HasBMI2,
			FMA:      cpu.X86.HasFMA,
			POPCNT:   cpu.X86.HasPOPCNT,
			SSE3:     cpu.X86.HasSSE3,
			SSSE3:    cpu.X86.HasSSSE3,
			SSE41:    cpu.X86.HasSSE41,
			SSE42:    cpu.X86.HasSSE42,
		}
	})
}
