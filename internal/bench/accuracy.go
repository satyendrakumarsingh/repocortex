package bench

// Resolver accuracy: precision, recall and F1 over a call-site-restricted set.
//
// This file is the second measurement in this package. The first (bench.go)
// asks whether retrieval returned the right code; this one asks whether the
// resolver's CALLS edges are right AND whether they are complete. Both are
// arithmetic over facts somebody else gathered — no I/O, no SQL, no clock —
// because a measurement that owns its own data source cannot be re-run against
// a different one, and re-running it against a user's repository is the point.
//
// # Why recall needs a restriction, and what the restriction is
//
// docs/accuracy.md refused to publish recall, and the refusal was correct as
// stated: SCIP records that one symbol REFERENCES another, never that it CALLS
// it. A type annotation, an import, a field read and a call all arrive as the
// same occurrence. Dividing the resolver's CALLS edges by every SCIP reference
// would penalise it for not emitting a CALLS edge where no call exists, which
// is not an error — it is the specification.
//
// That objection is an objection to the DENOMINATOR, not to recall. It
// dissolves once the denominator is restricted to the references that ARE at
// call sites, and we know which those are: the parser recorded every call site
// it found in symbol_references (kind "reference.call"), independently of
// whether the resolver could then bind it. So the ground truth is
//
//	G = { (file, line) : the compiler resolved that line to exactly one
//	                     definition, AND the parser saw a call there }
//
// and every element of G is a call whose correct target is known. A resolver
// that produces no edge there has missed a caller — the expensive error for a
// change-safety product, and the one precision cannot see.
//
// The "exactly one definition" half is inherited unchanged from the precision
// measurement (registry.ResolutionPrecision): the two tools disagree about
// columns, so judgement happens per line, and a line the compiler resolved to
// several targets has no defined right answer.
//
// # What the restriction does NOT cover, stated because it bounds the claim
//
// The call-site oracle is RepoCortex's own parser. A call the parser never saw —
// an unsupported syntax, a language with no tree-sitter query, a macro-
// generated call — is absent from G, so it is neither scored nor missed. This
// is therefore RESOLVER recall over PARSED call sites, not recall over the
// calls in the repository. Naming that is what keeps the number honest: the
// alternative oracle would be the compiler's own call graph, which SCIP does
// not emit.
//
// # Why one call site counts once
//
// Precision in doctor --resolution-precision counts EDGES. Recall must count
// CALL SITES, because a missed call site produces no edge to count. Mixing the
// two units would make precision and recall denominators that cannot be
// combined, and F1 of two incompatible fractions is a number with no referent.
// So this measurement collapses every call site to one outcome — correct at
// some tier, wrong at some tier, or missed — and computes all three statistics
// over that single unit.

import (
	"fmt"
	"sort"
)

// ConfidenceTiers is the reading order docs/accuracy.md states: act on `high`,
// show `medium`, ignore `low`. Fixed policy, not derived from the numbers.
//
// `exact` is deliberately absent. The store's CHECK constraint forbids an
// `exact` edge without source='compiler', so no heuristic edge can carry it,
// and scoring compiler edges against themselves would be circular.
var ConfidenceTiers = []string{"high", "medium", "low"}

// CallSiteOutcome is a group of adjudicated call sites that share a verdict.
//
// One row per (language, tier, correctness) rather than one per call site: the
// gatherer aggregates in the query, and the arithmetic below never needs the
// individual site.
type CallSiteOutcome struct {
	// Language is the language of the file the call site is in.
	Language string
	// Tier is the STRONGEST confidence the resolver attached to any CALLS edge
	// at this call site. Empty means the resolver produced no edge at all,
	// which is the missed-caller case and the reason this file exists.
	//
	// Strongest rather than any: a consumer filtering --min-confidence high
	// sees the high edge, so the high edge is what its behaviour follows.
	Tier string
	// Correct is true when some edge at the call site named the definition the
	// compiler named. It is false both when an edge named something else and
	// when Tier is empty.
	Correct bool
	Count   int64
}

// TierAccuracy is one confidence tier's standing over the restricted set.
//
// Every rate is published beside the counts it came from. A tier at 100% over
// four call sites and a tier at 100% over four thousand are the same number
// and not the same claim.
type TierAccuracy struct {
	Tier string `json:"tier"`

	// TruePositives are call sites this tier got right; FalsePositives are call
	// sites where this tier produced an edge naming the wrong definition.
	TruePositives  int64 `json:"true_positives"`
	FalsePositives int64 `json:"false_positives"`
	// Retrieved is TP+FP: the call sites this tier answered at all.
	Retrieved int64 `json:"retrieved"`
	// Relevant is the whole adjudicated call-site set — the recall denominator,
	// identical for every tier, which is what makes the tiers comparable and
	// what makes their recalls sum to the all-tiers recall.
	Relevant int64 `json:"relevant"`

	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`

	// PrecisionMeasured is false when the tier answered nothing: precision is
	// undefined there, and printing 0% would read as "answered, and wrong".
	PrecisionMeasured bool `json:"precision_measured"`
	// Measured is false when nothing was adjudicated at all, in which case
	// every rate above is a placeholder rather than a result.
	Measured bool `json:"measured"`
}

// LanguageAccuracy is one language's rows, plus what got away.
type LanguageAccuracy struct {
	Language string `json:"language"`
	// Adjudicated is |G| for this language: call sites the compiler could
	// decide. It is the recall denominator.
	Adjudicated int64 `json:"adjudicated_call_sites"`
	// Missed is the count of those the resolver produced NO edge for. It is the
	// error precision cannot see, so it is a field and not a derived remark.
	Missed int64 `json:"missed_call_sites"`
	// Answered is Adjudicated-Missed.
	Answered int64 `json:"answered_call_sites"`

	Tiers []TierAccuracy `json:"tiers"`
	All   TierAccuracy   `json:"all_tiers"`

	// Measured is false when no call site in this language was adjudicated —
	// no SCIP index imported for it, or none of its call sites landed on a line
	// the compiler resolved unambiguously.
	Measured bool `json:"measured"`
}

// AllLanguages is the label the aggregate row carries. A language literally
// named "all" would collide; no ecosystem uses it, and the JSON marks the
// aggregate structurally anyway by living in its own field.
const AllLanguages = "all"

// Restriction is the soundness argument, in one paragraph, carried by the
// report itself.
//
// It is a constant rather than a sentence in docs/accuracy.md because the
// number travels: it lands in CI logs, in JSON a script parses, in a terminal
// somebody screenshots. A recall figure separated from the set it was computed
// over is the exact misreading docs/accuracy.md refused to risk, and a
// reference to a document is not a defence against a screenshot.
const Restriction = "Recall is computed ONLY over call sites: lines where the parser recorded a " +
	"call AND the compiler resolved that line to exactly one definition. SCIP records " +
	"references, a superset of calls, so recall against every SCIP reference would be " +
	"unsound — it would count a type annotation the index was never meant to emit a " +
	"CALLS edge for as a miss. Restricted to call sites, the denominator is calls whose " +
	"correct target is known, and an index that emitted nothing there missed a caller."

// NotMeasured is what this number does not cover, stated wherever it is shown.
var NotMeasured = []string{
	"It is not recall over the calls in the repository. The call-site oracle is " +
		"RepoCortex's own parser, so a call it never parsed is neither scored nor missed.",
	"It is not precision as `doctor --resolution-precision` reports it. That counts " +
		"edges over every adjudicable line; this counts call sites, so precision and " +
		"recall share a denominator and F1 means something.",
	"It is not a claim about proven edges. Compiler-backed edges are the ground truth here " +
		"and are never scored against themselves.",
	"A language with no SCIP index imported is NOT MEASURED, which is not the same as " +
		"accurate. It reports no rate at all rather than a zero.",
}

// AccuracyOf folds call-site outcomes into per-language rows and an aggregate.
//
// The returned slice is sorted by adjudicated count descending then by name, so
// the language a reader cares about is first and the report is diffable between
// runs — the determinism rule this package's doc comment states.
//
// Languages present in outcomes with zero adjudicated call sites are kept and
// marked unmeasured rather than dropped. Silently omitting them is the single
// failure mode a trust report cannot have: an unmeasured language then reads
// like a measured one that had nothing to report.
func AccuracyOf(outcomes []CallSiteOutcome) (langs []LanguageAccuracy, overall LanguageAccuracy) {
	// key is (language, tier, correct). Aggregated in a map, emitted in sorted
	// order — no map iteration reaches the output.
	type key struct {
		lang, tier string
		correct    bool
	}
	counts := map[key]int64{}
	seen := map[string]bool{}
	for _, o := range outcomes {
		lang := o.Language
		if lang == "" {
			lang = "unknown"
		}
		seen[lang] = true
		counts[key{lang, o.Tier, o.Correct}] += o.Count
	}

	names := make([]string, 0, len(seen))
	for lang := range seen {
		names = append(names, lang)
	}
	sort.Strings(names)

	total := map[key]int64{}
	langs = make([]LanguageAccuracy, 0, len(names))
	for _, lang := range names {
		la := LanguageAccuracy{Language: lang, Tiers: []TierAccuracy{}}

		// Every outcome for this language, whatever tier it carries, counts
		// towards the denominator — including tiers ConfidenceTiers does not
		// list, so an unlisted tier cannot quietly shrink the recall divisor.
		for k, n := range counts {
			if k.lang != lang {
				continue
			}
			la.Adjudicated += n
			if k.tier == "" {
				la.Missed += n
			}
			total[key{AllLanguages, k.tier, k.correct}] += n
		}
		la.Answered = la.Adjudicated - la.Missed
		la.Measured = la.Adjudicated > 0

		for _, tier := range ConfidenceTiers {
			la.Tiers = append(la.Tiers, tierAccuracy(tier,
				counts[key{lang, tier, true}], counts[key{lang, tier, false}], la.Adjudicated))
		}
		la.All = allTiers(la.Tiers, la.Adjudicated)
		langs = append(langs, la)
	}

	sort.SliceStable(langs, func(i, j int) bool {
		if langs[i].Adjudicated != langs[j].Adjudicated {
			return langs[i].Adjudicated > langs[j].Adjudicated
		}
		return langs[i].Language < langs[j].Language
	})

	overall = LanguageAccuracy{Language: AllLanguages, Tiers: []TierAccuracy{}}
	for k, n := range total {
		overall.Adjudicated += n
		if k.tier == "" {
			overall.Missed += n
		}
	}
	overall.Answered = overall.Adjudicated - overall.Missed
	overall.Measured = overall.Adjudicated > 0
	for _, tier := range ConfidenceTiers {
		overall.Tiers = append(overall.Tiers, tierAccuracy(tier,
			total[key{AllLanguages, tier, true}], total[key{AllLanguages, tier, false}],
			overall.Adjudicated))
	}
	overall.All = allTiers(overall.Tiers, overall.Adjudicated)
	return langs, overall
}

// tierAccuracy computes one row. relevant is |G|, shared by every tier.
func tierAccuracy(tier string, tp, fp, relevant int64) TierAccuracy {
	t := TierAccuracy{
		Tier:              tier,
		TruePositives:     tp,
		FalsePositives:    fp,
		Retrieved:         tp + fp,
		Relevant:          relevant,
		PrecisionMeasured: tp+fp > 0,
		Measured:          relevant > 0,
	}
	if t.PrecisionMeasured {
		t.Precision = float64(tp) / float64(tp+fp)
	}
	if relevant > 0 {
		t.Recall = float64(tp) / float64(relevant)
	}
	t.F1 = harmonic(t.Precision, t.Recall)
	return t
}

// allTiers sums the tier rows. Summed over the slice rather than recomputed
// from the map so the floating-point rounding order is fixed across runs.
func allTiers(tiers []TierAccuracy, relevant int64) TierAccuracy {
	var tp, fp int64
	for _, t := range tiers {
		tp += t.TruePositives
		fp += t.FalsePositives
	}
	all := tierAccuracy(AllLanguages, tp, fp, relevant)
	all.Tier = "all"
	return all
}

// harmonic is F1: the harmonic mean of precision and recall, 0 when either is.
//
// Zero rather than undefined at the boundary, and it is the honest answer:
// a tier that got nothing right has no useful balance between the two, and the
// counts beside it say which of the two failed.
func harmonic(precision, recall float64) float64 {
	if precision <= 0 || recall <= 0 {
		return 0
	}
	return 2 * precision * recall / (precision + recall)
}

// Rate formats a rate as a percentage, or a dash when it was not measured.
//
// An unmeasured rate printed as "0.0%" reads as "measured, and zero", which is
// the opposite of what it means — the same rule doctor's precision cell keeps.
func Rate(value float64, measured bool) string {
	if !measured {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", 100*value)
}
