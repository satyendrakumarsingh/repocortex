package registry

import (
	"context"
	"math/rand"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// MarkResolutions must apply its batch in ascending reference-id order.
//
// This is the exact regression that made `cortex resolve` superlinear. The
// method used to take map[int64]ResolvedRef and iterate it directly, so Go's
// randomised map order sent every UPDATE to a different part of the reference
// table's B-trees. On a 25,000-file tree that was 2.6 million shuffled
// root-to-leaf descents into indexes far larger than the page cache, and it cost
// 111 seconds of a 180-second pass.
//
// Order is observed with an AFTER UPDATE trigger, which fires once per row in
// the order SQLite touches them. Nothing else exposes it: the batching is an
// implementation detail and the resulting table contents are identical either
// way, which is precisely why the bug survived every correctness test.
func TestMarkResolutionsWritesInKeyOrder(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "auto", "order")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	fileID, err := w.AddFile(ctx, &model.File{
		Path: "a/B.java", Language: "java", ContentHash: "h", ParseStatus: "ok",
	})
	if err != nil {
		t.Fatalf("add file: %v", err)
	}
	// More than markChunk references, so the batching path is exercised rather
	// than the single-statement one.
	const refs = markChunk*2 + 37
	for i := 0; i < refs; i++ {
		if err := w.AddReference(ctx, fileID, Reference{
			Name: "target", Kind: "reference.call", Line: i + 1,
		}); err != nil {
			t.Fatalf("add reference: %v", err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	ids := make([]int64, 0, refs)
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM symbol_references WHERE index_run_id = ? ORDER BY id`, run.ID)
	if err != nil {
		t.Fatalf("read reference ids: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if len(ids) != refs {
		t.Fatalf("staged %d references, want %d", len(ids), refs)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE update_order (seq INTEGER PRIMARY KEY AUTOINCREMENT, ref_id INTEGER);
		CREATE TRIGGER record_update AFTER UPDATE ON symbol_references
		BEGIN
			INSERT INTO update_order (ref_id) VALUES (NEW.id);
		END;`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	// Hand the batch over shuffled. A caller is entitled to do that; the point
	// is that the registry, not the caller, is what guarantees the write order.
	outcomes := make([]RefOutcome, 0, refs)
	for _, id := range ids {
		outcomes = append(outcomes, RefOutcome{RefID: id, Resolution: ResolutionUnresolved})
	}
	rnd := rand.New(rand.NewSource(1))
	rnd.Shuffle(len(outcomes), func(i, j int) {
		outcomes[i], outcomes[j] = outcomes[j], outcomes[i]
	})
	shuffled := make([]RefOutcome, len(outcomes))
	copy(shuffled, outcomes)

	if err := r.MarkResolutions(ctx, outcomes); err != nil {
		t.Fatalf("mark resolutions: %v", err)
	}

	// The caller's slice must come back untouched: reordering it under them
	// would be an invisible side effect.
	for i := range shuffled {
		if outcomes[i] != shuffled[i] {
			t.Fatalf("MarkResolutions reordered the caller's slice at %d", i)
		}
	}

	var applied []int64
	orderRows, err := db.QueryContext(ctx, `SELECT ref_id FROM update_order ORDER BY seq`)
	if err != nil {
		t.Fatalf("read update order: %v", err)
	}
	defer func() { _ = orderRows.Close() }()
	for orderRows.Next() {
		var id int64
		if err := orderRows.Scan(&id); err != nil {
			t.Fatalf("scan order: %v", err)
		}
		applied = append(applied, id)
	}
	if err := orderRows.Err(); err != nil {
		t.Fatalf("update order rows: %v", err)
	}

	if len(applied) != refs {
		t.Fatalf("%d rows updated, want %d", len(applied), refs)
	}
	for i := 1; i < len(applied); i++ {
		if applied[i] <= applied[i-1] {
			t.Fatalf("reference %d was updated after %d: MarkResolutions is writing "+
				"in the caller's order again. Shuffled primary-key writes are what "+
				"made resolution superlinear; sort the batch before applying it.",
				applied[i], applied[i-1])
		}
	}
}

// TestMarkResolutionsRecordsEveryOutcome is the correctness half: batching must
// not change what lands, only how it gets there.
func TestMarkResolutionsRecordsEveryOutcome(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "auto", "outcomes")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	fileID, err := w.AddFile(ctx, &model.File{
		Path: "a/B.java", Language: "java", ContentHash: "h", ParseStatus: "ok",
	})
	if err != nil {
		t.Fatalf("add file: %v", err)
	}
	symID, err := w.AddSymbol(ctx, &model.Symbol{
		Kind: model.KindMethod, Name: "target", FQN: "a.B#target", FileID: fileID,
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	})
	if err != nil {
		t.Fatalf("add symbol: %v", err)
	}
	// A count that straddles a chunk boundary, so the short trailing statement
	// is exercised as well as the full ones.
	const refs = markChunk + 5
	for i := 0; i < refs; i++ {
		if err := w.AddReference(ctx, fileID, Reference{
			Name: "target", Kind: "reference.call", Line: i + 1,
		}); err != nil {
			t.Fatalf("add reference: %v", err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	var ids []int64
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM symbol_references WHERE index_run_id = ? ORDER BY id`, run.ID)
	if err != nil {
		t.Fatalf("read ids: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()

	// Rotate the three outcome shapes so resolved, ambiguous and unresolved all
	// cross a chunk boundary.
	want := make(map[int64]RefOutcome, len(ids))
	outcomes := make([]RefOutcome, 0, len(ids))
	for i, id := range ids {
		o := RefOutcome{RefID: id}
		switch i % 3 {
		case 0:
			o.SymbolID, o.Resolution = symID, ResolutionResolved
		case 1:
			o.Resolution = ResolutionAmbiguous
		default:
			o.Resolution = ResolutionUnresolved
		}
		want[id] = o
		outcomes = append(outcomes, o)
	}
	if err := r.MarkResolutions(ctx, outcomes); err != nil {
		t.Fatalf("mark resolutions: %v", err)
	}

	got, err := db.QueryContext(ctx,
		`SELECT id, COALESCE(resolved_symbol_id, 0), resolution
		   FROM symbol_references WHERE index_run_id = ? ORDER BY id`, run.ID)
	if err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	defer func() { _ = got.Close() }()

	var seen int
	for got.Next() {
		var o RefOutcome
		var res string
		if err := got.Scan(&o.RefID, &o.SymbolID, &res); err != nil {
			t.Fatalf("scan outcome: %v", err)
		}
		o.Resolution = Resolution(res)
		if o != want[o.RefID] {
			t.Fatalf("reference %d recorded as %+v, want %+v", o.RefID, o, want[o.RefID])
		}
		seen++
	}
	if err := got.Err(); err != nil {
		t.Fatalf("outcome rows: %v", err)
	}
	if seen != refs {
		t.Fatalf("read back %d references, want %d", seen, refs)
	}
}
