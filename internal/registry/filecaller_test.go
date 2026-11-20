package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fileCallerFixture is a two-file run. a.test.ts has a function `helper` on
// lines 10-20, a call inside it (line 12) and a call outside every definition
// (line 30). b.ts has one definition and a call inside it. Every reference is
// pending, as the parser leaves them.
func fileCallerFixture(t *testing.T) (*Registry, int64, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := New(db)
	run, err := reg.BeginRun(ctx, "auto", "fc")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	addFile := func(path string) int64 {
		id, err := w.AddFile(ctx, &model.File{Path: path, Language: "typescript", ContentHash: "h" + path, ParseStatus: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		ids[path] = id
		return id
	}
	addSym := func(fileID int64, path, name string, start, end int) {
		id, err := w.AddSymbol(ctx, &model.Symbol{
			FileID: fileID, Kind: model.KindFunction, Name: name, FQN: path + ":" + name,
			Range:      model.Range{Start: model.Position{Line: start}, End: model.Position{Line: end}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	a := addFile("a.test.ts")
	b := addFile("b.ts")
	addSym(a, "a.test.ts", "helper", 10, 20)
	addSym(b, "b.ts", "target", 1, 5)
	for _, r := range []struct {
		file int64
		name string
		line int
	}{{a, "target", 12}, {a, "target", 30}, {b, "target", 3}} {
		if err := w.AddReference(ctx, r.file, Reference{Name: r.name, Kind: "reference.call", Line: r.line, ArgCount: model.ArityUnknown}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return reg, run.ID, ids
}

// TestFileCallersAreCreatedOnlyForCodeOutsideDefinitions: a file gets the
// synthetic caller when a reference to bind sits outside every definition, and
// not otherwise; creating it twice changes nothing.
func TestFileCallersAreCreatedOnlyForCodeOutsideDefinitions(t *testing.T) {
	ctx := context.Background()
	reg, runID, _ := fileCallerFixture(t)

	n, err := reg.EnsureFileCallers(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("created %d file caller(s), want 1 (a.test.ts line 30 is outside `helper`)", n)
	}
	if n, err = reg.EnsureFileCallers(ctx, runID); err != nil || n != 0 {
		t.Fatalf("second call created %d (err %v), want 0", n, err)
	}
	ids, err := reg.FileCallerIDs(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ids["a.test.ts"]; !ok || len(ids) != 1 {
		t.Fatalf("file callers = %v, want only a.test.ts", ids)
	}
}

// TestFileCallerIsNotADefinition: the synthetic caller is in no lookup, listing
// or count a definition is in, and is reachable by id for naming a caller.
func TestFileCallerIsNotADefinition(t *testing.T) {
	ctx := context.Background()
	reg, runID, _ := fileCallerFixture(t)
	if _, err := reg.EnsureFileCallers(ctx, runID); err != nil {
		t.Fatal(err)
	}
	name := FileCallerName("a.test.ts")

	if hits, err := reg.FindSymbolsByName(ctx, runID, name, "", 10); err != nil || len(hits) != 0 {
		t.Errorf("FindSymbolsByName(%q) = %v, %v; the synthetic caller must not be found by name", name, hits, err)
	}
	syms, err := reg.Symbols(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		if s.Kind == model.KindFile {
			t.Errorf("Symbols lists %s", s.FQN)
		}
	}
	counts, err := countRun(ctx, reg.db, runID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Symbols != 2 {
		t.Errorf("run counts %d symbols, want the 2 definitions", counts.Symbols)
	}
	if c, total, err := reg.SymbolCandidatesByFQN(ctx, runID, FileCallerFQN("a.test.ts"), 10); err != nil || total != 0 || len(c) != 0 {
		t.Errorf("a qualified-name lookup found the synthetic caller: %v %d %v", c, total, err)
	}

	ids, err := reg.FileCallerIDs(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	hit, ok, err := reg.SymbolByID(ctx, runID, ids["a.test.ts"])
	if err != nil || !ok || hit.Name != name {
		t.Fatalf("SymbolByID = %+v, %v, %v; an edge's source must be nameable as %q", hit, ok, err, name)
	}
}

// TestFileCallerEdgesStayOutOfTheWholeGraphReaders: Edges, the reader the
// whole-graph consumers share, leaves out edges that start at the synthetic
// caller, because those consumers read nodes from Symbols.
func TestFileCallerEdgesStayOutOfTheWholeGraphReaders(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := fileCallerFixture(t)
	if _, err := reg.EnsureFileCallers(ctx, runID); err != nil {
		t.Fatal(err)
	}
	fc, err := reg.FileCallerIDs(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(src int64, line int) model.Edge {
		return model.Edge{SrcID: src, DstID: ids["target"], Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{FilePath: "a.test.ts", Range: model.Range{Start: model.Position{Line: line}},
				Source: model.SourceSourceCode, Detail: "t"}}
	}
	if _, err := reg.AddResolvedEdges(ctx, runID, []model.Edge{mk(ids["helper"], 12), mk(fc["a.test.ts"], 30)}); err != nil {
		t.Fatal(err)
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].SrcID != ids["helper"] {
		t.Fatalf("Edges returned %d edge(s), want only the one from helper", len(edges))
	}
	// The caller query still sees both, which is what a caller list is made of.
	var n int
	if err := reg.db.QueryRowContext(ctx, `SELECT count(*) FROM edges WHERE dst_id = ?`, ids["target"]).Scan(&n); err != nil || n != 2 {
		t.Fatalf("edges into target = %d (%v), want 2", n, err)
	}
	// TopLevelEdges is exactly what Edges left out, with the file it stands for.
	top, err := reg.TopLevelEdges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].SrcID != fc["a.test.ts"] || top[0].CallerPath != "a.test.ts" ||
		top[0].Evidence.Range.Start.Line != 30 || top[0].Confidence != model.ConfidenceHigh {
		t.Fatalf("TopLevelEdges = %+v, want the one edge from the top level of a.test.ts at line 30", top)
	}
}

// TestTopLevelCallsReturnsTheCallersAndTheirEdges: the read the whole-graph
// consumers add to Symbols and Edges returns the synthetic callers that start
// an edge, and exactly those edges.
func TestTopLevelCallsReturnsTheCallersAndTheirEdges(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := fileCallerFixture(t)
	if _, err := reg.EnsureFileCallers(ctx, runID); err != nil {
		t.Fatal(err)
	}
	fc, err := reg.FileCallerIDs(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(src int64, line int) model.Edge {
		return model.Edge{SrcID: src, DstID: ids["target"], Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{FilePath: "a.test.ts", Range: model.Range{Start: model.Position{Line: line}},
				Source: model.SourceSourceCode, Detail: "t"}}
	}
	if _, err := reg.AddResolvedEdges(ctx, runID, []model.Edge{mk(ids["helper"], 12), mk(fc["a.test.ts"], 30)}); err != nil {
		t.Fatal(err)
	}
	top, err := reg.TopLevelCalls(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	// b.ts has a caller row too, but no edge starts there.
	if len(top.Callers) != 1 || top.Callers[0].ID != fc["a.test.ts"] || top.Callers[0].Kind != model.KindFile {
		t.Fatalf("callers = %+v, want only the top level of a.test.ts", top.Callers)
	}
	if len(top.Edges) != 1 || top.Edges[0].SrcID != fc["a.test.ts"] || top.Edges[0].Evidence.Range.Start.Line != 30 {
		t.Fatalf("edges = %+v, want only the call at line 30", top.Edges)
	}
}

// TestPruneFileCallersKeepsOnlyTheOnesWithEdges.
func TestPruneFileCallersKeepsOnlyTheOnesWithEdges(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := fileCallerFixture(t)
	if _, err := reg.EnsureFileCallers(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if n, err := reg.PruneFileCallers(ctx, runID); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want the one with no edge", n, err)
	}
	fileID, err := reg.fileIDOf(ctx, runID, "a.test.ts")
	if err != nil {
		t.Fatal(err)
	}
	id, err := reg.FileCaller(ctx, runID, fileID, "a.test.ts")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddResolvedEdges(ctx, runID, []model.Edge{{
		SrcID: id, DstID: ids["target"], Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{FilePath: "a.test.ts", Range: model.Range{Start: model.Position{Line: 30}},
			Source: model.SourceSourceCode, Detail: "t"}}}); err != nil {
		t.Fatal(err)
	}
	if n, err := reg.PruneFileCallers(ctx, runID); err != nil || n != 0 {
		t.Fatalf("pruned %d (%v) a caller that has an edge", n, err)
	}
}

// fileIDOf is the id of a file of the run, for the tests of this file.
func (r *Registry) fileIDOf(ctx context.Context, runID int64, path string) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM files WHERE index_run_id = ? AND path = ?`, runID, path).Scan(&id)
	return id, err
}
