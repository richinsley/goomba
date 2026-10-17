// Subdir loading follows libmamba src/core/subdir_index.cpp
// (BSD-3-Clause, see NOTICE): the platform subdir may be missing, noarch
// may not; compressed variants are preferred.

package repodata

import (
	"bytes"
	"compress/bzip2"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/richinsley/goomba/record"
)

// FullIndex is a Source backed by the full repodata.json of a channel's
// platform and noarch subdirs, loaded completely at construction. It is
// safe for concurrent use.
type FullIndex struct {
	channel string
	byName  map[string][]*record.PackageRecord
	listed  map[string]struct{} // every name the indexes list, before filtering
}

// NewFullIndex downloads (or reads from the cache) the repodata.json of
// <channelURL>/<opt.Platform> and <channelURL>/noarch and indexes the
// records by name. For each subdir it tries repodata.json.zst, then
// repodata.json, then repodata.json.bz2. A missing platform subdir is
// treated as empty (as libmamba does); a missing noarch subdir is an error
// wrapping ErrNotFound. An index larger than goomba's limits (512 MiB
// compressed, 1 GiB decompressed) or whose decoded records would take far
// more memory than its size warrants is an error wrapping ErrTooLarge.
func NewFullIndex(ctx context.Context, channelURL string, opt Options) (*FullIndex, error) {
	s, err := opt.settings()
	if err != nil {
		return nil, err
	}
	ch, pol, err := canonicalChannel(channelURL)
	if err != nil {
		return nil, err
	}
	f := newFetcher(s, s.client, pol)
	cum := &cumulative{max: s.maxRetained}
	fi := &FullIndex{channel: ch.String(), byName: map[string][]*record.PackageRecord{}, listed: map[string]struct{}{}}
	results := make([][]*record.PackageRecord, len(s.subdirs))
	names := make([]map[string]struct{}, len(s.subdirs))
	errs := make([]error, len(s.subdirs))
	var wg sync.WaitGroup
	for i, sd := range s.subdirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], names[i], errs[i] = loadFullSubdir(ctx, f, ch, sd, s.python, cum)
		}()
	}
	wg.Wait()
	for i, sd := range s.subdirs {
		if err := errs[i]; err != nil {
			if errors.Is(err, ErrNotFound) && sd != "noarch" {
				continue
			}
			return nil, err
		}
		for n := range names[i] {
			fi.listed[n] = struct{}{}
		}
		for _, r := range results[i] {
			k := strings.ToLower(r.Name)
			fi.byName[k] = append(fi.byName[k], r)
		}
	}
	for _, recs := range fi.byName {
		sortRecords(recs)
	}
	return fi, nil
}

// Records implements Source.
func (fi *FullIndex) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	recs := fi.byName[strings.ToLower(name)]
	return append([]*record.PackageRecord(nil), recs...), nil
}

// HasName implements NameLister: whether the indexes list name, whether or
// not any of its records survived filtering.
func (fi *FullIndex) HasName(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, ok := fi.listed[strings.ToLower(name)]
	return ok, nil
}

// Names returns the sorted names of all packages with records in the index.
func (fi *FullIndex) Names() []string {
	out := make([]string, 0, len(fi.byName))
	for n := range fi.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// repodataVariants lists the file names tried for a full index, best first.
func repodataVariants() [3]string {
	return [3]string{"repodata.json.zst", "repodata.json", "repodata.json.bz2"}
}

func loadFullSubdir(ctx context.Context, f *fetcher, ch *url.URL, subdir string, py *pythonMinor, cum *cumulative) ([]*record.PackageRecord, map[string]struct{}, error) {
	base := subdirURL(ch, subdir)
	var firstErr error
	for _, name := range repodataVariants() {
		u := base.JoinPath(name).String()
		body, err := f.fetchIndex(ctx, u, maxIndexDownload)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotCached) {
			if firstErr == nil || errors.Is(firstErr, ErrNotFound) && errors.Is(err, ErrNotCached) {
				firstErr = err
			}
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		rc, err := decompressIndex(name, body)
		if err != nil {
			return nil, nil, fmt.Errorf("repodata: %s: %w", redactString(u), err)
		}
		defer rc.Close()
		return loadRepodataJSONBudget(&ctxReader{ctx: ctx, r: &limitedReader{r: rc, left: maxIndexJSON}}, ch, f.pol, subdir, py,
			func(input func() int64) *budget {
				return &budget{base: jsonBudgetBase, ratio: jsonBudgetRatio, input: input, cum: cum}
			})
	}
	return nil, nil, firstErr
}

// decompressIndex returns a reader of the decompressed index; the caller
// closes it.
func decompressIndex(name string, body []byte) (io.ReadCloser, error) {
	switch {
	case strings.HasSuffix(name, ".zst"):
		d, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	case strings.HasSuffix(name, ".bz2"):
		return io.NopCloser(bzip2.NewReader(bytes.NewReader(body))), nil
	default:
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

// limitedReader fails with ErrTooLarge instead of truncating silently.
type limitedReader struct {
	r    io.Reader
	left int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			return 0, fmt.Errorf("decompressed index: %w", ErrTooLarge)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// ctxReader stops a long parse promptly when ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
