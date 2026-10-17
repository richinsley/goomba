// Ported from libmamba src/core/history.cpp (History::add_entry,
// UserRequest::prefilled) (BSD-3-Clause, see NOTICE).

package link

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/richinsley/goomba/record"
)

// historyCondaVersion is the "conda version" micromamba writes into
// conda-meta/history.
const historyCondaVersion = "3.8.0"

// HistoryEntry is one revision appended to conda-meta/history. It is a
// plain value; AppendHistoryEntry only reads it.
type HistoryEntry struct {
	// Time is written as "2006-01-02 15:04:05" in its own location.
	Time time.Time
	// Cmd is the command line that made the change.
	Cmd string
	// Removed and Added are the unlinked and linked packages, written as
	// "-<channel>/<subdir>::<dist>" and "+<channel>/<subdir>::<dist>".
	Removed, Added []*record.PackageRecord
	// Update, Remove and Neutered are the user's specs, written as
	// "# update specs: [...]" and so on when non-empty.
	Update, Remove, Neutered []string
}

// AppendHistory appends a revision to conda-meta/history in prefix,
// recording the command and the packages removed and added, in the format
// micromamba writes. Use AppendHistoryEntry to also record specs.
func AppendHistory(prefix string, cmd string, added, removed []*record.PackageRecord, now time.Time) error {
	return AppendHistoryEntry(prefix, HistoryEntry{Time: now, Cmd: cmd, Added: added, Removed: removed})
}

// AppendHistoryEntry appends e to conda-meta/history in prefix, creating
// the file and the conda-meta directory if needed. Text that would break
// the line-based format (line breaks in the command, specs or package
// fields) is refused.
func AppendHistoryEntry(prefix string, e HistoryEntry) error {
	fail := func(err error) error { return fmt.Errorf("link: append history: %w", err) }
	prefix, err := checkPrefix(prefix)
	if err != nil {
		return fail(err)
	}
	text, err := formatHistoryEntry(e)
	if err != nil {
		return fail(err)
	}
	dir, _, err := newRoot(prefix).resolve("conda-meta", resolveCreate, nil, nil)
	if err != nil {
		return fail(err)
	}
	name := filepath.Join(dir, "history")
	if fi, err := os.Lstat(name); err == nil && !fi.Mode().IsRegular() {
		return fail(fmt.Errorf("%w: %s is not a regular file", ErrUnsafePath, name))
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return fail(err)
	}
	_, werr := f.WriteString(text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fail(werr)
	}
	return nil
}

func formatHistoryEntry(e HistoryEntry) (string, error) {
	var b strings.Builder
	if err := checkHistoryText("command", e.Cmd); err != nil {
		return "", err
	}
	b.WriteString("==> " + e.Time.Format(time.DateTime) + " <==\n")
	b.WriteString("# cmd: " + e.Cmd + "\n")
	b.WriteString("# conda version: " + historyCondaVersion + "\n")
	for _, group := range []struct {
		sign string
		recs []*record.PackageRecord
	}{{"-", e.Removed}, {"+", e.Added}} {
		for _, r := range group.recs {
			s, err := historyDist(r)
			if err != nil {
				return "", err
			}
			b.WriteString(group.sign + s + "\n")
		}
	}
	for _, group := range []struct {
		action string
		specs  []string
	}{{"update", e.Update}, {"remove", e.Remove}, {"neutered", e.Neutered}} {
		if len(group.specs) == 0 {
			continue
		}
		quoted := make([]string, len(group.specs))
		for i, s := range group.specs {
			if err := checkHistoryText("spec", s); err != nil {
				return "", err
			}
			quoted[i] = quoteCxx(s)
		}
		b.WriteString("# " + group.action + " specs: [" + strings.Join(quoted, ", ") + "]\n")
	}
	return b.String(), nil
}

// historyDist renders PackageInfo::long_str: "<channel>/<subdir>::<dist>".
func historyDist(r *record.PackageRecord) (string, error) {
	if r == nil {
		return "", errors.New("nil package record")
	}
	ch := strings.TrimSuffix(r.Channel, "/")
	if ch == "" {
		ch = channelFromURL(r.URL, r.Subdir, r.Fn)
	}
	s := ch + "/" + r.Subdir + "::" + r.Dist()
	if err := checkHistoryText("package", s); err != nil {
		return "", err
	}
	return s, nil
}

func checkHistoryText(what, s string) error {
	if strings.ContainsAny(s, "\n\r") {
		return fmt.Errorf("%w: %s %q contains a line break", ErrUnsafePath, what, s)
	}
	return nil
}

// quoteCxx mirrors std::quoted: wrap in double quotes, escaping '"' and
// '\' with a backslash.
func quoteCxx(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
