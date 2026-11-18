package mcp

import (
	"context"
	"fmt"
	"time"
)

// Answers that follow an agent's edits.
//
// The session an agent works in goes ask, edit, ask again. The index used to
// change only when `watch`, a git hook or a manual `index` ran, so the second
// question was answered from the code as it was before the edit: callers that
// no longer exist, a signature that changed, a function the agent just added
// reported as absent. A Refresher closes that: before a tool runs, the files
// changed since the last check are re-indexed, and the answer says which.

// Refresher brings the index up to date with the working tree before a tool
// answers. The server holds a read-only handle on the index; a Refresher has
// its own way to write, so the agent's own connection never can.
type Refresher interface {
	Refresh(ctx context.Context) RefreshOutcome
}

// RefreshOutcome is what one check found and did.
type RefreshOutcome struct {
	// LastCheck is when the working tree was last compared with the index:
	// by this call, or by an earlier one when this call came too soon after
	// it to check again. Zero when no check has run yet.
	LastCheck time.Time
	// Refreshed are the files this call re-indexed, in the order found.
	Refreshed []string
	// Busy is true when another process (a git hook's background pass, a
	// watcher) was writing the index, so this call did not check.
	Busy bool
	// Pending is how many changed files are still being re-indexed in the
	// background after this call refreshed what its budget allowed. While it
	// is above zero the index is partly behind the code.
	Pending int
	// Err is a check or an update that failed. The answer is then given from
	// the index as it stands, and says so.
	Err error
}

type refreshKey struct{}

func withRefresh(ctx context.Context, o RefreshOutcome) context.Context {
	return context.WithValue(ctx, refreshKey{}, o)
}

func refreshFrom(ctx context.Context) (RefreshOutcome, bool) {
	o, ok := ctx.Value(refreshKey{}).(RefreshOutcome)
	return o, ok
}

// maxRefreshedListed bounds the files named in one answer's provenance. A
// branch switch can change thousands; the count says how many more.
const maxRefreshedListed = 10

// applyRefresh copies what the check found into the provenance block, and
// replaces the "working-tree edits are invisible" caveat when they are not.
func applyRefresh(p *Provenance, o RefreshOutcome) {
	if n := len(o.Refreshed); n > 0 {
		shown := o.Refreshed
		if n > maxRefreshedListed {
			shown = shown[:maxRefreshedListed]
			p.RefreshedMore = n - maxRefreshedListed
		}
		p.RefreshedFiles = append([]string(nil), shown...)
	}
	if !o.LastCheck.IsZero() {
		p.EditsCheckedAt = o.LastCheck.UTC().Format(time.RFC3339)
	}
	p.IndexBusy = o.Busy || o.Pending > 0
	p.RefreshPending = o.Pending
	switch {
	case o.Pending > 0:
		noun := "files are"
		if o.Pending == 1 {
			noun = "file is"
		}
		p.Caveat = fmt.Sprintf("The index is busy: %d changed %s still being re-indexed in the background "+
			"(a large edit, such as a branch switch, is refreshed in steps so one answer never waits for all of "+
			"it). Answers about those files may be out of date; ask again for progress. ", o.Pending, noun) + p.Caveat
	case o.Err != nil:
		p.Caveat = "Checking the working tree for edits failed (" + o.Err.Error() + "), so this answer is from the " +
			"index as it stands. " + p.Caveat
	case o.Busy:
		p.Caveat = "Another process was updating the index, so edits from the last few seconds may not be in this " +
			"answer yet; ask again to pick them up. " + p.Caveat
	case !o.LastCheck.IsZero():
		p.Caveat = editsCheckedCaveat(p, o.LastCheck)
	}
}

// editsCheckedCaveat states what a checked index covers. The commit comparison
// above still holds; what changes is that working-tree edits up to the check
// are in the index.
func editsCheckedCaveat(p *Provenance, at time.Time) string {
	s := "Working-tree edits (saved files, staged or not) were checked at " + at.UTC().Format(time.RFC3339) +
		" and every file changed by then is in this answer"
	if n := len(p.RefreshedFiles) + p.RefreshedMore; n > 0 {
		noun := "files"
		if n == 1 {
			noun = "file"
		}
		s += fmt.Sprintf("; %d %s changed since the previous check %s re-indexed first", n, noun, verbBe(n))
	}
	return s + ". A file saved after that moment is not, and unsaved editor buffers never are."
}

func verbBe(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
