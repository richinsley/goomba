# lock test fixtures

- `explicit-envs/*.txt`: copied verbatim from rattler
  (`$GOOMBA_REFS/rattler/test-data/explicit-envs/`, BSD-3-Clause).
- `prefix-base/conda-meta/*.json`: the 28 conda-meta records of a
  micromamba 2.2.0 environment created with
  `micromamba create -n base-3.11 -c conda-forge python=3.11`
  (`$GOOMBA_REF_ROOT/envs/base-3.11`), with the machine-specific and bulky keys
  `files`, `paths_data`, `extracted_package_dir`, `package_tarball_full_path`
  and `link` removed and the keys sorted.
- `base-3.11.micromamba-explicit.txt`: the output of
  `micromamba env export --explicit` for that environment, the oracle for
  `WriteExplicit`'s package order.
