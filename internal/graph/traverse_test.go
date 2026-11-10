package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestTraverseLinearChain(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(2, 3), e(3, 4), e(4, 5)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 2})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("nodes = %v, want [1 2 3]", got)
	}
	for _, n := range res.Nodes {
		if want := int(n.SymbolID - 1); n.Depth != want {
			t.Errorf("symbol %d depth = %d, want %d", n.SymbolID, n.Depth, want)
		}
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true: node 3 was never expanded and node 4 is missing")
	}
}

// TestTraverseCycleTerminates is the test that would hang or exhaust memory
// under a naive implementation. Mutual recursion is normal in real code.
func TestTraverseCycleTerminates(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(2, 3), e(3, 1)})

	res, err := g.Traverse(context.Background(), Query{
		Roots: []int64{1}, MaxDepth: 50, MaxNodes: 100,
	})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("nodes = %v, want [1 2 3] exactly once each", got)
	}
	if res.Truncated {
		t.Errorf("Truncated = true (%s), want false: the cycle was fully explored", res.TruncatedReason)
	}
}

// TestTraverseDiamondDoesNotDoubleCount guards the property impact analysis
// depends on: a shared dependency reached by two routes is one affected symbol,
// not two.
func TestTraverseDiamondDoesNotDoubleCount(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(1, 3), e(2, 4), e(3, 4), e(4, 5)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 3})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("nodes = %v, want [1 2 3 4 5]", got)
	}
	if d := nodeByID(t, res, 4).Depth; d != 2 {
		t.Errorf("symbol 4 depth = %d, want 2 (shallowest of two equal routes)", d)
	}
}

func TestTraverseSelfEdge(t *testing.T) {
	g := build(t, []tedge{e(1, 1), e(1, 2)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 5})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2}) {
		t.Fatalf("nodes = %v, want [1 2]: the self-edge must not re-emit the root", got)
	}
	if d := nodeByID(t, res, 1).Depth; d != 0 {
		t.Errorf("root depth = %d, want 0", d)
	}
}

func TestTraverseDisconnectedComponent(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(2, 3), e(10, 11), e(11, 12)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 10})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("nodes = %v, want [1 2 3]: the second component is unreachable", got)
	}
}

func TestTraverseUnknownRoot(t *testing.T) {
	g := build(t, []tedge{e(1, 2)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{999}})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{999}) {
		t.Fatalf("nodes = %v, want [999]: an unknown root is simply isolated", got)
	}
}

func TestTraverseNoRoots(t *testing.T) {
	g := build(t, []tedge{e(1, 2)})

	res, err := g.Traverse(context.Background(), Query{})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if len(res.Nodes) != 0 || res.Truncated {
		t.Fatalf("got %+v, want an empty untruncated result", res)
	}
}

func TestTraverseMaxNodesTruncates(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(1, 3), e(1, 4), e(1, 5), e(1, 6)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxNodes: 3})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if len(res.Nodes) != 3 {
		t.Fatalf("got %d nodes, want exactly 3", len(res.Nodes))
	}
	if !res.Truncated {
		t.Fatal("Truncated = false: reporting a capped result as complete would under-report impact")
	}
	if !strings.Contains(res.TruncatedReason, "node limit 3") {
		t.Errorf("reason = %q, want it to name the node limit", res.TruncatedReason)
	}
}

func TestTraverseMaxNodesBelowRootCount(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(3, 4)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1, 3}, MaxNodes: 1})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if len(res.Nodes) != 1 || !res.Truncated {
		t.Fatalf("got %+v, want 1 node and Truncated", res)
	}
	if !strings.Contains(res.TruncatedReason, "root") {
		t.Errorf("reason = %q, want it to say roots were dropped", res.TruncatedReason)
	}
}

func TestTraverseMaxDepthTruncationIsHonest(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(2, 3)})

	// Depth 1 leaves node 2 unexpanded, so node 3 is missing.
	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 1, MaxNodes: 100})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if !res.Truncated || !strings.Contains(res.TruncatedReason, "depth limit 1") {
		t.Fatalf("got %+v, want depth-limit truncation", res)
	}

	// Depth 5 exhausts the component, so nothing is missing.
	res, err = g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 5, MaxNodes: 100})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if res.Truncated {
		t.Fatalf("Truncated = true (%s), want false: the whole component fits", res.TruncatedReason)
	}
}

func TestTraverseDirection(t *testing.T) {
	// 1 -> 2 -> 3, plus 4 -> 2.
	edges := []tedge{e(1, 2), e(2, 3), e(4, 2)}

	cases := []struct {
		name string
		dir  Direction
		want []int64
	}{
		// Results are ordered (Depth, SymbolID), so the root leads regardless of
		// how its id compares to its neighbours'.
		{"outgoing", Outgoing, []int64{1, 2, 3}},
		{"incoming", Incoming, []int64{2, 1, 4}},
		{"both", Both, []int64{2, 1, 3, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := build(t, edges)
			root := int64(1)
			if tc.dir != Outgoing {
				root = 2
			}
			res, err := g.Traverse(context.Background(), Query{
				Roots: []int64{root}, MaxDepth: 5, MaxNodes: 100, Direction: tc.dir,
			})
			if err != nil {
				t.Fatalf("traverse: %v", err)
			}
			if got := ids(res.Nodes); !equalIDs(got, tc.want) {
				t.Fatalf("nodes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTraverseInvalidDirection(t *testing.T) {
	g := build(t, []tedge{e(1, 2)})
	if _, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, Direction: Direction(9)}); err == nil {
		t.Fatal("want an error for an out-of-range direction")
	}
}

func TestTraverseKindFilter(t *testing.T) {
	g := build(t, []tedge{
		e(1, 2),
		e(1, 3).withKind(model.EdgeImports),
		e(1, 4).withKind(model.EdgeReferences),
	})

	res, err := g.Traverse(context.Background(), Query{
		Roots: []int64{1}, Kinds: []model.EdgeKind{model.EdgeCalls, model.EdgeImports}, MaxNodes: 100,
	})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("nodes = %v, want [1 2 3]: REFERENCES was not requested", got)
	}
}

func TestTraverseMinConfidenceExcludesWeakEdges(t *testing.T) {
	g := build(t, []tedge{
		e(1, 2).withConf(model.ConfidenceExact),
		e(1, 3).withConf(model.ConfidenceMedium),
		e(1, 4).withConf(model.ConfidenceLow),
	})

	res, err := g.Traverse(context.Background(), Query{
		Roots: []int64{1}, MinConfidence: model.ConfidenceMedium, MaxNodes: 100,
	})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("nodes = %v, want [1 2 3]: the low edge must not be expanded", got)
	}
}

// TestTraverseConfidenceIsMinimumAlongPath is the product promise: reachability
// through a guess is reported as a guess, however strong the later hops are.
func TestTraverseConfidenceIsMinimumAlongPath(t *testing.T) {
	g := build(t, []tedge{
		e(1, 2).withConf(model.ConfidenceExact),
		e(2, 3).withConf(model.ConfidenceLow),
		e(3, 4).withConf(model.ConfidenceExact),
	})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 3, MaxNodes: 100})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	want := map[int64]model.Confidence{
		1: model.ConfidenceExact,
		2: model.ConfidenceExact,
		3: model.ConfidenceLow,
		4: model.ConfidenceLow,
	}
	for id, wantConf := range want {
		if got := nodeByID(t, res, id).Confidence; got != wantConf {
			t.Errorf("symbol %d confidence = %q, want %q", id, got, wantConf)
		}
	}
}

// TestTraverseConfidencePrefersStrongerRouteAtSameDepth: given two routes of
// equal length, the node should report the stronger of the two path minima.
func TestTraverseConfidencePrefersStrongerRouteAtSameDepth(t *testing.T) {
	g := build(t, []tedge{
		e(1, 2).withConf(model.ConfidenceExact),
		e(1, 3).withConf(model.ConfidenceLow),
		e(2, 4).withConf(model.ConfidenceExact),
		e(3, 4).withConf(model.ConfidenceExact),
	})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 3, MaxNodes: 100})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := nodeByID(t, res, 4).Confidence; got != model.ConfidenceExact {
		t.Fatalf("symbol 4 confidence = %q, want exact via 1->2->4", got)
	}
}

// TestTraverseCostOrdersLongerProvenPathBeforeShorterGuess is the end-to-end
// counterpart to TestTwoLowEdgesCostMoreThanThreeExactEdges (cost_test.go):
// that test proves the property for PathCost in isolation; this one proves it
// through Traverse and Result.finish(), on the caller-facing Nodes order.
//
// Branch A reaches leaf 3 in 2 LOW hops (cost 13+13=26). Branch B reaches leaf
// 6 in 3 EXACT hops (cost 1+1+1=3). Under plain unweighted BFS depth 2 would
// sort before depth 3, putting the guessed leaf 3 ahead of the proven leaf 6.
// With cost as the primary sort key, the longer proven chain must win.
func TestTraverseCostOrdersLongerProvenPathBeforeShorterGuess(t *testing.T) {
	g := build(t, []tedge{
		e(1, 2).withConf(model.ConfidenceLow),
		e(2, 3).withConf(model.ConfidenceLow),
		e(1, 4).withConf(model.ConfidenceExact),
		e(4, 5).withConf(model.ConfidenceExact),
		e(5, 6).withConf(model.ConfidenceExact),
	})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxDepth: 3, MaxNodes: 100})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}

	got := ids(res.Nodes)
	lowIdx, exactIdx := -1, -1
	for i, id := range got {
		switch id {
		case 3:
			lowIdx = i
		case 6:
			exactIdx = i
		}
	}
	if lowIdx == -1 || exactIdx == -1 {
		t.Fatalf("nodes = %v, want both leaf 3 and leaf 6 present", got)
	}
	if exactIdx >= lowIdx {
		t.Fatalf("nodes = %v: depth-3 exact leaf (symbol 6, index %d) must sort before depth-2 low leaf (symbol 3, index %d)", got, exactIdx, lowIdx)
	}
}

func TestTraverseVia(t *testing.T) {
	g := build(t, []tedge{e(1, 2).withKind(model.EdgeImplements)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxNodes: 100})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := nodeByID(t, res, 2).Via; got != model.EdgeImplements {
		t.Errorf("Via = %q, want IMPLEMENTS", got)
	}
	if got := nodeByID(t, res, 1).Via; got != "" {
		t.Errorf("root Via = %q, want empty", got)
	}
}

// TestTraverseChunksLargeFrontier crosses the maxSQLVars boundary. A frontier
// larger than SQLite's variable limit is the failure mode a small fixture will
// never surface and a real repository hits immediately.
func TestTraverseChunksLargeFrontier(t *testing.T) {
	const n = maxSQLVars*2 + 37

	edges := make([]tedge, 0, n)
	roots := make([]int64, 0, n)
	for i := int64(1); i <= n; i++ {
		roots = append(roots, i)
		edges = append(edges, e(i, i+n))
	}

	g := build(t, edges)
	res, err := g.Traverse(context.Background(), Query{
		Roots: roots, MaxDepth: 1, MaxNodes: 10 * n,
	})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if len(res.Nodes) != 2*n {
		t.Fatalf("got %d nodes, want %d: a chunk boundary dropped rows", len(res.Nodes), 2*n)
	}
}

// TestTraverseCancellationMidWalk uses the afterLevel seam so the cancellation
// lands between levels deterministically, instead of racing a timer.
func TestTraverseCancellationMidWalk(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(2, 3), e(3, 4), e(4, 5)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.afterLevel = func(depth int) {
		if depth == 2 {
			cancel()
		}
	}

	_, err := g.Traverse(ctx, Query{Roots: []int64{1}, MaxDepth: 10, MaxNodes: 100})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTraverseCancelledUpFront(t *testing.T) {
	g := build(t, []tedge{e(1, 2)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := g.Traverse(ctx, Query{Roots: []int64{1}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTraverseWithoutCommittedRun(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	_, err = New(db).Traverse(context.Background(), Query{Roots: []int64{1}})
	if !errors.Is(err, registry.ErrNoCommittedRun) {
		t.Fatalf("err = %v, want registry.ErrNoCommittedRun", err)
	}
}

// TestTraverseIgnoresUncommittedRun proves invariant 3 holds on the query path:
// a staging run's edges are invisible even though they are physically present
// and point at symbols the traversal can see.
func TestTraverseIgnoresUncommittedRun(t *testing.T) {
	g := build(t, []tedge{e(1, 2)}, 3)

	staging := seedRun(t, g.db, false, nil)
	insertEdges(t, g.db, staging, []tedge{e(1, 3)})

	res, err := g.Traverse(context.Background(), Query{Roots: []int64{1}, MaxNodes: 100})
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	if got := ids(res.Nodes); !equalIDs(got, []int64{1, 2}) {
		t.Fatalf("nodes = %v, want [1 2]: the staging run's edge must be invisible", got)
	}
}

// TestTraverseSameDepthPrefersCheaperRoute pins the tie-break that ranking by
// cost made load-bearing.
//
// Two routes reach X at the same depth, with the same path confidence (medium)
// and the same edge kind (CALLS), so neither confidence nor kind can separate
// them — but one costs 6 (exact then medium) and the other 10 (medium then
// medium). Because Result.finish ranks by cost first, whichever route is
// recorded decides where X lands in the caller-facing order, and MaxItems
// truncation downstream cuts in that order. If the choice were left to
// whichever row SQLite returned first, re-indexing identical source could
// change which symbols an agent is shown.
//
// The two subtests are the same graph with the parent ids swapped, which
// swaps the order the level scan returns the two hops in. The cheaper route
// must win both times.
func TestTraverseSameDepthPrefersCheaperRoute(t *testing.T) {
	const (
		root = int64(1)
		x    = int64(9)
	)
	tests := []struct {
		name            string
		cheapP, costlyP int64
	}{
		{name: "cheap route scanned first", cheapP: 2, costlyP: 3},
		{name: "cheap route scanned second", cheapP: 3, costlyP: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := build(t, []tedge{
				e(root, tt.cheapP).withConf(model.ConfidenceExact),
				e(tt.cheapP, x).withConf(model.ConfidenceMedium),
				e(root, tt.costlyP).withConf(model.ConfidenceMedium),
				e(tt.costlyP, x).withConf(model.ConfidenceMedium),
			})

			res, err := g.Traverse(context.Background(), Query{
				Roots: []int64{root}, MaxDepth: 3, MaxNodes: 100,
			})
			if err != nil {
				t.Fatalf("traverse: %v", err)
			}
			n := nodeByID(t, res, x)
			if n.Confidence != model.ConfidenceMedium {
				t.Fatalf("symbol %d confidence = %q, want medium on both routes", x, n.Confidence)
			}
			wantCost := EdgeCost(model.ConfidenceExact) + EdgeCost(model.ConfidenceMedium)
			if n.cost != wantCost {
				t.Fatalf("symbol %d cost = %d, want %d: the cheaper same-depth route must win regardless of row arrival order",
					x, n.cost, wantCost)
			}
		})
	}
}
