# WP5c — `virtual`: a kernel-differential test for the trust walk

Read `AGENTS.md` first. You own `virtual/` test files only. Do not change
non-test code unless the new test finds a defect; if it does, fix the defect
minimally and report it.

## Goal

`trustedUnixExecutable` (`virtual/trust.go`) decides whether a program at a
fixed location may run. It resolves the path one component at a time with
lstat and readlink. Its correctness rests on resolving paths exactly as the
kernel does. During review, a throwaway test checked this against the real
kernel on random trees and found no mismatches in 279,000 queries. That test
was not kept. Add a permanent version.

## Test

`TestTrustWalkMatchesKernel` (Linux and other Unix systems; skip elsewhere):

- Build random trees under `t.TempDir()`: directories, regular files and
  symlinks. The symlinks must cover:
  - relative and absolute targets (absolute ones inside the tree);
  - `..` and `.` components, `//` and trailing slashes;
  - dangling links, link loops and chains near the 40-link limit.
- For random query paths, run the walk through a search FS that reports
  every entry as root-owned and not writable by others, so only path
  resolution is under test. Compare with the kernel:
  - the walk accepts a path exactly when `os.Stat` succeeds and yields a
    regular file;
  - every accepted walk returns a path that is `os.SameFile` with the
    kernel's target;
  - that path is clean (`filepath.Clean`) and contains no symlink component.
- Use a fixed seed, so the test is deterministic, and keep it fast: about
  300 trees by default. `-short` should take under 2 s. An environment
  variable such as `GOOMBA_TRUSTWALK_TREES` may raise the count; tests may
  read env vars.
- The tree must stay inside the temp dir. Absolute targets point into it,
  and nothing is created elsewhere.

Also add a 60 s fuzz target over the tree generator's seed, if that is
cheap.

## Gates

`scripts/gate.sh ./virtual/...`, plus one run with 5,000 trees under
`ulimit -v 16000000`, reporting the number of queries and mismatches.
