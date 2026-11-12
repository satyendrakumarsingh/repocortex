package watch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

// ErrWatchLimit is reported when the tree needs more watched directories than
// Options.MaxWatchDirs allows.
//
// Watch descriptors are a finite OS resource — on Linux one inotify watch per
// directory against a per-user max_user_watches, on macOS/BSD one file
// descriptor per directory (and, in kqueue, per file inside it) against
// RLIMIT_NOFILE. Running out is not a condition to shrug at: the watcher would
// keep delivering events for the directories it did manage to register and
// silently deliver nothing for the rest, and a consumer cannot tell "nothing
// changed" from "nothing was watched". So the limit is ours, it is hit before
// the kernel's, and crossing it is always reported.
var ErrWatchLimit = errors.New("watch: watched directory limit exceeded")

// backend is the fsnotify surface this package uses.
//
// It exists so tests can drive the recursive-watch bookkeeping and the
// debouncer with hand-written events. Testing that logic against a real
// fsnotify watcher would mean racing the kernel: you write a file, then hope
// the event arrives before the assertion, which is precisely the kind of test
// that passes locally and fails in CI. fsnotify.Event is a plain struct, so
// depending on it here costs nothing.
type backend interface {
	Add(name string) error
	Remove(name string) error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

// fsnotifyBackend adapts *fsnotify.Watcher, whose Events and Errors are struct
// fields rather than methods.
type fsnotifyBackend struct{ w *fsnotify.Watcher }

func (b fsnotifyBackend) Add(name string) error         { return b.w.Add(name) }
func (b fsnotifyBackend) Remove(name string) error      { return b.w.Remove(name) }
func (b fsnotifyBackend) Events() <-chan fsnotify.Event { return b.w.Events }
func (b fsnotifyBackend) Errors() <-chan error          { return b.w.Errors }
func (b fsnotifyBackend) Close() error                  { return b.w.Close() }

// builtinIgnoredDirs are never watched, whatever Options.Ignore says.
//
// This is not a convenience default that a caller may override. A watch on
// .git turns every `git status` into thousands of events for index.lock and
// object files, none of which can change the source tree; node_modules is
// hundreds of thousands of directories, more than the OS will grant watches
// for; .repocortex is RepoCortex's own store, so watching it would let the
// consumer's own writes feed back in as changes to re-index — an infinite
// loop with a filesystem in the middle.
var builtinIgnoredDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	".repocortex":  {},
}

// entry is one path discovered while registering watches.
type entry struct {
	rel   string
	isDir bool
}

// dirSet owns the mapping from directories to watches. Only the watcher's
// event loop mutates it (before the loop starts, New has it to itself), so
// nothing here takes a lock. count is the single exception, and is backed by an
// atomic precisely so that it can be read from elsewhere.
type dirSet struct {
	backend backend
	root    string
	ignore  func(rel string, isDir bool) bool
	max     int

	// dirs holds every directory currently watched, keyed by absolute path.
	// fsnotify drops watches for deleted directories on its own, but it will
	// not tell us that it did, and a rename moves a watch to follow the inode
	// rather than dropping it. Neither is observable through fsnotify's API, so
	// this set — not the kernel's — is the authority on what we believe is
	// watched, and it is what lets an event for a vanished path be classified
	// as a directory when the filesystem can no longer answer that.
	dirs map[string]struct{}

	// n mirrors len(dirs) so the count can be read from another goroutine (a
	// status line, `repocortex doctor`) without a lock and without the race
	// detector rightly objecting to a concurrent map read.
	n atomic.Int64
}

func newDirSet(b backend, root string, ignore func(string, bool) bool, max int) *dirSet {
	return &dirSet{backend: b, root: root, ignore: ignore, max: max, dirs: make(map[string]struct{})}
}

// ignored reports whether a path is out of scope. The built-in directory list
// is checked first and cannot be turned off by the caller's predicate.
func (d *dirSet) ignored(rel string, isDir bool) bool {
	if isDir {
		if _, deny := builtinIgnoredDirs[filepath.Base(rel)]; deny {
			return true
		}
	}
	if d.ignore == nil {
		return false
	}
	return d.ignore(rel, isDir)
}

// ignoredPath is ignored() applied to a path and to every directory above it.
//
// The ancestor scan is not redundant with the pruning done while adding
// watches. A watch is only ever placed on a non-ignored directory, but events
// arrive for paths, and a backend is free to report one for a path we never
// watched — kqueue reports on the watched parent, and inotify can deliver
// queued events for a directory that became ignored between the two. Deciding
// on the leaf alone would let `.git/index` through, since the built-in list
// names directories and `index` is a file.
func (d *dirSet) ignoredPath(rel string, isDir bool) bool {
	for i, c := range rel {
		if c == '/' {
			if d.ignored(rel[:i], true) {
				return true
			}
		}
	}
	return d.ignored(rel, isDir)
}

func (d *dirSet) watched(abs string) bool {
	_, ok := d.dirs[abs]
	return ok
}

func (d *dirSet) count() int { return int(d.n.Load()) }

// addTree registers a watch on abs and on every non-ignored directory beneath
// it, and returns everything it found on the way (excluding abs itself).
//
// The returned entries are the fix for a race that is inherent to every
// non-recursive watcher: between the mkdir that produced the event we are
// reacting to and the moment our watch on the new directory is live, files can
// be created inside it, and those creations are simply never reported. `git
// checkout` of a branch that adds a directory does exactly this, at speed. So
// the caller replays the entries as synthetic create events. Doing this costs
// one ReadDir per new directory and duplicates a handful of events that
// coalescing then folds away — much cheaper than the alternative, which is an
// index that is missing files until something else happens to touch them.
//
// On failure the watches already registered are kept rather than rolled back:
// watching part of the tree and saying so beats watching none of it.
func (d *dirSet) addTree(abs, rel string) ([]entry, error) {
	var found []entry
	err := d.walk(abs, rel, &found, false)
	return found, err
}

// rewatchTree is addTree except that it re-registers directories it already
// believes are watched, instead of assuming a registration made earlier is
// still in force.
//
// WHY THIS IS NECESSARY — a real bug, reproduced deterministically.
//
// fsnotify's kqueue backend (macOS/BSD, v1.10.1) races with its own callers.
// When a directory appears inside a watched tree, fsnotify's reader goroutine
// runs sendCreateIfNew: it emits the Create event, and only THEN registers its
// own watch on the new directory, via internalWatch:
//
//	info, _ := w.watches.byPath(name)                              // (1) read
//	return w.addWatch(name, info.dirFlags|NOTE_DELETE|NOTE_RENAME)  // (2) write
//
// Our watcher reacts to that Create by calling Add on the new directory, which
// is exactly what recursive watching requires. If our Add lands between (1) and
// (2), fsnotify has already sampled dirFlags as zero, so it re-registers the
// descriptor with NOTE_DELETE|NOTE_RENAME only — and kqueue's EV_ADD REPLACES
// the filter flags rather than merging them. NOTE_WRITE is gone.
//
// The result is the worst possible failure mode: the directory is registered,
// Add returned nil, WatchList reports it, our own bookkeeping says it is
// watched — and no write inside it is ever reported again. Silence is
// indistinguishable from "nothing changed", which is the exact confusion
// ErrWatchLimit exists to prevent elsewhere in this file.
//
// The window is microseconds wide and is entered from the reader goroutine
// resuming after we receive its event, so whether we lose it is decided by the
// scheduler. It reproduced 100% of the time when any other test had already
// run, and 0% of the time in isolation.
//
// There is no upstream fix — v1.10.1 is current — and no API that reports a
// descriptor's live filter flags, so this cannot be detected, only repaired.
// Re-registering after the window has closed restores NOTE_WRITE, because our
// flags are then written last. See Watcher.repairWatches for when it runs.
//
// The rescan is not incidental. fsnotify's re-registration also marks the
// directory's current entries as "seen", which suppresses the Create events it
// would otherwise emit for them, so anything written during the dead window is
// lost from its side too. Returning the entries lets the caller replay them —
// the same mechanism, and the same reason, as addTree's replay above.
func (d *dirSet) rewatchTree(abs, rel string) ([]entry, error) {
	var found []entry
	err := d.walk(abs, rel, &found, true)
	return found, err
}

// walk registers abs and descends. force re-registers directories already in
// the set; see rewatchTree.
func (d *dirSet) walk(abs, rel string, found *[]entry, force bool) error {
	known := d.watched(abs)
	if known && force {
		// Deliberately not counted or re-inserted: this is the same watch,
		// re-asserted. An error here means the directory went away, which is
		// ordinary churn and is handled by the not-watched path below on the
		// next event.
		if err := d.backend.Add(abs); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("re-watch %q: %w", displayPath(rel), err)
		}
	}
	if !known {
		if d.count() >= d.max {
			return fmt.Errorf("%w: %d directories, at %q", ErrWatchLimit, d.max, displayPath(rel))
		}
		if err := d.backend.Add(abs); err != nil {
			// A directory that vanished between discovery and Add is normal
			// churn during a checkout, not a failure worth surfacing.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("watch %q: %w", displayPath(rel), err)
		}
		d.dirs[abs] = struct{}{}
		d.n.Add(1)
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		// Unreadable subtree: the watch above is still registered and will
		// report changes if permissions change. Nothing more to do here.
		return nil //nolint:nilerr // deliberate: one unreadable directory must not abort setup
	}
	// Sorted so that a limit, when it is hit, is hit at the same place on every
	// run and every platform. A nondeterministic subset of a tree being watched
	// would be miserable to debug.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		childRel := e.Name()
		if rel != "" {
			childRel = rel + "/" + e.Name()
		}
		isDir := e.IsDir()
		if d.ignored(childRel, isDir) {
			continue
		}
		*found = append(*found, entry{rel: childRel, isDir: isDir})
		if !isDir {
			continue
		}
		// Symlinked directories are not descended into. The acquisition walker
		// does not index through symlinks (they are a path-traversal vector and
		// a cycle source), so watching through them would produce events for
		// paths that can never appear in the index.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := d.walk(filepath.Join(abs, e.Name()), childRel, found, force); err != nil {
			return err
		}
	}
	return nil
}

// removeTree drops abs and every directory beneath it.
//
// Doing this explicitly, rather than trusting fsnotify to clean up after a
// deleted directory, is what keeps the set honest in the rename case: when a
// directory is renamed, inotify keeps the watch alive on the moved inode and
// keeps reporting events under the OLD name. Those events would be relativised
// against a path that no longer exists and attributed to files that were never
// touched. Removing the watch and re-adding under the new name (via the create
// event for the destination) is the only correct handling.
func (d *dirSet) removeTree(abs string) {
	prefix := abs + string(filepath.Separator)
	for dir := range d.dirs {
		if dir != abs && !strings.HasPrefix(dir, prefix) {
			continue
		}
		delete(d.dirs, dir)
		d.n.Add(-1)
		// Errors are expected and uninteresting: fsnotify returns
		// ErrNonExistentWatch for a watch the kernel already dropped when the
		// directory was deleted, which is the common case here.
		_ = d.backend.Remove(dir)
	}
}

// displayPath renders a relative path for error messages, naming the root
// explicitly instead of printing an empty string.
func displayPath(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}
