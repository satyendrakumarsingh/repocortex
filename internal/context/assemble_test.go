package context

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEmptyTaskIsRejected(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	for _, task := range []string{"", "   ", "\t\n "} {
		_, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: task})
		if !errors.Is(err, ErrEmptyTask) {
			t.Errorf("Assemble(%q) error = %v, want ErrEmptyTask", task, err)
		}
	}
}

// TestRankingIsDeterministic is a gate, not a nicety. An agent that gets a
// different answer on a retry cannot cache, cannot test, and cannot trust the
// engine — and the usual cause is Go map iteration order leaking into a result,
// which shows up only occasionally and never in the run where you look for it.
// Comparing serialised packs makes any such leak fail loudly.
func TestRankingIsDeterministic(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	ctx := context.Background()

	tasks := []string{
		goldenTask,
		"refactor TransactionRepository.save to use a batch update",
		"why does the notification fire twice",
	}

	for _, task := range tasks {
		first, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: task})
		if err != nil {
			t.Fatalf("Assemble(%q): %v", task, err)
		}
		want, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i++ {
			again, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: task})
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
	}
}

// TestEveryItemExplainsItself enforces the contract on Item.Why across a spread
// of tasks, not just the golden one. An unexplained item is one an agent cannot
// justify to the human reading its output.
func TestEveryItemExplainsItself(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	ctx := context.Background()

	for _, task := range []string{
		goldenTask,
		"make NotificationService retry on failure",
		"add an index to the transactions table",
		"CallbackController returns the wrong status code",
		"update transaction status handling",
	} {
		pack, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: task})
		if err != nil {
			t.Fatalf("Assemble(%q): %v", task, err)
		}
		for _, it := range pack.Items {
			if strings.TrimSpace(it.Why) == "" {
				t.Errorf("task %q: item %s (%s) has an empty Why", task, it.Name, it.Path)
			}
			if it.Name == "" || it.Kind == "" {
				t.Errorf("task %q: item at %s is missing name or kind: %+v", task, it.Path, it)
			}
		}
		for _, ex := range pack.Excluded {
			if strings.TrimSpace(ex.Reason) == "" {
				t.Errorf("task %q: exclusion %s carries no reason", task, ex.Name)
			}
		}
	}
}

// TestTaskMatchingNothingReturnsNothingUseful checks the failure mode that
// matters most in practice. Faced with a task about code that does not exist,
// an engine that returns its best guesses teaches an agent to distrust every
// answer; one that says "nothing matched" costs a sentence and stays credible.
func TestTaskMatchingNothingReturnsNothingUseful(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{
		Task: "rewrite the quantum teleportation scheduler for the holography cluster",
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(pack.Items) != 0 {
		for _, it := range pack.Items {
			t.Logf("unexpected item: %s (%s) — %s", it.Name, it.Path, it.Why)
		}
		t.Fatalf("got %d items for a task naming nothing in the repository, want 0", len(pack.Items))
	}
	if len(pack.Notes) == 0 {
		t.Fatal("empty pack carries no note explaining why")
	}
	if !strings.Contains(strings.ToLower(strings.Join(pack.Notes, " ")), "nothing in this index matched") {
		t.Errorf("note does not explain the empty result: %v", pack.Notes)
	}
	if pack.Interpretation == "" {
		t.Error("empty pack has no interpretation; the caller cannot tell a misread task from a missing feature")
	}
}

// TestTaskOfOnlyStopWordsIsReported separates "we understood you and found
// nothing" from "we could not understand you", which need different fixes.
func TestTaskOfOnlyStopWordsIsReported(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: "it is not for we"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(pack.Items) != 0 {
		t.Errorf("got %d items for a task of stop words", len(pack.Items))
	}
	if len(pack.Notes) == 0 || !strings.Contains(pack.Notes[0], "no retrievable terms") {
		t.Errorf("notes = %v, want an explanation that nothing retrievable was extracted", pack.Notes)
	}
}

// TestBudgetIsRespectedAndTruncationReported drives the budget down to one item
// and checks both halves of the contract: the limit binds, and the fact that it
// bound is visible in the output rather than inferable from a short list.
func TestBudgetIsRespectedAndTruncationReported(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{
		Task:   goldenTask,
		Budget: Budget{MaxFiles: 2, MaxSymbols: 2, MaxLines: 4, MaxTokensEstimate: 4000, MaxGraphDepth: 2},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(pack.Items) > 2 {
		t.Errorf("got %d items, want at most 2", len(pack.Items))
	}
	paths := map[string]bool{}
	lines := 0
	for _, it := range pack.Items {
		paths[it.Path] = true
		lines += itemLines(it)
	}
	if len(paths) > 2 {
		t.Errorf("got %d distinct files, want at most 2", len(paths))
	}
	if lines > 4 {
		t.Errorf("got %d excerpt lines, want at most 4", lines)
	}

	if !strings.Contains(strings.Join(pack.Notes, " "), "Truncated") {
		t.Errorf("truncation was silent; notes = %v", pack.Notes)
	}
	var budgetExclusions int
	for _, ex := range pack.Excluded {
		if strings.Contains(ex.Reason, "budget") {
			budgetExclusions++
		}
	}
	if budgetExclusions == 0 {
		t.Errorf("nothing was reported as dropped by the budget; excluded = %+v", pack.Excluded)
	}
}

// TestTokenBudgetBinds checks the one bound that depends on assembled text, and
// that it cuts source before it cuts summaries.
func TestTokenBudgetBinds(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	ctx := context.Background()

	full, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatal(err)
	}

	const limit = 300
	pack, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{
		Task:   goldenTask,
		Budget: Budget{MaxTokensEstimate: limit},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if got := pack.ContentTokens(); got > limit {
		t.Errorf("content is ~%d tokens under a %d-token budget", got, limit)
	}
	// One item always survives: "nothing fits" is a worse answer than the
	// single best thing plus a note saying the rest was cut.
	if len(pack.Items) == 0 {
		t.Fatal("token budget removed every item")
	}
	if len(pack.Items) >= len(full.Items) {
		t.Errorf("budget did not bind: %d items at ~%d tokens vs %d unbounded",
			len(pack.Items), limit, len(full.Items))
	}
	for _, it := range pack.Items {
		if it.Excerpt != "" {
			t.Errorf("item %s kept a source excerpt while items were being dropped", it.Name)
		}
	}
	if !strings.Contains(strings.Join(pack.Notes, " "), "Truncated") {
		t.Errorf("token truncation was silent; notes = %v", pack.Notes)
	}
}

// TestGraphDepthBoundIsHonoured proves MaxGraphDepth actually bounds the walk:
// at depth 1 the repository layer is two hops away and must not appear.
func TestGraphDepthBoundIsHonoured(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	ctx := context.Background()

	shallow, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{
		Task:   goldenTask,
		Budget: Budget{MaxGraphDepth: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if names(shallow)["TransactionRepository"] {
		t.Error("TransactionRepository is two hops away but appeared at MaxGraphDepth=1")
	}
	if !names(shallow)["TransactionService"] {
		t.Error("TransactionService is one hop away and should still appear at MaxGraphDepth=1")
	}

	deep, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask, Budget: Budget{MaxGraphDepth: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if !names(deep)["TransactionRepository"] {
		t.Error("TransactionRepository should appear at MaxGraphDepth=3")
	}
}

// TestNilGraphStillProducesAContextPack keeps the engine usable before
// `repocortex resolve` has run: with no call graph there is no expansion, but
// name and text matches still answer the question, and the pack says so by
// simply containing less.
func TestNilGraphStillProducesAContextPack(t *testing.T) {
	reg, _, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, nil, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatalf("Assemble with nil graph: %v", err)
	}
	if !names(pack)["CallbackController"] {
		t.Error("the symbol the task names should still surface without a graph")
	}
	for _, it := range pack.Items {
		if strings.HasPrefix(it.Path, goldenMustExclude) {
			t.Errorf("decoy leaked in when the graph was unavailable: %s", it.Path)
		}
	}
}

func names(p Pack) map[string]bool {
	out := map[string]bool{}
	for _, it := range p.Items {
		out[it.Name] = true
	}
	return out
}
