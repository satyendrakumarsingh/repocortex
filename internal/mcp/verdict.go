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
//
// THE STATUS IS NOT COMPUTED HERE. OverallSignal in signal.go computes it, from
// the rows AND from what the index knows about its own coverage of the symbol.
// This file turns that status into the sentences an agent reads. It used to
// grade the rows alone, which is how "verdict: strong ... Act on these" came to
// be printed under "COVERAGE TOO LOW ... Do not present what follows as an
// answer".

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// VerdictStatus is how an answer should be taken.
type VerdictStatus string

const (
	// VerdictProven: every row was compiler-proven, coverage is good and
	// nothing was cut.
	VerdictProven VerdictStatus = "proven"
	// VerdictStrong: no row fell below high, coverage is good and nothing was
	// cut.
	VerdictStrong VerdictStatus = "strong"
	// VerdictIncomplete: the rows are high or better, and the index knows the
	// list is not all of them: rows were cut, or references to this very name
	// were left unbound.
	VerdictIncomplete VerdictStatus = "incomplete"
	// VerdictMixed: rows of differing strength are present.
	VerdictMixed VerdictStatus = "mixed"
	// VerdictWeak: every graded row was inferred, most of it weakly.
	VerdictWeak VerdictStatus = "weak"
	// VerdictUnreliable: coverage is too low, or unknown, for the rows to
	// stand as an answer, whatever their tiers.
	VerdictUnreliable VerdictStatus = "unreliable"
	// VerdictEmpty: nothing was returned, which is ambiguous.
	VerdictEmpty VerdictStatus = "empty"
	// VerdictBreaksBuild: a proposed patch makes the type checker report new
	// errors. It outranks every other status.
	VerdictBreaksBuild VerdictStatus = "breaks_build"
)

// compileErrors counts the items that are new compile errors.
func (r *Response) compileErrors() int {
	n := 0
	for _, it := range r.Items {
		if it.Kind == "compile_error" {
			n++
		}
	}
	return n
}

// actionable reports whether a status tells the agent it may act on the rows
// as the answer. Exactly two do, and OverallSignal never returns either unless
// coverage is good and nothing was cut.
func (s VerdictStatus) actionable() bool { return s == VerdictProven || s == VerdictStrong }

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

// VerdictFor grades an answer set that has no known coverage problem and was
// not cut short. It is verdictFor with nothing to hold against the rows.
//
// Items with an empty Confidence are not relationship results — a full-text
// search hit has no provenance to grade — and are excluded from the histogram
// rather than counted as unknown, which would make every search look weak.
func VerdictFor(items []Item) *Verdict {
	return verdictFor(items, nil, false)
}

// verdictFor grades an answer set against its coverage and truncation.
func verdictFor(items []Item, rel *Reliability, truncated bool) *Verdict {
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

	level := ReliabilityGood
	if rel != nil {
		level = rel.Level
	}
	status := OverallSignal(SignalInput{Counts: counts, Reliability: level, Truncated: truncated})

	if status == VerdictEmpty {
		return &Verdict{
			Status:   VerdictEmpty,
			NextStep: "No resolved relationships. This is ambiguous: there may be none, or RepoCortex may not have resolved them. Confirm with search_code before concluding the symbol is unused.",
		}
	}

	exact := counts[string(model.ConfidenceExact)]
	medium := counts[string(model.ConfidenceMedium)]
	low := counts[string(model.ConfidenceLow)]

	v := &Verdict{Status: status, Counts: counts}

	switch status {
	case VerdictProven:
		v.NextStep = "Every result was compiler-proven. Safe to act on."

	case VerdictStrong:
		v.NextStep = "No result fell below high confidence, and nothing the index holds says the list is " +
			"short. High is inferred, not compiler-proven: read each site before an edit, and verify " +
			"before deleting anything."

	case VerdictIncomplete:
		// The reason is whichever one applied; both can.
		if truncated {
			v.Signals = append(v.Signals, "rows exist that this answer does not carry (see truncated)")
		}
		if rel != nil && rel.Level == ReliabilityIncomplete && rel.Lead != "" {
			v.Signals = append(v.Signals, rel.Lead)
		}
		v.NextStep = "The rows shown are high confidence or better and can be used as they are. " +
			"The LIST is known to be incomplete, so conclude nothing from what is absent: "
		if truncated {
			v.NextStep += "read on with offset, then "
		}
		v.NextStep += "run search_code for the name."

	case VerdictUnreliable:
		if rel != nil && rel.Lead != "" {
			v.Signals = append(v.Signals, rel.Lead)
		}
		if exact > 0 {
			v.Signals = append(v.Signals, fmt.Sprintf(
				"%d row(s) are compiler-proven and true as far as they go; the list they sit in is not complete", exact))
		}
		v.NextStep = "Do not present these rows as the answer. They are leads from a minority of the " +
			"evidence: confirm with search_code for the name, and treat what is absent as unknown."

	case VerdictWeak:
		v.Signals = append(v.Signals, fmt.Sprintf(
			"all %d results are inferred (%d medium, %d low); none was compiler-proven", graded, medium, low))
		v.NextStep = "Treat these as leads, not facts. Run search_code for the symbol name to check for references RepoCortex could not bind."

	default:
		if rel != nil && rel.Level == ReliabilityUnverified {
			// Mixed because of what the rows say about themselves, not because
			// of their tiers: every one may be labelled high.
			v.Signals = append(v.Signals, rel.Lead)
			v.NextStep = "Read how each row's call is written before using it: the tier says how the " +
				"name was matched, not that the qualifier in front of it names this definition."
			break
		}
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
// It reads Reliability and Truncated off the response, which is what makes the
// verdict unable to exceed the coverage it rests on: there is no path to a
// status that does not go through OverallSignal.
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

	v := verdictFor(r.Items, r.Reliability, r.Truncated)
	if r.Provenance != nil && !r.Provenance.HeadMatchesBuild {
		softenForStaleIndex(v)
	}
	// A patch the type checker rejects is not safe or proven, however sure
	// each row is: the rows are the errors.
	if n := r.compileErrors(); n > 0 {
		v.Status = VerdictBreaksBuild
		v.Signals = append(v.Signals, fmt.Sprintf("the patch adds %d compile error(s)", n))
		v.NextStep = "The patch breaks the build. Fix the compile errors listed before applying it."
	}
	r.Verdict = v
}
