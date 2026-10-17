# solve test data

Copied from rattler (https://github.com/conda/rattler, BSD-3-Clause, see
NOTICE), commit d195468495813cbd0543ab0ec2727e89bc6662b6.

- `channels/dummy/linux-64/repodata.json`: from
  `test-data/channels/dummy/linux-64/repodata.json`, normalized so that
  goomba's strict repodata loader (`repodata.LoadRepodataJSON`) keeps every
  record, as rattler's lenient loader does. The records themselves are
  unchanged; only their keys moved:
  - `.conda` entries listed under `packages` moved to `packages.conda`
    (`cuda-version-12.5-hd4f0392_3`, `foo-3.0.2-py36h1af98f8_{1,2,3}`);
  - keys renamed to the `name-version-build` of their record:
    `bar-1.0-unix_py36h1af98f8_2.tar.bz2` to `bar-1.2.3-...`,
    `baz-1.0-unix_py36h1af98f8_2.tar.bz2` to `baz-1.2.3-...`,
    `issue_717-2.1-bla_1.{tar.bz2,conda}` to `issue_717-2.1-issue_717.*`,
    `conda-only-0.1.0-foobar_0.conda` to `conda-only-0.1.0-foobar.conda`.
- `channels/pytorch/linux-64/repodata.json.gz`: gzip of
  `test-data/channels/pytorch/linux-64/repodata.json`, unchanged.
- `snapshots/backends__resolvo__*.snap`: from
  `crates/rattler_solve/tests/backends/snapshots/`.
- `snapshots/sorting__test_ordering_*_highest.snap`: from
  `crates/rattler_solve/tests/snapshots/`.

The real-world snapshots (`solve_python`, `solve_quetz`, ...) and the
ordering snapshots are checked against rattler's frozen conda-forge test
repodata, which the live tests download (about 260 MB) from
https://rattler-test.pixi.run/test-data/channels/conda-forge/.

`fuzz/` holds regression inputs for `FuzzSolveSmallChannel`.
