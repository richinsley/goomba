package link

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSymlinkTargetShape checks which symlink targets are accepted and the
// normalised form the checks see: "." and empty components are dropped
// (WP8c), ".." after a name, absolute and drive targets, NUL, backslash
// and names folding to conda-meta stay refused.
func TestSymlinkTargetShape(t *testing.T) {
	for _, c := range []struct {
		target string
		ups    int
		names  []string
	}{
		{".", 0, nil},
		{"./", 0, nil},
		{".//./", 0, nil},
		{"x", 0, []string{"x"}},
		{"./x", 0, []string{"x"}},
		{"x/", 0, []string{"x"}},
		{"x/.", 0, []string{"x"}},
		{"x//y", 0, []string{"x", "y"}},
		{"x/./y/", 0, []string{"x", "y"}},
		{"..", 1, nil},
		{"./..", 1, nil},
		{"../", 1, nil},
		{"..//x", 1, []string{"x"}},
		{".././61/adm1178", 1, []string{"61", "adm1178"}}, // conda-forge ncurses-6.6-hdb14827_1
		{".././../x", 2, []string{"x"}},
		{"./.././../x/./y//", 2, []string{"x", "y"}},
		{"../../", 2, nil},
		{"...", 0, []string{"..."}},
		{"x/.../y", 0, []string{"x", "...", "y"}},
		{"../.x/..y", 1, []string{".x", "..y"}},
		{"../C:/x", 1, []string{"C:", "x"}}, // a name on Unix; checkResolved refuses ':' for Windows
	} {
		ups, names, err := symlinkTargetShape(c.target)
		if err != nil || ups != c.ups || !reflect.DeepEqual(names, c.names) {
			t.Errorf("symlinkTargetShape(%q) = %d, %q, %v; want %d, %q", c.target, ups, names, err, c.ups, c.names)
		}
	}
	for _, c := range []struct{ target, why string }{
		{"", "is empty"},
		{"/", "is absolute"},
		{"/x", "is absolute"},
		{"//x", "is absolute"},
		{"C:/x", "is absolute"},
		{"c:x", "is absolute"},
		{"./C:/x", "drive path"},
		{".//C:x", "drive path"},
		{`x\y`, "backslash"},
		{`..\x`, "backslash"},
		{"x\x00y", "NUL"},
		{"x/../y", `".." after a name`},
		{"x/./../y", `".." after a name`},
		{"x//../y", `".." after a name`},
		{"./x/..", `".." after a name`},
		{"../x/../..", `".." after a name`},
		{"conda-meta", "names conda-meta"},
		{"./conda-meta", "names conda-meta"},
		{".././/CONDA-META/x", "names conda-meta"},
		{"x/./conda-meta", "names conda-meta"},
		{"x/conda\u200b-meta/", "names conda-meta"},
	} {
		_, _, err := symlinkTargetShape(c.target)
		if !errors.Is(err, ErrUnsafePath) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("symlinkTargetShape(%q) = %v, want ErrUnsafePath (%s)", c.target, err, c.why)
		}
	}
}

// TestSymlinkTargetDepth checks the climb limit on the normalised target at
// and around each symlink's depth.
func TestSymlinkTargetDepth(t *testing.T) {
	for _, c := range []struct {
		dst, target string
		ok          bool
	}{
		{"top", ".", true},
		{"top", "./", true},
		{"top", "x/.", true},
		{"top", "./..", false},
		{"top", "../", false},
		{"share/l", "../x", true},
		{"share/l", ".././x", true},
		{"share/l", "./../x", true},
		{"share/l", ".././.", true},
		{"share/l", ".././../x", false}, // two levels from depth 1
		{"share/l", "..//..", false},
		{"share/terminfo/l", ".././../x", true}, // two levels from depth 2
		{"share/terminfo/l", "./.././..//", true},
		{"share/terminfo/l", ".././.././../x", false},
		{"share/terminfo/31/1178", ".././61/adm1178", true},
		{"share/terminfo/31/1178", "../.././../../x", false},
		{"a/b", ".././conda-meta/x", false},
		{"conda-meta/x", "./y", false}, // a symlink inside conda-meta
	} {
		err := checkSymlinkTarget(c.dst, c.target)
		if (err == nil) != c.ok || err != nil && !errors.Is(err, ErrUnsafePath) {
			t.Errorf("checkSymlinkTarget(%q, %q) = %v, want ok=%v", c.dst, c.target, err, c.ok)
		}
	}
}

// ncursesSpec has the shape of conda-forge's ncurses-6.6-hdb14827_1, which
// ships 1,038 terminfo symlinks like share/terminfo/31/1178 ->
// .././61/adm1178.
func ncursesSpec() pkgSpec {
	return pkgSpec{name: "ncurses", version: "6.6", build: "hdb14827_1", files: []pkgFile{
		{path: "share/terminfo/61/adm1178", data: "adm1178 terminfo\n"},
		{path: "share/terminfo/31/1178", symlink: ".././61/adm1178"},
		{path: "share/terminfo/61/aaa-60", data: "aaa terminfo\n"},
		{path: "share/terminfo/61/aaa", symlink: "aaa-60"},
		{path: "share/terminfo/45/Eterm", symlink: ".././61/aaa"},
		{path: "share/terminfo/E", symlink: "./45/"},
		{path: "lib/terminfo", symlink: "../share/terminfo"},
	}}
}

// The ncurses case: Link and PlannedPaths accept the package, the targets
// are written exactly as shipped, they resolve to the files they name, and
// the symlinks' digests are those of their targets.
func TestLinkNcursesTerminfo(t *testing.T) {
	tmp := t.TempDir()
	spec := ncursesSpec()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), spec)
	prefix := filepath.Join(tmp, "env")
	opt := Options{Prefix: prefix, Platform: "linux-64"}
	planned, err := PlannedPaths(context.Background(), pkgDir, rec, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != len(spec.files) {
		t.Errorf("planned %d paths, want %d", len(planned), len(spec.files))
	}
	inst, err := Link(context.Background(), pkgDir, rec, opt)
	if err != nil {
		t.Fatal(err)
	}
	digest := map[string]string{}
	for _, p := range inst.Paths {
		digest[p.Path] = p.SHA256InPrefix
	}
	for _, f := range spec.files {
		if f.symlink == "" {
			continue
		}
		if got, err := os.Readlink(filepath.Join(prefix, f.path)); err != nil || got != f.symlink {
			t.Errorf("%s -> %q, %v; want %q", f.path, got, err, f.symlink)
		}
	}
	for link, file := range map[string]string{
		"share/terminfo/31/1178":       "share/terminfo/61/adm1178",
		"share/terminfo/45/Eterm":      "share/terminfo/61/aaa-60",
		"lib/terminfo/31/1178":         "share/terminfo/61/adm1178",
		"share/terminfo/E/Eterm":       "share/terminfo/61/aaa-60",
		"lib/terminfo/E/./Eterm":       "share/terminfo/61/aaa-60",
		"share/terminfo/31/../E/Eterm": "share/terminfo/61/aaa-60",
	} {
		a, err := os.Stat(filepath.Join(prefix, link))
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		b, err := os.Stat(filepath.Join(prefix, file))
		if err != nil || !os.SameFile(a, b) {
			t.Errorf("%s does not lead to %s (%v)", link, file, err)
		}
	}
	if want := sha256Hex([]byte("adm1178 terminfo\n")); digest["share/terminfo/31/1178"] != want {
		t.Errorf("digest of share/terminfo/31/1178 = %q, want %q", digest["share/terminfo/31/1178"], want)
	}
	if want := sha256Hex([]byte("aaa terminfo\n")); digest["share/terminfo/45/Eterm"] != want {
		t.Errorf("digest of share/terminfo/45/Eterm = %q, want %q", digest["share/terminfo/45/Eterm"], want)
	}

	// Unlink removes it all again.
	if err := WriteCondaMeta(prefix, rec, inst, opt); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(context.Background(), prefix, rec); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, prefix); len(got) != 0 {
		t.Errorf("left after Unlink: %q", got)
	}
}

// TestHashSymlinksTrailingSlash (review of WP8c, N1): link records the
// empty digest for a symlink whose target ends in "/" or "/." after a file
// (ENOTDIR), as libmamba does, and the file's digest for "./f".
func TestHashSymlinksTrailingSlash(t *testing.T) {
	tmp := t.TempDir()
	pkgDir, rec := buildPkg(t, filepath.Join(tmp, "pkgs"), pkgSpec{name: "p", files: []pkgFile{
		{path: "share/f", data: "data\n"},
		{path: "share/l1", symlink: "f/"},
		{path: "share/l2", symlink: "f/."},
		{path: "share/l3", symlink: "./f"},
		{path: "share/l4", symlink: "l3/"},
	}})
	inst, err := Link(context.Background(), pkgDir, rec, Options{Prefix: filepath.Join(tmp, "env"), Platform: "linux-64"})
	if err != nil {
		t.Fatal(err)
	}
	file := sha256Hex([]byte("data\n"))
	want := map[string]string{"share/f": file, "share/l1": emptySHA256, "share/l2": emptySHA256, "share/l3": file, "share/l4": emptySHA256}
	for _, p := range inst.Paths {
		if p.SHA256InPrefix != want[p.Path] {
			t.Errorf("%s: %s, want %s", p.Path, p.SHA256InPrefix, want[p.Path])
		}
	}
}
