package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	acquiregit "github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

func newIndexCmd(g *Global) *cobra.Command {
	var (
		workers  int
		showDig  bool
		accuracy string
		full     bool
		rehash   bool
	)

	cmd := &cobra.Command{
		Use:   "index",
		Short: "Build or update the index for this project",
		Long: `Walks the repository, parses every supported file, and makes the result
queryable.

By default this is incremental: only files whose content changed are reparsed,
so an update costs a small fraction of a rebuild. It is not independent of
repository size. Every file is stat'ed on every run, and after a change
references are re-resolved across the whole index. docs/performance.md has
measured figures. Use --full to rebuild from scratch.

A file is read only when it might have changed. If its size and modification
time match the index, and both are more than two seconds older than the
previous run, it is taken as unchanged without being opened; on Linux and macOS
the inode change time must be that old too, which catches an edit whose
modification time was put back. Every other file is hashed and the hash
decides, so a touched file with the same content is not reparsed and a same-size
edit made in the same second is not missed. Windows has no change time to
check: there, a same-size edit whose modification time was restored to the
second is not seen. --rehash reads and hashes every file regardless.

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

			if full && rehash {
				return fmt.Errorf("--rehash applies to an incremental update; --full already reads every file")
			}
			if !full {
				done, err := tryIncremental(cmd, g, showDig, rehash)
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
	cmd.Flags().BoolVar(&rehash, "rehash", false,
		"read and hash every file instead of trusting an unchanged size and modification time")
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

	// Parse describes every file now in the index, not only the ones this
	// pass touched: what a reader wants after an update is the state of the
	// index, and a file skipped for its size last week is still skipped.
	Parse registry.ParseSummary `json:"parse"`
}

// tryIncremental updates the committed revision in place.
//
// Returns false when there is nothing to update — no index, or an index that
// has never been populated — so the caller can fall back to a full build.
func tryIncremental(cmd *cobra.Command, g *Global, showDigest, rehash bool) (bool, error) {
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

	stats, err := refreshIndex(ctx, reg, current, walker, cfg.RepoRoot, rehash)
	if err != nil {
		return false, fmt.Errorf("incremental update failed: %w", err)
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
	if report.Parse, err = reg.ParseSummary(ctx, current.ID); err != nil {
		return false, err
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
		// Said even when nothing changed. A file left out for its size is left
		// out on every run, and a command that reports "up to date" over it
		// without naming it is how it stayed unnoticed.
		writeIndexGaps(out, report.Parse)
	} else {
		_, _ = fmt.Fprintf(out, "Updated %s (revision %d)\n", report.Project, report.IndexRun)
		_, _ = fmt.Fprintf(out, "  Added:     %d\n", report.Added)
		_, _ = fmt.Fprintf(out, "  Modified:  %d\n", report.Modified)
		_, _ = fmt.Fprintf(out, "  Deleted:   %d\n", report.Deleted)
		_, _ = fmt.Fprintf(out, "  Unchanged: %d\n", report.Unchanged)
		_, _ = fmt.Fprintf(out, "  Symbols:   %d written\n", report.Symbols)
		writeParseSummary(out, report.Parse, "  ", len("Parse failures:"))
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
	PartialParses int64  `json:"partial_parses"`
	Symbols       int64  `json:"symbols"`
	References    int64  `json:"references"`
	Imports       int64  `json:"imports"`
	Digest        string `json:"registry_digest,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	AccuracyMode  string `json:"accuracy_mode"`

	// Parse says what happened to every file: how many parsed cleanly, with
	// syntax errors, or incompletely; how many are searchable text only; and
	// which were left out for their size, by name.
	Parse registry.ParseSummary `json:"parse"`
	// MaxFileBytes is the size limit this run applied.
	MaxFileBytes int64 `json:"max_file_bytes"`
}

// writeIndexGaps prints only the rows of a summary that name something missing
// from the index. Used where the full breakdown would be noise.
func writeIndexGaps(out io.Writer, s registry.ParseSummary) {
	gaps := registry.ParseSummary{
		Incomplete: s.Incomplete, IncompleteFiles: s.IncompleteFiles,
		Failed: s.Failed, FailedFiles: s.FailedFiles,
		TooLarge: s.TooLarge, LargestTooLarge: s.LargestTooLarge,
	}
	for _, l := range parseSummaryLines(gaps) {
		if l.Label == "Parsed" {
			continue
		}
		_, _ = fmt.Fprintf(out, "  %s: %s\n", l.Label, l.Value)
		for _, d := range l.Detail {
			_, _ = fmt.Fprintf(out, "      %s\n", d)
		}
	}
}

func runIndex(cmd *cobra.Command, g *Global, workers int, showDigest bool, accuracyOverride string) error {
	ctx := cmd.Context()
	cfg := g.Config()
	log := g.Log()
	start := time.Now()
	defer startProfiling()()

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
			if repo.HistoryAvailable() {
				log.Info("go-git declined this repository; revision is read from .git and history through the git binary",
					"reason", d)
			} else {
				log.Warn("git support is degraded; revision is read directly from .git and history is unavailable",
					"reason", d)
			}
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

	// The first index of a project writes into tables nothing else reads, so
	// its secondary indexes and statistics triggers are built after the load
	// instead of being maintained row by row (see registry.DeferBulkObjects).
	// A run that replaces an index somebody may be querying keeps them.
	bulk := false
	if ok, err := reg.BulkLoadEligible(ctx); err != nil {
		return err
	} else if ok {
		if n, err := reg.DeferBulkObjects(ctx); err != nil {
			return err
		} else {
			bulk = n > 0
		}
	}

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

	if bulk {
		// Before publishing, so a revision a reader can see always has every
		// index its queries rely on. A failure here aborts the run, and the
		// abort puts back whatever is still set aside.
		if _, err := reg.RestoreDeferred(ctx); err != nil {
			return fmt.Errorf("build the indexes: %w", err)
		}
	}

	if err := reg.CommitRun(ctx, run.ID); err != nil {
		return fmt.Errorf("publish index revision: %w", err)
	}
	committed = true

	// Symbols and parse outcomes are counted from the rows just published,
	// not from the pipeline's running tally.
	//
	// The two disagreed, and the rows were right. The tally counts what the
	// parsers returned for files whose parse succeeded. The index also holds
	// the endpoints, tables, configuration keys and deployment units the
	// analysers found, and the symbols of files that parsed only in part, and
	// it holds one row where a file declared the same thing twice on a line.
	// On Newtonsoft.Json this command printed 6,432 symbols and 43 parse
	// failures while the run it had just stored said 6,745 and 48, and
	// `repocortex status` then printed 6,745. The 48 was 43 files that failed
	// and 5 that parsed in part, which is how they are shown: the same two
	// numbers, from the same rows, that `status` prints.
	//
	// `repocortex resolve` can still lower the symbol count afterwards: it
	// folds an event topic declared in several files into one node, and says
	// how many it merged. This repository's index reads 14,550 and its status
	// 14,549 for that reason, which is not a miscount.
	counts, err := reg.Counts(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("count the published revision: %w", err)
	}

	report := indexReport{
		Project:      cfg.ProjectName,
		IndexRun:     run.ID,
		GitRevision:  gitRev,
		Files:        stats.FilesDiscovered,
		Indexed:      stats.FilesIndexed,
		Skipped:      stats.FilesSkipped,
		Symbols:      counts.Symbols,
		References:   stats.References,
		Imports:      stats.Imports,
		DurationMS:   time.Since(start).Milliseconds(),
		AccuracyMode: accuracy,
		MaxFileBytes: walker.MaxFileSize(),
	}
	if report.Parse, err = reg.ParseSummary(ctx, run.ID); err != nil {
		return fmt.Errorf("summarise index run: %w", err)
	}
	// The same two figures `status` prints, from the same rows.
	report.ParseFailures = report.Parse.Abandoned()
	report.PartialParses = report.Parse.SyntaxErrors + report.Parse.Incomplete + report.Parse.LegacyPartial
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
	_, _ = fmt.Fprintf(out, "  References:    %d (bound by `repocortex resolve`)\n", r.References)
	_, _ = fmt.Fprintf(out, "  Imports:       %d\n", r.Imports)
	// What happened to each file, from the index itself. "Skipped" above is
	// every file no syntax tree was built for; these rows say which of them
	// are still searchable, which were too large, and name the largest.
	writeParseSummary(out, r.Parse, "  ", len("Parse failures:"))
	if r.Digest != "" {
		_, _ = fmt.Fprintf(out, "  Digest:        %s\n", r.Digest)
	}
	_, _ = fmt.Fprintf(out, "  Duration:      %dms\n", r.DurationMS)
	return nil
}

// context helper for Go versions without context.WithoutCancel semantics in
// scope; kept explicit so the abort path is obviously immune to the cancelled
// parent context.

// refreshIndex brings the committed run up to date with the working tree, in
// place, and records which commit its contents now describe.
//
// `index` and `watch` share it. pipeline.Refresh stamps the build and the time
// of the pass; the commit is stamped here because reading it needs git, which
// the pipeline does not depend on. Only the full path stamps a commit at
// BeginRun, because only it creates a run; without this an incrementally
// updated index keeps naming the commit it was FIRST built at, so
// `head_matches_build` reports a mismatch for an index that is in fact current.
// The portal's index bar and the MCP provenance block both read that field,
// and a provenance claim that is wrong is worse than absent.
//
// current.GitRevision is updated to match, so a long-lived caller (watch)
// compares the next pass against what is stored and not against what it read
// at start-up.
func refreshIndex(
	ctx context.Context,
	reg *registry.Registry,
	current *model.IndexRun,
	walker *fs.Walker,
	repoRoot string,
	rehash bool,
) (pipeline.IncrementalStats, error) {
	stats, err := pipeline.Refresh(ctx, reg, current.ID, walker, pipeline.RefreshOptions{
		RunningAs: Version,
		Rehash:    rehash,
		Root:      repoRoot,
	})
	if err != nil {
		return stats, err
	}
	if rev := headRevision(ctx, repoRoot); rev != "" && rev != current.GitRevision {
		if err := reg.StampGitRevision(ctx, current.ID, rev); err != nil {
			return stats, err
		}
		current.GitRevision = rev
	}
	return stats, nil
}

// headRevision reads the commit HEAD names, or "" when this is not a repository.
//
// It reads the ref and nothing else. It used to call Repo.Info, which also
// computes whether the working tree is dirty by comparing every tracked file
// with the git index. Nothing here uses that answer, and on django it was half
// of an update that changed nothing: about 1 s of 2.5 s, spent after the index
// was already up to date.
//
// Extracted so the incremental path can stamp the same value the full path
// records at BeginRun. Warnings are the caller's business: this is used on the
// incremental path where a missing revision is not worth a second warning.
func headRevision(ctx context.Context, root string) string {
	repo, err := acquiregit.Open(root)
	if err != nil {
		return ""
	}
	sha, err := repo.HeadSHA(ctx)
	if err != nil {
		return ""
	}
	return sha
}
