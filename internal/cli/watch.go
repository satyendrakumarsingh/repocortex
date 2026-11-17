package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
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

It holds one watch per directory and none per file. On Linux and Windows the
kernel reports every change through those. On macOS the kernel reports a file
created, deleted or renamed, which is how most editors save, at once; a file
rewritten in place is found by a sweep of file sizes and times every two
seconds or so (longer on a tree that takes long to sweep), so such an edit can
take that long to be noticed.

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
	return watchLoop(cmd.Context(), g, debounce, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// watchLoop is the watch command's body, also run inside `serve --watch`.
// out receives what a person at a terminal reads and errOut the start-up
// progress; serve passes io.Discard for both, because on stdio its stdout is
// the MCP transport and any stray byte there corrupts the protocol.
func watchLoop(ctx context.Context, g *Global, debounce time.Duration, out, errOut io.Writer) error {
	cfg := g.Config()
	log := g.Log()

	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no index at %s — run 'repocortex init' first", dbPath)
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
			return fmt.Errorf("no committed index — run 'repocortex index' first")
		}
		return fmt.Errorf("read current index revision: %w", err)
	}

	walker, err := fs.NewWalker(cfg.RepoRoot, cfg)
	if err != nil {
		return fmt.Errorf("prepare discovery: %w", err)
	}

	// Watch first, reconcile second.
	//
	// It used to be the other way round, and the gap between the two was where
	// edits were lost: the reconcile ran, then registering the tree took 17
	// seconds to over a minute on a 7,100-file repository, and a file saved in
	// that time was neither in the reconcile that had already finished nor
	// seen by a watch that was not yet placed. Registering first means every
	// directory is covered from some moment during start-up, and the
	// reconcile that follows reads the tree as it is after all of them are.
	//
	// The context goes in so Ctrl-C during start-up stops it between two
	// directories instead of after the last one, and progress is printed so
	// that a slow start says it is still starting.
	started := time.Now()
	progress := func(dirs int) {
		if !g.JSON && !g.Quiet {
			fmt.Fprintf(errOut, "watching %d directories...\n", dirs)
		}
	}
	w, err := watch.New(cfg.RepoRoot, watch.Options{
		Ignore:   walker.IgnorePredicate(),
		Debounce: debounce,
		Context:  ctx,
		Progress: progress,
	})
	if err != nil {
		if ctx.Err() != nil {
			// Interrupted while starting. Nothing was changed, so there is
			// nothing to report beyond having stopped.
			if !g.JSON {
				_, _ = fmt.Fprintln(out, "\nStopped before watching began.")
			}
			return nil
		}
		return fmt.Errorf("start watcher: %w", err)
	}
	defer func() { _ = w.Close() }()

	// Files change while the watcher is not running (a branch switch, a pull,
	// an editor save between sessions) and while it was starting. Starting
	// from a stale index would leave those edits invisible until the affected
	// file happened to be touched again.
	if stats, err := refreshIndex(ctx, reg, current, walker, cfg.RepoRoot, false); err != nil {
		if ctx.Err() != nil {
			if !g.JSON {
				_, _ = fmt.Fprintln(out, "\nStopped before watching began.")
			}
			return nil
		}
		return fmt.Errorf("initial reconcile: %w", err)
	} else if changed := stats.Added + stats.Modified + stats.Deleted; changed > 0 {
		log.Info("reconciled changes made while not watching",
			"added", stats.Added, "modified", stats.Modified, "deleted", stats.Deleted)
	}

	if !g.JSON {
		fmt.Fprintf(out,
			"Watching %s: %d directories (revision %d), ready in %s. Press Ctrl-C to stop.\n",
			cfg.ProjectName, w.WatchedDirs(), current.ID, time.Since(started).Round(10*time.Millisecond))
	}

	var stoppedPrinted bool
	printStopped := func() {
		if !g.JSON && !stoppedPrinted {
			stoppedPrinted = true
			_, _ = fmt.Fprintln(out, "\nStopped.")
		}
	}
	defer func() {
		if ctx.Err() != nil {
			printStopped()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// SIGINT/SIGTERM. Nothing to flush: every incremental pass has
			// already committed per file, so stopping here cannot lose or
			// corrupt work.
			printStopped()
			return nil

		case err, ok := <-w.Errors():
			if !ok {
				if ctx.Err() != nil {
					printStopped()
				}
				return nil
			}
			// A watcher error is usually a lost watch descriptor or a
			// permission problem on one directory. It degrades coverage
			// rather than ending the session, so it is reported and the loop
			// continues.
			log.Warn("watcher error", "error", err)

		case batch, ok := <-w.Changes():
			if !ok {
				if ctx.Err() != nil {
					printStopped()
				}
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
			stats, err := refreshIndex(ctx, reg, current, walker, cfg.RepoRoot, false)
			if err != nil {
				if ctx.Err() != nil {
					printStopped()
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
