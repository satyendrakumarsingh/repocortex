package pipeline

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

const paymentPath = "src/main/java/com/pay/model/Payment.java"

// symbolNamed is the id of the one class of that name.
func symbolNamed(t *testing.T, reg *registry.Registry, runID int64, name string) int64 {
	t.Helper()
	hits, err := reg.FindSymbolsByName(context.Background(), runID, name, "class", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("symbol %s: %v, %v", name, hits, err)
	}
	return hits[0].ID
}

func compilerExtends(src, dst int64) model.Edge {
	return model.Edge{
		SrcID: src, DstID: dst, Kind: model.EdgeExtends, Confidence: model.ConfidenceExact,
		Evidence: model.Evidence{
			FilePath: paymentPath, Range: model.Range{Start: model.Position{Line: 3, Col: 20}},
			Source: model.SourceCompiler, Detail: "scip",
		},
	}
}

// TestHeuristicResolutionIgnoresAHierarchyOnlyACompilerKnows pins the order
// rule between the two kinds of resolution. A full index resolves heuristically
// first and imports the compiler's edges after, so the heuristic never sees an
// inheritance edge only a compiler found. An incremental update resolves with
// every compiler edge of the untouched files already in the table; if it read
// them, the changed file would bind a call through an ancestor the same file
// leaves unbound in a full index, and the two indexes of one tree would differ.
func TestHeuristicResolutionIgnoresAHierarchyOnlyACompilerKnows(t *testing.T) {
	ctx := context.Background()
	reg, runID := inheritanceFixture(t, t.TempDir(), "Payment")

	// The compiler has already linked Payment to BasePayment.
	if _, err := reg.UpgradeEdges(ctx, runID, []model.Edge{
		compilerExtends(symbolNamed(t, reg, runID, "Payment"), symbolNamed(t, reg, runID, "BasePayment")),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(ctx, reg, runID, ResolveOptions{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if e := findEdge(t, reg, runID, edges, model.EdgeCalls, "process", "settle"); e != nil {
		t.Errorf("the call bound through a compiler-only hierarchy: %+v", *e)
	}
	if !hasEdge(t, reg, runID, edges, model.EdgeExtends, "Payment", "BasePayment") {
		t.Error("the compiler's own edge was lost")
	}
}

// TestHeuristicResolutionKeepsAHierarchyACompilerConfirmed is the other half:
// an inheritance edge the heuristic bound and a compiler later confirmed is
// still the heuristic's own finding, so an update resolving with it in the
// table binds what a full index binds.
func TestHeuristicResolutionKeepsAHierarchyACompilerConfirmed(t *testing.T) {
	ctx := context.Background()
	reg, runID := inheritanceFixture(t, t.TempDir())

	if _, err := Resolve(ctx, reg, runID, ResolveOptions{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if findEdge(t, reg, runID, edges, model.EdgeCalls, "process", "settle") == nil {
		t.Fatal("a full index does not bind the inherited call; the fixture proves nothing")
	}

	// The compiler confirms the EXTENDS edge (upgrading it in place), then the
	// file holding the call changes: its references are pending again.
	if _, err := reg.UpgradeEdges(ctx, runID, []model.Edge{
		compilerExtends(symbolNamed(t, reg, runID, "Payment"), symbolNamed(t, reg, runID, "BasePayment")),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.ResetHeuristicResolutions(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(ctx, reg, runID, ResolveOptions{}); err != nil {
		t.Fatalf("incremental resolve: %v", err)
	}
	edges, err = reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if findEdge(t, reg, runID, edges, model.EdgeCalls, "process", "settle") == nil {
		t.Error("the inherited call is gone once the heuristic's own EXTENDS edge was confirmed by a compiler")
	}
}
