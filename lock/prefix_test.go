package lock

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const prefixBase = "testdata/prefix-base"

func TestFromPrefixFixture(t *testing.T) {
	l, err := FromPrefix(filepath.FromSlash(prefixBase), "linux-64")
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "testdata/base-3.11.lock.json", writeString(t, l))
	if len(l.Packages) != 28 || l.Platform != "linux-64" {
		t.Fatalf("got %d packages for %q", len(l.Packages), l.Platform)
	}
	if strings.Join(l.Channels, ",") != cf || strings.Join(l.Specs, ",") != "python=3.11" || len(l.Virtual) != 0 {
		t.Errorf("channels %q, specs %q, virtual %v", l.Channels, l.Specs, l.Virtual)
	}
	for _, p := range l.Packages {
		if !strings.HasPrefix(p.URL, "https://") || len(p.SHA256) != 64 || len(p.MD5) != 32 || p.Size <= 0 {
			t.Errorf("package %+v", p)
		}
	}
	byName := map[string]Package{}
	for _, p := range l.Packages {
		byName[p.Name] = p
	}
	if p := byName["python"]; p.SHA256 != pythonSHA || p.MD5 != pythonMD5 || p.Size != 30949404 || p.URL != pythonURL ||
		len(p.Constrains) != 1 || p.Constrains[0] != "python_abi 3.11.* *_cp311" || p.Depends[len(p.Depends)-1] != "pip" {
		t.Errorf("python = %+v", p)
	}
	if p := byName["pip"]; p.Noarch != "python" || p.Subdir != "noarch" {
		t.Errorf("pip = %+v", p)
	}
	if p := byName["tzdata"]; p.Noarch != "generic" {
		t.Errorf("tzdata = %+v", p)
	}
	if p := byName["_openmp_mutex"]; p.BuildNumber != 20 {
		t.Errorf("_openmp_mutex = %+v", p)
	}
	// The platform can be inferred.
	inferred, err := FromPrefix(filepath.FromSlash(prefixBase), "")
	if err != nil {
		t.Fatal(err)
	}
	if writeString(t, inferred) != writeString(t, l) {
		t.Error("inferred platform gives a different lockfile")
	}
	// A different platform is rejected by validation.
	if _, err := FromPrefix(filepath.FromSlash(prefixBase), "osx-64"); !errors.Is(err, ErrInvalid) {
		t.Errorf("osx-64: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FromPrefixContext(ctx, filepath.FromSlash(prefixBase), "linux-64"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// TestFromPrefixReference reads the micromamba-built environment the
// fixture was taken from, when GOOMBA_REF_ROOT points at it.
func TestFromPrefixReference(t *testing.T) {
	root := os.Getenv("GOOMBA_REF_ROOT")
	if root == "" {
		t.Skip("set GOOMBA_REF_ROOT to the reference micromamba root")
	}
	l, err := FromPrefix(filepath.Join(root, "envs", "base-3.11"), "linux-64")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Packages) != 28 {
		t.Errorf("%d packages, want 28", len(l.Packages))
	}
	for _, p := range l.Packages {
		if !strings.HasPrefix(p.URL, "https://") || len(p.SHA256) != 64 {
			t.Errorf("package %s: url %q sha256 %q", p.Name, p.URL, p.SHA256)
		}
	}
	fixture, err := FromPrefix(filepath.FromSlash(prefixBase), "linux-64")
	if err != nil {
		t.Fatal(err)
	}
	if writeString(t, l) != writeString(t, fixture) {
		t.Error("the reference environment and the trimmed fixture give different lockfiles")
	}
}

// copyPrefix copies the fixture prefix into a temporary directory.
func copyPrefix(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	meta := filepath.Join(dst, "conda-meta")
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(filepath.FromSlash(prefixBase), "conda-meta")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(meta, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func editRecord(t *testing.T, prefix, file string, edit func(string) string) {
	t.Helper()
	p := filepath.Join(prefix, "conda-meta", file)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := edit(string(data))
	if out == string(data) {
		t.Fatalf("edit of %s changed nothing", file)
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFromPrefixErrors(t *testing.T) {
	const py = "python-3.11.15-hd63d673_0_cpython.json"
	cases := []struct {
		name  string
		setup func(t *testing.T, prefix string)
		want  error
	}{
		{"no conda-meta", func(t *testing.T, prefix string) {
			if err := os.RemoveAll(filepath.Join(prefix, "conda-meta")); err != nil {
				t.Fatal(err)
			}
		}, fs.ErrNotExist},
		{"file name mismatch", func(t *testing.T, prefix string) {
			if err := os.Rename(filepath.Join(prefix, "conda-meta", py), filepath.Join(prefix, "conda-meta", "python-3.11.json")); err != nil {
				t.Fatal(err)
			}
		}, ErrInvalid},
		{"http url", func(t *testing.T, prefix string) {
			editRecord(t, prefix, py, func(s string) string { return strings.Replace(s, `"url": "https:`, `"url": "http:`, 1) })
		}, ErrInvalid},
		{"no sha256", func(t *testing.T, prefix string) {
			editRecord(t, prefix, py, func(s string) string { return strings.Replace(s, `"sha256": "`+pythonSHA+`"`, `"sha256": null`, 1) })
		}, ErrInvalid},
		{"no name", func(t *testing.T, prefix string) {
			editRecord(t, prefix, py, func(s string) string { return strings.Replace(s, `"name": "python"`, `"name": ""`, 1) })
		}, ErrInvalid},
		{"duplicate key", func(t *testing.T, prefix string) {
			editRecord(t, prefix, py, func(s string) string {
				return strings.Replace(s, `"name": "python",`, `"name": "python", "name": "python",`, 1)
			})
		}, ErrInvalid},
		{"wrong type", func(t *testing.T, prefix string) {
			editRecord(t, prefix, py, func(s string) string { return strings.Replace(s, `"size": 30949404`, `"size": "big"`, 1) })
		}, ErrInvalid},
		{"truncated", func(t *testing.T, prefix string) {
			editRecord(t, prefix, py, func(s string) string { return s[:len(s)/2] })
		}, ErrInvalid},
		{"symlink", func(t *testing.T, prefix string) {
			p := filepath.Join(prefix, "conda-meta", py)
			tmp := filepath.Join(prefix, "elsewhere.json")
			if err := os.Rename(p, tmp); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tmp, p); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
		}, ErrInvalid},
		{"mixed platforms", func(t *testing.T, prefix string) {
			editRecord(t, prefix, py, func(s string) string {
				s = strings.Replace(s, `"subdir": "linux-64"`, `"subdir": "osx-64"`, 1)
				return strings.Replace(s, "/linux-64/python", "/osx-64/python", 1)
			})
		}, ErrInvalid},
	}
	for _, c := range cases {
		prefix := copyPrefix(t)
		c.setup(t, prefix)
		platform := "linux-64"
		if c.name == "mixed platforms" {
			platform = ""
		}
		if _, err := FromPrefix(prefix, platform); !errors.Is(err, c.want) {
			t.Errorf("%s: FromPrefix = %v, want %v", c.name, err, c.want)
		}
	}
	// Host spellings are normalized as in FromRecords.
	for _, spelling := range []string{"https://Conda.Anaconda.org", "https://conda.anaconda.org.:443", "HTTPS://conda.anaconda.org:443"} {
		prefix := copyPrefix(t)
		const py = "python-3.11.15-hd63d673_0_cpython.json"
		editRecord(t, prefix, py, func(s string) string {
			return strings.Replace(s, `"url": "https://conda.anaconda.org`, `"url": "`+spelling, 1)
		})
		l, err := FromPrefix(prefix, "linux-64")
		if err != nil {
			t.Errorf("%s: %v", spelling, err)
			continue
		}
		if fixture, _ := FromPrefix(filepath.FromSlash(prefixBase), "linux-64"); writeString(t, l) != writeString(t, fixture) {
			t.Errorf("%s: host not normalized", spelling)
		}
	}
	// Non-JSON files and directories in conda-meta are ignored.
	prefix := copyPrefix(t)
	if err := os.WriteFile(filepath.Join(prefix, "conda-meta", "history"), []byte("==> x <==\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(prefix, "conda-meta", "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if l, err := FromPrefix(prefix, "linux-64"); err != nil || len(l.Packages) != 28 {
		t.Errorf("with history: %v", err)
	}
	// An empty conda-meta cannot infer a platform but locks fine with one.
	empty := t.TempDir()
	if err := os.Mkdir(filepath.Join(empty, "conda-meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := FromPrefix(empty, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty, inferred: %v", err)
	}
	if l, err := FromPrefix(empty, "linux-64"); err != nil || len(l.Packages) != 0 {
		t.Errorf("empty: %v", err)
	}
}

func TestParsePrefixRecord(t *testing.T) {
	cases := []struct {
		name, in string
		noarch   string
		build    string
		wantErr  bool
	}{
		{"noarch string", `{"name":"a","version":"1","build":"b","noarch":"python"}`, "python", "b", false},
		{"noarch true", `{"name":"a","version":"1","build":"b","noarch":true}`, "generic", "b", false},
		{"noarch false", `{"name":"a","version":"1","build":"b","noarch":false}`, "", "b", false},
		{"noarch null", `{"name":"a","version":"1","build":"b","noarch":null}`, "", "b", false},
		{"noarch object", `{"name":"a","version":"1","build":"b","noarch":{"type":"python","x":[1]}}`, "python", "b", false},
		{"build_string fallback", `{"name":"a","version":"1","build_string":"bs"}`, "", "bs", false},
		{"build and build_string agree", `{"name":"a","version":"1","build":"b","build_string":"b"}`, "", "b", false},
		{"build and build_string differ", `{"name":"a","version":"1","build":"b","build_string":"bs"}`, "", "", true},
		{"unknown keys skipped", `{"name":"a","version":"1","build":"b","files":["x"],"paths_data":{"paths":[{"_path":"x"}]},"link":null}`, "", "b", false},
		{"nulls", `{"name":"a","version":"1","build":"b","depends":null,"md5":null,"size":null,"build_number":null}`, "", "b", false},
		{"noarch number", `{"name":"a","version":"1","build":"b","noarch":1}`, "", "", true},
		{"noarch array", `{"name":"a","version":"1","build":"b","noarch":[]}`, "", "", true},
		{"missing version", `{"name":"a","build":"b"}`, "", "", true},
		{"negative build number", `{"name":"a","version":"1","build":"b","build_number":-1}`, "", "", true},
		{"wrong-case key ignored, so name missing", `{"Name":"a","version":"1","build":"b"}`, "", "", true},
		{"depends element not string", `{"name":"a","version":"1","build":"b","depends":[1]}`, "", "", true},
		{"trailing data", `{"name":"a","version":"1","build":"b"} {}`, "", "", true},
		{"array", `[]`, "", "", true},
	}
	for _, c := range cases {
		pr, err := parsePrefixRecord([]byte(c.in), "test")
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: err %v does not wrap ErrInvalid", c.name, err)
			}
			continue
		}
		if pr.rec.Noarch != c.noarch || pr.rec.Build != c.build {
			t.Errorf("%s: noarch %q build %q", c.name, pr.rec.Noarch, pr.rec.Build)
		}
	}
}

// TestPrefixRecordSymlinkRace is a regression test: while one goroutine
// keeps replacing a record with a symbolic link to another file and back,
// reading the record must never return the link target's content.
func TestPrefixRecordSymlinkRace(t *testing.T) {
	dir := t.TempDir()
	const good = `{"name":"a","version":"1","build":"b","md5":"00000000000000000000000000000000"}`
	const evil = `{"name":"a","version":"1","build":"b","md5":"ffffffffffffffffffffffffffffffff"}`
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "a-1-b.json")
	if err := os.WriteFile(record, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "probe")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	stop := make(chan struct{})
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		link, reg := filepath.Join(dir, "link.tmp"), filepath.Join(dir, "reg.tmp")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				os.Remove(link)
				if os.Symlink(target, link) == nil {
					os.Rename(link, record)
				}
			} else if os.WriteFile(reg, []byte(good), 0o644) == nil {
				os.Rename(reg, record)
			}
		}
	}()
	accepted, leaked := 0, 0
	for i := 0; i < 5000; i++ {
		pr, err := readPrefixRecordFile(record)
		if err != nil {
			continue
		}
		accepted++
		if pr.rec.MD5 != "00000000000000000000000000000000" {
			leaked++
		}
	}
	close(stop)
	<-swapped
	if leaked > 0 {
		t.Errorf("%d of %d accepted reads returned the symlink target", leaked, accepted)
	}
	t.Logf("%d of 5000 reads accepted", accepted)
}
