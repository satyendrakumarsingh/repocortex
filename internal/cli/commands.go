package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/autorun"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/gotypes"
	"github.com/satyendrakumarsingh/repocortex/internal/store"

	"github.com/spf13/cobra"
)

func osName() string   { return runtime.GOOS }
func archName() string { return runtime.GOARCH }

func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---------------------------------------------------------------- init

func newInitCmd(g *Global) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the RepoCortex index for this project",
		Long: `Creates .repocortex/ and applies the registry schema.

This does not index anything; run "repocortex index" afterwards.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := g.Config()
			dir := filepath.Join(cfg.RepoRoot, IndexDir)
			primary := filepath.Join(dir, IndexFile)
			alt := filepath.Join(cfg.RepoRoot, ".devintel", IndexFile)

			if _, err := os.Stat(primary); err == nil && !force {
				return fmt.Errorf("index already exists at %s (use --force to recreate)", primary)
			} else if err == nil && force {
				if err := os.Remove(primary); err != nil {
					return fmt.Errorf("remove existing index: %w", err)
				}
				_ = os.Remove(primary + "-wal")
				_ = os.Remove(primary + "-shm")
				g.Log().Warn("removed existing index", "path", primary)
			}

			if _, err := os.Stat(alt); err == nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: found legacy index at %s. Initializing fresh index at %s (safe to delete .devintel/ once migrated).\n", alt, primary)
			}

			// 0o750 — see internal/store: the index maps private source.
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
			// The registry contains a structural map of the codebase and must
			// never be committed.
			if err := writeIfAbsent(filepath.Join(dir, ".gitignore"), "*\n"); err != nil {
				return err
			}

			db, err := store.Open(ctx, store.Options{Path: primary})
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			if err := store.VerifyFTS5(ctx, db.DB); err != nil {
				return err
			}
			applied, err := store.Migrate(ctx, db.DB, time.Now().Unix)
			if err != nil {
				return fmt.Errorf("apply schema: %w", err)
			}
			version, err := store.SchemaVersion(ctx, db.DB)
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"project":            cfg.ProjectName,
					"index_path":         primary,
					"schema_version":     version,
					"migrations_applied": applied,
					"accuracy_mode":      cfg.Accuracy,
				})
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Initialized RepoCortex for %q\n", cfg.ProjectName)
			_, _ = fmt.Fprintf(out, "  Index:          %s\n", primary)
			_, _ = fmt.Fprintf(out, "  Schema version: %d (%d migration(s) applied)\n", version, len(applied))
			_, _ = fmt.Fprintf(out, "  Accuracy mode:  %s\n", cfg.Accuracy)
			_, _ = fmt.Fprintf(out, "\nNext: repocortex index\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "recreate the index if it already exists")
	return cmd
}

func writeIfAbsent(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// ---------------------------------------------------------------- status

func newStatusCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report index state and counts",
		Long: `Reports the current index revision and entity counts.

All figures are read from the registry. Nothing is estimated.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := g.Config()
			dbPath := g.DBPath()

			st := statusReport{
				Project:      cfg.ProjectName,
				IndexPath:    dbPath,
				AccuracyMode: string(cfg.Accuracy),
				MCPProfile:   string(cfg.MCPProfile),
				Capabilities: cfg.Capabilities,
			}

			fi, err := os.Stat(dbPath)
			if os.IsNotExist(err) {
				st.IndexStatus = "absent"
				return renderStatus(cmd, g, st, "Run 'repocortex init' to create the index.")
			} else if err != nil {
				return err
			}
			st.IndexSizeBytes = fi.Size()

			// status is the command someone runs *because* something is wrong,
			// so an out-of-date schema is a thing to report, not a reason to
			// refuse. Open without the version assertion and diagnose below.
			db, err := store.Open(ctx, store.Options{Path: dbPath, ReadOnly: true})
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			if st.SchemaVersion, err = store.SchemaVersion(ctx, db.DB); err != nil {
				return err
			}
			if err := store.CheckVersion(ctx, db.DB); err != nil {
				var newer *store.ErrSchemaNewer
				if errors.As(err, &newer) {
					st.IndexStatus = "newer"
					st.ExpectedSchema = newer.Latest
					return renderStatus(cmd, g, st, newer.Error()+".")
				}
				var outdated *store.ErrSchemaOutdated
				if errors.As(err, &outdated) {
					st.IndexStatus = "outdated"
					st.ExpectedSchema = outdated.Wanted
					return renderStatus(cmd, g, st, fmt.Sprintf(
						"Index schema is v%d but this build expects v%d. Run 'repocortex index' to upgrade.",
						outdated.Found, outdated.Wanted))
				}
				return err
			}

			// An interrupted first index can leave schema objects set aside.
			// Say so before anything else: every figure below was read
			// without them.
			if deferred, err := registry.New(db).DeferredObjects(ctx); err != nil {
				return err
			} else if len(deferred) > 0 {
				for _, o := range deferred {
					st.DeferredSchema = append(st.DeferredSchema, o.Name)
				}
			}

			// Counts come from the committed run only. An in-flight indexing
			// run must never leak partial numbers into status.
			var runID int64
			err = db.QueryRowContext(ctx,
				`SELECT id FROM index_runs WHERE committed = 1`).Scan(&runID)
			switch {
			case err != nil && strings.Contains(err.Error(), "no rows"):
				st.IndexStatus = "empty"
				return renderStatus(cmd, g, st, "Schema is ready but nothing is indexed yet. Run 'repocortex index'."+deferredHint(st))
			case err != nil:
				return err
			}

			// Count live rows rather than reading index_runs' snapshot columns.
			//
			// Those columns are written once, by CommitRun. Anything that
			// changes the graph afterwards — `repocortex resolve` building the
			// call graph, an incremental update adding or removing symbols —
			// leaves them stale, and status reported "Relationships: 0" while
			// 16 edges sat in the table. A status command that contradicts the
			// database is worse than no status command.
			row := db.QueryRowContext(ctx, `
				SELECT r.status, COALESCE(r.git_revision,''),
				       (SELECT count(*) FROM files   WHERE index_run_id = r.id),
				       (SELECT count(*) FROM symbols WHERE index_run_id = r.id AND kind <> 'file'),
				       (SELECT count(*) FROM edges   WHERE index_run_id = r.id),
				       (SELECT count(*) FROM files   WHERE index_run_id = r.id
				                                       AND detected_by = 'extension_ambiguous'),
				       COALESCE(r.finished_unix, 0), r.accuracy_mode
				FROM index_runs r WHERE r.id = ?`, runID)
			var finished int64
			if err := row.Scan(&st.IndexStatus, &st.GitRevision, &st.Files, &st.Symbols,
				&st.Edges, &st.AmbiguousLanguages, &finished, &st.AccuracyMode); err != nil {
				return err
			}
			// The per-file breakdown, read live for the same reason the counts
			// above are: an incremental update changes it and nothing rewrites
			// the run row's snapshot.
			summary, err := parseSummaryFor(ctx, db, runID)
			if err != nil {
				return err
			}
			st.Parse = &summary
			// Whether the Go graph is backed by the compiler's front end,
			// read from the edges themselves so that it cannot disagree with
			// them: an incremental update that drops edges changes this.
			goFiles, checked, edges, err := registry.New(db).GoTypesCoverage(ctx, runID, gotypes.Detail)
			if err != nil {
				return err
			}
			if goFiles > 0 {
				st.GoTypes = &goTypesStatus{GoFiles: goFiles, FilesWithProof: checked, ExactEdges: edges}
			}
			langs, err := registry.New(db).CompilerCoverage(ctx, runID, gotypes.Detail)
			if err != nil {
				return err
			}
			for _, l := range langs {
				if l.Language != "go" && !documentLanguage[l.Language] {
					st.Compiler = append(st.Compiler, l)
				}
			}
			langsWithFiles := make([]indexer.LanguageFiles, 0, len(langs))
			for _, l := range langs {
				langsWithFiles = append(langsWithFiles, indexer.LanguageFiles{Language: l.Language, Files: l.Files})
			}
			st.SuggestedIndexers = indexer.Suggest(ctx, nil, cfg, langsWithFiles)

			// The same number the run row and the portal carry: files whose
			// indexing was abandoned. A gate that reads this field and wants
			// zero is asking the right question.
			st.ParseFailures = summary.Abandoned()
			st.PartialParses = summary.SyntaxErrors + summary.Incomplete + summary.LegacyPartial
			st.IndexRun = runID
			if finished > 0 {
				st.LastIndexed = time.Unix(finished, 0).Format(time.RFC3339)
			}
			st.LastAutoIndex = autorun.Read(filepath.Dir(g.DBPath()))
			return renderStatus(cmd, g, st, strings.TrimPrefix(deferredHint(st), "\n"))
		},
	}
}

// deferredHint explains a status report's DeferredSchema, or returns "".
func deferredHint(st statusReport) string {
	if len(st.DeferredSchema) == 0 {
		return ""
	}
	return fmt.Sprintf("\nA previous index was interrupted while loading and %d schema objects (%s) were not put back. "+
		"Answers can be slow until they are. Run 'repocortex index' to restore them.",
		len(st.DeferredSchema), strings.Join(st.DeferredSchema, ", "))
}

type statusReport struct {
	Project       string `json:"project"`
	IndexPath     string `json:"index_path"`
	IndexStatus   string `json:"index_status"`
	SchemaVersion int    `json:"schema_version"`
	// ExpectedSchema is set only when the index is behind this build, so that
	// a JSON consumer can tell "v4, current" from "v4, needs upgrade to v5".
	ExpectedSchema int    `json:"expected_schema,omitempty"`
	IndexRun       int64  `json:"index_run,omitempty"`
	GitRevision    string `json:"git_revision,omitempty"`
	Files          int64  `json:"files"`
	Symbols        int64  `json:"symbols"`
	Edges          int64  `json:"relationships"`
	ParseFailures  int64  `json:"parse_failures"`
	// ParseFailures (above) is files whose indexing was abandoned: the parse
	// failed, or was stopped before the end of the file. It is what makes an
	// index DEGRADED.
	//
	// PartialParses is files with parse_status partial: read with syntax
	// errors, or stopped before the end. The second kind is in both counts;
	// Parse breaks the figures down.
	PartialParses int64 `json:"partial_parses"`
	// Parse is what happened to every file in the index: clean, syntax
	// errors, incomplete, failed, text only, too large (with the largest
	// named), and skipped for other reasons. Nil when nothing is indexed.
	Parse *registry.ParseSummary `json:"parse,omitempty"`
	// AmbiguousLanguages is files whose language is a guess: the extension is
	// shared by several languages and nothing in the content settled which.
	// Such a file is parsed with the extension's default grammar, so when it
	// yields no symbols the likeliest cause is the wrong grammar, and the fix
	// is an index.language_overrides entry rather than a bug report.
	AmbiguousLanguages int64 `json:"ambiguous_languages"`
	// DeferredSchema names indexes and triggers a first index set aside for
	// its bulk load and never put back, because the run was interrupted. While
	// any are listed, queries may run slowly or report stale coverage figures;
	// 'repocortex index' restores them.
	DeferredSchema []string `json:"deferred_schema,omitempty"`
	LastIndexed    string   `json:"last_indexed,omitempty"`
	// LastAutoIndex is the last background pass a git hook started, and how
	// it ended. A hook's run has no terminal, so a failure is reported here or
	// nowhere. Absent when no automatic pass has run.
	LastAutoIndex  *autorun.Run `json:"last_auto_index,omitempty"`
	IndexSizeBytes int64        `json:"index_size_bytes"`
	AccuracyMode   string       `json:"accuracy_mode"`
	// GoTypes says whether the Go graph has been type-checked. Nil when the
	// index holds no Go.
	GoTypes *goTypesStatus `json:"go_types,omitempty"`
	// Compiler says, per language that has definitions, how many files a
	// compiler read: Go through the built-in type check, the others through
	// the compiler indexer `resolve` ran. Absent languages are heuristic only.
	Compiler []registry.CheckedLanguage `json:"compiler_checked,omitempty"`
	// SuggestedIndexers are compiler indexers that are not installed for a
	// language with many files here, each with the command to get it. Status
	// installs nothing.
	SuggestedIndexers []indexer.Suggestion `json:"suggested_indexers,omitempty"`
	MCPProfile        string               `json:"mcp_profile"`
	Capabilities      []string             `json:"capabilities"`
}

// goTypesStatus is the built-in Go type check's footprint in an index.
type goTypesStatus struct {
	GoFiles        int64 `json:"go_files"`
	FilesWithProof int64 `json:"files_with_proven_edges"`
	ExactEdges     int64 `json:"exact_edges"`
}

func renderStatus(cmd *cobra.Command, g *Global, st statusReport, hint string) error {
	if g.JSON {
		return emitJSON(cmd.OutOrStdout(), st)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "Project:\t%s\n", st.Project)
	_, _ = fmt.Fprintf(w, "Index:\t%s\n", strings.ToUpper(st.IndexStatus))
	if st.SchemaVersion > 0 {
		_, _ = fmt.Fprintf(w, "Schema:\tv%d\n", st.SchemaVersion)
	}
	if st.GitRevision != "" {
		_, _ = fmt.Fprintf(w, "Revision:\t%s\n", st.GitRevision)
	}
	if st.IndexStatus != "absent" && st.IndexStatus != "empty" {
		_, _ = fmt.Fprintf(w, "Files:\t%d\n", st.Files)
		_, _ = fmt.Fprintf(w, "Symbols:\t%d\n", st.Symbols)
		_, _ = fmt.Fprintf(w, "Relationships:\t%d\n", st.Edges)
		if st.Parse != nil {
			for _, l := range parseSummaryLines(*st.Parse) {
				_, _ = fmt.Fprintf(w, "%s:\t%s\n", l.Label, l.Value)
				for _, d := range l.Detail {
					_, _ = fmt.Fprintf(w, "\t  %s\n", d)
				}
			}
		}
		if g := st.GoTypes; g != nil {
			if g.ExactEdges > 0 {
				_, _ = fmt.Fprintf(w, "Go graph:\ttype-checked (go/types): %d exact edge(s) across %d of %d Go file(s)\n",
					g.ExactEdges, g.FilesWithProof, g.GoFiles)
			} else {
				_, _ = fmt.Fprintf(w, "Go graph:\theuristic only (%d Go file(s), no type check on record): run 'repocortex resolve'\n", g.GoFiles)
			}
		}
		for _, l := range st.Compiler {
			if l.Checked > 0 {
				_, _ = fmt.Fprintf(w, "%s graph:\tcompiler-checked (%s): %d exact edge(s) across %d of %d file(s)\n",
					titleCase(l.Language), l.Tool, l.ExactEdges, l.Checked, l.Files)
			} else {
				_, _ = fmt.Fprintf(w, "%s graph:\theuristic only (%d file(s), no compiler index on record)%s\n",
					titleCase(l.Language), l.Files, compilerHint(l.Language))
			}
		}
		if !g.Quiet {
			for _, s := range st.SuggestedIndexers {
				_, _ = fmt.Fprintf(w, "Suggested:\t%s: %d files with definitions, no %s installed. It would add %s. To get it: %s\n",
					s.Language, s.Files, s.Indexer, s.Adds, s.Command)
			}
		}
		if st.AmbiguousLanguages > 0 {
			_, _ = fmt.Fprintf(w, "Ambiguous languages:\t%d  (language guessed from the extension; see index.language_overrides)\n",
				st.AmbiguousLanguages)
		}
		if st.LastIndexed != "" {
			_, _ = fmt.Fprintf(w, "Last indexed:\t%s\n", st.LastIndexed)
		}
		if a := st.LastAutoIndex; a != nil {
			took := (time.Duration(a.DurationMS) * time.Millisecond).Round(100 * time.Millisecond)
			if a.OK {
				_, _ = fmt.Fprintf(w, "Last auto-index:\t%s, ok in %s\n", a.At, took)
			} else {
				_, _ = fmt.Fprintf(w, "Last auto-index:\t%s, FAILED: %s (the index may be behind; see .repocortex/logs/auto-index.log, then run 'repocortex index')\n", a.At, a.Error)
			}
		}
	}
	if st.IndexSizeBytes > 0 {
		_, _ = fmt.Fprintf(w, "Index size:\t%s\n", humanBytes(st.IndexSizeBytes))
	}
	_, _ = fmt.Fprintf(w, "Accuracy mode:\t%s\n", st.AccuracyMode)
	_, _ = fmt.Fprintf(w, "MCP profile:\t%s\n", st.MCPProfile)
	if len(st.Capabilities) > 0 {
		_, _ = fmt.Fprintf(w, "Capabilities:\t%s\n", strings.Join(st.Capabilities, ", "))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if hint != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", hint)
	}
	return nil
}

// documentLanguage are languages whose "definitions" are headings and keys, so
// that a line about whether a compiler checked them would only be noise.
var documentLanguage = map[string]bool{
	"markdown": true, "json": true, "yaml": true, "toml": true, "html": true, "css": true,
	"ini": true, "xml": true, "text": true, "editorconfig": true, "dockerfile": true,
	"hcl": true, "terraform": true, "sql": true, "bash": true, "shell": true,
}

// compilerHint says what would put a compiler behind a language, when
// something could.
func compilerHint(lang string) string {
	switch lang {
	case "typescript", "tsx", "javascript", "jsx", "python", "java", "kotlin", "scala", "c", "cpp", "rust":
		return ": run 'repocortex resolve' with the language's indexer installed (see 'repocortex indexers list')"
	}
	return ": no compiler indexer is wired for it"
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---------------------------------------------------------------- doctor

func newDoctorCmd(g *Global) *cobra.Command {
	var verifyIndex bool
	var precision bool
	var vacuum bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the environment and verify index integrity",
		Long: `Checks that RepoCortex's runtime prerequisites hold, and optionally
validates the graph for structural defects.

--verify-index runs the graph invariant gate: dangling edges, orphaned
symbols, and edges claiming compiler certainty without compiler evidence.

--resolution-precision measures the heuristic resolver against compiler facts
already imported into this index, and reports agreement per confidence tier —
on YOUR repository, not on ours. It needs a run holding both, so import a SCIP
index first:

    repocortex index && repocortex resolve
    scip-go --output index.scip
    repocortex scip import index.scip
    repocortex doctor --resolution-precision

It reports precision, never recall, and it reports the coverage the precision
was computed over: how many heuristic edges were adjudicated out of how many
exist, and how many lines were excluded as ambiguous. A language with heuristic
edges and no imported SCIP index is named as unmeasured rather than left out.

Add --json to gate on it in CI; the report lands under "resolution_precision"
alongside the checks. doctor's exit status is not affected by the measurement —
see docs/accuracy.md for what the numbers do and do not mean.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := g.Config()
			var checks []check

			checks = append(checks, check{
				Name: "go runtime", Status: "ok",
				Detail: fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH),
			})

			// SQLite + FTS5. Search is unusable without FTS5, and modernc does
			// not document that it ships it, so this is a real check.
			mem, err := store.Open(ctx, store.Options{})
			if err != nil {
				checks = append(checks, check{Name: "sqlite", Status: "fail", Detail: err.Error()})
			} else {
				var ver string
				_ = mem.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&ver)
				checks = append(checks, check{Name: "sqlite", Status: "ok", Detail: ver})
				if err := store.VerifyFTS5(ctx, mem.DB); err != nil {
					checks = append(checks, check{Name: "fts5", Status: "fail", Detail: err.Error()})
				} else {
					checks = append(checks, check{Name: "fts5", Status: "ok", Detail: "ENABLE_FTS5 present"})
				}
				_ = mem.Close()
			}

			// Config provenance. Worth surfacing: it tells the user which
			// settings could have come from the repository.
			userPath, _ := config.UserConfigPath()
			ucStatus, ucDetail := "ok", "using defaults (no user config)"
			if cfg.UserConfigPresent {
				ucDetail = userPath
			} else if userPath != "" {
				ucDetail = fmt.Sprintf("absent (%s) — using defaults", userPath)
			}
			checks = append(checks, check{Name: "user config", Status: ucStatus, Detail: ucDetail})

			repoCfg := cfg.RepoConfigFile
			if repoCfg == "" {
				repoCfg = filepath.Join(cfg.RepoRoot, config.RepoConfigName)
			}
			rcDetail := "none"
			if _, err := os.Stat(repoCfg); err == nil {
				rcDetail = repoCfg + " (indexing settings only; cannot grant execution)"
			}
			checks = append(checks, check{Name: "repo config", Status: "ok", Detail: rcDetail})

			// Index presence.
			var prec *precisionReport
			dbPath := g.DBPath()
			if _, err := os.Stat(dbPath); os.IsNotExist(err) {
				checks = append(checks, check{Name: "index", Status: "warn", Detail: "absent — run 'repocortex init'"})
			} else {
				if bad := indexHealthCheck(cmd, dbPath); bad != nil {
					// A file that cannot be read, or was made by a newer
					// build, has nothing further to examine.
					checks = append(checks, *bad)
					return renderChecks(cmd, g, checks, nil)
				}
				checks = append(checks, check{Name: "index", Status: "ok", Detail: dbPath})
				checks = append(checks, deferredSchemaChecks(cmd, dbPath)...)
				checks = append(checks, parseChecks(cmd, dbPath)...)
				if verifyIndex {
					checks = append(checks, verifyGraph(cmd, dbPath)...)
				}
				if precision {
					prec, err = collectPrecision(cmd, dbPath)
					if err != nil {
						return err
					}
				}
				if vacuum {
					rw, err := store.Open(ctx, store.Options{Path: dbPath})
					if err != nil {
						checks = append(checks, check{Name: "vacuum", Status: "fail", Detail: err.Error()})
					} else {
						var beforeSize int64
						if fi, err := os.Stat(dbPath); err == nil {
							beforeSize = fi.Size()
						}
						_, vErr := rw.ExecContext(ctx, "VACUUM")
						_ = rw.Close()
						if vErr != nil {
							checks = append(checks, check{Name: "vacuum", Status: "fail", Detail: vErr.Error()})
						} else {
							var afterSize int64
							if fi, err := os.Stat(dbPath); err == nil {
								afterSize = fi.Size()
							}
							saved := beforeSize - afterSize
							detail := fmt.Sprintf("reclaimed %s (now %s)", humanBytes(saved), humanBytes(afterSize))
							if saved <= 0 {
								detail = fmt.Sprintf("already compact (%s)", humanBytes(afterSize))
							}
							checks = append(checks, check{Name: "vacuum", Status: "ok", Detail: detail})
						}
					}
				} else {
					dbRO, err := store.Open(ctx, store.Options{Path: dbPath, ReadOnly: true})
					if err == nil {
						var freelist, pageSize int64
						_ = dbRO.DB.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freelist)
						_ = dbRO.DB.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize)
						_ = dbRO.Close()
						if freelist > 0 {
							fragBytes := freelist * pageSize
							if fragBytes > 2<<20 {
								checks = append(checks, check{
									Name:   "storage",
									Status: "warn",
									Detail: fmt.Sprintf("%s in %d freelist pages — run 'repocortex doctor --vacuum' to reclaim", humanBytes(fragBytes), freelist),
								})
							}
						}
					}
				}
			}

			return renderChecks(cmd, g, checks, prec)
		},
	}
	cmd.Flags().BoolVar(&verifyIndex, "verify-index", false, "run graph invariant checks")
	cmd.Flags().BoolVar(&precision, "resolution-precision", false,
		"measure the heuristic resolver against imported compiler facts")
	cmd.Flags().BoolVar(&vacuum, "vacuum", false, "reclaim freelist pages and defragment index database")
	return cmd
}

// indexHealthCheck opens the index read-only and returns a failing check when
// SQLite cannot read it or it was made by a newer build, nil when it opens.
func indexHealthCheck(cmd *cobra.Command, dbPath string) *check {
	ctx := cmd.Context()
	db, err := store.Open(ctx, store.Options{Path: dbPath, ReadOnly: true})
	if err == nil {
		defer func() { _ = db.Close() }()
		err = store.CheckVersion(ctx, db.DB)
	}
	var outdated *store.ErrSchemaOutdated
	if err == nil || errors.As(err, &outdated) {
		return nil
	}
	err = store.ExplainCorrupt(err)
	c := check{Name: "index", Status: "fail", Detail: err.Error()}
	var corrupt *store.ErrCorrupt
	var newer *store.ErrSchemaNewer
	switch {
	case errors.As(err, &corrupt):
		c.Detail = dbPath + " is corrupt: " + corrupt.Cause.Error()
		c.Remedy = "run 'repocortex init --force', then 'repocortex index', to rebuild it"
	case errors.As(err, &newer):
		c.Remedy = "upgrade repocortex, or rebuild with 'repocortex init --force' then 'repocortex index'"
	}
	return &c
}

// deferredSchemaChecks reports indexes and triggers that an interrupted first
// index set aside and did not put back. Nothing is reported when none are.
func deferredSchemaChecks(cmd *cobra.Command, dbPath string) []check {
	ctx := cmd.Context()
	db, err := store.Open(ctx, store.Options{Path: dbPath, ReadOnly: true})
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	deferred, err := registry.New(db).DeferredObjects(ctx)
	if err != nil {
		return []check{{Name: "schema objects", Status: "warn", Detail: err.Error()}}
	}
	if len(deferred) == 0 {
		return nil
	}
	names := make([]string, 0, len(deferred))
	for _, o := range deferred {
		names = append(names, o.Name)
	}
	return []check{{
		Name: "schema objects", Status: "warn",
		Detail: fmt.Sprintf("%d missing: %s", len(names), strings.Join(names, ", ")),
		Impact: "an interrupted first index set these aside to load faster; until they are back, " +
			"queries can be slow and stored coverage figures can be stale",
		Remedy: "run 'repocortex index'; it restores them before doing anything else",
	}}
}

// parseChecks reports, as doctor checks, the files the index does not fully
// hold: abandoned parses, files over the size limit, and rows written under an
// older meaning of the parse status.
//
// A check that is "ok" is still printed with its numbers. An index nobody has
// looked at is the usual one, and "syntax errors: 121 files" beside an ok is
// the line that sends a reader to look.
func parseChecks(cmd *cobra.Command, dbPath string) []check {
	ctx := cmd.Context()
	db, err := store.Open(ctx, store.Options{Path: dbPath, ReadOnly: true})
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()

	var runID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM index_runs WHERE committed = 1`).Scan(&runID); err != nil {
		return nil
	}
	s, err := parseSummaryFor(ctx, db, runID)
	if err != nil {
		return []check{{Name: "parse status", Status: "warn", Detail: err.Error()}}
	}

	parsed := check{
		Name: "parse status", Status: "ok",
		// The same split `index` prints: files a tree was built for, and
		// files it was not (text only, too large, no grammar). Counting the
		// skipped ones in the total made it disagree with `index`.
		Detail: fmt.Sprintf("%d files indexed, %d skipped (%d discovered): %d clean, %d with syntax errors, %d incomplete, %d failed, %d text only",
			s.Parsed(), s.Files-s.Parsed()-s.Failed, s.Files,
			s.Clean, s.SyntaxErrors, s.Incomplete, s.Failed, s.TextOnly),
	}
	if s.Abandoned() > 0 {
		parsed.Status = "warn"
		parsed.Impact = "definitions in these files, and every edge through them, are missing from answers"
		parsed.Remedy = "read the reason beside each file; a file stopped on a limit can be excluded or split"
		parsed.Examples = append(noteLines(s.FailedFiles, s.Failed, true),
			noteLines(s.IncompleteFiles, s.Incomplete, true)...)
	}
	out := []check{parsed}

	if s.TooLarge > 0 {
		out = append(out, check{
			Name: "size limit", Status: "warn",
			Detail: fmt.Sprintf("%d %s over the size limit %s not in the index",
				s.TooLarge, files(s.TooLarge), isAre(s.TooLarge)),
			Impact:   "nothing in these files can be found, by name or by text",
			Remedy:   "raise index.max_file_bytes in " + config.RepoConfigName + ", or leave generated files out",
			Examples: noteLines(s.LargestTooLarge, s.TooLarge, false),
		})
	}
	if s.LegacyPartial > 0 {
		out = append(out, check{
			Name: "parse status age", Status: "warn",
			Detail: fmt.Sprintf("%d %s carry a status written by an older version, where \"partial\" meant "+
				"\"no definitions found\" and syntax errors were not recorded",
				s.LegacyPartial, files(s.LegacyPartial)),
			Remedy: "run 'repocortex index --full' to rewrite them",
		})
	}
	return out
}

// ------------------------------------------------- resolution precision
//
// This is the only place the product tells a user how accurate it is on THEIR
// repository, so the report is held to three rules:
//
//  1. Every precision figure is printed next to the denominator it came from.
//     A tier percentage without its judged count, and a judged count without
//     the heuristic edges that were never adjudicated at all, both invite the
//     reader to generalise a number over cases it never covered.
//  2. It never becomes a pass/fail gate here. `--json` exists so a CI job can
//     set its own threshold; doctor's exit status stays a function of the
//     invariant checks, because the moment the measurement gates the build,
//     somebody tunes the threshold instead of the resolver.
//  3. A language nobody measured is named. Reporting only the languages that
//     had a SCIP index imported would let an unmeasured language read as a
//     passing one, which is the one failure mode a trust report cannot have.
//
// docs/accuracy.md is the method and the caveats. Nothing printed here may
// claim more than that document does.

// precisionTier is one confidence tier's agreement with the compiler, and the
// verdict the project attaches to that tier.
type precisionTier struct {
	Confidence string `json:"confidence"`
	// Verdict is the reading rule: act on / show / ignore.
	Verdict  string `json:"verdict"`
	Judged   int64  `json:"judged"`
	Agree    int64  `json:"agree"`
	Disagree int64  `json:"disagree"`
	// Precision is Agree/Judged in 0..1, and is meaningless when Judged is 0 —
	// hence Measured, so a consumer cannot read an unmeasured tier as 0%.
	Precision float64 `json:"precision"`
	Measured  bool    `json:"measured"`
}

// precisionCoverage is what the percentages were computed over.
type precisionCoverage struct {
	HeuristicEdges int64 `json:"heuristic_edges"`
	JudgedEdges    int64 `json:"judged_edges"`
	// UnjudgedEdges is heuristic edges no compiler fact could adjudicate. They
	// are neither right nor wrong in this report.
	UnjudgedEdges  int64   `json:"unjudged_edges"`
	JudgedFraction float64 `json:"judged_fraction"`
	CompilerFacts  int64   `json:"compiler_facts"`
	JudgeableLines int64   `json:"judgeable_lines"`
	AmbiguousLines int64   `json:"ambiguous_lines"`
}

// precisionLanguage is one language's standing in the measurement.
type precisionLanguage struct {
	Language       string `json:"language"`
	Files          int64  `json:"files"`
	HeuristicEdges int64  `json:"heuristic_edges"`
	CompilerFacts  int64  `json:"compiler_facts"`
	// Measured is false when the language has heuristic edges but no compiler
	// facts to judge them against.
	Measured bool `json:"measured"`
}

// precisionReport is the CLI's own shape for the comparison.
//
// Declared here rather than emitting registry.PrecisionReport directly: the
// registry type is a query result and may grow fields as the query changes,
// and a CI job gating on this output needs a shape that is stable because
// somebody promised it would be.
type precisionReport struct {
	// Measured is false when no compiler facts are present at all; every other
	// field is then empty and the report is a statement that nothing is known.
	Measured bool `json:"measured"`
	// Measures is always "precision". See Caveats.
	Measures    string              `json:"measures"`
	ReadingRule string              `json:"reading_rule"`
	Tiers       []precisionTier     `json:"tiers"`
	All         precisionTier       `json:"all"`
	Coverage    precisionCoverage   `json:"coverage"`
	Languages   []precisionLanguage `json:"languages"`
	// Unmeasured names the languages carrying heuristic edges that no compiler
	// index adjudicated. Unmeasured is not the same as accurate.
	Unmeasured []string `json:"unmeasured_languages"`
	Warnings   []string `json:"warnings"`
	Caveats    []string `json:"caveats"`
	Reference  string   `json:"reference"`
}

// tierVerdicts is the reading rule docs/accuracy.md states: act on `high`,
// show `medium`, ignore `low`. It is fixed policy, not a function of the
// numbers below — the numbers say whether this repository's tiers behave the
// way the measured ones did.
var tierVerdicts = []struct{ confidence, verdict string }{
	{"high", "act on"},
	{"medium", "show"},
	{"low", "ignore"},
}

// notRecall is the caveat that outlives every reformatting of this report.
const notRecall = "This is precision, not recall: the compiler records references, not calls, " +
	"so its edge set is a superset by construction."

// collectPrecision reads the comparison and the per-language coverage.
func collectPrecision(cmd *cobra.Command, dbPath string) (*precisionReport, error) {
	ctx := cmd.Context()
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	reg := registry.New(db)
	run, err := reg.CurrentRun(ctx)
	if errors.Is(err, registry.ErrNoCommittedRun) {
		// Rewritten for the same reason `repocortex scip import` rewrites it: the
		// registry's message describes the store, not the next step.
		return nil, errors.New("no committed index to measure — run " +
			"'repocortex index && repocortex resolve' first")
	}
	if err != nil {
		return nil, fmt.Errorf("resolution precision: %w", err)
	}
	raw, err := reg.ResolutionPrecision(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	langs, err := languageCoverage(ctx, db.DB, run.ID)
	if err != nil {
		return nil, err
	}
	rep := buildPrecisionReport(raw, langs)
	return &rep, nil
}

// languageCoverage counts, per language, the edges that would be judged and
// the compiler facts available to judge them.
//
// Attribution is by the file the edge's SOURCE symbol lives in — the call
// site. evidence_file would do as well where it is populated, but every edge
// has a source symbol and every symbol has a file, so this cannot silently
// drop a language.
func languageCoverage(ctx context.Context, db *sql.DB, runID int64) ([]precisionLanguage, error) {
	byLang := map[string]*precisionLanguage{}
	get := func(lang string) *precisionLanguage {
		if l, ok := byLang[lang]; ok {
			return l
		}
		l := &precisionLanguage{Language: lang}
		byLang[lang] = l
		return l
	}

	files, err := db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(language, ''), 'unknown') AS lang, COUNT(*)
		  FROM files WHERE index_run_id = ? GROUP BY lang`, runID)
	if err != nil {
		return nil, fmt.Errorf("language coverage: count files: %w", err)
	}
	defer func() { _ = files.Close() }()
	for files.Next() {
		var lang string
		var n int64
		if err := files.Scan(&lang, &n); err != nil {
			return nil, fmt.Errorf("language coverage: count files: %w", err)
		}
		get(lang).Files = n
	}
	if err := files.Err(); err != nil {
		return nil, fmt.Errorf("language coverage: count files: %w", err)
	}

	// The two source predicates mirror registry.ResolutionPrecision exactly: a
	// language column that counted a different edge set than the one being
	// adjudicated would be worse than no column.
	edges, err := db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(f.language, ''), 'unknown') AS lang,
		       SUM(CASE WHEN e.source = 'source_code' AND e.kind IN ('CALLS', 'INSTANTIATES') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN e.source = 'compiler' THEN 1 ELSE 0 END)
		  FROM edges e
		  JOIN symbols s ON s.id = e.src_id
		  JOIN files f ON f.id = s.file_id
		 WHERE e.index_run_id = ?
		 GROUP BY lang`, runID)
	if err != nil {
		return nil, fmt.Errorf("language coverage: count edges: %w", err)
	}
	defer func() { _ = edges.Close() }()
	for edges.Next() {
		var lang string
		var heuristic, compiler int64
		if err := edges.Scan(&lang, &heuristic, &compiler); err != nil {
			return nil, fmt.Errorf("language coverage: count edges: %w", err)
		}
		l := get(lang)
		l.HeuristicEdges, l.CompilerFacts = heuristic, compiler
	}
	if err := edges.Err(); err != nil {
		return nil, fmt.Errorf("language coverage: count edges: %w", err)
	}

	// Only languages with something at stake. A repository's YAML and Markdown
	// files carry no call edges, so listing them as "unmeasured" would bury the
	// one language that genuinely is.
	out := make([]precisionLanguage, 0, len(byLang))
	for _, l := range byLang {
		if l.HeuristicEdges == 0 && l.CompilerFacts == 0 {
			continue
		}
		l.Measured = l.CompilerFacts > 0
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HeuristicEdges != out[j].HeuristicEdges {
			return out[i].HeuristicEdges > out[j].HeuristicEdges
		}
		return out[i].Language < out[j].Language
	})
	return out, nil
}

// buildPrecisionReport turns the raw comparison into what a reader is asked to
// act on. Slices are always non-nil: a CI job parsing this should iterate, not
// nil-check.
func buildPrecisionReport(raw registry.PrecisionReport, langs []precisionLanguage) precisionReport {
	rep := precisionReport{
		Measured:  raw.CompilerEdges > 0,
		Measures:  "precision",
		Tiers:     []precisionTier{},
		Languages: langs,
		Unmeasured: func() []string {
			out := []string{}
			for _, l := range langs {
				if !l.Measured {
					out = append(out, l.Language)
				}
			}
			return out
		}(),
		Warnings:  []string{},
		Caveats:   []string{notRecall},
		Reference: "docs/accuracy.md",
	}
	if rep.Languages == nil {
		rep.Languages = []precisionLanguage{}
	}

	byConfidence := map[string]registry.PrecisionRow{}
	for _, r := range raw.Rows {
		byConfidence[r.Confidence] = r
	}
	// Emitted in reading-rule order, not the query's alphabetical one, so the
	// tier a reader is told to act on is the first one they meet.
	for _, tv := range tierVerdicts {
		r := byConfidence[tv.confidence]
		rep.Tiers = append(rep.Tiers, precisionTier{
			Confidence: tv.confidence,
			Verdict:    tv.verdict,
			Judged:     r.Judged,
			Agree:      r.Agree,
			Disagree:   r.Disagree,
			Precision:  r.Precision(),
			Measured:   r.Judged > 0,
		})
		delete(byConfidence, tv.confidence)
	}
	// Any tier the resolver grew that this table does not know about still gets
	// reported; silence would be the wrong default for a trust report.
	for _, r := range raw.Rows {
		if _, ok := byConfidence[r.Confidence]; !ok {
			continue
		}
		rep.Tiers = append(rep.Tiers, precisionTier{
			Confidence: r.Confidence, Verdict: "unclassified",
			Judged: r.Judged, Agree: r.Agree, Disagree: r.Disagree,
			Precision: r.Precision(), Measured: r.Judged > 0,
		})
	}

	var judged, agree int64
	for _, t := range rep.Tiers {
		judged += t.Judged
		agree += t.Agree
	}
	rep.All = precisionTier{
		Confidence: "all", Verdict: "—",
		Judged: judged, Agree: agree, Disagree: judged - agree,
		Measured: judged > 0,
	}
	if judged > 0 {
		rep.All.Precision = float64(agree) / float64(judged)
	}

	rep.Coverage = precisionCoverage{
		HeuristicEdges: raw.HeuristicEdges,
		JudgedEdges:    judged,
		UnjudgedEdges:  raw.HeuristicEdges - judged,
		CompilerFacts:  raw.CompilerEdges,
		JudgeableLines: raw.JudgeableLines,
		AmbiguousLines: raw.AmbiguousLines,
	}
	if raw.HeuristicEdges > 0 {
		rep.Coverage.JudgedFraction = float64(judged) / float64(raw.HeuristicEdges)
	}

	rep.ReadingRule = "act on `high`, show `medium`, ignore `low`"
	rep.Warnings = precisionWarnings(rep)
	return rep
}

// precisionWarnings names the conditions under which the table above should
// not be read as a clean bill of health.
func precisionWarnings(rep precisionReport) []string {
	out := []string{}
	if !rep.Measured {
		return out
	}
	if len(rep.Unmeasured) > 0 {
		out = append(out, fmt.Sprintf(
			"%s carries heuristic edges that no compiler index in this run could judge. "+
				"Those edges are UNMEASURED here, which is not the same as accurate — "+
				"import a SCIP index for it before reading this report as covering them.",
			strings.Join(rep.Unmeasured, ", ")))
	}
	if rep.All.Judged == 0 {
		out = append(out, "No heuristic CALLS edge could be adjudicated: the compiler facts "+
			"and the resolver's edges share no unambiguous line. Nothing here is a "+
			"statement about accuracy.")
		return out
	}
	if rep.All.Judged < precisionSampleFloor {
		out = append(out, fmt.Sprintf(
			"Only %s edge(s) were judged. The percentages are arithmetic on a small "+
				"sample and will move materially with a few edges.",
			humanCount(rep.All.Judged)))
	}
	// docs/accuracy.md reports `high` between 86.8% and 96.1% across the five
	// repositories measured there, and rests "act on high" on that consistency.
	// A `high` tier materially under it is not behaving like the ones measured,
	// and the reading rule would be borrowing trust it has not earned here.
	for _, t := range rep.Tiers {
		if t.Confidence == "high" && t.Measured && t.Precision < highTierFloor {
			out = append(out, fmt.Sprintf(
				"`high` is %.1f%% here, materially below the 86.8–96.1%% docs/accuracy.md "+
					"reports across the repositories measured there. Treat 'act on high' as "+
					"unvalidated on this repository until that is explained.",
				100*t.Precision))
		}
	}
	return out
}

// precisionSampleFloor is where a percentage stops surviving a handful of
// edges moving. Not a threshold anything passes or fails — it only decides
// whether the reader is warned to distrust the decimal point.
const precisionSampleFloor = 200

// highTierFloor is where `high` stops resembling the tier docs/accuracy.md
// measured, as a fraction.
//
// Deliberately well under the bottom of that document's range (86.8% on
// prometheus) rather than at it. A repository landing a point or two below the
// lowest measured value is inside the spread of measuring a different codebase
// — today's prometheus run reads 86.7% — and a warning that fires on the
// project's own headline number is one every reader learns to skip. This
// catches a tier that has stopped meaning what the label says, not one at the
// edge of the band.
const highTierFloor = 0.80

// renderPrecision writes the human report.
func renderPrecision(out io.Writer, rep *precisionReport) error {
	_, _ = fmt.Fprintf(out, "\nRESOLUTION PRECISION\n")

	if !rep.Measured {
		_, _ = fmt.Fprintf(out,
			"Not measured. This index holds no compiler facts, so nothing here has been\n"+
				"checked against a compiler — including every language listed below.\n"+
				"Run 'repocortex scip import <index.scip>' first; see 'repocortex doctor --help'.\n")
		renderPrecisionLanguages(out, rep)
		return nil
	}

	// Warnings first. A caveat printed under the table is a caveat read after
	// the number it qualifies, which is too late.
	for _, warn := range rep.Warnings {
		_, _ = fmt.Fprintf(out, "%s\n", wrapIndented("  ! ", "    ", warn))
	}
	if len(rep.Warnings) > 0 {
		_, _ = fmt.Fprintln(out)
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  TIER\tVERDICT\tJUDGED\tAGREE\tDISAGREE\tPRECISION")
	for _, t := range append(append([]precisionTier{}, rep.Tiers...), rep.All) {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			t.Confidence, t.Verdict,
			humanCount(t.Judged), humanCount(t.Agree), humanCount(t.Disagree),
			precisionCell(t))
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("render precision table: %w", err)
	}

	// The reading rule, tied to the numbers immediately above it rather than to
	// the ones in the documentation. A user acts on their repository.
	high, medium, low := rep.tier("high"), rep.tier("medium"), rep.tier("low")
	_, _ = fmt.Fprintf(out, "\nHOW TO READ IT\n")
	_, _ = fmt.Fprintf(out, "  Act on `high`, show `medium`, ignore `low`.\n")
	_, _ = fmt.Fprintf(out, "  On THIS repository `high` is %s and `medium` is %s. "+
		"An agent consuming every\n  edge equally gets %s; one filtering "+
		"--min-confidence high gets %s.\n",
		precisionCell(high), precisionCell(medium), precisionCell(rep.All), precisionCell(high))
	if low.Measured {
		_, _ = fmt.Fprintf(out, "  `low` is %s: a hint that a name exists, nothing more.\n",
			precisionCell(low))
	}

	c := rep.Coverage
	_, _ = fmt.Fprintf(out, "\nCOVERAGE\n")
	_, _ = fmt.Fprintf(out, "  Judged %s of %s heuristic CALLS edge(s) (%.1f%%); "+
		"%s were not adjudicated\n  at all and are neither right nor wrong above.\n",
		humanCount(c.JudgedEdges), humanCount(c.HeuristicEdges),
		100*c.JudgedFraction, humanCount(c.UnjudgedEdges))
	_, _ = fmt.Fprintf(out, "  Against %s compiler fact(s), on the %s line(s) the compiler "+
		"resolved to\n  exactly one target; %s ambiguous line(s) were excluded.\n",
		humanCount(c.CompilerFacts), humanCount(c.JudgeableLines), humanCount(c.AmbiguousLines))

	renderPrecisionLanguages(out, rep)

	// Deliberately NOT wrapped: this is the sentence that keeps the whole
	// report honest, and a caveat somebody greps for must be greppable.
	_, _ = fmt.Fprintf(out, "\nWHAT THIS IS NOT\n")
	for _, caveat := range rep.Caveats {
		_, _ = fmt.Fprintf(out, "  %s\n", caveat)
	}
	_, _ = fmt.Fprintf(out, "  The method, and the caveats this summary does not repeat, "+
		"are in %s.\n", rep.Reference)
	return nil
}

// wrapWidth is where lines fold. Narrow enough to survive a split terminal,
// which is where an agent's output is usually read.
const wrapWidth = 78

// wrapIndented folds text to wrapWidth, prefixing the first line and indenting
// the rest. It never breaks a word, so a long path stays copyable.
func wrapIndented(prefix, indent, text string) string {
	var b strings.Builder
	line := prefix
	col := len([]rune(prefix))
	for i, word := range strings.Fields(text) {
		n := len([]rune(word))
		if i > 0 && col+1+n > wrapWidth {
			b.WriteString(line)
			b.WriteString("\n")
			line, col = indent, len([]rune(indent))
		} else if i > 0 {
			line += " "
			col++
		}
		line += word
		col += n
	}
	b.WriteString(line)
	return b.String()
}

func renderPrecisionLanguages(out io.Writer, rep *precisionReport) {
	if len(rep.Languages) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\nLANGUAGES\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  LANGUAGE\tFILES\tCALLS EDGES\tCOMPILER FACTS\tSTATUS")
	for _, l := range rep.Languages {
		status := "measured"
		if !l.Measured {
			status = "NOT MEASURED — no SCIP index imported for it"
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
			l.Language, humanCount(l.Files), humanCount(l.HeuristicEdges),
			humanCount(l.CompilerFacts), status)
	}
	_ = w.Flush()
}

// tier returns one tier, or a zero (unmeasured) tier if absent.
func (r *precisionReport) tier(confidence string) precisionTier {
	for _, t := range r.Tiers {
		if t.Confidence == confidence {
			return t
		}
	}
	return precisionTier{Confidence: confidence}
}

// precisionCell prints a percentage, or a dash when nothing was judged.
//
// An unmeasured tier printed as "0.0%" reads as "measured, and wrong", which
// is the opposite of what it means.
func precisionCell(t precisionTier) string {
	if !t.Measured {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", 100*t.Precision)
}

// humanCount groups digits so five- and six-figure counts can be compared at a
// glance — this report's numbers are routinely both.
func humanCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, d := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`

	// Impact says what the finding does to the ANSWERS, and Remedy what the
	// reader can do about it. A count on its own is not actionable: nobody
	// running this owns the extractor, so "47 duplicate declarations" without
	// these two lines is an invitation to stop trusting the tool rather than
	// to do anything.
	Impact string `json:"impact,omitempty"`
	Remedy string `json:"remedy,omitempty"`

	// Examples locate the finding. Whether it sits in vendored code or in the
	// module someone is about to change decides whether it matters at all,
	// and only the reader can judge that.
	Examples []string `json:"examples,omitempty"`
}

func verifyGraph(cmd *cobra.Command, dbPath string) []check {
	ctx := cmd.Context()
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return []check{{Name: "graph", Status: "fail", Detail: err.Error()}}
	}
	defer func() { _ = db.Close() }()

	invariants := []struct {
		name     string
		desc     string
		sql      string
		impact   string
		remedy   string
		examples string
	}{
		{
			name: "dangling edges",
			desc: "edges pointing at symbols that do not exist",
			sql: `SELECT count(*) FROM edges e
			 WHERE NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = e.src_id)
			    OR NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = e.dst_id)`,
			impact: "graph queries may return relationships whose ends cannot be shown",
			remedy: "re-run 'repocortex index --full && repocortex resolve'; report it if it comes back",
		},
		{
			name: "orphaned symbols",
			desc: "symbols whose file is gone",
			sql: `SELECT count(*) FROM symbols s
			 WHERE NOT EXISTS (SELECT 1 FROM files f WHERE f.id = s.file_id)`,
			impact: "search and symbol lookup can return locations that no longer exist",
			remedy: "re-run 'repocortex index --full'; report it if it comes back",
		},
		{
			name:   "unproven exact edges",
			desc:   "edges claiming compiler certainty without compiler evidence",
			sql:    `SELECT count(*) FROM edges WHERE confidence = 'exact' AND source <> 'compiler'`,
			impact: "a heuristic guess is being presented as compiler-proven fact",
			remedy: "do not act on 'exact' results from this index; report it — this should be impossible",
		},
		{
			name: "cross-run edges",
			desc: "edges joining symbols from different index runs",
			sql: `SELECT count(*) FROM edges e
			 JOIN symbols a ON a.id = e.src_id
			 JOIN symbols b ON b.id = e.dst_id
			 WHERE a.index_run_id <> e.index_run_id OR b.index_run_id <> e.index_run_id`,
			impact: "relationships may mix code from two different revisions",
			remedy: "re-run 'repocortex index --full && repocortex resolve'; report it if it comes back",
		},
		{
			name: "uncommitted visible runs",
			desc: "more than one run marked committed",
			sql: `SELECT CASE WHEN count(*) > 1 THEN count(*) ELSE 0 END
			 FROM index_runs WHERE committed = 1`,
			impact: "queries may read a half-finished revision",
			remedy: "re-run 'repocortex index --full'; report it if it comes back",
		},
		{
			// Double extraction. Two declarations that agree on file, qualified
			// name and KIND within a few lines are one declaration counted
			// twice, not two things a reader could tell apart: a decorated
			// Python function was captured at both the decorator and the `def`,
			// 4,224 times on one service.
			//
			// The discriminator is the END line, not proximity. Two captures
			// of ONE declaration cover the same code and therefore end
			// together: a decorated Python function starts at the decorator
			// (14) and at the `def` (15) but both end at 18. Two DISTINCT
			// declarations end apart, however close they start — Go's
			// generated protobuf writes
			//
			//   func (*CountInt) isHistogram_Count()   {}   // 480 -> 480
			//   func (*CountFloat) isHistogram_Count() {}   // 481 -> 481
			//
			// which share a name and a file and are one line apart, and are
			// two real methods on two receivers. An earlier version of this
			// check keyed on proximity and reported 69 of those on prometheus
			// the first time the corpus gate ran against it.
			name: "duplicate declarations",
			desc: "one declaration extracted more than once",
			sql: `SELECT count(*) FROM symbols a JOIN symbols b
			   ON b.file_id = a.file_id AND b.fqn = a.fqn AND b.kind = a.kind
			  AND b.end_line = a.end_line AND b.start_line > a.start_line
			  AND b.index_run_id = a.index_run_id`,
			impact: "symbol counts are inflated and a context pack may spend budget saying the same thing twice; callers and references are unaffected",
			remedy: "exclude the affected paths under index.exclude in .repocortex.yml if they are vendored, otherwise report the language and one example line",
			examples: `SELECT f.path || ':' || a.start_line || ' (' || a.name || ')'
			   FROM symbols a JOIN symbols b
			     ON b.file_id = a.file_id AND b.fqn = a.fqn AND b.kind = a.kind
			    AND b.end_line = a.end_line AND b.start_line > a.start_line
			    AND b.index_run_id = a.index_run_id
			   JOIN files f ON f.id = a.file_id LIMIT 3`,
		},
		{
			// A name nothing can refer to is a row that can only be noise.
			// Go's blank identifier is a definition to a tags query, and a real
			// service contributed 46 of them.
			name:   "unreferenceable symbols",
			desc:   "symbols named `_` or nothing, which no reference can bind to",
			sql:    `SELECT count(*) FROM symbols WHERE name = '_' OR trim(name) = ''`,
			impact: "symbol search returns rows nobody can use; nothing else is affected",
			remedy: "harmless to ignore; report the language so the extractor can drop them",
			examples: `SELECT f.path || ':' || s.start_line FROM symbols s
			   JOIN files f ON f.id = s.file_id
			  WHERE s.name = '_' OR trim(s.name) = '' LIMIT 3`,
		},
		{
			// A table name that is not an identifier did not come from SQL. A
			// Vue template's `class="truncate flex-1"` reads as TRUNCATE of a
			// table called `flex-1`, and an invented table costs more trust
			// than a missed one earns.
			name: "implausible table names",
			desc: "table names containing characters no SQL identifier can hold",
			sql: `SELECT count(*) FROM table_accesses
			  WHERE table_name GLOB '*[^A-Za-z0-9_.$ ]*'`,
			impact: "'repocortex db' lists tables your schema does not have; real tables are still correct",
			remedy: "ignore those rows, and report the file and line below — a string in that file is being read as SQL",
			examples: `SELECT f.path || ':' || t.start_line || ' (' || t.table_name || ')'
			   FROM table_accesses t JOIN files f ON f.id = t.file_id
			  WHERE t.table_name GLOB '*[^A-Za-z0-9_.$ ]*' LIMIT 3`,
		},
		{
			// A configuration key of pure punctuation came from a comment, not
			// a call site.
			name:   "implausible config keys",
			desc:   "configuration keys with no letter or digit in them",
			sql:    `SELECT count(*) FROM config_usages WHERE key_name NOT GLOB '*[A-Za-z0-9]*'`,
			impact: "'repocortex config' lists keys your service does not read; real keys are still correct",
			remedy: "ignore those rows, and report the file and line below",
			examples: `SELECT f.path || ':' || c.start_line || ' (' || c.key_name || ')'
			   FROM config_usages c JOIN files f ON f.id = c.file_id
			  WHERE c.key_name NOT GLOB '*[A-Za-z0-9]*' LIMIT 3`,
		},
		{
			// This state is why `repocortex arch` once reported "No violations"
			// on a repo that had one: the reference had been silently orphaned
			// by an edit to the definition it pointed at, and resolve skips
			// anything not marked pending, so the call edge never came back.
			name: "orphaned resolutions",
			desc: "references marked resolved that point at no symbol",
			sql: `SELECT count(*) FROM symbol_references
			 WHERE resolution = 'resolved' AND resolved_symbol_id IS NULL`,
			impact: "callers, references and arch may under-report: edges that should exist are missing",
			remedy: "run 'repocortex resolve' — it repairs this state; report it if the count survives",
		},
	}

	out := make([]check, 0, len(invariants))
	for _, inv := range invariants {
		var n int64
		if err := db.QueryRowContext(ctx, inv.sql).Scan(&n); err != nil {
			out = append(out, check{Name: inv.name, Status: "fail", Detail: err.Error()})
			continue
		}
		if n == 0 {
			out = append(out, check{Name: inv.name, Status: "ok", Detail: "none"})
			continue
		}
		c := check{
			Name:   inv.name,
			Status: "fail",
			Detail: fmt.Sprintf("%d found — %s", n, inv.desc),
			Impact: inv.impact,
			Remedy: inv.remedy,
		}
		if inv.examples != "" {
			c.Examples = queryExamples(ctx, db.DB, inv.examples)
		}
		out = append(out, c)
	}
	return out
}

// queryExamples returns up to a few locations for a failing invariant.
//
// Errors are swallowed on purpose: an example is a convenience, and a
// diagnostic command that fails while explaining a failure helps nobody.
func queryExamples(ctx context.Context, db *sql.DB, query string) []string {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() && len(out) < 3 {
		var s string
		if err := rows.Scan(&s); err != nil {
			return out
		}
		out = append(out, s)
	}
	return out
}

// renderChecks writes doctor's findings, and the precision report when one was
// requested (prec is nil otherwise).
//
// One JSON document, not two. `--json` used to emit the precision report and
// the checks as separate top-level objects on the same stream, which no JSON
// parser accepts — so the report could not actually be gated on in CI, which
// is the only reason to emit it as JSON at all.
func renderChecks(cmd *cobra.Command, g *Global, checks []check, prec *precisionReport) error {
	failed := 0
	for _, c := range checks {
		if c.Status == "fail" {
			failed++
		}
	}

	if g.JSON {
		doc := map[string]any{"checks": checks, "failed": failed}
		if prec != nil {
			doc["resolution_precision"] = prec
		}
		if err := emitJSON(cmd.OutOrStdout(), doc); err != nil {
			return err
		}
	} else {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		for _, c := range checks {
			mark := "ok  "
			switch c.Status {
			case "warn":
				mark = "warn"
			case "fail":
				mark = "FAIL"
			}
			_, _ = fmt.Fprintf(w, "[%s]\t%s\t%s\n", mark, c.Name, c.Detail)
			for _, ex := range c.Examples {
				_, _ = fmt.Fprintf(w, "\t\t  at %s\n", ex)
			}
			if c.Impact != "" {
				_, _ = fmt.Fprintf(w, "\t\t  means: %s\n", c.Impact)
			}
			if c.Remedy != "" {
				_, _ = fmt.Fprintf(w, "\t\t  do:    %s\n", c.Remedy)
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if prec != nil {
			if err := renderPrecision(cmd.OutOrStdout(), prec); err != nil {
				return err
			}
		}
	}

	// Failing checks are invariant violations. The precision measurement is
	// deliberately not part of this: see the note above precisionTier.
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

// ---------------------------------------------------------------- migrate

func newMigrateCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Bring an existing index schema up to date with this build",
		Long: `Applies any unapplied schema migrations to the project's index database.
Safe and idempotent; does not rebuild or clear existing indexed data.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dbPath := g.DBPath()
			if _, err := os.Stat(dbPath); os.IsNotExist(err) {
				return fmt.Errorf("no index at %s — run 'repocortex init' first", dbPath)
			}
			db, err := store.Open(ctx, store.Options{Path: dbPath})
			if err != nil {
				return fmt.Errorf("open index: %w", err)
			}
			defer func() { _ = db.Close() }()

			applied, err := store.Migrate(ctx, db.DB, time.Now().Unix)
			if err != nil {
				return fmt.Errorf("apply schema: %w", err)
			}
			version, err := store.SchemaVersion(ctx, db.DB)
			if err != nil {
				return err
			}
			if len(applied) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "index is already at latest schema version v%d\n", version)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "upgraded index schema to v%d (applied migrations: %v)\n", version, applied)
			}
			reg := registry.New(db)
			if run, err := reg.CurrentRun(ctx); err == nil {
				if n, err := reg.PruneImpossibleEdges(ctx, run.ID); err == nil && n > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "pruned %d impossible edge(s) from earlier index schema\n", n)
				}
			}
			return nil
		},
	}
	return cmd
}
