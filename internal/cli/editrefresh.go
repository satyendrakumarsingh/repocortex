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
}

const (
	checkFloor   = 2 * time.Second
	checkCeiling = 30 * time.Second
	checkFactor  = 10
	lockWait     = 2 * time.Second
)

func newEditRefresher(g *Global, dbPath string) *editRefresher {
	return &editRefresher{
		g:        g,
		dbPath:   dbPath,
		lockPath: filepath.Join(filepath.Dir(dbPath), "reindex.lock"),
		interval: checkFloor,
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
	defer func() { _ = lock.Unlock() }()

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

	if _, err := pipeline.Refresh(ctx, r.reg, run.ID, r.walker, pipeline.RefreshOptions{
		RunningAs: run.ProducerVersion,
		Root:      r.g.Config().RepoRoot,
	}); err != nil {
		return devmcp.RefreshOutcome{LastCheck: began, Err: fmt.Errorf("re-index changed files: %w", err)}
	}
	r.g.Log().Info("re-indexed files changed since the last answer", "files", len(paths), "took", time.Since(began).Round(time.Millisecond))
	return devmcp.RefreshOutcome{LastCheck: began, Refreshed: paths}
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
