package cli

import (
	stdctx "context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/docs"
)

// newDocsCmd generates architecture documentation from the index.
//
// The distinction that justifies the command: this is DERIVED, not written. Every
// component, flow and inheritance edge in the output came out of the graph and
// carries the file and line that put it there, so a reader can disagree with the
// evidence rather than with the prose. Regenerating from the same index produces
// byte-identical output, which is what makes the result committable and
// diffable — and what a model-written document, however fluent, cannot offer.
func newDocsCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Generate architecture documentation from the index",
		Long: `Writes ARCHITECTURE.md and CAPABILITIES.md from the indexed graph, with
Mermaid diagrams for component dependencies, endpoint-to-table flows and
inheritance.

Every claim cites the file and line it came from. Nothing here is inferred
prose: a component appears because .repocortex.yml declares it, an edge appears
because an indexer recorded it, and where the graph is silent the document says
so instead of guessing. A repository that declares no components is told that,
rather than being handed a component model invented from its directory names.

Output is deterministic. The same index regenerates byte-identical files, so
these can be committed and reviewed like any other artifact:

  repocortex docs --out docs/generated
  repocortex docs --check          # non-zero if the committed copies have drifted

--check is the useful one in CI. It regenerates in memory and compares, so a
change that alters the architecture without updating its documentation fails the
build rather than being noticed months later.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDocs(cmd, g)
		},
	}

	cmd.Flags().String("out", "", "write ARCHITECTURE.md and CAPABILITIES.md to this directory (default: stdout)")
	cmd.Flags().Bool("check", false, "regenerate and compare against the files in --out; exit non-zero on drift")
	cmd.Flags().Int("max-components", 0, "cap declared components in the output (0 = default)")
	cmd.Flags().Int("max-endpoints", 0, "cap endpoint flow diagrams in the output (0 = default)")
	cmd.Flags().Int("max-depth", 0, "cap call depth followed from a handler (0 = default)")
	return cmd
}

func runDocs(cmd *cobra.Command, g *Global) error {
	ctx := cmd.Context()

	out, _ := cmd.Flags().GetString("out")
	check, _ := cmd.Flags().GetBool("check")
	if check && out == "" {
		return fmt.Errorf("--check needs --out: it compares against files on disk, so it has to know where they are")
	}

	s, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()

	lim := docs.DefaultLimits()
	if n, _ := cmd.Flags().GetInt("max-components"); n > 0 {
		lim.MaxComponents = n
	}
	if n, _ := cmd.Flags().GetInt("max-endpoints"); n > 0 {
		lim.MaxEndpointFlows = n
	}
	if n, _ := cmd.Flags().GetInt("max-depth"); n > 0 {
		lim.MaxCallDepth = n
	}

	gen, err := docs.New(s.reg,
		docs.WithConfigPath(config.RepoConfigName),
		docs.WithLimits(lim))
	if err != nil {
		return fmt.Errorf("docs: %w", err)
	}

	view, err := gen.Derive(ctx, s.runID, g.Config().Architecture)
	if err != nil {
		return fmt.Errorf("docs: derive: %w", err)
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		return emitJSON(cmd.OutOrStdout(), view)
	}

	files := map[string]string{
		"ARCHITECTURE.md": view.ArchitectureMarkdown(),
		"CAPABILITIES.md": view.CapabilityMarkdown(),
	}

	if out == "" {
		for _, name := range []string{"ARCHITECTURE.md", "CAPABILITIES.md"} {
			fmt.Fprintf(cmd.OutOrStdout(), "%s", files[name])
		}
		return nil
	}

	if check {
		return checkDocsDrift(cmd, out, files)
	}
	return writeDocs(ctx, cmd, out, files)
}

// checkDocsDrift compares regenerated documents against what is on disk.
//
// It names every file that differs before failing, rather than stopping at the
// first: a reader running this in CI wants the whole list, not one at a time.
func checkDocsDrift(cmd *cobra.Command, dir string, files map[string]string) error {
	var drifted []string
	for _, name := range []string{"ARCHITECTURE.md", "CAPABILITIES.md"} {
		path := filepath.Join(dir, name)
		have, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied
		if err != nil {
			if os.IsNotExist(err) {
				drifted = append(drifted, name+" (missing)")
				continue
			}
			return fmt.Errorf("docs: read %s: %w", path, err)
		}
		if string(have) != files[name] {
			drifted = append(drifted, name)
		}
	}
	if len(drifted) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "documentation is current (%d file(s) checked).\n", len(files))
		return nil
	}
	for _, name := range drifted {
		fmt.Fprintf(cmd.ErrOrStderr(), "drifted: %s\n", name)
	}
	return fmt.Errorf("%d generated document(s) differ from the index; run 'repocortex docs --out %s'",
		len(drifted), dir)
}

func writeDocs(_ stdctx.Context, cmd *cobra.Command, dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("docs: create %s: %w", dir, err)
	}
	for _, name := range []string{"ARCHITECTURE.md", "CAPABILITIES.md"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(files[name]), 0o600); err != nil {
			return fmt.Errorf("docs: write %s: %w", path, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
	}
	return nil
}
