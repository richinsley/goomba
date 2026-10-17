package repodata

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/richinsley/goomba/record"
)

// Memory is a Source over a fixed set of records, for tests and for
// records that come from somewhere other than a channel. Records are
// returned as given, without normalization. It is safe for concurrent use.
type Memory struct {
	byName map[string][]*record.PackageRecord
}

// NewMemory indexes records by name (case-insensitively). Nil entries are
// ignored. The records are shared, not copied.
func NewMemory(records []*record.PackageRecord) *Memory {
	m := &Memory{byName: map[string][]*record.PackageRecord{}}
	for _, r := range records {
		if r == nil {
			continue
		}
		k := strings.ToLower(r.Name)
		m.byName[k] = append(m.byName[k], r)
	}
	return m
}

// Records implements Source.
func (m *Memory) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]*record.PackageRecord(nil), m.byName[strings.ToLower(name)]...), nil
}

// HasName implements NameLister.
func (m *Memory) HasName(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, ok := m.byName[strings.ToLower(name)]
	return ok, nil
}

// Multi combines sources with strict channel priority: for each name, only
// the first source that lists it answers. A source implementing NameLister
// answers when HasName reports the name, even with no records left after
// filtering; for other sources, listing means returning at least one
// record. It is safe for concurrent use if its sources are.
type Multi struct {
	sources []Source
	err     error // a nil source was given
}

// NewMulti returns a Multi over sources, highest priority first. A nil
// source (also a typed nil such as (*ShardedChannel)(nil)) makes every
// Records call fail: silently skipping a channel would change which
// channel answers for a name.
func NewMulti(sources ...Source) *Multi {
	m := &Multi{sources: sources}
	for i, s := range sources {
		if isNilSource(s) {
			m.err = fmt.Errorf("repodata: NewMulti: source %d is nil", i)
			break
		}
	}
	return m
}

func isNilSource(s Source) bool {
	if s == nil {
		return true
	}
	v := reflect.ValueOf(s)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return v.IsNil()
	}
	return false
}

// Records implements Source. An error from a source is returned at once:
// without its answer the priority order cannot be honoured, so Multi never
// falls through to a lower-priority source on failure.
func (m *Multi) Records(ctx context.Context, name string) ([]*record.PackageRecord, error) {
	if m.err != nil {
		return nil, m.err
	}
	for _, s := range m.sources {
		if nl, ok := s.(NameLister); ok {
			listed, err := nl.HasName(ctx, name)
			if err != nil {
				return nil, err
			}
			if !listed {
				continue
			}
			return s.Records(ctx, name)
		}
		recs, err := s.Records(ctx, name)
		if err != nil {
			return nil, err
		}
		if len(recs) > 0 {
			return recs, nil
		}
	}
	return nil, ctx.Err()
}

// HasName implements NameLister: whether any source lists name.
func (m *Multi) HasName(ctx context.Context, name string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	for _, s := range m.sources {
		if nl, ok := s.(NameLister); ok {
			listed, err := nl.HasName(ctx, name)
			if err != nil || listed {
				return listed, err
			}
			continue
		}
		recs, err := s.Records(ctx, name)
		if err != nil || len(recs) > 0 {
			return len(recs) > 0, err
		}
	}
	return false, ctx.Err()
}

// Prefetch implements Prefetcher by forwarding to every source that
// implements it.
func (m *Multi) Prefetch(ctx context.Context, names []string) {
	if m.err != nil {
		return
	}
	for _, s := range m.sources {
		if p, ok := s.(Prefetcher); ok {
			p.Prefetch(ctx, names)
		}
	}
}
