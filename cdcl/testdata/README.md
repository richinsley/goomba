# cdcl test data

`snapshots/` holds the insta snapshot files of resolvo v0.12.2 (BSD-3-Clause,
see NOTICE), copied verbatim:

- `solver__*.snap` from `$GOOMBA_REFS/resolvo/tests/solver/snapshots/`, the
  expected output of the solver tests ported in `solver_test.go`;
- `resolvo__solver__binary_encoding__test__at_most_once_tracker.snap` from
  `$GOOMBA_REFS/resolvo/src/solver/snapshots/`, used by
  `TestAtMostOnceTracker` in `internal_test.go`.

Each file is a YAML-ish header between `---` lines followed by the expected
text. Two snapshots have no test upstream (see `solver_test.go`).
