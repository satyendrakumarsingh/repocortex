package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/coverage"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/spf13/cobra"
)

func newCoverageCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Import and analyze test coverage reports",
		Long: `Imports and analyzes test coverage reports across Go coverprofile, LCOV,
Cobertura XML and JaCoCo XML formats, correlating test coverage with indexed code entities.`,
	}
	cmd.AddCommand(newCoverageImportCmd(g))
	return cmd
}

func newCoverageImportCmd(g *Global) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "import <coverage-file>",
		Short: "Import a coverage file (Go coverprofile, LCOV, Cobertura XML, JaCoCo XML)",
		Long: `Reads a coverage report and stores, for each function, method and class in the
index, how many of its instrumented lines ran. analyze_impact then says how much
of a change's target and callers a test run executed, and find_tests says when
no test run executed the target at all.

  repocortex coverage import coverage.out
  repocortex coverage import lcov.info --dry-run

This never runs tests: produce the report with your own test command first. One
profile is kept; importing another replaces it. A file that changes after the
import is reported as stale and its numbers are not used.

A coverage report holds no per-test data, so it can say a function was never
executed but not which test executed it. --dry-run parses and prints the
summary without touching the index.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			if _, err := os.Stat(filePath); err != nil {
				return fmt.Errorf("coverage file %q: %w", filePath, err)
			}

			rep, err := coverage.ParseCoverageFile(filePath)
			if err != nil {
				return fmt.Errorf("parse coverage file: %w", err)
			}

			if dryRun {
				if g.JSON {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(rep)
				}
				_, _ = fmt.Fprint(cmd.OutOrStdout(), rep.Summary())
				return nil
			}

			res, err := storeCoverage(cmd, g, filePath, rep)
			if err != nil {
				return err
			}
			if g.JSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"report":                  rep,
					"symbols_with_coverage":   res.Symbols,
					"files_matched":           res.FilesMatched,
					"files_without_index_row": res.FilesUnmatched,
				})
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprint(out, rep.Summary())
			_, _ = fmt.Fprintf(out, "\nStored: %d symbol(s) now have coverage; %d instrumented file(s) matched an indexed file, %d matched none.\n",
				res.Symbols, res.FilesMatched, len(res.FilesUnmatched))
			for i, p := range res.FilesUnmatched {
				if i == 5 {
					_, _ = fmt.Fprintf(out, "  ... and %d more\n", len(res.FilesUnmatched)-5)
					break
				}
				_, _ = fmt.Fprintf(out, "  no indexed file: %s\n", p)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "parse and print the summary without storing it in the index")
	return cmd
}

// storeCoverage writes the parsed report to the committed run.
//
// The run is read here and not through openQuery because this command writes:
// it opens the index through openWritable like every other command that
// changes it.
func storeCoverage(cmd *cobra.Command, g *Global, filePath string, rep *coverage.Report) (registry.CoverageImportResult, error) {
	ctx := cmd.Context()
	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return registry.CoverageImportResult{}, fmt.Errorf("no index at %s — run 'repocortex init' first", dbPath)
	}
	db, err := openWritable(ctx, g, dbPath)
	if err != nil {
		return registry.CoverageImportResult{}, err
	}
	defer func() { _ = db.Close() }()
	reg := registry.New(db)
	run, err := reg.CurrentRun(ctx)
	if err != nil {
		return registry.CoverageImportResult{}, fmt.Errorf("no committed index — run 'repocortex index' first: %w", err)
	}

	//nolint:gosec // the user-supplied coverage file, already parsed above
	raw, err := os.ReadFile(filepath.Clean(filePath))
	if err != nil {
		return registry.CoverageImportResult{}, fmt.Errorf("read coverage file: %w", err)
	}
	sum := sha256.Sum256(raw)

	in := registry.CoverageInput{
		ContentHash: hex.EncodeToString(sum[:]),
		Format:      rep.Format,
		GitRevision: headRevision(ctx, g.Config().RepoRoot),
	}
	paths := make([]string, 0, len(rep.Files))
	for p := range rep.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		in.Files = append(in.Files, registry.CoverageFile{Path: p, Lines: rep.Files[p].Lines})
	}
	return reg.ImportCoverage(ctx, run.ID, in)
}
