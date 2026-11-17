package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/coverage"
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
	cmd := &cobra.Command{
		Use:   "import <coverage-file>",
		Short: "Import a coverage file (Go coverprofile, LCOV, Cobertura XML, JaCoCo XML)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			if _, err := os.Stat(filePath); err != nil {
				return fmt.Errorf("coverage file %q: %w", filePath, err)
			}

			rep, err := coverage.ParseCoverageFile(filePath)
			if err != nil {
				return fmt.Errorf("parse coverage file: %w", err)
			}

			if g.JSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}

			fmt.Print(rep.Summary())
			return nil
		},
	}
	return cmd
}
