package link

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/richinsley/goomba/record"
)

// plannedSpecs are packages of every kind Link handles: plain files,
// symlinks, directories, prefix placeholders, no_link, the legacy
// info/files, noarch: python with .pyc candidates and entry points, and an
// allowed link script.
func plannedSpecs() map[string]pkgSpec {
	return map[string]pkgSpec{
		"basic":  basicSpec(testPlaceholder()),
		"noarch": noarchSpec(),
		"legacy": {name: "old", legacy: true, files: []pkgFile{
			{path: "bin/old", data: "#!/opt/anaconda1anaconda2anaconda3/bin/sh\n", mode: 0o755, placeholder: "/opt/anaconda1anaconda2anaconda3", fileMode: FileModeText},
			{path: "lib/libold.so.1", data: "x"},
			{path: "lib/libold.so", symlink: "libold.so.1"},
			{path: "share/copied", data: "c", noLink: true},
		}},
		"no_link symlink": {name: "a", files: []pkgFile{
			{path: "lib/real", data: "content"},
			{path: "lib/alias", symlink: "real", noLink: true},
		}},
		"directories": {name: "d", files: []pkgFile{
			{path: "share/empty", dir: true},
			{path: "share/x/y", data: "y"},
		}},
	}
}

// TestPlannedPathsMatchLink: PlannedPaths lists exactly the paths, path
// types and order of Installed.Paths, for every kind of package.
func TestPlannedPathsMatchLink(t *testing.T) {
	ctx := context.Background()
	for name, spec := range plannedSpecs() {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), spec)
			opt := Options{Prefix: filepath.Join(tmp, "env"), Platform: "linux-64", Python: pythonInfo()}
			planned, err := PlannedPaths(ctx, pkgDir, rec, opt)
			if err != nil {
				t.Fatal(err)
			}
			inst, err := Link(ctx, pkgDir, rec, opt)
			if err != nil {
				t.Fatal(err)
			}
			var want []PlannedPath
			for _, p := range inst.Paths {
				want = append(want, PlannedPath{Path: p.Path, PathType: p.PathType})
			}
			if !reflect.DeepEqual(planned, want) {
				t.Errorf("PlannedPaths\n %v\nLink\n %v", planned, want)
			}
		})
	}
}

// TestPlannedPathsNoarch: the relocation, .pyc and entry point paths.
func TestPlannedPathsNoarch(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), noarchSpec())
	got, err := PlannedPaths(context.Background(), pkgDir, rec, Options{Prefix: filepath.Join(tmp, "env"), Platform: "linux-64", Python: pythonInfo()})
	if err != nil {
		t.Fatal(err)
	}
	sp := "lib/python3.11/site-packages/"
	want := []PlannedPath{
		{sp + "pkg/__init__.py", PathHardlink}, {sp + "pkg/mod.py", PathHardlink},
		{sp + "pkg-1.0.dist-info/METADATA", PathHardlink}, {sp + "pkg/data.pyi", PathHardlink},
		{"bin/pkg-script", PathHardlink}, {"etc/pkg.conf", PathHardlink},
		{sp + "pkg/__pycache__/__init__.cpython-311.pyc", PathPycFile}, {sp + "pkg/__pycache__/mod.cpython-311.pyc", PathPycFile},
		{"bin/pkg", PathUnixPythonEntryPoint}, {"bin/pkg-alt", PathUnixPythonEntryPoint},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if tree(t, filepath.Join(tmp, "env")) != nil {
		t.Error("PlannedPaths wrote to the prefix")
	}
}

// TestPlannedPathsErrors: PlannedPaths refuses what Link's planning
// refuses, with the same errors, and writes nothing.
func TestPlannedPathsErrors(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		spec   pkgSpec
		mutate func(*Options, *record.PackageRecord)
		want   error
	}{
		{"no python", noarchSpec(), func(o *Options, _ *record.PackageRecord) { o.Python = nil }, ErrNoPython},
		{"invalid entry point", func() pkgSpec {
			s := noarchSpec()
			s.entryPoints = append(s.entryPoints, "evil = os:system('x')")
			return s
		}(), nil, ErrInvalidEntryPoint},
		{"entry point clobbers package file", func() pkgSpec {
			s := noarchSpec()
			s.entryPoints = append(s.entryPoints, "pkg-script = pkg:main")
			return s
		}(), nil, ErrInvalidPackage},
		{"link script", pkgSpec{name: "a", files: []pkgFile{{path: "bin/.a-post-link.sh", data: "x"}}}, nil, ErrLinkScript},
		{"inside conda-meta", pkgSpec{name: "a", files: []pkgFile{{path: "conda-meta/x.json", data: "x"}}}, nil, ErrUnsafePath},
		{"relative prefix", basicSpec(testPlaceholder()), func(o *Options, _ *record.PackageRecord) { o.Prefix = "env" }, ErrUnsafePath},
		{"prefix too long", basicSpec(testPlaceholder()), func(o *Options, _ *record.PackageRecord) {
			o.Prefix = "/" + strings.Repeat("p", 300)
		}, ErrPrefixTooLong},
		{"windows entry points", noarchSpec(), func(o *Options, _ *record.PackageRecord) {
			o.Platform = "win-64"
			o.Python = &PythonInfo{Version: "3.11", SitePackagesPath: "Lib/site-packages", BinDir: "Scripts", Executable: "python.exe"}
		}, ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), tc.spec)
			opt := Options{Prefix: filepath.Join(tmp, "env"), Platform: "linux-64", Python: pythonInfo()}
			if tc.mutate != nil {
				tc.mutate(&opt, rec)
			}
			_, perr := PlannedPaths(ctx, pkgDir, rec, opt)
			_, lerr := Link(ctx, pkgDir, rec, opt)
			if perr == nil || lerr == nil {
				t.Fatalf("PlannedPaths: %v; Link: %v", perr, lerr)
			}
			if tc.want != nil && (!errors.Is(perr, tc.want) || !errors.Is(lerr, tc.want)) {
				t.Errorf("PlannedPaths: %v; Link: %v; want %v", perr, lerr, tc.want)
			}
			if perr.Error() != lerr.Error() {
				t.Errorf("different errors:\nPlannedPaths %v\nLink         %v", perr, lerr)
			}
		})
	}
	ctx2, cancel := context.WithCancel(ctx)
	cancel()
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), basicSpec(testPlaceholder()))
	if _, err := PlannedPaths(ctx2, pkgDir, rec, Options{Prefix: filepath.Join(tmp, "env")}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	if _, err := PlannedPaths(ctx, pkgDir, nil, Options{Prefix: filepath.Join(tmp, "env")}); err == nil {
		t.Error("nil record accepted")
	}
}

// TestExistsError: an existing destination is an *ExistsError naming the
// planned path, also when it is reached through a directory symlink or is
// another path of the same package.
func TestExistsError(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), basicSpec(testPlaceholder()))
	prefix := filepath.Join(tmp, "env")
	if _, err := Link(ctx, pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"}); err != nil {
		t.Fatal(err)
	}
	// The same package again: its first file exists.
	_, err := Link(ctx, pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"})
	var ee *ExistsError
	if !errors.As(err, &ee) || !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want an *ExistsError", err)
	}
	planned, _ := PlannedPaths(ctx, pkgDir, rec, Options{Prefix: prefix, Platform: "linux-64"})
	found := false
	for _, p := range planned {
		found = found || p.Path == ee.Path
	}
	if !found || ee.SamePackage {
		t.Errorf("ExistsError %+v does not name a planned path of another package", ee)
	}

	// Two paths of one package landing on one file through the package's
	// own directory symlink.
	pkgDir2, rec2 := buildPkg(t, filepath.Join(tmp, "pkgs2"), pkgSpec{name: "s", files: []pkgFile{
		{path: "lib64/x", data: "1"},
		{path: "lib", symlink: "lib64"},
		{path: "lib/x", data: "2"},
	}})
	_, err = Link(ctx, pkgDir2, rec2, Options{Prefix: filepath.Join(tmp, "env2"), Platform: "linux-64"})
	if !errors.As(err, &ee) || ee.Path != "lib/x" || !ee.SamePackage {
		t.Errorf("same package: err = %v (%+v)", err, ee)
	}
}
