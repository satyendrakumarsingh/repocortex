package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/planchange"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
	"github.com/spf13/cobra"
)

func newPlanChangeCmd(g *Global) *cobra.Command {
	var minConfStr string

	cmd := &cobra.Command{
		Use:   "plan-change <symbol>",
		Short: "Plan refactoring steps, affected callers, and test verification gates",
		Long: `Generates a deterministic change blueprint for refactoring a symbol:
  - Resolves target definition and direct callers
  - Assesses risk score and blast radius
  - Discovers covering tests that must pass before and after changes
  - Produces an ordered, step-by-step execution plan with pre/post-conditions

Examples:
  repocortex plan-change updateStatus
  repocortex plan-change service.TransactionService --min-confidence high
  repocortex plan-change auth.Login --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			targetSymbol := args[0]

			db, err := openReadOnly(ctx, g.DBPath())
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			reg := registry.New(db)
			currentRun, err := reg.CurrentRun(ctx)
			if err != nil {
				return fmt.Errorf("read current index run: %w", err)
			}
			if currentRun == nil {
				return fmt.Errorf("index is empty: run 'repocortex index' first")
			}

			gr := graph.New(db)
			caps := capability.NewDeps(reg, gr)
			planner := planchange.NewPlanner(caps)

			minConf := model.ConfidenceMedium
			if minConfStr != "" {
				minConf = model.Confidence(minConfStr)
				if !minConf.Valid() {
					return fmt.Errorf("unknown confidence %q: use exact, high, medium, or low", minConfStr)
				}
			}

			plan, err := planner.Plan(ctx, currentRun.ID, targetSymbol, planchange.Options{
				MinConfidence: minConf,
			})
			if err != nil {
				return fmt.Errorf("plan change: %w", err)
			}

			if g.JSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(plan)
			}

			// Human readable output
			fmt.Printf("RepoCortex Change Plan for %s (%s)\n", plan.Target.Name, plan.Target.Kind)
			fmt.Printf("Location:      %s:%d\n", plan.Target.FilePath, plan.Target.Line)
			fmt.Printf("Risk Level:    %s (Score: %d/100)\n", strings.ToUpper(plan.RiskLevel), plan.RiskScore)
			fmt.Printf("Blast Radius:  %d direct caller(s), %d total reached\n\n", plan.DirectCallers, plan.TotalCallers)

			fmt.Println("Preconditions:")
			for _, pre := range plan.Preconditions {
				fmt.Printf("  [ ] %s\n", pre)
			}
			fmt.Println()

			fmt.Println("Execution Sequence:")
			for _, step := range plan.Steps {
				loc := ""
				if step.Path != "" {
					loc = fmt.Sprintf(" [%s]", step.Path)
				}
				fmt.Printf("  Step %d: %s%s\n", step.Order, step.Description, loc)
				if step.Command != "" {
					fmt.Printf("          Run: `%s`\n", step.Command)
				}
			}
			fmt.Println()

			fmt.Println("Postconditions:")
			for _, post := range plan.Postconditions {
				fmt.Printf("  [ ] %s\n", post)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&minConfStr, "min-confidence", "medium", "Lowest confidence edge to walk (exact, high, medium, low)")
	return cmd
}
