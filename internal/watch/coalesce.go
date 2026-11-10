package watch

import (
	"path"
	"sort"
	"time"
)

// ChangeKind is what happened to a path, after coalescing.
//
// These are stable identifiers: they cross a package boundary into whatever
// drives the incremental re-index, so changing one is an API change.
type ChangeKind string

const (
	ChangeCreated  ChangeKind = "created"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeRenamed  ChangeKind = "renamed"
)

// Change is one coalesced change to one path.
type Change struct {
	// Path is repo-relative and ALWAYS uses forward slashes, matching
	// fs.Discovered.Path so a consumer can look the file up in the index
	// without translating separators.
	Path string

	// OldPath is set only for ChangeRenamed, and is the pre-rename relative
	// path. A consumer can then move the file's rows instead of deleting and
	// re-parsing them.
	OldPath string

	Kind  ChangeKind
	IsDir bool
}

// op is a filesystem event class, decoupled from fsnotify's Op so the
// coalescing logic can be tested without constructing backend events.
type op uint8

const (
	opCreate op = iota + 1
	opWrite
	opRemove
	opRename
	opChmod
)

// pathAgg accumulates every event seen for one path inside one debounce window.
//
// Only the FIRST op is kept, not the whole sequence, because the first op is
// the only one that carries information the filesystem cannot re-tell us at
// flush time. Whether the path exists now is a question we answer by looking,
// which is more reliable than replaying a sequence of events that the kernel is
// explicitly allowed to drop or coalesce.
type pathAgg struct {
	first     op
	sawRename bool // a rename moved this path away; it may pair with a create
	isDir     bool // best knowledge from the caller; a deleted dir cannot be stat'ed
	deadline  time.Time
}

// existsFunc reports whether a repo-relative path currently exists and whether
// it is a directory. Injected so coalescing is unit-testable with no
// filesystem, and so the watcher can use a single lstat per flushed path.
type existsFunc func(rel string) (exists, isDir bool)

// coalescer is the debounce state machine. It performs no I/O beyond the
// injected existsFunc, holds no locks, and is driven entirely by its caller's
// clock — every method takes the current time rather than reading one. That is
// what makes debounce behaviour testable without sleeping.
type coalescer struct {
	debounce time.Duration
	exists   existsFunc
	pending  map[string]*pathAgg
}

func newCoalescer(debounce time.Duration, exists existsFunc) *coalescer {
	return &coalescer{
		debounce: debounce,
		exists:   exists,
		pending:  make(map[string]*pathAgg),
	}
}

// add records one raw event and (re)starts the quiet period for its path.
//
// The deadline is pushed out on every event, so a burst of ten writes produces
// one flush a quiet period after the LAST write rather than ten flushes. This
// is the whole point: editors emit write/rename/chmod storms per save, and a
// consumer that re-parses per event does five times the work per keystroke.
func (c *coalescer) add(rel string, o op, isDir bool, now time.Time) {
	agg, ok := c.pending[rel]
	if !ok {
		// A chmod on a path with no other activity is not a change. Content is
		// identical, so re-indexing would produce a byte-identical result. It
		// must not open a window of its own — Go tooling chmods files in bulk.
		if o == opChmod {
			return
		}
		agg = &pathAgg{first: o}
		c.pending[rel] = agg
	}
	if isDir {
		// isDir is sticky: a remove event carries no type information, so the
		// directory-ness observed at create time is the only thing we will ever
		// know about a directory that has already been deleted.
		agg.isDir = true
	}
	if o == opRename {
		agg.sawRename = true
	}
	agg.deadline = now.Add(c.debounce)
}

// nextDeadline returns the earliest pending deadline.
func (c *coalescer) nextDeadline() (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, agg := range c.pending {
		if !found || agg.deadline.Before(earliest) {
			earliest, found = agg.deadline, true
		}
	}
	return earliest, found
}

func (c *coalescer) empty() bool { return len(c.pending) == 0 }

// candidate is one due path, resolved against the filesystem but not yet
// classified, because classification depends on the other paths in the batch
// (rename pairing).
type candidate struct {
	rel    string
	agg    *pathAgg
	exists bool
	isDir  bool
}

// due flushes every path whose quiet period has elapsed, as one batch.
//
// Paths that are still receiving events stay pending: one noisy file must not
// hold back a batch, and must not be reported half-written either.
//
// The returned slice is sorted by Path. Order is not semantically meaningful,
// but a consumer that hashes or logs batches should not see it wobble between
// runs, and Go's map iteration order guarantees it would.
func (c *coalescer) due(now time.Time) []Change {
	var ready []candidate
	for rel, agg := range c.pending {
		if agg.deadline.After(now) {
			continue
		}
		delete(c.pending, rel)
		exists, isDir := c.exists(rel)
		ready = append(ready, candidate{rel: rel, agg: agg, exists: exists, isDir: isDir || agg.isDir})
	}
	if len(ready) == 0 {
		return nil
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].rel < ready[j].rel })

	changes := pairRenames(ready)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// pairRenames classifies a batch, turning matched rename-out/create pairs into
// a single ChangeRenamed.
//
// WHY THIS IS A HEURISTIC. inotify emits a MOVED_FROM/MOVED_TO pair sharing a
// cookie that identifies the two halves exactly, but fsnotify does not surface
// the cookie, and kqueue on macOS/BSD has no equivalent at all — there we see a
// rename on the old path and a create on the new one with nothing linking them.
// So pairing is inference, and it is deliberately conservative:
//
//   - same base name wins first, which covers moving a file between directories
//     (git mv, an editor's "move to folder");
//   - otherwise a pair is only made when exactly one unmatched rename-out and
//     exactly one unmatched create remain in the window, so two simultaneous
//     renames never get cross-wired into two wrong ones;
//   - a directory is never paired with a file.
//
// Anything unpaired degrades to deleted + created, which is always correct —
// just more expensive for the consumer to act on.
func pairRenames(ready []candidate) []Change {
	// Rename-out: the path was renamed and is gone. Create-in: the path is
	// present and its first event in this window was a create.
	var outs, ins []int
	for i := range ready {
		switch {
		case ready[i].agg.sawRename && !ready[i].exists:
			outs = append(outs, i)
		case ready[i].exists && ready[i].agg.first == opCreate:
			ins = append(ins, i)
		}
	}

	paired := make(map[int]int, len(outs)) // out index -> in index
	usedIn := make(map[int]bool, len(ins))

	match := func(pred func(o, n candidate) bool) {
		for _, oi := range outs {
			if _, done := paired[oi]; done {
				continue
			}
			for _, ni := range ins {
				if usedIn[ni] || ready[oi].isDir != ready[ni].isDir {
					continue
				}
				if pred(ready[oi], ready[ni]) {
					paired[oi], usedIn[ni] = ni, true
					break
				}
			}
		}
	}

	match(func(o, n candidate) bool { return path.Base(o.rel) == path.Base(n.rel) })
	if len(paired) < len(outs) && len(outs)-len(paired) == 1 && len(ins)-len(usedIn) == 1 {
		match(func(candidate, candidate) bool { return true })
	}

	changes := make([]Change, 0, len(ready))
	for i := range ready {
		if ni, ok := paired[i]; ok {
			changes = append(changes, Change{
				Path:    ready[ni].rel,
				OldPath: ready[i].rel,
				Kind:    ChangeRenamed,
				IsDir:   ready[ni].isDir,
			})
			continue
		}
		if usedIn[i] {
			continue // already emitted as the destination of a rename
		}
		if ch, ok := classify(ready[i]); ok {
			changes = append(changes, ch)
		}
	}
	return changes
}

// classify turns one unpaired candidate into a change, or reports that it is
// not a change at all.
//
// The rules follow from "what would a consumer have to do about this?":
//
//   - created then deleted inside one window: nothing ever became observable,
//     so there is nothing to index and nothing to remove. Emitting a delete for
//     a path the consumer never saw created would be a phantom.
//   - gone now, and it was not this window that created it: deleted.
//   - present now, and the window opened with a create: created.
//   - present now, and the window opened with a write or a remove: modified.
//     The remove case is not an oddity — it is how every atomic-save editor
//     works (write temp, remove target, rename temp into place), and reporting
//     that as "created" would lie about a file the consumer already has.
func classify(c candidate) (Change, bool) {
	ch := Change{Path: c.rel, IsDir: c.isDir}
	switch {
	case !c.exists && c.agg.first == opCreate:
		return Change{}, false
	case !c.exists:
		ch.Kind = ChangeDeleted
	case c.agg.first == opCreate:
		ch.Kind = ChangeCreated
	default:
		ch.Kind = ChangeModified
	}
	return ch, true
}
