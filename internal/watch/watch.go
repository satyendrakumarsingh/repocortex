// Package watch turns a directory tree into a stream of debounced, coalesced
// change sets.
//
// It is a pure event producer. It does not index, parse, hash, or touch the
// registry — something downstream does that with the batches this package
// emits. That boundary is deliberate: it is what lets the hard parts here
// (recursive watch bookkeeping, debounce timing, rename inference) be tested
// deterministically, with a fake clock and a fake fsnotify backend, instead of
// by writing files and hoping the kernel is quick today.
//
// Three problems dominate the implementation.
//
//  1. RECURSION IS NOT PROVIDED. fsnotify watches a single directory; a
//     subdirectory created a second later is invisible until something adds a
//     watch for it. Maintaining that set as directories appear, disappear and
//     get renamed is where watchers usually go wrong. See dirs.go.
//
//  2. WATCHES ARE A FINITE RESOURCE. One inotify watch per directory on Linux,
//     one file descriptor per directory on macOS/BSD. Ignored directories are
//     never watched — a watch on .git alone produces thousands of useless
//     events per `git status` — and the total is bounded by MaxWatchDirs, with
//     ErrWatchLimit reported rather than changes silently going missing.
//
//  3. EDITORS DO NOT SAVE ONCE. A single ⌘S is a write, a rename, a chmod and
//     sometimes a create, per save. Events are therefore debounced per path and
//     coalesced into one Change, so ten writes to a file cost the consumer one
//     re-index rather than ten. See coalesce.go.
//
// Cross-platform note: inotify, kqueue and ReadDirectoryChangesW disagree about
// which events a given operation produces, so this package never depends on a
// specific event sequence. It records the FIRST event seen for a path and then
// asks the filesystem what is true at flush time; that answer is the same
// everywhere.
package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce is the quiet period a path must have before its change is
// emitted. Long enough to swallow an editor's save burst and a `git checkout`,
// short enough that a human does not perceive the resulting re-index as lag.
const DefaultDebounce = 300 * time.Millisecond

// DefaultMaxWatchDirs bounds the watched directory count.
//
// 8192 is the historical Linux default for max_user_watches, which makes it the
// limit a user is most likely to actually hit; staying under it means we report
// ErrWatchLimit with a path and a number instead of letting inotify_add_watch
// fail with ENOSPC halfway through a tree. Source repositories with the
// standard exclusions applied are far below it.
const DefaultMaxWatchDirs = 8192

// errorBuffer is the depth of the Errors channel. Errors here are rare and
// individually meaningful (watch limit, backend overflow), so they are buffered
// rather than dropped; a caller that never drains Errors will eventually stall
// the loop, which is documented on Errors and is the honest failure mode.
const errorBuffer = 16

// Options configures a Watcher. The zero value is usable: every field has a
// working default.
type Options struct {
	// Ignore reports whether a repo-relative path is out of scope. It is a
	// function rather than a pattern list so that the caller can supply the
	// acquisition layer's ignore rules (.gitignore, config excludes) without
	// this package importing — and thereby depending on the evolution of —
	// internal/acquire/fs.
	//
	// It is consulted before a directory is watched, never after, so an ignored
	// subtree costs no watch descriptors at all.
	//
	// .git, node_modules and .cortex are always ignored regardless of what
	// this returns; see builtinIgnoredDirs.
	Ignore func(relPath string, isDir bool) bool

	// Debounce is the per-path quiet period. Defaults to DefaultDebounce.
	Debounce time.Duration

	// MaxWatchDirs bounds watched directories. Defaults to
	// DefaultMaxWatchDirs.
	MaxWatchDirs int

	// Clock is the time source. Defaults to RealClock. Tests inject
	// ManualClock to make debounce behaviour deterministic.
	Clock Clock

	// Context, when non-nil, shuts the watcher down on cancellation exactly as
	// Close does. Close is still required to release the backend's resources
	// and to learn whether that succeeded.
	Context context.Context

	// backend is injected by tests. Nil means a real fsnotify watcher.
	backend backend

	// synced, when non-nil, receives once per event the loop has finished
	// handling. Test-only, and it exists because of a real trade-off.
	//
	// The event loop no longer reads the backend directly — a buffered drain
	// sits in between, so that slow work inside handle cannot stall fsnotify
	// and lose kernel events (see drainBackend). That buffer removed the
	// property the fake-backend tests were built on: with an unbuffered
	// channel, a completed send meant the loop had finished processing.
	//
	// Rather than weaken the fix to keep tests convenient, the loop offers an
	// explicit barrier. A test sends an event, waits here, and knows exactly
	// what has been applied — a stronger guarantee than the old accidental one,
	// and one that does not depend on channel capacity at all.
	//
	// Production cost is a nil check per event.
	synced chan struct{}
}

// Watcher watches a tree and emits coalesced change batches.
//
// Two goroutines are started per Watcher — a drain that keeps the backend's
// channel empty, and the event loop — plus whatever the backend runs itself.
// All mutable state below is confined to the event loop after New returns, so
// there are no locks on the event path.
type Watcher struct {
	root    string
	clock   Clock
	dirs    *dirSet
	co      *coalescer
	backend backend
	ctxDone <-chan struct{}

	// events is the drain's output: a buffered queue between the backend and
	// the event loop. The loop MUST NOT read the backend directly — see
	// drainBackend for the bug that caused.
	events  chan fsnotify.Event
	dropped atomic.Int64

	// rewatch holds directories registered since the last flush, absolute path
	// to repo-relative, awaiting the repair pass. Loop-owned, like dirs.
	rewatch map[string]string

	changes  chan []Change
	errs     chan error
	done     chan struct{}
	synced   chan struct{}
	wg       sync.WaitGroup
	closeOne sync.Once
	closeErr error
}

// New starts watching root.
//
// The initial pass registers watches for the whole tree before returning, so a
// change made after New returns cannot be missed. It does NOT emit changes for
// what already exists: the caller is expected to have indexed the tree, and
// replaying it as a batch of creates would duplicate that work.
//
// Returning an error wrapping ErrWatchLimit means the tree is bigger than the
// configured bound. That is a hard failure by design — a watcher that quietly
// covers 60% of a repository is worse than one that refuses to start, because
// the missing 40% looks exactly like "nothing changed".
func New(root string, opts Options) (*Watcher, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("watch: root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("watch: resolve root %q: %w", root, err)
	}
	// Symlinks are resolved once, here, for the same reason the acquisition
	// walker does it: on macOS /tmp is a symlink to /private/tmp and fsnotify
	// reports the resolved path, so an unresolved root would make
	// filepath.Rel produce ../../ nonsense for every single event.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("watch: resolve symlinks in root %q: %w", abs, err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("watch: stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watch: root %q is not a directory", resolved)
	}

	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	maxDirs := opts.MaxWatchDirs
	if maxDirs <= 0 {
		maxDirs = DefaultMaxWatchDirs
	}
	clock := opts.Clock
	if clock == nil {
		clock = RealClock{}
	}

	b := opts.backend
	if b == nil {
		fsw, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, fmt.Errorf("watch: create backend: %w", err)
		}
		b = fsnotifyBackend{w: fsw}
	}

	w := &Watcher{
		root:    resolved,
		clock:   clock,
		dirs:    newDirSet(b, resolved, opts.Ignore, maxDirs),
		backend: b,
		events:  make(chan fsnotify.Event, eventQueueSize),
		rewatch: make(map[string]string),
		changes: make(chan []Change),
		errs:    make(chan error, errorBuffer),
		done:    make(chan struct{}),
		synced:  opts.synced,
	}
	w.co = newCoalescer(debounce, w.exists)
	if opts.Context != nil {
		w.ctxDone = opts.Context.Done()
	}

	// The drain starts BEFORE the initial walk, not after. That walk registers a
	// watch for every directory in the tree, so the backend begins producing
	// events partway through it — and on a large tree the walk is exactly the
	// long stretch during which nothing else would be reading.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		drainBackend(b.Events(), w.events, &w.dropped, w.done)
	}()

	// The entries discovered here are discarded: they are the pre-existing
	// tree, not changes.
	if _, err := w.dirs.addTree(resolved, ""); err != nil {
		close(w.done)
		_ = b.Close()
		w.wg.Wait()
		return nil, err
	}

	w.wg.Add(1)
	go w.run()
	return w, nil
}

// Root reports the absolute, symlink-resolved root being watched.
func (w *Watcher) Root() string { return w.root }

// WatchedDirs reports how many directories currently hold a watch. Safe to call
// from any goroutine at any time; intended for diagnostics, where the useful
// question is how close a repository is running to MaxWatchDirs.
func (w *Watcher) WatchedDirs() int { return w.dirs.count() }

// Changes delivers one coalesced batch per receive. Batches are non-empty and
// sorted by Path.
//
// The channel is unbuffered: back-pressure is a feature here, because a
// consumer that has not finished re-indexing the last batch should be
// coalescing further events into the next one rather than accumulating a queue
// of stale work. It is closed after Close, once the event loop has stopped.
func (w *Watcher) Changes() <-chan []Change { return w.changes }

// Errors delivers non-fatal problems: ErrWatchLimit for a subtree that could
// not be watched, and backend errors such as fsnotify.ErrEventOverflow.
//
// Drain it. These are conditions under which changes are being missed, so they
// are never discarded to keep the loop moving; a caller that ignores the
// channel entirely will eventually stall event delivery instead.
func (w *Watcher) Errors() <-chan error { return w.errs }

// Close stops watching and releases the backend.
//
// It is idempotent and safe to call concurrently with event delivery, from any
// goroutine: repeat calls return the first call's result without touching the
// backend again. When Close returns, the event loop has exited and both output
// channels are closed, so a consumer ranging over Changes terminates.
func (w *Watcher) Close() error {
	w.closeOne.Do(func() {
		// Order matters. Closing done first releases the loop even if it is
		// parked on an unbuffered send to a consumer that has gone away;
		// closing the backend then makes fsnotify's own goroutine exit.
		close(w.done)
		w.closeErr = w.backend.Close()
	})
	w.wg.Wait()
	return w.closeErr
}

// run is the event loop. It owns dirs, co and the debounce timer.
func (w *Watcher) run() {
	defer w.wg.Done()
	// The sender closes. Nothing else writes to these channels, so closing here
	// is what makes "Close returned, therefore no more batches" true.
	defer close(w.changes)
	defer close(w.errs)

	var (
		timer Timer
		tick  <-chan time.Time
	)
	// rearm points the loop at the earliest pending deadline. A single timer
	// serves all pending paths: waking once per due batch rather than once per
	// path keeps a checkout of ten thousand files to a handful of wakeups.
	rearm := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		tick = nil
		deadline, ok := w.co.nextDeadline()
		if !ok {
			return
		}
		d := deadline.Sub(w.clock.Now())
		if d < 0 {
			d = 0
		}
		timer = w.clock.NewTimer(d)
		tick = timer.C()
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	// events comes from the drain, NEVER from w.backend.Events() directly.
	// handle() below can spend real time in dirSet.addTree, and a loop that
	// consumed the backend's channel would stall it for that whole window —
	// which loses kernel events outright. See drainBackend.
	events := w.events
	backendErrs := w.backend.Errors()

	// reported tracks how many drops have already been surfaced, so an overflow
	// is announced once per burst rather than once per event.
	var reported int64
	reportDrops := func() {
		if n := w.dropped.Load(); n > reported {
			reported = n
			w.emitErr(overflowError(n))
		}
	}

	for {
		select {
		case <-w.done:
			return
		case <-w.ctxDone:
			return

		case ev, ok := <-events:
			if !ok {
				// The backend closed underneath us (Close, or fsnotify giving
				// up). Pending changes are abandoned rather than flushed: they
				// describe a tree we are no longer watching, so a consumer
				// acting on them would be working from a snapshot with no
				// guarantee that anything after it will be reported.
				return
			}
			// Re-arming only when the pending set actually changed keeps a
			// stream of ignored events (a build tool churning inside an
			// excluded directory) from repeatedly cancelling and recreating
			// the debounce timer for unrelated work.
			if w.handle(ev) {
				rearm()
			}
			w.signalSynced()

		case err, ok := <-backendErrs:
			if !ok {
				backendErrs = nil // keep selecting on events
				continue
			}
			w.emitErr(fmt.Errorf("watch: backend: %w", err))

		case <-tick:
			// Reported here rather than on the event path: the drain only
			// overflows while the loop is behind, and a flush is the first
			// moment it is caught up enough for the consumer to act on it.
			reportDrops()
			w.repairWatches()
			if batch := w.co.due(w.clock.Now()); len(batch) > 0 {
				select {
				case w.changes <- batch:
				case <-w.done:
					return
				case <-w.ctxDone:
					return
				}
			}
			rearm()
		}
	}
}

// handle folds one backend event into the pending set and keeps the watched
// directory set in step with the tree. It reports whether the pending set was
// touched, which tells the caller whether the debounce timer needs re-arming.
func (w *Watcher) handle(ev fsnotify.Event) bool {
	abs := filepath.Clean(ev.Name)
	rel, ok := w.relPath(abs)
	if !ok {
		// The root itself, or a path outside it. Neither is a change to a file
		// in the repository; the root disappearing would be reported by the
		// backend closing its channel.
		return false
	}

	o := toOp(ev.Op)
	if o == 0 {
		return false
	}

	// Directory-ness: for a path that still exists, ask the filesystem; for one
	// that has been removed or renamed away, the watched set is the only
	// remaining record. Getting this wrong would mean a deleted directory is
	// reported as a deleted file, and the consumer would remove one index row
	// instead of a subtree.
	isDir := w.dirs.watched(abs)
	if !isDir {
		if info, err := os.Lstat(abs); err == nil {
			isDir = info.IsDir()
		}
	}

	if w.dirs.ignoredPath(rel, isDir) {
		return false
	}

	now := w.clock.Now()

	switch {
	case isDir && (o == opCreate || o == opWrite):
		// A directory appeared (created, renamed into place, or moved in from
		// outside the tree). Register it and everything under it, and replay
		// what is already inside as creates — see dirSet.addTree for why that
		// replay is not optional.
		found, err := w.dirs.addTree(abs, rel)
		if err != nil {
			w.emitErr(err)
		}
		for _, e := range found {
			w.co.add(e.rel, opCreate, e.isDir, now)
		}
		// The watch just registered may already have been silently disarmed by
		// the backend; queue it for re-assertion once that race has closed.
		// See dirSet.rewatchTree.
		w.rewatch[abs] = rel
	case isDir && (o == opRemove || o == opRename):
		w.dirs.removeTree(abs)
	}

	w.co.add(rel, o, isDir, now)

	// The path is pending unless the coalescer declined to record it, which it
	// does for a chmod on an otherwise quiet path — no change, so no deadline
	// to move.
	_, recorded := w.co.pending[rel]
	return recorded
}

// repairWatches re-asserts the watch on every directory registered since the
// last flush, and replays anything found inside that is not already pending.
//
// It exists because a watch on a newly created directory can be registered
// successfully and then silently stripped of its write notifications by the
// backend itself — see dirSet.rewatchTree for the mechanism and the evidence.
// Nothing observable distinguishes a repaired watch from a broken one, so this
// runs unconditionally rather than on detection.
//
// WHY AT FLUSH TIME. The repair must land after the backend's own registration
// has finished, and a directory's creation is always pending in the coalescer,
// so a tick is guaranteed within one debounce period — long after a race
// measured in microseconds. Using the timer that already exists costs no extra
// wakeups, and batching the whole set into one pass keeps a `git checkout`
// creating a thousand directories to one repair rather than a thousand.
//
// Entries already pending are skipped, so the ordinary case adds no events and
// does not push the batch's deadline out. What survives the filter is the case
// this is for: a file written into the directory while its watch was dead,
// which neither side would otherwise ever report.
func (w *Watcher) repairWatches() {
	if len(w.rewatch) == 0 {
		return
	}
	now := w.clock.Now()
	for abs, rel := range w.rewatch {
		delete(w.rewatch, abs)
		if !w.dirs.watched(abs) {
			// Removed again before the repair ran — a directory created and
			// deleted inside one debounce window. Nothing to re-assert.
			continue
		}
		found, err := w.dirs.rewatchTree(abs, rel)
		if err != nil {
			w.emitErr(err)
		}
		for _, e := range found {
			if _, pending := w.co.pending[e.rel]; pending {
				continue
			}
			w.co.add(e.rel, opCreate, e.isDir, now)
		}
	}
}

// signalSynced tells a test that one event has been fully applied, including
// the timer re-arm. A no-op outside tests, where synced is nil.
func (w *Watcher) signalSynced() {
	if w.synced == nil {
		return
	}
	select {
	case w.synced <- struct{}{}:
	case <-w.done:
	case <-w.ctxDone:
	}
}

// relPath converts an absolute event path to a repo-relative slash path,
// rejecting the root itself and anything outside it.
func (w *Watcher) relPath(abs string) (string, bool) {
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// exists answers the coalescer's "is it there now?" question.
//
// Lstat, not Stat: a symlink that was replaced still exists as a path and the
// consumer needs to know it changed, and following it would classify a link
// pointing outside the repository by whatever it happens to target.
func (w *Watcher) exists(rel string) (bool, bool) {
	info, err := os.Lstat(filepath.Join(w.root, filepath.FromSlash(rel)))
	if err != nil {
		return false, false
	}
	return true, info.IsDir()
}

// emitErr delivers a non-fatal error without ever discarding it. It blocks only
// when the buffer is full, and gives up only on shutdown.
func (w *Watcher) emitErr(err error) {
	select {
	case w.errs <- err:
	case <-w.done:
	case <-w.ctxDone:
	}
}

// toOp reduces an fsnotify op mask to the single most informative class.
//
// Backends differ in what they combine: inotify may report Create and Write
// separately where kqueue reports one event, and Windows reports a rename as a
// Rename/Create pair. Picking the most structural bit — a removal or a rename
// says more about a path than a write to it — keeps the coalescer's view the
// same everywhere, and coalescing means the discarded bits would not have
// changed the emitted Change anyway.
func toOp(o fsnotify.Op) op {
	switch {
	case o.Has(fsnotify.Remove):
		return opRemove
	case o.Has(fsnotify.Rename):
		return opRename
	case o.Has(fsnotify.Create):
		return opCreate
	case o.Has(fsnotify.Write):
		return opWrite
	case o.Has(fsnotify.Chmod):
		return opChmod
	default:
		return 0
	}
}
