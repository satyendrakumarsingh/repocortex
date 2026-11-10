package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// stageOneFile writes a single trivial file into a staging run, so a run has
// something to publish. Completeness is about the run, not its contents.
func stageOneFile(t *testing.T, r *Registry, runID int64, path string) *Writer {
	t.Helper()
	ctx := context.Background()
	w, err := r.NewWriter(ctx, runID)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.AddFile(ctx, &model.File{Path: path, Language: "go", ContentHash: "h"}); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	return w
}

// TestShortfallIsPersistedAndReadBackAsPartial is A28's whole point in one
// test.
//
// The run publishes — a partial index beats none, and the atomic swap keeps its
// meaning — but the row it publishes says so. Before this, a run that discovered
// 1,200 files and accounted for 1,163 was indistinguishable from a repository
// that genuinely contained 1,163, and every query answered confidently about
// the 37 that were missing.
func TestShortfallIsPersistedAndReadBackAsPartial(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w := stageOneFile(t, r, run.ID, "a.go")

	// What the pipeline knows the moment its pool is joined.
	if err := w.RecordCompleteness(ctx, Completeness{
		FilesDiscovered:  1200,
		FilesUnaccounted: 37,
		State:            model.IndexStatePartial,
	}); err != nil {
		t.Fatalf("RecordCompleteness: %v", err)
	}
	// Flush before Close: Close rolls an open batch back, so this is the same
	// order the pipeline uses (Run -> Flush -> Close -> CommitRun).
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("a partial run must still publish: %v", err)
	}

	got, err := r.CurrentRun(ctx)
	if err != nil {
		t.Fatalf("CurrentRun: %v", err)
	}
	if got.IndexState != model.IndexStatePartial {
		t.Errorf("index_state = %q, want %q — the shortfall was not persisted",
			got.IndexState, model.IndexStatePartial)
	}
	if got.Complete() {
		t.Error("IndexRun.Complete() is true on a run that lost 37 files")
	}
	if got.FilesDiscovered != 1200 {
		t.Errorf("files_discovered = %d, want 1200", got.FilesDiscovered)
	}
	if got.FilesUnaccounted != 37 {
		t.Errorf("files_unaccounted = %d, want 37", got.FilesUnaccounted)
	}

	// Completeness is orthogonal to lifecycle: nothing failed to parse, so the
	// run is `ready` AND partial. That pair is exactly why this is not a fourth
	// value of `status`.
	if got.Status != model.StatusReady {
		t.Errorf("status = %q, want %q — completeness must not overwrite the lifecycle state",
			got.Status, model.StatusReady)
	}
}

// TestCommitRunStampsCompletenessFromTheCommitter proves the publish-time
// option beats whatever the staging run was stamped with, since it is the more
// recent measurement.
func TestCommitRunStampsCompletenessFromTheCommitter(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w := stageOneFile(t, r, run.ID, "a.go")
	if err := w.RecordCompleteness(ctx, Completeness{
		FilesDiscovered: 10, State: model.IndexStateComplete,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if err := r.CommitRun(ctx, run.ID, WithCompleteness(Completeness{
		FilesDiscovered: 10, FilesUnaccounted: 4, State: model.IndexStatePartial,
	})); err != nil {
		t.Fatal(err)
	}

	got, err := r.CurrentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.IndexState != model.IndexStatePartial || got.FilesUnaccounted != 4 {
		t.Fatalf("committer's measurement lost: state=%q unaccounted=%d",
			got.IndexState, got.FilesUnaccounted)
	}
}

// TestCommitRunWithoutMeasurementClaimsNothing pins the fallback for a caller
// that cannot measure coverage. It must not invent a shortfall, and it must not
// leave a discovered count of zero next to a non-zero indexed count, which
// would read as an accounting error rather than as an absent measurement.
func TestCommitRunWithoutMeasurementClaimsNothing(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w := stageOneFile(t, r, run.ID, "a.go")
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	got, err := r.CurrentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete() {
		t.Errorf("an unmeasured run reads as partial (%q); silence must not become an accusation",
			got.IndexState)
	}
	if got.FilesDiscovered != got.FilesIndexed {
		t.Errorf("files_discovered = %d, files_indexed = %d — an unmeasured run must report "+
			"the same thing the pre-completeness schema did", got.FilesDiscovered, got.FilesIndexed)
	}
	if got.FilesUnaccounted != 0 {
		t.Errorf("files_unaccounted = %d on a run nobody measured", got.FilesUnaccounted)
	}
}

// TestRecordCompletenessRejectsAnUnknownState keeps the readable error in front
// of the CHECK constraint.
func TestRecordCompletenessRejectsAnUnknownState(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w := stageOneFile(t, r, run.ID, "a.go")
	defer func() { _ = w.Close() }()

	if err := w.RecordCompleteness(ctx, Completeness{State: model.IndexState("mostly")}); err == nil {
		t.Fatal("an unknown index_state was accepted")
	}
}
