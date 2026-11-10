package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/watch"

	"github.com/spf13/cobra"
)

func newWatchCmd(g *Global) *cobra.Command {
	var debounce time.Duration

	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Keep the index current as files change",
		Long: `Watches the repository and updates the index incrementally as you edit.

Runs until interrupted. Editors produce bursts of filesystem events for a
single save, so changes are debounced and coalesced before any reparsing
happens.

The index stays queryable throughout: each file is updated in its own
transaction, so an agent querying concurrently always sees a file either
entirely before or entirely after a change.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWatch(cmd, g, debounce)
		},
	}

	cmd.Flags().DurationVar(&debounce, "debounce", 300*time.Millisecond,
		"quiet period before reacting to a burst of edits")
	return cmd
}

func runWatch(cmd *cobra.Command, g *Global, debounce time.Duration) error {
	ctx := cmd.Context()
	cfg := g.Config()
	log := g.Log()

	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no index at %s — run 'cortex init' first", dbPath)
	}

	db, err := openWritable(ctx, g, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	reg := registry.New(db)
	current, err := reg.CurrentRun(ctx)
	if err != nil {
		if errors.Is(err, registry.ErrNoCommittedRun) {
			return fmt.Errorf("no committed index — run 'cortex index' first")
		}
		return fmt.Errorf("read current index revision: %w", err)
	}

	walker, err := fs.NewWalker(cfg.RepoRoot, cfg)
	if err != nil {
		return fmt.Errorf("prepare discovery: %w", err)
	}

	// Reconcile before watching. Files change while the watcher is not
	// running — a branch switch, a pull, an editor save between sessions — and
	// starting from a stale index would leave those edits invisible until the
	// affected file happened to be touched again.
	if stats, err := reconcile(ctx, reg, current.ID, walker); err != nil {
		return fmt.Errorf("initial reconcile: %w", err)
	} else if changed := stats.Added + stats.Modified + stats.Deleted; changed > 0 {
		log.Info("reconciled changes made while not watching",
			"added", stats.Added, "modified", stats.Modified, "deleted", stats.Deleted)
	}

	w, err := watch.New(cfg.RepoRoot, watch.Options{
		Ignore:   walker.IgnorePredicate(),
		Debounce: debounce,
	})
	if err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}
	defer func() { _ = w.Close() }()

	if !g.JSON {
		fmt.Fprintf(cmd.OutOrStdout(),
			"Watching %s (revision %d). Press Ctrl-C to stop.\n", cfg.ProjectName, current.ID)
	}

	for {
		select {
		case <-ctx.Done():
			// SIGINT/SIGTERM. Nothing to flush: every incremental pass has
			// already committed per file, so stopping here cannot lose or
			// corrupt work.
			if !g.JSON {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nStopped.")
			}
			return nil

		case err, ok := <-w.Errors():
			if !ok {
				return nil
			}
			// A watcher error is usually a lost watch descriptor or a
			// permission problem on one directory. It degrades coverage
			// rather than ending the session, so it is reported and the loop
			// continues.
			log.Warn("watcher error", "error", err)

		case batch, ok := <-w.Changes():
			if !ok {
				return nil
			}
			if len(batch) == 0 {
				continue
			}
			start := time.Now()

			// Re-diff rather than trusting the event batch. Events say a path
			// was touched, not that its content differs — editors rewrite
			// files unchanged, and formatters run on save. Hashing keeps the
			// no-op case cheap and, more importantly, correct.
			stats, err := reconcile(ctx, reg, current.ID, walker)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				log.Error("incremental update failed; index left unchanged", "error", err)
				continue
			}

			changed := stats.Added + stats.Modified + stats.Deleted
			if changed == 0 {
				// Touched but semantically identical. Common enough that
				// logging it at info would be noise.
				log.Debug("filesystem events produced no content change", "events", len(batch))
				continue
			}
			log.Info("index updated",
				"added", stats.Added,
				"modified", stats.Modified,
				"deleted", stats.Deleted,
				"symbols", stats.Symbols,
				"took", time.Since(start).Round(time.Millisecond))
		}
	}
}

// reconcile diffs the working tree against the index and applies what changed.
func reconcile(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	walker *fs.Walker,
) (pipeline.IncrementalStats, error) {
	changes, err := pipeline.Diff(ctx, reg, runID, walker, Version)
	if err != nil {
		return pipeline.IncrementalStats{}, err
	}
	return pipeline.Incremental(ctx, reg, runID, walker, changes, pipeline.IncrementalOptions{
		Resolve: true,
	})
}
