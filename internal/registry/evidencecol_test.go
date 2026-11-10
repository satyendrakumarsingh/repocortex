package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestIncomingEdgesCarryTheEvidenceColumn: every edge insert writes
// evidence_col (writer.go AddEdge); a reader that needs it as a hint must be
// able to get it back.
func TestIncomingEdgesCarryTheEvidenceColumn(t *testing.T) {
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
	fid, err := w.AddFile(ctx, &model.File{Path: "a.go", Language: "go", ContentHash: "h", ParseStatus: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	sym := func(name string, line int) int64 {
		id, err := w.AddSymbol(ctx, &model.Symbol{
			FileID: fid, Kind: model.KindFunction, Name: name, FQN: "a." + name,
			Range:      model.Range{Start: model.Position{Line: line}, End: model.Position{Line: line + 1}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	callee, caller := sym("Save", 1), sym("Run", 4)
	if _, err := w.AddEdge(ctx, &model.Edge{
		SrcID: caller, DstID: callee, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{
			FilePath: "a.go",
			Range:    model.Range{Start: model.Position{Line: 5, Col: 7}},
			Source:   model.SourceSourceCode,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	hits, err := r.IncomingEdges(ctx, run.ID, callee, 10, EdgeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].EvidenceLine != 5 || hits[0].EvidenceCol != 7 {
		t.Fatalf("got %+v, want one edge at line 5 col 7", hits)
	}
}
