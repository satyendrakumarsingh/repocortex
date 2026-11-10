package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/internal/report"
)

// `cortex report`: CI artefacts rendered from other commands' --json output.
//
// It reads files rather than the index on purpose. CI already runs `arch`,
// `missed` and `impact`; re-running them here would produce a second answer
// that could disagree with the one the gate enforced.
func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render SARIF or a pull-request comment from other commands' --json output",
		Long: `Turns the --json output of arch, missed and impact into CI artefacts.

  cortex --json arch   > arch.json
  cortex --json missed > missed.json
  cortex --json impact --base origin/main --head HEAD > impact.json

  cortex report sarif --arch arch.json --missed missed.json --out cortex.sarif
  cortex report pr --impact impact.json --arch arch.json --missed missed.json --out comment.md

An input you do not pass is reported as "not run", never as zero findings.`,
	}
	cmd.AddCommand(newReportSARIFCmd(), newReportPRCmd())
	return cmd
}

type reportInputs struct {
	impactPath, missedPath, archPath string
}

func (in *reportInputs) bind(cmd *cobra.Command, withImpact bool) {
	if withImpact {
		cmd.Flags().StringVar(&in.impactPath, "impact", "", "output of 'cortex --json impact'")
	}
	cmd.Flags().StringVar(&in.missedPath, "missed", "", "output of 'cortex --json missed'")
	cmd.Flags().StringVar(&in.archPath, "arch", "", "output of 'cortex --json arch'")
}

func (in *reportInputs) load() (report.PRInput, error) {
	var pr report.PRInput
	if in.impactPath == "" && in.missedPath == "" && in.archPath == "" {
		return pr, errors.New("pass at least one of --impact, --missed, --arch")
	}
	if in.impactPath != "" {
		pr.Impact = &impact.DiffReport{}
		if err := readJSONFile(in.impactPath, "base_rev", pr.Impact); err != nil {
			return pr, err
		}
	}
	if in.missedPath != "" {
		pr.Missed = &missed.Report{}
		if err := readJSONFile(in.missedPath, "blindspots", pr.Missed); err != nil {
			return pr, err
		}
	}
	if in.archPath != "" {
		// arch --json embeds the Report beside architecture_declared.
		var wrapped struct {
			Declared bool `json:"architecture_declared"`
			architecture.Report
		}
		if err := readJSONFile(in.archPath, "architecture_declared", &wrapped); err != nil {
			return pr, err
		}
		// An undeclared architecture was not checked: report it as "not run"
		// rather than as a clean pass with zero violations.
		if wrapped.Declared {
			pr.Arch = &wrapped.Report
		}
	}
	return pr, nil
}

func newReportSARIFCmd() *cobra.Command {
	var (
		in         reportInputs
		out        string
		pathPrefix string
	)
	cmd := &cobra.Command{
		Use:   "sarif",
		Short: "Write blindspots and architecture violations as one SARIF 2.1.0 run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pr, err := in.load()
			if err != nil {
				return err
			}
			var findings []report.Finding
			if pr.Missed != nil {
				findings = append(findings, report.FromBlindspots(pr.Missed.Blindspots)...)
			}
			if pr.Arch != nil {
				findings = append(findings, report.FromViolations(pr.Arch.Violations)...)
				findings = append(findings, report.FromDataViolations(pr.Arch.DataViolations)...)
			}
			var opts []report.SARIFOption
			if pathPrefix != "" {
				opts = append(opts, report.WithPathPrefix(pathPrefix))
			}
			data, err := json.MarshalIndent(report.BuildSARIF(Version, findings, opts...), "", "  ")
			if err != nil {
				return fmt.Errorf("encode SARIF: %w", err)
			}
			return writeOutput(cmd, out, append(data, '\n'))
		},
	}
	in.bind(cmd, false)
	cmd.Flags().StringVar(&out, "out", "-", "output file, or - for stdout")
	cmd.Flags().StringVar(&pathPrefix, "path-prefix", "", "prefix joined onto every file path, for an index built from a subdirectory of the repository")
	return cmd
}

func newReportPRCmd() *cobra.Command {
	var (
		in      reportInputs
		out     string
		maxRows int
	)
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Render the pull-request comment (Markdown, with a sticky marker)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pr, err := in.load()
			if err != nil {
				return err
			}
			pr.MaxRows = maxRows
			return writeOutput(cmd, out, []byte(report.RenderPR(pr)))
		},
	}
	in.bind(cmd, true)
	cmd.Flags().StringVar(&out, "out", "-", "output file, or - for stdout")
	cmd.Flags().IntVar(&maxRows, "max-rows", report.DefaultPRRows, "rows per table before the rest are counted")
	return cmd
}

// readJSONFile decodes path into v after checking that it carries requiredKey.
// A file handed to the wrong flag would otherwise decode to an all-zero value
// and render as a clean result.
func readJSONFile(path, requiredKey string, v any) error {
	data, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // explicit CLI argument naming a report file
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if _, ok := keys[requiredKey]; !ok {
		return fmt.Errorf("%s does not look like 'cortex --json <cmd>' output (missing %q)", path, requiredKey)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func writeOutput(cmd *cobra.Command, path string, data []byte) error {
	if path == "" || path == "-" {
		if _, err := cmd.OutOrStdout().Write(data); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(filepath.Clean(path), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
