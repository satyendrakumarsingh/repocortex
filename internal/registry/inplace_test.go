package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// seedCommitted builds a committed run holding one file with one symbol.
func seedCommitted(t *testing.T) (*Registry, int64) {
	t.Helper()
	ctx := context.Background()

	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := model.File{Path: "pkg/a.go", ContentHash: "h1", ParseStatus: "ok"}
	fileID, err := w.AddFile(ctx, &f)
	if err != nil {
		t.Fatal(err)
	}
	s := model.Symbol{
		FileID: fileID, Kind: model.KindFunction, Name: "Alpha", FQN: "pkg/a.go:Alpha",
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	if _, err := w.AddSymbol(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return r, run.ID
}

// TestCommittedRunRejectsWritesByDefault: full indexing must never be able to
// mutate the live revision. That is what makes an interrupted `cortex index`
// leave the previous revision intact.
func TestCommittedRunRejectsWritesByDefault(t *testing.T) {
	ctx := context.Background()
	r, runID := seedCommitted(t)

	err := r.ReplaceFile(ctx, runID,
		&model.File{Path: "pkg/a.go", ContentHash: "h2", ParseStatus: "ok"}, nil, nil)
	if err == nil {
		t.Fatal("ReplaceFile mutated a committed run without opt-in")
	}
	if !strings.Contains(err.Error(), "AllowCommittedRun") {
		t.Errorf("error should name the opt-in; got: %v", err)
	}

	// The asymmetry this guards against: an early draft of DeleteFile omitted
	// the check entirely, so a caller could erase a file from the live index
	// while being refused permission to modify one.
	if err := r.DeleteFile(ctx, runID, "pkg/a.go"); err == nil {
		t.Fatal("DeleteFile erased from a committed run without opt-in")
	}
}

// TestAllowCommittedRunPermitsIncrementalUpdate proves the opt-in works and is
// the mechanism incremental indexing relies on.
func TestAllowCommittedRunPermitsIncrementalUpdate(t *testing.T) {
	ctx := context.Background()
	r, runID := seedCommitted(t)

	updated := model.File{Path: "pkg/a.go", ContentHash: "h2", ParseStatus: "ok"}
	newSyms := []model.Symbol{{
		Kind: model.KindFunction, Name: "Beta", FQN: "pkg/a.go:Beta",
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}}
	if err := r.ReplaceFile(ctx, runID, &updated, newSyms, nil, AllowCommittedRun()); err != nil {
		t.Fatalf("opt-in replace failed: %v", err)
	}

	syms, err := r.Symbols(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range syms {
		names[s.Name] = true
	}
	if !names["Beta"] {
		t.Error("replacement symbol missing")
	}
	if names["Alpha"] {
		t.Error("stale symbol survived an in-place replacement")
	}

	// The run must remain the committed, visible one — in-place update does
	// not swap revisions.
	current, err := r.CurrentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != runID {
		t.Errorf("current run changed to %d; in-place update should not swap revisions", current.ID)
	}
}

// TestInPlaceDeletePreservesPerFileAtomicity checks the guarantee the design
// explicitly keeps: a deleted file takes its symbols with it, wholly.
func TestInPlaceDeletePreservesPerFileAtomicity(t *testing.T) {
	ctx := context.Background()
	r, runID := seedCommitted(t)

	if err := r.DeleteFile(ctx, runID, "pkg/a.go", AllowCommittedRun()); err != nil {
		t.Fatalf("opt-in delete failed: %v", err)
	}

	files, err := r.Files(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "pkg/a.go" {
			t.Error("file row survived deletion")
		}
	}
	syms, err := r.Symbols(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		if s.Name == "Alpha" {
			t.Error("symbol survived its file's deletion")
		}
	}
}
