// Package cli assembles RepoCortex's command-line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/observe"

	"github.com/spf13/cobra"
)

// Build metadata, injected via -ldflags at release time. Spec §64.
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

// Global holds flags shared by every command.
type Global struct {
	JSON    bool
	Verbose bool
	Quiet   bool
	Project string

	cfg config.Config
	log *slog.Logger
}

// IndexDir is where RepoCortex stores its registry inside a project.
const IndexDir = ".repocortex"

// IndexFile is the SQLite registry filename.
const IndexFile = "index.db"

// Config returns the resolved configuration.
func (g *Global) Config() config.Config { return g.cfg }

// Log returns the logger.
func (g *Global) Log() *slog.Logger { return g.log }

// DBPath returns the registry path for the resolved project root.
func (g *Global) DBPath() string {
	primary := filepath.Join(g.cfg.RepoRoot, IndexDir, IndexFile)
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	// If a repository config file (.repocortex.yml) is present, strictly use .repocortex/.
	repoCfg := filepath.Join(g.cfg.RepoRoot, config.RepoConfigName)
	if _, err := os.Stat(repoCfg); err == nil {
		return primary
	}
	alt := filepath.Join(g.cfg.RepoRoot, ".devintel", IndexFile)
	if _, err := os.Stat(alt); err == nil {
		if g.log != nil {
			g.log.Warn("using legacy .devintel index as fallback; run 'repocortex init' to migrate to .repocortex/", "path", alt)
		}
		return alt
	}
	return primary
}

// NewRoot builds the root command tree.
func NewRoot() *cobra.Command {
	g := &Global{}

	root := &cobra.Command{
		Use:     "repocortex",
		Version: Version,
		Short:   "Deterministic code intelligence & change safety for AI coding agents",
		Long: `RepoCortex builds a queryable knowledge graph of a software system —
symbols, references, APIs, database access, tests, configuration and Git
history — and serves it to AI coding agents over MCP.

Every relationship it reports carries a confidence level and the source
location that justifies it, so an agent can tell a compiler-proven fact from
a heuristic guess.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return g.setup(cmd)
		},
	}

	pf := root.PersistentFlags()
	pf.BoolVar(&g.JSON, "json", false, "emit machine-readable JSON on stdout")
	pf.BoolVarP(&g.Verbose, "verbose", "v", false, "verbose logging")
	pf.BoolVarP(&g.Quiet, "quiet", "q", false, "suppress all logging")
	pf.StringVar(&g.Project, "project", "", "project root (default: current directory)")

	root.AddCommand(
		newInitCmd(g),
		newIndexCmd(g),
		newIndexersCmd(g),
		newResolveCmd(g),
		newSCIPCmd(g),
		newServeCmd(g),
		newWatchCmd(g),
		newAPICmd(g),
		newArchCmd(g),
		newDocsCmd(g),
		newVerifyCmd(g),
		newDiffCmd(g),
		newAgentCmd(g),
		newInstallCmd(g),
		newWhoCmd(g),
		newImpactCmd(g),
		newPRImpactCmd(g),
		newInfraCmd(g),
		newSystemCmd(g),
		newUICmd(g),
		newHooksCmd(g),
		newAutoIndexCmd(g),
		newCoverageCmd(g),
		newTraceCmd(g),
		newCompileCmd(g),
		newRenameCmd(g),
		newMoveCmd(g),
		newBenchCmd(g),
		newConfigCmd(g),
		newMigrateCmd(g),
		newDepsCmd(g),
		newDBCmd(g),
		newSearchCmd(g),
		newSymbolCmd(g),
		newReferencesCmd(g),
		newCallersCmd(g),
		newCalleesCmd(g),
		newExploreCmd(g),
		newContextCmd(g),
		newStatusCmd(g),
		newDoctorCmd(g),
		newAccuracyCmd(g),
		newVersionCmd(g),
		newLicensesCmd(g),
		newLanguagesCmd(g),
		newExportCmd(g),
		newPackCmd(g),
		newPruneCmd(g),
		newDiffMinimizeCmd(g),
		newMissedCmd(g),
		newPlanChangeCmd(g),
		newEventsCmd(g),
		newReportCmd(),
		newToolCmd(g),
		newStatsCmd(g),
		newDeadCodeCmd(g),
		newHotspotsCmd(g),
		newBusFactorCmd(g),
		newCommunitiesCmd(g),
		newPathCmd(g),
		newImplementationsCmd(g),
		newTestsCmd(g),
		newQueryCmd(g),
		newSBOMCmd(g),
		newVulnsCmd(g),
	)
	applyCommandGroups(root)
	return root
}

func (g *Global) setup(cmd *cobra.Command) error {
	if err := checkLimitFlags(cmd); err != nil {
		return err
	}
	level := "info"
	if g.Verbose {
		level = "debug"
	}
	g.log = observe.New(observe.Options{
		Level: level,
		JSON:  g.JSON,
		Quiet: g.Quiet,
	})

	root := g.Project
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("determine working directory: %w", err)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve project root %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("project root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project root %q is not a directory", abs)
	}

	cfg, err := config.Load(abs)
	if err != nil {
		return err
	}
	if cfg.ProjectName == "" {
		cfg.ProjectName = filepath.Base(abs)
	}
	g.cfg = cfg
	return nil
}

// errInvalidArgument marks an error as the caller's argument being wrong.
var errInvalidArgument = errors.New("invalid argument")

// checkLimitFlags enforces one rule for every command's -n/--limit: when the
// flag is given it must be a positive number. Zero used to mean "the default"
// in one command and "no limit" in another, and a negative number was passed
// to a query as it was. An unset flag keeps the command's own default.
func checkLimitFlags(cmd *cobra.Command) error {
	f := cmd.Flags().Lookup("limit")
	if f == nil || !f.Changed || f.Value.Type() != "int" {
		return nil
	}
	n, err := strconv.Atoi(f.Value.String())
	if err != nil || n < 1 {
		return fmt.Errorf("%w: --limit must be positive, got %s (omit it for the default)", errInvalidArgument, f.Value.String())
	}
	return nil
}

// jsonError is the object a failing command prints on stdout under --json.
type jsonError struct {
	Error jsonErrorBody `json:"error"`
}

type jsonErrorBody struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// writeJSONError prints err as a JSON object so a consumer reading stdout gets
// a document, not empty input.
func writeJSONError(w io.Writer, err error) {
	code := "error"
	if errors.Is(err, errInvalidArgument) {
		code = "invalid_argument"
	}
	_ = emitJSON(w, jsonError{Error: jsonErrorBody{Message: err.Error(), Code: code}})
}

// ExitCodeError is an error that carries the process exit code it wants.
//
// It exists so a command whose exit code IS its output — `verify`, whose four
// dispositions a CI job branches on — can express that without calling os.Exit
// from inside RunE. A command that exits the process directly cannot be tested
// for the one behaviour that matters about it, and the codes would then be the
// only untested part of a gate whose entire job is to fail correctly.
type ExitCodeError struct {
	Code int
	// Silent is set when the command already printed its report. The code is
	// the message at that point, and an "error:" line beneath a rendered report
	// reads as a malfunction rather than a verdict.
	Silent bool
	Err    error
}

func (e *ExitCodeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

func (e *ExitCodeError) Unwrap() error { return e.Err }

// Execute runs the CLI, returning a process exit code.
func Execute(ctx context.Context) int {
	return executeRoot(ctx, NewRoot(), os.Stdout, os.Stderr)
}

// executeRoot runs root and reports its error as text on stderr and, under
// --json, as a JSON object on stdout too, so a consumer parsing stdout is
// never handed empty input.
func executeRoot(ctx context.Context, root *cobra.Command, stdout, stderr io.Writer) int {
	if err := root.ExecuteContext(ctx); err != nil {
		asJSON, _ := root.PersistentFlags().GetBool("json")
		var ec *ExitCodeError
		if errors.As(err, &ec) {
			if !ec.Silent {
				fmt.Fprintln(stderr, "error:", err)
				if asJSON {
					writeJSONError(stdout, err)
				}
			}
			return ec.Code
		}
		fmt.Fprintln(stderr, "error:", err)
		if asJSON {
			writeJSONError(stdout, err)
		}
		return 1
	}
	return 0
}

func newVersionCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build metadata",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]string{
					"version":    Version,
					"commit":     Commit,
					"build_time": BuildTime,
					"go_os":      osName(),
					"go_arch":    archName(),
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "repocortex %s (commit %s, built %s, %s/%s)\n",
				Version, Commit, BuildTime, osName(), archName())
			return nil
		},
	}
}
