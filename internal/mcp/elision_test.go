package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// manyItems builds n items whose kinds follow the requested mix, in the order
// the map's kinds are named by sortedKeys so the fixture is deterministic.
func manyItems(n int, byKind map[string]int) []Item {
	items := make([]Item, 0, n)
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	// Deterministic fixture order: sort the kind names.
	for i := 0; i < len(kinds); i++ {
		for j := i + 1; j < len(kinds); j++ {
			if kinds[j] < kinds[i] {
				kinds[i], kinds[j] = kinds[j], kinds[i]
			}
		}
	}
	for _, k := range kinds {
		for i := 0; i < byKind[k] && len(items) < n; i++ {
			items = append(items, Item{
				Kind: k,
				Name: fmt.Sprintf("%s%d", k, i),
				Path: "a.go",
				Line: i + 1,
			})
		}
	}
	return items
}

// itemsWithTailConfidence builds n items where the LAST tail carry conf and
// everything before them is "low" — the ordering failure the summary exists to
// expose, where the bound keeps the guesses and drops the proofs.
func itemsWithTailConfidence(n, tail int, conf string) []Item {
	items := make([]Item, 0, n)
	for i := 0; i < n; i++ {
		c := "low"
		if i >= n-tail {
			c = conf
		}
		items = append(items, Item{
			Kind:       "calls",
			Name:       fmt.Sprintf("caller%d", i),
			Path:       "a.go",
			Line:       i + 1,
			Confidence: c,
		})
	}
	return items
}

func TestOmissionSummarisesWhatWasDroppedByKind(t *testing.T) {
	r := &Response{Items: manyItems(100, map[string]int{"function": 60, "method": 40})}
	r.Bound(Limits{MaxItems: 10})

	if !r.Truncated {
		t.Fatal("Truncated = false after bounding 100 items to 10")
	}
	if r.TruncatedReason == "" {
		t.Error("TruncatedReason empty: the existing honesty envelope must not regress")
	}
	if r.Omitted == nil {
		t.Fatal("Omitted is nil: a bounded response must describe what it dropped")
	}
	if r.Omitted.Count != 90 {
		t.Errorf("Omitted.Count = %d, want 90", r.Omitted.Count)
	}
	if len(r.Omitted.ByKind) == 0 {
		t.Fatal("ByKind empty: the summary must say what kind of things went missing")
	}
	var total int
	for _, k := range r.Omitted.ByKind {
		total += k.Count
	}
	if total != 90 {
		t.Errorf("ByKind sums to %d, want 90", total)
	}
	// Descending by count, ties on name, so the summary is byte-stable.
	for i := 1; i < len(r.Omitted.ByKind); i++ {
		prev, cur := r.Omitted.ByKind[i-1], r.Omitted.ByKind[i]
		if cur.Count > prev.Count || (cur.Count == prev.Count && cur.Kind < prev.Kind) {
			t.Errorf("ByKind not sorted descending by count, ties on name: %+v", r.Omitted.ByKind)
		}
	}
}

func TestOmissionFlagsWhenStrongResultsWereDropped(t *testing.T) {
	// The dropped tail contains the only exact-confidence hits. An agent that
	// stops here would miss the best evidence in the index.
	r := &Response{Items: itemsWithTailConfidence(100, 10, "exact")}
	r.Bound(Limits{MaxItems: 10})

	joined := strings.Join(r.Notes, " ")
	if !strings.Contains(joined, "exact") {
		t.Errorf("dropping exact-confidence results must be called out, got notes: %v", r.Notes)
	}
	if r.Omitted == nil {
		t.Fatal("Omitted is nil after dropping 90 items")
	}
	if len(r.Omitted.StrongerDropped) == 0 || r.Omitted.StrongerDropped[0] != "exact" {
		t.Errorf("StrongerDropped = %v, want [exact]", r.Omitted.StrongerDropped)
	}
	if r.Omitted.WeakestShown != "low" {
		t.Errorf("WeakestShown = %q, want \"low\"", r.Omitted.WeakestShown)
	}
	var exact int
	for _, c := range r.Omitted.ByConfidence {
		if c.Confidence == "exact" {
			exact = c.Count
		}
	}
	if exact != 10 {
		t.Errorf("ByConfidence exact = %d, want 10", exact)
	}
}

func TestNoOmissionWhenNothingWasCut(t *testing.T) {
	r := &Response{Items: manyItems(5, map[string]int{"function": 5})}
	r.Bound(Limits{MaxItems: 10})
	if r.Omitted != nil {
		t.Fatalf("Omitted set on a complete response: %+v", r.Omitted)
	}
	if r.Truncated {
		t.Error("Truncated set on a complete response")
	}
	if len(r.Notes) != 0 {
		t.Errorf("notes on a complete response: %v", r.Notes)
	}
}

// TestOmissionNoteOnlyWhenTheTailWasStronger is the discriminating case: a
// response whose drops are no better than what survived must not cry wolf,
// because a note that fires on every truncation is a note an agent learns to
// ignore.
func TestOmissionNoteOnlyWhenTheTailWasStronger(t *testing.T) {
	cases := []struct {
		name      string
		items     []Item
		wantNote  bool
		wantTiers []string
	}{
		{
			name:      "proofs dropped, guesses kept",
			items:     itemsWithTailConfidence(40, 10, "exact"),
			wantNote:  true,
			wantTiers: []string{"exact"},
		},
		{
			name:     "best first, tail is weaker",
			items:    reversedConfidence(40, 10, "exact"),
			wantNote: false,
		},
		{
			name:     "uniform confidence",
			items:    itemsWithTailConfidence(40, 10, "low"),
			wantNote: false,
		},
		{
			name:     "no confidence at all",
			items:    manyItems(40, map[string]int{"file": 40}),
			wantNote: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Response{Items: tc.items}
			r.Bound(Limits{MaxItems: 10})

			got := len(r.Notes) > 0
			if got != tc.wantNote {
				t.Fatalf("note present = %v, want %v (notes: %v)", got, tc.wantNote, r.Notes)
			}
			if r.Omitted == nil {
				t.Fatal("Omitted is nil after dropping 30 items")
			}
			if len(r.Omitted.StrongerDropped) != len(tc.wantTiers) {
				t.Fatalf("StrongerDropped = %v, want %v", r.Omitted.StrongerDropped, tc.wantTiers)
			}
			for i, want := range tc.wantTiers {
				if r.Omitted.StrongerDropped[i] != want {
					t.Errorf("StrongerDropped[%d] = %q, want %q", i, r.Omitted.StrongerDropped[i], want)
				}
			}
		})
	}
}

// reversedConfidence puts the strong items FIRST, which is what a cost-ordered
// traversal produces.
func reversedConfidence(n, head int, conf string) []Item {
	items := itemsWithTailConfidence(n, n-head, "low")
	for i := 0; i < head; i++ {
		items[i].Confidence = conf
	}
	for i := head; i < n; i++ {
		items[i].Confidence = "low"
	}
	return items
}

// TestRenderTextIncludesOmissionComposition pins the Critical-defect class
// this branch already shipped once: a field that marshals correctly but never
// reaches RenderText never reaches the model, because Server.reply sets
// Content from RenderText and that suppresses the go-sdk's structuredContent
// fallback. Omitted.ByKind/ByConfidence must therefore show up in the
// rendered text, not just the struct.
func TestRenderTextIncludesOmissionComposition(t *testing.T) {
	r := &Response{Items: manyItems(100, map[string]int{"function": 60, "method": 40})}
	r.Bound(Limits{MaxItems: 10})

	text := r.RenderText()
	if !strings.Contains(text, "omitted: 90") {
		t.Errorf("RenderText missing the omission count line, got:\n%s", text)
	}
	if !strings.Contains(text, "function=50") || !strings.Contains(text, "method=40") {
		t.Errorf("RenderText missing the by-kind breakdown, got:\n%s", text)
	}
}

// TestRenderTextIncludesStrongerEvidenceNote pins the same reachability
// requirement for the case that matters most: when the dropped tail outranks
// what survived, that warning must be legible to the model, not only present
// in r.Notes.
func TestRenderTextIncludesStrongerEvidenceNote(t *testing.T) {
	r := &Response{Items: itemsWithTailConfidence(100, 10, "exact")}
	r.Bound(Limits{MaxItems: 10})

	text := r.RenderText()
	if !strings.Contains(text, omissionNotePrefix) {
		t.Errorf("RenderText missing the stronger-evidence note, got:\n%s", text)
	}
	if !strings.Contains(text, "exact=10") {
		t.Errorf("RenderText missing the dropped-tier count, got:\n%s", text)
	}
}

// TestRenderTextOmitsCompositionWhenNothingWasCut guards the other direction:
// a complete response must not carry an empty or spurious omission line.
func TestRenderTextOmitsCompositionWhenNothingWasCut(t *testing.T) {
	r := &Response{Items: manyItems(5, map[string]int{"function": 5})}
	r.Bound(Limits{MaxItems: 10})

	text := r.RenderText()
	if strings.Contains(text, "omitted:") {
		t.Errorf("RenderText carries an omission line on a complete response:\n%s", text)
	}
	if strings.Contains(text, omissionNotePrefix) {
		t.Errorf("RenderText carries a stronger-evidence note on a complete response:\n%s", text)
	}
}

// TestOmissionCountsBytesDroppedItemsToo pins that the summary describes every
// item Bound removed, not only the ones the item cap took. The byte bound drops
// from the same tail and is just as invisible to the caller.
func TestOmissionCountsBytesDroppedItemsToo(t *testing.T) {
	l := DefaultLimits()
	r := &Response{Summary: "big"}
	for i := 0; i < l.MaxItems; i++ {
		r.Items = append(r.Items, Item{
			Kind:    "func",
			Path:    "a.go",
			Excerpt: strings.Repeat("x", l.MaxExcerptBytes),
		})
	}
	before := len(r.Items)

	r.Bound(l)

	// The fixture is built to force this, not merely hoped to: MaxItems items
	// (50) each carrying a full MaxExcerptBytes excerpt (2000 bytes) is
	// ~100KB before any per-item overhead, well over MaxTotalBytes (24KB). A
	// t.Skip guard here would go quiet the day DefaultLimits changes enough to
	// stop tripping the byte bound, and a vacuous test that reports green is
	// worse than a failing one — so this fails loudly instead of skipping.
	if len(r.Items) == before {
		t.Fatalf("byte bound dropped nothing: fixture no longer forces truncation under DefaultLimits (%d items × %d excerpt bytes vs %d budget) — strengthen the fixture, don't skip",
			before, l.MaxExcerptBytes, l.MaxTotalBytes)
	}
	if r.Omitted == nil {
		t.Fatal("Omitted is nil though the byte bound dropped items")
	}
	if want := before - len(r.Items); r.Omitted.Count != want {
		t.Errorf("Omitted.Count = %d, want %d", r.Omitted.Count, want)
	}
	if got := len(r.RenderText()); got > l.MaxTotalBytes {
		t.Errorf("rendered %d bytes, over budget %d: the omission note is unbudgeted", got, l.MaxTotalBytes)
	}
}

// TestOmissionAccumulatesAcrossRepeatedBounds guards Server.reply, which calls
// Bound more than once on the same response. A second pass must not forget what
// the first one dropped.
func TestOmissionAccumulatesAcrossRepeatedBounds(t *testing.T) {
	r := &Response{Items: manyItems(100, map[string]int{"function": 100})}
	r.Bound(Limits{MaxItems: 40})
	if r.Omitted == nil || r.Omitted.Count != 60 {
		t.Fatalf("after first bound: %+v, want Count 60", r.Omitted)
	}
	r.Bound(Limits{MaxItems: 10})
	if r.Omitted.Count != 90 {
		t.Errorf("after second bound: Count = %d, want 90", r.Omitted.Count)
	}
	var total int
	for _, k := range r.Omitted.ByKind {
		total += k.Count
	}
	if total != 90 {
		t.Errorf("ByKind sums to %d, want 90", total)
	}
	// Idempotent: bounding again with the same limit drops nothing new.
	r.Bound(Limits{MaxItems: 10})
	if r.Omitted.Count != 90 {
		t.Errorf("after a no-op bound: Count = %d, want 90", r.Omitted.Count)
	}
}
