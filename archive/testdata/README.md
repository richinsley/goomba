# archive test fixtures

Real conda packages, unmodified:

| File | Origin |
| --- | --- |
| `wheel-0.46.3-pyhd8ed1ab_0.conda` | conda-forge noarch, copied from a micromamba package cache (`$GOOMBA_REF_ROOT/pkgs`) |
| `libzlib-1.3.2-h25fd6f3_2.conda` | conda-forge linux-64, copied from a micromamba package cache (`$GOOMBA_REF_ROOT/pkgs`) |
| `bzip2-1.0.8-h7f98852_4.tar.bz2` | https://conda.anaconda.org/conda-forge/linux-64/bzip2-1.0.8-h7f98852_4.tar.bz2 (used by rattler `tests/extract.rs`) |
| `bzip2-1.0.8-hd590300_5.conda` | https://conda.anaconda.org/conda-forge/linux-64/bzip2-1.0.8-hd590300_5.conda (used by rattler `tests/extract.rs`) |
| `ca-certificates-2024.7.4-hbcca054_0.conda` | rattler `crates/rattler_package_streaming/tests/resources/` (BSD-3-Clause), a hand re-zipped package. Re-zipped again here with Go's archive/zip: the same three members, Deflate, zip data descriptors, nested in `ca-certificates-2024.7.4-hbcca054_0/` (the original nested them under a user's home path) |

`golden/<dist>.json` records micromamba 2.2.0's extraction of each fixture
(`micromamba package extract`): every path with its type, file mode, sha256,
mtime and symlink target. Regenerate with

    GOOMBA_MICROMAMBA=/path/to/micromamba go test ./archive -run 'TestRealFixturesMatchMicromamba$' -args -update-golden
