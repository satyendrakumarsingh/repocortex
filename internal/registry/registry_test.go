package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Note on test isolation: store.OpenMemory uses the DSN
// "file::memory:?cache=shared", so two stores open at the same moment in one
// process are the SAME database. Every test therefore opens exactly one store
// and closes it before returning, and anything needing two independent bodies
// of content builds them as two runs inside one store. That is the stronger
// arrangement for digest tests anyway: two runs in one database necessarily get
// different rowids, so an equal digest proves rowid independence.

func newTestRegistry(t *testing.T) (*Registry, *store.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := New(db)
	r.now = func() int64 { return 1_700_000_000 }
	return r, db
}

// --- content fixtures -------------------------------------------------------

type symSpec struct {
	fqn  string
	name string
	line int
	kind model.EntityKind
}

type fileSpec struct {
	path    string
	lang    string
	hash    string
	content string
	syms    []symSpec
}

type edgeSpec struct {
	srcFQN string
	dstFQN string
	kind   model.EdgeKind
	conf   model.Confidence
	source model.SourceType
	evFile string
	evLine int
}

// writeContent inserts files (in the given slice order) then edges (likewise),
// so callers can vary insertion order while keeping logical content identical.
func writeContent(t *testing.T, r *Registry, runID int64, files []fileSpec, edges []edgeSpec) map[string]int64 {
	t.Helper()
	ctx := context.Background()

	w, err := r.NewWriter(ctx, runID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	symIDs := map[string]int64{}
	for _, fs := range files {
		f := &model.File{
			Path:        fs.path,
			Language:    fs.lang,
			ContentHash: fs.hash,
			SizeBytes:   int64(len(fs.content)),
			// Deliberately varied per call: mod time must never reach the digest.
			ModTimeUnix: time.Now().UnixNano(),
			ParseStatus: "ok",
		}
		fileID, err := w.AddFile(ctx, f)
		if err != nil {
			t.Fatalf("add file %q: %v", fs.path, err)
		}
		if fs.content != "" {
			if err := w.IndexFileText(ctx, fileID, fs.path, fs.content); err != nil {
				t.Fatalf("index text %q: %v", fs.path, err)
			}
		}
		for _, ss := range fs.syms {
			s := &model.Symbol{
				Kind:       ss.kind,
				Name:       ss.name,
				FQN:        ss.fqn,
				FileID:     fileID,
				Range:      model.Range{Start: model.Position{Line: ss.line}, End: model.Position{Line: ss.line + 3}},
				Exported:   true,
				Confidence: model.ConfidenceHigh,
				Source:     model.SourceSourceCode,
			}
			id, err := w.AddSymbol(ctx, s)
			if err != nil {
				t.Fatalf("add symbol %q: %v", ss.fqn, err)
			}
			symIDs[ss.fqn] = id
		}
	}

	for _, es := range edges {
		e := &model.Edge{
			SrcID:      symIDs[es.srcFQN],
			DstID:      symIDs[es.dstFQN],
			Kind:       es.kind,
			Confidence: es.conf,
			Evidence: model.Evidence{
				FilePath: es.evFile,
				Range:    model.Range{Start: model.Position{Line: es.evLine}},
				Source:   es.source,
			},
		}
		if _, err := w.AddEdge(ctx, e); err != nil {
			t.Fatalf("add edge %s->%s: %v", es.srcFQN, es.dstFQN, err)
		}
	}

	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return symIDs
}

func sampleContent() ([]fileSpec, []edgeSpec) {
	files := []fileSpec{
		{
			path: "pkg/a.go", lang: "go", hash: "hash-a",
			content: "func handleCallback(w http.ResponseWriter) { updateStatus() }",
			syms: []symSpec{
				{fqn: "pkg/a.go#handleCallback", name: "handleCallback", line: 10, kind: model.KindFunction},
				{fqn: "pkg/a.go#helper", name: "helper", line: 40, kind: model.KindFunction},
			},
		},
		{
			path: "pkg/b.go", lang: "go", hash: "hash-b",
			content: "func updateStatus() { /* mutate the RetryPolicy */ }",
			syms: []symSpec{
				{fqn: "pkg/b.go#updateStatus", name: "updateStatus", line: 5, kind: model.KindFunction},
			},
		},
	}
	edges := []edgeSpec{
		{srcFQN: "pkg/a.go#handleCallback", dstFQN: "pkg/b.go#updateStatus",
			kind: model.EdgeCalls, conf: model.ConfidenceHigh, source: model.SourceSourceCode,
			evFile: "pkg/a.go", evLine: 11},
		{srcFQN: "pkg/a.go#helper", dstFQN: "pkg/b.go#updateStatus",
			kind: model.EdgeReferences, conf: model.ConfidenceMedium, source: model.SourceSourceCode,
			evFile: "pkg/a.go", evLine: 42},
	}
	return files, edges
}

func mustCount(t *testing.T, db *store.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// --- two-phase swap ---------------------------------------------------------

func TestCommitRunSwapsAtomically(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	files, edges := sampleContent()
	run1, err := r.BeginRun(ctx, "auto", "rev1")
	if err != nil {
		t.Fatalf("begin run 1: %v", err)
	}
	writeContent(t, r, run1.ID, files, edges)
	if err := r.CommitRun(ctx, run1.ID); err != nil {
		t.Fatalf("commit run 1: %v", err)
	}

	// A second pass over a changed tree.
	files2, edges2 := sampleContent()
	files2[0].hash = "hash-a-v2"
	files2 = append(files2, fileSpec{
		path: "pkg/c.go", lang: "go", hash: "hash-c", content: "func newThing() {}",
		syms: []symSpec{{fqn: "pkg/c.go#newThing", name: "newThing", line: 1, kind: model.KindFunction}},
	})
	run2, err := r.BeginRun(ctx, "auto", "rev2")
	if err != nil {
		t.Fatalf("begin run 2: %v", err)
	}
	writeContent(t, r, run2.ID, files2, edges2)

	// Before the swap, readers still see run 1 and nothing of run 2.
	cur, err := r.CurrentRun(ctx)
	if err != nil {
		t.Fatalf("current run mid-index: %v", err)
	}
	if cur.ID != run1.ID {
		t.Fatalf("mid-index reader saw run %d, want the previously committed run %d", cur.ID, run1.ID)
	}

	if err := r.CommitRun(ctx, run2.ID); err != nil {
		t.Fatalf("commit run 2: %v", err)
	}

	cur, err = r.CurrentRun(ctx)
	if err != nil {
		t.Fatalf("current run after swap: %v", err)
	}
	if cur.ID != run2.ID {
		t.Fatalf("after swap current run is %d, want %d", cur.ID, run2.ID)
	}
	if cur.Status != model.StatusReady {
		t.Errorf("status = %q, want %q", cur.Status, model.StatusReady)
	}
	if cur.FilesIndexed != 3 || cur.SymbolsFound != 4 || cur.EdgesFound != 2 {
		t.Errorf("counts = files %d symbols %d edges %d, want 3/4/2",
			cur.FilesIndexed, cur.SymbolsFound, cur.EdgesFound)
	}
	if cur.RegistryDigest == "" {
		t.Error("committed run has no recorded registry digest")
	}

	// The old run and every row hanging off it are gone.
	if n := mustCount(t, db, `SELECT count(*) FROM index_runs WHERE id = ?`, run1.ID); n != 0 {
		t.Errorf("old run row survived the swap (%d rows)", n)
	}
	for _, tbl := range []string{"files", "symbols", "edges"} {
		q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE index_run_id = ?`, tbl)
		if n := mustCount(t, db, q, run1.ID); n != 0 {
			t.Errorf("%s rows from the retired run survived: %d", tbl, n)
		}
	}
	// files_fts has no foreign key, so orphaned search rows are the failure
	// mode this specifically guards.
	if n := mustCount(t, db, `SELECT count(*) FROM files_fts`); n != 3 {
		t.Errorf("files_fts holds %d rows, want 3 (one per file in the live run)", n)
	}
	if n := mustCount(t, db, `SELECT count(*) FROM files_fts_map`); n != 3 {
		t.Errorf("files_fts_map holds %d rows, want 3", n)
	}

	if n := mustCount(t, db, `SELECT count(*) FROM index_runs WHERE committed = 1`); n != 1 {
		t.Fatalf("committed run count = %d, want exactly 1", n)
	}
}

func TestCommitRunRejectsSecondCommittedRun(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	run1, _ := r.BeginRun(ctx, "auto", "rev1")
	if err := r.CommitRun(ctx, run1.ID); err != nil {
		t.Fatalf("commit run 1: %v", err)
	}
	// Prove the gate can fail: try to make a second run committed behind the
	// registry's back. The partial unique index must refuse.
	run2, _ := r.BeginRun(ctx, "auto", "rev2")
	_, err := db.ExecContext(ctx, `UPDATE index_runs SET committed = 1 WHERE id = ?`, run2.ID)
	if err == nil {
		t.Fatal("schema allowed two committed runs; idx_index_runs_current is not doing its job")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("unexpected error shape: %v", err)
	}
}

func TestAbortRunLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	files, edges := sampleContent()
	run, err := r.BeginRun(ctx, "fast", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	writeContent(t, r, run.ID, files, edges)

	if err := r.AbortRun(ctx, run.ID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM index_runs`,
		`SELECT count(*) FROM files`,
		`SELECT count(*) FROM symbols`,
		`SELECT count(*) FROM edges`,
		`SELECT count(*) FROM files_fts`,
		`SELECT count(*) FROM files_fts_map`,
	} {
		if n := mustCount(t, db, q); n != 0 {
			t.Errorf("%s = %d, want 0 after abort", q, n)
		}
	}

	if err := r.AbortRun(ctx, run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("second abort error = %v, want ErrRunNotFound", err)
	}
}

func TestAbortRefusesCommittedRun(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, _ := r.BeginRun(ctx, "auto", "rev")
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := r.AbortRun(ctx, run.ID); err == nil {
		t.Fatal("abort of the committed run succeeded; live data is deletable")
	}
}

// TestCrashedRunIsInvisibleAndReclaimable simulates a process killed mid-index:
// batches were committed, the run never was, and the Writer was never flushed
// or closed.
func TestCrashedRunIsInvisibleAndReclaimable(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	files, edges := sampleContent()
	good, _ := r.BeginRun(ctx, "auto", "rev1")
	writeContent(t, r, good.ID, files, edges)
	if err := r.CommitRun(ctx, good.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The crash: a writer that gets far enough to commit batches, then vanishes.
	// batchSize is lowered so several batches land before the "crash".
	r.batchSize = 2
	doomed, _ := r.BeginRun(ctx, "auto", "rev2")
	w, err := r.NewWriter(ctx, doomed.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	for i := 0; i < 5; i++ {
		f := &model.File{Path: fmt.Sprintf("staging/%d.go", i), Language: "go", ContentHash: "h", ParseStatus: "ok"}
		id, err := w.AddFile(ctx, f)
		if err != nil {
			t.Fatalf("add file: %v", err)
		}
		if err := w.IndexFileText(ctx, id, f.Path, "func secretStagingThing() {}"); err != nil {
			t.Fatalf("index text: %v", err)
		}
	}
	// No Flush, no Close. The last partial batch is lost; earlier ones persist.
	if n := mustCount(t, db, `SELECT count(*) FROM files WHERE index_run_id = ?`, doomed.ID); n == 0 {
		t.Fatal("test setup did not persist any staging rows, so it is not simulating a crash")
	}

	// Invisible: readers still see the committed run only.
	cur, err := r.CurrentRun(ctx)
	if err != nil {
		t.Fatalf("current run: %v", err)
	}
	if cur.ID != good.ID {
		t.Fatalf("reader saw run %d, want the committed run %d", cur.ID, good.ID)
	}
	hits, err := r.SearchFiles(ctx, "staging", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("search surfaced %d file(s) from an uncommitted run: %+v", len(hits), hits)
	}

	// Reclaimable.
	ids, err := r.ReclaimAbandoned(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(ids) != 1 || ids[0] != doomed.ID {
		t.Fatalf("reclaimed %v, want [%d]", ids, doomed.ID)
	}
	if n := mustCount(t, db, `SELECT count(*) FROM files WHERE index_run_id = ?`, doomed.ID); n != 0 {
		t.Errorf("%d staging file rows survived reclaim", n)
	}
	if n := mustCount(t, db, `SELECT count(*) FROM files_fts`); n != 2 {
		t.Errorf("files_fts = %d rows after reclaim, want 2 (the live run's files)", n)
	}
	if cur, err = r.CurrentRun(ctx); err != nil || cur.ID != good.ID {
		t.Fatalf("committed run damaged by reclaim: run=%v err=%v", cur, err)
	}
}

// --- edge provenance --------------------------------------------------------

func TestAddEdgeRejectsInvalidProvenance(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	files, _ := sampleContent()
	run, _ := r.BeginRun(ctx, "auto", "rev")
	ids := writeContent(t, r, run.ID, files, nil)

	src := ids["pkg/a.go#handleCallback"]
	dst := ids["pkg/b.go#updateStatus"]

	cases := []struct {
		name string
		edge model.Edge
		want string
	}{
		{
			name: "unset confidence",
			edge: model.Edge{SrcID: src, DstID: dst, Kind: model.EdgeCalls,
				Evidence: model.Evidence{FilePath: "pkg/a.go", Source: model.SourceSourceCode}},
			want: "invalid confidence",
		},
		{
			name: "bogus confidence",
			edge: model.Edge{SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: "very-sure",
				Evidence: model.Evidence{FilePath: "pkg/a.go", Source: model.SourceSourceCode}},
			want: "invalid confidence",
		},
		{
			name: "exact from a non-compiler source",
			edge: model.Edge{SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact,
				Evidence: model.Evidence{FilePath: "pkg/a.go", Source: model.SourceInferred}},
			want: "requires source=compiler",
		},
		{
			name: "exact with no source at all",
			edge: model.Edge{SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact,
				Evidence: model.Evidence{FilePath: "pkg/a.go"}},
			want: "requires source=compiler",
		},
		{
			name: "no evidence source",
			edge: model.Edge{SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
				Evidence: model.Evidence{FilePath: "pkg/a.go"}},
			want: "evidence source must be set",
		},
		{
			name: "no endpoints",
			edge: model.Edge{Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
				Evidence: model.Evidence{FilePath: "pkg/a.go", Source: model.SourceSourceCode}},
			want: "src and dst must be set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh Writer per case: the first rejection latches the failure,
			// which is the behaviour we want in production but would mask the
			// per-case error here.
			w, err := r.NewWriter(ctx, run.ID)
			if err != nil {
				t.Fatalf("new writer: %v", err)
			}
			defer func() { _ = w.Close() }()

			e := tc.edge
			_, err = w.AddEdge(ctx, &e)
			if err == nil {
				t.Fatalf("AddEdge accepted an edge it must reject: %+v", tc.edge)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSQLCheckAlsoRejectsExactNonCompiler proves the second, independent guard
// works — the one that catches a writer bypassing the Go check entirely. A gate
// only ever seen passing proves nothing, so the defect is injected deliberately.
func TestSQLCheckAlsoRejectsExactNonCompiler(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)
	files, _ := sampleContent()
	run, _ := r.BeginRun(ctx, "auto", "rev")
	ids := writeContent(t, r, run.ID, files, nil)

	_, err := db.ExecContext(ctx, `
		INSERT INTO edges (index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, evidence_col, source)
		VALUES (?,?,?,'CALLS','exact','pkg/a.go',1,0,'inferred')`,
		run.ID, ids["pkg/a.go#handleCallback"], ids["pkg/b.go#updateStatus"])
	if err == nil {
		t.Fatal("raw SQL wrote an exact edge with source=inferred; the CHECK constraint is not protecting invariant 2")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("unexpected error shape: %v", err)
	}
}

func TestAddSymbolRejectsExactNonCompiler(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, _ := r.BeginRun(ctx, "auto", "rev")
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	f := &model.File{Path: "x.go", ContentHash: "h", Language: "go"}
	fileID, err := w.AddFile(ctx, f)
	if err != nil {
		t.Fatalf("add file: %v", err)
	}
	_, err = w.AddSymbol(ctx, &model.Symbol{
		Kind: model.KindFunction, Name: "f", FQN: "x.go#f", FileID: fileID,
		Confidence: model.ConfidenceExact, Source: model.SourceInferred,
	})
	if err == nil {
		t.Fatal("AddSymbol accepted confidence=exact from a non-compiler source")
	}
	if !strings.Contains(err.Error(), "requires source=compiler") {
		t.Errorf("error = %v, want it to mention the exact/compiler rule", err)
	}
}

// TestCommitRejectsCrossRunEdge injects the defect that the schema cannot catch
// (nothing ties edges.index_run_id to symbols.index_run_id) and confirms the
// commit gate refuses to publish it.
func TestCommitRejectsCrossRunEdge(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	files, _ := sampleContent()
	runA, _ := r.BeginRun(ctx, "auto", "a")
	idsA := writeContent(t, r, runA.ID, files, nil)

	runB, _ := r.BeginRun(ctx, "auto", "b")
	idsB := writeContent(t, r, runB.ID, files, nil)

	// An edge in run B pointing at a symbol owned by run A. Committing this
	// would leave run B's graph referencing rows that the next swap deletes.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO edges (index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, evidence_col, source)
		VALUES (?,?,?,'CALLS','high','pkg/a.go',1,0,'source_code')`,
		runB.ID, idsB["pkg/a.go#handleCallback"], idsA["pkg/b.go#updateStatus"]); err != nil {
		t.Fatalf("inject cross-run edge: %v", err)
	}

	err := r.CommitRun(ctx, runB.ID)
	if err == nil {
		t.Fatal("commit published a run containing a cross-run edge")
	}
	if !strings.Contains(err.Error(), "another run") {
		t.Errorf("error = %v, want it to name the cross-run problem", err)
	}
	if n := mustCount(t, db, `SELECT count(*) FROM index_runs WHERE committed = 1`); n != 0 {
		t.Errorf("a run was published despite failing validation")
	}
}

// --- incremental replace ----------------------------------------------------

func TestReplaceFileRemovesStaleEdges(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)

	files, edges := sampleContent()
	// A third edge produced by parsing a.go but with both endpoints elsewhere:
	// the kind of edge nothing cascades to.
	files = append(files, fileSpec{
		path: "pkg/d.go", lang: "go", hash: "hash-d", content: "func other() {}",
		syms: []symSpec{{fqn: "pkg/d.go#other", name: "other", line: 2, kind: model.KindFunction}},
	})
	edges = append(edges, edgeSpec{
		srcFQN: "pkg/b.go#updateStatus", dstFQN: "pkg/d.go#other",
		kind: model.EdgeReferences, conf: model.ConfidenceLow, source: model.SourceInferred,
		evFile: "pkg/a.go", evLine: 90,
	})

	run, _ := r.BeginRun(ctx, "auto", "rev")
	writeContent(t, r, run.ID, files, edges)

	if n := mustCount(t, db, `SELECT count(*) FROM edges WHERE index_run_id = ?`, run.ID); n != 3 {
		t.Fatalf("setup wrote %d edges, want 3", n)
	}

	// a.go changes: handleCallback is gone, renamedEntry replaces it, and the
	// only edge it now produces is a reference to helper.
	newFile := &model.File{Path: "pkg/a.go", Language: "go", ContentHash: "hash-a-v2", ParseStatus: "ok"}
	newSyms := []model.Symbol{
		{ID: -1, Kind: model.KindFunction, Name: "renamedEntry", FQN: "pkg/a.go#renamedEntry",
			Range:      model.Range{Start: model.Position{Line: 12}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode},
		{ID: -2, Kind: model.KindFunction, Name: "helper", FQN: "pkg/a.go#helper",
			Range:      model.Range{Start: model.Position{Line: 40}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode},
	}
	newEdges := []model.Edge{
		{SrcID: -1, DstID: -2, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{FilePath: "pkg/a.go", Range: model.Range{Start: model.Position{Line: 13}},
				Source: model.SourceSourceCode}},
	}

	if err := r.ReplaceFile(ctx, run.ID, newFile, newSyms, newEdges,
		WithContent("func renamedEntry() { helper() }")); err != nil {
		t.Fatalf("replace file: %v", err)
	}

	remaining, err := r.Edges(ctx, run.ID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("after replace there are %d edges, want exactly the 1 new one: %+v", len(remaining), remaining)
	}
	if remaining[0].SrcID != newSyms[0].ID || remaining[0].DstID != newSyms[1].ID {
		t.Errorf("edge endpoints = %d -> %d, want %d -> %d (placeholders were not remapped)",
			remaining[0].SrcID, remaining[0].DstID, newSyms[0].ID, newSyms[1].ID)
	}

	// The vanished symbol is gone, and so are the FTS rows for the old text.
	syms, err := r.Symbols(ctx, run.ID)
	if err != nil {
		t.Fatalf("list symbols: %v", err)
	}
	for _, s := range syms {
		if s.FQN == "pkg/a.go#handleCallback" {
			t.Error("a symbol deleted from the file survived ReplaceFile")
		}
	}
	if n := mustCount(t, db, `SELECT count(*) FROM files WHERE index_run_id = ? AND path = 'pkg/a.go'`, run.ID); n != 1 {
		t.Errorf("pkg/a.go has %d rows, want exactly 1", n)
	}
	// Three files carry text; a.go's old row must have been replaced, not added
	// alongside — a leftover would keep matching the pre-change source.
	if n := mustCount(t, db, `SELECT count(*) FROM files_fts`); n != 3 {
		t.Errorf("files_fts = %d, want 3 (one row per file with content)", n)
	}

	// And the replacement is committable — the stale state did not poison it.
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit after replace: %v", err)
	}
	hits, err := r.SearchFiles(ctx, "renamed", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "pkg/a.go" {
		t.Errorf("search for the replacement text returned %+v", hits)
	}
	// And the text that ReplaceFile removed no longer matches anything.
	if hits, err = r.SearchFiles(ctx, "handleCallback", 10); err != nil {
		t.Fatalf("search: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("search still matches source deleted by ReplaceFile: %+v", hits)
	}
}

func TestReplaceFileRejectsUnknownPlaceholder(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, _ := r.BeginRun(ctx, "auto", "rev")

	err := r.ReplaceFile(ctx, run.ID,
		&model.File{Path: "x.go", ContentHash: "h"},
		[]model.Symbol{{ID: -1, Kind: model.KindFunction, Name: "f", FQN: "x.go#f",
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode}},
		[]model.Edge{{SrcID: -1, DstID: -99, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{FilePath: "x.go", Source: model.SourceSourceCode}}})
	if err == nil {
		t.Fatal("ReplaceFile accepted an edge referencing a placeholder with no symbol")
	}
}

func TestReplaceFileRefusesCommittedRun(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	files, edges := sampleContent()
	run, _ := r.BeginRun(ctx, "auto", "rev")
	writeContent(t, r, run.ID, files, edges)
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	err := r.ReplaceFile(ctx, run.ID, &model.File{Path: "pkg/a.go", ContentHash: "h2"}, nil, nil)
	if err == nil {
		t.Fatal("ReplaceFile mutated the committed run; readers could observe a half-replaced file")
	}
}

// --- full-text search -------------------------------------------------------

func TestFTSExpansionFindsIdentifierFragment(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	files, edges := sampleContent()
	run, _ := r.BeginRun(ctx, "auto", "rev")
	writeContent(t, r, run.ID, files, edges)
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// "callback" appears in the corpus only as a fragment of handleCallback.
	// Finding it is the entire justification for the expanded column.
	hits, err := r.SearchFiles(ctx, "callback", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "pkg/a.go" {
		t.Fatalf("search for \"callback\" returned %+v, want pkg/a.go — identifier expansion is not working", hits)
	}

	// The original text must still be searchable verbatim, not only expanded.
	if hits, err = r.SearchFiles(ctx, `"handleCallback"`, 10); err != nil {
		t.Fatalf("search: %v", err)
	} else if len(hits) != 1 {
		t.Errorf("verbatim identifier search returned %+v", hits)
	}
}

// --- concurrency ------------------------------------------------------------

// TestConcurrentReadersDuringWriteRun is the reason writes are batched rather
// than wrapped in one long transaction. Readers run continuously while a
// staging run is filled; every one of them must complete (not stall behind a
// minutes-long write lock) and must see the committed run's content unchanged
// throughout. Run under -race.
func TestConcurrentReadersDuringWriteRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, _ := newTestRegistry(t)

	files, edges := sampleContent()
	live, _ := r.BeginRun(ctx, "auto", "live")
	writeContent(t, r, live.ID, files, edges)
	if err := r.CommitRun(ctx, live.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	r.batchSize = 50
	staging, _ := r.BeginRun(ctx, "auto", "staging")

	done := make(chan struct{})
	var wg sync.WaitGroup
	readErr := make(chan error, 8)
	var reads int64
	var mu sync.Mutex

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				cur, err := r.CurrentRun(ctx)
				if err != nil {
					readErr <- fmt.Errorf("current run: %w", err)
					return
				}
				if cur.ID != live.ID {
					readErr <- fmt.Errorf("reader saw run %d, want the committed run %d", cur.ID, live.ID)
					return
				}
				fs, err := r.Files(ctx, cur.ID)
				if err != nil {
					readErr <- fmt.Errorf("list files: %w", err)
					return
				}
				if len(fs) != 2 {
					readErr <- fmt.Errorf("committed run has %d files mid-write, want 2", len(fs))
					return
				}
				hits, err := r.SearchFiles(ctx, "callback", 10)
				if err != nil {
					readErr <- fmt.Errorf("search: %w", err)
					return
				}
				for _, h := range hits {
					if strings.HasPrefix(h.Path, "staging/") {
						readErr <- fmt.Errorf("search leaked an uncommitted file: %s", h.Path)
						return
					}
				}
				mu.Lock()
				reads++
				mu.Unlock()
			}
		}()
	}

	w, err := r.NewWriter(ctx, staging.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	for i := 0; i < 400; i++ {
		f := &model.File{Path: fmt.Sprintf("staging/%03d.go", i), Language: "go",
			ContentHash: fmt.Sprintf("h%03d", i), ParseStatus: "ok"}
		fid, err := w.AddFile(ctx, f)
		if err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("add file %d: %v", i, err)
		}
		if err := w.IndexFileText(ctx, fid, f.Path, "func stagingCallbackThing() {}"); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("index text %d: %v", i, err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	_ = w.Close()

	close(done)
	wg.Wait()
	close(readErr)
	for err := range readErr {
		t.Error(err)
	}

	mu.Lock()
	n := reads
	mu.Unlock()
	if n == 0 {
		t.Fatal("no read completed while the write run was in progress — readers were starved")
	}
	t.Logf("%d reader iterations completed during a 400-file staging write", n)

	// And the swap still works afterwards.
	if err := r.CommitRun(ctx, staging.ID); err != nil {
		t.Fatalf("commit staging: %v", err)
	}
	cur, err := r.CurrentRun(ctx)
	if err != nil || cur.ID != staging.ID {
		t.Fatalf("post-swap current run = %v (err %v), want %d", cur, err, staging.ID)
	}
}

func TestWriterRejectsCommittedRun(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, _ := r.BeginRun(ctx, "auto", "rev")
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := r.NewWriter(ctx, run.ID); err == nil {
		t.Fatal("NewWriter opened a writer on the committed run")
	}
	if _, err := r.NewWriter(ctx, run.ID+999); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("NewWriter on a missing run = %v, want ErrRunNotFound", err)
	}
}

func TestDegradedStatusOnParseFailures(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, _ := r.BeginRun(ctx, "auto", "rev")
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if _, err := w.AddFile(ctx, &model.File{Path: "ok.go", ContentHash: "h", ParseStatus: "ok"}); err != nil {
		t.Fatalf("add file: %v", err)
	}
	if _, err := w.AddFile(ctx, &model.File{Path: "bad.go", ContentHash: "h",
		ParseStatus: "failed", ParseError: "unexpected token"}); err != nil {
		t.Fatalf("add file: %v", err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	_ = w.Close()

	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	cur, err := r.CurrentRun(ctx)
	if err != nil {
		t.Fatalf("current run: %v", err)
	}
	if cur.Status != model.StatusDegraded {
		t.Errorf("status = %q, want %q — a run with parse failures must not claim to be complete",
			cur.Status, model.StatusDegraded)
	}
	if cur.ParseFailures != 1 {
		t.Errorf("parse failures = %d, want 1", cur.ParseFailures)
	}
}

func TestNoCommittedRunIsNotAnError(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	if _, err := r.CurrentRun(ctx); !errors.Is(err, ErrNoCommittedRun) {
		t.Errorf("CurrentRun on an empty store = %v, want ErrNoCommittedRun", err)
	}
}
