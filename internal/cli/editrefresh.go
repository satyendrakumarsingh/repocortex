package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// editRefresher re-indexes the files an agent changed before the server
// answers its next question (mcp.Refresher). See internal/mcp/refresh.go for
// why; this is how.
//
// THE COST, MEASURED. Comparing the working tree with the index (size and
// modification time, hashing only what differs) took 0.2 s on this
// repository's 1,900 files and 0.4-0.6 s on a 25,000-file one. Paid on every
// call that is too much, so a check runs at most every checkFloor, and the
// interval stretches to ten times the last check's duration: the check never
// takes more than about a tenth of the server's time, whatever the size of
// the tree. Re-indexing a changed file is the incremental path the watcher
// and the git hooks use, a few milliseconds a file.
//
// ONE WRITER AT A TIME. A git hook's background pass and `--watch` write the
// same index, and all three take .repocortex/reindex.lock. A call waits up to
// lockWait for it, long enough for a watcher's pass over a just-saved file to
// land so the answer includes it; past that the answer is given as the index
// stands and says another update was running.
//
// THE AGENT'S OWN CONNECTION STAYS READ-ONLY. The refresher opens its own
// writable handle, lazily, and never migrates the schema: an index that is
// behind this build is reported once and left for `repocortex migrate`.
type editRefresher struct {
	g        *Global
	dbPath   string
	lockPath string

	mu       sync.Mutex
	last     time.Time // when the last check began
	interval time.Duration
	disabled error // set once when checking cannot work (schema, open)

	db     *store.DB
	reg    *registry.Registry
	walker *fs.Walker

	// The work one call may do, so a branch switch of thousands of files
	// never blocks a tool call for minutes. See refreshInBackground.
	maxFiles   int           // files re-indexed inside one call
	syncBudget time.Duration // and about this long, when positive
	chunk      int           // files per background step

	// Background state. bgMu guards it; the goroutine never takes mu.
	bgMu        sync.Mutex
	bgPending   int      // changed files the background pass has yet to do
	bgDone      []string // files the finished background pass re-indexed, handed out once
	bgCancel    stdctx.CancelFunc
	bgFinished  chan struct{}
	beforeChunk func() // test hook, run before each background step
}

const (
	checkFloor   = 2 * time.Second
	checkCeiling = 30 * time.Second
	checkFactor  = 10
	lockWait     = 2 * time.Second

	// refreshMaxFiles bounds the files one call re-indexes before it answers.
	// Measured on a C repository: about 0.45 s fixed (resolving the changed
	// files' references) plus about 0.1 s a file, so 20 files is about 2.5 s,
	// the longest a tool call should wait. Anything beyond is finished by a
	// background pass that holds the same reindex.lock.
	refreshMaxFiles = 20
	// refreshSyncBudget is the time a call may spend before it answers, in
	// the same units: the files it takes are chosen so they fit, up to
	// refreshMaxFiles. The count alone put a call at 9 s on a loaded machine.
	refreshSyncBudget = 2500 * time.Millisecond
	// syncFirstStep is how many files a call re-indexes before it knows what
	// a file costs.
	syncFirstStep = 5
	// refreshChunk is the background pass's step: large enough to amortise the
	// fixed cost, small enough that Close and progress reports stay prompt.
	refreshChunk = 100
)

func newEditRefresher(g *Global, dbPath string) *editRefresher {
	return &editRefresher{
		g:          g,
		dbPath:     dbPath,
		lockPath:   filepath.Join(filepath.Dir(dbPath), "reindex.lock"),
		interval:   checkFloor,
		maxFiles:   refreshMaxFiles,
		syncBudget: refreshSyncBudget,
		chunk:      refreshChunk,
	}
}

// Refresh implements mcp.Refresher. Calls are serialised: one that arrives
// while a check runs waits for it, which is what makes its answer include the
// edits that check found.
func (r *editRefresher) Refresh(ctx stdctx.Context) devmcp.RefreshOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled != nil {
		return devmcp.RefreshOutcome{LastCheck: r.last, Err: r.disabled}
	}
	// A background pass is finishing an earlier call's work and holds the
	// lock: report its progress, and the files it re-indexed once it is done.
	if pending, done := r.background(); pending > 0 {
		return devmcp.RefreshOutcome{LastCheck: r.last, Pending: pending}
	} else if len(done) > 0 {
		return devmcp.RefreshOutcome{LastCheck: r.last, Refreshed: done}
	}
	if !r.last.IsZero() && time.Since(r.last) < r.interval {
		return devmcp.RefreshOutcome{LastCheck: r.last}
	}

	lock, err := lockWithin(ctx, r.lockPath, lockWait)
	if err != nil {
		if errors.Is(err, fs.ErrLocked) {
			return devmcp.RefreshOutcome{LastCheck: r.last, Busy: true}
		}
		return devmcp.RefreshOutcome{LastCheck: r.last, Err: err}
	}
	handedOver := false
	defer func() {
		if !handedOver {
			_ = lock.Unlock()
		}
	}()

	if err := r.open(ctx); err != nil {
		r.disabled = err
		r.g.Log().Warn("not checking edits before answering", "error", err)
		return devmcp.RefreshOutcome{Err: err}
	}
	run, err := r.reg.CurrentRun(ctx)
	if err != nil {
		// No committed index: the tools answer with guidance, and there is
		// nothing to bring up to date.
		return devmcp.RefreshOutcome{}
	}

	began := time.Now()
	// Diffed as the build that made the index. Diffing as this build would,
	// after an upgrade, mark every file changed and re-parse the repository
	// inside one request; that is `repocortex index`'s work, and it says so.
	changes, err := pipeline.DiffWith(ctx, r.reg, run.ID, r.walker, run.ProducerVersion, pipeline.DiffOptions{})
	took := time.Since(began)
	r.last = began
	r.interval = min(max(checkFloor, checkFactor*took), checkCeiling)
	if err != nil {
		return devmcp.RefreshOutcome{LastCheck: began, Err: fmt.Errorf("compare the working tree: %w", err)}
	}

	var paths []string
	touched := false
	for _, ch := range changes {
		switch {
		case ch.Kind != pipeline.ChangeSame:
			paths = append(paths, ch.Path)
		case ch.Touched:
			touched = true
		}
	}
	if len(paths) == 0 && !touched {
		// Nothing to write. Writing anyway would re-stamp the run, which
		// makes every cache in the server reload for no change at all.
		return devmcp.RefreshOutcome{LastCheck: began}
	}

	if r.maxFiles > 0 && len(paths) > r.maxFiles {
		out, err := r.refreshInBackground(ctx, run, changes, paths, began, lock)
		handedOver = err == nil
		if err != nil {
			return devmcp.RefreshOutcome{LastCheck: began, Err: fmt.Errorf("re-index changed files: %w", err)}
		}
		return out
	}

	if _, err := pipeline.Refresh(ctx, r.reg, run.ID, r.walker, pipeline.RefreshOptions{
		RunningAs: run.ProducerVersion,
		Root:      r.g.Config().RepoRoot,
	}); err != nil {
		return devmcp.RefreshOutcome{LastCheck: began, Err: fmt.Errorf("re-index changed files: %w", err)}
	}
	r.g.Log().Info("re-indexed files changed since the last answer", "files", len(paths), "took", time.Since(began).Round(time.Millisecond))
	return devmcp.RefreshOutcome{LastCheck: began, Refreshed: paths}
}

// background reports the files a background pass has left, or, once it has
// finished, the files it re-indexed (once).
func (r *editRefresher) background() (pending int, done []string) {
	r.bgMu.Lock()
	defer r.bgMu.Unlock()
	if r.bgPending > 0 {
		return r.bgPending, nil
	}
	done, r.bgDone = r.bgDone, nil
	return 0, done
}

// refreshInBackground handles a change set larger than one call's budget: it
// re-indexes the first maxFiles files now, then hands the rest, and the
// reindex.lock, to a goroutine and answers with how many remain. Files the
// background pass has not reached still differ from the index, so a pass that
// fails part way loses nothing: the next check finds them again.
func (r *editRefresher) refreshInBackground(
	ctx stdctx.Context, run *model.IndexRun, changes []pipeline.FileChange, paths []string,
	began time.Time, lock *fs.FileLock,
) (devmcp.RefreshOutcome, error) {
	var work, touched []pipeline.FileChange
	for _, ch := range changes {
		switch {
		case ch.Kind != pipeline.ChangeSame:
			work = append(work, ch)
		case ch.Touched:
			touched = append(touched, ch)
		}
	}
	if _, err := r.reg.BackfillDefinitionText(ctx, run.ID); err != nil {
		return devmcp.RefreshOutcome{}, err
	}
	// At most maxFiles files, and, when syncBudget is set, about that long:
	// the first step is a few files, and each later one is sized from what
	// the files so far cost, so a slow machine or a heavy file stops sooner
	// than a count would. Always at least the first step, so a call makes
	// progress however slow the files are.
	taken := 0
	started := time.Now()
	for taken < r.maxFiles {
		n := r.maxFiles - taken // no time cap: one step of every file allowed
		if r.syncBudget > 0 {
			n = min(syncFirstStep, n)
			if taken > 0 {
				elapsed := time.Since(started)
				perFile := elapsed / time.Duration(taken)
				if elapsed >= r.syncBudget || perFile <= 0 {
					break
				}
				n = min(int((r.syncBudget-elapsed)/perFile), r.maxFiles-taken)
				if n < 1 {
					break
				}
			}
		}
		if err := r.step(ctx, run, work[taken:taken+n]); err != nil {
			return devmcp.RefreshOutcome{}, err
		}
		taken += n
	}
	first := work[:taken]
	later := work[taken:]
	rest := append(append([]pipeline.FileChange(nil), later...), touched...)
	remaining := len(later)
	r.g.Log().Info("re-indexing a large edit in the background", "files", len(paths), "now", len(first), "remaining", remaining)

	bgCtx, cancel := stdctx.WithCancel(stdctx.Background())
	finished := make(chan struct{})
	r.bgMu.Lock()
	r.bgPending, r.bgCancel, r.bgFinished = remaining, cancel, finished
	r.bgMu.Unlock()
	go r.drain(bgCtx, run, rest, remaining, later, began, lock, finished)

	refreshed := make([]string, 0, len(first))
	for _, ch := range first {
		refreshed = append(refreshed, ch.Path)
	}
	return devmcp.RefreshOutcome{LastCheck: began, Refreshed: refreshed, Pending: remaining}, nil
}

// step re-indexes one batch of changes and brings the run's recorded health
// and figures up to date, as pipeline.Refresh does after a pass.
func (r *editRefresher) step(ctx stdctx.Context, run *model.IndexRun, batch []pipeline.FileChange) error {
	stats, err := pipeline.Incremental(ctx, r.reg, run.ID, r.walker, batch, pipeline.IncrementalOptions{
		Resolve: true, Root: r.g.Config().RepoRoot,
	})
	if err != nil {
		return err
	}
	if stats.Added+stats.Modified+stats.Deleted > 0 {
		if err := r.reg.RefreshRunHealth(ctx, run.ID); err != nil {
			return err
		}
		return r.reg.RefreshRunStats(ctx, run.ID)
	}
	return nil
}

// drain re-indexes the rest in steps, then stamps the run as pipeline.Refresh
// does (only now: a half-finished pass has not made the index this build's
// work), and releases the lock. It is the lock's owner from the hand-over.
func (r *editRefresher) drain(
	ctx stdctx.Context, run *model.IndexRun, rest []pipeline.FileChange, pending int,
	listed []pipeline.FileChange, began time.Time, lock *fs.FileLock, finished chan struct{},
) {
	defer close(finished)
	defer func() { _ = lock.Unlock() }()
	var failure error
	remaining := pending
	for len(rest) > 0 && failure == nil {
		if r.beforeChunk != nil {
			r.beforeChunk()
		}
		n := min(r.chunk, len(rest))
		if err := r.step(ctx, run, rest[:n]); err != nil {
			failure = err
			break
		}
		for _, ch := range rest[:n] {
			if ch.Kind != pipeline.ChangeSame {
				remaining--
			}
		}
		rest = rest[n:]
		r.bgMu.Lock()
		// Never zero before the pass has stamped the run and listed what it
		// did: a call that saw zero would take the lock this goroutine still
		// holds, and find nothing to report.
		r.bgPending = max(remaining, 1)
		r.bgMu.Unlock()
	}
	if failure == nil {
		if err := r.reg.StampProducer(ctx, run.ID, run.ProducerVersion); err != nil {
			failure = err
		} else if err := r.reg.MarkUpdated(ctx, run.ID, began.Unix()); err != nil {
			failure = err
		}
	}
	r.bgMu.Lock()
	defer r.bgMu.Unlock()
	r.bgPending = 0
	if failure != nil {
		if ctx.Err() == nil {
			r.g.Log().Warn("background re-index stopped; the next check picks up the rest", "error", failure)
		}
		return
	}
	for _, ch := range listed {
		r.bgDone = append(r.bgDone, ch.Path)
	}
	r.g.Log().Info("background re-index finished", "files", len(listed), "took", time.Since(began).Round(time.Millisecond))
}

// open prepares the writable handle and the walker on first use.
func (r *editRefresher) open(ctx stdctx.Context) error {
	if r.reg != nil {
		return nil
	}
	db, err := openWritableCurrent(ctx, r.dbPath)
	if err != nil {
		return fmt.Errorf("open the index for writing: %w; run 'repocortex migrate' if the schema is behind", err)
	}
	walker, err := fs.NewWalker(r.g.Config().RepoRoot, r.g.Config())
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("prepare discovery: %w", err)
	}
	r.db, r.reg, r.walker = db, registry.New(db), walker
	return nil
}

// Close releases the writable handle.
func (r *editRefresher) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bgMu.Lock()
	cancel, finished := r.bgCancel, r.bgFinished
	r.bgMu.Unlock()
	if cancel != nil {
		cancel()
		<-finished
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db, r.reg = nil, nil
	}
}

// lockWithin takes the index's write lock, waiting up to d for whoever holds
// it. It returns fs.ErrLocked when d passes first.
func lockWithin(ctx stdctx.Context, path string, d time.Duration) (*fs.FileLock, error) {
	deadline := time.Now().Add(d)
	for {
		lock, err := fs.TryLock(path)
		if err == nil || !errors.Is(err, fs.ErrLocked) {
			return lock, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
