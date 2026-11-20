package cli

import (
	stdctx "context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestExportIncludesCallsMadeAtAFilesTopLevel: a call outside every function
// starts at the file's synthetic caller. `callers` lists it, so the exported
// graph must hold the edge and its source node.
func TestExportIncludesCallsMadeAtAFilesTopLevel(t *testing.T) {
	ctx := stdctx.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "export-top")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := w.AddFile(ctx, &model.File{Path: "main.py", Language: "python", ContentHash: "h", ParseStatus: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := w.AddSymbol(ctx, &model.Symbol{
		FileID: fileID, Kind: model.KindFunction, Name: "setup", FQN: "main.py:setup",
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 3}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	})
	if err != nil {
		t.Fatal(err)
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
	caller, err := reg.FileCaller(ctx, run.ID, fileID, "main.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddResolvedEdges(ctx, run.ID, []model.Edge{{
		SrcID: caller, DstID: target, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{FilePath: "main.py", Range: model.Range{Start: model.Position{Line: 5}},
			Source: model.SourceSourceCode, Detail: "t"},
	}}); err != nil {
		t.Fatal(err)
	}

	g, st, err := loadExportGraph(ctx, &session{db: db, reg: reg, runID: run.ID}, model.ConfidenceLow)
	if err != nil {
		t.Fatal(err)
	}
	top := registry.FileCallerFQN("main.py")
	if !contains(g.Keys(), top) {
		t.Errorf("the top-level caller is not a node; keys = %v", g.Keys())
	}
	found := false
	for _, e := range g.Edges() {
		if e.From == top && e.To == "main.py:setup" && e.Kind == model.EdgeCalls {
			found = true
		}
	}
	if !found {
		t.Errorf("the top-level call is not an edge; edges = %+v", g.Edges())
	}
	if st.edgesDropped() != 0 {
		t.Errorf("edges dropped: %+v", st)
	}
}
