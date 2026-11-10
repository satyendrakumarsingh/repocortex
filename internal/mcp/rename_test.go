package mcp

import "testing"

func TestPartitionEditsSplitsApplyableFromReviewable(t *testing.T) {
	plan := partitionEdits([]RenameEdit{
		{Path: "a.go", Line: 10, Confidence: "exact"},
		{Path: "b.go", Line: 20, Confidence: "high"},
		{Path: "c.go", Line: 30, Confidence: "medium"},
		{Path: "d.go", Line: 40, Confidence: "low"},
	})

	if len(plan.Apply) != 1 {
		t.Errorf("Apply should hold only exact edits, got %d: %+v", len(plan.Apply), plan.Apply)
	}
	if plan.Apply[0].Path != "a.go" {
		t.Errorf("Apply[0] = %q, want a.go", plan.Apply[0].Path)
	}
	if len(plan.Review) != 3 {
		t.Errorf("Review should hold high, medium and low, got %d", len(plan.Review))
	}
}

func TestPartitionEditsRefusesToClaimCompleteness(t *testing.T) {
	plan := partitionEdits([]RenameEdit{{Path: "a.go", Line: 1, Confidence: "exact"}})
	if plan.Complete {
		t.Error("a plan may never claim completeness; RepoCortex cannot prove it found every reference")
	}
	if plan.Warning == "" {
		t.Error("every plan must carry the incompleteness warning")
	}
}

func TestPartitionEditsOnAnEmptySetIsStillAWarnedPlan(t *testing.T) {
	plan := partitionEdits(nil)
	if len(plan.Apply) != 0 || len(plan.Review) != 0 {
		t.Errorf("empty input must give an empty plan, got %+v", plan)
	}
	if plan.Warning == "" {
		t.Error("an empty plan is the MOST dangerous one and must still warn")
	}
}

func TestPartitionEditsIsDeterministic(t *testing.T) {
	in := []RenameEdit{
		{Path: "z.go", Line: 1, Confidence: "exact"},
		{Path: "a.go", Line: 9, Confidence: "exact"},
		{Path: "a.go", Line: 2, Confidence: "exact"},
	}
	first := partitionEdits(in)
	second := partitionEdits(in)
	for i := range first.Apply {
		if first.Apply[i] != second.Apply[i] {
			t.Fatalf("partitionEdits is not deterministic at %d: %+v vs %+v",
				i, first.Apply[i], second.Apply[i])
		}
	}
	if first.Apply[0].Path != "a.go" || first.Apply[0].Line != 2 {
		t.Errorf("edits must be sorted by path then line, got %+v", first.Apply)
	}
}
