# spec test data

- `parsed_versions.txt` is copied unchanged from rattler
  (`test-data/parsed_versions.txt`, github.com/conda/rattler, BSD-3-Clause,
  commit d195468). Each line is a version string from real repodata and the
  result of parsing it with conda's `VersionOrder`; the file header explains
  how it was generated.
- `realworld_specs.txt` holds match specs taken from the `depends` and
  `constrains` fields of conda-forge's `linux-64` and `noarch` repodata
  (March 2026): every spec with an unusual shape (brackets, globs, `|`,
  `!=`, `~=`, unusual spacing) plus a sample of the rest. See
  `TestRealWorldSpecs`.
- `fuzz/` holds regression inputs for the fuzz targets.
