package lock

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzReadLockfile(f *testing.F) {
	for _, p := range []string{"testdata/example.lock.json", "testdata/base-3.11.lock.json"} {
		data, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	good := string(mustRead(f, "testdata/example.lock.json"))
	for _, s := range []string{
		"", "{}", "null", "[]", good + "x",
		strings.Replace(good, `"version": 1,`, `"version": 1, "version": 1,`, 1),
		strings.Replace(good, `"platform"`, `"Platform"`, 1),
		strings.Replace(good, `"md5": ""`, `"md5": null`, 1),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		l, err := Read(bytes.NewReader(data))
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Read error %v does not wrap ErrInvalid", err)
			}
			return
		}
		var out bytes.Buffer
		if err := l.Write(&out); err != nil {
			t.Fatalf("Write after a successful Read: %v", err)
		}
		back, err := Read(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatalf("Read(Write(Read(x))): %v\n%s", err, out.String())
		}
		var again bytes.Buffer
		if err := back.Write(&again); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), again.Bytes()) {
			t.Fatalf("Write is not stable:\n%s\nvs\n%s", out.String(), again.String())
		}
		var explicit bytes.Buffer
		if err := l.WriteExplicit(&explicit); err != nil {
			t.Fatalf("WriteExplicit: %v", err)
		}
		e, err := ReadExplicit(&explicit)
		if err != nil || len(e.Entries) != len(l.Packages) {
			t.Fatalf("ReadExplicit(WriteExplicit): %v", err)
		}
	})
}

func mustRead(f *testing.F, p string) []byte {
	data, err := os.ReadFile(filepath.FromSlash(p))
	if err != nil {
		f.Fatal(err)
	}
	return data
}

func FuzzReadBundle(f *testing.F) {
	f.Add(mustRead(f, "testdata/example.bundle.json"))
	f.Add([]byte(`{"version": 1, "platforms": {}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := ReadBundle(bytes.NewReader(data))
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ReadBundle error %v does not wrap ErrInvalid", err)
			}
			return
		}
		var out bytes.Buffer
		if err := b.Write(&out); err != nil {
			t.Fatalf("Write after a successful ReadBundle: %v", err)
		}
		if _, err := ReadBundle(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("ReadBundle(Write): %v", err)
		}
	})
}

func FuzzReadExplicit(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "explicit-envs", "*.txt"))
	files = append(files, filepath.Join("testdata", "base-3.11.micromamba-explicit.txt"))
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	for _, s := range []string{
		"", "@EXPLICIT", "@EXPLICIT\nimnotanurl", "# platform: notaplatform\n@EXPLICIT",
		"@EXPLICIT\nhttps://conda.anaconda.org/conda-forge/linux-64/pkg-6.4-bld.conda#sha256:7dbaa197d7ba6032caf7ae7f32c1efa07dbaa197d7ba6032caf7ae7f32c1efa0",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := ReadExplicit(bytes.NewReader(data))
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ReadExplicit error %v does not wrap ErrInvalid", err)
			}
			return
		}
		if e.Platform != "" && !knownPlatform(e.Platform) {
			t.Fatalf("platform %q", e.Platform)
		}
		for _, ent := range e.Entries {
			if strings.ContainsAny(ent.URL, "# \t\r\n") || archiveExt(ent.URL) == "" {
				t.Fatalf("entry URL %q", ent.URL)
			}
			if (ent.MD5 != "" && !isLowerHex(ent.MD5, 32)) || (ent.SHA256 != "" && !isLowerHex(ent.SHA256, 64)) {
				t.Fatalf("entry hashes %+v", ent)
			}
			if ent.MD5 != "" && ent.SHA256 != "" {
				t.Fatalf("entry with two hashes %+v", ent)
			}
		}
		// Resolving against an index that knows every URL either fails
		// cleanly or yields a valid lockfile.
		urls, md5s := entryURLs(e)
		l, err := e.Resolve(fakeIndex(urls, md5s))
		if err != nil {
			return
		}
		if err := l.Validate(); err != nil {
			t.Fatalf("Resolve returned an invalid lockfile: %v", err)
		}
	})
}

func FuzzParsePrefixRecord(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "prefix-base", "conda-meta", "*.json"))
	for _, p := range files[:5] {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"name":"a","version":"1","build":"b","noarch":{"type":"python"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		pr, err := parsePrefixRecord(data, "fuzz")
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error %v does not wrap ErrInvalid", err)
			}
			return
		}
		if pr.rec.Name == "" || pr.rec.Version == "" || pr.rec.Build == "" {
			t.Fatalf("record without name, version or build: %+v", pr.rec)
		}
	})
}
