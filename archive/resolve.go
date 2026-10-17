// Symlink containment check, an iterative form of POSIX path resolution
// (Linux path_resolution(7)), memoised per symlink.

package archive

import (
	"fmt"
	"slices"
	"strings"
)

// linkRes is where a symlink resolves to and how many symlinks resolving it
// followed, itself included.
type linkRes struct {
	stack []string // components relative to the extraction root
	hops  int
}

// resolver checks that symlinks resolve inside the extraction directory.
// Each symlink is resolved once and its result reused by every chain
// through it; steps counts component lookups and reused components across
// the whole archive against Limits.MaxSymlinkSteps.
type resolver struct {
	x     *extractor
	memo  map[string]linkRes
	busy  map[string]bool
	steps int
}

func newResolver(x *extractor) *resolver {
	return &resolver{x: x, memo: make(map[string]linkRes), busy: make(map[string]bool)}
}

func (r *resolver) step(n int) error {
	r.steps += n
	if r.steps > r.x.lim.MaxSymlinkSteps {
		return fmt.Errorf("%w: symlink resolution over %d steps", ErrLimit, r.x.lim.MaxSymlinkSteps)
	}
	return nil
}

// link resolves the symlink rel.
func (r *resolver) link(rel string) (linkRes, error) {
	if res, ok := r.memo[rel]; ok {
		return res, nil
	}
	if r.busy[rel] {
		return linkRes{}, fmt.Errorf("%w: symlink loop through %s", ErrUnsafePath, quote(rel))
	}
	r.busy[rel] = true
	defer delete(r.busy, rel)
	if err := r.x.ctx.Err(); err != nil {
		return linkRes{}, err
	}
	target, err := r.x.fs.readlink(rel)
	if err != nil {
		return linkRes{}, err
	}
	if err := checkLinkTarget(target, r.x.lim); err != nil {
		return linkRes{}, err
	}
	var stack []string
	if dir, _ := splitRel(rel); dir != "" {
		stack = strings.Split(dir, "/")
	}
	stack, hops, err := r.walk(stack, splitTarget(target), 1)
	if err != nil {
		return linkRes{}, fmt.Errorf("target %s: %w", quote(target), err)
	}
	res := linkRes{stack: slices.Clip(stack), hops: hops}
	r.memo[rel] = res
	return res, nil
}

// walk resolves comps from the directory stack, following symlinks on the
// real filesystem (so case-insensitive lookups are honoured) and failing if
// any step leaves the root. Components that do not exist (dangling links
// are normal: a library link may point into another package) are taken
// lexically, which can only be stricter.
func (r *resolver) walk(stack, comps []string, hops int) ([]string, int, error) {
	for _, c := range comps {
		if err := r.step(1); err != nil {
			return nil, 0, err
		}
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return nil, 0, fmt.Errorf("%w: resolves outside the destination", ErrUnsafePath)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		next := strings.Join(append(stack, c), "/")
		k, err := r.x.fs.kindOf(next)
		if err != nil {
			return nil, 0, err
		}
		if k != kindSymlink {
			stack = append(stack, c)
			continue
		}
		res, err := r.link(next)
		if err != nil {
			return nil, 0, err
		}
		if hops += res.hops; hops > maxSymlinkHops {
			return nil, 0, fmt.Errorf("%w: too many levels of symbolic links", ErrUnsafePath)
		}
		if err := r.step(len(res.stack)); err != nil {
			return nil, 0, err
		}
		stack = append(stack[:0:0], res.stack...)
	}
	return stack, hops, nil
}
