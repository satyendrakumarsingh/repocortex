package context

import (
	"context"
	"strings"
	"testing"
)

// goldenTask is spec §43's golden task, verbatim.
const goldenTask = "Add idempotency to payment callback processing"

// goldenMustSurface is what a correct answer contains: the entry point, the
// service where the idempotency check belongs, the persistence that must become
// idempotent, the duplicate side effect being prevented, the two tables written
// on a status change, and the existing regression coverage.
var goldenMustSurface = []string{
	"CallbackController",
	"TransactionService",
	"TransactionRepository",
	"NotificationService",
	"transactions",
	"transaction_events",
	"CallbackControllerTest",
	"TransactionServiceTest",
}

// goldenMustExclude is the assertion that actually tests the engine.
//
// unrelated/marketing/MarketingReport.java is a decoy, and a well-built one: it
// carries the comment "Unrelated to payments", so a full-text search for the
// task's words ranks it ABOVE CallbackController. Any engine that returns
// everything relevant returns it. Passing the surface list is easy; passing this
// line is the product.
const goldenMustExclude = "unrelated/marketing/"

func TestGoldenPaymentCallbackIdempotency(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	surfaced := map[string]Item{}
	for _, it := range pack.Items {
		surfaced[it.Name] = it
	}

	t.Logf("interpretation: %s", pack.Interpretation)
	for _, it := range pack.Items {
		t.Logf("  %-6s %-24s %5.2f %-7s %s:%d — %s",
			it.Kind, it.Name, it.Relevance, it.Confidence, it.Path, it.Line, it.Why)
	}

	for _, want := range goldenMustSurface {
		it, ok := surfaced[want]
		if !ok {
			t.Errorf("golden entity %q is missing from the pack", want)
			continue
		}
		if it.Why == "" {
			t.Errorf("golden entity %q has no Why", want)
		}
		if it.Path == "" {
			t.Errorf("golden entity %q has no path", want)
		}
	}

	// The exclusion, checked two ways: absent from the items, AND reported as a
	// deliberate decision rather than merely missing. "We do not know about it"
	// and "we considered it and here is why not" are different products.
	for _, it := range pack.Items {
		if strings.HasPrefix(it.Path, goldenMustExclude) {
			t.Errorf("excluded area leaked into the pack: %s (%s) — why: %s", it.Name, it.Path, it.Why)
		}
	}
	var explained bool
	for _, ex := range pack.Excluded {
		if strings.HasPrefix(ex.Path, goldenMustExclude) {
			explained = true
			if ex.Reason == "" {
				t.Errorf("exclusion of %s carries no reason", ex.Name)
			}
			t.Logf("excluded: %s — %s", ex.Name, ex.Reason)
		}
	}
	if !explained {
		t.Errorf("MarketingReport was left out but not explained; Pack.Excluded = %+v", pack.Excluded)
	}

	// Precision matters as much as recall: a pack that contains the eight
	// wanted entities plus thirty others has not solved the problem. The
	// fixture holds ten files, so anything beyond a couple of extra items means
	// the engine is padding.
	if extra := len(pack.Items) - len(goldenMustSurface); extra > 2 {
		t.Errorf("pack carries %d items for %d wanted entities; %d extras is padding",
			len(pack.Items), len(goldenMustSurface), extra)
	}
}

// TestGoldenPackIsCheap guards the actual objective: understanding per token.
// The whole answer for the golden task must cost less than reading one of the
// files it points at.
func TestGoldenPackIsCheap(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatal(err)
	}
	const ceiling = 2000
	if pack.TokenEstimate > ceiling {
		t.Errorf("pack costs ~%d tokens, want <= %d", pack.TokenEstimate, ceiling)
	}
	if pack.TokenEstimate == 0 {
		t.Error("token estimate is zero for a non-empty pack")
	}
	t.Logf("%d items, ~%d estimated tokens", len(pack.Items), pack.TokenEstimate)
}
