//go:build soak

package soak

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/watch"

	"github.com/fsnotify/fsnotify"
)

// soakDebounce is much shorter than watch.DefaultDebounce.
//
// 300ms is tuned for a human's perception of lag. A soak wants the opposite:
// the interesting state is the watcher's bookkeeping across thousands of
// debounce windows, so the window is made small enough that a 30-second run
// still opens and closes hundreds of them. Coalescing behaviour is unit-tested
// elsewhere with a fake clock; what is being stressed here is what happens
// after the ten-thousandth window.
const soakDebounce = 40 * time.Millisecond

// quiesceWindow is how long the watcher must go without producing a batch
// before the tree is declared settled.
//
// Generous on purpose — 25 debounce intervals. Convergence is only meaningful
// once nothing is in flight, and declaring quiescence early would produce a
// "lost event" failure for an event that was merely still travelling. A soak
// test that cries wolf gets deleted, and then it protects nothing.
const quiesceWindow = 25 * soakDebounce

// TestWatcherEndurance runs the watcher against a repository that is being
// continuously rewritten, and checks the three things that only go wrong with
// time: goroutines accumulating, heap growing without returning, and events
// going missing permanently rather than late.
//
// The consumer here is deliberately event-driven — it applies exactly the
// paths the watcher reported, rather than re-walking the tree the way
// cli/watch.go does. Re-walking is the right production choice (it is cheap
// and it self-heals), but it would also mask the failure this test exists to
// find: a dropped event would be silently repaired by the next full diff, and
// the watcher could lose half its events without a single assertion noticing.
func TestWatcherEndurance(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t, *flagFiles)

	var c counters
	w, err := watch.New(r.root, watch.Options{
		Ignore:   r.walker.IgnorePredicate(),
		Debounce: soakDebounce,
	})
	if err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	baseDirs := w.WatchedDirs()

	// lastBatch is the quiescence clock: the wall time at which the consumer
	// last had work. Read by the main goroutine, written by the consumer.
	var lastBatch atomic.Int64
	lastBatch.Store(time.Now().UnixNano())

	var (
		applyMu  sync.Mutex
		applyErr error
	)
	failApply := func(err error) {
		applyMu.Lock()
		if applyErr == nil {
			applyErr = err
		}
		applyMu.Unlock()
	}

	var wg sync.WaitGroup

	// Error drain. The watcher documents that a caller which ignores Errors
	// will eventually stall event delivery, so a soak MUST drain it — not
	// draining would turn this into a test of the error buffer's depth.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for err := range w.Errors() {
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				// The kernel's event queue overflowed, so events really were
				// lost — by the OS, not by RepoCortex. A correct consumer falls
				// back to a full reconcile, which is what cli/watch.go's
				// re-diff does implicitly. Counted and reported prominently,
				// because a run with overflows has proved less than a run
				// without.
				c.overflows.Add(1)
				if _, rerr := r.reconcile(ctx); rerr != nil {
					failApply(fmt.Errorf("reconcile after event overflow: %w", rerr))
				}
				continue
			}
			if errors.Is(err, watch.ErrWatchLimit) {
				failApply(fmt.Errorf("watch descriptor limit hit on a %d-file fixture: %w", *flagFiles, err))
				continue
			}
			t.Logf("watcher error (non-fatal): %v", err)
		}
	}()

	// Consumer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for batch := range w.Changes() {
			c.batches.Add(1)
			c.events.Add(int64(len(batch)))
			for _, ch := range batch {
				for _, fc := range toFileChanges(ch) {
					raced, err := r.applyChange(ctx, fc)
					if raced {
						c.raced.Add(1)
					}
					if err != nil {
						if isLockError(err) {
							failApply(fmt.Errorf("SQLite contention escaped to the caller while applying %s: %w", fc.Path, err))
							continue
						}
						failApply(fmt.Errorf("apply %s (%s): %w", fc.Path, fc.Kind, err))
					}
				}
			}
			lastBatch.Store(time.Now().UnixNano())
		}
	}()

	// Warm up before baselining: the watcher's goroutine, fsnotify's backend,
	// the grammar and the connection pool must all already exist, or their
	// one-time cost is charged to "growth".
	m := newMutator(r, &c, *flagSeed)
	warmDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(warmDeadline) {
		m.step()
		time.Sleep(*flagInterval)
	}
	waitQuiet(&lastBatch, quiesceWindow, 30*time.Second)

	tr := newTracker("watcher endurance")
	deadline := time.Now().Add(budget())
	stop := make(chan struct{})

	var mutWG sync.WaitGroup
	mutWG.Add(1)
	go func() {
		defer mutWG.Done()
		m.runUntil(deadline, stop)
	}()

	// Sample about a dozen times across the window. Fewer and the trend line
	// is guesswork; many more and the settling pauses start to distort the
	// workload they are measuring.
	interval := budget() / 12
	if interval < time.Second {
		interval = time.Second
	}
	for time.Now().Before(deadline) {
		time.Sleep(minDuration(interval, time.Until(deadline)))
		s := tr.sample()
		if dirs := w.WatchedDirs(); dirs > baseDirs+goroutineSlack {
			t.Errorf("watched directories climbing during a run that creates no directories: %d, started at %d (sample t=%s)",
				dirs, baseDirs, s.elapsed.Round(time.Second))
		}
	}
	close(stop)
	mutWG.Wait()

	// Convergence. Everything up to here allowed the index to lag; from the
	// moment the tree stops moving it must catch up exactly, and whatever is
	// still missing after quiesce was lost, not late.
	if !waitQuiet(&lastBatch, quiesceWindow, 60*time.Second) {
		t.Error("watcher never went quiet within 60s of the last mutation; events are being produced with no filesystem activity to explain them")
	}

	applyMu.Lock()
	firstApplyErr := applyErr
	applyMu.Unlock()
	if firstApplyErr != nil {
		t.Errorf("incremental apply failed during the run: %v", firstApplyErr)
	}

	// Two gates, because two different parties are responsible.
	//
	// The event stream is a joint effort with the kernel, so it gets a small,
	// measured budget (see lostEventBudget). The diff-and-apply path is
	// entirely RepoCortex's, so it gets none: whatever the events missed, a full
	// reconcile must repair exactly, and an index that a full reconcile cannot
	// bring into line with the tree is corrupt rather than merely stale.
	mutations := c.created.Load() + c.modified.Load() + c.deleted.Load() + c.renamed.Load()
	renames := c.renamed.Load()
	allowed := lostEventBudget(renames)

	diverged, err := r.divergence(ctx)
	if err != nil {
		t.Fatalf("divergence check after quiesce: %v", err)
	}
	if len(diverged) > allowed {
		t.Errorf("the watcher lost %d event(s) permanently over %d mutations including %d renames "+
			"(budget %d) — after quiesce these paths still differ from the filesystem:%s",
			len(diverged), mutations, renames, allowed, formatDivergence(diverged))
		if c.overflows.Load() > 0 {
			t.Logf("NOTE: %d kernel event-queue overflow(s) were reported. Events the OS itself "+
				"dropped are outside RepoCortex's control; re-run with a larger -soak.interval to "+
				"separate that cause from a watcher bug.", c.overflows.Load())
		}
	}
	if len(diverged) > 0 {
		if _, err := r.reconcile(ctx); err != nil {
			t.Errorf("reconcile after quiesce failed: %v", err)
		}
		after, err := r.divergence(ctx)
		if err != nil {
			t.Fatalf("divergence check after reconcile: %v", err)
		}
		if len(after) > 0 {
			t.Errorf("a full reconcile did not converge — the index cannot be brought into line "+
				"with the tree, which is corruption, not lag: %d path(s)%s",
				len(after), formatDivergence(after))
		}
	}

	for _, p := range r.integrityProblems(ctx) {
		t.Errorf("referential integrity broken after sustained incremental updates: %s", p)
	}

	if err := w.Close(); err != nil {
		t.Errorf("close watcher: %v", err)
	}
	wg.Wait()

	tr.notef("mutations    %d created, %d modified, %d deleted, %d renamed",
		c.created.Load(), c.modified.Load(), c.deleted.Load(), c.renamed.Load())
	tr.notef("watcher      %d batches carrying %d change(s), %d dirs watched (started at %d)",
		c.batches.Load(), c.events.Load(), w.WatchedDirs(), baseDirs)
	tr.notef("event loss   %d path(s) still stale after quiesce, budget %d over %d renames",
		len(diverged), allowed, renames)
	tr.notef("tolerated    %d apply(s) raced a delete, %d kernel event-queue overflow(s)",
		c.raced.Load(), c.overflows.Load())
	tr.sample()
	tr.finish(t)
}

// lostEventBudget is how many paths may still be stale after quiesce before the
// watcher is called broken. It is expressed against the RENAME count, because
// renames are the only operation observed to lose events.
//
// Zero is the number RepoCortex would deserve if it owned the whole stack, and on
// Linux — where inotify pairs every rename with a MOVED_FROM/MOVED_TO cookie —
// it is close to what you get. macOS is structurally different: kqueue has no
// rename cookie and fsnotify tracks each file through its own descriptor, so a
// file renamed twice in quick succession can have its second rename land on a
// descriptor that has already been retired. Nothing reports that this happened
// — kqueue has no equivalent of inotify's queue-overflow signal — so the loss
// is silent by construction and no amount of care in this package can recover
// it.
//
// Measured on macOS during development, with the mutation stream at the default
// 3ms interval and roughly a third of operations being renames:
//
//	plain run  (~1,000 renames per 15s):  0 to 1 stale paths
//	-race run  (~1,000 renames per 15s):  up to 5 stale paths
//
// -race is worse for a structural reason worth knowing: the consumer runs about
// an order of magnitude slower, the unbuffered Changes channel applies
// backpressure, and the resulting long debounce windows put many more renames
// in flight at once. So the budget is calibrated against the -race figure, at
// 1% of renames with a floor of 5 — roughly twice the worst measurement.
//
// Insisting on zero instead would fail this test about one run in three on a
// Mac, for a reason nobody can fix in this repository. A soak test that cries
// wolf gets deleted, and a deleted test detects nothing. This one still detects
// what matters, because a watcher that has genuinely stopped delivering does
// not lose 1% of renames, it loses all of them — and the observed number is
// printed on every run, pass or fail, so a slow drift upward is visible long
// before it trips the gate.
func lostEventBudget(renames int64) int {
	const floor = 5
	b := int(renames / 100)
	if b < floor {
		return floor
	}
	return b
}

// TestWatcherDirectoryChurn repeatedly creates and destroys directory trees.
//
// Separate from the endurance test on purpose. Watch descriptors are a finite
// OS resource — one inotify watch per directory on Linux, one file descriptor
// per directory on macOS — and the classic watcher bug is a per-directory
// watch that is registered on create and never released on delete. That leak is
// invisible in a run that only edits files, and mixing directory deletion into
// the convergence test would make a failure ambiguous between a lost file
// event and a lost directory event.
//
// The assertion is the watched-directory count itself rather than an OS-level
// descriptor count, because dirSet is the authority on what RepoCortex believes
// is watched: fsnotify silently drops watches for deleted directories and
// silently keeps them across renames, so the kernel's number and RepoCortex's
// number diverging IS the bug.
func TestWatcherDirectoryChurn(t *testing.T) {
	r := newRepo(t, minInt(*flagFiles, 64))

	w, err := watch.New(r.root, watch.Options{
		Ignore:   r.walker.IgnorePredicate(),
		Debounce: soakDebounce,
	})
	if err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	var lastBatch atomic.Int64
	lastBatch.Store(time.Now().UnixNano())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range w.Changes() {
			lastBatch.Store(time.Now().UnixNano())
		}
	}()
	go func() {
		defer wg.Done()
		for err := range w.Errors() {
			if errors.Is(err, watch.ErrWatchLimit) {
				// Exactly the symptom a descriptor leak produces, so it is
				// reported as one rather than logged as noise.
				t.Errorf("watch limit reached during directory churn — descriptors are not being released: %v", err)
				continue
			}
			t.Logf("watcher error (non-fatal): %v", err)
		}
	}()

	// Warm up, then take the baseline the count must return to.
	churnRound(t, r, 0, 8)
	waitQuiet(&lastBatch, quiesceWindow, 30*time.Second)
	baseDirs := w.WatchedDirs()
	tr := newTracker("watcher directory churn")

	var (
		rounds     int
		dirsMade   int
		dirSeries  []int
		deadline   = time.Now().Add(budget())
		sampleEach = maxDuration(budget()/12, time.Second)
		nextSample = time.Now().Add(sampleEach)
	)
	for time.Now().Before(deadline) {
		rounds++
		dirsMade += churnRound(t, r, rounds, 8)

		// Quiesce every round, because the only defensible question is whether
		// the count comes back down — mid-round it is legitimately high.
		waitQuiet(&lastBatch, quiesceWindow, 20*time.Second)
		got := w.WatchedDirs()
		dirSeries = append(dirSeries, got)
		if got > baseDirs+watchDirSlack {
			t.Errorf("watch descriptors leaking: %d directories watched after round %d, baseline %d "+
				"(%d directories created and destroyed so far; per-round series %v)",
				got, rounds, baseDirs, dirsMade, dirSeries)
			break
		}
		if time.Now().After(nextSample) {
			tr.sample()
			nextSample = time.Now().Add(sampleEach)
		}
	}

	if err := w.Close(); err != nil {
		t.Errorf("close watcher: %v", err)
	}
	wg.Wait()

	tr.notef("churn        %d rounds, %d directories created and destroyed", rounds, dirsMade)
	tr.notef("watched dirs baseline %d, per-round after quiesce %v", baseDirs, dirSeries)
	tr.sample()
	tr.finish(t)
}

// watchDirSlack tolerates a handful of directories still being registered when
// a round is measured.
//
// Not zero, because a delete event the kernel dropped leaves a watch that
// RepoCortex has no way to learn about, and one such event over a long run must
// not fail the build. Small, because the leak being hunted is one descriptor
// per churned directory: over even a hundred rounds it is in the hundreds, and
// nothing lands between 4 and that.
const watchDirSlack = 4

// churnRound creates a small directory tree, fills it, and destroys it. The
// created files matter: a directory that is watched but empty exercises none
// of the replay path in dirSet.addTree.
func churnRound(t *testing.T, r *repo, round, dirs int) int {
	t.Helper()
	base := fmt.Sprintf("churn%04d", round)
	for i := 0; i < dirs; i++ {
		dir := fmt.Sprintf("%s/d%02d/nested", base, i)
		rel := fmt.Sprintf("%s/f.go", dir)
		r.writeFile(rel, fixtureSource("nested", round*1000+i, 1))
	}
	// A brief pause so the creates are actually delivered and watched before
	// the tree is removed. Without it the watcher may never see the directory
	// at all, and a leak check that never registers a watch proves nothing.
	time.Sleep(4 * soakDebounce)
	if err := os.RemoveAll(filepath.Join(r.root, base)); err != nil {
		t.Fatalf("remove churn tree %s: %v", base, err)
	}
	return dirs * 2 // each round makes dNN and dNN/nested
}

// toFileChanges maps one watcher change onto the pipeline's change vocabulary.
//
// Directories are dropped: the index has no row for a directory, and the
// watcher already replays the contents of a newly created directory as
// individual create events (see dirSet.addTree), so nothing is lost.
//
// A rename becomes a delete plus an add rather than a move, matching what
// pipeline.Diff infers from content hashes. Keeping the two paths in agreement
// is what lets the final divergence check compare like with like.
func toFileChanges(ch watch.Change) []pipeline.FileChange {
	if ch.IsDir {
		return nil
	}
	switch ch.Kind {
	case watch.ChangeCreated:
		return []pipeline.FileChange{{Path: ch.Path, Kind: pipeline.ChangeAdded}}
	case watch.ChangeModified:
		return []pipeline.FileChange{{Path: ch.Path, Kind: pipeline.ChangeModified}}
	case watch.ChangeDeleted:
		return []pipeline.FileChange{{Path: ch.Path, Kind: pipeline.ChangeDeleted}}
	case watch.ChangeRenamed:
		return []pipeline.FileChange{
			{Path: ch.OldPath, Kind: pipeline.ChangeDeleted},
			{Path: ch.Path, Kind: pipeline.ChangeAdded},
		}
	default:
		return nil
	}
}

// waitQuiet blocks until no batch has arrived for window, or until timeout. It
// reports whether quiescence was reached.
func waitQuiet(last *atomic.Int64, window, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if time.Since(time.Unix(0, last.Load())) >= window {
			return true
		}
		time.Sleep(window / 5)
	}
	return false
}

// formatDivergence renders a bounded, readable list of the paths where the
// index and the tree disagree. Bounded because a systemic failure diverges on
// everything, and ten thousand lines of test output is not a diagnosis.
func formatDivergence(diverged []pipeline.FileChange) string {
	const show = 20
	out := "\n"
	for i, d := range diverged {
		if i == show {
			out += fmt.Sprintf("  ... and %d more\n", len(diverged)-show)
			break
		}
		out += fmt.Sprintf("  %-8s %s\n", d.Kind, d.Path)
	}
	return out
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
