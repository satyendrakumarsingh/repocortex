package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	acquiregit "github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"

	"github.com/spf13/cobra"
)

func newIndexCmd(g *Global) *cobra.Command {
	var (
		workers  int
		showDig  bool
		accuracy string
		full     bool
	)

	cmd := &cobra.Command{
		Use:   "index",
		Short: "Build or update the index for this project",
		Long: `Walks the repository, parses every supported file, and makes the result
queryable.

By default this is incremental: files are compared by content hash against
the current revision and only what changed is reparsed, so cost scales with
your edits rather than repository size. Use --full to rebuild from scratch.

A full rebuild publishes atomically — the new revision becomes visible only
once the entire run is written, so an interrupted rebuild leaves the previous
revision in place.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Validate before choosing a path. This used to run only inside
			// the full-index branch, so `index --accuracy nonsense` exited 0
			// on the incremental path having silently ignored the flag — a
			// typo in CI would have looked like success.
			switch accuracy {
			case "", string(config.AccuracyFast), string(config.AccuracyAuto), string(config.AccuracyExact):
			default:
				return fmt.Errorf("--accuracy %q: want fast, auto or exact", accuracy)
			}
			if workers < 0 {
				return fmt.Errorf("--workers %d: must be zero (use the default) or positive", workers)
			}

			if !full {
				done, err := tryIncremental(cmd, g, showDig)
				if err != nil {
					return err
				}
				if done {
					return nil
				}
				// No committed revision to update — fall through to a full build.
			}
			return runIndex(cmd, g, workers, showDig, accuracy)
		},
	}

	cmd.Flags().IntVar(&workers, "workers", 0,
		"parse concurrency (default 4; peak memory scales with this, not with repository size)")
	cmd.Flags().BoolVar(&showDig, "digest", false, "print the registry digest (determinism gate)")
	cmd.Flags().StringVar(&accuracy, "accuracy", "", "override accuracy mode: fast|auto|exact")
	cmd.Flags().BoolVar(&full, "full", false,
		"rebuild from scratch instead of updating incrementally (discards resolved edges; re-run 'repocortex resolve')")
	return cmd
}

type incrementalReport struct {
	Project    string `json:"project"`
	IndexRun   int64  `json:"index_run"`
	Examined   int64  `json:"files_examined"`
	Added      int64  `json:"files_added"`
	Modified   int64  `json:"files_modified"`
	Deleted    int64  `json:"files_deleted"`
	Unchanged  int64  `json:"files_unchanged"`
	Symbols    int64  `json:"symbols_written"`
	Digest     string `json:"registry_digest,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// tryIncremental updates the committed revision in place.
//
// Returns false when there is nothing to update — no index, or an index that
// has never been populated — so the caller can fall back to a full build.
func tryIncremental(cmd *cobra.Command, g *Global, showDigest bool) (bool, error) {
	ctx := cmd.Context()
	cfg := g.Config()
	start := time.Now()

	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("no index at %s — run 'repocortex init' first", dbPath)
	}

	db, err := openWritable(ctx, g, dbPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()

	reg := registry.New(db)
	current, err := reg.CurrentRun(ctx)
	if err != nil {
		// No committed revision yet: this is a first index, not an error.
		return false, nil
	}

	walker, err := fs.NewWalker(cfg.RepoRoot, cfg)
	if err != nil {
		return false, fmt.Errorf("prepare discovery: %w", err)
	}

	changes, err := pipeline.Diff(ctx, reg, current.ID, walker, Version)
	if err != nil {
		return false, fmt.Errorf("diff working tree: %w", err)
	}

	stats, err := pipeline.Incremental(ctx, reg, current.ID, walker, changes, pipeline.IncrementalOptions{
		Resolve: true,
	})
	if err != nil {
		return false, fmt.Errorf("incremental update failed: %w", err)
	}

	// The pass succeeded, so this run's contents are now this build's work —
	// record that. Only the full path stamps at BeginRun, because only it
	// creates a run; an incremental pass updates the committed run in place.
	//
	// Skipping this would turn the staleness fix into a permanent penalty: the
	// differ treats a version mismatch as "everything is dirty", so a run that
	// re-parsed under the new build and never said so would re-parse in full on
	// every subsequent index, forever.
	if err := reg.StampProducer(ctx, current.ID, Version); err != nil {
		return false, err
	}

	// And record which commit these contents now describe. Only the full path
	// stamps at BeginRun because only it creates a run; without this an
	// incrementally-updated index keeps naming the commit it was FIRST built at,
	// so `head_matches_build` reports a mismatch for an index that is in fact
	// current. The portal's index bar and the MCP provenance block both read
	// this field, and a provenance claim that is wrong is worse than absent.
	if rev := headRevision(ctx, cfg.RepoRoot); rev != "" && rev != current.GitRevision {
		if err := reg.StampGitRevision(ctx, current.ID, rev); err != nil {
			return false, err
		}
	}

	report := incrementalReport{
		Project:    cfg.ProjectName,
		IndexRun:   current.ID,
		Examined:   stats.Examined,
		Added:      stats.Added,
		Modified:   stats.Modified,
		Deleted:    stats.Deleted,
		Unchanged:  stats.Unchanged,
		Symbols:    stats.Symbols,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if showDigest {
		digest, err := reg.RegistryDigest(ctx, current.ID)
		if err != nil {
			return false, fmt.Errorf("compute digest: %w", err)
		}
		report.Digest = digest
	}

	if g.JSON {
		return true, emitJSON(cmd.OutOrStdout(), report)
	}
	out := cmd.OutOrStdout()
	changed := report.Added + report.Modified + report.Deleted
	if changed == 0 {
		_, _ = fmt.Fprintf(out, "%s is up to date (%d files, revision %d, %dms)\n",
			report.Project, report.Unchanged, report.IndexRun, report.DurationMS)
	} else {
		_, _ = fmt.Fprintf(out, "Updated %s (revision %d)\n", report.Project, report.IndexRun)
		_, _ = fmt.Fprintf(out, "  Added:     %d\n", report.Added)
		_, _ = fmt.Fprintf(out, "  Modified:  %d\n", report.Modified)
		_, _ = fmt.Fprintf(out, "  Deleted:   %d\n", report.Deleted)
		_, _ = fmt.Fprintf(out, "  Unchanged: %d\n", report.Unchanged)
		_, _ = fmt.Fprintf(out, "  Symbols:   %d written\n", report.Symbols)
		if report.Digest != "" {
			_, _ = fmt.Fprintf(out, "  Digest:    %s\n", report.Digest)
		}
		_, _ = fmt.Fprintf(out, "  Duration:  %dms\n", report.DurationMS)
	}
	return true, nil
}

type indexReport struct {
	Project       string `json:"project"`
	IndexRun      int64  `json:"index_run"`
	GitRevision   string `json:"git_revision,omitempty"`
	Files         int64  `json:"files_discovered"`
	Indexed       int64  `json:"files_indexed"`
	Skipped       int64  `json:"files_skipped"`
	ParseFailures int64  `json:"parse_failures"`
	Symbols       int64  `json:"symbols"`
	References    int64  `json:"references"`
	Imports       int64  `json:"imports"`
	Digest        string `json:"registry_digest,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	AccuracyMode  string `json:"accuracy_mode"`
}

func runIndex(cmd *cobra.Command, g *Global, workers int, showDigest bool, accuracyOverride string) error {
	ctx := cmd.Context()
	cfg := g.Config()
	log := g.Log()
	start := time.Now()

	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no index at %s — run 'repocortex init' first", dbPath)
	}

	db, err := openWritable(ctx, g, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Stamped with this build, so an index produced by different resolution
	// rules is detectable later rather than silently answering with them.
	reg := registry.New(db, registry.WithProducerVersion(Version))

	// A previous run may have died mid-index. Its rows are invisible (never
	// committed) but they occupy space, so reclaim before starting.
	if reclaimed, err := reg.ReclaimAbandoned(ctx); err != nil {
		return fmt.Errorf("reclaim abandoned runs: %w", err)
	} else if len(reclaimed) > 0 {
		log.Warn("reclaimed abandoned index runs", "runs", reclaimed)
	}

	accuracy := string(cfg.Accuracy)
	if accuracyOverride != "" {
		accuracy = accuracyOverride
	}

	// Git revision is metadata, not a requirement. RepoCortex must work on a
	// plain directory, so a missing repository is recorded as empty and the
	// run proceeds.
	gitRev := ""
	if repo, err := acquiregit.Open(cfg.RepoRoot); err != nil {
		// Reported, not swallowed. This used to be `if err == nil`, so a
		// repository go-git declined looked exactly like a plain directory and
		// the index recorded no revision at all — silently.
		log.Warn("git unavailable; the index will record no revision", "err", err)
	} else {
		if d := repo.Degraded(); d != "" {
			log.Warn("git support is degraded; revision is read directly from .git and history is unavailable",
				"reason", d)
		}
		if info, err := repo.Info(ctx); err != nil {
			log.Warn("git revision unavailable", "err", err)
		} else if info.IsRepo {
			gitRev = info.HeadSHA
			if info.Dirty {
				log.Warn("working tree has uncommitted changes; the index describes files on disk, not the commit")
			}
		}
	}

	run, err := reg.BeginRun(ctx, accuracy, gitRev)
	if err != nil {
		return fmt.Errorf("begin index run: %w", err)
	}

	// Any failure past this point must leave the previous revision intact.
	committed := false
	defer func() {
		if !committed {
			if err := reg.AbortRun(context.WithoutCancel(ctx), run.ID); err != nil {
				log.Error("failed to abort staging run; 'repocortex index' will reclaim it", "run", run.ID, "error", err)
			}
		}
	}()

	walker, err := fs.NewWalker(cfg.RepoRoot, cfg)
	if err != nil {
		return fmt.Errorf("prepare discovery: %w", err)
	}

	writer, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("open writer: %w", err)
	}
	defer func() { _ = writer.Close() }()

	sink := pipeline.NewRegistrySink(writer)

	var lastPct int64 = -1
	runner := pipeline.NewRunner(walker, pipeline.Options{
		Workers: workers,
		OnProgress: func(done, total int64) {
			if total == 0 || g.Quiet || g.JSON {
				return
			}
			// Log on decile boundaries: a line per file would drown the
			// output on a large repository.
			if pct := done * 100 / total; pct/10 > lastPct/10 {
				lastPct = pct
				log.Info("indexing", "progress", fmt.Sprintf("%d%%", pct), "files", fmt.Sprintf("%d/%d", done, total))
			}
		},
	})

	stats, err := runner.Run(ctx, sink)
	if err != nil {
		return fmt.Errorf("indexing failed: %w", err)
	}
	// Refuse to publish or flush a run the user interrupted: a partial index that looks
	// complete is worse than no new index at all.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("indexing cancelled; previous revision left in place")
	}
	if err := sink.Flush(ctx); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close writer: %w", err)
	}

	if err := reg.CommitRun(ctx, run.ID); err != nil {
		return fmt.Errorf("publish index revision: %w", err)
	}
	committed = true

	report := indexReport{
		Project:       cfg.ProjectName,
		IndexRun:      run.ID,
		GitRevision:   gitRev,
		Files:         stats.FilesDiscovered,
		Indexed:       stats.FilesIndexed,
		Skipped:       stats.FilesSkipped,
		ParseFailures: stats.ParseFailures,
		Symbols:       stats.Symbols,
		References:    stats.References,
		Imports:       stats.Imports,
		DurationMS:    time.Since(start).Milliseconds(),
		AccuracyMode:  accuracy,
	}
	if showDigest {
		digest, err := reg.RegistryDigest(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("compute digest: %w", err)
		}
		report.Digest = digest
	}

	return renderIndexReport(cmd, g, report)
}

func renderIndexReport(cmd *cobra.Command, g *Global, r indexReport) error {
	if g.JSON {
		return emitJSON(cmd.OutOrStdout(), r)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Indexed %s\n", r.Project)
	_, _ = fmt.Fprintf(out, "  Revision:      %d\n", r.IndexRun)
	if r.GitRevision != "" {
		short := r.GitRevision
		if len(short) > 12 {
			short = short[:12]
		}
		_, _ = fmt.Fprintf(out, "  Git:           %s\n", short)
	}
	_, _ = fmt.Fprintf(out, "  Files:         %d indexed, %d skipped (%d discovered)\n", r.Indexed, r.Skipped, r.Files)
	_, _ = fmt.Fprintf(out, "  Symbols:       %d\n", r.Symbols)
	_, _ = fmt.Fprintf(out, "  References:    %d (unresolved until M2)\n", r.References)
	_, _ = fmt.Fprintf(out, "  Imports:       %d\n", r.Imports)
	if r.ParseFailures > 0 {
		_, _ = fmt.Fprintf(out, "  Parse failures: %d\n", r.ParseFailures)
	}
	if r.Digest != "" {
		_, _ = fmt.Fprintf(out, "  Digest:        %s\n", r.Digest)
	}
	_, _ = fmt.Fprintf(out, "  Duration:      %dms\n", r.DurationMS)
	return nil
}

// context helper for Go versions without context.WithoutCancel semantics in
// scope; kept explicit so the abort path is obviously immune to the cancelled
// parent context.

// headRevision reads the commit HEAD names, or "" when this is not a repository.
//
// Extracted so the incremental path can stamp the same value the full path
// records at BeginRun. Warnings are the caller's business: this is used on the
// incremental path where a missing revision is not worth a second warning.
func headRevision(ctx context.Context, root string) string {
	repo, err := acquiregit.Open(root)
	if err != nil {
		return ""
	}
	info, err := repo.Info(ctx)
	if err != nil || !info.IsRepo {
		return ""
	}
	return info.HeadSHA
}
