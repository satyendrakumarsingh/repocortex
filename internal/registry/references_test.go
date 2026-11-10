package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// refSpec is one reference to stage plus the outcome the resolver would have
// recorded for it. Named in the style of fileSpec and symSpec above.
type refSpec struct {
	path       string
	name       string
	line       int
	col        int
	resolution Resolution
}

// seedResolutions stages the given references and records their outcomes,
// returning the run they were written to.
//
// It goes through AddReference and MarkResolutions rather than raw INSERTs on
// purpose: the read under test must agree with what the write path actually
// produces, including the NULL-rather-than-zero handling of
// resolved_symbol_id.
func seedResolutions(t *testing.T, r *Registry, specs []refSpec) int64 {
	t.Helper()
	ctx := context.Background()

	run, err := r.BeginRun(ctx, "auto", "diagnose")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	files := map[string]int64{}
	for _, spec := range specs {
		fileID, ok := files[spec.path]
		if !ok {
			fileID, err = w.AddFile(ctx, &model.File{
				Path: spec.path, Language: "go", ContentHash: "h-" + spec.path, ParseStatus: "ok",
			})
			if err != nil {
				t.Fatalf("add file %q: %v", spec.path, err)
			}
			files[spec.path] = fileID
		}
		if err := w.AddReference(ctx, fileID, Reference{
			Name: spec.name, Kind: "reference.call", Receiver: "svc",
			Line: spec.line, Col: spec.col,
		}); err != nil {
			t.Fatalf("add reference %q: %v", spec.name, err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	// Read the staged ids back in insertion order so each spec's outcome lands
	// on its own row.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM symbol_references WHERE index_run_id = ? ORDER BY id`, run.ID)
	if err != nil {
		t.Fatalf("read reference ids: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read reference ids: %v", err)
	}
	if len(ids) != len(specs) {
		t.Fatalf("staged %d references, want %d", len(ids), len(specs))
	}

	outcomes := make([]RefOutcome, 0, len(specs))
	for i, spec := range specs {
		outcomes = append(outcomes, RefOutcome{RefID: ids[i], Resolution: spec.resolution})
	}
	if err := r.MarkResolutions(ctx, outcomes); err != nil {
		t.Fatalf("mark resolutions: %v", err)
	}
	return run.ID
}

// A negative answer must be diagnosable. ResolutionStats can say that none of
// this run resolved; only ResolutionsForName can say which reference failed,
// how it failed, and where it is written.
func TestResolutionsForNameReportsPerReferenceOutcomes(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	// Deliberately staged out of the order they must come back in: the wanted
	// order is by path then position, and handler.go sorts before worker.go
	// while line 88 sorts before line 140.
	run := seedResolutions(t, r, []refSpec{
		{path: "svc/worker.go", name: "Get", line: 12, col: 3, resolution: ResolutionUnresolved},
		{path: "svc/handler.go", name: "Get", line: 140, col: 9, resolution: ResolutionUnresolved},
		{path: "svc/handler.go", name: "Get", line: 88, col: 5, resolution: ResolutionAmbiguous},
		// A different name in the same run must not bleed into the answer.
		{path: "svc/handler.go", name: "Put", line: 90, col: 2, resolution: ResolutionAmbiguous},
	})

	got, err := r.ResolutionsForName(ctx, run, "Get", 0)
	if err != nil {
		t.Fatalf("resolutions for name: %v", err)
	}
	want := []ReferenceResolution{
		{Name: "Get", Kind: "reference.call", Receiver: "svc",
			Path: "svc/handler.go", Line: 88, Col: 5, Resolution: ResolutionAmbiguous},
		{Name: "Get", Kind: "reference.call", Receiver: "svc",
			Path: "svc/handler.go", Line: 140, Col: 9, Resolution: ResolutionUnresolved},
		{Name: "Get", Kind: "reference.call", Receiver: "svc",
			Path: "svc/worker.go", Line: 12, Col: 3, Resolution: ResolutionUnresolved},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d resolutions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g := got[i]
		if g.RefID == 0 {
			t.Errorf("resolution %d carries no reference id", i)
		}
		// An ambiguous or unresolved reference points at no symbol, and the
		// column holds NULL rather than 0 on disk.
		if g.SymbolID != 0 {
			t.Errorf("resolution %d bound symbol %d, want none", i, g.SymbolID)
		}
		g.RefID = 0
		if g != want[i] {
			t.Errorf("resolution %d = %+v, want %+v", i, g, want[i])
		}
	}

	// The aggregate cannot tell these apart; that is the whole point.
	stats, err := r.ResolutionStats(ctx, run)
	if err != nil {
		t.Fatalf("resolution stats: %v", err)
	}
	if stats.Ambiguous != 2 || stats.Unresolved != 2 || stats.Resolved != 0 {
		t.Fatalf("stats = %+v, want 2 ambiguous, 2 unresolved, 0 resolved", stats)
	}
}

// A name nobody references is an empty result, not an error: "we looked and
// nothing is recorded" is itself a usable answer.
func TestResolutionsForNameUnknownNameIsEmpty(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run := seedResolutions(t, r, []refSpec{
		{path: "svc/handler.go", name: "Get", line: 88, col: 5, resolution: ResolutionAmbiguous},
	})

	got, err := r.ResolutionsForName(ctx, run, "NeverWritten", 0)
	if err != nil {
		t.Fatalf("resolutions for name: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d resolutions for an unreferenced name, want 0", len(got))
	}
}

// The result is bounded whatever the caller asks for, so a common name in a
// large tree cannot pull the whole table into memory.
func TestResolutionsForNameIsBounded(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	const staged = maxNameResolutions + 17
	specs := make([]refSpec, 0, staged)
	for i := 0; i < staged; i++ {
		specs = append(specs, refSpec{
			path: "svc/handler.go", name: "Get", line: i + 1, resolution: ResolutionUnresolved,
		})
	}
	run := seedResolutions(t, r, specs)

	got, err := r.ResolutionsForName(ctx, run, "Get", 0)
	if err != nil {
		t.Fatalf("resolutions for name: %v", err)
	}
	if len(got) != maxNameResolutions {
		t.Fatalf("default limit returned %d rows, want %d", len(got), maxNameResolutions)
	}
	// The bounded prefix is the deterministic one, not an arbitrary slice.
	if got[0].Line != 1 {
		t.Errorf("first row is line %d, want the lowest line", got[0].Line)
	}

	got, err = r.ResolutionsForName(ctx, run, "Get", 5)
	if err != nil {
		t.Fatalf("resolutions for name with explicit limit: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("explicit limit returned %d rows, want 5", len(got))
	}
}

// Cancellation must be honoured, not merely accepted.
func TestResolutionsForNameHonoursContext(t *testing.T) {
	r, _ := newTestRegistry(t)

	run := seedResolutions(t, r, []refSpec{
		{path: "svc/handler.go", name: "Get", line: 88, col: 5, resolution: ResolutionAmbiguous},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ResolutionsForName(ctx, run, "Get", 0); err == nil {
		t.Fatal("cancelled context returned no error")
	}
}
