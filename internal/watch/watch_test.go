package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// failAfter bounds every blocking operation in these tests. It is not a timing
// assumption: correct behaviour always completes immediately here because the
// clock is manual, so hitting this bound means the code deadlocked.
const failAfter = 10 * time.Second

// fakeBackend is a hand-driven fsnotify. Tests decide exactly which events the
// watcher sees and in which order, which is the only way to assert on watch
// bookkeeping and debouncing without racing the kernel.
type fakeBackend struct {
	events chan fsnotify.Event
	errs   chan error

	mu      sync.Mutex
	added   []string
	removed []string
	closed  bool
	addErr  error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		// Unbuffered on purpose: a send completing means the event loop has
		// received it, which is what makes harness.settle a real barrier.
		events: make(chan fsnotify.Event),
		errs:   make(chan error),
	}
}

func (f *fakeBackend) Add(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, name)
	return f.addErr
}

func (f *fakeBackend) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeBackend) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeBackend) Errors() <-chan error          { return f.errs }

func (f *fakeBackend) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.events)
	close(f.errs)
	return nil
}

func (f *fakeBackend) addedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.added...)
}

func (f *fakeBackend) removedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func (f *fakeBackend) addCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.added {
		if a == name {
			n++
		}
	}
	return n
}

type harness struct {
	t       *testing.T
	root    string
	clock   *ManualClock
	backend *fakeBackend
	w       *Watcher

	// synced receives once per event the loop has finished applying. See
	// Options.synced: the watcher buffers backend events so that slow work in
	// handle cannot stall fsnotify, so a completed send no longer implies the
	// event was processed. This makes the barrier explicit instead.
	synced chan struct{}

	debounce time.Duration
}

// newHarness builds a watcher over a real temporary tree but with a fake
// backend and a manual clock: the filesystem is real (the watcher lstats it),
// while event delivery and time are entirely under the test's control.
func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	return newHarnessAt(t, tempRoot(t), opts)
}

// tempRoot returns a temp directory with symlinks resolved, matching what the
// watcher stores as its root. On macOS t.TempDir() sits under /var, which is a
// symlink to /private/var, so unresolved paths would not be relative to it.
func tempRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

func (h *harness) abs(rel string) string {
	return filepath.Join(h.root, filepath.FromSlash(rel))
}

func (h *harness) write(rel, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.abs(rel)), 0o755); err != nil {
		h.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(h.abs(rel), []byte(content), 0o600); err != nil {
		h.t.Fatalf("write %s: %v", rel, err)
	}
}

func (h *harness) mkdir(rel string) {
	h.t.Helper()
	if err := os.MkdirAll(h.abs(rel), 0o755); err != nil {
		h.t.Fatalf("mkdir %s: %v", rel, err)
	}
}

func (h *harness) remove(rel string) {
	h.t.Helper()
	if err := os.RemoveAll(h.abs(rel)); err != nil {
		h.t.Fatalf("remove %s: %v", rel, err)
	}
}

// send delivers one backend event.
func (h *harness) send(rel string, op fsnotify.Op) {
	h.t.Helper()
	h.sendAbs(h.abs(rel), op)
}

// sendAbs delivers one event and returns only once the loop has fully applied
// it, including the debounce timer re-arm.
//
// The wait is what makes every assertion below deterministic. It used to be
// implicit — the fake backend's channel was unbuffered, so a completed send
// meant the loop had come back round to its select. That stopped being true
// when the watcher grew a buffered drain between the backend and the loop,
// which it needs so that slow work in handle cannot stall fsnotify and lose
// kernel events. The barrier is now stated outright rather than inferred from
// channel capacity.
func (h *harness) sendAbs(abs string, op fsnotify.Op) {
	h.t.Helper()
	select {
	case h.backend.events <- fsnotify.Event{Name: abs, Op: op}:
	case <-time.After(failAfter):
		h.t.Fatalf("event loop did not accept an event for %s", abs)
	}
	select {
	case <-h.synced:
	case <-time.After(failAfter):
		h.t.Fatalf("event loop did not finish handling the event for %s", abs)
	}
}

// settle is retained for readability at call sites that want to say "everything
// sent so far is applied". Each send is already a barrier, so this is a
// belt-and-braces no-op event on a path outside the root, which the watcher
// discards.
func (h *harness) settle() {
	h.t.Helper()
	h.sendAbs(filepath.Join(filepath.Dir(h.root), "__barrier__"), fsnotify.Write)
}

// advance moves the manual clock, firing any debounce deadline it passes.
func (h *harness) advance(d time.Duration) {
	h.clock.Advance(d)
}

func (h *harness) expectBatch() []Change {
	h.t.Helper()
	select {
	case batch, ok := <-h.w.Changes():
		if !ok {
			h.t.Fatal("Changes channel closed while waiting for a batch")
		}
		return batch
	case <-time.After(failAfter):
		h.t.Fatal("no batch emitted")
		return nil
	}
}

// expectNoBatch asserts nothing is pending delivery. Sound because Changes is
// unbuffered: a value can only be waiting if the loop already decided to emit
// it, and the manual clock means that decision is not a matter of timing.
func (h *harness) expectNoBatch() {
	h.t.Helper()
	select {
	case batch := <-h.w.Changes():
		h.t.Fatalf("unexpected batch %+v", batch)
	default:
	}
}

func (h *harness) expectError() error {
	h.t.Helper()
	select {
	case err, ok := <-h.w.Errors():
		if !ok {
			h.t.Fatal("Errors channel closed while waiting for an error")
		}
		return err
	case <-time.After(failAfter):
		h.t.Fatal("no error reported")
		return nil
	}
}

func TestSingleFileModify(t *testing.T) {
	h := newHarness(t, Options{})
	h.write("a.go", "package a")

	h.send("a.go", fsnotify.Write)
	h.settle()
	h.advance(h.debounce)

	assertChanges(t, h.expectBatch(), []Change{{Path: "a.go", Kind: ChangeModified}})
}

// TestBurstCoalescesToOneChange is the headline behaviour: an editor save
// storm must cost the consumer exactly one re-index.
func TestBurstCoalescesToOneChange(t *testing.T) {
	h := newHarness(t, Options{})
	h.write("a.go", "package a")

	for range 10 {
		h.send("a.go", fsnotify.Write)
	}
	h.send("a.go", fsnotify.Chmod) // editors chmod after writing; still one change
	h.settle()
	h.advance(h.debounce)

	assertChanges(t, h.expectBatch(), []Change{{Path: "a.go", Kind: ChangeModified}})
	h.expectNoBatch()
}

// TestDebounceWindowExtends verifies the timing rule itself — quiet period
// measured from the LAST event, not the first — entirely on the fake clock.
func TestDebounceWindowExtends(t *testing.T) {
	h := newHarness(t, Options{})
	h.write("a.go", "package a")

	h.send("a.go", fsnotify.Write)
	h.settle()

	h.advance(200 * time.Millisecond)
	h.expectNoBatch()

	// A second event 200ms in must push the deadline out by a full window.
	h.send("a.go", fsnotify.Write)
	h.settle()

	h.advance(200 * time.Millisecond) // 400ms since the first event
	h.expectNoBatch()

	h.advance(100 * time.Millisecond) // 300ms since the last one
	assertChanges(t, h.expectBatch(), []Change{{Path: "a.go", Kind: ChangeModified}})
}

// TestCreateThenDeleteCancels pairs the cancelled path with a sentinel that
// does change, so the assertion is positive: the batch arrives, and the
// cancelled path is not in it.
func TestCreateThenDeleteCancels(t *testing.T) {
	h := newHarness(t, Options{})
	h.write("sentinel.go", "package s")

	h.write("tmp.go", "scratch")
	h.send("tmp.go", fsnotify.Create)
	h.send("tmp.go", fsnotify.Write)
	h.remove("tmp.go")
	h.send("tmp.go", fsnotify.Remove)
	h.send("sentinel.go", fsnotify.Write)
	h.settle()
	h.advance(h.debounce)

	assertChanges(t, h.expectBatch(), []Change{{Path: "sentinel.go", Kind: ChangeModified}})
}

// TestNewDirectoryIsWatched also covers the race the recursive watcher has to
// close: pkg/deep/x.go exists before the watch on pkg is registered, so it
// would never produce an event of its own.
func TestNewDirectoryIsWatched(t *testing.T) {
	h := newHarness(t, Options{})
	h.write("pkg/deep/x.go", "package deep")

	h.send("pkg", fsnotify.Create)
	h.settle()

	added := h.backend.addedPaths()
	for _, want := range []string{h.abs("pkg"), h.abs("pkg/deep")} {
		if !contains(added, want) {
			t.Errorf("no watch added for %s; added=%v", want, added)
		}
	}
	if got := h.w.WatchedDirs(); got != 3 { // root, pkg, pkg/deep
		t.Errorf("WatchedDirs() = %d, want 3", got)
	}

	h.advance(h.debounce)
	assertChanges(t, h.expectBatch(), []Change{
		{Path: "pkg", Kind: ChangeCreated, IsDir: true},
		{Path: "pkg/deep", Kind: ChangeCreated, IsDir: true},
		{Path: "pkg/deep/x.go", Kind: ChangeCreated},
	})
}

func TestDeletedDirectoryStopsBeingWatched(t *testing.T) {
	// pkg exists before the watcher starts, so the initial pass registers it.
	root := tempRoot(t)
	if err := os.Mkdir(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := newHarnessAt(t, root, Options{})
	if !contains(h.backend.addedPaths(), h.abs("pkg")) {
		t.Fatalf("pkg was not watched at startup: %v", h.backend.addedPaths())
	}

	h.remove("pkg")
	h.send("pkg", fsnotify.Remove)
	h.settle()

	if !contains(h.backend.removedPaths(), h.abs("pkg")) {
		t.Errorf("watch on pkg was not removed: %v", h.backend.removedPaths())
	}
	h.advance(h.debounce)
	assertChanges(t, h.expectBatch(), []Change{{Path: "pkg", Kind: ChangeDeleted, IsDir: true}})

	// Recreating it must add a watch again — proof that the bookkeeping really
	// dropped the entry rather than just calling Remove on the backend.
	h.mkdir("pkg")
	h.send("pkg", fsnotify.Create)
	h.settle()
	if got := h.backend.addCount(h.abs("pkg")); got != 2 {
		t.Errorf("watch on pkg added %d times, want 2", got)
	}
	h.advance(h.debounce)
	assertChanges(t, h.expectBatch(), []Change{{Path: "pkg", Kind: ChangeCreated, IsDir: true}})
}

func TestIgnoredDirectoriesAreNeverWatched(t *testing.T) {
	root := tempRoot(t)
	for _, dir := range []string{".git/objects", "node_modules/pkg", ".repocortex/store", "vendor/lib", "src"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	h := newHarnessAt(t, root, Options{
		Ignore: func(rel string, _ bool) bool {
			return rel == "vendor" || strings.HasPrefix(rel, "vendor/")
		},
	})

	added := h.backend.addedPaths()
	want := []string{root, filepath.Join(root, "src")}
	if len(added) != len(want) {
		t.Fatalf("watched %v, want exactly %v", added, want)
	}
	for i := range want {
		if added[i] != want[i] {
			t.Errorf("watch %d = %s, want %s", i, added[i], want[i])
		}
	}

	// Events from inside an ignored tree are dropped even if one reaches us.
	h.write("src/a.go", "package a")
	h.send(".git/index", fsnotify.Write)
	h.send("node_modules/pkg/index.js", fsnotify.Create)
	h.send("vendor/lib/dep.go", fsnotify.Write)
	h.send("src/a.go", fsnotify.Write)
	h.settle()
	h.advance(h.debounce)

	assertChanges(t, h.expectBatch(), []Change{{Path: "src/a.go", Kind: ChangeModified}})
}

func TestRenameIsDetected(t *testing.T) {
	h := newHarness(t, Options{})
	h.write("new.go", "package n") // old.go has already been renamed away

	h.send("old.go", fsnotify.Rename)
	h.send("new.go", fsnotify.Create)
	h.settle()
	h.advance(h.debounce)

	assertChanges(t, h.expectBatch(), []Change{
		{Path: "new.go", OldPath: "old.go", Kind: ChangeRenamed},
	})
}

// TestRenameAcrossWindowsDegrades covers the case the heuristic cannot see:
// when the two halves land in different debounce windows there is nothing to
// pair, and delete + create is the honest answer.
func TestRenameAcrossWindowsDegrades(t *testing.T) {
	h := newHarness(t, Options{})

	h.send("old.go", fsnotify.Rename)
	h.settle()
	h.advance(h.debounce)
	assertChanges(t, h.expectBatch(), []Change{{Path: "old.go", Kind: ChangeDeleted}})

	h.write("new.go", "package n")
	h.send("new.go", fsnotify.Create)
	h.settle()
	h.advance(h.debounce)
	assertChanges(t, h.expectBatch(), []Change{{Path: "new.go", Kind: ChangeCreated}})
}

func TestWatchLimitFailsStartup(t *testing.T) {
	root := tempRoot(t)
	for _, dir := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	b := newFakeBackend()
	_, err := New(root, Options{MaxWatchDirs: 2, backend: b, Clock: NewManualClock(epoch)})
	if !errors.Is(err, ErrWatchLimit) {
		t.Fatalf("New error = %v, want ErrWatchLimit", err)
	}
	if !strings.Contains(err.Error(), "2 directories") {
		t.Errorf("error does not report the limit: %v", err)
	}
}

// TestWatchLimitReportedAtRuntime is the more dangerous case: the limit is hit
// while running, and the subtree simply cannot be watched. That must reach the
// caller, because from here on changes under it are invisible.
func TestWatchLimitReportedAtRuntime(t *testing.T) {
	h := newHarness(t, Options{MaxWatchDirs: 1}) // room for the root only
	h.mkdir("pkg")

	h.send("pkg", fsnotify.Create)
	h.settle()

	err := h.expectError()
	if !errors.Is(err, ErrWatchLimit) {
		t.Fatalf("error = %v, want ErrWatchLimit", err)
	}
	if !strings.Contains(err.Error(), "pkg") {
		t.Errorf("error does not name the directory: %v", err)
	}

	// The directory itself is still reported: we saw it appear, we just cannot
	// watch inside it.
	h.advance(h.debounce)
	assertChanges(t, h.expectBatch(), []Change{{Path: "pkg", Kind: ChangeCreated, IsDir: true}})
}

func TestCloseIsIdempotent(t *testing.T) {
	h := newHarness(t, Options{})

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = h.w.Close()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Close %d: %v", i, err)
		}
	}
	if err := h.w.Close(); err != nil {
		t.Errorf("fourth Close: %v", err)
	}

	// Both channels must be closed, so a consumer ranging over them finishes.
	if _, ok := <-h.w.Changes(); ok {
		t.Error("Changes channel still open after Close")
	}
	if _, ok := <-h.w.Errors(); ok {
		t.Error("Errors channel still open after Close")
	}
}

// TestCloseUnblocksPendingBatch closes while the loop is parked on a send to a
// consumer that has walked away. Without the shutdown case in that select this
// deadlocks forever.
func TestCloseUnblocksPendingBatch(t *testing.T) {
	h := newHarness(t, Options{})
	h.write("a.go", "package a")
	h.send("a.go", fsnotify.Write)
	h.settle()
	h.advance(h.debounce) // loop is now blocked sending the batch

	done := make(chan error, 1)
	go func() { done <- h.w.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(failAfter):
		t.Fatal("Close blocked behind an undelivered batch")
	}
}

func TestContextCancellationShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := tempRoot(t)
	b := newFakeBackend()
	w, err := New(root, Options{Context: ctx, backend: b, Clock: NewManualClock(epoch)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancel()

	select {
	case _, ok := <-w.Changes():
		if ok {
			t.Error("got a batch after cancellation")
		}
	case <-time.After(failAfter):
		t.Fatal("event loop did not exit on context cancellation")
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close after cancellation: %v", err)
	}
}

// TestNoGoroutineLeak runs against a REAL fsnotify backend, because the
// goroutine most likely to be left behind is fsnotify's own.
func TestNoGoroutineLeak(t *testing.T) {
	root := tempRoot(t)
	// Warm up first: the first fsnotify watcher on a process may start
	// long-lived runtime machinery that must not be counted as a leak.
	warm, err := New(root, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	settleGoroutines(t, runtime.NumGoroutine())

	before := runtime.NumGoroutine()
	for range 5 {
		w, err := New(root, Options{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Idempotent close must not leave anything behind either.
		if err := w.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	}
	settleGoroutines(t, before)

	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after; watcher goroutines leaked", before, after)
	}
}

// settleGoroutines waits for the scheduler to finish tearing goroutines down.
// Close guarantees the watcher's own loop has returned, but a goroutine that
// has returned may still be counted for a moment.
func settleGoroutines(t *testing.T, target int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= target {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRealBackendEndToEnd is the one test that talks to the kernel. Everything
// above proves the logic; this proves the wiring — that fsnotify's events reach
// the loop, that their absolute paths relativise against the resolved root, and
// that a watch added for a subdirectory created after startup actually fires.
//
// It is deliberately the only timing-dependent test, it asserts on the first
// batch to arrive rather than on batch boundaries (the kernel decides those),
// and its bound is seconds rather than milliseconds so a loaded CI machine
// cannot fail it.
func TestRealBackendEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("touches the real filesystem watcher")
	}
	root := tempRoot(t)
	w, err := New(root, Options{Debounce: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = w.Close() }()

	sub := filepath.Join(root, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Give the watcher a moment to register the new directory before writing
	// inside it. If it does not, addTree's replay of existing entries is the
	// backstop and the file is still reported — which is exactly the race this
	// package is built to survive, so the test passes either way.
	time.Sleep(100 * time.Millisecond)

	// ONE write, asserted without retries. That is the point: this test used to
	// rewrite the file every 500ms for 15 seconds and still saw nothing,
	// because the watch on pkg had been registered and then silently stripped
	// of write notifications by fsnotify's own re-registration (see
	// dirSet.rewatchTree). Retrying cannot recover from a dead watch, so a
	// retry loop here would only make a real regression take longer to fail
	// while proving nothing extra.
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("package a"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitPath(t, w, "pkg/a.go", 10*time.Second)
}

// TestRealBackendSurvivesDirectoryCreationRace is the regression test for the
// bug above, in the form that actually reproduced it.
//
// Two things make it reproduce where a simpler test does not. It writes into
// the new directory IMMEDIATELY, with no pause for the watch to settle, which
// is what `git checkout` does. And it nests, because fsnotify re-registers
// every entry of a directory it watches, so a subdirectory is exposed to the
// same race as its parent.
//
// The bug was scheduler-sensitive: 100% reproducible once any other test in the
// package had run, and 0% in isolation. Several directories give it several
// chances rather than relying on one.
func TestRealBackendSurvivesDirectoryCreationRace(t *testing.T) {
	if testing.Short() {
		t.Skip("touches the real filesystem watcher")
	}
	root := tempRoot(t)
	w, err := New(root, Options{Debounce: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = w.Close() }()

	const dirs = 5
	want := make(map[string]bool, dirs)
	for i := range dirs {
		rel := fmt.Sprintf("m%d/internal", i)
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		// No sleep. The write races the watch registration deliberately.
		file := rel + "/svc.go"
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file)),
			[]byte("package internal"), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		want[file] = false
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case batch, ok := <-w.Changes():
			if !ok {
				t.Fatal("Changes closed before every file was reported")
			}
			for _, c := range batch {
				if _, tracked := want[c.Path]; tracked {
					want[c.Path] = true
				}
			}
			missing := 0
			for _, seen := range want {
				if !seen {
					missing++
				}
			}
			if missing == 0 {
				return
			}
		case err := <-w.Errors():
			t.Fatalf("watcher error: %v", err)
		case <-deadline:
			for path, seen := range want {
				if !seen {
					t.Errorf("never reported: %s", path)
				}
			}
			t.Fatal("files written into freshly created directories were never " +
				"reported; the watch on those directories is registered but not firing")
		}
	}
}

// awaitPath waits for one path to appear in a batch.
func awaitPath(t *testing.T, w *Watcher, want string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case batch, ok := <-w.Changes():
			if !ok {
				t.Fatalf("Changes closed before %s was reported", want)
			}
			for _, c := range batch {
				if c.Path == want {
					if c.Kind != ChangeCreated && c.Kind != ChangeModified {
						t.Errorf("%s reported as %q", want, c.Kind)
					}
					return
				}
			}
		case err := <-w.Errors():
			t.Fatalf("watcher error: %v", err)
		case <-deadline:
			t.Fatalf("no change reported for %s within %v; the real fsnotify "+
				"backend is not delivering events", want, within)
		}
	}
}

func TestNewRejectsBadRoots(t *testing.T) {
	if _, err := New("  ", Options{}); err == nil {
		t.Error("empty root accepted")
	}
	if _, err := New(filepath.Join(tempRoot(t), "nope"), Options{}); err == nil {
		t.Error("missing root accepted")
	}
	file := filepath.Join(tempRoot(t), "f.go")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(file, Options{}); err == nil {
		t.Error("regular file accepted as root")
	}
}

func TestToOpPrefersStructuralEvents(t *testing.T) {
	tests := []struct {
		in   fsnotify.Op
		want op
	}{
		{fsnotify.Create, opCreate},
		{fsnotify.Write, opWrite},
		{fsnotify.Remove, opRemove},
		{fsnotify.Rename, opRename},
		{fsnotify.Chmod, opChmod},
		{fsnotify.Create | fsnotify.Write, opCreate},
		{fsnotify.Write | fsnotify.Chmod, opWrite},
		{fsnotify.Remove | fsnotify.Write, opRemove},
		{0, 0},
	}
	for _, tc := range tests {
		if got := toOp(tc.in); got != tc.want {
			t.Errorf("toOp(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// newHarnessAt is newHarness over an existing directory.
func newHarnessAt(t *testing.T, root string, opts Options) *harness {
	t.Helper()
	h := &harness{
		t:        t,
		root:     root,
		clock:    NewManualClock(epoch),
		backend:  newFakeBackend(),
		synced:   make(chan struct{}),
		debounce: 300 * time.Millisecond,
	}
	opts.Clock = h.clock
	opts.backend = h.backend
	opts.Debounce = h.debounce
	opts.synced = h.synced

	w, err := New(root, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.w = w
	t.Cleanup(func() { _ = w.Close() })
	return h
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
