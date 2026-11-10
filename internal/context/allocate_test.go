package context

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// bodyCandidate builds a candidate that is eligible for source, with the score
// the test cares about. unitID must be positive or the candidate has no source
// range and is not a claimant on the envelope at all.
func bodyCandidate(id int64, path string, score float64) *candidate {
	return &candidate{
		unitID: id,
		kind:   "class",
		name:   path,
		path:   path,
		line:   1,
		score:  score,
		via:    admitNamed,
	}
}

func allocate(t *testing.T, cands []*candidate, envelope int) []int {
	t.Helper()
	tun := DefaultTuning()
	markBodyCliff(cands, tun.Weights, tun.Thresholds)
	return allocateBodies(cands, tun.Thresholds, envelope)
}

// TestBudgetFollowsRelevanceNotCount is the defect this allocator exists to fix.
// Two items of identical size, one three times as relevant: under allocation by
// count they receive the same window, which is how a score-18 file ends up
// shipping whole while a score-41 file is clipped mid-method.
func TestBudgetFollowsRelevanceNotCount(t *testing.T) {
	strong := bodyCandidate(1, "src/pay/Strong.java", 6.0)
	weak := bodyCandidate(2, "src/pay/Weak.java", 2.0)

	grants := allocate(t, []*candidate{strong, weak}, 4000)

	if grants[0] <= grants[1] {
		t.Fatalf("strong item got %d chars, weak got %d: relevance bought nothing", grants[0], grants[1])
	}
	if grants[1] == 0 {
		t.Error("the weaker item got no floor at all; a diffuse question needs a spread")
	}
	if sum := grants[0] + grants[1]; sum > 4000 {
		t.Errorf("allocation overspent the envelope: %d of 4000", sum)
	}
	// The floor is a floor, not an equaliser: the strong item must still win
	// clearly, or "allocation by relevance" is a label rather than a behaviour.
	if float64(grants[0]) < 1.5*float64(grants[1]) {
		t.Errorf("allocation is nearly flat: %d vs %d for a 3x relevance gap", grants[0], grants[1])
	}
}

// TestWorthPenalisesBytesThatTeachNothing pins the second application of the
// relevance penalty. Generated source ranks honestly on a name collision and
// still teaches nothing when read, so it must not take an equal share of the
// envelope from a hand-written file of the same score.
func TestWorthPenalisesBytesThatTeachNothing(t *testing.T) {
	handwritten := bodyCandidate(1, "src/pay/PaymentRequest.java", 4.0)
	generated := bodyCandidate(2, "src/generated/PaymentRequestBuilder.java", 4.0)

	grants := allocate(t, []*candidate{handwritten, generated}, 4000)

	if grants[0] <= grants[1] {
		t.Errorf("generated source got %d chars against %d for hand-written source at the same score",
			grants[1], grants[0])
	}

	// Text-only admission is the decoy shape: a word that may well be in a
	// comment should not buy the same bytes as a symbol the task named.
	named := bodyCandidate(1, "src/pay/A.java", 4.0)
	textOnly := bodyCandidate(2, "src/pay/B.java", 4.0)
	textOnly.via = admitLexical
	grants = allocate(t, []*candidate{named, textOnly}, 4000)
	if grants[0] <= grants[1] {
		t.Errorf("a text-only match got %d chars against %d for a named symbol", grants[1], grants[0])
	}
}

// TestCliffedItemIsNamedWithAReason is the honesty half of the contract. An
// item below the cliff keeps its place in the pack and says why it arrived
// without source; "we chose not to spend your budget here" and a silently
// bodiless item are different products.
func TestCliffedItemIsNamedWithAReason(t *testing.T) {
	top := bodyCandidate(1, "src/pay/Top.java", 10.0)
	faint := bodyCandidate(2, "src/pay/Faint.java", 0.4) // 4% of the top weight

	grants := allocate(t, []*candidate{top, faint}, 4000)

	if grants[1] != 0 {
		t.Errorf("an item at 4%% of the top weight was granted %d chars; the cliff did not bind", grants[1])
	}
	if faint.withheld == "" {
		t.Fatal("a cliffed candidate carries no reason: it was dropped silently")
	}
	if !strings.Contains(faint.withheld, "source withheld") {
		t.Errorf("withheld reason does not say what happened: %q", faint.withheld)
	}
	if top.withheld != "" {
		t.Errorf("the strongest item was cliffed: %q", top.withheld)
	}
	if grants[0] != 4000 {
		t.Errorf("the envelope the cliff freed was not handed on: top got %d of 4000", grants[0])
	}
}

// TestCliffNeverEmptiesTheResponse guards the failure that costs a whole round
// trip. Whatever the spread, something must come back with a body.
func TestCliffNeverEmptiesTheResponse(t *testing.T) {
	tun := DefaultTuning()
	tun.BodyCliffFraction = 1.0 // pathological: nothing but the leader qualifies

	cands := []*candidate{
		bodyCandidate(1, "src/a/A.java", 3.0),
		bodyCandidate(2, "src/b/B.java", 3.0),
		bodyCandidate(3, "src/c/C.java", 0.1),
	}
	markBodyCliff(cands, tun.Weights, tun.Thresholds)
	grants := allocateBodies(cands, tun.Thresholds, 2000)

	var withBody int
	for _, g := range grants {
		if g > 0 {
			withBody++
		}
	}
	if withBody == 0 {
		t.Fatal("every candidate was cliffed; the whole response has no source in it")
	}
}

// TestCliffIsAppliedBeforeTheItemCap is the ordering the fix turns on. A
// cliff-bound item sitting between two strong ones must not hold a share of the
// envelope: what it frees has to reach the item behind it.
func TestCliffIsAppliedBeforeTheItemCap(t *testing.T) {
	cands := []*candidate{
		bodyCandidate(1, "src/a/A.java", 8.0),
		bodyCandidate(2, "src/b/B.java", 0.2), // cliff-bound, but ranked second
		bodyCandidate(3, "src/c/C.java", 4.0),
	}
	grants := allocate(t, cands, 3000)

	if grants[1] != 0 {
		t.Errorf("the cliff-bound middle item held %d chars of the envelope", grants[1])
	}
	if grants[2] == 0 {
		t.Error("the item behind the cliff-bound one got nothing; its share was never handed on")
	}
	if sum := grants[0] + grants[2]; sum != 3000 {
		t.Errorf("the freed envelope leaked: %d of 3000 allocated", sum)
	}
}

// TestThinEnvelopeWithholdsRatherThanDusts checks the other reason a body is
// withheld. Splitting a small envelope over many items buys every one of them a
// signature and half a statement, which teaches less than the summary line
// already did.
func TestThinEnvelopeWithholdsRatherThanDusts(t *testing.T) {
	var cands []*candidate
	for i := 0; i < 20; i++ {
		cands = append(cands, bodyCandidate(int64(i+1), "src/pay/F.java", 5.0-0.1*float64(i)))
	}
	grants := allocate(t, cands, 600) // 0.35 * 600 = 210 chars of floor to share

	var withBody, explained int
	for i, g := range grants {
		if g > 0 {
			withBody++
			if g < DefaultTuning().MinBodyChars {
				t.Errorf("item %d got %d chars, below the %d-char minimum worth printing",
					i, g, DefaultTuning().MinBodyChars)
			}
			continue
		}
		if cands[i].withheld == "" {
			t.Errorf("item %d has no body and no reason", i)
			continue
		}
		explained++
	}
	if withBody == 0 || withBody > 2 {
		t.Errorf("a 600-char envelope gave bodies to %d of 20 items; want 1 or 2", withBody)
	}
	if explained != 20-withBody {
		t.Errorf("%d items lost their body, %d said why", 20-withBody, explained)
	}
}

// TestCliffedItemSurvivesInTheRealPack is the end-to-end half of the honesty
// contract: the item is still in Items, still explains itself, and carries no
// source. The cliff is raised through Tuning rather than by finding a fixture
// with a wider spread, because the ranking policy is the configurable thing.
func TestCliffedItemSurvivesInTheRealPack(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	steep := DefaultTuning()
	steep.BodyCliffFraction = 0.5 // anything under half the leader loses its body

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{
		Task: goldenTask, Tuning: &steep,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var cliffed int
	for _, it := range pack.Items {
		if !strings.Contains(it.Why, "source withheld") {
			continue
		}
		cliffed++
		if it.Excerpt != "" {
			t.Errorf("%s says its source was withheld and carries source anyway", it.Name)
		}
		if it.Name == "" || it.Path == "" {
			t.Errorf("a cliffed item lost its identity: %+v", it)
		}
	}
	if cliffed == 0 {
		t.Fatalf("no item was cliffed at a 50%% cliff; items = %d", len(pack.Items))
	}
	// The strongest item must still be readable, or the pack is all pointers.
	if pack.Items[0].Excerpt == "" {
		t.Error("the top item lost its body while weaker items were being cliffed")
	}
	if !strings.Contains(strings.Join(pack.Notes, " "), "allocated by relevance") {
		t.Errorf("the allocation policy was applied silently; notes = %v", pack.Notes)
	}
}

// TestRelevanceBuysBytesEndToEnd checks the same claim on a real pack, and only
// where it is checkable.
//
// Most files in this fixture are SHORTER than the budget they are granted, so
// their excerpt size is set by how much source exists, not by relevance — the
// item the task named holds 258 characters of class body and cannot spend the
// 764 it won. The claim "more relevance buys more bytes" is therefore tested
// over the items the envelope actually clipped, identified by comparing against
// a pack assembled with source budget to spare.
func TestRelevanceBuysBytesEndToEnd(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	ctx := context.Background()

	roomy := DefaultTuning()
	roomy.BodyShare = 0.9
	full, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask, Tuning: &roomy})
	if err != nil {
		t.Fatal(err)
	}
	natural := map[string]int{}
	for _, it := range full.Items {
		natural[it.Name] = len(it.Excerpt)
	}

	tight := DefaultTuning()
	tight.BodyShare = 0.05 // 1,600 characters of source across the whole answer
	pack, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask, Tuning: &tight})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if pack.Items[0].Excerpt == "" {
		t.Fatal("the item the task named came back with no source at all")
	}

	type bound struct {
		name      string
		relevance float64
		bytes     int
		quantum   int // the longest line in the excerpt
	}
	var clipped []bound
	for _, it := range pack.Items {
		if it.Excerpt != "" && len(it.Excerpt) < natural[it.Name] {
			clipped = append(clipped, bound{it.Name, it.Relevance, len(it.Excerpt), longestLine(it.Excerpt)})
		}
	}
	if len(clipped) < 2 {
		t.Fatalf("a 1,600-character envelope clipped %d items; the test proves nothing", len(clipped))
	}

	// Excerpts are cut on line boundaries, so a grant is spent in whole lines and
	// two adjacent grants can realise within one line of each other in either
	// order. Anything BEYOND one line is the allocator, not the quantum.
	for i := 1; i < len(clipped); i++ {
		prev, cur := clipped[i-1], clipped[i]
		if cur.relevance < prev.relevance && cur.bytes > prev.bytes+cur.quantum {
			t.Errorf("%s (relevance %.2f) was clipped to %d chars while %s (relevance %.2f) got %d",
				cur.name, cur.relevance, cur.bytes, prev.name, prev.relevance, prev.bytes)
		}
	}
	t.Logf("budget-bound items: %+v", clipped)
}

// TestComfortableResponseIsNotTrimmed guards the direction this change could
// most easily go wrong in. A pack that fits its budget with room to spare must
// come back whole: no withheld bodies, no clipped lines, no truncation note.
// Trimming an answer that fits is pure loss.
func TestComfortableResponseIsNotTrimmed(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	roomy := DefaultTuning()
	roomy.BodyShare = 0.9 // far more source budget than this fixture can spend

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{
		Task:   goldenTask,
		Budget: Budget{MaxFiles: 50, MaxSymbols: 50, MaxLines: 5000, MaxTokensEstimate: 100000, MaxGraphDepth: 3},
		Tuning: &roomy,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	for _, note := range pack.Notes {
		if strings.Contains(note, "Truncated") {
			t.Errorf("a response that fits comfortably was truncated: %s", note)
		}
		if strings.Contains(note, "allocated by relevance") {
			t.Errorf("bodies were withheld with budget to spare: %s", note)
		}
	}
	for _, it := range pack.Items {
		if strings.Contains(it.Why, "source withheld") {
			t.Errorf("%s lost its body with budget to spare: %s", it.Name, it.Why)
		}
		if strings.HasSuffix(it.Excerpt, "…") {
			t.Errorf("%s was clipped mid-line with budget to spare", it.Name)
		}
	}
	if pack.ContentTokens() >= 100000 {
		t.Fatalf("the fixture somehow filled a 100k-token budget (%d)", pack.ContentTokens())
	}

	// And the comfortable pack must actually be RICHER than the default one, or
	// "not trimmed" is being satisfied by returning nothing.
	tight, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatal(err)
	}
	if bodyBytes(pack) <= bodyBytes(tight) {
		t.Errorf("a 9x source budget bought %d body bytes against %d at the default",
			bodyBytes(pack), bodyBytes(tight))
	}
}

// TestBodyAllocationIsByteIdentical is the determinism gate for this path.
// Allocation reads scores, sorts and divides; any map iteration or float
// instability that leaked into it would show up here as two different answers to
// the same question.
func TestBodyAllocationIsByteIdentical(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	ctx := context.Background()

	for _, task := range []string{goldenTask, "make the notification idempotent", "update transaction status"} {
		shared := NewAssembler()

		first, err := shared.Assemble(ctx, reg, g, runID, Request{Task: task})
		if err != nil {
			t.Fatalf("Assemble(%q): %v", task, err)
		}
		want, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}

		// Once through the same Assembler (warm catalogue) and once through a
		// fresh one (cold), because a cache is exactly the place a difference
		// hides.
		for i, a := range []*Assembler{shared, NewAssembler(), shared, NewAssembler()} {
			again, err := a.Assemble(ctx, reg, g, runID, Request{Task: task})
			if err != nil {
				t.Fatalf("Assemble(%q) run %d: %v", task, i, err)
			}
			got, err := json.Marshal(again)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("run %d differs for task %q\n first: %s\nsecond: %s", i, task, want, got)
			}
		}
		if bodyBytes(first) == 0 {
			t.Errorf("task %q produced a pack with no source at all; determinism proves nothing here", task)
		}
	}
}

// TestClipToCharsKeepsWholeLines pins the clipping rule: whole lines, and a
// marked cut only when not even the first line fits.
func TestClipToCharsKeepsWholeLines(t *testing.T) {
	const src = "public void handleCallback() {\n  repo.save(tx);\n  notify(tx);\n}"

	if got := clipToChars(src, 1000); got != src {
		t.Errorf("clip under the limit changed the text: %q", got)
	}
	got := clipToChars(src, 50)
	if strings.Contains(got, "notify") {
		t.Errorf("clip overspent its grant: %q", got)
	}
	if !strings.HasPrefix(got, "public void handleCallback() {") {
		t.Errorf("clip did not keep whole lines: %q", got)
	}
	if len(got) > 50 {
		t.Errorf("clip returned %d chars for a 50-char grant", len(got))
	}
	if tiny := clipToChars(src, 12); !strings.HasSuffix(tiny, "…") {
		t.Errorf("a cut inside the first line is unmarked: %q", tiny)
	}
}

func longestLine(s string) int {
	n := 0
	for _, ln := range strings.Split(s, "\n") {
		if len(ln) > n {
			n = len(ln)
		}
	}
	return n
}

func bodyBytes(p Pack) int {
	n := 0
	for _, it := range p.Items {
		n += len(it.Excerpt)
	}
	return n
}
