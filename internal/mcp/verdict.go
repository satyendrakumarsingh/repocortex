package mcp

// A per-answer judgement, so the agent does not have to aggregate confidence
// itself.
//
// Every item already carries its own tier. That is necessary and not
// sufficient: a model reading twelve results, nine of them low-confidence, will
// usually act on the first plausible one. Stating the shape of the answer set
// once, in RepoCortex's own voice, is the difference between measuring confidence
// and using it.
//
// Deliberately NOT a score. A number invites the model to threshold it, and any
// threshold would be a calibration claim this project has not measured. Status
// plus named signals plus one concrete next step is a claim it can support.

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// VerdictStatus is the shape of an answer set.
type VerdictStatus string

const (
	// VerdictProven: every graded result was compiler-proven.
	VerdictProven VerdictStatus = "proven"
	// VerdictStrong: no result fell below high.
	VerdictStrong VerdictStatus = "strong"
	// VerdictMixed: proven and guessed results are both present.
	VerdictMixed VerdictStatus = "mixed"
	// VerdictWeak: every graded result was inferred, most of it weakly.
	VerdictWeak VerdictStatus = "weak"
	// VerdictEmpty: nothing was returned, which is ambiguous.
	VerdictEmpty VerdictStatus = "empty"
)

// Verdict is RepoCortex's judgement of its own answer.
type Verdict struct {
	Status VerdictStatus `json:"status"`

	// Counts is the tier histogram. Present so an agent that wants to reason
	// numerically can, without RepoCortex inventing a score.
	Counts map[string]int `json:"counts,omitempty"`

	// Signals name what is wrong, in prose an agent can act on.
	Signals []string `json:"signals,omitempty"`

	// NextStep is one concrete thing to do. Always set, including when the
	// answer is good — "nothing further" is itself useful.
	NextStep string `json:"next_step"`
}

// VerdictFor grades an answer set.
//
// Items with an empty Confidence are not relationship results — a full-text
// search hit has no provenance to grade — and are excluded from the histogram
// rather than counted as unknown, which would make every search look weak.
func VerdictFor(items []Item) *Verdict {
	counts := map[string]int{}
	graded := 0
	for _, it := range items {
		if it.Confidence == "" {
			continue
		}
		counts[it.Confidence]++
		graded++
	}

	// An UNGRADED ANSWER SET IS NOT AN EMPTY ONE. Returning VerdictEmpty here
	// regardless of len(items) let a response list N results and simultaneously
	// tell the agent there were none — the single most confusing thing an
	// envelope like this can do, and reachable, because Confidence is filled
	// from a bare SQL text column and graph/cost.go already guards one layer
	// down for "a new edge kind that forgets to set confidence".
	if graded == 0 && len(items) > 0 {
		return &Verdict{
			Status: VerdictWeak,
			Signals: []string{fmt.Sprintf(
				"%d result(s) carry no confidence tier at all, so RepoCortex cannot grade them; "+
					"this is a RepoCortex defect, not a property of your code", len(items))},
			NextStep: "Treat these as unverified leads and confirm each with search_code before acting.",
		}
	}

	if graded == 0 {
		return &Verdict{
			Status:   VerdictEmpty,
			NextStep: "No resolved relationships. This is ambiguous: there may be none, or RepoCortex may not have resolved them. Confirm with search_code before concluding the symbol is unused.",
		}
	}

	exact := counts[string(model.ConfidenceExact)]
	high := counts[string(model.ConfidenceHigh)]
	medium := counts[string(model.ConfidenceMedium)]
	low := counts[string(model.ConfidenceLow)]

	v := &Verdict{Counts: counts}

	switch {
	case exact == graded:
		v.Status = VerdictProven
		v.NextStep = "Every result was compiler-proven. Safe to act on."

	case low == 0 && medium == 0:
		v.Status = VerdictStrong
		v.NextStep = "No result fell below high confidence. Act on these, but verify before deleting anything."

	case exact == 0 && high == 0:
		v.Status = VerdictWeak
		v.Signals = append(v.Signals, fmt.Sprintf(
			"all %d results are inferred (%d medium, %d low); none was compiler-proven", graded, medium, low))
		v.NextStep = "Treat these as leads, not facts. Run search_code for the symbol name to check for references RepoCortex could not bind."

	default:
		v.Status = VerdictMixed
		v.Signals = append(v.Signals, fmt.Sprintf(
			"%d of %d results are proven; %d are low confidence", exact, graded, low))
		v.NextStep = "Act on the exact results first. Re-run with min_confidence=\"high\" to see only what RepoCortex can defend."
	}

	if low > 0 && low*2 >= graded {
		v.Signals = append(v.Signals, "low-confidence results are at least half the answer set")
	}
	return v
}

// staleIndexSignal names the one thing a confidence tier cannot survive.
//
// `exact` means a compiler proved the binding AT INDEX TIME. When the index no
// longer matches HEAD, that proof is about a revision the agent is not editing,
// so a verdict reading "Every result was compiler-proven. Safe to act on." next
// to a stale-index caveat is not merely redundant — the two contradict, and the
// confident sentence is the shorter one the model will follow.
const staleIndexSignal = "the index does not match HEAD; these tiers describe the indexed " +
	"revision, not your working tree"

// softenForStaleIndex rewrites a verdict that was graded against an index the
// working tree has moved past.
func softenForStaleIndex(v *Verdict) {
	v.Signals = append(v.Signals, staleIndexSignal)
	switch v.Status {
	case VerdictProven:
		v.NextStep = "Every result was compiler-proven against the indexed revision. " +
			"Reindex before acting if the working tree has moved."
	case VerdictStrong:
		v.NextStep = "No result fell below high confidence against the indexed revision. " +
			"Reindex before acting if the working tree has moved."
	default:
		v.NextStep += " Reindex first: these tiers describe the indexed revision, not your working tree."
	}
}

// gradeSelf attaches RepoCortex's judgement of this response's own answer set.
//
// Called from Server.reply, and only from there, so that every tool gets the
// same treatment rather than the two out of eight that used to remember. Two
// orderings are load-bearing and both are the caller's responsibility:
//
//  1. AFTER Bound. Grading the pre-truncation set produced histograms that
//     described results the agent never received — counts totalling fifty
//     printed beside twelve items.
//
//  2. AFTER Provenance is attached, so a stale index can soften the verdict it
//     invalidates.
//
// A response whose items carry no confidence gets NO verdict. A full-text
// search hit has no binding to grade, and grading it would manufacture a claim
// out of the absence of one. Idempotent: it recomputes from scratch, so reply
// can re-grade after a second Bound pass without accumulating signals.
func (r *Response) gradeSelf() {
	r.Verdict = nil

	graded := false
	for _, it := range r.Items {
		if it.Confidence != "" {
			graded = true
			break
		}
	}
	if !graded {
		return
	}

	v := VerdictFor(r.Items)
	if r.Provenance != nil && !r.Provenance.HeadMatchesBuild {
		softenForStaleIndex(v)
	}
	r.Verdict = v
}
