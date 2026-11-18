package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// explicitImplTree is `Impl : I` with `void I.M()`, an unqualified `M()` in
// Impl, and a test that holds an `I`.
func explicitImplTree() map[string]string {
	return map[string]string{
		"a.cs": `interface I {
    void M();
}
class Impl : I {
    void I.M() {}
    void T() {
        M();
    }
}
class Other : I {
    public void M() {}
}
`,
		"Tests/ImplTests.cs": `class ImplTests {
    void TestM(I i) {
        i.M();
    }
}
`,
	}
}

// TestExplicitInterfaceImplementationKeepsItsDispatchLink checks that `void
// I.M()` is indexed under the name `I.M` (no unqualified call reaches it) and is still
// recorded as implementing `I.M`, with a method-level IMPLEMENTS edge as Go's
// are, so impact and find_tests see it.
func TestExplicitInterfaceImplementationKeepsItsDispatchLink(t *testing.T) {
	got := langEdges(t, explicitImplTree())
	requireEdge(t, got, "a.cs:Impl.M", "a.cs:I.M", model.ConfidenceLow)
	forbidEdge(t, got, "a.cs:Impl.T", "a.cs:Impl.M")
	forbidEdge(t, got, "Tests/ImplTests.cs:ImplTests.TestM", "a.cs:Impl.M")
	// An implicit implementation is not given this edge: it pairs by name.
	forbidEdge(t, got, "a.cs:Other.M", "a.cs:I.M")
}

// TestExplicitInterfaceImplementationReachesImpactAndTests runs the two
// capabilities over a real index: impact from `I.M` reaches the explicit
// implementation, and find_tests for it finds the test that calls through `I`.
func TestExplicitInterfaceImplementationReachesImpactAndTests(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, explicitImplTree())
	dbPath := filepath.Join(t.TempDir(), "index.db")
	reg, runID, _ := indexTree(t, root, dbPath)
	db, err := store.Open(ctx, store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	deps := capability.NewDeps(reg, graph.New(db))

	rep, err := impact.Analyze(ctx, deps, runID, "a.cs:I.M")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	found := false
	for _, d := range rep.Dispatch {
		if d.FQN == "a.cs:Impl.M" {
			found = true
		}
	}
	if !found {
		t.Fatalf("impact from I.M did not reach the explicit implementation; dispatch=%v", dispatchFQNs(rep.Dispatch))
	}

	tr, err := tests.FindTests(ctx, deps, runID, "a.cs:Impl.M")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	found = false
	for _, h := range tr.Tests {
		if h.FQN == "Tests/ImplTests.cs:ImplTests.TestM" {
			found = true
		}
	}
	if !found {
		t.Fatalf("find_tests for the explicit implementation missed the test that calls through I: %+v", tr.Tests)
	}
}

func dispatchFQNs(in []impact.Dispatched) []string {
	out := make([]string, len(in))
	for i, d := range in {
		out[i] = d.FQN
	}
	return out
}
