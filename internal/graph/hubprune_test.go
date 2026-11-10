package graph

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// newHubFixture builds root -> hub -> fan leaves.
//
// It returns the hub as well as the root because the two interesting cases are
// different symbols: reaching a hub mid-walk must stop the expansion, and
// asking about the hub itself must not.
func newHubFixture(tb testing.TB, fan int) (g *Graph, root, hub int64) {
	tb.Helper()
	root, hub = 1, 2
	edges := make([]tedge, 0, fan+1)
	edges = append(edges, e(root, hub))
	for i := 0; i < fan; i++ {
		edges = append(edges, e(hub, int64(1000+i)))
	}
	return build(tb, edges), root, hub
}

func TestHubPruningStopsExpansionThroughAHub(t *testing.T) {
	// root -> hub -> 400 leaves. Without pruning a depth-2 walk reaches them
	// all; with a hub limit of 50 it stops at the hub and says so.
	g, root, hub := newHubFixture(t, 400)
	ctx := context.Background()

	base := Query{
		Roots: []int64{root}, MaxDepth: 2, MaxNodes: 10_000,
		Direction: Outgoing, MinConfidence: model.ConfidenceLow,
	}

	unpruned, err := g.Traverse(ctx, base)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(unpruned.Nodes) < 400 {
		t.Fatalf("fixture wrong: unpruned walk found %d nodes, want >= 400", len(unpruned.Nodes))
	}

	q := base
	q.HubDegreeLimit = 50
	pruned, err := g.Traverse(ctx, q)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(pruned.Nodes) >= len(unpruned.Nodes) {
		t.Fatalf("pruning did nothing: %d vs %d nodes", len(pruned.Nodes), len(unpruned.Nodes))
	}
	if len(pruned.PrunedHubs) == 0 {
		t.Fatal("PrunedHubs empty: a walk that refused to expand must name where")
	}
	if got := pruned.PrunedHubs[0].SymbolID; got != hub {
		t.Errorf("pruned symbol = %d, want the hub %d", got, hub)
	}
	if got := pruned.PrunedHubs[0].Degree; got != 400 {
		t.Errorf("recorded degree = %d, want 400", got)
	}
	// The hub itself is genuinely reachable and must still be reported; only
	// its neighbours go unexplored.
	if _, ok := findNode(pruned, hub); !ok {
		t.Error("hub missing from Nodes: pruning must refuse expansion, not reachability")
	}
	if !pruned.Truncated {
		t.Error("Truncated = false after pruning: the answer is not the complete reachable set")
	}
}

func TestHubPruningTruncatedReasonNamesCountAndLimit(t *testing.T) {
	g, root, _ := newHubFixture(t, 400)
	got, err := g.Traverse(context.Background(), Query{
		Roots: []int64{root}, MaxDepth: 2, MaxNodes: 10_000,
		Direction: Outgoing, MinConfidence: model.ConfidenceLow,
		HubDegreeLimit: 50,
	})
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	reason := got.TruncatedReason
	for _, want := range []string{
		strconv.Itoa(len(got.PrunedHubs)), // how many were refused
		"50",                              // the limit that refused them
		"hub_degree_limit",                // the remedy
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("TruncatedReason %q does not mention %q", reason, want)
		}
	}
}

func TestHubPruningNeverPrunesASeed(t *testing.T) {
	// The root IS the hub. Pruning it would return nothing for the very symbol
	// the caller asked about.
	g, _, hub := newHubFixture(t, 400)
	got, err := g.Traverse(context.Background(), Query{
		Roots: []int64{hub}, MaxDepth: 1, MaxNodes: 10_000,
		Direction: Outgoing, MinConfidence: model.ConfidenceLow,
		HubDegreeLimit: 50,
	})
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(got.PrunedHubs) != 0 {
		t.Errorf("pruned %d node(s) including the seed; roots are exempt", len(got.PrunedHubs))
	}
	if len(got.Nodes) < 401 {
		t.Fatalf("got %d nodes, want the seed plus its 400 neighbours: "+
			"a caller asking about a hub must still get them", len(got.Nodes))
	}
}

func TestHubPruningOffByDefault(t *testing.T) {
	g, root, _ := newHubFixture(t, 400)

	tests := []struct {
		name string
		q    Query
	}{
		{"no hub fields set", Query{}},
		{"limit above the hub's degree", Query{HubDegreeLimit: 10_000}},
		{"zero percentile is not a percentile", Query{HubPercentile: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := tt.q
			q.Roots = []int64{root}
			q.MaxDepth, q.MaxNodes = 2, 10_000
			q.Direction, q.MinConfidence = Outgoing, model.ConfidenceLow

			got, err := g.Traverse(context.Background(), q)
			if err != nil {
				t.Fatalf("Traverse: %v", err)
			}
			if len(got.PrunedHubs) != 0 {
				t.Fatalf("pruning fired (%d hub(s)); it must be opt-in and above the limit",
					len(got.PrunedHubs))
			}
			if len(got.Nodes) < 400 {
				t.Fatalf("got %d nodes, want the full unpruned walk", len(got.Nodes))
			}
		})
	}
}

// TestHubPruningPercentileDerivesALimit pins that a percentile can prune when
// the distribution is wide enough to clear the floor.
func TestHubPruningPercentileDerivesALimit(t *testing.T) {
	// One symbol with 900 out-edges among many with one, so p99 sits far below
	// the hub but the floor (minHubDegree) still applies.
	edges := []tedge{e(1, 2)}
	for i := 0; i < 900; i++ {
		edges = append(edges, e(2, int64(1000+i)))
	}
	for i := 0; i < 200; i++ {
		edges = append(edges, e(int64(5000+i), int64(6000+i)))
	}
	g := build(t, edges)

	got, err := g.Traverse(context.Background(), Query{
		Roots: []int64{1}, MaxDepth: 2, MaxNodes: 10_000,
		Direction: Outgoing, MinConfidence: model.ConfidenceLow,
		HubPercentile: 0.99,
	})
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(got.PrunedHubs) != 1 || got.PrunedHubs[0].SymbolID != 2 {
		t.Fatalf("PrunedHubs = %+v, want just symbol 2", got.PrunedHubs)
	}
	if !got.Truncated {
		t.Error("Truncated = false after percentile pruning")
	}
}

// TestHubPruningIsDeterministic pins that repeated runs agree, including the
// order of PrunedHubs.
func TestHubPruningIsDeterministic(t *testing.T) {
	edges := []tedge{e(1, 2), e(1, 3)}
	for i := 0; i < 300; i++ {
		edges = append(edges, e(2, int64(1000+i)), e(3, int64(2000+i)))
	}
	g := build(t, edges)
	q := Query{
		Roots: []int64{1}, MaxDepth: 3, MaxNodes: 10_000,
		Direction: Outgoing, MinConfidence: model.ConfidenceLow,
		HubDegreeLimit: 50,
	}

	first, err := g.Traverse(context.Background(), q)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := g.Traverse(context.Background(), q)
		if err != nil {
			t.Fatalf("Traverse: %v", err)
		}
		if !equalIDs(ids(again.Nodes), ids(first.Nodes)) {
			t.Fatalf("run %d returned different nodes", i)
		}
		if len(again.PrunedHubs) != len(first.PrunedHubs) {
			t.Fatalf("run %d pruned %d hubs, first pruned %d",
				i, len(again.PrunedHubs), len(first.PrunedHubs))
		}
		for j := range again.PrunedHubs {
			if again.PrunedHubs[j] != first.PrunedHubs[j] {
				t.Fatalf("run %d PrunedHubs[%d] = %+v, want %+v",
					i, j, again.PrunedHubs[j], first.PrunedHubs[j])
			}
		}
	}
	if len(first.PrunedHubs) != 2 {
		t.Fatalf("PrunedHubs = %+v, want both hubs", first.PrunedHubs)
	}
}

// TestHubPruningPercentileFlooredOnNarrowGraphs pins minHubDegree: the widest
// node in a narrow graph is not a hub, and a percentile alone would say it was.
func TestHubPruningPercentileFlooredOnNarrowGraphs(t *testing.T) {
	// Out-degrees are 1 everywhere except one node with 5, so p99 is 5. Without
	// the floor that would prune the only branch point in the graph.
	edges := []tedge{e(1, 2)}
	for i := 0; i < 5; i++ {
		edges = append(edges, e(2, int64(1000+i)), e(int64(1000+i), int64(2000+i)))
	}
	g := build(t, edges)

	got, err := g.Traverse(context.Background(), Query{
		Roots: []int64{1}, MaxDepth: 2, MaxNodes: 10_000,
		Direction: Outgoing, MinConfidence: model.ConfidenceLow,
		HubPercentile: 0.99,
	})
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(got.PrunedHubs) != 0 {
		t.Fatalf("pruned %+v; a degree of 5 is under the floor of %d",
			got.PrunedHubs, minHubDegree)
	}
	if len(got.Nodes) != 7 {
		t.Fatalf("got %d nodes, want the full 2-hop walk (7)", len(got.Nodes))
	}
}

// TestHubPruningRejectsAnOutOfRangePercentile pins that 99 (meaning 99%) is an
// error, not a silent clamp.
func TestHubPruningRejectsAnOutOfRangePercentile(t *testing.T) {
	g, root, _ := newHubFixture(t, 400)
	_, err := g.Traverse(context.Background(), Query{
		Roots: []int64{root}, MaxDepth: 2, Direction: Outgoing,
		HubPercentile: 99,
	})
	if err == nil {
		t.Fatal("HubPercentile = 99 accepted; out-of-range must be an error")
	}
	if !strings.Contains(err.Error(), "percentile") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// TestHubPruningDepthMatchesNodeDepth pins that PrunedHub.Depth agrees with
// the SAME node's Node.Depth in Result.Nodes. A pruned hub is still reported
// as a Node (partitionHubs never withholds reachability, only expansion), so
// the two records describe the same discovery and a caller joining
// PrunedHubs to Nodes on depth must get a consistent answer.
//
// The fixture prunes at two different depths on purpose: a bug that shifts
// every recorded Depth by the same constant would still look self-consistent
// if only one depth were exercised.
func TestHubPruningDepthMatchesNodeDepth(t *testing.T) {
	root, mid, hubDepth1, hubDepth2 := int64(1), int64(2), int64(4), int64(3)
	edges := []tedge{
		e(root, mid),      // root(0) -> mid(1)
		e(mid, hubDepth2), // mid(1) -> hub(2)
		e(root, hubDepth1),
	}
	for i := 0; i < 900; i++ {
		edges = append(edges, e(hubDepth2, int64(3000+i)))
	}
	for i := 0; i < 900; i++ {
		edges = append(edges, e(hubDepth1, int64(4000+i)))
	}
	g := build(t, edges)

	got, err := g.Traverse(context.Background(), Query{
		Roots: []int64{root}, MaxDepth: 3, MaxNodes: 10_000,
		Direction: Outgoing, MinConfidence: model.ConfidenceLow,
		HubDegreeLimit: 50,
	})
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(got.PrunedHubs) != 2 {
		t.Fatalf("PrunedHubs = %+v, want both hubs pruned", got.PrunedHubs)
	}

	for _, ph := range got.PrunedHubs {
		n, ok := findNode(got, ph.SymbolID)
		if !ok {
			t.Fatalf("pruned symbol %d missing from Nodes: pruning must refuse expansion, not reachability", ph.SymbolID)
		}
		if ph.Depth != n.Depth {
			t.Errorf("PrunedHub{SymbolID:%d}.Depth = %d, but Nodes has Node{SymbolID:%d}.Depth = %d; "+
				"a caller cross-referencing the two must see the same depth",
				ph.SymbolID, ph.Depth, n.SymbolID, n.Depth)
		}
	}
}

func findNode(r Result, id int64) (Node, bool) {
	for _, n := range r.Nodes {
		if n.SymbolID == id {
			return n, true
		}
	}
	return Node{}, false
}
