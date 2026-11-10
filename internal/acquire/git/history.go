package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Commit is one commit, reduced to what the indexer consumes.
//
// The full message body is deliberately absent: the context engine ranks and
// displays subjects, and carrying bodies through the graph would spend agent
// context on prose nobody reads.
type Commit struct {
	SHA         string `json:"sha"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`

	// When is the author timestamp in Unix seconds. Author time, not commit
	// time, because rebases and cherry-picks rewrite commit time and would make
	// untouched code look freshly changed to the recency ranker.
	When int64 `json:"when"`

	// Subject is the first line of the message, trimmed.
	Subject string `json:"subject"`

	// Paths are the repo-relative paths this commit touched, sorted. Populated
	// by RecentCommits and left nil by RecentCommitsMeta.
	Paths []string `json:"paths,omitempty"`
}

// FileRevision identifies the commit that last changed a path.
type FileRevision struct {
	SHA  string `json:"sha"`
	When int64  `json:"when"` // author timestamp, Unix seconds
}

// RecentCommits walks back from HEAD and returns at most limit commits, newest
// first, each with the paths it touched.
//
// limit must be positive; see ErrUnboundedWalk. A repository with no commits
// returns an empty slice and a nil error, because that is the state
// `cortex init` finds on a fresh project.
//
// Filling Paths costs one tree diff per commit and still dominates this call,
// but by a factor rather than by three orders of magnitude: on a 767-commit
// repository, 200 commits take 184 ms with paths against 98 ms without. It used
// to take 10.4 s, for the reason objectview.go documents. Use RecentCommitsMeta
// when only recency and authorship are needed, and persist the paths rather
// than recomputing them for the same commits.
func (r *Repo) RecentCommits(ctx context.Context, limit int) ([]Commit, error) {
	return r.recentCommits(ctx, limit, true)
}

// RecentCommitsMeta is RecentCommits without the per-commit tree diff. Paths is
// nil on every returned Commit.
func (r *Repo) RecentCommitsMeta(ctx context.Context, limit int) ([]Commit, error) {
	return r.recentCommits(ctx, limit, false)
}

func (r *Repo) recentCommits(ctx context.Context, limit int, withPaths bool) (out []Commit, err error) {
	if !r.isRepo {
		return nil, ErrNotRepository
	}
	if limit <= 0 {
		return nil, ErrUnboundedWalk
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The whole walk reads from one pooled view, so the packfile is opened once
	// rather than once per object. See objectview.go for the measurement.
	view, err := r.objectView()
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := view.close(); cerr != nil && err == nil {
			out, err = nil, cerr
		}
	}()

	// LogOrderCommitterTime rather than go-git's default depth-first order:
	// depth-first on a merge-heavy history returns one long ancestor chain
	// before the recent commits on other parents, so "the 20 newest commits"
	// would not actually be the 20 newest.
	iter, err := view.repo.Log(&gogit.LogOptions{Order: gogit.LogOrderCommitterTime})
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, nil // no commits yet
		}
		return nil, fmt.Errorf("git: open commit log: %w", err)
	}
	defer iter.Close()

	out = make([]Commit, 0, limit)
	for len(out) < limit {
		// Checked every iteration rather than only at entry: this loop is the
		// unbounded-work hot spot, and a cancelled index run must stop here
		// rather than after `limit` more tree diffs.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("git: walk commits: %w", err)
		}

		commit := Commit{
			SHA:         c.Hash.String(),
			AuthorName:  c.Author.Name,
			AuthorEmail: c.Author.Email,
			When:        c.Author.When.Unix(),
			Subject:     subject(c.Message),
		}
		if withPaths {
			paths, perr := r.commitPaths(ctx, c)
			if perr != nil {
				return nil, perr
			}
			commit.Paths = paths
		}
		out = append(out, commit)
	}

	return out, nil
}

// commitPaths lists the paths a commit touched, relative to its first parent.
//
// First parent only. For a merge, diffing against every parent reports the
// whole side branch as changed by the merge commit, which would tell the
// recency ranker that hundreds of untouched files just changed.
//
// Rename detection is off here on purpose: this answers "which paths does the
// indexer need to revisit", and an undetected rename simply yields both the old
// and the new path — which is the correct answer for that question, at a
// fraction of the cost of the quadratic similarity search.
func (r *Repo) commitPaths(ctx context.Context, c *object.Commit) ([]string, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("git: read tree of %s: %w", c.Hash, err)
	}

	var parentTree *object.Tree
	if c.NumParents() > 0 {
		parent, perr := c.Parent(0)
		if perr != nil {
			return nil, fmt.Errorf("git: read parent of %s: %w", c.Hash, perr)
		}
		if parentTree, err = parent.Tree(); err != nil {
			return nil, fmt.Errorf("git: read tree of %s: %w", parent.Hash, err)
		}
	}

	changes, err := object.DiffTreeContext(ctx, parentTree, tree)
	if err != nil {
		return nil, wrapCanceled(ctx, fmt.Errorf("git: diff commit %s: %w", c.Hash, err))
	}

	seen := make(map[string]struct{}, len(changes))
	paths := make([]string, 0, len(changes))
	for _, ch := range changes {
		for _, name := range []string{ch.From.Name, ch.To.Name} {
			p := normPath(name)
			if p == "" {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// FileLastModified reports the newest commit that changed the given path,
// walking back from HEAD.
//
// This is the recency signal behind context ranking. It is path-scoped, not
// content-scoped: like `git log -- <path>` without --follow, a rename ends the
// trail at the commit that performed it. Following renames would require a full
// similarity diff at every commit, which is not affordable here.
//
// A commit that deleted the path counts as having changed it, so callers asking
// about a removed file get the deletion rather than ErrNoHistory.
//
// The walk stops after MaxHistoryScan commits and returns ErrScanLimit. That
// bound exists because go-git has no path index: asking about a path that was
// never committed would otherwise read every commit in the repository.
func (r *Repo) FileLastModified(ctx context.Context, path string) (rev FileRevision, err error) {
	if !r.isRepo {
		return FileRevision{}, ErrNotRepository
	}
	path = normPath(path)
	if path == "" {
		return FileRevision{}, fmt.Errorf("git: FileLastModified: empty path")
	}

	// Up to MaxHistoryScan commits, two tree lookups each. Pooled for the same
	// reason recentCommits is; see objectview.go.
	view, err := r.objectView()
	if err != nil {
		return FileRevision{}, err
	}
	defer func() {
		if cerr := view.close(); cerr != nil && err == nil {
			rev, err = FileRevision{}, cerr
		}
	}()

	iter, err := view.repo.Log(&gogit.LogOptions{Order: gogit.LogOrderCommitterTime})
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return FileRevision{}, ErrNoHistory // no commits yet
		}
		return FileRevision{}, fmt.Errorf("git: open commit log: %w", err)
	}
	defer iter.Close()

	// Deliberately not go-git's LogOptions.PathFilter. That iterator gives no
	// way to observe or bound how many commits it consumed internally, and an
	// unbounded walk is precisely what MaxHistoryScan exists to prevent. Doing
	// the tree lookups here costs a FindEntry per commit — far cheaper than a
	// patch — and keeps the bound and the cancellation check under our control.
	for scanned := 0; scanned < MaxHistoryScan; scanned++ {
		if err := ctx.Err(); err != nil {
			return FileRevision{}, err
		}

		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return FileRevision{}, ErrNoHistory
		}
		if err != nil {
			return FileRevision{}, fmt.Errorf("git: walk commits: %w", err)
		}

		hash, present, err := blobAt(c, path)
		if err != nil {
			return FileRevision{}, err
		}

		if c.NumParents() == 0 {
			// Root commit: the file's presence here is its introduction.
			if present {
				return FileRevision{SHA: c.Hash.String(), When: c.Author.When.Unix()}, nil
			}
			return FileRevision{}, ErrNoHistory
		}

		parent, err := c.Parent(0)
		if err != nil {
			return FileRevision{}, fmt.Errorf("git: read parent of %s: %w", c.Hash, err)
		}
		parentHash, parentPresent, err := blobAt(parent, path)
		if err != nil {
			return FileRevision{}, err
		}

		if present != parentPresent || hash != parentHash {
			return FileRevision{SHA: c.Hash.String(), When: c.Author.When.Unix()}, nil
		}
	}

	return FileRevision{}, ErrScanLimit
}

// blobAt reports the object hash of path in a commit's tree, and whether it is
// present at all. A missing entry is a normal answer, not an error — the whole
// point of the caller's comparison is to notice appearance and disappearance.
func blobAt(c *object.Commit, path string) (plumbing.Hash, bool, error) {
	tree, err := c.Tree()
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("git: read tree of %s: %w", c.Hash, err)
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return plumbing.ZeroHash, false, nil
		}
		return plumbing.ZeroHash, false, fmt.Errorf("git: look up %q in %s: %w", path, c.Hash, err)
	}
	return entry.Hash, true, nil
}

// subject returns the first line of a commit message.
func subject(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}
