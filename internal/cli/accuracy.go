package cli

import (
	stdctx "context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/bench"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// `repocortex accuracy` — what RepoCortex gets right and what it misses, on YOUR
// repository, adjudicated against YOUR compiler.
//
// The distinction that justifies a command of its own, rather than another flag
// on doctor: doctor --resolution-precision answers "when RepoCortex says
// something, is it right". That is half the question a change-safety tool is
// asked. The other half — "when there IS a caller, does RepoCortex find it" — is
// the expensive one, because a missed caller is a change shipped without its
// blast radius, and precision is structurally incapable of seeing it. A metric
// that only ever measures the errors you make when you speak rewards silence.
//
// docs/accuracy.md declined to publish recall, and its argument was right about
// the comparison it considered: SCIP records REFERENCES, a superset of calls,
// so dividing by every SCIP reference would score the resolver down for not
// emitting a CALLS edge where no call exists. That objection is about the
// denominator. Restricting the denominator to lines where the parser recorded a
// call makes it well defined, and internal/bench states the argument in full.
//
// Three rules this report inherits from the precision report next door, for the
// same reasons:
//
//  1. Every rate is printed beside the counts it came from.
//  2. It never becomes a pass/fail gate. --json exists so a CI job can set its
//     own threshold; the exit status stays 0 whatever the numbers say, because
//     the moment the measurement gates the build somebody tunes the threshold
//     instead of the resolver.
//  3. A language nobody measured is NAMED as unmeasured, never rendered as 0%
//     and never silently omitted.

func newAccuracyCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accuracy",
		Short: "Measure resolution precision, recall and F1 against an imported compiler index",
		Long: `Reports how accurate RepoCortex is on THIS repository, adjudicated against the
compiler index YOU imported — per confidence tier and per language.

Precision alone cannot answer the question a change-safety tool is asked. A
resolver that emits one edge and stays silent everywhere else scores 100%
precision and is useless, because the caller it never mentioned is the one that
breaks. So this reports recall too, and F1 over the same denominator.

RECALL IS RESTRICTED TO CALL SITES, and the restriction is the soundness
argument. SCIP records references — a superset of calls — so recall against
every SCIP reference would count a type annotation as a missed call. The
comparison here runs only over lines where the parser recorded a call AND the
compiler resolved that line to exactly one definition. There, the correct
target is known, and an edge RepoCortex did not produce is a caller it missed.

It needs a run holding both the heuristic graph and compiler facts:

    repocortex index && repocortex resolve
    scip-go --output index.scip
    repocortex scip import index.scip
    repocortex accuracy

A language with no SCIP index imported reports NOT MEASURED. It is never shown
as 0% and never dropped from the table: an unmeasured language that reads like a
measured one is the single failure a trust report cannot survive.

Add --json for CI. The exit status is not a function of the numbers — see
docs/accuracy.md for the method and the caveats.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAccuracy(cmd, g)
		},
	}
	return cmd
}

func runAccuracy(cmd *cobra.Command, g *Global) error {
	ctx := cmd.Context()

	s, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()

	rep, err := collectAccuracy(ctx, s.reg, s.runID)
	if err != nil {
		return err
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		return emitJSON(cmd.OutOrStdout(), rep)
	}
	renderAccuracy(cmd.OutOrStdout(), rep)
	return nil
}

// ------------------------------------------------------------- the report

// accuracyCoverage is how much of the repository was adjudicated at all.
//
// Without it a rate is unreadable: 95% over 40 of 10,000 call sites and 95%
// over 9,000 of them are the same number and different products.
type accuracyCoverage struct {
	// ParsedCallSites is every (file, line) the parser recorded a call on.
	ParsedCallSites int64 `json:"parsed_call_sites"`
	// AdjudicatedCallSites is those the compiler could decide — the recall
	// denominator, |G|.
	AdjudicatedCallSites int64   `json:"adjudicated_call_sites"`
	AdjudicatedFraction  float64 `json:"adjudicated_fraction"`

	HeuristicEdges int64 `json:"heuristic_calls_edges"`
	CompilerFacts  int64 `json:"compiler_facts"`
	// UnambiguousLines are lines the compiler resolved to exactly one target;
	// AmbiguousLines are the rest, excluded because no right answer is defined.
	UnambiguousLines int64 `json:"unambiguous_lines"`
	AmbiguousLines   int64 `json:"ambiguous_lines"`
}

// accuracyLanguage is one language's standing, measured or not.
type accuracyLanguage struct {
	Language       string `json:"language"`
	Files          int64  `json:"files"`
	CallSites      int64  `json:"parsed_call_sites"`
	HeuristicEdges int64  `json:"heuristic_calls_edges"`
	CompilerFacts  int64  `json:"compiler_facts"`
	Adjudicated    int64  `json:"adjudicated_call_sites"`
	// Measured is false when nothing in this language was adjudicated. Not the
	// same as accurate, and rendered as NOT MEASURED rather than as a rate.
	Measured bool `json:"measured"`
}

// accuracyMiss is one reason a call site produced no edge at all.
//
// Reported because "missed" without a cause is unactionable, and the causes
// have different owners: an unresolved reference is a resolver problem, a
// pending one means `repocortex resolve` was never run, and a resolved reference
// with no edge is a call outside any indexed symbol — no caller to attribute
// it to, which is a modelling choice rather than a defect.
type accuracyMiss struct {
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
	CallSites   int64  `json:"call_sites"`
}

type accuracyReport struct {
	// Measured is false when nothing could be adjudicated at all; every rate
	// below is then absent and the report says so rather than printing zeroes.
	Measured bool     `json:"measured"`
	Measures []string `json:"measures"`
	// Restriction is the soundness argument, carried inline. A recall figure
	// separated from the set it was computed over is the misreading this whole
	// measurement exists to avoid, and a screenshot does not carry a footnote.
	Restriction string `json:"restriction"`
	ReadingRule string `json:"reading_rule"`

	Overall   bench.LanguageAccuracy   `json:"overall"`
	Languages []bench.LanguageAccuracy `json:"languages"`

	Coverage         accuracyCoverage   `json:"coverage"`
	LanguageCoverage []accuracyLanguage `json:"language_coverage"`
	// Unmeasured names languages carrying call sites that no compiler index
	// adjudicated.
	Unmeasured []string       `json:"unmeasured_languages"`
	Misses     []accuracyMiss `json:"missed_call_site_causes"`
	Warnings   []string       `json:"warnings"`
	NotThis    []string       `json:"what_this_is_not"`
	Reference  string         `json:"reference"`
}

// ------------------------------------------------------------- gathering
//
// The SQL is NOT here. It is internal/registry.AccuracyAdjudication, which is
// where CLAUDE.md §2 puts a query, and this command is one of its two callers —
// the portal's /api/accuracy is the other.
//
// It used to live here, and the argument for that was that this was one
// command's read model. That argument held exactly until a second caller needed
// the same numbers. Two copies of a measurement are two measurements, and the
// failure is silent: the terminal and the portal publish different F1 scores
// for one index and nothing errors. What this file owns now is the report —
// which counts to fold, how to phrase them, and when to warn.

// accuracyAdjudicator is the read surface this command needs: the verdicts and
// the coverage they were computed over, never a rate.
//
// Declared here at the consumer (CLAUDE.md §1) and satisfied by
// *registry.Registry. The rates stay bench.AccuracyOf's job, so a port that
// returned them would make it possible for this command and the portal to fold
// the same outcomes two different ways.
type accuracyAdjudicator interface {
	AccuracyAdjudication(ctx stdctx.Context, runID int64) (registry.Adjudication, error)
}

// collectAccuracy folds one run's adjudication into the report this command
// prints.
//
// Every rate below comes from bench.AccuracyOf over the registry's outcomes.
// Nothing here counts anything itself.
func collectAccuracy(ctx stdctx.Context, src accuracyAdjudicator, runID int64) (accuracyReport, error) {
	adj, err := src.AccuracyAdjudication(ctx, runID)
	if err != nil {
		return accuracyReport{}, fmt.Errorf("accuracy: adjudicate run %d: %w", runID, err)
	}

	outcomes := make([]bench.CallSiteOutcome, 0, len(adj.Outcomes))
	for _, o := range adj.Outcomes {
		outcomes = append(outcomes, bench.CallSiteOutcome{
			Language: o.Language, Tier: o.Tier, Correct: o.Correct, Count: o.Count,
		})
	}
	langs, overall := bench.AccuracyOf(outcomes)

	cov := accuracyCoverage{
		ParsedCallSites:      adj.Coverage.ParsedCallSites,
		AdjudicatedCallSites: overall.Adjudicated,
		HeuristicEdges:       adj.Coverage.HeuristicEdges,
		CompilerFacts:        adj.Coverage.CompilerFacts,
		UnambiguousLines:     adj.Coverage.UnambiguousLines,
		AmbiguousLines:       adj.Coverage.AmbiguousLines,
	}
	if cov.ParsedCallSites > 0 {
		cov.AdjudicatedFraction = float64(overall.Adjudicated) / float64(cov.ParsedCallSites)
	}

	adjudicated := map[string]int64{}
	for _, l := range langs {
		adjudicated[l.Language] = l.Adjudicated
	}
	langCov := make([]accuracyLanguage, 0, len(adj.Languages))
	unmeasured := []string{}
	for _, l := range adj.Languages {
		row := accuracyLanguage{
			Language:       l.Language,
			Files:          l.Files,
			CallSites:      l.CallSites,
			HeuristicEdges: l.HeuristicEdges,
			CompilerFacts:  l.CompilerFacts,
			Adjudicated:    adjudicated[l.Language],
		}
		row.Measured = row.Adjudicated > 0
		if !row.Measured {
			unmeasured = append(unmeasured, row.Language)
		}
		langCov = append(langCov, row)
	}

	misses := make([]accuracyMiss, 0, len(adj.Misses))
	for _, m := range adj.Misses {
		misses = append(misses, accuracyMiss{
			Reason: m.Reason, Explanation: m.Explanation, CallSites: m.CallSites,
		})
	}

	rep := accuracyReport{
		Measured:         overall.Adjudicated > 0,
		Measures:         []string{"precision", "recall", "f1"},
		Restriction:      bench.Restriction,
		ReadingRule:      "act on `high`, show `medium`, ignore `low`",
		Overall:          overall,
		Languages:        langs,
		Coverage:         cov,
		LanguageCoverage: langCov,
		Unmeasured:       unmeasured,
		Misses:           misses,
		NotThis:          bench.NotMeasured,
		Reference:        "docs/accuracy.md",
	}
	rep.Warnings = accuracyWarnings(rep)
	return rep, nil
}

// accuracyWarnings names the conditions under which the tables must not be read
// as a clean bill of health.
func accuracyWarnings(rep accuracyReport) []string {
	out := []string{}
	if !rep.Measured {
		return out
	}
	if len(rep.Unmeasured) > 0 {
		carry, it := "carries", "it"
		if len(rep.Unmeasured) > 1 {
			carry, it = "carry", "them"
		}
		out = append(out, fmt.Sprintf(
			"%s %s call sites that no compiler index in this run could adjudicate. "+
				"Those are UNMEASURED here, which is not the same as accurate — import a "+
				"SCIP index for %s before reading this report as covering them.",
			strings.Join(rep.Unmeasured, ", "), carry, it))
	}
	if rep.Overall.Adjudicated < accuracySampleFloor {
		out = append(out, fmt.Sprintf(
			"Only %s call site(s) were adjudicated. The percentages are arithmetic on a "+
				"small sample and will move materially with a few call sites.",
			humanCount(rep.Overall.Adjudicated)))
	}
	// Recall is the number this command exists to surface, so a low one is
	// stated as a consequence rather than left for the reader to derive: a
	// missed call site is a caller `repocortex callers` will not list. The two
	// ways of failing are separated, because only one of them is invisible to
	// precision and they have different fixes.
	if rep.Overall.All.Recall < accuracyRecallFloor {
		out = append(out, fmt.Sprintf(
			"Recall across all tiers is %s: %s of %s adjudicated call site(s) did not get a "+
				"correct CALLS edge — %s produced no edge at all and %s named the wrong "+
				"definition. The missed ones are absent from `repocortex callers`, from blast "+
				"radius, and from `repocortex verify` — treat an empty result there as unknown "+
				"rather than as none.",
			bench.Rate(rep.Overall.All.Recall, rep.Overall.All.Measured),
			humanCount(rep.Overall.Adjudicated-rep.Overall.All.TruePositives),
			humanCount(rep.Overall.Adjudicated),
			humanCount(rep.Overall.Missed),
			humanCount(rep.Overall.All.FalsePositives)))
	}
	if rep.Coverage.AdjudicatedFraction < accuracyCoverageFloor {
		out = append(out, fmt.Sprintf(
			"Only %.1f%% of parsed call sites were adjudicated. The rates describe that "+
				"slice of the repository, not the repository.",
			100*rep.Coverage.AdjudicatedFraction))
	}
	return out
}

// accuracySampleFloor is where a percentage stops surviving a handful of call
// sites moving. Not a threshold anything passes or fails — it only decides
// whether the reader is warned to distrust the decimal point.
const accuracySampleFloor = 200

// accuracyRecallFloor is where "RepoCortex found the callers" stops being a fair
// summary. Set at 80% deliberately: recall has never been published before, so
// there is no measured band to sit under the way highTierFloor sits under
// docs/accuracy.md's 86.8–96.1%. Until one exists this warns wherever one call
// site in five is missed, which is a number a reader should see whether or not
// the project has decided how to feel about it.
const accuracyRecallFloor = 0.80

// accuracyCoverageFloor is where the adjudicated slice stops standing in for
// the repository.
const accuracyCoverageFloor = 0.25

// ------------------------------------------------------------- rendering

func renderAccuracy(out io.Writer, rep accuracyReport) {
	_, _ = fmt.Fprintf(out, "RESOLUTION ACCURACY\n")

	if !rep.Measured {
		_, _ = fmt.Fprintf(out,
			"Not measured. No call site in this index could be adjudicated: it holds no\n"+
				"compiler facts, or none of them landed on a line the parser saw a call on.\n"+
				"Nothing here — including every language listed below — has been checked\n"+
				"against a compiler. Run 'repocortex scip import <index.scip>' first.\n")
		renderAccuracyLanguages(out, rep)
		return
	}

	// The restriction before the numbers. It is the reason the recall column is
	// allowed to exist, and a reader who meets it after the table has already
	// read the table without it.
	_, _ = fmt.Fprintf(out, "%s\n\n", wrapIndented("  ", "  ", rep.Restriction))

	for _, warn := range rep.Warnings {
		_, _ = fmt.Fprintf(out, "%s\n", wrapIndented("  ! ", "    ", warn))
	}

	// The aggregate is printed only when there is something to aggregate. On a
	// single-language repository it would be a byte-identical copy of the one
	// language below it, and a duplicated table teaches the reader that the two
	// headings mean nothing.
	if len(rep.Languages) > 1 {
		renderAccuracyTiers(out, "ALL LANGUAGES", rep.Overall)
	}
	for _, l := range rep.Languages {
		renderAccuracyTiers(out, strings.ToUpper(l.Language), l)
	}

	_, _ = fmt.Fprintf(out, "\nHOW TO READ IT\n")
	_, _ = fmt.Fprintf(out, "  Precision is how often an edge at that tier is right.\n")
	_, _ = fmt.Fprintf(out, "  Recall is how many of the %s adjudicated call site(s) that tier caught.\n",
		humanCount(rep.Overall.Adjudicated))
	_, _ = fmt.Fprintf(out, "  Every tier shares the same recall denominator, so the tier recalls sum\n"+
		"  to the all-tiers recall and F1 compares like with like.\n")
	high := tierOf(rep.Overall, "high")
	_, _ = fmt.Fprintf(out, "  Filtering --min-confidence high keeps %s of the calls at %s precision;\n"+
		"  taking every tier keeps %s at %s.\n",
		bench.Rate(high.Recall, high.Measured), bench.Rate(high.Precision, high.PrecisionMeasured),
		bench.Rate(rep.Overall.All.Recall, rep.Overall.All.Measured),
		bench.Rate(rep.Overall.All.Precision, rep.Overall.All.PrecisionMeasured))

	renderAccuracyMisses(out, rep)

	c := rep.Coverage
	_, _ = fmt.Fprintf(out, "\nCOVERAGE\n")
	_, _ = fmt.Fprintf(out, "  Adjudicated %s of %s parsed call site(s) (%.1f%%). The rest sit on lines\n"+
		"  no compiler fact covers, and are neither right nor wrong above.\n",
		humanCount(c.AdjudicatedCallSites), humanCount(c.ParsedCallSites),
		100*c.AdjudicatedFraction)
	_, _ = fmt.Fprintf(out, "  Against %s compiler fact(s) on %s unambiguous line(s); %s ambiguous\n"+
		"  line(s) were excluded. The resolver produced %s heuristic CALLS edge(s) overall.\n",
		humanCount(c.CompilerFacts), humanCount(c.UnambiguousLines),
		humanCount(c.AmbiguousLines), humanCount(c.HeuristicEdges))

	renderAccuracyLanguages(out, rep)

	// Deliberately unwrapped: these are the sentences that keep the report
	// honest, and a caveat somebody greps for must stay greppable.
	_, _ = fmt.Fprintf(out, "\nWHAT THIS IS NOT\n")
	for _, s := range rep.NotThis {
		_, _ = fmt.Fprintf(out, "%s\n", wrapIndented("  ", "  ", s))
	}
	_, _ = fmt.Fprintf(out, "  The method, and the caveats this summary does not repeat, are in %s.\n",
		rep.Reference)
}

func renderAccuracyTiers(out io.Writer, title string, la bench.LanguageAccuracy) {
	_, _ = fmt.Fprintf(out, "\n%s\n", title)
	if !la.Measured {
		_, _ = fmt.Fprintf(out, "  NOT MEASURED — no call site adjudicated for it.\n")
		return
	}
	// SITES, not EDGES: every column here counts adjudicated CALL SITES. That
	// is the unit change recall forced — a missed call site has no edge to
	// count — and labelling it "edges" would quietly re-import the unit that
	// cannot express a miss.
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  TIER\tSITES\tRIGHT\tWRONG\tPRECISION\tRECALL\tF1")
	for _, t := range append(append([]bench.TierAccuracy{}, la.Tiers...), la.All) {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Tier, humanCount(t.Retrieved), humanCount(t.TruePositives),
			humanCount(t.FalsePositives),
			bench.Rate(t.Precision, t.PrecisionMeasured),
			bench.Rate(t.Recall, t.Measured),
			bench.Rate(t.F1, t.Measured))
	}
	// The row precision cannot produce. It is in the table rather than in a
	// footnote because it belongs to the same denominator as every row above.
	_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		"missed", humanCount(la.Missed), "—", "—", "—", "—", "—")
	_ = w.Flush()
	_, _ = fmt.Fprintf(out, "  %s adjudicated call site(s); %s produced no edge at all.\n",
		humanCount(la.Adjudicated), humanCount(la.Missed))
}

func renderAccuracyMisses(out io.Writer, rep accuracyReport) {
	if rep.Overall.Missed == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\nWHERE THE MISSES COME FROM\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  CAUSE\tCALL SITES\tMEANING")
	for _, m := range rep.Misses {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", m.Reason, humanCount(m.CallSites), m.Explanation)
	}
	_ = w.Flush()
}

func renderAccuracyLanguages(out io.Writer, rep accuracyReport) {
	if len(rep.LanguageCoverage) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\nLANGUAGES\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  LANGUAGE\tFILES\tCALL SITES\tCALLS EDGES\tCOMPILER FACTS\tADJUDICATED\tSTATUS")
	for _, l := range rep.LanguageCoverage {
		status := "measured"
		if !l.Measured {
			status = "NOT MEASURED — no SCIP index imported for it"
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			l.Language, humanCount(l.Files), humanCount(l.CallSites),
			humanCount(l.HeuristicEdges), humanCount(l.CompilerFacts),
			humanCount(l.Adjudicated), status)
	}
	_ = w.Flush()
}

// tierOf returns one tier's row, or an unmeasured zero when absent.
func tierOf(la bench.LanguageAccuracy, tier string) bench.TierAccuracy {
	for _, t := range la.Tiers {
		if t.Tier == tier {
			return t
		}
	}
	return bench.TierAccuracy{Tier: tier}
}
