package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/satyendrakumarsingh/repocortex/internal/arch/adr"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

// ArchMind: does the code match the architecture we said we had?
//
// The command is built to be run in CI as much as by a person, which drives two
// decisions. `--fail-on` gives it a non-zero exit so a pipeline can gate on it,
// and the output is sorted deterministically so "did this get worse?" is
// answerable from a diff rather than from reading.

func newArchCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arch",
		Short: "Check the code against the declared architecture",
		Long: `Compares the dependency graph against the components and rules declared
in .cortex.yml, and reports boundary crossings and violations.

This checks the GRAPH, not imports. That is the point: "the reporting module
must not write the payments table" is an architectural rule no import-based
linter can see, because the write happens through a repository class reporting
is perfectly entitled to call.

Components are DECLARED, never inferred. A component model RepoCortex guessed
would be a description of the code's current shape, and checking code against a
description of itself finds nothing by construction.

Declare them in .cortex.yml:

  architecture:
    components:
      - name: controller
        include: ["src/main/java/com/pay/controller/**"]
      - name: repository
        include: ["src/main/java/com/pay/repository/**"]
    rules:
      - id: controllers-do-not-touch-the-database
        description: Controllers must go through a service.
        from: [controller]
        forbidden: [repository]
        severity: high

Architecture rules are read from the repository's own config even though that
file is untrusted, because they are pure declarative data — path globs and
component names, matched by a hand-written matcher and never compiled as a
regular expression. Nothing here can select a tool or an argument. And this is
where they belong: a rule that lives in one contributor's home directory is a
rule that is not enforced.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runArch(cmd, g)
		},
	}

	cmd.Flags().String("fail-on", "", "exit non-zero when a violation of this severity or worse exists (high|medium|low)")
	cmd.Flags().String("min-confidence", "", "ignore relationships below this confidence (exact|high|medium|low)")
	cmd.Flags().Bool("crossings", false, "list every component-to-component dependency, not only violations")
	cmd.AddCommand(newADRCmd(g))
	return cmd
}

func runArch(cmd *cobra.Command, g *Global) error {
	ctx := cmd.Context()
	failOn, _ := cmd.Flags().GetString("fail-on")
	minConf, _ := cmd.Flags().GetString("min-confidence")
	showCrossings, _ := cmd.Flags().GetBool("crossings")

	if failOn != "" && !isSeverity(failOn) {
		return fmt.Errorf("--fail-on %q: want high, medium or low", failOn)
	}
	var min model.Confidence
	if minConf != "" {
		min = model.Confidence(minConf)
		if !min.Valid() {
			return fmt.Errorf("--min-confidence %q: want exact, high, medium or low", minConf)
		}
	}

	s, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()

	cfg := g.Config()
	rep, err := architecture.Check(ctx, s.reg, s.runID, cfg.Architecture, min)
	if errors.Is(err, architecture.ErrNoModel) {
		out := cmd.OutOrStdout()
		// --json must stay machine-readable on EVERY path. Emitting prose here
		// broke `cortex arch --json | jq` for the most common case — a
		// repository that has not declared an architecture yet — which is
		// exactly when a script is most likely to be probing for one.
		if g.JSON {
			return emitJSON(out, map[string]any{
				"architecture_declared": false,
				"violations":            []architecture.Violation{},
				"crossings":             []architecture.Crossing{},
				"components":            []architecture.ComponentSummary{},
				"note": "No architecture declared. Add an `architecture:` block to " +
					".cortex.yml. Components are declared rather than inferred: a model " +
					"RepoCortex guessed would describe the code as it is, and checking code " +
					"against a description of itself finds nothing.",
			})
		}
		fmt.Fprintln(out, "No architecture declared.")
		fmt.Fprintln(out, "\nAdd an `architecture:` block to .cortex.yml with the components "+
			"of your system and the rules between them. See 'cortex arch --help'.")
		fmt.Fprintln(out, "\nComponents are declared rather than inferred on purpose: a model "+
			"RepoCortex guessed would describe the code as it is, and checking code against a "+
			"description of itself finds nothing.")
		return nil
	}
	if err != nil {
		return err
	}

	if g.JSON {
		// architecture_declared is emitted on both paths so a consumer can
		// branch on one field instead of inferring it from an empty list.
		if err := emitJSON(cmd.OutOrStdout(), struct {
			Declared bool `json:"architecture_declared"`
			architecture.Report
		}{Declared: true, Report: rep}); err != nil {
			return err
		}
	} else {
		renderArch(cmd, rep, showCrossings)
	}

	if failOn != "" {
		if worst := rep.HighestSeverity(); worst != "" && severityRank(worst) <= severityRank(failOn) {
			return fmt.Errorf("%d architecture violation(s), worst severity %q (--fail-on %s)",
				len(rep.Violations), worst, failOn)
		}
	}
	return nil
}

func renderArch(cmd *cobra.Command, rep architecture.Report, showCrossings bool) {
	out := cmd.OutOrStdout()

	if len(rep.Violations) > 0 {
		fmt.Fprintf(out, "VIOLATIONS (%d)\n", len(rep.Violations))
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "  SEVERITY\tRULE\tCROSSING\tEVIDENCE\tCONFIDENCE")
		for _, v := range rep.Violations {
			crossing := fmt.Sprintf("%s -> %s (%s -> %s)", v.From, v.To, v.FromName, v.ToName)
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s:%d\t%s\n",
				v.Severity, v.RuleID, crossing, v.FilePath, v.Line, v.Confidence)
		}
		_ = tw.Flush()
		fmt.Fprintln(out)
	} else if rep.RulesEvaluated > 0 {
		fmt.Fprintf(out, "No violations across %d rule(s).\n\n", rep.RulesEvaluated)
	}

	// Data reachability gets its own section rather than being folded into
	// VIOLATIONS above. A forbidden component dependency and an endpoint that
	// can reach cardholder data are both rule breaches, but they go to different
	// people and carry different consequences — one is a design decision, the
	// other is a compliance finding — and merging them buries the second in the
	// first on any repository with a few architectural drifts.
	if len(rep.DataViolations) > 0 {
		fmt.Fprintf(out, "SENSITIVE DATA REACHABILITY (%d)\n", len(rep.DataViolations))
		for _, v := range rep.DataViolations {
			fmt.Fprintf(out, "  %s\n", v.String())
		}
		fmt.Fprintln(out)
	} else if rep.DataCoverage.ClassifiedTables > 0 {
		fmt.Fprintf(out, "No endpoint reaches classified data across %d classified table(s).\n\n",
			rep.DataCoverage.ClassifiedTables)
	}

	// Printed whenever a classification exists, findings or not. "No endpoint
	// reaches cardholder data" is exactly the sentence someone will put in an
	// audit response, and it is only as good as what the check could see.
	if cav := rep.DataCoverage.Caveats(); len(cav) > 0 {
		fmt.Fprintln(out, "  NOT CHECKED")
		for _, c := range cav {
			fmt.Fprintf(out, "    %s\n", c)
		}
		fmt.Fprintln(out)
	}

	if showCrossings && len(rep.Crossings) > 0 {
		fmt.Fprintf(out, "COMPONENT DEPENDENCIES (%d)\n", len(rep.Crossings))
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		for _, c := range rep.Crossings {
			mark := ""
			if c.Violation {
				mark = "  ← violates a rule"
			}
			fmt.Fprintf(tw, "  %s -> %s\t%d edge(s)\tweakest %s%s\n",
				c.From, c.To, c.Count, c.Weakest, mark)
		}
		_ = tw.Flush()
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, "COMPONENTS")
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	empty := 0
	for _, c := range rep.Components {
		note := ""
		if c.Symbols == 0 {
			note = "  ← matched nothing; check the include patterns"
			empty++
		}
		fmt.Fprintf(tw, "  %s\t%d symbol(s)\t%d file(s)%s\n", c.Name, c.Symbols, c.Files, note)
	}
	_ = tw.Flush()

	fmt.Fprintf(out, "\n%d relationship(s) crossed a component boundary.\n", rep.EdgesConsidered)

	// The coverage caveat. A rule can only be broken by code the model covers,
	// so a large unassigned count means every rule is weaker than it looks —
	// a forbidden dependency routed through an unassigned file is invisible.
	if rep.Unassigned > 0 {
		fmt.Fprintf(out, "%d symbol(s) belong to no declared component and were not checked. "+
			"A forbidden dependency routed through one of them would not appear above.\n",
			rep.Unassigned)
	}
	if empty > 0 {
		fmt.Fprintf(out, "%d component(s) matched nothing, so every rule naming them is inert.\n", empty)
	}
	if len(rep.Violations) > 0 {
		fmt.Fprintln(out, "\nConfidence is the confidence of the RELATIONSHIP, not of the rule. "+
			"A violation on a low-confidence edge is a lead; one on a compiler-proven edge is a fact.")
	}
}

func isSeverity(s string) bool {
	return s == "high" || s == "medium" || s == "low"
}

func severityRank(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	}
	return 3
}

// ---------------------------------------------------------------- adr

func newADRCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "adr [number-or-substring]",
		Short: "List or show architecture decision records",
		Long: `Lists the ADRs in this repository, or shows one.

An ADR is why a choice was made. Surfacing it to an agent is how "we deliberately
use optimistic locking here" survives contact with a refactor, instead of being
re-litigated from scratch every session.

Read from docs/adr/, docs/decisions/ and the other conventional locations, in
both the Nygard and MADR formats.

RepoCortex does not check whether an ADR is still TRUE. A record saying the system
uses Postgres is evidence of a decision, not of the current state — the graph is
where you check the current state.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			cfg := g.Config()
			files, err := s.reg.Files(ctx, s.runID)
			if err != nil {
				return err
			}

			var records []adr.Record
			for _, f := range files {
				if !adr.IsADRPath(f.Path) {
					continue
				}
				// Containment is checked, not assumed. Joining RepoRoot to a
				// stored path and calling os.ReadFile made
				// `cortex arch adr --json` print the contents of any file the
				// process could read: a repository containing
				// docs/adr/0001-x.md -> /etc/hosts had it emitted as an ADR
				// summary. security.ValidatePath resolves symlinks and rejects
				// anything landing outside the root; it existed with the right
				// contract and no production caller, which is the only reason
				// this was reachable.
				//
				// ValidatePath alone is the guard, deliberately. An earlier
				// version also skipped files recorded parse_status='skipped',
				// reasoning that symlinks land there — but so does every
				// markdown file, because no tree-sitter grammar claims one. That
				// dropped every legitimate ADR while still passing the
				// exfiltration test: a containment check that rejects everything
				// looks secure and breaks the feature.
				abs, err := security.ValidatePath(cfg.RepoRoot, f.Path)
				if err != nil {
					g.Log().Warn("refusing to read a file that resolves outside the repository",
						"path", f.Path, "error", err)
					continue
				}
				src, err := os.ReadFile(abs) //nolint:gosec // path validated against the repo root above
				if err != nil {
					continue
				}
				if rec, ok := adr.Parse(f.Path, src); ok {
					records = append(records, rec)
				}
			}

			out := cmd.OutOrStdout()
			if len(records) == 0 {
				if g.JSON {
					return emitJSON(out, map[string]any{"adrs": []adr.Record{}})
				}
				fmt.Fprintln(out, "No architecture decision records found.")
				fmt.Fprintln(out, "\nLooked in docs/adr/, docs/adrs/, docs/decisions/, "+
					"doc/adr/, adr/ and architecture/decisions/, for .md files.")
				return nil
			}

			if len(args) > 0 {
				query := strings.ToLower(args[0])
				var matched []adr.Record
				for _, r := range records {
					if strings.Contains(strings.ToLower(r.Title), query) ||
						strings.Contains(strings.ToLower(r.Path), query) ||
						fmt.Sprint(r.Number) == query {
						matched = append(matched, r)
					}
				}
				if len(matched) == 0 {
					return fmt.Errorf("no ADR matches %q; %d record(s) indexed", args[0], len(records))
				}
				records = matched
			}

			if g.JSON {
				return emitJSON(out, map[string]any{"adrs": records})
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "ADR\tSTATUS\tTITLE\tFILE")
			for _, r := range records {
				num := "—"
				if r.Number > 0 {
					num = fmt.Sprintf("%04d", r.Number)
				}
				status := r.Status
				if status == "" {
					status = "(none)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", num, status, r.Title, r.Path)
			}
			_ = tw.Flush()

			if len(records) == 1 && records[0].Summary != "" {
				fmt.Fprintf(out, "\n%s\n", records[0].Summary)
			}
			return nil
		},
	}
}
