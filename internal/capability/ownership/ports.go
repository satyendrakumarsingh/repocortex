package ownership

import "context"

// Commit is one commit, reduced to what a routing decision needs.
//
// A near-copy of internal/acquire/git.Commit, restated here rather than
// imported, for two reasons that are both about what a port is for.
//
// The first is dependency direction. internal/acquire/git is an acquisition
// adapter that pulls go-git into everything it touches; a capability that named
// its types in its own signatures would drag a git implementation into the
// dependency graph of every consumer and every test double. CLAUDE.md §1 puts
// the interface at the consumer, and this is the consumer.
//
// The second is the one that would actually cause a bug. Paths here must already
// be in the INDEX's path space — repo-relative to the same root model.File.Path
// is relative to. A git worktree root and a RepoCortex project root are frequently
// not the same directory (a service inside a monorepo is the ordinary case), and
// a git path that has not been rebased onto the project root silently matches
// nothing: every person would rank at zero and the report would look like a
// repository nobody has ever touched. Accepting the adapter's type directly
// would make that translation easy to forget; a distinct type makes the caller
// perform it.
type Commit struct {
	SHA         string   `json:"sha"`
	AuthorName  string   `json:"author_name"`
	AuthorEmail string   `json:"author_email"`
	When        int64    `json:"when"`    // author time, Unix seconds
	Subject     string   `json:"subject"` // first line of the message
	Paths       []string `json:"paths"`   // repo-relative, index path space
}

// History is the read surface this package needs: recent commits with the paths
// they touched.
//
// One method. Everything else a routing report might want — per-file last
// modification, blame, a path-scoped log — is either derivable from this or too
// expensive to be worth exposing, and every method added here is a method a fake
// must implement before it can assert anything about ranking.
//
// limit is a bound, not a preference: reading paths costs one tree diff per
// commit, and an unbounded walk over a large history is the failure mode the
// whole design avoids. An implementation that cannot honour a bound should
// return an error rather than reading everything.
type History interface {
	Commits(ctx context.Context, limit int) ([]Commit, error)
}
