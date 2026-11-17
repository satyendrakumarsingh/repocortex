package context

import (
	"context"
	"strings"
	"testing"
)

// Detail falls with distance. Items that lost their source to the budget show
// their signature when the task names them or they are one call away, and
// nothing when they are further; the long reason for withholding is stated
// once, in the notes, not on every item.
func TestPackDetailFallsWithDistance(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	tight := DefaultTuning()
	tight.BodyShare = 0.01 // most items lose their source
	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: goldenTask, Tuning: &tight})
	if err != nil {
		t.Fatal(err)
	}
	sigs := 0
	for _, it := range pack.Items {
		if strings.Contains(it.Why, "would cost more than they teach") || strings.Contains(it.Why, "which teaches less than") {
			t.Errorf("%s repeats the full withholding reason: %q", it.Name, it.Why)
		}
		if strings.Contains(it.Why, "signature only") {
			sigs++
			if strings.Contains(it.Excerpt, "\n") || it.Excerpt == "" {
				t.Errorf("%s: a signature is one line, got %q", it.Name, it.Excerpt)
			}
		}
		if strings.Contains(it.Why, "2 hops") && strings.Contains(it.Why, "signature only") {
			t.Errorf("%s is two hops away and got a signature", it.Name)
		}
	}
	if sigs == 0 {
		t.Fatalf("no item showed a signature in place of withheld source; items: %+v", pack.Items)
	}
	notes := strings.Join(pack.Notes, "\n")
	if !strings.Contains(notes, "would cost more than they teach") && !strings.Contains(notes, "past this pack's source budget") {
		t.Errorf("the rule for withholding is not stated once in the notes:\n%s", notes)
	}
}
