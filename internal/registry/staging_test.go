package registry

import (
	"context"
	"testing"
)

// TestStagingRun pins the four states a reader can meet: nothing at all, a
// first run still being built, a committed run alone, and a committed run with
// a newer one being built behind it.
func TestStagingRun(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	got, err := r.StagingRun(ctx)
	if err != nil || got != nil {
		t.Fatalf("empty store: got (%v, %v), want (nil, nil)", got, err)
	}

	first, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err = r.StagingRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != first.ID {
		t.Fatalf("first run in progress: got %v, want run %d", got, first.ID)
	}

	if err := r.CommitRun(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	got, err = r.StagingRun(ctx)
	if err != nil || got != nil {
		t.Fatalf("after commit: got (%v, %v), want (nil, nil)", got, err)
	}

	second, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err = r.StagingRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != second.ID {
		t.Fatalf("newer run in progress: got %v, want run %d", got, second.ID)
	}
}
