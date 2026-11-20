package diffgraph

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// addTopLevelCall gives a run a synthetic top-level caller for path with one
// call edge to the symbol fqn.
func addTopLevelCall(b *runBuilder, path, fqn string, line int) {
	top := b.src.top[b.runID]
	callerFQN := path + ":(top level of " + path + ")"
	id := b.id()
	top.Callers = append(top.Callers, model.Symbol{
		ID: id, FileID: b.fileID[path], Kind: model.KindFile,
		Name: "(top level of " + path + ")", FQN: callerFQN,
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 1}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	})
	top.Edges = append(top.Edges, model.Edge{
		ID: b.id(), SrcID: id, DstID: b.symID[fqn], Kind: model.EdgeCalls,
		Confidence: model.ConfidenceHigh, IndexRunID: b.runID,
		Evidence: model.Evidence{
			FilePath: path, Range: model.Range{Start: model.Position{Line: line, Col: 1}},
			Source: model.SourceSourceCode,
		},
	})
	b.src.top[b.runID] = top
}

// TestTopLevelCallIsAnEdgeChangeNotASymbolChange: a call added at a file's top
// level is reported as an added edge, and the synthetic caller is never
// reported as an added symbol.
func TestTopLevelCallIsAnEdgeChangeNotASymbolChange(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)
	head := baseline(src, 2, 5_000, 0)
	addTopLevelCall(head, "pkg/api.go", "pkg/service.go:Refund", 3)

	rep := mustDiff(t, src, 1, 2)
	if len(rep.Symbols) != 0 {
		t.Errorf("symbol changes = %v, want none: the top-level caller is not a definition", rep.Symbols)
	}
	if len(rep.Edges) != 1 {
		t.Fatalf("edge changes = %d, want the one top-level call: %+v", len(rep.Edges), rep.Edges)
	}
	ch := rep.Edges[0]
	if ch.Change != EdgeAdded || ch.Key.Kind != model.EdgeCalls || ch.Key.Src.Kind != model.KindFile {
		t.Errorf("edge change = %+v, want a CALLS edge added from the file's top level", ch)
	}
	if ch.Key.Dst.FQN != "pkg/service.go:Refund" {
		t.Errorf("edge target = %s", ch.Key.Dst.FQN)
	}

	back := mustDiff(t, src, 2, 1)
	if len(back.Edges) != 1 || back.Edges[0].Change != EdgeRemoved {
		t.Errorf("reverse diff = %+v, want one removed edge", back.Edges)
	}

	// The same top-level call on both sides, in disjoint id spaces, is no change.
	src2 := newFakeSource()
	a := baseline(src2, 1, 0, 0)
	b := baseline(src2, 2, 7_000, 0)
	addTopLevelCall(a, "pkg/api.go", "pkg/service.go:Refund", 3)
	addTopLevelCall(b, "pkg/api.go", "pkg/service.go:Refund", 3)
	requireEmpty(t, mustDiff(t, src2, 1, 2))
}
