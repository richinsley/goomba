# goomba test fixtures

- `pkgs/*.conda`: real conda-forge packages, copied unchanged from the
  package cache of a micromamba 2.2.0 environment created with
  `micromamba create -n base-3.11 -c conda-forge python=3.11`
  (`$GOOMBA_REF_ROOT/pkgs`), also downloadable from
  `https://conda.anaconda.org/conda-forge/<subdir>/<file>`. Their licenses
  are recorded in each archive's `info/index.json`.
- `topological-sort/*.json`: rattler's topological sort test data
  (BSD-3-Clause). `big_resolution.json` is copied verbatim from
  `$GOOMBA_REFS/rattler/test-data/topological-sort/`; the other files are
  the JSON literals of the test functions in
  `$GOOMBA_REFS/rattler/crates/rattler_conda_types/src/repo_data/topological_sort.rs`
  (`get_resolved_packages_for_python`, `_numpy`, `_two_roots`,
  `_rootless_graph`, `_python_pip` (python plus its pip literal) and
  `get_packages_with_disconnected_cycle`), reformatted.
- `clobber/*`: rattler's clobbering test packages (BSD-3-Clause), copied
  unchanged from `$GOOMBA_REFS/rattler/test-data/clobber/`.
