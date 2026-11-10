package mcp

import (
	"strings"
	"testing"
)

func TestVerdictForAllExactIsTrustworthy(t *testing.T) {
	v := VerdictFor([]Item{
		{Confidence: "exact"}, {Confidence: "exact"}, {Confidence: "exact"},
	})
	if v.Status != VerdictProven {
		t.Errorf("Status = %q, want %q", v.Status, VerdictProven)
	}
	if v.NextStep == "" {
		t.Error("every verdict must carry a next step, even a reassuring one")
	}
}

func TestVerdictForAllLowIsUnreliable(t *testing.T) {
	v := VerdictFor([]Item{
		{Confidence: "low"}, {Confidence: "low"},
	})
	if v.Status != VerdictWeak {
		t.Errorf("Status = %q, want %q", v.Status, VerdictWeak)
	}
	joined := strings.Join(v.Signals, " ")
	if !strings.Contains(joined, "low") {
		t.Errorf("signals should name the problem, got %v", v.Signals)
	}
}

func TestVerdictForMixedIsMixedAndCountsTiers(t *testing.T) {
	v := VerdictFor([]Item{
		{Confidence: "exact"}, {Confidence: "low"}, {Confidence: "low"}, {Confidence: "medium"},
	})
	if v.Status != VerdictMixed {
		t.Errorf("Status = %q, want %q", v.Status, VerdictMixed)
	}
	if v.Counts["low"] != 2 {
		t.Errorf("Counts[low] = %d, want 2", v.Counts["low"])
	}
	if v.Counts["exact"] != 1 {
		t.Errorf("Counts[exact] = %d, want 1", v.Counts["exact"])
	}
}

// An empty result is the case that matters most: it is ambiguous between "there
// are none" and "we could not resolve them", and an agent told "no callers"
// will delete the method.
func TestVerdictForEmptyIsAmbiguousNotProven(t *testing.T) {
	v := VerdictFor(nil)
	if v.Status != VerdictEmpty {
		t.Errorf("Status = %q, want %q", v.Status, VerdictEmpty)
	}
	if !strings.Contains(strings.ToLower(v.NextStep), "search_code") {
		t.Errorf("an empty verdict must point at a fallback, got %q", v.NextStep)
	}
}

// Items with no confidence are not relationship results (a search hit, say) and
// must not drag the verdict down.
func TestVerdictIgnoresItemsWithoutConfidence(t *testing.T) {
	v := VerdictFor([]Item{
		{Confidence: "exact"}, {Confidence: ""}, {Confidence: ""},
	})
	if v.Status != VerdictProven {
		t.Errorf("Status = %q, want %q", v.Status, VerdictProven)
	}
}

// TestVerdictForUngradedItemsDoesNotClaimEmptiness is the I4 guard.
//
// graded == 0 used to return VerdictEmpty regardless of len(items), so a
// response could list N results and simultaneously tell the agent there were
// none. Reachable rather than theoretical: Item.Confidence is filled from a
// bare SQL text column, and graph/cost.go already guards one layer down for
// "a new edge kind that forgets to set confidence".
func TestVerdictForUngradedItemsDoesNotClaimEmptiness(t *testing.T) {
	items := []Item{
		{Kind: "calls", Name: "a"},
		{Kind: "calls", Name: "b"},
		{Kind: "calls", Name: "c"},
	}
	v := VerdictFor(items)

	if v.Status == VerdictEmpty {
		t.Fatalf("3 results were graded %q — a verdict may not contradict the list it describes",
			VerdictEmpty)
	}
	if v.Status != VerdictWeak {
		t.Errorf("Status = %q, want %q for results carrying no tier", v.Status, VerdictWeak)
	}
	if len(v.Signals) == 0 {
		t.Fatal("an ungraded answer set must name the problem in a signal")
	}
	joined := strings.Join(v.Signals, " ")
	if !strings.Contains(joined, "3") {
		t.Errorf("the signal must name how many results went ungraded, got %q", joined)
	}
	if v.NextStep == "" {
		t.Error("NextStep is always set")
	}
}

// TestVerdictForNoItemsIsStillEmpty holds the other side of the same branch:
// nothing returned is genuinely ambiguous and must keep saying so.
func TestVerdictForNoItemsIsStillEmpty(t *testing.T) {
	if v := VerdictFor([]Item{}); v.Status != VerdictEmpty {
		t.Errorf("Status = %q, want %q for a genuinely empty set", v.Status, VerdictEmpty)
	}
}

// TestStaleIndexSoftensAProvenVerdict is the I1 guard.
//
// A response could carry provenance.head_matches_build=false with a stale-index
// caveat AND next_step "Every result was compiler-proven. Safe to act on."
// Both were individually defensible; together they contradict, and the
// confident sentence is the shorter one the model will follow. `exact` means a
// compiler proved the binding AT INDEX TIME, which is exactly what staleness
// invalidates.
func TestStaleIndexSoftensAProvenVerdict(t *testing.T) {
	fresh := &Response{
		Items:      []Item{{Kind: "calls", Confidence: "exact"}},
		Provenance: ProvenanceFor("abc1234567", "abc1234567", 5),
	}
	fresh.gradeSelf()
	if fresh.Verdict.Status != VerdictProven {
		t.Fatalf("Status = %q, want %q on a fresh index", fresh.Verdict.Status, VerdictProven)
	}
	if !strings.Contains(fresh.Verdict.NextStep, "Safe to act on") {
		t.Errorf("a fresh proven verdict should stay confident, got %q", fresh.Verdict.NextStep)
	}

	stale := &Response{
		Items:      []Item{{Kind: "calls", Confidence: "exact"}},
		Provenance: ProvenanceFor("abc1234567", "def7654321", 5),
	}
	stale.gradeSelf()
	if stale.Verdict.Status != VerdictProven {
		t.Fatalf("Status = %q, want %q: staleness softens the advice, it does not regrade the tiers",
			stale.Verdict.Status, VerdictProven)
	}
	if strings.Contains(stale.Verdict.NextStep, "Safe to act on") {
		t.Errorf("a stale index still told the agent it was safe to act: %q", stale.Verdict.NextStep)
	}
	if !strings.Contains(strings.ToLower(stale.Verdict.NextStep), "reindex") {
		t.Errorf("NextStep must name the remedy, got %q", stale.Verdict.NextStep)
	}
	if !strings.Contains(strings.Join(stale.Verdict.Signals, " "), "does not match HEAD") {
		t.Errorf("the staleness must appear as a signal, got %v", stale.Verdict.Signals)
	}
}

// TestGradeSelfIsIdempotent: reply may grade twice when charging the verdict's
// own rendered bytes costs an item. Signals must not accumulate.
func TestGradeSelfIsIdempotent(t *testing.T) {
	r := &Response{
		Items:      []Item{{Kind: "calls", Confidence: "exact"}},
		Provenance: ProvenanceFor("abc1234567", "def7654321", 5),
	}
	r.gradeSelf()
	first := len(r.Verdict.Signals)
	r.gradeSelf()
	if len(r.Verdict.Signals) != first {
		t.Errorf("grading twice produced %d signals, want %d: gradeSelf must recompute, not append",
			len(r.Verdict.Signals), first)
	}
}

// TestUngradedItemsGetNoVerdict: a full-text search hit has no binding to
// grade, so grading it would manufacture a claim out of the absence of one.
func TestUngradedItemsGetNoVerdict(t *testing.T) {
	r := &Response{Items: []Item{{Kind: "match", Path: "a.go", Line: 3}}}
	r.gradeSelf()
	if r.Verdict != nil {
		t.Errorf("a response whose items carry no confidence was graded %#v", r.Verdict)
	}
}

// TestVerdictDescribesTheItemsTheAgentReceived is the I3 guard: the histogram
// used to be computed before Bound, so an agent could read
// counts{exact:4, low:46} beside twelve items.
func TestVerdictDescribesTheItemsTheAgentReceived(t *testing.T) {
	l := DefaultLimits()
	l.MaxItems = 12

	r := &Response{Summary: "many", IndexRevision: 1, IndexStatus: "fresh"}
	for i := 0; i < 50; i++ {
		conf := "low"
		if i < 4 {
			conf = "exact"
		}
		r.Items = append(r.Items, Item{Kind: "calls", Name: "caller", Path: "a.go", Line: i + 1, Confidence: conf})
	}

	r.Bound(l)
	r.gradeSelf()

	total := 0
	for _, n := range r.Verdict.Counts {
		total += n
	}
	if total != len(r.Items) {
		t.Errorf("verdict counts total %d beside %d items; the histogram describes results the agent never received",
			total, len(r.Items))
	}
}
