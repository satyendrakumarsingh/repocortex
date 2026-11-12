package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	acquiregit "github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// defaultImpactListLimit bounds what the human rendering prints per section.
//
// It bounds the PRINTING only. The report itself is already bounded by
// impact.DefaultMaxSeeds and by the traversal's own node limit, and both of
// those say so on the report; this one is a terminal courtesy, so every section
// that elides rows says how many it elided rather than trailing off.
const defaultImpactListLimit = 40

// newImpactCmd answers "what does this change put at risk?" from a git diff.
//
// # Why a diff and not a symbol
//
// `repocortex owners` and the MCP tool analyze_impact both start from ONE symbol
// the caller already knows to be interesting, which presumes the caller has
// already worked out what their change touches. A branch does not arrive in
// that form. It arrives as a range of commits, and the interesting question —
// which of the things I changed has the widest blast radius — is precisely the
// one a per-symbol tool makes the reviewer answer by hand, once per symbol,
// before they may ask it.
//
// # Why it does not shell out, and does not index
//
// The diff is read through internal/acquire/git (go-git, never the `git`
// binary), and the blast radius is read from the run already committed to this
// project's index. Nothing here checks anything out and nothing here indexes: a
// command that silently reindexed would answer about a tree the caller never
// asked about. The consequence is that the index may describe neither end of
// the range, and that is not hidden — impact.AnalyzeDiff compares the run's
// recorded commit against both ends and the report leads with STALE INDEX when
// it matches neither.
func newImpactCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "impact",
		Short: "What a range of commits puts at risk",
		Long: `Reads the files a git range changed, finds the symbols defined in them, and
walks INWARD from each one to everything that depends on it.

  repocortex impact --base HEAD~1 --head HEAD
  repocortex impact --base main --head my-branch --depth 3
  repocortex impact --base origin/main --json

The direction is the whole point. A change to a symbol threatens the code that
DEPENDS ON it, not the code it calls, so this walks callers rather than callees.
Renamed and deleted files are seeded under the name the INDEX has for them —
otherwise the two changes most likely to break a caller would report an empty
blast radius.

Nothing here runs git checkout and nothing here reindexes. Both ends of the
range are read out of the repository, while the symbols and edges come from the
run already committed to this project's index — so the index may describe
neither end of the range. That case is reported as STALE INDEX rather than
answered quietly, because every symbol in a stale report is about other code.

The result is a LOWER BOUND, always. Files the index holds no symbols for are
named rather than counted, unresolved references are reported with the answer,
and an empty result is "nothing found", never "nothing breaks".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImpactDiff(cmd, g)
		},
	}

	cmd.Flags().String("base", "", "the revision to compare FROM — any git revision (required)")
	cmd.Flags().String("head", "", "the revision to compare TO (default: HEAD)")
	cmd.Flags().Int("depth", 0, "hops to follow out from each changed symbol (0 = default)")
	// The default is named, as `export --min-confidence` names its own. An
	// empty string in the flag help is not "no floor": impact.Options applies
	// DefaultMinConfidence, so a reader who does not pass this is already
	// filtering and has no way to know it from here.
	cmd.Flags().String("min-confidence", "",
		fmt.Sprintf("confidence floor for the blast radius (exact|high|medium|low) (default %s)",
			impact.DefaultMinConfidence))
	cmd.Flags().Int("max-seeds", 0, "maximum changed symbols to analyse (0 = default)")
	cmd.Flags().Int("limit", 0, "maximum rows to print per section (0 = default)")
	return cmd
}

func runImpactDiff(cmd *cobra.Command, g *Global) error {
	ctx := cmd.Context()

	base, _ := cmd.Flags().GetString("base")
	base = strings.TrimSpace(base)
	if base == "" {
		// Not defaulted. A blast radius computed over a range the caller did
		// not ask for is a wrong answer wearing the shape of a right one, and
		// "HEAD~1" is only the right guess on one of the several workflows this
		// command serves.
		return errors.New("--base is required: name the revision to compare from, " +
			"e.g. --base HEAD~1 or --base origin/main")
	}
	head := strings.TrimSpace(mustString(cmd, "head"))

	opts, err := impactOptions(cmd)
	if err != nil {
		return err
	}

	root := g.Config().RepoRoot
	repo, err := acquiregit.Open(root)
	if err != nil {
		return fmt.Errorf("impact: open repository at %s: %w", root, err)
	}
	if !repo.IsRepo() {
		return fmt.Errorf("impact: %s is not a git repository; --base and --head name git revisions", root)
	}

	s, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()

	deps := capability.NewDeps(s.reg, graph.NewForRun(s.db, s.runID))
	rep, err := impact.AnalyzeDiff(ctx, deps, s.runID, impact.DiffRequest{
		Changes:   repo,
		BaseRev:   base,
		HeadRev:   head,
		Runs:      s.reg,
		Revisions: repo,
		// The WALKER root, which is what internal/parse recorded every
		// model.File.Path against (see index.go: fs.NewWalker(cfg.RepoRoot)).
		// git.Open searches parent directories for .git, so in a monorepo
		// subdirectory the diff's paths and the index's paths are two different
		// spaces; passing this is what lets impact rebase between them instead
		// of looking every changed file up under a name it never had.
		IndexRoot: canonicalRoot(root),
		MaxSeeds:  mustInt(cmd, "max-seeds"),
		Options:   opts,
	})
	if err != nil {
		return fmt.Errorf("impact: blast radius of %s: %w", rangeLabel(base, head), err)
	}

	if g.JSON {
		return emitJSON(cmd.OutOrStdout(), rep)
	}

	limit := mustInt(cmd, "limit")
	if limit <= 0 {
		limit = defaultImpactListLimit
	}
	writeImpactDiff(cmd.OutOrStdout(), rep, limit, bindingCaveat(rep.Completeness.Resolution))
	return nil
}

// canonicalRoot resolves symlinks in the project root.
//
// internal/acquire/git reports the RESOLVED worktree root, while the project
// root arrives from the caller's own path — and on macOS a directory under
// /tmp differs between the two by a symlink alone. Comparing the unresolved
// forms makes filepath.Rel answer "..", which impact correctly reports as an
// index root outside the repository: a caveat about a problem that does not
// exist. Resolution failure falls back to the original, which is the input the
// caller gave and no worse than not trying.
func canonicalRoot(root string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return root
}

func impactOptions(cmd *cobra.Command) (impact.Options, error) {
	var opts impact.Options
	if n := mustInt(cmd, "depth"); n > 0 {
		opts.MaxDepth = n
	}
	if raw := strings.TrimSpace(mustString(cmd, "min-confidence")); raw != "" {
		c := model.Confidence(raw)
		if !c.Valid() {
			return opts, fmt.Errorf("--min-confidence %q is not one of exact, high, medium, low", raw)
		}
		opts.MinConfidence = c
	}
	return opts, nil
}

func mustInt(cmd *cobra.Command, name string) int {
	n, _ := cmd.Flags().GetInt(name)
	return n
}

func mustString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func rangeLabel(base, head string) string {
	if head == "" {
		head = "HEAD"
	}
	return base + ".." + head
}

// ---------------------------------------------------------------- rendering

// writeImpactDiff prints the report.
//
// The order is chosen so that the sentences which INVALIDATE the report come
// before the report. A stale index, an index root that could not be rebased, or
// a diff whose every file is unindexed each mean the impressive-looking list
// below describes other code, and a reader who meets that fact after the list
// has already read the list.
func writeImpactDiff(out io.Writer, rep impact.DiffReport, limit int, coverage string) {
	fmt.Fprintf(out, "Blast radius of %s\n", rangeLabel(rep.BaseRev, rep.HeadRev))

	// printed records what the top section already said, so the closing
	// section does not say it again. The provenance doubts reach the reader
	// twice by design in the report itself — impact.finishDiff copies them into
	// Completeness.Caveats as well as leaving them in Unknown — which is right
	// for a JSON consumer reading one field and wrong for a human reading one
	// screen.
	printed := make(map[string]bool)
	if !rep.RevisionMatchesRange {
		fmt.Fprintln(out, "\nUNVERIFIED PROVENANCE")
		for _, u := range rep.Unknown {
			if u.Kind == impact.UnknownStaleIndex {
				fmt.Fprintf(out, "\n  %s\n", u.Detail)
				printed[u.Detail] = true
			}
		}
	}

	fmt.Fprintf(out, "\n%s changed; %s found in the index, %d analysed.\n",
		countOf(len(rep.ChangedFiles), "file", "files"),
		countOf(rep.SeedsFound, "changed symbol", "changed symbols"),
		len(rep.Seeds))
	fmt.Fprintf(out, "%s at risk through recorded references, plus %d reachable only through dispatch.\n",
		countOf(rep.TotalImpacted(), "symbol", "symbols"), len(rep.Dispatch))
	if rep.RiskiestSeed != "" {
		fmt.Fprintf(out, "Riskiest change: %s — risk %s (score %d, certainty %s).\n",
			rep.RiskiestSeed, rep.Risk.Level, rep.Risk.Score, rep.Risk.Certainty)
		fmt.Fprintln(out, "  This is the MAXIMUM over the changed symbols, never a total: "+
			"two risks of 40 do not make an 80.")
	}
	if rep.IndexRootPrefix != "" {
		fmt.Fprintf(out, "\nPaths below the CHANGED FILES heading are relative to the repository root; "+
			"symbol paths are relative to the indexed root (%s).\n", rep.IndexRootPrefix)
	}

	fmt.Fprintf(out, "\nCHANGED FILES\n\n")
	writeList(out, rep.ChangedFiles, limit)

	if len(rep.UnindexedFiles) > 0 {
		fmt.Fprintf(out, "\nCHANGED FILES THE INDEX HOLDS NO SYMBOLS FOR (%d)\n\n", len(rep.UnindexedFiles))
		writeList(out, rep.UnindexedFiles, limit)
		fmt.Fprintln(out, "\n  Each of these is a hole in the answer: a non-code file, a file added "+
			"since the index was built, a file that did not parse, or one outside the indexed tree.")
	}

	if len(rep.Seeds) > 0 {
		fmt.Fprintf(out, "\nCHANGED SYMBOLS ANALYSED\n\n")
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "  CHANGE\tKIND\tSYMBOL\tLOCATION\tAT RISK")
		for i, sd := range rep.Seeds {
			if i == limit {
				fmt.Fprintf(tw, "  …and %d more\n", len(rep.Seeds)-limit)
				break
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\n",
				sd.ChangeKind, sd.Kind, sd.Name, location(sd.IndexedPath, 0),
				len(sd.Report.Direct)+len(sd.Report.Indirect))
		}
		_ = tw.Flush()
	}

	if len(rep.Impacted) > 0 {
		fmt.Fprintf(out, "\nWHAT DEPENDS ON THE CHANGED CODE (%d)\n\n", len(rep.Impacted))
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "  HOPS\tSYMBOL\tLOCATION\tCONFIDENCE\tTEST")
		for i, imp := range rep.Impacted {
			if i == limit {
				fmt.Fprintf(tw, "  …and %d more\n", len(rep.Impacted)-limit)
				break
			}
			test := ""
			if imp.IsTest {
				test = "test"
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\n",
				imp.Depth, imp.Name, location(imp.FilePath, 0), imp.Confidence, test)
		}
		_ = tw.Flush()
		fmt.Fprintln(out, "\n  CONFIDENCE is the MINIMUM along the path, not of the last hop: a path "+
			"through one weak edge is a weak claim however strong its other edges are.")
	}

	if len(rep.Dispatch) > 0 {
		fmt.Fprintf(out, "\nREACHABLE ONLY THROUGH DISPATCH (%d)\n\n", len(rep.Dispatch))
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "  HOPS\tSYMBOL\tLOCATION\tWHY")
		for i, d := range rep.Dispatch {
			if i == limit {
				fmt.Fprintf(tw, "  …and %d more\n", len(rep.Dispatch)-limit)
				break
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\n", d.Depth, d.Name, location(d.FilePath, 0), d.Detail)
		}
		_ = tw.Flush()
		fmt.Fprintln(out, "\n  These are symbols the runtime MAY execute. No call site proves any of "+
			"them takes that path, which is why they are not counted above.")
	}

	if rep.Truncated {
		// Printed verbatim. impact aggregates the per-seed reasons into one
		// counted set before the report leaves that package, so there is
		// nothing left here to condense — and condensing it again would be a
		// second grouping of the same field, which is how this section and the
		// caveat below came to print different counts for one fact.
		fmt.Fprintf(out, "\nTRUNCATED: %s\n", rep.TruncatedReason)
	}

	fmt.Fprintf(out, "\nWHAT THIS DOES NOT SAY\n\n")
	// coverage (bindingCaveat) is the resolution-coverage fact stated once,
	// here. impact.UnknownUnresolvedReferences and the Completeness.Caveats
	// resolution sentence say the identical fact again in two more wordings —
	// both are legitimate on their own JSON field, but a human reading one
	// screen does not need the same number restated three times, so both are
	// skipped below rather than printed.
	fmt.Fprintf(out, "  - %s\n", coverage)
	for _, group := range groupUnknowns(rep.Unknown, printed) {
		for _, u := range group {
			printed[u.Detail] = true
		}
		if group[0].Kind == impact.UnknownUnresolvedReferences {
			continue // restates `coverage` above; see the note before this loop
		}
		if len(group) == 1 {
			fmt.Fprintf(out, "  - NOT VISIBLE (%s): %s\n", group[0].Kind, group[0].Detail)
			continue
		}
		fmt.Fprintf(out, "  - NOT VISIBLE (%s) — %d occurrences aggregated because they differ "+
			"only by a count: %s\n", group[0].Kind, len(group), aggregateUnknownDetail(group))
	}
	for _, c := range rep.Completeness.Caveats {
		if printed[c] {
			continue
		}
		printed[c] = true
		if isResolutionRestatement(c) {
			continue // restates `coverage` above; see the note before the Unknown loop
		}
		fmt.Fprintf(out, "  - %s\n", c)
	}
	fmt.Fprintln(out, "  - Static analysis cannot see reflection, dynamic dispatch or runtime "+
		"wiring. An empty or short result is 'nothing found', never proof that nothing breaks.")
}

// countRun matches a run of ASCII digits, so two Unknown.Detail strings that
// differ only in an embedded count normalise to the same template.
var countRun = regexp.MustCompile(`[0-9]+`)

// groupUnknowns partitions a diff's Unknown list into runs that render as one
// line each: entries already printed elsewhere (via `printed`) are dropped,
// and the remainder are grouped by Kind plus their Detail with every digit run
// replaced by a placeholder.
//
// The digit-blind key is deliberately loose about WHERE a number sits in the
// sentence and deliberately strict about everything else: internal/capability/
// impact is off limits for this fix (see the package doc on
// unknownSet.addAll, which already does the equivalent collapse for
// below_confidence_floor upstream, one seed's worth of report at a time), so
// this is a presentation-only re-grouping of what that package already
// produced — it changes what is printed, never what impact computed. Two
// unknowns whose non-numeric text differs — a genuinely different cause — key
// differently and are never folded together; only entries whose prose is
// identical once counts are blanked out collapse, which is exactly the
// "unexpanded_frontier ×14, node count varying" case this fix exists for.
func groupUnknowns(us []impact.Unknown, printed map[string]bool) [][]impact.Unknown {
	order := make([]string, 0, len(us))
	groups := make(map[string][]impact.Unknown, len(us))
	for _, u := range us {
		if printed[u.Detail] {
			continue // already stated at the top, where it changes the reading
		}
		key := string(u.Kind) + "|" + countRun.ReplaceAllString(u.Detail, "#")
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], u)
	}
	out := make([][]impact.Unknown, 0, len(order))
	for _, k := range order {
		out = append(out, groups[k])
	}
	return out
}

// aggregateUnknownDetail states an aggregated group honestly: it says how many
// occurrences were folded in, sums their Count when the underlying unknown is
// sized, and keeps one full sentence as a concrete example rather than only a
// template — a reader who wants the exact wording for one occurrence still has
// it.
func aggregateUnknownDetail(group []impact.Unknown) string {
	var total int64
	bounded := false
	sized := false
	for _, u := range group {
		if u.Count > 0 {
			sized = true
		}
		total += u.Count
		bounded = bounded || u.CountIsLowerBound
	}
	sum := ""
	if sized {
		lb := ""
		if bounded {
			lb = ", a lower bound"
		}
		sum = fmt.Sprintf("Summed across occurrences, at least %d%s. ", total, lb)
	}
	return fmt.Sprintf("%sExample: %s", sum, group[0].Detail)
}

// isResolutionRestatement reports whether a Completeness caveat is the
// resolution-coverage fact already printed as `coverage`, one field of
// capability.NewCompleteness's mandatory-caveats contract restated as
// prose. internal/capability is off limits for this fix, so recognition goes
// by the two sentence openings that function is documented to produce — the
// counted case ("Reference resolution for this index run: ...") and the
// zero-reference case ("...so resolution coverage is unknown...") — rather
// than by changing what that package emits.
func isResolutionRestatement(c string) bool {
	return strings.HasPrefix(c, "Reference resolution for this index run:") ||
		strings.Contains(c, "so resolution coverage is unknown rather than complete")
}

func writeList(out io.Writer, items []string, limit int) {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	for i, s := range sorted {
		if i == limit {
			fmt.Fprintf(out, "  …and %d more\n", len(sorted)-limit)
			return
		}
		fmt.Fprintf(out, "  %s\n", s)
	}
}
