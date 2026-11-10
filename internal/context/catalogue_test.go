package context

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSeedRetrievalMatchesAFullScan is the safety net under the inverted index.
//
// findSeeds used to score every symbol in the catalogue; it now scores only the
// symbols catalogue.seedCandidates returns. That is a pure speed change ONLY if
// the candidate set is exactly the set the scan would have scored above zero —
// a retrieval stage that quietly drops a seed does not get slower answers, it
// gets wrong ones, and the golden test would only catch the cases it happens to
// name.
//
// So this compares the two implementations directly, on the fixture and across
// tasks that exercise each way matchesTerm can score: a verbatim identifier, a
// stem-only match, a plural, and a task whose words appear nowhere.
func TestSeedRetrievalMatchesAFullScan(t *testing.T) {
	reg, _, runID := paymentFixture(t)
	cat, err := loadCatalogue(context.Background(), reg, runID)
	if err != nil {
		t.Fatalf("loadCatalogue: %v", err)
	}
	th := DefaultTuning().Thresholds

	tasks := []string{
		goldenTask,
		"handleCallback duplicates the notification",
		"make transactions idempotent",
		"why does processing notify twice",
		"TransactionRepository.save",
		"rename the settlement ledger",
		"nothing here resembles this repository at all",
	}
	for _, task := range tasks {
		in := ParseTask(task)
		got := findSeeds(cat, in, th)
		want := seedsByFullScan(cat, in, th)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("task %q: indexed seeds differ from a full scan\n got %v\nwant %v",
				task, seedIDs(got), seedIDs(want))
		}
	}
}

// TestSeedIndexCoversEveryIdentifierPart guards the one way this index could
// lose recall: skipping tokens.
//
// A generated test name splits into more parts than any hand-written
// identifier, and the term that names the task can be any one of them —
// including the last.
func TestSeedIndexCoversEveryIdentifierPart(t *testing.T) {
	const name = "test_that_the_settlement_ledger_rejects_a_duplicate_payout_reversal_request"

	c := &catalogue{
		nameTokens: map[string][]int64{},
		stemTokens: map[string][]int64{},
	}
	c.indexTokens(7, name, strings.ToLower(name))

	for _, part := range []string{"settlement", "ledger", "duplicate", "payout", "reversal", "request"} {
		if ids := c.nameTokens[part]; len(ids) != 1 || ids[0] != 7 {
			t.Errorf("identifier part %q is not indexed: %v", part, ids)
		}
	}
	// Stems too: a task saying "reversals" must still reach "reversal".
	if ids := c.seedCandidates([]Term{{Text: "reversals", Weight: 1, Stem: "reversal"}}); len(ids) != 1 {
		t.Errorf("plural form did not reach the symbol through the stem index: %v", ids)
	}
}

// TestTestSubjectIndexMatchesAFullScan pins the same property for fromTests's
// lookup: every test unit a scan would have found is reachable by subject.
func TestTestSubjectIndexMatchesAFullScan(t *testing.T) {
	reg, _, runID := paymentFixture(t)
	cat, err := loadCatalogue(context.Background(), reg, runID)
	if err != nil {
		t.Fatalf("loadCatalogue: %v", err)
	}

	want := map[string][]int64{}
	for _, id := range cat.order {
		if cat.unit(id) != id || !cat.isTestUnit(id) {
			continue
		}
		if subject := testSubject(cat.symbols[id].Name); subject != "" {
			want[subject] = append(want[subject], id)
		}
	}
	if len(want) == 0 {
		t.Fatal("fixture holds no test units; this test would prove nothing")
	}
	if !reflect.DeepEqual(cat.testsBySubject, want) {
		t.Errorf("testsBySubject = %v, want %v", cat.testsBySubject, want)
	}
	if got := cat.testsBySubject["CallbackController"]; len(got) == 0 {
		t.Error("CallbackControllerTest is not indexed against its subject")
	}
}

// seedsByFullScan is the implementation findSeeds replaced, kept here as the
// reference the indexed version is checked against.
func seedsByFullScan(c *catalogue, in Intent, th Thresholds) []seed {
	var seeds []seed
	for _, id := range c.order {
		score, matched := nameSignal(c.symbols[id].Name, in.Terms)
		if score < th.SeedName {
			continue
		}
		seeds = append(seeds, seed{symbolID: id, score: score, matched: matched})
	}
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].score != seeds[j].score {
			return seeds[i].score > seeds[j].score
		}
		return seeds[i].symbolID < seeds[j].symbolID
	})
	if len(seeds) > th.MaxSeeds {
		seeds = seeds[:th.MaxSeeds]
	}
	return seeds
}

func seedIDs(seeds []seed) []string {
	out := make([]string, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, fmt.Sprintf("%d(%.2f:%s)", s.symbolID, s.score, strings.Join(s.matched, "+")))
	}
	return out
}

// TestSeedCandidatesAreCompleteAtScale closes the hole a small fixture leaves.
//
// TestSeedRetrievalMatchesAFullScan compares the two implementations on the
// payment fixture, which holds ten files. Every candidate set it produces is
// tiny, so a truncation bug — the most natural way to make retrieval "faster",
// and the one that silently costs recall — slips straight through it: capping
// seedCandidates at fifty was measured to leave that test green.
//
// The number of symbols sharing a term is a property of the repository, not of
// the query, so the index must return all of them however many there are.
func TestSeedCandidatesAreCompleteAtScale(t *testing.T) {
	const total = 400

	c := &catalogue{
		nameTokens: map[string][]int64{},
		stemTokens: map[string][]int64{},
	}
	want := make([]int64, 0, total)
	for i := 0; i < total; i++ {
		// Every symbol carries the same term, so the candidate set for that
		// term is the whole catalogue and any cap is visible.
		name := fmt.Sprintf("TransactionHandler%03d", i)
		id := int64(i + 1)
		c.indexTokens(id, name, strings.ToLower(name))
		want = append(want, id)
	}

	got := c.seedCandidates([]Term{{Text: "transaction", Weight: 1, Stem: "transaction"}})
	if len(got) != total {
		t.Fatalf("seedCandidates returned %d of %d symbols sharing the term — "+
			"the index is dropping candidates, which loses recall silently", len(got), total)
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("seedCandidates returned the wrong ids, or out of ascending order")
	}
}
