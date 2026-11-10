package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scip"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scipimport"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

func newResolveCmd(g *Global) *cobra.Command {
	var minConfidence string
	var noSCIP bool

	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Bind extracted references to their definitions",
		Long: `Resolves references to symbol definitions, building the call graph.

Indexing extracts references but cannot bind them: doing so requires the
complete symbol table, which does not exist until every file is parsed.

Every binding records the evidence that justified it and a confidence level.
Nothing here produces 'exact' confidence — that is reserved for compiler-backed
SCIP indexes.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := g.Config()
			start := time.Now()

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
			run, err := reg.CurrentRun(ctx)
			if err != nil {
				// Distinguish "nothing indexed yet" from "the database is
				// locked or corrupt". Collapsing both into one message sent
				// users to re-run indexing when the real problem was a held
				// lock, which indexing would also hit.
				if errors.Is(err, registry.ErrNoCommittedRun) {
					return fmt.Errorf("no committed index — run 'cortex index' first")
				}
				return fmt.Errorf("read current index revision: %w", err)
			}

			min := model.Confidence(minConfidence)
			if minConfidence != "" && !min.Valid() {
				return fmt.Errorf("--min-confidence %q: want exact, high, medium or low", minConfidence)
			}

			// Deployment units join to endpoints in the same pass, and the
			// summary is captured rather than dropped: "0 endpoints bound" and
			// "no manifests declare a build context" are different facts, and a
			// user who cannot tell them apart will read the first as a bug.
			var infra pipeline.InfraBindSummary
			stats, err := pipeline.Resolve(ctx, reg, run.ID, pipeline.ResolveOptions{
				MinConfidence: min,
				OnInfraUnits:  func(s pipeline.InfraBindSummary) { infra = s },
			})
			if err != nil {
				return err
			}
			rs, err := reg.ResolutionStats(ctx, run.ID)
			if err != nil {
				return err
			}

			// Table accesses bind in the same pass, for the same reason
			// references do: the table a query names may be declared by a file
			// parsed long after the query was. Doing it here rather than in a
			// command of its own means a user who runs `resolve` gets a
			// complete graph, instead of a graph that is complete except for
			// the part nobody remembered to run.
			access, err := reg.BindTableAccesses(ctx, run.ID)
			if err != nil {
				return err
			}
			cfgUse, err := reg.BindConfigUsages(ctx, run.ID)
			if err != nil {
				return err
			}

			// SCIP auto-discovery: an index.scip in the project root or .cortex/
			// upgrades heuristic edges to 'exact'. The outcome is always reported,
			// including failure, because a skipped upgrade is otherwise
			// indistinguishable from "no SCIP index was present".
			scipOut := scipAutoOutcome{}
			if !noSCIP {
				scipOut = autoImportSCIP(ctx, g, reg, run.ID, cfg.RepoRoot)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"project": cfg.ProjectName, "index_run": run.ID,
					"references": rs.Total, "resolved": rs.Resolved,
					"ambiguous": rs.Ambiguous, "unresolved": rs.Unresolved,
					// A8 — how much of `ambiguous` is "too common to weigh"
					// rather than "two candidates and no evidence". See
					// infer.Stats.
					"cap_exceeded": stats.CapExceeded,
					"cap_dropped":  stats.CapDropped,
					// binding_rate is the published coverage number, and the
					// only rate emitted here (capability.CoverageFloor says
					// why). ResolutionStats.Rate is deliberately absent: two
					// rates in one payload is how a consumer ends up quoting
					// the one this project does not stand behind.
					"edges":    stats.Edges,
					"bindable": rs.Bindable(), "binding_rate": rs.BindingRate(),
					"by_confidence": stats.ByConfidence,
					"table_accesses": map[string]any{
						"total": access.Total, "bound": access.Bound,
						"unbound": access.Unbound, "edges": access.Edges,
						"unbound_tables": access.UnboundTables,
					},
					"config_usages": map[string]any{
						"total": cfgUse.Total, "bound": cfgUse.Bound,
						"unbound": cfgUse.Unbound, "edges": cfgUse.Edges,
					},
					"deployment_units": map[string]any{
						"units": infra.Units, "with_context": infra.WithContext,
						"endpoints": infra.Endpoints, "bound": infra.Bound,
						"ambiguous": infra.Ambiguous, "edges": infra.Edges,
					},
					"scip":        scipOut.json(),
					"duration_ms": time.Since(start).Milliseconds(),
				})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Resolved %s (revision %d)\n", cfg.ProjectName, run.ID)
			fmt.Fprintf(out, "  References:  %d\n", rs.Total)
			// The percentage is over BINDABLE references, not over all of
			// them, so this command publishes the same number the MCP tools
			// and the portal do — see capability.CoverageFloor. Ambiguous and
			// Unresolved are printed immediately below, which is the condition
			// ResolutionStats.Bindable places on that denominator.
			fmt.Fprintf(out, "  Resolved:    %d (%.1f%% of the %d that name something this index defines)\n",
				rs.Resolved, rs.BindingRate()*100, rs.Bindable())
			fmt.Fprintf(out, "  Ambiguous:   %d\n", rs.Ambiguous)
			// A8 — name the part of `Ambiguous` that is a cap drop. Without
			// this a name with 200 definitions and a name with two are the
			// same number, and only the second is a disambiguation problem.
			if stats.CapDropped > 0 {
				fmt.Fprintf(out, "    %d had more candidates than the cap allows (%d reached the cap; the receiver rescued %d)\n",
					stats.CapDropped, stats.CapExceeded, stats.CapExceeded-stats.CapDropped)
			}
			fmt.Fprintf(out, "  Unresolved:  %d  (calls outside this repository)\n", rs.Unresolved)
			fmt.Fprintf(out, "  Edges:       %d\n", stats.Edges)
			for _, c := range []model.Confidence{model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow} {
				if n := stats.ByConfidence[c]; n > 0 {
					fmt.Fprintf(out, "    %-8s %d\n", string(c)+":", n)
				}
			}
			if access.Total > 0 {
				fmt.Fprintf(out, "  Data access: %d statement(s), %d bound to a declared table, %d edges\n",
					access.Total, access.Bound, access.Edges)
				if access.Unbound > 0 {
					// Not a failure. A query against a table this repository
					// does not declare is a service reading somebody else's
					// database, which is worth naming rather than hiding.
					fmt.Fprintf(out, "    %d access(es) name tables no migration here declares", access.Unbound)
					if len(access.UnboundTables) > 0 {
						shown := access.UnboundTables
						if len(shown) > 6 {
							shown = shown[:6]
						}
						fmt.Fprintf(out, " (e.g. %s)", strings.Join(shown, ", "))
					}
					fmt.Fprintln(out)
				}
			}
			if cfgUse.Total > 0 {
				fmt.Fprintf(out, "  Config:      %d read(s), %d bound to a declared key, %d edges\n",
					cfgUse.Total, cfgUse.Bound, cfgUse.Edges)
				if cfgUse.Unbound > 0 {
					// The finding that catches a deployment failing at start-up:
					// code reads a setting no committed file provides.
					fmt.Fprintf(out, "    %d read(s) name keys no configuration file here declares "+
						"— see 'cortex config'\n", cfgUse.Unbound)
				}
			}
			if infra.Units > 0 {
				fmt.Fprintf(out, "  Deployment:  %d unit(s), %d naming a build context, %d edges\n",
					infra.Units, infra.WithContext, infra.Edges)
				// Two different silences. A unit that names no build context
				// says what runs without saying which directory it came from —
				// a Kubernetes container or a Terraform resource, and a
				// coverage fact rather than a failure. An endpoint that matched
				// several units is a real ambiguity the join REFUSED to guess
				// at. Reporting them as one number would hide the second.
				if unplaced := infra.WithContext - infra.Bound; unplaced > 0 {
					fmt.Fprintf(out, "    %d unit(s) name a build context that matched no endpoint\n", unplaced)
				}
				if infra.Ambiguous > 0 {
					fmt.Fprintf(out, "    %d endpoint(s) matched more than one unit and were left unbound "+
						"— see 'cortex infra'\n", infra.Ambiguous)
				}
			}
			scipOut.render(out)
			fmt.Fprintf(out, "  Duration:    %dms\n", time.Since(start).Milliseconds())
			return nil
		},
	}
	cmd.Flags().StringVar(&minConfidence, "min-confidence", "", "drop bindings weaker than: high|medium|low")
	cmd.Flags().BoolVar(&noSCIP, "no-scip", false, "do not import an index.scip found in the project root or .cortex/")
	return cmd
}

// scipAutoOutcome records what SCIP auto-discovery did, so resolve can say so
// in both its text and JSON output instead of only in the log.
type scipAutoOutcome struct {
	Path     string
	Tool     string
	Upgraded int
	Promoted int
	Err      error
}

func (o scipAutoOutcome) found() bool { return o.Path != "" }

func (o scipAutoOutcome) json() map[string]any {
	if !o.found() {
		return map[string]any{"found": false}
	}
	m := map[string]any{"found": true, "path": o.Path}
	if o.Err != nil {
		m["error"] = o.Err.Error()
		return m
	}
	m["tool"], m["edges_upgraded"], m["calls_promoted"] = o.Tool, o.Upgraded, o.Promoted
	return m
}

func (o scipAutoOutcome) render(out io.Writer) {
	switch {
	case !o.found():
		return
	case o.Err != nil:
		fmt.Fprintf(out, "  SCIP:        %s found but NOT imported: %v\n", o.Path, o.Err)
		fmt.Fprintf(out, "    edges were not upgraded to exact; fix the index or run 'cortex scip import %s'\n", o.Path)
	default:
		fmt.Fprintf(out, "  SCIP:        imported %s (%s): %d edge(s) upgraded to exact, %d call(s) promoted\n",
			o.Path, o.Tool, o.Upgraded, o.Promoted)
	}
}

// autoImportSCIP imports the first non-empty index.scip it finds under
// repoRoot. A failed import is returned in the outcome, never swallowed, and
// does not fail resolve: the heuristic graph it just built is still valid.
func autoImportSCIP(ctx context.Context, g *Global, reg *registry.Registry, runID int64, repoRoot string) scipAutoOutcome {
	for _, candidate := range []string{
		filepath.Join(repoRoot, "index.scip"),
		filepath.Join(repoRoot, ".cortex", "index.scip"),
	} {
		fi, err := os.Stat(candidate)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue
		}
		g.Log().Info("auto-discovered SCIP index; importing compiler ground truth", "path", candidate)
		imp := scipimport.NewImporter(reg, runID, scip.Options{
			RepoRoot: repoRoot,
			MaxBytes: scip.DefaultMaxBytes,
		})
		res, err := imp.Import(ctx, candidate)
		if err != nil {
			g.Log().Warn("scip auto-import failed", "path", candidate, "error", err)
			return scipAutoOutcome{Path: candidate, Err: fmt.Errorf("import %s: %w", candidate, err)}
		}
		g.Log().Info("scip auto-import complete",
			"tool", res.Ingest.Tool.String(),
			"edges_upgraded", res.Edges.Upgraded,
			"calls_promoted", res.Promote.Promoted)
		return scipAutoOutcome{
			Path: candidate, Tool: res.Ingest.Tool.String(),
			Upgraded: res.Edges.Upgraded, Promoted: res.Promote.Promoted,
		}
	}
	return scipAutoOutcome{}
}
