// Ported from libmamba src/core/virtual_packages.cpp get_archspec_x86_64 and
// get_archspec (BSD-3-Clause, see NOTICE).

package virtual

// x86Features are the CPU features libmamba checks to pick an x86-64
// microarchitecture level.
type x86Features struct {
	AVX512F, AVX512BW, AVX512CD, AVX512DQ, AVX512VL bool
	AVX, AVX2, BMI1, BMI2, FMA                      bool
	POPCNT, SSE3, SSSE3, SSE41, SSE42               bool
}

// x86Level returns the archspec name of the highest x86-64 microarchitecture
// level f supports: x86_64_v4, x86_64_v3, x86_64_v2 or x86_64. It checks the
// same feature subsets as libmamba, which are what conda-forge's
// microarch-level packages key on.
func x86Level(f x86Features) string {
	switch {
	case f.AVX512F && f.AVX512BW && f.AVX512CD && f.AVX512DQ && f.AVX512VL:
		return "x86_64_v4"
	case f.AVX && f.AVX2 && f.BMI1 && f.BMI2 && f.FMA:
		return "x86_64_v3"
	case f.POPCNT && f.SSE3 && f.SSSE3 && f.SSE41 && f.SSE42:
		return "x86_64_v2"
	}
	return "x86_64"
}

// hostArchspecFor returns the __archspec build string of a host: the x86-64
// level from features on amd64, and libmamba's platform-derived name
// (x86, aarch64, arm64, ppc64le, ...) everywhere else.
func hostArchspecFor(goarch, platform string, features func() x86Features) string {
	if goarch == "amd64" {
		return x86Level(features())
	}
	if _, arch, ok := splitPlatform(platform); ok {
		return archspecForPlatform(arch)
	}
	return "0"
}
