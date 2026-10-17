package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// noise returns deterministic pseudo-random bytes (xorshift) that do not
// compress, like rattler's test helper of the same name.
func noise(n int, seed uint64) []byte {
	state := seed | 1
	out := make([]byte, 0, n+8)
	for len(out) < n {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		for i := range 8 {
			out = append(out, byte(state>>(8*i)))
		}
	}
	return out[:n]
}

const rawMtime = 1_700_000_000

// defaultLimits are the limits extraction applies when given Limits{}.
var defaultLimits = Limits{}.withDefaults()

// rawTar builds tar archives from hand-made 512-byte headers so entries can
// carry names, link targets, types and sizes archive/tar would refuse to
// write. It is a port of rattler's RawTar test helper.
type rawTar struct{ buf bytes.Buffer }

func octal(field []byte, v int64) {
	s := strconv.FormatInt(v, 8)
	w := len(field) - 1
	for len(s) < w {
		s = "0" + s
	}
	copy(field, s)
	field[w] = 0
}

// header writes one header. magic selects "ustar\x0000" (true) or a
// pre-POSIX header without magic (false).
func (t *rawTar) header(typ byte, name, link string, mode, size, mtime int64, magic bool) {
	var h [512]byte
	if len(name) > 100 || len(link) > 100 {
		panic("rawTar: name too long: " + name)
	}
	copy(h[0:100], name)
	octal(h[100:108], mode)
	octal(h[108:116], 0)
	octal(h[116:124], 0)
	octal(h[124:136], size)
	octal(h[136:148], mtime)
	h[156] = typ
	copy(h[157:257], link)
	if magic {
		copy(h[257:263], "ustar\x00")
		copy(h[263:265], "00")
	}
	copy(h[148:156], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:155], fmt.Sprintf("%06o\x00", sum))
	h[155] = ' '
	t.buf.Write(h[:])
}

func (t *rawTar) data(b []byte) {
	t.buf.Write(b)
	if pad := (512 - len(b)%512) % 512; pad > 0 {
		t.buf.Write(make([]byte, pad))
	}
}

func (t *rawTar) fileMode(name string, data []byte, mode int64) *rawTar {
	t.header(tar.TypeReg, name, "", mode, int64(len(data)), rawMtime, true)
	t.data(data)
	return t
}

func (t *rawTar) file(name string, data []byte) *rawTar { return t.fileMode(name, data, 0o644) }

// fileWithSize writes data but claims size in the header.
func (t *rawTar) fileWithSize(name string, data []byte, size int64) *rawTar {
	t.header(tar.TypeReg, name, "", 0o644, size, rawMtime, true)
	t.data(data)
	return t
}

func (t *rawTar) fileMtime(name string, data []byte, mtime int64) *rawTar {
	t.header(tar.TypeReg, name, "", 0o644, int64(len(data)), mtime, true)
	t.data(data)
	return t
}

func (t *rawTar) dir(name string) *rawTar {
	t.header(tar.TypeDir, name, "", 0o755, 0, rawMtime, true)
	return t
}

// oldDir writes a pre-POSIX directory: a regular entry with a trailing
// slash and no ustar magic.
func (t *rawTar) oldDir(name string) *rawTar {
	t.header(tar.TypeReg, name, "", 0o755, 0, rawMtime, false)
	return t
}

func (t *rawTar) symlink(name, target string) *rawTar {
	t.header(tar.TypeSymlink, name, target, 0o777, 0, rawMtime, true)
	return t
}

func (t *rawTar) hardlink(name, target string) *rawTar {
	t.header(tar.TypeLink, name, target, 0o644, 0, rawMtime, true)
	return t
}

func (t *rawTar) special(typ byte, name string) *rawTar {
	t.header(typ, name, "", 0o644, 0, rawMtime, true)
	return t
}

// pax appends an entry through archive/tar, which emits PAX records for
// long names and arbitrary PAXRecords.
func (t *rawTar) pax(hdr *tar.Header, data []byte) *rawTar {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if hdr.ModTime.IsZero() {
		hdr.ModTime = time.Unix(rawMtime, 0)
	}
	if err := tw.WriteHeader(hdr); err != nil {
		panic(err)
	}
	if _, err := tw.Write(data); err != nil {
		panic(err)
	}
	if err := tw.Flush(); err != nil {
		panic(err)
	}
	t.buf.Write(b.Bytes())
	return t
}

func (t *rawTar) longFile(name string, data []byte) *rawTar {
	return t.pax(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(data))}, data)
}

// finish appends the end-of-archive marker and returns the tar bytes.
func (t *rawTar) finish() []byte {
	out := slices.Clone(t.buf.Bytes())
	return append(out, make([]byte, 1024)...)
}

func (t *rawTar) finishBz2() []byte { return bz2Compress(t.finish()) }
func (t *rawTar) finishZst() []byte { return zstdCompress(t.finish()) }

func zstdCompress(b []byte) []byte {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		panic(err)
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

type zipMember struct {
	name string
	data []byte
}

func buildZip(members ...zipMember) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, m := range members {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Store})
		if err != nil {
			panic(err)
		}
		if _, err := w.Write(m.data); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// buildConda builds a minimal .conda around pkgTar (uncompressed) with an
// info tar holding info/index.json, like rattler's build_conda helper.
func buildConda(name string, pkgTar []byte) []byte {
	index := fmt.Sprintf(`{"name":%q,"version":"1.0","build":"0","build_number":0,"subdir":"noarch","depends":[]}`, name)
	info := (&rawTar{}).file("info/index.json", []byte(index)).finish()
	return buildCondaTars(name, info, pkgTar)
}

func buildCondaTars(name string, infoTar, pkgTar []byte) []byte {
	return buildZip(
		zipMember{"metadata.json", []byte(`{"conda_pkg_format_version": 2}`)},
		zipMember{"pkg-" + name + "-1.0-0.tar.zst", zstdCompress(pkgTar)},
		zipMember{"info-" + name + "-1.0-0.tar.zst", zstdCompress(infoTar)},
	)
}

// fixture writes data to a fresh file named name and returns its path.
func fixture(t testing.TB, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// sandbox returns a fresh parent directory and a destination inside it
// that does not exist yet.
func sandbox(t testing.TB) (parent, dest string) {
	t.Helper()
	parent = t.TempDir()
	return parent, filepath.Join(parent, "dest")
}

// extractBytes writes data to a temp file named name and extracts it.
func extractBytes(t testing.TB, name string, data []byte, dest string, lim Limits) error {
	t.Helper()
	return Extract(context.Background(), fixture(t, name, data), dest, lim)
}

func extractTarBz2(t testing.TB, data []byte, dest string) error {
	t.Helper()
	return ExtractReader(context.Background(), bytes.NewReader(data), TarBz2, dest, Limits{})
}

// node describes one path in a snapshot.
type node struct {
	Type   string `json:"type"` // "file", "dir" or "symlink"
	Mode   string `json:"mode,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
	Mtime  int64  `json:"mtime_ns,omitempty"`
}

// snapshot returns every path below root keyed by its slash-separated
// relative path. Symlinks are not followed; directory modes and times are
// omitted because they depend on the umask and on extraction order.
func snapshot(t testing.TB, root string) map[string]node {
	t.Helper()
	out := map[string]node{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = node{Type: "symlink", Target: target}
		case fi.IsDir():
			out[rel] = node{Type: "dir"}
		case fi.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[rel] = node{
				Type:   "file",
				Mode:   fmt.Sprintf("%04o", fi.Mode().Perm()),
				SHA256: hex.EncodeToString(sum[:]),
				Mtime:  fi.ModTime().UnixNano(),
			}
		default:
			return fmt.Errorf("unexpected file type %v at %s", fi.Mode().Type(), rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// entries lists the names in dir, or nil if it does not exist.
func entries(t testing.TB, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range des {
		names = append(names, d.Name())
	}
	return names
}

// assertNothingLeft fails unless parent holds exactly the names in keep.
func assertNothingLeft(t testing.TB, parent string, keep ...string) {
	t.Helper()
	got := entries(t, parent)
	slices.Sort(got)
	slices.Sort(keep)
	if !slices.Equal(got, keep) {
		t.Fatalf("%s holds %q, want %q", parent, got, keep)
	}
}

func assertIs(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error %v, want errors.Is(%v)", err, target)
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
