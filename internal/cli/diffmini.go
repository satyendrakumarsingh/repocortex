package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/diffmini"
)

func newDiffMinimizeCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "minimize-diff [patch-file]",
		Short: "Compress a unified diff into a token-efficient representation for AI agents",
		Long: `Diff Minimizer compresses unified diffs (from git diff or pull requests) into
structured, token-efficient formats. It strips redundant headers, extracts enclosing
symbol scopes, and compresses large unchanged context runs.

Reads from a specified patch file or directly from stdin:
  git diff | repocortex minimize-diff
  repocortex minimize-diff patch.diff --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var diffBytes []byte
			var err error

			if len(args) == 1 {
				diffBytes, err = os.ReadFile(args[0])
				if err != nil {
					return fmt.Errorf("read diff file: %w", err)
				}
			} else {
				// Read from stdin
				diffBytes, err = io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
			}

			if len(diffBytes) == 0 {
				return fmt.Errorf("empty diff: pass a file or pipe diff content to stdin")
			}

			rep, err := diffmini.ParseAndCompress(string(diffBytes))
			if err != nil {
				return fmt.Errorf("compress diff: %w", err)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), rep)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Compressed diff: %d -> %d bytes (%.1f%% token reduction, ~%d tokens)\n\n",
				rep.OriginalBytes, rep.CompressedBytes, rep.SavedTokensPct, rep.EstimatedTokens)
			fmt.Fprint(cmd.OutOrStdout(), rep.FormatText())
			return nil
		},
	}

	return cmd
}
