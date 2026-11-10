package git

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

// ChangeKind classifies what happened to a path between two revisions.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"

	// ChangeRenamed carries both names. The indexer relies on this to *move*
	// a file's symbols and their inbound edges; treating a rename as
	// delete+add would discard every edge pointing into that file and rebuild
	// them at lower confidence.
	ChangeRenamed ChangeKind = "renamed"
)

// Change is one path's fate between two revisions.
type Change struct {
	// Path is the path after the change. For ChangeDeleted it is the path the
	// file had when it was removed, so Path is never empty.
	Path string `json:"path"`

	// OldPath is set only for ChangeRenamed, and holds the pre-rename path.
	OldPath string `json:"old_path,omitempty"`

	Kind ChangeKind `json:"kind"`
}

// ChangedSince reports the paths that differ between two revisions.
//
// Both arguments accept anything `git rev-parse` would: a full or abbreviated
// SHA, a branch or tag name, HEAD, HEAD~3.
//
// The empty string means different things on each side, and deliberately so:
//
//	from == ""  the empty tree — every file at `to` is reported as added. This
//	            is what a first-ever index run asks for.
//	to   == ""  HEAD. This is what "what changed since the last indexed
//	            revision?" asks for.
//
// Rename detection is on, bounded by renameLimit. Results are sorted by Path.
func (r *Repo) ChangedSince(ctx context.Context, from, to string) (out []Change, err error) {
	if !r.isRepo {
		return nil, ErrNotRepository
	}
	// Checked up front so that an already-cancelled context fails immediately
	// and identically regardless of how large the diff turns out to be.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// A wide diff with rename detection reads a blob per candidate. Pooled for
	// the same reason the history walks are; see objectview.go.
	view, err := r.objectView()
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := view.close(); cerr != nil && err == nil {
			out, err = nil, cerr
		}
	}()

	fromTree, err := view.treeAt(from)
	if err != nil {
		return nil, err
	}
	if to == "" {
		to = "HEAD"
	}
	toTree, err := view.treeAt(to)
	if err != nil {
		return nil, err
	}

	changes, err := object.DiffTreeWithOptions(ctx, fromTree, toTree, &object.DiffTreeOptions{
		DetectRenames: true,
		RenameScore:   60, // go-git's recommended default; matches git's -M50..60 range
		RenameLimit:   renameLimit,
	})
	if err != nil {
		return nil, wrapCanceled(ctx, fmt.Errorf("git: diff %s..%s: %w", from, to, err))
	}

	out = make([]Change, 0, len(changes))
	for _, c := range changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ch, ok, err := classify(c)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ch)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// classify maps a go-git change onto a ChangeKind.
//
// go-git reports a rename as a Modify whose From and To names differ — there is
// no distinct rename action — so the name comparison, not the action, is what
// distinguishes the two cases.
func classify(c *object.Change) (Change, bool, error) {
	action, err := c.Action()
	if err != nil {
		// A change with neither side populated is a go-git internal
		// inconsistency; surfacing it beats emitting a Change with an empty
		// path that would later corrupt the file table.
		return Change{}, false, fmt.Errorf("git: classify change: %w", err)
	}

	switch action {
	case merkletrie.Insert:
		return Change{Path: normPath(c.To.Name), Kind: ChangeAdded}, true, nil
	case merkletrie.Delete:
		return Change{Path: normPath(c.From.Name), Kind: ChangeDeleted}, true, nil
	default:
		from, to := normPath(c.From.Name), normPath(c.To.Name)
		if from != to {
			return Change{Path: to, OldPath: from, Kind: ChangeRenamed}, true, nil
		}
		return Change{Path: to, Kind: ChangeModified}, true, nil
	}
}

// treeAt resolves a revision to its tree. An empty revision yields a nil tree,
// which go-git's differ treats as the empty tree.
//
// Declared on the view rather than on Repo so the tree and everything reachable
// from it carry the pooled storer: a tree read through one repository and
// diffed through another would go back to a packfile open per object.
func (v *objectView) treeAt(rev string) (*object.Tree, error) {
	if rev == "" {
		return nil, nil
	}

	hash, err := v.repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("git: resolve revision %q: %w", rev, err)
	}
	commit, err := v.repo.CommitObject(*hash)
	if err != nil {
		return nil, fmt.Errorf("git: read commit %s: %w", hash.String(), err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("git: read tree of %s: %w", hash.String(), err)
	}
	return tree, nil
}

// wrapCanceled restores the caller's context error when go-git has swallowed
// it. go-git's differ converts cancellation into its own object.ErrCanceled,
// which would make errors.Is(err, context.Canceled) false at the call site and
// send a cancelled request down an error-reporting path instead of a
// shutdown path.
func wrapCanceled(ctx context.Context, err error) error {
	if errors.Is(err, object.ErrCanceled) {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
	}
	return err
}

// ResolveRevision turns anything `git rev-parse` accepts into a full commit SHA.
//
// It exists so a caller can decide whether two revision NAMES designate the
// same commit. Comparing the strings cannot answer that: "HEAD", "main" and
// "9685bd8" are routinely the same commit and never the same string, and a
// staleness check built on string equality would either warn on every correct
// index or, worse, miss a genuinely unrelated one.
//
// An empty revision resolves to "" with a nil error, matching ChangedSince's
// convention that the empty string is a position (the empty tree) rather than a
// name. A revision that does not resolve is an error, not "": not knowing and
// knowing-it-is-nothing are different answers, and only the caller can decide
// what to do about the first.
func (r *Repo) ResolveRevision(ctx context.Context, rev string) (string, error) {
	if !r.isRepo || r.repo == nil {
		// Includes the degraded state: without go-git's object store there is
		// no revision parser here, and inventing one would be a second
		// implementation of rev-parse.
		return "", ErrNotRepository
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if rev == "" {
		return "", nil
	}
	hash, err := r.repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return "", fmt.Errorf("git: resolve revision %q: %w", rev, err)
	}
	return hash.String(), nil
}
