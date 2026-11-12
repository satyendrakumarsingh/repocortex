package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/web"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

// The CLI half of the portal's refresh.
//
// internal/web declares what it needs (web.Reindexer) and knows nothing about
// how an index gets built. This file is the adapter that satisfies that port,
// and it is allowed to depend on the indexing pipeline precisely because the
// dependency points this way and never back (CLAUDE.md §1, §3 DIP). Putting it
// the other way round — a portal that imports internal/pipeline — is what would
// make "the portal is read-only" a claim nobody could check by reading imports.
//
// It rebuilds by RUNNING THE COMMANDS rather than by re-implementing them.
// `repocortex index` already chooses between an incremental pass and a full
// rebuild, reclaims runs a crash abandoned, stamps the producer version and
// refuses to publish a run the user interrupted; `repocortex resolve` already
// binds references, table accesses, configuration reads and deployment units in
// one pass. A second copy of that sequence living inside the portal would be a
// copy that drifts, and the first symptom would be a refresh that quietly
// produces a less complete graph than the CLI does — visible to nobody, because
// both would report success.

// refreshStep is one stage of a refresh: index, then resolve.
type refreshStep func(ctx context.Context) error

// runReader reports what a finished refresh produced.
//
// Declared here at the consumer, and deliberately two methods wide: the adapter
// needs the committed revision and its row totals, not a registry (CLAUDE.md
// §1, ISP). Counts recomputes from the rows themselves rather than reading the
// run's summary columns, which matters here because the incremental path
// updates a run in place and leaves those columns describing the build before
// it.
type runReader interface {
	CurrentRun(ctx context.Context) (*model.IndexRun, error)
	Counts(ctx context.Context, runID int64) (registry.Counts, error)
}

// uiReindexer implements web.Reindexer for `repocortex ui --allow-refresh`.
type uiReindexer struct {
	runs    runReader
	index   refreshStep
	resolve refreshStep
	now     func() time.Time
}

// uiReindexOption configures a uiReindexer (CLAUDE.md §2, functional options).
type uiReindexOption func(*uiReindexer)

// withRefreshSteps substitutes the two stages.
//
// The only caller is a test: running the real pipeline to assert that a POST is
// gated, cancelled or reported correctly would test SQLite and tree-sitter
// instead of the wiring under examination.
func withRefreshSteps(index, resolve refreshStep) uiReindexOption {
	return func(u *uiReindexer) {
		if index != nil {
			u.index = index
		}
		if resolve != nil {
			u.resolve = resolve
		}
	}
}

// withRefreshClock injects the clock so a reported duration is assertable.
func withRefreshClock(now func() time.Time) uiReindexOption {
	return func(u *uiReindexer) {
		if now != nil {
			u.now = now
		}
	}
}

// newUIReindexer builds the adapter the portal calls.
//
// runs is the portal's own read-only registry. Reusing it rather than opening a
// third handle is safe and is the point: the refresh's writes land through the
// indexer's own writable connection, and this connection reads back what that
// connection committed.
func newUIReindexer(g *Global, runs runReader, opts ...uiReindexOption) *uiReindexer {
	u := &uiReindexer{
		runs: runs,
		index: func(ctx context.Context) error {
			return runSubcommand(ctx, newIndexCmd(g))
		},
		resolve: func(ctx context.Context) error {
			return runSubcommand(ctx, newResolveCmd(g))
		},
		now: time.Now,
	}
	for _, o := range opts {
		o(u)
	}
	return u
}

// Reindex implements web.Reindexer.
func (u *uiReindexer) Reindex(ctx context.Context) (web.ReindexResult, error) {
	start := u.now()

	// Checked before doing anything: the portal bounds a refresh with a
	// timeout, and a request that has already been abandoned must not start an
	// indexer that then writes over a database nobody is waiting on.
	if err := ctx.Err(); err != nil {
		return web.ReindexResult{}, fmt.Errorf("refresh not started: %w", err)
	}
	if err := u.index(ctx); err != nil {
		return web.ReindexResult{}, fmt.Errorf("refresh: 'repocortex index' failed and the previous revision was left in place: %w", err)
	}
	// Between the stages as well as before them. Indexing publishes atomically,
	// so a cancellation caught here leaves a complete index with stale edges
	// rather than a torn one — a state worth stopping in, and worth naming
	// rather than reporting as a successful refresh.
	if err := ctx.Err(); err != nil {
		return web.ReindexResult{}, fmt.Errorf("refresh cancelled after indexing, before resolution: %w", err)
	}
	if err := u.resolve(ctx); err != nil {
		return web.ReindexResult{}, fmt.Errorf("refresh: 'repocortex resolve' failed; the index is current but its edges are not: %w", err)
	}

	run, err := u.runs.CurrentRun(ctx)
	if err != nil {
		return web.ReindexResult{}, fmt.Errorf("refresh: read the revision it produced: %w", err)
	}
	counts, err := u.runs.Counts(ctx, run.ID)
	if err != nil {
		return web.ReindexResult{}, fmt.Errorf("refresh: count revision %d: %w", run.ID, err)
	}
	return web.ReindexResult{
		Revision:    run.ID,
		GitRevision: run.GitRevision,
		Files:       int(counts.Files),
		Symbols:     int(counts.Symbols),
		Edges:       int(counts.Edges),
		DurationMS:  u.now().Sub(start).Milliseconds(),
	}, nil
}

// runSubcommand runs one of RepoCortex's own commands under ctx, silently.
//
// Two things here are not cosmetic. SetArgs with an empty (but non-nil) slice
// is required: cobra falls back to os.Args when a command has none set, and
// os.Args in this process belongs to `repocortex ui` — an unguarded refresh would
// hand `--port` and `--allow-refresh` to `index` and fail on an unknown flag.
// Discarding the writers keeps the command's human report off the terminal of
// an operator who asked for a portal, not for an index summary; the logger is
// left alone, so progress still appears where they expect it.
func runSubcommand(ctx context.Context, cmd *cobra.Command) error {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{})
	return cmd.ExecuteContext(ctx)
}
