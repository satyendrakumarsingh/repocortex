package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/satyendrakumarsingh/repocortex/internal/bench"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"

	"github.com/spf13/cobra"
)

func newBenchCmd(g *Global) *cobra.Command {
	var failOnRegression bool

	cmd := &cobra.Command{
		Use:   "bench <cases.yml>",
		Short: "Measure whether the context engine retrieves the right code",
		Long: `Runs a set of hand-written cases through the context engine and scores what
came back.

This exists because every other claim about retrieval quality would otherwise be
an assertion. A case names a task, the code a correct answer MUST contain, and
the code it must NOT — and the second half is what makes the number mean
something. Recall alone is trivially gamed by returning the whole repository.

A case file:

  - id: idempotent-callback
    task: Add idempotency to payment callback processing
    expect:
      - CallbackController
      - TransactionService
      - transactions
    forbid:
      - MarketingReport
    max_tokens: 4000

Scored against the pack the engine actually produces, so a change to ranking,
budgeting or extraction shows up here as a number rather than as a surprise in
an agent session weeks later.

WHAT THIS DOES NOT MEASURE: whether an agent completed the task. Only whether
the context contained what a human said it should. Those are different claims
and conflating them would be the most flattering possible mistake.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			path := args[0]

			src, err := os.ReadFile(path) //nolint:gosec // caller-supplied case file
			if err != nil {
				return fmt.Errorf("read case file %q: %w", path, err)
			}
			cases, err := bench.LoadCases(src)
			if err != nil {
				return fmt.Errorf("parse %q: %w", path, err)
			}
			if err := bench.Validate(cases); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			cfg := g.Config()
			gr := graph.NewForRun(s.db, s.runID)

			// One assembler across every case. The cases all query the SAME
			// run, so the catalogue is built once instead of per case — the
			// exact reuse the cache exists for, and it was being thrown away by
			// constructing inside the loop.
			asm := devctx.NewAssembler()

			results := make([]bench.Result, 0, len(cases))
			for _, c := range cases {
				pack, err := asm.Assemble(ctx, s.reg, gr, s.runID, devctx.Request{
					Task:   c.Task,
					Budget: devctx.BudgetFrom(cfg),
				})
				if err != nil {
					return fmt.Errorf("case %s: %w", c.ID, err)
				}
				results = append(results, bench.Score(c, pack))
			}
			suite := bench.Summarise(results)

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), suite)
			}

			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "CASE\tRESULT\tRECALL\tTOKENS\tMISSING")
			for _, r := range suite.Results {
				status := "pass"
				if !r.Passed {
					status = "FAIL"
				}
				missing := strings.Join(r.Missing, ", ")
				if len(r.Violations) > 0 {
					missing += "  forbidden: " + strings.Join(r.Violations, ", ")
				}
				if missing == "" {
					missing = "—"
				}
				fmt.Fprintf(tw, "%s\t%s\t%.0f%%\t%d\t%s\n",
					r.CaseID, status, r.Recall*100, r.TokenEstimate, missing)
			}
			_ = tw.Flush()

			fmt.Fprintf(out, "\n%d/%d passed, mean recall %.0f%%",
				suite.Passed, suite.Cases, suite.MeanRecall*100)
			if suite.TotalViolations > 0 {
				fmt.Fprintf(out, ", %d forbidden item(s) retrieved", suite.TotalViolations)
			}
			fmt.Fprintln(out, ".")

			if suite.TotalViolations > 0 {
				// A forbidden item is worse than a missing one and must not be
				// averaged away. Recall can look excellent while the pack drags
				// in an unrelated module, which is precisely the failure the
				// forbid list exists to catch.
				fmt.Fprintln(out, "\nA forbidden item retrieved is a precision failure, not a "+
					"rounding error: it is context an agent will read and act on.")
			}

			if failOnRegression && suite.Passed < suite.Cases {
				return fmt.Errorf("%d of %d benchmark case(s) failed", suite.Cases-suite.Passed, suite.Cases)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&failOnRegression, "fail-on-regression", false,
		"exit non-zero when any case fails, for CI")
	return cmd
}
