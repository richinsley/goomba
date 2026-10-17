// Package virtual detects virtual packages (__glibc, __linux, __unix, __osx,
// __win, __archspec, __cuda, __cuda_arch) for the current machine. Every
// value can be overridden by the caller.
//
// Detect probes the host without cgo; Defaults describes any platform
// without probing, for locking environments for other machines. Records
// are fresh on every call, and every function is safe for concurrent use.
//
// # NVIDIA GPUs
//
// On Linux and Windows, Detect reports __cuda and __cuda_arch from the
// output of nvidia-smi, as conda and rattler do. goomba never searches
// PATH. It runs Options.NvidiaSMI when that is set, or else the first
// usable program at these locations:
//
//   - Linux: /usr/bin/nvidia-smi (distribution packages, NVIDIA's
//     installer), /usr/lib/wsl/lib/nvidia-smi (WSL2, assuming WSL presents
//     it as root's, which has not been checked on a WSL2 machine),
//     /usr/local/nvidia/bin/nvidia-smi (GPU containers) and
//     /run/current-system/sw/bin/nvidia-smi (NixOS). A location is usable
//     only if, resolving it one component at a time, every directory on the
//     way is owned by root and not writable by group or others, or is a
//     sticky directory in which root owns the entry used (as in /nix/store),
//     and the program is a regular file owned by root and not writable by
//     group or others. Then nobody but root can swap it, and the resolved
//     path is what runs, named after the location (for multi-call binaries
//     such as busybox). getconf, the only other program this package runs
//     (for __glibc when libc.so.6 cannot be read), follows the same policy.
//     The check covers the program file and the path to it, not what the
//     program loads (a #! interpreter, the ELF loader, shared libraries):
//     those are root's configuration, and the empty environment removes
//     LD_PRELOAD, LD_LIBRARY_PATH and PATH. With the kernel's
//     protected_hardlinks off, another user could hard-link a root-owned
//     file into a sticky directory where it would pass as root's entry; no
//     built-in location has a file or link directly in a sticky directory.
//   - Windows: nvidia-smi.exe in the system directory, then the newest
//     DriverStore\FileRepository\nv*\nvidia-smi.exe below it (searched only
//     when the system directory holds nvcuda.dll), then
//     NVIDIA Corporation\NVSMI\nvidia-smi.exe under Program Files. On a
//     default installation only administrators can write these directories;
//     goomba relies on that and does not check their ACLs.
//
// Only the first usable location runs: if its output is unusable, no other
// is tried. Set Options.NvidiaSMI when the locations do not fit the
// machine:
//   - inside WSL2, a stray /usr/bin/nvidia-smi (from a Linux driver package,
//     which cannot work there) hides /usr/lib/wsl/lib/nvidia-smi;
//   - in a user-namespace container (rootless Podman or Docker), the host's
//     nvidia-smi appears owned by the overflow user (uid 65534), not root,
//     so it is not used and no CUDA is reported;
//   - a 32-bit goomba on 64-bit Windows sees the 32-bit system directory
//     and Program Files and finds no nvidia-smi; pass the 64-bit one through
//     the Sysnative alias (C:\Windows\Sysnative\nvidia-smi.exe).
//
// nvidia-smi runs with an empty environment (on Windows only SystemRoot,
// taken from the API), in / (on Windows in the system directory), for at
// most 10 seconds per query, and output beyond 64 KiB is discarded.
package virtual
