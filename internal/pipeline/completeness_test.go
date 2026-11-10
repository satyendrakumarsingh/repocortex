package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// recordingSink is a Sink that also implements CompletenessRecorder, so the
// wiring can be observed without a database.
type recordingSink struct {
	got   Reconciliation
	calls int
}

func (s *recordingSink) WriteFile(context.Context, FileFacts) error { return nil }

func (s *recordingSink) RecordCompleteness(_ context.Context, r Reconciliation) error {
	s.calls++
	s.got = r
	return nil
}

// TestRunnerHandsCompletenessToTheSink pins the connection A28 was missing.
// reconcile.go computed the verdict and nothing carried it anywhere; the whole
// defect was a value that existed and was never persisted.
func TestRunnerHandsCompletenessToTheSink(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.go": "package a\n\nfunc A() {}\n",
		"b.go": "package a\n\nfunc B() {}\n",
	})

	sink := &recordingSink{}
	stats, err := runnerFor(t, root, Options{Workers: 2}).Run(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 {
		t.Fatalf("sink recorded completeness %d times, want exactly 1", sink.calls)
	}
	if sink.got != stats.Reconciliation {
		t.Fatalf("sink was handed %+v, stats report %+v", sink.got, stats.Reconciliation)
	}
	if !sink.got.Complete() || sink.got.Discovered != 2 {
		t.Fatalf("clean two-file run reconciled as %+v", sink.got)
	}
}

// TestRegistrySinkPersistsAShortfall is the other half: the verdict reaches the
// staging row, survives publication, and reads back as partial with its count
// intact.
//
// The shortfall is fabricated rather than provoked, deliberately. The bug being
// guarded against was observed under load with no error returned, and a test
// that could only reproduce it by racing the pool would be a flake pretending
// to be a regression test. reconcile.go already proves the arithmetic; this
// proves the number survives the journey to disk.
func TestRegistrySinkPersistsAShortfall(t *testing.T) {
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatal(err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewRegistrySink(writer)

	if err := sink.WriteFile(ctx, FileFacts{
		File: model.File{Path: "a.go", Language: "go", ContentHash: "h", ParseStatus: "ok"},
	}); err != nil {
		t.Fatal(err)
	}
	short := Reconciliation{
		Discovered: 1200, Indexed: 1163, Accounted: 1163, Unaccounted: 37,
		State: IndexStatePartial,
	}
	if err := sink.RecordCompleteness(ctx, short); err != nil {
		t.Fatalf("RecordCompleteness: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	// Still published. A partial index beats none; what changes is that the row
	// no longer claims to be a full picture of the repository.
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("a partial run must still publish: %v", err)
	}

	got, err := reg.CurrentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete() {
		t.Fatalf("published run reads as complete after a 37-file shortfall: %+v", got)
	}
	if got.FilesDiscovered != 1200 || got.FilesUnaccounted != 37 {
		t.Fatalf("discovered=%d unaccounted=%d, want 1200/37", got.FilesDiscovered, got.FilesUnaccounted)
	}
	if got.Status != model.StatusReady {
		t.Errorf("status = %q; completeness must not overwrite the lifecycle state", got.Status)
	}
	if short.Warning() == "" {
		t.Error("a partial reconciliation produced no warning text to surface")
	}
}
