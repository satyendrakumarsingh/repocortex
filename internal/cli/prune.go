package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/prune"
)

func newPruneCmd(g *Global) *cobra.Command {
	var (
		noDocstrings bool
		placeholder  string
	)

	cmd := &cobra.Command{
		Use:   "prune <file>",
		Short: "Strip implementation bodies from a source file, keeping signatures and docstrings",
		Long: `AST Pruning removes method and function bodies from source files while preserving
type declarations, interfaces, function signatures, return types, and docstrings.

This reduces token consumption by up to 90-95%, giving AI coding agents complete
architectural interface context without wasting token budgets on function implementations.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			src, err := os.ReadFile(filePath) //nolint:gosec // the file argument is typed by the local user running the CLI
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}

			opts := prune.DefaultOptions()
			opts.KeepDocstrings = !noDocstrings
			if placeholder != "" {
				opts.Placeholder = placeholder
			}

			pruned, err := prune.PruneFileWithOptions(filePath, src, opts)
			if err != nil {
				return fmt.Errorf("prune file: %w", err)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"path":             filePath,
					"original_bytes":   len(src),
					"pruned_bytes":     len(pruned),
					"saved_tokens_pct": fmt.Sprintf("%.1f%%", float64(len(src)-len(pruned))/float64(len(src))*100),
					"content":          string(pruned),
				})
			}

			_, err = cmd.OutOrStdout().Write(pruned)
			return err
		},
	}

	cmd.Flags().BoolVar(&noDocstrings, "no-docstrings", false, "strip docstrings as well as bodies")
	cmd.Flags().StringVar(&placeholder, "placeholder", "/* ... */", "placeholder text to place in stripped bodies")
	return cmd
}
