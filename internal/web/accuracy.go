package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/bench"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The accuracy screen: what RepoCortex gets right on YOUR repository, and — the
// half that matters — what it MISSES.
//
// # Why this is the flagship and not a footnote
//
// Every other screen in this portal is RepoCortex talking about a repository.
// This one is RepoCortex being measured, on the reader's own code, against a
// compiler. It exists to publish the number that makes the product look worse:
// precision alone is trivially gamed by a resolver that emits one edge and
// stays silent, and a missed caller is a change shipped without its blast
// radius — the exact failure this product is sold to prevent. So RECALL and the
// MISS COUNTS are first-class fields on the payload, never optional extras a
// client could omit and still render a complete-looking screen.
//
// # The restriction is part of the number
//
// Recall is computed only over call sites: lines where the parser recorded a
// call AND the compiler resolved that line to exactly one definition. The
// argument for that denominator is carried inline in Restriction, from
// internal/bench, because a rate separated from the set it was computed over is
// precisely the misreading a screenshot produces.
//
// # Nothing here is scored in this package
//
// The rates come from bench.AccuracyOf, the same function `repocortex accuracy`
// folds its outcomes with — so a tier's precision on this screen and in the
// terminal cannot disagree. What this file does is ask for the adjudication,
// render it, and, when it cannot have it, say exactly why rather than printing
// zeroes.
//
// # The fact that makes recall computable, and where it is read
//
// Recall needs one thing precision does not: what the RESOLVER claimed about an
// edge BEFORE a compiler import upgraded it in place. UpgradeEdges rewrites a
// weaker row rather than adding one beside it, so every call the resolver got
// RIGHT stops saying source='source_code' the moment a compiler fact confirms
// it — and a query reading the live column then counts only the calls it got
// WRONG. That is the documented inversion that once reported 51.2% recall as
// 0.5%.
//
// Migration 0012 keeps the overwritten claim in prior_source/prior_confidence,
// and registry.AccuracyAdjudication is the one place that reads it back. This
// screen holds a port for that method rather than a query of its own, because
// the same method is what `repocortex accuracy` folds — one computation, two
// callers, and no way for the terminal and the portal to publish two different
// F1 scores for one index.
//
// When the port is unwired the screen still serves the precision half, which
// the narrower PrecisionReader can adjudicate, plus coverage, plus guidance
// naming the command that produces the rest.

// AccuracySource is the adjudication surface the accuracy screen needs.
//
// One method, returning the verdicts and the coverage they were computed over
// rather than any rate: the rates are bench.AccuracyOf's job, and a port that
// returned them would make it possible for the portal and the CLI to publish
// two different F1 scores for one run.
//
// It returns the REGISTRY's type, exactly as PrecisionReader does, and this
// package adapts it below. The other direction — a registry method returning
// web.Adjudication — would have the Repository adapter importing an HTTP
// delivery layer, which is the arrow CLAUDE.md §1 forbids and which
// tests/architecture executes.
//
// Declared here at the consumer (CLAUDE.md §1) and resolved by asserting on the
// Registry the server was given, the same way PackReader is: a caller whose
// Registry does not satisfy it still gets the precision half, without a change
// to Deps.
type AccuracySource interface {
	AccuracyAdjudication(ctx context.Context, runID int64) (registry.Adjudication, error)
}

// PrecisionReader is the narrower surface the production registry ALREADY
// satisfies: heuristic CALLS edges adjudicated against imported compiler facts,
// per tier.
//
// It cannot answer recall — it counts edges, not call sites, and it has no
// denominator for the calls no edge was produced for. It is read anyway,
// because a screen that can say "high is 97.8% precise over 5,587 adjudicated
// lines, and this portal cannot measure how many callers were missed" is
// strictly more useful than one that says nothing at all.
type PrecisionReader interface {
	ResolutionPrecision(ctx context.Context, runID int64) (registry.PrecisionReport, error)
}

// Compile-time proof the production adapter satisfies both ports. The narrower
// one is kept and asserted separately because it is the fallback: a Registry
// implementation that can compare tiers but cannot adjudicate call sites still
// lights up half this screen.
var (
	_ AccuracySource  = (*registry.Registry)(nil)
	_ PrecisionReader = (*registry.Registry)(nil)
)

// Adjudication is one run's compiler-adjudicated call sites, before any rate is
// computed from them, in the vocabulary this package renders from.
//
// It is the registry's Adjudication translated into bench's outcome type and
// this package's JSON-tagged coverage — a projection, not a second read model.
// adjudicationFrom is the only thing that builds one.
type Adjudication struct {
	// Outcomes is one row per (language, tier, correctness) group. An empty
	// Tier means the resolver produced no edge at that call site — the miss.
	Outcomes []bench.CallSiteOutcome

	// Coverage is what the rates will have been computed over.
	Coverage AccuracyCoverage

	// Unadjudicated names languages carrying parsed call sites that no compiler
	// index could decide.
	//
	// It cannot be derived from Outcomes, and that is the whole reason it is
	// carried: a language nothing adjudicated produces no outcome row at all,
	// so folding Outcomes can only ever name languages that WERE measured.
	// Deriving the unmeasured list from them would report every language as
	// measured, which is the single failure a trust report cannot survive.
	Unadjudicated []string

	// Misses breaks the no-edge call sites down by what the RESOLVER did with
	// the reference there.
	Misses []AccuracyMiss
}

// adjudicationFrom projects the registry's adjudication into this package's.
//
// The projection is deliberate rather than an embedding: the registry counts
// rows and this package publishes JSON, and a shared struct would make the wire
// format a property of the storage adapter.
func adjudicationFrom(adj registry.Adjudication) Adjudication {
	out := Adjudication{
		Outcomes: make([]bench.CallSiteOutcome, 0, len(adj.Outcomes)),
		Coverage: AccuracyCoverage{
			ParsedCallSites:  adj.Coverage.ParsedCallSites,
			HeuristicEdges:   adj.Coverage.HeuristicEdges,
			CompilerFacts:    adj.Coverage.CompilerFacts,
			UnambiguousLines: adj.Coverage.UnambiguousLines,
			AmbiguousLines:   adj.Coverage.AmbiguousLines,
		},
		Unadjudicated: []string{},
		Misses:        make([]AccuracyMiss, 0, len(adj.Misses)),
	}
	adjudicated := map[string]bool{}
	for _, o := range adj.Outcomes {
		out.Outcomes = append(out.Outcomes, bench.CallSiteOutcome{
			Language: o.Language, Tier: o.Tier, Correct: o.Correct, Count: o.Count,
		})
		adjudicated[o.Language] = true
	}
	for _, l := range adj.Languages {
		if l.CallSites > 0 && !adjudicated[l.Language] {
			out.Unadjudicated = append(out.Unadjudicated, l.Language)
		}
	}
	for _, m := range adj.Misses {
		out.Misses = append(out.Misses, AccuracyMiss{
			Reason: m.Reason, Explanation: m.Explanation, CallSites: m.CallSites,
		})
	}
	return out
}

// AccuracyCoverage is how much of the repository was adjudicated at all.
//
// Without it a rate is unreadable: 95% over 40 of 10,000 call sites and 95%
// over 9,000 of them are the same number and different products. It is also
// what an EMPTY accuracy screen reports instead of nothing — the index's
// coverage is the answer to "why is this blank", and it distinguishes "no
// compiler index was imported" from "this repository has no calls".
type AccuracyCoverage struct {
	// ParsedCallSites is every (file, line) the parser recorded a call on.
	ParsedCallSites int64 `json:"parsed_call_sites"`

	// AdjudicatedCallSites is those the compiler could decide — the recall
	// denominator.
	AdjudicatedCallSites int64   `json:"adjudicated_call_sites"`
	AdjudicatedFraction  float64 `json:"adjudicated_fraction"`

	// HeuristicEdges is every CALLS edge the resolver produced; CompilerFacts
	// is every fact imported from a compiler-backed index. CompilerFacts of
	// zero is the whole explanation for an empty screen.
	HeuristicEdges int64 `json:"heuristic_calls_edges"`
	CompilerFacts  int64 `json:"compiler_facts"`

	// UnambiguousLines are lines the compiler resolved to exactly one target —
	// the only lines where a right answer is defined. AmbiguousLines are the
	// rest, excluded and counted rather than silently scored.
	UnambiguousLines int64 `json:"unambiguous_lines"`
	AmbiguousLines   int64 `json:"ambiguous_lines"`
}

// AccuracyMiss is one reason an adjudicable call site produced no edge at all.
//
// Reported because "missed" without a cause is unactionable and the causes have
// different owners: unresolved is a resolver problem, pending means `repocortex
// resolve` was never run, and resolved-with-no-edge is a call outside any
// indexed symbol — a modelling choice rather than a defect.
type AccuracyMiss struct {
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
	CallSites   int64  `json:"call_sites"`
}

// AccuracyScores is the headline: precision, recall, F1, and the counts every
// one of them came from.
//
// Recall and Missed are fields, not derivations, and they are never omitempty.
// A client cannot render this struct and leave out the number that makes the
// product look worse.
type AccuracyScores struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`

	// AdjudicableCallSites is the recall denominator: call sites where the
	// parser saw a call and the compiler named exactly one target.
	AdjudicableCallSites int64 `json:"adjudicable_call_sites"`

	// Answered is the call sites the resolver produced some edge for; Missed is
	// the ones it produced NONE for. Missed is the error precision is
	// structurally incapable of seeing.
	Answered int64 `json:"answered_call_sites"`
	Missed   int64 `json:"missed_call_sites"`

	TruePositives  int64 `json:"true_positives"`
	FalsePositives int64 `json:"false_positives"`

	// PrecisionMeasured is false when the resolver answered nothing at all, in
	// which case precision is undefined and a zero would read as "answered, and
	// wrong". RecallMeasured is false when nothing was adjudicated.
	PrecisionMeasured bool `json:"precision_measured"`
	RecallMeasured    bool `json:"recall_measured"`
}

// AccuracyTier is one confidence tier's standing.
//
// Relevant is the recall denominator and is identical for every tier, which is
// what makes the tiers comparable. It is 0, with RecallMeasured false, when the
// screen could only be given the precision half.
type AccuracyTier struct {
	Tier string `json:"tier"`

	TruePositives  int64 `json:"true_positives"`
	FalsePositives int64 `json:"false_positives"`
	Retrieved      int64 `json:"retrieved"`
	Relevant       int64 `json:"relevant"`

	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`

	PrecisionMeasured bool `json:"precision_measured"`
	RecallMeasured    bool `json:"recall_measured"`

	// Meaning is the legend's sentence for this tier, carried here so a tier
	// row is readable without joining it against confidence_legend.
	Meaning string `json:"meaning,omitempty"`
}

// AccuracyLanguage is one language's standing, measured or not.
//
// A language with call sites and no imported compiler index is NAMED as
// unmeasured, never rendered as 0% and never dropped: an unmeasured language
// that reads like a measured one is the single failure a trust report cannot
// survive.
type AccuracyLanguage struct {
	Language    string `json:"language"`
	Adjudicated int64  `json:"adjudicated_call_sites"`
	Answered    int64  `json:"answered_call_sites"`
	Missed      int64  `json:"missed_call_sites"`

	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`

	Measured bool `json:"measured"`
}

// AccuracyMissBreakdown is the split the screen exists to publish beside recall.
type AccuracyMissBreakdown struct {
	// Total is every adjudicable call site that produced no edge.
	Total int64 `json:"missed_call_sites"`

	// Unresolved is the resolver binding the reference to nothing; Ambiguous is
	// several candidates and no way to choose. They are separated because they
	// have different fixes: unresolved is extraction, ambiguous is inference.
	Unresolved int64 `json:"unresolved"`
	Ambiguous  int64 `json:"ambiguous"`

	// Other is every remaining cause, itemised in Causes.
	Other int64 `json:"other"`

	Causes []AccuracyMiss `json:"causes"`

	// Measured is false when the miss breakdown could not be computed. Its
	// zeroes are then placeholders, not findings.
	Measured bool `json:"measured"`
}

// AccuracyResponse is the whole accuracy screen.
type AccuracyResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Measured is false when nothing could be adjudicated. Every rate below is
	// then a placeholder and Guidance says why.
	Measured bool `json:"measured"`

	// Restriction is the soundness argument for the recall denominator,
	// carried inline. A recall figure separated from the set it was computed
	// over is the misreading this measurement exists to avoid, and a screenshot
	// does not carry a footnote.
	Restriction string `json:"restriction"`
	ReadingRule string `json:"reading_rule"`

	Scores    AccuracyScores        `json:"scores"`
	Tiers     []AccuracyTier        `json:"tiers"`
	Languages []AccuracyLanguage    `json:"languages"`
	Coverage  AccuracyCoverage      `json:"coverage"`
	Misses    AccuracyMissBreakdown `json:"misses"`

	// Unmeasured names languages carrying call sites that no compiler index
	// adjudicated.
	Unmeasured []string `json:"unmeasured_languages"`

	Warnings []string `json:"warnings"`
	NotThis  []string `json:"what_this_is_not"`

	Reference string `json:"reference"`

	Help *Guidance `json:"guidance,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

// maxAccuracyLanguages bounds the per-language table. Whatever it cuts is
// counted in the omission block.
const maxAccuracyLanguages = 40

func (s *Server) handleAccuracy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := AccuracyResponse{
		Meta: meta, Legend: Legend(),
		Restriction: bench.Restriction,
		ReadingRule: "act on `high`, show `medium`, ignore `low`",
		Tiers:       []AccuracyTier{}, Languages: []AccuracyLanguage{},
		Unmeasured: []string{}, Warnings: []string{},
		NotThis:   bench.NotMeasured,
		Reference: "docs/accuracy.md",
		Misses:    AccuracyMissBreakdown{Causes: []AccuracyMiss{}},
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label:   "Index this repository first",
			Detail:  "There is no committed run to measure. Accuracy is a property of an index, not of the tool.",
			Command: "repocortex index && repocortex resolve",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	var om Omissions

	// The full adjudication, when the composition root has wired one.
	if src, wired := s.deps.Registry.(AccuracySource); wired {
		adj, err := src.AccuracyAdjudication(ctx, run.ID)
		if err != nil {
			s.fail(w, r, fmt.Errorf("web: adjudicate run %d: %w", run.ID, err))
			return
		}
		s.fillMeasuredAccuracy(&resp, adjudicationFrom(adj), &om)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	// The precision half, which the production registry can already adjudicate.
	prec, wired := s.deps.Registry.(PrecisionReader)
	if !wired {
		resp.Help = &Guidance{
			Title: "This portal cannot measure itself",
			Detail: "The accuracy screen adjudicates RepoCortex's own CALLS edges against a compiler " +
				"index. The registry this portal was given exposes neither the adjudication nor the " +
				"per-tier precision comparison, so there is nothing to measure with — which is a gap " +
				"in this build's wiring, not a verdict on how references are bound.",
			Steps: []string{
				"repocortex accuracy — the same measurement, from the command line",
				"docs/accuracy.md — the method, the denominators and the caveats",
			},
		}
		resp.Next = accuracyNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	rep, err := prec.ResolutionPrecision(ctx, run.ID)
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: resolution precision for run %d: %w", run.ID, err))
		return
	}
	s.fillPrecisionOnly(&resp, rep, &om)
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// fillMeasuredAccuracy renders a full adjudication.
//
// The arithmetic is bench.AccuracyOf's, not this package's. What happens here is
// projection: bench's per-language rows become the response's rows, and the
// aggregate row becomes the headline.
func (s *Server) fillMeasuredAccuracy(resp *AccuracyResponse, adj Adjudication, om *Omissions) {
	langs, overall := bench.AccuracyOf(adj.Outcomes)

	cov := adj.Coverage
	cov.AdjudicatedCallSites = overall.Adjudicated
	if cov.ParsedCallSites > 0 {
		cov.AdjudicatedFraction = float64(overall.Adjudicated) / float64(cov.ParsedCallSites)
	}
	resp.Coverage = cov
	resp.Measured = overall.Adjudicated > 0

	resp.Scores = AccuracyScores{
		Precision:            overall.All.Precision,
		Recall:               overall.All.Recall,
		F1:                   overall.All.F1,
		AdjudicableCallSites: overall.Adjudicated,
		Answered:             overall.Answered,
		Missed:               overall.Missed,
		TruePositives:        overall.All.TruePositives,
		FalsePositives:       overall.All.FalsePositives,
		PrecisionMeasured:    overall.All.PrecisionMeasured,
		RecallMeasured:       overall.Adjudicated > 0,
	}

	meanings := tierMeanings()
	for _, t := range overall.Tiers {
		resp.Tiers = append(resp.Tiers, AccuracyTier{
			Tier: t.Tier, TruePositives: t.TruePositives, FalsePositives: t.FalsePositives,
			Retrieved: t.Retrieved, Relevant: t.Relevant,
			Precision: t.Precision, Recall: t.Recall, F1: t.F1,
			PrecisionMeasured: t.PrecisionMeasured, RecallMeasured: t.Measured,
			Meaning: meanings[t.Tier],
		})
	}

	rows := make([]AccuracyLanguage, 0, len(langs)+len(adj.Unadjudicated))
	for _, l := range langs {
		rows = append(rows, AccuracyLanguage{
			Language: l.Language, Adjudicated: l.Adjudicated, Answered: l.Answered,
			Missed: l.Missed, Precision: l.All.Precision, Recall: l.All.Recall,
			F1: l.All.F1, Measured: l.Measured,
		})
		if !l.Measured {
			resp.Unmeasured = append(resp.Unmeasured, l.Language)
		}
	}
	// A language nothing adjudicated has no outcome row, so bench cannot see it
	// and the loop above cannot name it. It is appended here, marked unmeasured
	// and carrying no rate — never rendered as 0%, and never dropped.
	for _, lang := range adj.Unadjudicated {
		rows = append(rows, AccuracyLanguage{Language: lang})
		resp.Unmeasured = append(resp.Unmeasured, lang)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Adjudicated != rows[j].Adjudicated {
			return rows[i].Adjudicated > rows[j].Adjudicated
		}
		return rows[i].Language < rows[j].Language
	})
	sort.Strings(resp.Unmeasured)
	resp.Languages = capped(rows, maxAccuracyLanguages, om, ReasonRowBudget,
		fmt.Sprintf("Languages beyond the first %d (ordered by compiler-checked call sites) are not "+
			"listed. The overall row above is computed over ALL of them, so the headline is not "+
			"shortened — only the table is.", maxAccuracyLanguages),
		"repocortex accuracy --json")

	resp.Misses = missBreakdown(adj.Misses, overall.Missed)

	if overall.Missed > 0 {
		om.Note(ReasonUnresolvedReferences,
			fmt.Sprintf("%d adjudicable call site(s) produced NO edge at all. Those callers are "+
				"absent from every blast radius on this portal — not shown weakly, absent. This is "+
				"the error precision cannot see, which is why it is a headline field here.",
				overall.Missed),
			"repocortex resolve, then re-import the compiler index")
	}
	if !resp.Measured {
		resp.Help = unadjudicatedGuidance(cov)
	}
	resp.Warnings = accuracyWarnings(*resp)
	resp.Next = accuracyNextAction(*resp)
}

// unadjudicatedGuidance says why a wired, working adjudication measured nothing.
//
// The two causes are different problems with different fixes and must not share
// a sentence. No compiler facts at all means nothing has ever judged this index.
// Compiler facts that adjudicated no call site means the two tools looked at the
// same repository and their evidence never met on a line — a coverage fact about
// the SCIP index, not a verdict on how references are bound.
func unadjudicatedGuidance(cov AccuracyCoverage) *Guidance {
	if cov.CompilerFacts == 0 {
		return &Guidance{
			Title: "No compiler index has been imported into this run",
			Detail: fmt.Sprintf(
				"Accuracy is checked against a compiler, and this index holds %d compiler "+
					"fact(s). That is why this screen is empty: nothing has judged the %d name-inferred "+
					"CALLS edge(s) RepoCortex produced here. It is not a claim that those edges are "+
					"wrong, and it is not a claim that they are right — they are UNMEASURED, which "+
					"is a third thing.",
				cov.CompilerFacts, cov.HeuristicEdges),
			Steps: []string{
				"scip-go --output index.scip   (or scip-typescript / scip-python for your language)",
				"repocortex scip import index.scip",
				"repocortex accuracy — the same measurement in the terminal",
			},
		}
	}
	return &Guidance{
		Title: "A compiler index is present, and it settled no call site",
		Detail: fmt.Sprintf(
			"This index holds %d compiler fact(s) over %d unambiguously-resolved line(s), and none "+
				"of those lines is one the parser recorded a call on — so the recall denominator is "+
				"empty and every rate here would be arithmetic over nothing. The likely cause is a "+
				"SCIP index built from a different tree, or built for a language whose call sites "+
				"this parser does not record.",
			cov.CompilerFacts, cov.UnambiguousLines),
		Steps: []string{
			"Rebuild the SCIP index from the same checkout, then repocortex scip import index.scip",
			"repocortex accuracy — the same measurement, with the per-language coverage",
			"docs/accuracy.md — why recall is restricted to call sites",
		},
	}
}

// fillPrecisionOnly renders what the registry alone can adjudicate.
//
// The recall fields are present and marked unmeasured rather than absent. A
// client that omitted them when they were unavailable would render a screen
// that looks complete on exactly the repositories where the honest answer is
// "we did not measure the thing that matters most".
func (s *Server) fillPrecisionOnly(resp *AccuracyResponse, rep registry.PrecisionReport, om *Omissions) {
	resp.Coverage = AccuracyCoverage{
		HeuristicEdges:   rep.HeuristicEdges,
		CompilerFacts:    rep.CompilerEdges,
		UnambiguousLines: rep.JudgeableLines,
		AmbiguousLines:   rep.AmbiguousLines,
	}

	meanings := tierMeanings()
	var tp, fp int64
	rows := make([]AccuracyTier, 0, len(rep.Rows))
	for _, row := range rep.Rows {
		tp += row.Agree
		fp += row.Disagree
		rows = append(rows, AccuracyTier{
			Tier: row.Confidence, TruePositives: row.Agree, FalsePositives: row.Disagree,
			Retrieved: row.Judged, Relevant: 0,
			Precision: row.Precision(), PrecisionMeasured: row.Judged > 0,
			RecallMeasured: false, Meaning: meanings[row.Confidence],
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return model.Confidence(rows[i].Tier).Rank() > model.Confidence(rows[j].Tier).Rank()
	})
	resp.Tiers = rows

	resp.Scores = AccuracyScores{
		TruePositives: tp, FalsePositives: fp,
		PrecisionMeasured: tp+fp > 0,
		RecallMeasured:    false,
	}
	if resp.Scores.PrecisionMeasured {
		resp.Scores.Precision = float64(tp) / float64(tp+fp)
	}
	// Measured stays false: this screen's claim is precision AND recall, and
	// half of it is a different, smaller claim that must not inherit the word.
	resp.Measured = false

	resp.Misses = AccuracyMissBreakdown{Causes: []AccuracyMiss{}}

	if rep.CompilerEdges == 0 {
		resp.Help = &Guidance{
			Title: "No compiler index has been imported into this run",
			Detail: fmt.Sprintf(
				"Accuracy is checked against a compiler, and this index holds %d compiler "+
					"fact(s). That is why this screen is empty: nothing has judged the %d name-inferred "+
					"CALLS edge(s) RepoCortex produced here. It is not a claim that those edges are "+
					"wrong, and it is not a claim that they are right — they are UNMEASURED, which "+
					"is a third thing.",
				rep.CompilerEdges, rep.HeuristicEdges),
			Steps: []string{
				"scip-go --output index.scip   (or scip-typescript / scip-python for your language)",
				"repocortex scip import index.scip",
				"repocortex accuracy — precision, recall and F1 over the imported facts",
			},
		}
	} else {
		om.Note(ReasonEvidenceBudget,
			fmt.Sprintf("Recall and F1 are NOT served on this screen. Computing them needs what the "+
				"INDEX claimed about each edge before a compiler import upgraded it in place, and "+
				"that is not on this portal's read port. Deriving it from the live columns instead is "+
				"the documented inversion that once reported 51.2%% recall as 0.5%%, so the number is "+
				"withheld rather than guessed. Precision below is over %d compiler-checked line(s) and is "+
				"real.", rep.JudgeableLines),
			"repocortex accuracy")
		resp.Help = &Guidance{
			Title: "Half of this measurement is served here; the other half is one command away",
			Detail: fmt.Sprintf(
				"This index holds %d compiler fact(s) over %d unambiguously-resolved line(s), so the "+
					"per-tier PRECISION below is measured on your own code. RECALL — how many callers "+
					"RepoCortex missed entirely — is the number that matters more and this portal cannot "+
					"compute it correctly from its read port. It is reported as unmeasured rather than "+
					"as zero.", rep.CompilerEdges, rep.JudgeableLines),
			Steps: []string{
				"repocortex accuracy — precision, recall, F1 and the miss breakdown",
				"docs/accuracy.md — why recall is restricted to call sites",
			},
		}
	}
	resp.Warnings = accuracyWarnings(*resp)
	resp.Next = accuracyNextAction(*resp)
}

// missBreakdown splits the no-edge call sites by cause.
//
// total comes from bench rather than from summing the causes: the two are
// computed by different queries over the same population and a discrepancy
// between them is a finding, not something to paper over by deriving one from
// the other.
func missBreakdown(causes []AccuracyMiss, total int64) AccuracyMissBreakdown {
	out := AccuracyMissBreakdown{Total: total, Causes: causes, Measured: true}
	if out.Causes == nil {
		out.Causes = []AccuracyMiss{}
	}
	for _, c := range causes {
		switch c.Reason {
		case "unresolved":
			out.Unresolved += c.CallSites
		case "ambiguous":
			out.Ambiguous += c.CallSites
		default:
			out.Other += c.CallSites
		}
	}
	return out
}

// tierMeanings indexes the served legend by tier, so a tier row carries the
// same sentence the legend does rather than a second phrasing of it.
func tierMeanings() map[string]string {
	out := map[string]string{}
	for _, t := range Legend() {
		out[t.Confidence] = t.Meaning
	}
	return out
}

// accuracyWarnings names the conditions under which the tables must not be read
// as a clean bill of health.
func accuracyWarnings(resp AccuracyResponse) []string {
	out := []string{}
	if len(resp.Unmeasured) > 0 {
		out = append(out, fmt.Sprintf(
			"%d language(s) carry call sites that no compiler index checked: %v. They are NOT "+
				"MEASURED, which is not the same as accurate.", len(resp.Unmeasured), resp.Unmeasured))
	}
	if resp.Coverage.AmbiguousLines > 0 {
		out = append(out, fmt.Sprintf(
			"%d line(s) the compiler resolved to more than one target were excluded: no right answer "+
				"is defined there, so scoring them either way would be invented.",
			resp.Coverage.AmbiguousLines))
	}
	if resp.Coverage.ParsedCallSites > 0 && resp.Coverage.AdjudicatedFraction < 0.25 {
		out = append(out, fmt.Sprintf(
			"Only %.1f%% of this repository's parsed call sites were adjudicable. The rates above "+
				"describe that slice, not the repository.", 100*resp.Coverage.AdjudicatedFraction))
	}
	if !resp.Scores.RecallMeasured {
		out = append(out, "Recall is UNMEASURED on this screen. A precision figure alone rewards a "+
			"index for staying silent, and silence is what a missed caller looks like.")
	}
	return out
}

// accuracyNextAction is the concrete thing to do about the numbers above.
func accuracyNextAction(resp AccuracyResponse) NextAction {
	switch {
	case resp.Coverage.CompilerFacts == 0:
		return NextAction{
			Label:   "Import a compiler index to measure this repository",
			Detail:  "Until one is imported, every edge on every screen is unmeasured on YOUR code.",
			Command: "repocortex scip import index.scip",
		}
	case resp.Scores.Missed > 0:
		return NextAction{
			Label: fmt.Sprintf("%d caller(s) were missed entirely", resp.Scores.Missed),
			Detail: "Each is a call site with a known correct target that produced no edge, so it is " +
				"absent from every blast radius here. Open the miss breakdown before trusting a radius.",
			Screen: "blast",
		}
	case !resp.Scores.RecallMeasured:
		return NextAction{
			Label:   "Run the full measurement from the command line",
			Detail:  "Recall and the miss breakdown are the half this portal cannot compute.",
			Command: "repocortex accuracy",
		}
	default:
		return NextAction{
			Label:  "Read each tier beside the count it came from",
			Detail: "A tier at 100% over four call sites and one at 100% over four thousand are the same number and not the same claim.",
			Screen: "overview",
		}
	}
}
