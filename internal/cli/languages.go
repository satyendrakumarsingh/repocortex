package cli

// Prints what RepoCortex can actually do per language, in the terms a user
// cares about: can it find definitions, can it bind references, can it tell
// two same-named methods apart. The numbers come from the query maps at run
// time, so this output and the README table cannot disagree.

import (
	"fmt"
	"text/tabwriter"

	"github.com/satyendrakumarsingh/repocortex/internal/parse"

	"github.com/spf13/cobra"
)

type languageReport struct {
	Typed    []string `json:"typed"`
	Resolved []string `json:"resolved"`
	Parsed   []string `json:"parsed"`
	Total    int      `json:"total"`
}

func newLanguagesCmd(g *Global) *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "languages",
		Short: "Report what RepoCortex can do for each language it parses",
		Long: `Languages fall into three tiers.

  typed     definitions, imports, receiver and arity discrimination.
            References here can reach 'high' confidence.
  resolved  definitions and imports. References bind across files.
  parsed    definitions and full-text search only. No cross-file
            relationship is claimed for these.

The tiers are derived from the query set at run time, not from a list, so
this output always matches what the binary can actually do.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := languageReport{
				Typed:    parse.LanguagesInTier(parse.TierTyped),
				Resolved: parse.LanguagesInTier(parse.TierResolved),
				Parsed:   parse.LanguagesInTier(parse.TierParsed),
			}
			rep.Total = len(rep.Typed) + len(rep.Resolved) + len(rep.Parsed)

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), rep)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "TIER\tCOUNT\tWHAT YOU GET\n")
			fmt.Fprintf(w, "typed\t%d\t%s\n", len(rep.Typed), parse.TierTyped.Description())
			fmt.Fprintf(w, "resolved\t%d\t%s\n", len(rep.Resolved), parse.TierResolved.Description())
			fmt.Fprintf(w, "parsed\t%d\t%s\n", len(rep.Parsed), parse.TierParsed.Description())
			fmt.Fprintf(w, "total\t%d\t\n", rep.Total)
			if err := w.Flush(); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "\ntyped:    %v\n", rep.Typed)
			fmt.Fprintf(cmd.OutOrStdout(), "resolved: %v\n", rep.Resolved)
			if verbose {
				fmt.Fprintf(cmd.OutOrStdout(), "parsed:   %v\n", rep.Parsed)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "\n(%d parsed-tier languages not listed; pass --verbose)\n", len(rep.Parsed))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&verbose, "verbose", false, "list the parsed-tier languages too")
	return cmd
}
