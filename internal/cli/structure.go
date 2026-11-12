package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/deadcode"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/hotspots"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

// This file holds the terminal commands for the questions structure.go in
// internal/mcp answers for agents: unused definitions, change hotspots, a
// route between two symbols, a type's hierarchy and the tests for a symbol.
//
// Each command renders for a person. The agent form of the same answer is
// `repocortex tool call <tool>`, which returns the text an MCP client receives;
// the capability underneath is one function either way.

// ---------------------------------------------------------------- dead-code

func newDeadCodeCmd(g *Global) *cobra.Command {
	var (
		pathPrefix      string
		limit           int
		all             bool
		includeExported bool
	)
	cmd := &cobra.Command{
		Use:   "dead-code",
		Short: "List definitions nothing in the index calls or references",
		Long: `Lists functions, methods and types with no caller in the index, graded by how
far that can be trusted:

  likely    unexported, cleanly parsed, and no unbound reference anywhere is
            spelled like it. Every ordinary reason for a missing caller was
            checked and ruled out.
  possible  one of those could not be ruled out. The row says which.

  repocortex dead-code
  repocortex dead-code --path internal/billing/
  repocortex dead-code --all --limit 100
  repocortex dead-code --include-exported

Only the likely tier is listed unless --all is given. The possible tier is
mostly what a static index could not bind rather than what is unused, so it is
a list to search through, not a list to act on.

A definition with no caller in the index is NOT proven unused. Reflection,
dependency injection, framework registration and code in other repositories all
reach code without a reference a static index can bind. Search for the name
before deleting anything listed here.

Tests, entry points and, unless --include-exported is given, exported
definitions are left out: an empty caller list is their normal state. Languages
the index parses without binding references (see 'repocortex languages') are not
judged at all, and the count of what that hides is printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			opts := deadcode.Options{
				Limit: limit, PathPrefix: pathPrefix,
				IncludeExported: includeExported, Judgeable: bindsReferences,
			}
			// An exported definition is never better than possible, so asking
			// for them is asking for that tier.
			if !all && !includeExported {
				opts.MinCertainty = deadcode.CertaintyLikely
			}
			deps := capability.NewDeps(s.reg, graph.NewForRun(s.db, s.runID))
			rep, err := deadcode.Find(ctx, deps, s.reg, s.runID, opts)
			if err != nil {
				return err
			}
			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), rep)
			}
			renderDeadCode(cmd.OutOrStdout(), rep, includeExported, all || includeExported)
			return nil
		},
	}
	cmd.Flags().StringVar(&pathPrefix, "path", "", "only report definitions in files under this path prefix")
	cmd.Flags().IntVarP(&limit, "limit", "n", deadcode.DefaultLimit, "maximum candidates to list")
	cmd.Flags().BoolVar(&all, "all", false, "also list candidates graded possible")
	cmd.Flags().BoolVar(&includeExported, "include-exported", false, "also list exported definitions (implies --all)")
	return cmd
}

// textCheckNote says how far the likely count can be trusted.
func textCheckNote(rep *deadcode.Report) string {
	if rep.LikelyUnchecked == 0 {
		return fmt.Sprintf("Every candidate the graph graded likely was also checked against the indexed "+
			"source text (%d checked), so the likely count is what survived both.", rep.TextChecked)
	}
	return fmt.Sprintf("Every row shown as likely was also checked against the indexed source text "+
		"(%d checked). %d more were graded likely from the graph alone and not checked, so the likely "+
		"count is an upper bound. Narrow with --path to check them.", rep.TextChecked, rep.LikelyUnchecked)
}

func renderDeadCode(out io.Writer, rep *deadcode.Report, includeExported, showingPossible bool) {
	total := rep.Likely + rep.Possible
	switch {
	case total == 0:
		fmt.Fprintln(out, "No unreferenced definitions found.")
	case len(rep.Candidates) == 0:
		fmt.Fprintf(out, "%d definition(s) have no caller in the index: %d likely unused, %d possibly.\n\n",
			total, rep.Likely, rep.Possible)
		fmt.Fprintln(out, "Nothing is graded likely unused.")
		if !showingPossible && rep.Possible > 0 {
			fmt.Fprintf(out, "Pass --all to list the %d possible candidate(s); most are uses the index could not bind.\n", rep.Possible)
		}
		fmt.Fprintf(out, "\n%s\n", wrapText(textCheckNote(rep), 100))
	default:
		fmt.Fprintf(out, "%d definition(s) have no caller in the index: %d likely unused, %d possibly.\n\n",
			total, rep.Likely, rep.Possible)
		usedReasons := map[deadcode.Reason]bool{}
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "CERTAINTY\tKIND\tSYMBOL\tLOCATION\tLINES\tWHY IT IS NOT CERTAIN")
		for _, c := range rep.Candidates {
			why := "-"
			if len(c.Reasons) > 0 {
				parts := make([]string, 0, len(c.Reasons))
				for _, r := range c.Reasons {
					parts = append(parts, r.Label())
					usedReasons[r] = true
				}
				why = strings.Join(parts, "; ")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s:%d\t%d\t%s\n",
				c.Certainty, c.Kind, c.Name, c.Path, c.Line, c.Lines(), why)
		}
		_ = tw.Flush()
		shown := rep.Likely
		if showingPossible {
			shown = total
		}
		if len(rep.Candidates) < shown {
			fmt.Fprintf(out, "\nShowing %d of %d, most certain and largest first. Raise --limit or narrow with --path.\n",
				len(rep.Candidates), shown)
		} else if !showingPossible && rep.Possible > 0 {
			fmt.Fprintf(out, "\n%d more are graded possible; pass --all to list them.\n", rep.Possible)
		}
		// The table cell holds a label; the sentence behind each label used
		// above is printed once, here.
		first := true
		for _, r := range deadcode.Reasons() {
			if !usedReasons[r] {
				continue
			}
			if first {
				fmt.Fprintln(out)
				first = false
			}
			fmt.Fprintf(out, "  %s: %s.\n", r.Label(), r.Text())
		}
		fmt.Fprintf(out, "\n%s\n", wrapText(textCheckNote(rep), 100))
	}

	fmt.Fprintln(out)
	if n := rep.Skipped - rep.SkippedUnjudged; n > 0 {
		what := "tests, entry points and exported definitions (--include-exported lists the last)"
		if includeExported {
			what = "tests and entry points"
		}
		fmt.Fprintf(out, "Left out as expected: %d %s.\n", n, what)
	}
	if rep.SkippedUnjudged > 0 {
		fmt.Fprintf(out, "Not judged: %d definition(s) in %s. The index parses those languages without "+
			"binding references, so every definition in them would look unused.\n",
			rep.SkippedUnjudged, strings.Join(rep.UnjudgedLangs, ", "))
	}
	if rep.Completeness.Truncated {
		fmt.Fprintf(out, "TRUNCATED: %s.\n", rep.Completeness.TruncatedReason)
	}
	if rs := rep.Completeness.Resolution; rs.Total > 0 && rep.Completeness.BindingRate < capability.CoverageFloor {
		fmt.Fprintf(out, "\nCOVERAGE TOO LOW: only %.0f%% of the references that name something this index "+
			"defines are bound. Most of this list is more likely unbound than unused. Run 'repocortex resolve' first.\n",
			100*rep.Completeness.BindingRate)
	}
	fmt.Fprintf(out, "\n%s\n", wrapText(deadcode.Limits, 100))
}

// ---------------------------------------------------------------- hotspots

func newHotspotsCmd(g *Global) *cobra.Command {
	var (
		pathPrefix   string
		limit        int
		commits      int
		includeTests bool
		allFiles     bool
	)
	cmd := &cobra.Command{
		Use:   "hotspots",
		Short: "Rank files by how often they change against how much code they hold",
		Long: `Ranks files by change frequency x size over recent git history, and lists for
each the files usually committed with it.

  repocortex hotspots
  repocortex hotspots --path internal/ --commits 1000
  repocortex hotspots --json

A large file nobody touches costs nothing, and neither does a three-line file
touched daily. The files that cost are in both sets, and this is the list of
them. SCORE is relative to this repository and this commit window: it orders
the list and is not a quality measurement.

CHANGES WITH names the files most often committed together with each hotspot.
When no call or import connects two of them, that is a dependency no static
analysis can see, 'repocortex impact' included. Check those files when you change
the hotspot.

Size is the file's size on disk, used as a proxy for how much there is to
understand; it is not a complexity metric. Commits touching more than 30 files
are left out of CHANGES WITH, because a formatting sweep couples everything to
everything. Test files are left out unless --include-tests is given, and files
with no definitions (bundled assets, generated text, data) unless --all-files.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			hist := openHistory(g)
			rep, err := hotspots.Rank(ctx, hotspotHistory{hist}, s.reg, s.runID, hotspots.Options{
				Limit: limit, CommitWindow: commits, PathPrefix: pathPrefix, IncludeTests: includeTests,
				IncludeNonCode: allFiles,
			})
			if err != nil {
				return err
			}
			if hist.note != "" {
				rep.Notes = append([]string{strings.TrimSuffix(hist.note, " The blast radius below is still the index's.")}, rep.Notes...)
			}
			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), rep)
			}
			renderHotspots(cmd.OutOrStdout(), rep)
			return nil
		},
	}
	cmd.Flags().StringVar(&pathPrefix, "path", "", "only rank files under this path prefix")
	cmd.Flags().IntVarP(&limit, "limit", "n", hotspots.DefaultLimit, "maximum files to list")
	cmd.Flags().IntVar(&commits, "commits", hotspots.DefaultCommitWindow, "how many recent commits to read")
	cmd.Flags().BoolVar(&includeTests, "include-tests", false, "also rank test files")
	cmd.Flags().BoolVar(&allFiles, "all-files", false, "also rank files with no definitions (assets, generated text, data)")
	return cmd
}

func renderHotspots(out io.Writer, rep *hotspots.Report) {
	if len(rep.Hotspots) == 0 {
		fmt.Fprintln(out, "No hotspots to rank.")
	} else {
		fmt.Fprintf(out, "Hotspots from the last %d commit(s), touching %d indexed file(s).\n\n",
			rep.CommitsRead, rep.FilesChanged)
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "SCORE\tFILE\tCOMMITS\tSIZE\tSYMBOLS\tINBOUND\tLAST CHANGED\tCHANGES WITH")
		for _, h := range rep.Hotspots {
			last := "-"
			if h.LastChanged > 0 {
				last = time.Unix(h.LastChanged, 0).UTC().Format("2006-01-02")
			}
			with := "-"
			if len(h.ChangesWith) > 0 {
				parts := make([]string, 0, len(h.ChangesWith))
				for _, p := range h.ChangesWith {
					parts = append(parts, fmt.Sprintf("%s (%.0f%%)", p.Path, 100*p.Share))
				}
				with = strings.Join(parts, ", ")
			}
			fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%d\t%d\t%s\t%s\n",
				h.Score, h.Path, h.Commits, humanBytes(h.Size), h.Symbols, h.Dependents, last, with)
		}
		_ = tw.Flush()
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(out, "\n%s\n", wrapText(n, 100))
	}
}

// ---------------------------------------------------------------- path

func newPathCmd(g *Global) *cobra.Command {
	var (
		fromPath, toPath string
		maxDepth         int
		maxPaths         int
	)
	cmd := &cobra.Command{
		Use:   "path <from> <to>",
		Short: "Show how one symbol reaches another, hop by hop",
		Long: `Finds the shortest chains of calls and references from one symbol to another.

  repocortex path handleCallback saveTransaction
  repocortex path Execute openReadOnly --max-depth 8

Each hop is printed with the file and line that evidences it and the confidence
of that one relationship. A route is as trustworthy as its weakest hop.

Only SHORTEST routes are shown. "No route found" is not proof of independence:
a route passes only through relationships the index bound, so one unresolved
call, an interface dispatch or a callback anywhere along the way breaks it.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if maxDepth < 1 || maxPaths < 1 {
				return errors.New("--max-depth and --max-paths must be at least 1")
			}
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			from, fromAll, err := resolveTarget(ctx, s, strings.TrimSpace(args[0]), fromPath)
			if err != nil {
				return err
			}
			to, toAll, err := resolveTarget(ctx, s, strings.TrimSpace(args[1]), toPath)
			if err != nil {
				return err
			}

			routes, err := graph.NewForRun(s.db, s.runID).Paths(ctx, from.ID, to.ID, maxPaths, maxDepth)
			if err != nil {
				return fmt.Errorf("path search: %w", err)
			}

			type hop struct {
				From       string `json:"from"`
				To         string `json:"to"`
				Kind       string `json:"kind"`
				Confidence string `json:"confidence"`
				Source     string `json:"source"`
				File       string `json:"evidence_file"`
				Line       int    `json:"evidence_line"`
			}
			var rendered [][]hop
			for _, route := range routes {
				var hops []hop
				for i := 0; i+1 < len(route); i++ {
					e, ok, err := s.reg.EdgeBetween(ctx, s.runID, route[i], route[i+1])
					if err != nil {
						return err
					}
					if !ok {
						continue
					}
					hops = append(hops, hop{From: e.SrcName, To: e.DstName, Kind: e.Kind,
						Confidence: e.Confidence, Source: e.Source, File: e.EvidenceFile, Line: e.EvidenceLine})
				}
				rendered = append(rendered, hops)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"index_revision": s.runID,
					"from":           map[string]any{"name": from.Name, "path": from.FilePath, "line": from.StartLine},
					"to":             map[string]any{"name": to.Name, "path": to.FilePath, "line": to.StartLine},
					"max_depth":      maxDepth,
					"routes":         rendered,
				})
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s (%s:%d)  ->  %s (%s:%d)\n\n",
				from.Name, from.FilePath, from.StartLine, to.Name, to.FilePath, to.StartLine)
			if len(rendered) == 0 {
				fmt.Fprintf(out, "No route found within %d hop(s).\n\n", maxDepth)
				fmt.Fprintln(out, wrapText("This is not proof that none exists. A route passes only through "+
					"relationships the index bound, so one unresolved call, an interface dispatch or a "+
					"callback along the way breaks it. Try 'repocortex callers "+to.Name+"' and 'repocortex callees "+
					from.Name+"' to see where the two halves stop.", 100))
				fmt.Fprintf(out, "\n%s\n", emptyCaveat(ctx, s))
			}
			for i, hops := range rendered {
				fmt.Fprintf(out, "Route %d (%d hop(s)):\n", i+1, len(hops))
				tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
				for j, h := range hops {
					fmt.Fprintf(tw, "  %d.\t%s\t-%s->\t%s\t%s:%d\t%s (%s)\n",
						j+1, h.From, strings.ToLower(h.Kind), h.To, h.File, h.Line, h.Confidence, h.Source)
				}
				_ = tw.Flush()
				fmt.Fprintln(out)
			}
			if len(rendered) > 0 {
				fmt.Fprintln(out, "Only shortest routes are shown; longer ones may exist.")
			}
			reportAmbiguity(out, fromAll, from.Name)
			reportAmbiguity(out, toAll, to.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&fromPath, "from-path", "", "disambiguate <from> by file path substring")
	cmd.Flags().StringVar(&toPath, "to-path", "", "disambiguate <to> by file path substring")
	cmd.Flags().IntVar(&maxDepth, "max-depth", 6, "longest route to look for, in hops")
	cmd.Flags().IntVar(&maxPaths, "max-paths", 3, "maximum shortest routes to print")
	return cmd
}

// ---------------------------------------------------------------- implementations

func newImplementationsCmd(g *Global) *cobra.Command {
	var (
		path          string
		limit         int
		minConfidence string
	)
	cmd := &cobra.Command{
		Use:   "implementations <name>",
		Short: "Show what implements or extends a type, and what it implements or extends",
		Long: `Lists a type's hierarchy in both directions.

  repocortex implementations Store
  repocortex implementations PaymentGateway --min-confidence high

Use it before changing an interface, an abstract method or a base class, when
the callers of the declaration are not the whole story.

Only DECLARED relationships are visible from syntax: extends, implements, an
embedded type. A Go type that satisfies an interface without naming it appears
only after 'repocortex scip import'. An empty list means none declared, not none
exist.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("name must not be empty")
			}
			filter := registry.EdgeFilter{
				Kinds: []string{string(model.EdgeImplements), string(model.EdgeExtends)},
			}
			if minConfidence != "" {
				filter.MinConfidence = model.Confidence(minConfidence)
				if !filter.MinConfidence.Valid() {
					return fmt.Errorf("--min-confidence %q: want exact, high, medium or low", minConfidence)
				}
			}
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			target, all, err := resolveTarget(ctx, s, name, path)
			if err != nil {
				return err
			}
			subtypes, err := s.reg.IncomingEdges(ctx, s.runID, target.ID, limit, filter)
			if err != nil {
				return fmt.Errorf("relationship lookup: %w", err)
			}
			supertypes, err := s.reg.OutgoingEdges(ctx, s.runID, target.ID, limit, filter)
			if err != nil {
				return fmt.Errorf("relationship lookup: %w", err)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"name": name, "index_revision": s.runID,
					"target": map[string]any{
						"id": target.ID, "kind": target.Kind, "path": target.FilePath, "line": target.StartLine,
					},
					"ambiguous":  len(all) > 1,
					"subtypes":   subtypes,
					"supertypes": supertypes,
				})
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s (%s) at %s:%d\n", target.Name, target.Kind, target.FilePath, target.StartLine)
			section := func(title string, edges []registry.EdgeHit, incoming bool) {
				fmt.Fprintf(out, "\n%s (%d)\n", title, len(edges))
				if len(edges) == 0 {
					fmt.Fprintln(out, "  none declared")
					return
				}
				tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
				for _, e := range edges {
					other := e.DstName
					if incoming {
						other = e.SrcName
					}
					fmt.Fprintf(tw, "  %s\t%s\t%s:%d\t%s (%s)\n",
						strings.ToLower(e.Kind), other, e.EvidenceFile, e.EvidenceLine, e.Confidence, e.Source)
				}
				_ = tw.Flush()
			}
			section("Implemented or extended by", subtypes, true)
			section("Implements or extends", supertypes, false)
			if len(subtypes) == 0 && len(supertypes) == 0 {
				fmt.Fprintf(out, "\n%s\n", emptyCaveat(ctx, s))
			}
			reportAmbiguity(out, all, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "disambiguate by file path substring")
	cmd.Flags().IntVarP(&limit, "limit", "n", 100, "maximum relationships to report in each direction")
	cmd.Flags().StringVar(&minConfidence, "min-confidence", "",
		"drop edges below this confidence (exact, high, medium, low)")
	return cmd
}

// ---------------------------------------------------------------- tests

func newTestsCmd(g *Global) *cobra.Command {
	var minConfidence string
	cmd := &cobra.Command{
		Use:   "tests <symbol>",
		Short: "Find the tests that reach a symbol",
		Long: `Lists the tests that exercise a symbol, and says how each was found.

  repocortex tests updateStatus
  repocortex tests TransactionService --min-confidence high

A test found through a recorded call edge is evidence the test runs the code. A
test found by its name or by sitting beside the source file is a convention, and
the SIGNAL column says which you have. Run the first kind first.

"No tests found" is reported as a finding and still not as proof: a test that
reaches the symbol through an unresolved reference is one the index cannot see.

To find the tests for everything a change touches, use 'repocortex impact'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("symbol must not be empty")
			}
			var opts tests.Options
			if minConfidence != "" {
				opts.MinConfidence = model.Confidence(minConfidence)
				if !opts.MinConfidence.Valid() {
					return fmt.Errorf("--min-confidence %q: want exact, high, medium or low", minConfidence)
				}
			}
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			deps := capability.NewDeps(s.reg, graph.NewForRun(s.db, s.runID))
			rep, err := tests.FindTestsWith(ctx, deps, s.runID, name, opts)
			if err != nil {
				if errors.Is(err, capability.ErrTargetNotFound) {
					return fmt.Errorf("no symbol named %q in the index\n%s", name, absenceDiagnosis(ctx, s, name))
				}
				return err
			}
			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), rep)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s at %s\n\n", rep.Target.Name, rep.Target.FilePath)
			if len(rep.Tests) == 0 {
				fmt.Fprintf(out, "No tests found for %s.\n\n%s\n", rep.Target.Name, emptyCaveat(ctx, s))
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "TEST\tFILE\tKIND\tSIGNAL\tCONFIDENCE")
			for _, t := range rep.Tests {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.FilePath, t.Kind, t.Strongest, t.Confidence)
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\n%d test(s); %d reached through a recorded call edge, the rest by convention.\n",
				len(rep.Tests), rep.ProvenByGraph)
			if rep.Truncated {
				fmt.Fprintf(out, "TRUNCATED: %s.\n", rep.TruncatedReason)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&minConfidence, "min-confidence", "",
		"only follow edges at or above this confidence (exact, high, medium, low)")
	return cmd
}
