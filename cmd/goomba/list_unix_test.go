//go:build unix

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestListEscapesAndStatus: list escapes bidirectional-formatting
// characters and tabs read from conda-meta, as goomba escapes all text from
// prefixes, and tells an installation that is running (the prefix lock is
// held) from one that did not finish.
func TestListEscapesAndStatus(t *testing.T) {
	lockFile, srv, _ := testLockfile(t, "ca-certificates-2026.2.25-hbd8a1cb_0")
	c := &cli{
		client:   func() (*http.Client, error) { return srv.Client(), nil },
		cacheDir: func() (string, error) { return t.TempDir(), nil },
		virtual:  fakeDetect,
	}
	prefix := filepath.Join(t.TempDir(), "env")
	if code, _, stderr := runCLI(t, c, "create", "--lock", lockFile, "-p", prefix, "--no-pyc"); code != exitOK {
		t.Fatalf("create: exit %d: %s", code, stderr)
	}
	recs, err := filepath.Glob(filepath.Join(prefix, "conda-meta", "*.json"))
	if err != nil || len(recs) != 1 {
		t.Fatalf("conda-meta records: %v, %v", recs, err)
	}
	data, err := os.ReadFile(recs[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec["channel"], rec["url"] = "https://example.com/conda-\u202eegrof\tx", ""
	if data, err = json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recs[0], data, 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, c, "list", "-p", prefix)
	if code != exitOK || strings.ContainsRune(stdout, '\u202e') || !strings.Contains(stdout, `conda-\u202eegrof\tx`) {
		t.Errorf("list: exit %d, stdout %q, stderr %q; want the channel escaped", code, stdout, stderr)
	}

	if err := os.WriteFile(filepath.Join(prefix, "conda-meta", ".goomba-incomplete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := unix.Flock(int(d.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, _, stderr := runCLI(t, c, "list", "-p", prefix); !strings.Contains(stderr, "installing or removing it right now") {
		t.Errorf("list while the prefix lock is held: %s", stderr)
	}
	if err := unix.Flock(int(d.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, _, stderr := runCLI(t, c, "list", "-p", prefix); !strings.Contains(stderr, "did not finish") {
		t.Errorf("list of an interrupted prefix: %s", stderr)
	}
}
