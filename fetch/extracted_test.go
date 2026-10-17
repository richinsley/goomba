package fetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/richinsley/goomba/archive"
	"github.com/richinsley/goomba/record"
)

const wheelDist = "wheel-0.46.3-pyhd8ed1ab_0"

func wheelRecord(url string) *record.PackageRecord {
	return &record.PackageRecord{
		Name: "wheel", Version: "0.46.3", Build: "pyhd8ed1ab_0",
		SHA256: wheelSHA, Size: wheelSize, Fn: wheelFn, URL: url,
	}
}

// buildConda builds a .conda whose pkg tar holds files (name -> content)
// and whose info tar holds info/index.json.
func buildConda(t testing.TB, files map[string]string) []byte {
	t.Helper()
	tarOf := func(m map[string]string) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for name, data := range m {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			tw.Write([]byte(data))
		}
		tw.Close()
		enc, _ := zstd.NewWriter(nil)
		defer enc.Close()
		return enc.EncodeAll(b.Bytes(), nil)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, m := range []struct {
		name string
		data []byte
	}{
		{"metadata.json", []byte(`{"conda_pkg_format_version": 2}`)},
		{"pkg-x-1.0-0.tar.zst", tarOf(files)},
		{"info-x-1.0-0.tar.zst", tarOf(map[string]string{"info/index.json": "{}"})},
	} {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Store})
		w.Write(m.data)
	}
	zw.Close()
	return out.Bytes()
}

// tree lists the paths below root, not following symlinks.
func tree(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if p != root {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func inode(t testing.TB, p string) uint64 {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ino, _ := fileStamp(f)
	return ino
}

func TestExtractedHappyPath(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	ctx := context.Background()
	p, err := c.Extracted(ctx, wheelRecord(url))
	if err != nil {
		t.Fatal(err)
	}
	if p != treePath(c, wheelSHA) {
		t.Fatalf("path %s", p)
	}
	for _, f := range []string{"info/index.json", "info/paths.json", "site-packages/wheel/__init__.py"} {
		if _, err := os.Stat(filepath.Join(p, f)); err != nil {
			t.Fatal(err)
		}
	}
	key, err := c.markerKey()
	if err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(p, markerName))
	if err != nil || !bytes.Equal(marker, markerContent(key, wheelSHA)) {
		t.Fatalf("marker %q: %v", marker, err)
	}
	if !strings.HasPrefix(string(marker), "goomba-extracted 2\nsha256 "+wheelSHA+"\nmac ") {
		t.Fatalf("marker %q does not hold the archive sha256", marker)
	}
	ino := inode(t, filepath.Join(p, "info", "index.json"))

	// Later calls, also from a fresh Cache on the same directory, reuse the
	// tree without fetching or extracting.
	for _, cc := range []*Cache{c, {Dir: c.Dir, Client: s.Client(), AllowHTTP: true}} {
		if q, err := cc.Extracted(ctx, wheelRecord(url)); err != nil || q != p {
			t.Fatalf("%s, %v", q, err)
		}
	}
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("%d downloads, want 1", n)
	}
	if inode(t, filepath.Join(p, "info", "index.json")) != ino {
		t.Fatal("trusted tree was extracted again")
	}
	assertClean(t, c.Dir)

	// Without the archive, a trusted tree is still used.
	os.Remove(archivePath(c, wheelSHA, wheelFn))
	if _, err := c.Extracted(ctx, wheelRecord(url)); err != nil || s.hits.Load() != 1 {
		t.Fatalf("trusted tree needed its archive: %v", err)
	}
}

func TestExtractedReplacesUntrustedDirectories(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	ctx := context.Background()
	reference := func() []string {
		c := newCache(t, s)
		p, err := c.Extracted(ctx, wheelRecord(url))
		if err != nil {
			t.Fatal(err)
		}
		return tree(t, p)
	}()
	plant := map[string]func(t *testing.T, dir string, key []byte){
		// What micromamba leaves: a tree with repodata_record.json and no
		// marker, here with an extra file and a read-only directory.
		"micromamba tree": func(t *testing.T, dir string, _ []byte) {
			os.MkdirAll(filepath.Join(dir, "info"), 0o755)
			os.WriteFile(filepath.Join(dir, "info", "repodata_record.json"), []byte("{}"), 0o644)
			os.WriteFile(filepath.Join(dir, "evil.txt"), []byte("evil"), 0o644)
			os.MkdirAll(filepath.Join(dir, "ro", "deeper"), 0o755)
			os.WriteFile(filepath.Join(dir, "ro", "deeper", "f"), nil, 0o644)
			os.Chmod(filepath.Join(dir, "ro", "deeper"), 0o555)
			os.Chmod(filepath.Join(dir, "ro"), 0o555)
		},
		"marker with a bad mac": func(t *testing.T, dir string, key []byte) {
			os.MkdirAll(dir, 0o755)
			m := markerContent(key, wheelSHA)
			m[len(m)-2] ^= 1
			os.WriteFile(filepath.Join(dir, markerName), m, 0o644)
			os.WriteFile(filepath.Join(dir, "evil.txt"), []byte("evil"), 0o644)
		},
		"marker made with another key": func(t *testing.T, dir string, _ []byte) {
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, markerName), markerContent(make([]byte, 32), wheelSHA), 0o644)
			os.WriteFile(filepath.Join(dir, "evil.txt"), []byte("evil"), 0o644)
		},
		"marker for another archive": func(t *testing.T, dir string, key []byte) {
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, markerName), markerContent(key, strings.Repeat("0", 64)), 0o644)
			os.WriteFile(filepath.Join(dir, "evil.txt"), []byte("evil"), 0o644)
		},
		"marker that is a symlink": func(t *testing.T, dir string, key []byte) {
			os.MkdirAll(dir, 0o755)
			good := filepath.Join(t.TempDir(), "m")
			os.WriteFile(good, markerContent(key, wheelSHA), 0o644)
			os.Symlink(good, filepath.Join(dir, markerName))
			os.WriteFile(filepath.Join(dir, "evil.txt"), []byte("evil"), 0o644)
		},
		"symlink to a trusted tree elsewhere": func(t *testing.T, dir string, key []byte) {
			other := filepath.Join(t.TempDir(), "elsewhere")
			os.MkdirAll(other, 0o755)
			os.WriteFile(filepath.Join(other, markerName), markerContent(key, wheelSHA), 0o644)
			os.WriteFile(filepath.Join(other, "evil.txt"), []byte("evil"), 0o644)
			os.Symlink(other, dir)
		},
		"a regular file": func(t *testing.T, dir string, _ []byte) {
			os.WriteFile(dir, []byte("evil.txt"), 0o644)
		},
	}
	for name, fn := range plant {
		t.Run(name, func(t *testing.T) {
			c := newCache(t, s)
			key, err := c.markerKey()
			if err != nil {
				t.Fatal(err)
			}
			final := treePath(c, wheelSHA)
			os.MkdirAll(filepath.Dir(final), 0o755)
			fn(t, final, key)
			p, err := c.Extracted(ctx, wheelRecord(url))
			if err != nil {
				t.Fatal(err)
			}
			fi, err := os.Lstat(p)
			if err != nil || !fi.IsDir() {
				t.Fatalf("%s is not a directory: %v", p, err)
			}
			got := tree(t, p)
			if strings.Join(got, "\n") != strings.Join(reference, "\n") {
				t.Fatalf("tree differs from a clean extraction:\n%v\nwant\n%v", got, reference)
			}
			if !trusted(p, key, wheelSHA) {
				t.Fatal("result not trusted")
			}
			assertClean(t, c.Dir)
		})
	}
}

func TestExtractedRefusesPackageShippingMarker(t *testing.T) {
	s := newServer(t, false)
	data := buildConda(t, map[string]string{markerName: "goomba-extracted 1\n", "ok.txt": "ok"})
	url := s.put("/noarch/x-1.0-0.conda", data)
	c := newCache(t, s)
	rec := &record.PackageRecord{Name: "x", Version: "1.0", Build: "0", SHA256: sha256Hex(data), URL: url}
	_, err := c.Extracted(context.Background(), rec)
	if !errors.Is(err, archive.ErrFormat) {
		t.Fatalf("error %v, want archive.ErrFormat", err)
	}
	if _, err := os.Lstat(treePath(c, rec.SHA256)); err == nil {
		t.Fatal("package directory created")
	}
	assertClean(t, c.Dir)
}

func TestExtractedHostileArchive(t *testing.T) {
	s := newServer(t, false)
	data := buildConda(t, map[string]string{"../../escape.txt": "pwned"})
	url := s.put("/noarch/x-1.0-0.conda", data)
	c := newCache(t, s)
	rec := &record.PackageRecord{Name: "x", Version: "1.0", Build: "0", SHA256: sha256Hex(data), URL: url}
	if _, err := c.Extracted(context.Background(), rec); !errors.Is(err, archive.ErrUnsafePath) {
		t.Fatalf("error %v, want archive.ErrUnsafePath", err)
	}
	if _, err := os.Lstat(treePath(c, rec.SHA256)); err == nil {
		t.Fatal("package directory created")
	}
	assertClean(t, c.Dir)

	c.Limits = archive.Limits{MaxFileSize: 1}
	data = buildConda(t, map[string]string{"big.txt": "too big"})
	url = s.put("/noarch/y-1.0-0.conda", data)
	rec = &record.PackageRecord{Name: "y", Version: "1.0", Build: "0", SHA256: sha256Hex(data), URL: url}
	if _, err := c.Extracted(context.Background(), rec); !errors.Is(err, archive.ErrLimit) {
		t.Fatalf("error %v, want archive.ErrLimit", err)
	}
	assertClean(t, c.Dir)
}

func TestExtractedInvalidRecords(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	mod := func(f func(r *record.PackageRecord)) *record.PackageRecord {
		r := wheelRecord(url)
		f(r)
		return r
	}
	cases := map[string]*record.PackageRecord{
		"nil":               nil,
		"traversal name":    mod(func(r *record.PackageRecord) { r.Name = "../../x" }),
		"slash in version":  mod(func(r *record.PackageRecord) { r.Version = "1/2" }),
		"dot name":          mod(func(r *record.PackageRecord) { r.Name = ".." }),
		"empty build":       mod(func(r *record.PackageRecord) { r.Build = "" }),
		"fn mismatch":       mod(func(r *record.PackageRecord) { r.Fn = "other-1-0.conda" }),
		"no sha256":         mod(func(r *record.PackageRecord) { r.SHA256 = "" }),
		"md5 for sha256":    mod(func(r *record.PackageRecord) { r.SHA256 = "bdbd7385b4a67025ac2dba4ef8cb6a8f" }),
		"http url":          mod(func(r *record.PackageRecord) { r.URL = "http://example.invalid/" + wheelFn }),
		"dist not url name": mod(func(r *record.PackageRecord) { r.Build = "other_0"; r.Fn = "" }),
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCache(t, s)
			_, err := c.Extracted(context.Background(), rec)
			if !errors.Is(err, ErrInvalidInput) && !errors.Is(err, ErrInsecureURL) {
				t.Fatalf("error %v, want ErrInvalidInput", err)
			}
			if len(namesOrNil(c.Dir)) != 0 {
				t.Fatalf("invalid record touched the cache: %v", namesOrNil(c.Dir))
			}
		})
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("invalid records reached the server %d times", n)
	}
}

func TestExtractedIntegrityError(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	rec := wheelRecord(url)
	rec.SHA256 = strings.Repeat("1", 64)
	rec.Size = 0
	_, err := c.Extracted(context.Background(), rec)
	var ie *IntegrityError
	if !errors.As(err, &ie) {
		t.Fatalf("error %v, want *IntegrityError", err)
	}
	if _, err := os.Lstat(treePath(c, rec.SHA256)); err == nil {
		t.Fatal("package directory created")
	}
	assertClean(t, c.Dir)
}

func TestExtractedConcurrent(t *testing.T) {
	s := newServer(t, false)
	s.delay = 50 * time.Millisecond
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	dir := t.TempDir()
	caches := []*Cache{{Dir: dir, Client: s.Client(), AllowHTTP: true}, {Dir: dir, Client: s.Client(), AllowHTTP: true}, {Dir: dir, Client: s.Client(), AllowHTTP: true}}
	var wg sync.WaitGroup
	errs := make(chan error, 21)
	for i := range 21 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := caches[i%3].Extracted(context.Background(), wheelRecord(url))
			if err == nil && p != filepath.Join(dir, wheelSHA, treeName) {
				err = errors.New("wrong path " + p)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("%d downloads, want 1", n)
	}
	key, _ := caches[0].markerKey()
	for _, c := range caches[1:] {
		k, _ := c.markerKey()
		if !bytes.Equal(k, key) {
			t.Fatal("caches disagree on the marker key")
		}
	}
	assertClean(t, dir)
}

// Case from rattler tests/extract.rs
// data_descriptor_package_over_http_falls_back_to_buffering: a .conda
// whose zip members use data descriptors (here: the wheel package re-zipped
// by archive/zip, Deflate, nested in a directory) works over HTTP.
func TestExtractedDataDescriptorPackageOverHTTP(t *testing.T) {
	src, err := zip.NewReader(bytes.NewReader(wheelBytes(t)), wheelSize)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range src.File {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: wheelDist + "/" + f.Name, Method: zip.Deflate})
		r, _ := f.Open()
		io.Copy(w, r)
		r.Close()
	}
	zw.Close()
	data := out.Bytes()
	if zr, _ := zip.NewReader(bytes.NewReader(data), int64(len(data))); zr.File[0].Flags&0x8 == 0 {
		t.Fatal("re-zipped archive has no data descriptors")
	}
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, data)
	c := newCache(t, s)
	rec := wheelRecord(url)
	rec.SHA256, rec.Size = sha256Hex(data), int64(len(data))
	p, err := c.Extracted(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"info/index.json", "site-packages/wheel/__init__.py"} {
		if fi, err := os.Lstat(filepath.Join(p, f)); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestExtractedRemovesStaleLeftovers(t *testing.T) {
	s := newServer(t, false)
	url := s.put("/noarch/"+wheelFn, wheelBytes(t))
	c := newCache(t, s)
	content := filepath.Join(c.Dir, wheelSHA)
	stale := []string{
		".pkg.staging-0123456789abcdef",
		".pkg.old-fedcba9876543210",
		"..pkg.staging-0123456789abcdef.tmp-0011223344556677",
		wheelFn + ".partial-0123456789abcdef",
	}
	keep := []string{
		".pkg.staging-keep",
		".pkgx.staging-0123456789abcdef",
		".pkg.1.staging-0123456789abcdef",
	}
	for _, n := range slices.Concat(stale, keep) {
		os.MkdirAll(filepath.Join(content, n, "sub"), 0o755)
	}
	os.Chmod(filepath.Join(content, stale[0], "sub"), 0o555)
	if _, err := c.Extracted(context.Background(), wheelRecord(url)); err != nil {
		t.Fatal(err)
	}
	for _, n := range stale {
		if _, err := os.Lstat(filepath.Join(content, n)); err == nil {
			t.Errorf("%s not removed", n)
		}
	}
	for _, n := range keep {
		if _, err := os.Lstat(filepath.Join(content, n)); err != nil {
			t.Errorf("%s removed", n)
		}
	}
}

func TestMarkerKey(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir}
	k1, err := c.markerKey()
	if err != nil || len(k1) != 32 {
		t.Fatalf("key %x: %v", k1, err)
	}
	fi, err := os.Stat(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatal(err)
	}
	if fileIDsSupported() && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode())
	}
	if k2, err := (&Cache{Dir: dir}).markerKey(); err != nil || !bytes.Equal(k1, k2) {
		t.Fatal("key not persisted")
	}
	for _, bad := range []string{"", "zz\n", strings.Repeat("a", 64), strings.Repeat("A", 64) + "\n"} {
		os.WriteFile(filepath.Join(dir, keyName), []byte(bad), 0o600)
		if _, err := (&Cache{Dir: dir}).markerKey(); err == nil {
			t.Fatalf("corrupt key %q accepted", bad)
		}
	}
	// Concurrent creation in separate caches agrees on one key.
	dir = t.TempDir()
	keys := make([][]byte, 16)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], _ = (&Cache{Dir: dir}).markerKey()
		}()
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !bytes.Equal(k, keys[0]) || len(k) != 32 {
			t.Fatal("concurrent key creation disagreed")
		}
	}
	assertClean(t, dir)
}
