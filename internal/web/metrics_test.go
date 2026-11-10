package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// twoSymbolsOneFQN is the case that matters: cortex export found 1,178
// symbols sharing a fully-qualified name on the apm fixture. A projection that
// keys on FQN alone drops one of them AND re-parents its edges onto the
// survivor, which invents a relationship that does not exist.
func TestMetricsGraphKeepsBothSymbolsThatShareAnFQN(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{
			1: {Path: "a/one.go"},
			2: {Path: "b/two.go"},
		},
		syms: map[int64]symInfo{
			10: {Name: "Handle", FQN: "Handle", Kind: model.KindMethod, FileID: 1, Line: 4, Confidence: model.ConfidenceHigh},
			11: {Name: "Handle", FQN: "Handle", Kind: model.KindMethod, FileID: 2, Line: 9, Confidence: model.ConfidenceHigh},
			12: {Name: "Caller", FQN: "Caller", Kind: model.KindFunction, FileID: 1, Line: 1, Confidence: model.ConfidenceHigh},
		},
		edges: []compactEdge{
			{Src: 12, Dst: 10, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		},
	}

	g, nodes, dropped, err := idx.metricsGraph(context.Background(), LevelSymbol, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("metricsGraph: %v", err)
	}
	if got := g.Order(); got != 3 {
		t.Fatalf("Order() = %d, want 3 — a shared FQN must not collapse two symbols", got)
	}
	if dropped.BelowFloor != 0 || dropped.IntraGroup != 0 {
		t.Errorf("dropped = %+v, want nothing dropped", dropped)
	}
	seen := map[string]bool{}
	for k, n := range nodes {
		if n.SymbolID == 10 || n.SymbolID == 11 {
			seen[k] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("the two Handle symbols share %d key(s), want 2 distinct: %v", len(seen), seen)
	}
}

func TestMetricsGraphDropsEdgesBelowTheFloorAndCountsThem(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{1: {Path: "a/one.go"}},
		syms: map[int64]symInfo{
			10: {Name: "A", FQN: "a/one.go:A", Kind: model.KindFunction, FileID: 1, Confidence: model.ConfidenceHigh},
			11: {Name: "B", FQN: "a/one.go:B", Kind: model.KindFunction, FileID: 1, Confidence: model.ConfidenceHigh},
		},
		edges: []compactEdge{
			{Src: 10, Dst: 11, Kind: model.EdgeCalls, Confidence: model.ConfidenceLow},
		},
	}
	g, _, dropped, err := idx.metricsGraph(context.Background(), LevelSymbol, model.ConfidenceHigh)
	if err != nil {
		t.Fatalf("metricsGraph: %v", err)
	}
	if len(g.Edges()) != 0 {
		t.Errorf("a low edge survived a high floor: %v", g.Edges())
	}
	if dropped.BelowFloor != 1 {
		t.Errorf("dropped by the floor = %d, want 1 — what the floor hides must be counted", dropped.BelowFloor)
	}
}

// TestMetricsGraphAppliesTheFloorToSymbolsBeforeCountingCollisions is the
// divergence a reviewer measured against export: the floor was applied to edges
// only, so a below-floor symbol still inflated its base key's collision count
// and its surviving twin got a disambiguated key export never mints.
func TestMetricsGraphAppliesTheFloorToSymbolsBeforeCountingCollisions(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{1: {Path: "a/one.go"}, 2: {Path: "b/two.go"}},
		syms: map[int64]symInfo{
			10: {Name: "Floor", FQN: "Floor", Kind: model.KindMethod, FileID: 1, Line: 5, Confidence: model.ConfidenceHigh},
			11: {Name: "Floor", FQN: "Floor", Kind: model.KindMethod, FileID: 2, Line: 9, Confidence: model.ConfidenceLow},
			12: {Name: "Caller", FQN: "Caller", Kind: model.KindFunction, FileID: 1, Line: 1, Confidence: model.ConfidenceHigh},
		},
		edges: []compactEdge{
			// High-confidence edge into a below-floor symbol: it goes with the
			// symbol, and the floor is what removed it, so it is counted.
			{Src: 12, Dst: 11, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		},
	}

	g, lookup, dropped, err := idx.metricsGraph(context.Background(), LevelSymbol, model.ConfidenceHigh)
	if err != nil {
		t.Fatalf("metricsGraph: %v", err)
	}
	want := []string{"Caller", "Floor"}
	got := g.Keys()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("keys = %q, want %q — a below-floor symbol must neither be a node nor disambiguate one", got, want)
	}
	if _, ok := lookup["Floor"]; !ok {
		t.Errorf("lookup has no bare %q: %v", "Floor", keysOf(lookup))
	}
	if dropped.BelowFloor != 1 {
		t.Errorf("dropped by the floor = %d, want 1 — an edge orphaned by the floor was removed by the floor", dropped.BelowFloor)
	}
}

func TestMetricsGraphFileLevelCollapsesSymbolsToTheirFile(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{1: {Path: "a/one.go"}, 2: {Path: "b/two.go"}},
		syms: map[int64]symInfo{
			10: {Name: "A", FQN: "a/one.go:A", Kind: model.KindFunction, FileID: 1, Confidence: model.ConfidenceHigh},
			11: {Name: "B", FQN: "a/one.go:B", Kind: model.KindFunction, FileID: 1, Confidence: model.ConfidenceHigh},
			12: {Name: "C", FQN: "b/two.go:C", Kind: model.KindFunction, FileID: 2, Confidence: model.ConfidenceHigh},
		},
		edges: []compactEdge{
			{Src: 10, Dst: 12, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{Src: 11, Dst: 12, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		},
	}
	g, nodes, _, err := idx.metricsGraph(context.Background(), LevelFile, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("metricsGraph: %v", err)
	}
	if got := g.Order(); got != 2 {
		t.Fatalf("Order() = %d, want 2 files", got)
	}
	if _, ok := nodes["a/one.go"]; !ok {
		t.Errorf("file node keyed on something other than its path: %v", keysOf(nodes))
	}
	// Two symbol edges between the same file pair are one file edge, weighted 2.
	es := g.Edges()
	if len(es) != 1 || es[0].Weight != 2 {
		t.Fatalf("file edges = %+v, want one edge of weight 2", es)
	}
}

// TestMetricsGraphAggregatedEdgeCarriesTheWeakestConfidence: the aggregated
// edge used to be stamped medium unconditionally, so a bundle of forty proven
// edges and a bundle of one guess both claimed the same tier on a field the
// metrics package filters by. aggregatedEdge already settled the rule — a
// bundle is no stronger than its worst member.
func TestMetricsGraphAggregatedEdgeCarriesTheWeakestConfidence(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{
			1: {Path: "a/one.go"}, 2: {Path: "b/two.go"}, 3: {Path: "c/three.go"},
		},
		syms: map[int64]symInfo{
			10: {Name: "A", FQN: "a/one.go:A", Kind: model.KindFunction, FileID: 1, Confidence: model.ConfidenceHigh},
			11: {Name: "B", FQN: "b/two.go:B", Kind: model.KindFunction, FileID: 2, Confidence: model.ConfidenceHigh},
			12: {Name: "C", FQN: "c/three.go:C", Kind: model.KindFunction, FileID: 3, Confidence: model.ConfidenceHigh},
		},
		edges: []compactEdge{
			// a -> b: proven twice over, nothing weaker in the bundle.
			{Src: 10, Dst: 11, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
			{Src: 10, Dst: 11, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
			// a -> c: one exact edge and one guess. The bundle is a guess.
			{Src: 10, Dst: 12, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
			{Src: 10, Dst: 12, Kind: model.EdgeCalls, Confidence: model.ConfidenceLow},
		},
	}
	g, _, _, err := idx.metricsGraph(context.Background(), LevelFile, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("metricsGraph: %v", err)
	}
	got := map[string]model.Confidence{}
	for _, e := range g.Edges() {
		got[e.From+" -> "+e.To] = e.Confidence
	}
	if c := got["a/one.go -> b/two.go"]; c != model.ConfidenceExact {
		t.Errorf("all-exact bundle = %q, want %q — the tier must not be invented", c, model.ConfidenceExact)
	}
	if c := got["a/one.go -> c/three.go"]; c != model.ConfidenceLow {
		t.Errorf("mixed bundle = %q, want %q — a bundle is no stronger than its worst member", c, model.ConfidenceLow)
	}
}

// TestMetricsGraphDirectoryLevelNamesTheRootAsTheExplorerDoes: path.Dir calls
// the root ".", containerFor calls it "(repository root)", and a reader should
// not have to work out that two screens mean the same directory.
func TestMetricsGraphDirectoryLevelNamesTheRootAsTheExplorerDoes(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{1: {Path: "main.go"}, 2: {Path: "b/two.go"}},
		syms: map[int64]symInfo{
			10: {Name: "A", FQN: "main.go:A", Kind: model.KindFunction, FileID: 1, Confidence: model.ConfidenceHigh},
			11: {Name: "B", FQN: "b/two.go:B", Kind: model.KindFunction, FileID: 2, Confidence: model.ConfidenceHigh},
		},
	}
	_, nodes, _, err := idx.metricsGraph(context.Background(), LevelDirectory, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("metricsGraph: %v", err)
	}
	if _, ok := nodes[repositoryRootLabel]; !ok {
		t.Errorf("root directory keyed as %v, want %q", keysOf(nodes), repositoryRootLabel)
	}
	if _, ok := nodes["."]; ok {
		t.Errorf("root directory still keyed %q, which no other screen calls it", ".")
	}
}

func TestMetricsGraphHonoursACancelledContext(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{1: {Path: "a/one.go"}},
		syms: map[int64]symInfo{
			10: {Name: "A", FQN: "a/one.go:A", Kind: model.KindFunction, FileID: 1, Confidence: model.ConfidenceHigh},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, level := range []MetricsLevel{LevelSymbol, LevelFile, LevelDirectory} {
		if _, _, _, err := idx.metricsGraph(ctx, level, model.ConfidenceLow); err == nil {
			t.Errorf("level %s: cancelled context returned no error", level)
		}
	}
}

func keysOf(m map[string]metricsNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMetricsGraphIsDeterministic asserts on what Graph.Keys() cannot show.
//
// Keys() returns a sorted copy, so comparing it across runs can only detect a
// change in the key SET — it would pass over an implementation that assigned
// keys entirely in map order. So this test asserts on the two things map order
// could actually leak into: WHICH SYMBOL got which key (the lookup, which is
// not sorted by anything), and the edge sequence.
//
// The fixture forces the residual "#n" path — several symbols identical in FQN,
// path AND line, which the index genuinely cannot tell apart — because that is
// the one branch where the choice between two candidates is made by iteration
// order rather than by the symbols' own contents.
func TestMetricsGraphIsDeterministic(t *testing.T) {
	idx := &structuralIndex{
		files: map[int64]fileInfo{1: {Path: "a/one.go"}, 2: {Path: "b/two.go"}},
		syms:  map[int64]symInfo{},
		edges: []compactEdge{},
	}
	// Four indistinguishable groups of five, plus a caller per group, so both
	// the "#n" assignment and the edges that hang off it are under test.
	id := int64(10)
	for group := 0; group < 4; group++ {
		caller := id
		idx.syms[caller] = symInfo{
			Name: fmt.Sprintf("Caller%d", group), FQN: fmt.Sprintf("Caller%d", group),
			Kind: model.KindFunction, FileID: 2, Line: group, Confidence: model.ConfidenceHigh,
		}
		id++
		for i := 0; i < 5; i++ {
			idx.syms[id] = symInfo{
				Name: "S", FQN: "a/one.go:S", Kind: model.KindFunction,
				FileID: 1, Line: 7, Confidence: model.ConfidenceHigh,
			}
			idx.edges = append(idx.edges, compactEdge{
				Src: caller, Dst: id, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
			})
			id++
		}
	}

	first, lookup, _, err := idx.metricsGraph(context.Background(), LevelSymbol, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("metricsGraph: %v", err)
	}
	// The fixture must actually reach the branch under test, or the assertions
	// below are about nothing.
	residual := 0
	for k := range lookup {
		if len(k) > 2 && k[len(k)-2] == '#' {
			residual++
		}
	}
	if residual == 0 {
		t.Fatalf("fixture never mints an ordinal key; the tiebreak is untested: %v", keysOf(lookup))
	}

	wantKeyOf := keyBySymbolID(lookup)
	wantEdges := fmt.Sprintf("%+v", first.Edges())
	for run := 0; run < 20; run++ {
		again, lookupAgain, _, err := idx.metricsGraph(context.Background(), LevelSymbol, model.ConfidenceLow)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		gotKeyOf := keyBySymbolID(lookupAgain)
		if len(gotKeyOf) != len(wantKeyOf) {
			t.Fatalf("run %d: %d nodes, want %d", run, len(gotKeyOf), len(wantKeyOf))
		}
		for sym, want := range wantKeyOf {
			if got := gotKeyOf[sym]; got != want {
				t.Fatalf("run %d: symbol %d keyed %q, first run keyed it %q", run, sym, got, want)
			}
		}
		if got := fmt.Sprintf("%+v", again.Edges()); got != wantEdges {
			t.Fatalf("run %d: edges\n got %s\nwant %s", run, got, wantEdges)
		}
	}
}

func keyBySymbolID(lookup map[string]metricsNode) map[int64]string {
	out := make(map[int64]string, len(lookup))
	for k, n := range lookup {
		out[n.SymbolID] = k
	}
	return out
}

func TestNoiseTagsAreMechanicalAndStated(t *testing.T) {
	for _, tc := range []struct {
		name string
		node metricsNode
		want []string
	}{
		{"getter", metricsNode{Label: "getUserDetails", Kind: model.KindMethod, Path: "a/R.java"}, []string{"accessor"}},
		{"setter", metricsNode{Label: "setName", Kind: model.KindMethod, Path: "a/R.java"}, []string{"accessor"}},
		{"is-prefix", metricsNode{Label: "isValid", Kind: model.KindMethod, Path: "a/R.java"}, []string{"accessor"}},
		{"not an accessor: get alone", metricsNode{Label: "get", Kind: model.KindMethod, Path: "a/R.java"}, nil},
		{"not an accessor: getty", metricsNode{Label: "gettysburg", Kind: model.KindMethod, Path: "a/R.java"}, nil},
		{"not an accessor: a field named getX", metricsNode{Label: "getX", Kind: model.KindField, Path: "a/R.java"}, nil},
		{"unicode accessor: getÜber", metricsNode{Label: "getÜber", Kind: model.KindMethod, Path: "a/R.java"}, []string{"accessor"}},
		{"unicode accessor: getÉcole", metricsNode{Label: "getÉcole", Kind: model.KindMethod, Path: "a/R.java"}, []string{"accessor"}},
		{"not an accessor: unicode lower-case getéclair", metricsNode{Label: "getéclair", Kind: model.KindMethod, Path: "a/R.java"}, nil},
		{"not an accessor: getß has no upper-case form", metricsNode{Label: "getß", Kind: model.KindMethod, Path: "a/R.java"}, nil},
		{"test file", metricsNode{Label: "Thing", Kind: model.KindClass, Path: "a/ThingTest.java"}, []string{"test"}},
		{"both", metricsNode{Label: "getThing", Kind: model.KindMethod, Path: "a/ThingTest.java"}, []string{"accessor", "test"}},
		{"neither", metricsNode{Label: "process", Kind: model.KindFunction, Path: "a/R.java"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := noiseTags(tc.node)
			if len(got) != len(tc.want) {
				t.Fatalf("tags = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("tags = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestFiltersAreOffByDefaultAndRemoveOnlyWhatTheyName(t *testing.T) {
	all := MetricsFilters{}
	if !all.keep(metricsNode{Label: "x", Kind: model.KindField, Path: "a/AT.java"}) {
		t.Error("default filters removed a row; the default must rank everything")
	}
	callable := MetricsFilters{Callable: true}
	if callable.keep(metricsNode{Label: "x", Kind: model.KindField, Path: "a/R.java"}) {
		t.Error("kinds=callable kept a field")
	}
	if !callable.keep(metricsNode{Label: "x", Kind: model.KindMethod, Path: "a/R.java"}) {
		t.Error("kinds=callable removed a method")
	}
	noTests := MetricsFilters{ExcludeTests: true}
	if noTests.keep(metricsNode{Label: "x", Kind: model.KindMethod, Path: "a/RTest.java"}) {
		t.Error("exclude_tests kept a test-file symbol")
	}
}

func TestSectionsUseADifferentMeasureEach(t *testing.T) {
	g, lookup := bridgeFixture()
	secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget())
	if err != nil {
		t.Fatalf("buildSections: %v", err)
	}
	measures := map[string]bool{}
	for _, s := range secs {
		if s.Measure == "" {
			t.Fatalf("section %q names no measure", s.Title)
		}
		if measures[s.Measure] {
			t.Errorf("two sections share the measure %q — that is one leaderboard printed twice", s.Measure)
		}
		measures[s.Measure] = true
		if s.Next.Label == "" || s.Next.Screen == "" {
			t.Errorf("section %q ends in no NextAction", s.Title)
		}
	}
}

func TestRiskSectionRanksTheChokepointFirst(t *testing.T) {
	g, lookup := bridgeFixture()
	secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget())
	if err != nil {
		t.Fatalf("buildSections: %v", err)
	}
	risk := secs[0]
	if risk.Measure != "betweenness" {
		t.Fatalf("first section measure = %q, want betweenness", risk.Measure)
	}
	if len(risk.Rows) == 0 || risk.Rows[0].Key != "bridge" {
		t.Fatalf("top row = %+v, want the bridge node", risk.Rows)
	}
	if risk.Next.Screen != "blast" {
		t.Errorf("risk NextAction goes to %q, want blast", risk.Next.Screen)
	}
}

func TestReadingSectionRanksOnlyExportedSymbols(t *testing.T) {
	g, lookup := bridgeFixture()
	// bridge is unexported in the fixture; a1 is exported.
	secs, _, _ := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget())
	read := secs[2]
	if read.Measure != "pagerank" {
		t.Fatalf("third section measure = %q, want pagerank", read.Measure)
	}
	// Without this the loop below is vacuous: an empty ranking would prove the
	// exported-only rule by never testing it.
	if len(read.Rows) == 0 {
		t.Fatal("the reading path ranked nothing; the assertion below would pass over an empty list")
	}
	for _, r := range read.Rows {
		if !lookup[r.Key].Exported {
			t.Errorf("the reading path offered an unexported symbol: %q", r.Key)
		}
	}
}

func TestRowsCarryNoiseTags(t *testing.T) {
	g, lookup := bridgeFixture()
	secs, _, _ := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget())
	// Same reason as above: no rows, no tags, and the search below finds
	// nothing for a reason that has nothing to do with tagging.
	if len(secs[0].Rows) == 0 {
		t.Fatal("the risk section ranked nothing; the search below would be vacuous")
	}
	var found bool
	for _, r := range secs[0].Rows {
		if r.Key == "a1" {
			for _, tag := range r.Tags {
				if tag == "accessor" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("a getter row carried no accessor tag")
	}
}

// bridgeFixture is two clusters joined only through "bridge": the shape where
// betweenness and PageRank disagree, which is what makes it useful here.
//
// a1 sits INSIDE the left side rather than on its end, so it lies on real
// shortest paths. A node on the end of the chain scores betweenness 0, and
// buildSections drops zero-scored rows on purpose — a fixture whose interesting
// node is an endpoint would test the drop, not the ranking.
func bridgeFixture() (metrics.Graph, map[string]metricsNode) {
	e := func(f, t string) metrics.Edge {
		return metrics.Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1}
	}
	nodes := []metrics.Node{
		{Key: "a1", Kind: model.KindMethod}, {Key: "a2", Kind: model.KindMethod},
		{Key: "bridge", Kind: model.KindMethod},
		{Key: "b1", Kind: model.KindMethod}, {Key: "b2", Kind: model.KindMethod},
	}
	g := metrics.NewGraph(nodes, []metrics.Edge{
		e("a1", "a2"), e("a2", "a1"), e("a1", "bridge"), e("bridge", "a1"),
		e("bridge", "b1"), e("b1", "bridge"), e("b1", "b2"), e("b2", "b1"),
	})
	lookup := map[string]metricsNode{
		"a1":     {Key: "a1", Label: "getThing", Path: "a/A.java", Kind: model.KindMethod, Exported: true},
		"a2":     {Key: "a2", Label: "process", Path: "a/A.java", Kind: model.KindMethod, Exported: true},
		"bridge": {Key: "bridge", Label: "dispatch", Path: "a/B.java", Kind: model.KindMethod, Exported: false},
		"b1":     {Key: "b1", Label: "handle", Path: "b/C.java", Kind: model.KindMethod, Exported: true},
		"b2":     {Key: "b2", Label: "finish", Path: "b/C.java", Kind: model.KindMethod, Exported: true},
	}
	return g, lookup
}

// ringFixture is an n-node ring: ONE strongly connected component of n members,
// and a single elementary cycle whose length is n. With n above the budget's
// MaxLen the enumeration returns nothing while the component count is n — the
// exact shape that made the old caveat read as a failed enumeration.
func ringFixture(n int) (metrics.Graph, map[string]metricsNode) {
	nodes := make([]metrics.Node, 0, n)
	edges := make([]metrics.Edge, 0, n)
	lookup := make(map[string]metricsNode, n)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%d", i)
		nodes = append(nodes, metrics.Node{Key: k, Kind: model.KindMethod})
		edges = append(edges, metrics.Edge{
			From: k, To: fmt.Sprintf("n%d", (i+1)%n),
			Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1,
		})
		lookup[k] = metricsNode{Key: k, Label: k, Path: "a/A.java", Kind: model.KindMethod, Exported: true}
	}
	return metrics.NewGraph(nodes, edges), lookup
}

// chainFixture is src -> mid -> sink: one pure source (instability 1.0), one
// middle (0.5) and one pure SINK (0.0, maximally stable).
func chainFixture() (metrics.Graph, map[string]metricsNode) {
	e := func(f, t string) metrics.Edge {
		return metrics.Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1}
	}
	nodes := []metrics.Node{
		{Key: "src", Kind: model.KindMethod},
		{Key: "mid", Kind: model.KindMethod},
		{Key: "sink", Kind: model.KindMethod},
	}
	g := metrics.NewGraph(nodes, []metrics.Edge{e("src", "mid"), e("mid", "sink")})
	lookup := map[string]metricsNode{
		"src":  {Key: "src", Label: "src", Path: "a/A.java", Kind: model.KindMethod, Exported: true},
		"mid":  {Key: "mid", Label: "mid", Path: "a/A.java", Kind: model.KindMethod, Exported: true},
		"sink": {Key: "sink", Label: "sink", Path: "a/A.java", Kind: model.KindMethod, Exported: true},
	}
	return g, lookup
}

// TestStructureCaveatDoesNotPassTheSCCCountOffAsTheListedCycles pins the
// relationship between two numbers computed by two different algorithms over
// two different objects: the SCC membership total, and the bounded enumeration
// printed underneath it. On an 8-ring they are 8 and 0, and the caveat must say
// why they differ rather than leave the reader to conclude the count is wrong.
func TestStructureCaveatDoesNotPassTheSCCCountOffAsTheListedCycles(t *testing.T) {
	g, lookup := ringFixture(8)
	secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget())
	if err != nil {
		t.Fatalf("buildSections: %v", err)
	}
	st := secs[1]
	if len(st.Cycles) != 0 {
		t.Fatalf("fixture enumerated %d cycle(s); it must enumerate none for this test to be about anything", len(st.Cycles))
	}
	if st.Caveat == "" {
		t.Fatal("structure section carries no caveat")
	}
	// The wrong sentence: 8 nodes described as sitting in "a cycle" while the
	// list beneath shows none.
	if strings.Contains(st.Caveat, "8 node(s) sit in a cycle") {
		t.Errorf("caveat still calls the SCC total a cycle count: %q", st.Caveat)
	}
	if !strings.Contains(st.Caveat, "8 node(s) sit in a strongly connected component") {
		t.Errorf("caveat does not attribute the count to strongly connected components: %q", st.Caveat)
	}
	// The listed cycles must be counted, and counted separately, so the reader
	// cannot read 8 as the size of the list below.
	if !strings.Contains(st.Caveat, fmt.Sprintf("%d cycle(s) listed below", len(st.Cycles))) {
		t.Errorf("caveat does not state how many cycles are actually listed: %q", st.Caveat)
	}
	if !strings.Contains(st.Caveat, fmt.Sprintf("length %d or less", metrics.DefaultCycleBudget().MaxLen)) {
		t.Errorf("caveat does not state the enumeration bound: %q", st.Caveat)
	}
}

// TestStructureSectionKeepsAPureSink: instability 0.0 is a reading, not an
// absence. The zero-skip was deleting exactly the maximally stable nodes a
// section titled "What resists change?" exists to name — and the reading-path
// section on the same screen ranked that same node first.
func TestStructureSectionKeepsAPureSink(t *testing.T) {
	g, lookup := chainFixture()
	secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget())
	if err != nil {
		t.Fatalf("buildSections: %v", err)
	}
	st := secs[1]
	var got []string
	for _, r := range st.Rows {
		got = append(got, fmt.Sprintf("%s (%.1f)", r.Key, r.Instability))
	}
	want := []string{"src (1.0)", "mid (0.5)", "sink (0.0)"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("structure rows = %v, want %v — a pure sink is the most stable node, not a missing one", got, want)
	}
	// The betweenness section must keep its zero-skip: a node on no shortest
	// path answers that section's question with a non-answer.
	for _, r := range secs[0].Rows {
		if r.Value == 0 {
			t.Errorf("risk section listed a zero-betweenness row: %q", r.Key)
		}
	}
}

// TestSectionsAccountForEveryRowTheyDropped: metricsTopN, the zero-skip and the
// filters each remove rows, and nothing downstream can recover how many. The
// figure has to be captured where the removal happens.
func TestSectionsAccountForEveryRowTheyDropped(t *testing.T) {
	g, lookup := ringFixture(metricsTopN + 6)
	secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{ExcludeTests: true}, metrics.DefaultCycleBudget())
	if err != nil {
		t.Fatalf("buildSections: %v", err)
	}
	for _, s := range secs {
		if s.Considered < len(s.Rows) {
			t.Errorf("section %q considered %d but shows %d rows", s.Title, s.Considered, len(s.Rows))
		}
		dropped := 0
		for _, om := range s.Omissions.Omitted {
			dropped += om.Count
		}
		if s.Considered-len(s.Rows) != dropped {
			t.Errorf("section %q dropped %d row(s) but accounts for %d",
				s.Title, s.Considered-len(s.Rows), dropped)
		}
	}
	st := secs[1]
	if len(st.Rows) != metricsTopN {
		t.Fatalf("structure rows = %d, want the cap %d", len(st.Rows), metricsTopN)
	}
	var capped int
	for _, om := range st.Omissions.Omitted {
		if om.Reason == ReasonRowBudget {
			capped += om.Count
		}
	}
	if capped != 6 {
		t.Errorf("rows beyond the cap = %d, want 6", capped)
	}
	if capped > 0 && st.Omissions.Complete {
		t.Error("a section that hid rows still marshalled as complete")
	}

	// A ring ties every node at instability 0.5, and lifting the zero-skip adds
	// more ties at 0.0 elsewhere. Ties are what the key tiebreak exists for, so
	// the truncated top-20 must be the same twenty rows in the same order every
	// run — otherwise the cap itself becomes a coin toss.
	var want []string
	for _, r := range st.Rows {
		want = append(want, r.Key)
	}
	for run := 0; run < 20; run++ {
		again, _, err := buildSections(context.Background(), LevelSymbol, g, lookup, MetricsFilters{ExcludeTests: true}, metrics.DefaultCycleBudget())
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		var got []string
		for _, r := range again[1].Rows {
			got = append(got, r.Key)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("run %d: structure order\n got %v\nwant %v", run, got, want)
		}
	}
}

// ---------------------------------------------------------------- handler

func TestMetricsHandlerCarriesMetaOmissionsAndEverySection(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["meta"]; !ok {
		t.Error("no meta header")
	}
	if _, ok := body["omissions"]; !ok {
		t.Error("no omissions block")
	}
	secs, ok := body["sections"].([]any)
	if !ok || len(secs) != 3 {
		t.Fatalf("sections = %v, want 3", body["sections"])
	}
	for i, s := range secs {
		m, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("section %d is %T, not an object", i, s)
		}
		if m["caveat"] == "" {
			t.Errorf("section %d states no caveat", i)
		}
		n, ok := m["next"].(map[string]any)
		if !ok || n["label"] == "" {
			t.Errorf("section %d ends in no NextAction", i)
		}
	}
}

// TestMetricsHandlerDisclosesEveryLimitItCannotSee pins the four disclosures.
//
// Each must reach the reader on a run where its COUNT is zero too: the fixture
// has no self-loop, and a limit that is stated only when it happens to bite is
// a limit the reader is never told about.
func TestMetricsHandlerDisclosesEveryLimitItCannotSee(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, req)

	body := strings.ToLower(rec.Body.String())
	for _, want := range []string{"sampled", "self-loop", "standard of proof", "cycles longer than"} {
		if !strings.Contains(body, want) {
			t.Errorf("the response never mentions %q — a limit the reader cannot see is not disclosed:\n%s",
				want, rec.Body.String())
		}
	}
	if !strings.Contains(body, "70") {
		t.Errorf("the betweenness sample size is not named:\n%s", rec.Body.String())
	}
}

func TestMetricsHandlerWithoutAnIndexAsksForOne(t *testing.T) {
	t.Parallel()
	srv := unindexedServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("an unindexed repository produced status %d; it must explain, not fail", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cortex index") {
		t.Errorf("the response does not name the command that fixes it:\n%s", rec.Body.String())
	}
}

func TestMetricsRouteIsRegisteredAndReadOnly(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)
	mux := srv.routes()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Host = "127.0.0.1:7777"
	if _, pattern := mux.Handler(req); pattern == "" {
		t.Fatal("/api/metrics is not registered")
	}
	post := httptest.NewRequest(http.MethodPost, "/api/metrics", nil)
	post.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code == http.StatusOK {
		t.Error("POST /api/metrics succeeded; this portal is read-only")
	}
}

// The 404 body lists the endpoints this portal serves; an endpoint absent from
// it is unreachable to anyone who did not already know it exists.
func TestMetricsIsNamedInThePublishedEndpointList(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)
	rec := get(t, srv, "/api/nosuchthing", nil)
	if !strings.Contains(rec.Body.String(), "/api/metrics") {
		t.Errorf("the endpoint list does not name /api/metrics:\n%s", rec.Body.String())
	}
}

// The fixture graph is smaller than the sample budget, so the handler test
// exercises only the complete branch. This pins the other one: a graph above
// the budget must be described as sampled, and must name both numbers. The
// complete branch says "complete", not "exact": on screen, exact is a
// confidence tier, and the plain-language spec forbids the bare id in prose.
func TestBetweennessLimitDistinguishesSampledFromExact(t *testing.T) {
	t.Parallel()
	bo := metrics.DefaultBetweennessOptions()
	complete := betweennessLimit(bo, 10)
	if !strings.Contains(complete, "complete") || !strings.Contains(complete, "70") {
		t.Errorf("a graph under the budget is not described as complete: %q", complete)
	}
	sampled := betweennessLimit(bo, 5000)
	for _, want := range []string{"SAMPLED", "70", "5000"} {
		if !strings.Contains(sampled, want) {
			t.Errorf("the sampled description omits %q: %q", want, sampled)
		}
	}
}

// ------------------------------------------------- intra-group collapse

// intraGroupFixture is the shape the path levels lose edges on, with all three
// populations present and DIFFERENT sizes, so no assertion below can pass by
// one count standing in for another:
//
//   - 1 -> 2 is one file, one directory: collapsed at both path levels.
//   - 1 -> 3 is two files in one directory: collapsed at directory level only.
//   - 1 -> 4 crosses both, and survives everywhere.
//   - 1 -> 1 is direct recursion: a self-loop at every level, including the
//     path levels, where it is disclosed as recursion rather than as collapse.
func intraGroupFixture(t *testing.T) *Server {
	t.Helper()
	syms := []captest.Sym{
		captest.S(1, "alpha", "pkg/a/one.go").Export(),
		captest.S(2, "beta", "pkg/a/one.go").Export(),
		captest.S(3, "gamma", "pkg/a/two.go").Export(),
		captest.S(4, "delta", "pkg/b/three.go").Export(),
	}
	edges := []captest.Edge{
		captest.E(1, 2).WithConf(model.ConfidenceHigh),
		captest.E(1, 3).WithConf(model.ConfidenceHigh),
		captest.E(1, 4).WithConf(model.ConfidenceHigh),
		captest.E(1, 1).WithConf(model.ConfidenceHigh),
	}
	deps, _ := captest.Build(t, syms, edges, captest.Refs{Resolved: 4})
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	return newServer(t, reg, deps, config.Config{ProjectName: "intragroup"})
}

// omissionFor returns the single omission carrying reason, or fails.
func omissionFor(t *testing.T, om Omissions, reason string) Omission {
	t.Helper()
	var found []Omission
	for _, o := range om.Omitted {
		if o.Reason == reason {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("omissions carry %d entries for %q, want exactly 1: %+v", len(found), reason, om.Omitted)
	}
	return found[0]
}

// TestMetricsFileLevelDisclosesTheEdgesItCollapsed: at file level the screen
// dropped every edge between two symbols of the same file and said nothing,
// while publishing a self-loop count of an unrelated population. On the apm
// index that was 2,936 edges — 25.9% of the graph — omitted from every number
// with no entry accounting for them.
func TestMetricsFileLevelDisclosesTheEdgesItCollapsed(t *testing.T) {
	t.Parallel()
	srv := intraGroupFixture(t)

	var got MetricsResponse
	rec := get(t, srv, "/api/metrics?level=file&min_confidence=low", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	om := omissionFor(t, got.Omissions, ReasonIntraGroup)
	if om.Count != 1 {
		t.Errorf("intra_group count = %d, want 1 (alpha -> beta, both in pkg/a/one.go)", om.Count)
	}
	if !strings.Contains(om.Remedy+" "+om.Detail, "level=symbol") {
		t.Errorf("the disclosure never names the level that shows these edges: %+v", om)
	}
}

// TestMetricsDirectoryLevelDisclosesTheEdgesItCollapsed is the same defect one
// level coarser, where it was worst: 5,585 edges, 49.2% of the apm graph.
func TestMetricsDirectoryLevelDisclosesTheEdgesItCollapsed(t *testing.T) {
	t.Parallel()
	srv := intraGroupFixture(t)

	var got MetricsResponse
	rec := get(t, srv, "/api/metrics?level=directory&min_confidence=low", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	om := omissionFor(t, got.Omissions, ReasonIntraGroup)
	if om.Count != 2 {
		t.Errorf("intra_group count = %d, want 2 — a directory collapses the cross-file edge too", om.Count)
	}
}

// TestMetricsSymbolLevelCollapsesNothingAndSaysSo: symbol level keys one node
// per symbol, so no edge between two distinct symbols is collapsed. A sized
// entry there would be a disclosure of something that did not happen.
func TestMetricsSymbolLevelCollapsesNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	srv := intraGroupFixture(t)

	var got MetricsResponse
	rec := get(t, srv, "/api/metrics?level=symbol&min_confidence=low", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	for _, o := range got.Omissions.Omitted {
		if o.Reason == ReasonIntraGroup {
			t.Errorf("symbol level published an intra_group omission: %+v", o)
		}
	}
	// The LIMIT is still stated, for the reason MetricsLimits gives: a limit
	// the reader is told about only when it bites is a limit they never learn.
	if got.Limits == nil || got.Limits.IntraGroup == "" {
		t.Fatalf("symbol level states no intra-group limit: %+v", got.Limits)
	}
}

// TestMetricsIntraGroupAndSelfLoopsAreSeparatePopulations: an intra-group edge
// joins two DISTINCT symbols and reappears at level=symbol; a self-loop is
// recursion the graph model holds at no level. Folding either into the other
// hands the reader one number for two different claims.
func TestMetricsIntraGroupAndSelfLoopsAreSeparatePopulations(t *testing.T) {
	t.Parallel()
	srv := intraGroupFixture(t)

	var got MetricsResponse
	rec := get(t, srv, "/api/metrics?level=directory&min_confidence=low", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	intra := omissionFor(t, got.Omissions, ReasonIntraGroup)
	self := omissionFor(t, got.Omissions, ReasonSelfLoop)
	if intra.Count != 2 {
		t.Errorf("intra_group = %d, want 2", intra.Count)
	}
	if self.Count != 1 {
		t.Errorf("self_loop = %d, want 1 — the fixture has exactly one recursive edge", self.Count)
	}
	if intra.Count == self.Count {
		t.Errorf("the two counts are equal (%d); the fixture must tell them apart", intra.Count)
	}
	if got.Limits.SelfLoops == got.Limits.IntraGroup {
		t.Error("the self-loop and intra-group limits are the same sentence")
	}
	// Neither may absorb the other: the self-loop is collapsed by the
	// directory projection too, and counting it in both places would publish
	// one edge twice.
	if intra.Count != 2 {
		t.Errorf("intra_group = %d; the self-loop must not be counted as a collapse", intra.Count)
	}
}

// TestMetricsScreenIsReachableFromTheNav asserts the payload has a reader.
//
// An endpoint no screen fetches is a measurement nobody sees, and the portal's
// own rule (overview.go) is that a number without a next step is wallpaper. The
// halves are asserted separately because they fail separately: a nav button
// with no renderer opens a blank panel, and a renderer with no nav button is
// reachable only by editing the URL.
//
// Three substring checks used to be the whole test, and they missed the failure
// the comment above claims to catch: go() falls back to `overview` for a screen
// not in SCREENS, so dropping 'metrics' from that list — or deleting
// LOADERS.metrics — leaves the markup intact while the nav button quietly opens
// a different screen. Both are now asserted, and the parity check protects the
// other nineteen screens by the same rule.
func TestMetricsScreenIsReachableFromTheNav(t *testing.T) {
	html, err := assets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	// The rail's buttons are built by js/nav.mjs from one list, not written
	// into the shell, so the static shell is asserted to hold the rail and the
	// module that fills it, and the buttons are read from the list.
	if !strings.Contains(string(html), `<nav class="rail"`) {
		t.Error("the shell has no rail for js/nav.mjs to build into")
	}
	if !strings.Contains(string(html), `<script type="module" src="/assets/js/main.mjs">`) {
		t.Error("the shell does not load js/main.mjs, so no rail button is ever built")
	}
	nav, err := assets.ReadFile("assets/js/nav.mjs")
	if err != nil {
		t.Fatalf("read js/nav.mjs: %v", err)
	}
	if !strings.Contains(string(nav), `id: 'metrics'`) {
		t.Error("no nav entry reaches the metrics screen")
	}
	if !strings.Contains(string(html), `id="screen-metrics"`) {
		t.Error("no section for the metrics screen to render into")
	}
	js, err := assets.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	if !strings.Contains(string(js), "/api/metrics") {
		t.Error("app.js never calls /api/metrics")
	}
	// The two halves the substrings above cannot see.
	if !contains(screenList(t, string(js)), "metrics") {
		t.Error("SCREENS does not list 'metrics', so the nav button routes to the overview fallback")
	}
	if !strings.Contains(string(js), "LOADERS.metrics") {
		t.Error("no loader is registered for the metrics screen, so its panel renders empty")
	}
	for _, p := range screenParityProblems(t, string(js), string(nav), string(html)) {
		t.Error(p)
	}
}

// screenParityProblems states every disagreement between the client's screen
// list, the rail list js/nav.mjs builds the buttons from, and the markup that
// must exist for a screen to be reachable.
//
// Split out so the check itself is testable against a synthetic client, for the
// reason TestContractCheckerDetectsARename gives: a check never seen to fail is
// not known to work.
func screenParityProblems(t *testing.T, js, nav, html string) []string {
	t.Helper()
	var out []string
	inMarkup := map[string]bool{}
	for _, m := range regexp.MustCompile(`id: '([a-z]+)'`).FindAllStringSubmatch(nav, -1) {
		inMarkup[m[1]] = true
	}
	for _, screen := range screenList(t, js) {
		if !inMarkup[screen] {
			out = append(out, fmt.Sprintf("SCREENS lists %q, which no nav entry reaches", screen))
		}
		if !strings.Contains(html, `id="screen-`+screen+`"`) {
			out = append(out, fmt.Sprintf("SCREENS lists %q, which has no section to render into", screen))
		}
	}
	known := map[string]bool{}
	for _, screen := range screenList(t, js) {
		known[screen] = true
	}
	for screen := range inMarkup {
		if !known[screen] {
			out = append(out, fmt.Sprintf("a nav entry opens %q, which SCREENS does not list — go() will "+
				"route it to the overview fallback and the button will open the wrong screen", screen))
		}
	}
	sort.Strings(out)
	return out
}

// screenList reads the client's own SCREENS array. Parsed rather than
// duplicated here: a copy in the test would agree with itself while disagreeing
// with the shipped client.
func screenList(t *testing.T, js string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)const SCREENS = \[(.*?)\];`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js declares no SCREENS array; the nav's routing cannot be checked")
	}
	var out []string
	for _, q := range regexp.MustCompile(`'([a-z]+)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	if len(out) == 0 {
		t.Fatal("SCREENS is empty")
	}
	return out
}

func contains(in []string, want string) bool {
	for _, x := range in {
		if x == want {
			return true
		}
	}
	return false
}

// TestScreenParityCheckerCatchesADroppedScreen is the test that tests the test.
// Both halves of the failure the check exists for are synthesised here, because
// the shipped client is correct and a check that has never failed proves
// nothing.
func TestScreenParityCheckerCatchesADroppedScreen(t *testing.T) {
	t.Parallel()

	const nav = `{ id: 'graph', label: 'Map of the code' },
{ id: 'metrics', label: 'Shape of the code' },`
	const html = `<section id="screen-graph"></section><section id="screen-metrics"></section>`

	if p := screenParityProblems(t, "const SCREENS = [\n  'graph', 'metrics',\n];", nav, html); len(p) != 0 {
		t.Errorf("a client in agreement with its markup reported problems: %v", p)
	}
	// 'metrics' dropped from SCREENS: the nav entry still exists and still
	// looks right, and go() sends the reader to the overview.
	dropped := screenParityProblems(t, "const SCREENS = [\n  'graph',\n];", nav, html)
	if len(dropped) == 0 {
		t.Error("dropping a screen from SCREENS while its nav entry remains went unreported")
	}
	// A screen listed with no markup behind it: a nav entry that opens nothing.
	orphan := screenParityProblems(t, "const SCREENS = [\n  'graph', 'metrics', 'ghost',\n];", nav, html)
	if len(orphan) < 2 {
		t.Errorf("a listed screen with neither button nor section reported %d problem(s): %v", len(orphan), orphan)
	}
}

// TestMetricsClosingActionIsPopulated: the screen's own rule is that a number
// without a next step is wallpaper, and the top-level action shipped as
// {"label":"","detail":""} on every indexed response — so the client rendered
// nothing at the one place a reader lands after three rankings. Either it says
// something or the field has no business being in the payload.
func TestMetricsClosingActionIsPopulated(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got MetricsResponse
	rec := get(t, srv, "/api/metrics", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(got.Sections) == 0 {
		t.Fatal("no sections; this test is about the action that closes a screen that measured something")
	}
	if got.Next.Label == "" || got.Next.Detail == "" {
		t.Errorf("the screen's closing action is empty: %+v", got.Next)
	}
	if got.Next.Command == "" && got.Next.Screen == "" {
		t.Errorf("the closing action names neither a command nor a screen: %+v", got.Next)
	}
}

// pairCyclesFixture is the shape the live apm index has at symbol level: two
// strongly connected components of exactly two members each, and nothing
// longer. Every cycle in it is enumerable inside any budget worth shipping, and
// the two listed cycles account for all four members — which is why the
// structure caveat must not claim that longer cycles exist or that the list
// falls short of the SCC total.
func pairCyclesFixture() (metrics.Graph, map[string]metricsNode) {
	e := func(f, t string) metrics.Edge {
		return metrics.Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1}
	}
	keys := []string{"getId", "getName", "setId", "setName", "loner"}
	nodes := make([]metrics.Node, 0, len(keys))
	lookup := make(map[string]metricsNode, len(keys))
	for _, k := range keys {
		nodes = append(nodes, metrics.Node{Key: k, Kind: model.KindMethod})
		lookup[k] = metricsNode{Key: k, Label: k, Path: "a/A.java", Kind: model.KindMethod, Exported: true}
	}
	g := metrics.NewGraph(nodes, []metrics.Edge{
		e("getId", "getName"), e("getName", "getId"),
		e("setId", "setName"), e("setName", "setId"),
		e("getId", "loner"),
	})
	return g, lookup
}

// TestStructureCaveatSaysOnlyWhatTheEnumerationMeasured holds the two clauses
// that shipped as assertions to what was actually measured.
//
// "longer ones exist unreported" and "the list does not account for the N" were
// printed on every response with a non-empty component, including the live apm
// index, where the two components hold two members each and the two listed
// cycles cover all four nodes. Both clauses were false there, and the
// response's own cycles limit stated the same fact correctly — one fact in two
// vocabularies, the stronger one wrong.
func TestStructureCaveatSaysOnlyWhatTheEnumerationMeasured(t *testing.T) {
	t.Parallel()

	t.Run("a ring longer than the bound says longer cycles may exist", func(t *testing.T) {
		g, lookup := ringFixture(8)
		secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup,
			MetricsFilters{}, metrics.DefaultCycleBudget())
		if err != nil {
			t.Fatalf("buildSections: %v", err)
		}
		c := secs[1].Caveat
		if len(secs[1].Cycles) != 0 {
			t.Fatalf("fixture listed %d cycle(s); it must list none for this subtest to be about anything", len(secs[1].Cycles))
		}
		if !strings.Contains(c, "may exist unreported") {
			t.Errorf("an 8-ring under a length bound of 5 hides a cycle and the caveat does not say so: %q", c)
		}
		if !strings.Contains(c, "does not account for") {
			t.Errorf("no listed cycle covers any of the 8 members and the caveat does not say so: %q", c)
		}
	})

	t.Run("two-member components say nothing longer can exist", func(t *testing.T) {
		g, lookup := pairCyclesFixture()
		secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup,
			MetricsFilters{}, metrics.DefaultCycleBudget())
		if err != nil {
			t.Fatalf("buildSections: %v", err)
		}
		st := secs[1]
		if len(st.Cycles) != 2 {
			t.Fatalf("fixture listed %d cycle(s), want 2", len(st.Cycles))
		}
		c := st.Caveat
		if strings.Contains(c, "may exist unreported") || strings.Contains(c, "exist unreported") {
			t.Errorf("no cycle longer than 2 can exist in a graph whose largest component holds 2 nodes, "+
				"and the caveat claims otherwise: %q", c)
		}
		if strings.Contains(c, "does not account for") {
			t.Errorf("the 2 listed cycles cover all 4 component members, and the caveat claims otherwise: %q", c)
		}
		if !strings.Contains(c, "account for all 4") {
			t.Errorf("the caveat does not say the list accounts for every node in a component: %q", c)
		}
	})
}

// isolatedFixture is a 5-ring plus three nodes with no edge in either
// direction — the shape the live apm index has 7,291 of at symbol level:
// fields, constants and types the extractor records no relationship for.
func isolatedFixture() (metrics.Graph, map[string]metricsNode) {
	ring, lookup := ringFixture(5)
	nodes := make([]metrics.Node, 0, ring.Order()+3)
	for k := range lookup {
		nodes = append(nodes, metrics.Node{Key: k, Kind: model.KindMethod})
	}
	edges := make([]metrics.Edge, 0, 5)
	for i := 0; i < 5; i++ {
		edges = append(edges, metrics.Edge{
			From: fmt.Sprintf("n%d", i), To: fmt.Sprintf("n%d", (i+1)%5),
			Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1,
		})
	}
	for _, k := range []string{"lonelyA", "lonelyB", "lonelyC"} {
		nodes = append(nodes, metrics.Node{Key: k, Kind: model.KindField})
		lookup[k] = metricsNode{Key: k, Label: k, Path: "a/A.java", Kind: model.KindField, Exported: true}
	}
	return metrics.NewGraph(nodes, edges), lookup
}

// TestStructureSectionAccountsForTheNodesItNeverRanks: the section skipped
// every node with no edges BEFORE the accounting take exists to produce, so on
// the live apm index 7,291 symbols left the screen with no disclosure at all —
// section 1 reported 3,809 considered while the sections either side of it
// reported 11,100 over the same graph.
func TestStructureSectionAccountsForTheNodesItNeverRanks(t *testing.T) {
	t.Parallel()
	g, lookup := isolatedFixture()
	secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup,
		MetricsFilters{}, metrics.DefaultCycleBudget())
	if err != nil {
		t.Fatalf("buildSections: %v", err)
	}
	st := secs[1]
	if st.Considered != secs[0].Considered {
		t.Errorf("the structure section considered %d candidate(s) of the same graph the risk section "+
			"considered %d of; the difference is the nodes it drops before counting them",
			st.Considered, secs[0].Considered)
	}
	var isolated *Omission
	for i, om := range st.Omissions.Omitted {
		if om.Reason == ReasonIsolatedNode {
			isolated = &st.Omissions.Omitted[i]
		}
	}
	if isolated == nil {
		t.Fatalf("three nodes with no edges are absent from the ranking and nothing says so: %+v",
			st.Omissions.Omitted)
	}
	if isolated.Count != 3 {
		t.Errorf("isolated_node count = %d, want 3", isolated.Count)
	}
	if isolated.Remedy == "" {
		t.Error("the omission states no remedy")
	}
	dropped := 0
	for _, om := range st.Omissions.Omitted {
		dropped += om.Count
	}
	if st.Considered-len(st.Rows) != dropped {
		t.Errorf("section dropped %d row(s) but accounts for %d", st.Considered-len(st.Rows), dropped)
	}
}

// sampledFixture is a graph larger than DefaultBetweennessOptions().SampleSize,
// so betweenness over it is estimated from a stride of its sources rather than
// measured over all of them. The leaves score zero: nothing passes through a
// node nothing points at.
func sampledFixture(order int) (metrics.Graph, map[string]metricsNode) {
	ringSize := order / 2
	nodes := make([]metrics.Node, 0, order)
	edges := make([]metrics.Edge, 0, order)
	lookup := make(map[string]metricsNode, order)
	add := func(k string) {
		nodes = append(nodes, metrics.Node{Key: k, Kind: model.KindMethod})
		lookup[k] = metricsNode{Key: k, Label: k, Path: "a/A.java", Kind: model.KindMethod, Exported: true}
	}
	edge := func(f, t string) {
		edges = append(edges, metrics.Edge{
			From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1,
		})
	}
	for i := 0; i < ringSize; i++ {
		add(fmt.Sprintf("r%03d", i))
	}
	for i := 0; i < ringSize; i++ {
		edge(fmt.Sprintf("r%03d", i), fmt.Sprintf("r%03d", (i+1)%ringSize))
	}
	for i := 0; i < order-ringSize; i++ {
		k := fmt.Sprintf("leaf%03d", i)
		add(k)
		edge(k, fmt.Sprintf("r%03d", i%ringSize))
	}
	return metrics.NewGraph(nodes, edges), lookup
}

// TestZeroBetweennessSaysWhatAZeroMeansUnderASample: at symbol level on the
// live apm index betweenness samples 70 sources of 11,100 — a 0.6% stride —
// and 11,086 rows are then hidden as zero_score under the remedy "None — a
// zero score is not hidden data." Under a stride that thin a zero is
// overwhelmingly an artefact of which sources were drawn, not a reading that
// the row is on no path at all, and the response-level sampled_betweenness
// omission on the same screen says exactly that. Two descriptions of one
// population, and they disagree.
func TestZeroBetweennessSaysWhatAZeroMeansUnderASample(t *testing.T) {
	t.Parallel()
	bo := metrics.DefaultBetweennessOptions()

	zeroScore := func(t *testing.T, sec MetricsSection) Omission {
		t.Helper()
		for _, om := range sec.Omissions.Omitted {
			if om.Reason == ReasonZeroScore {
				return om
			}
		}
		t.Fatalf("section %q hid nothing under zero_score: %+v", sec.Measure, sec.Omissions.Omitted)
		return Omission{}
	}

	t.Run("sampled", func(t *testing.T) {
		g, lookup := sampledFixture(bo.SampleSize + 30)
		if g.Order() <= bo.SampleSize {
			t.Fatalf("fixture order %d is not above the sample size %d", g.Order(), bo.SampleSize)
		}
		secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup,
			MetricsFilters{}, metrics.DefaultCycleBudget())
		if err != nil {
			t.Fatalf("buildSections: %v", err)
		}
		om := zeroScore(t, secs[0])
		said := om.Detail + " " + om.Remedy
		for _, w := range []string{"sampled", fmt.Sprint(bo.SampleSize), fmt.Sprint(g.Order())} {
			if !strings.Contains(strings.ToLower(said), w) {
				t.Errorf("the betweenness zero-score disclosure never says %q under a sample: %q", w, said)
			}
		}
		if strings.Contains(said, "a zero score is not hidden data") {
			t.Errorf("under a sample a zero is a row the sample never measured a path through, and the "+
				"disclosure still calls it a reading: %q", said)
		}
		// PageRank is computed over every node, so if it hides a zero at all
		// that zero is a reading and must not borrow betweenness's caveat.
		for _, p := range secs[2].Omissions.Omitted {
			if p.Reason == ReasonZeroScore && strings.Contains(p.Detail, "sampled") {
				t.Errorf("the pagerank section describes its zeros as sampled: %q", p.Detail)
			}
		}
	})

	t.Run("exact", func(t *testing.T) {
		g, lookup := sampledFixture(20)
		if g.Order() > bo.SampleSize {
			t.Fatalf("fixture order %d is above the sample size %d", g.Order(), bo.SampleSize)
		}
		secs, _, err := buildSections(context.Background(), LevelSymbol, g, lookup,
			MetricsFilters{}, metrics.DefaultCycleBudget())
		if err != nil {
			t.Fatalf("buildSections: %v", err)
		}
		om := zeroScore(t, secs[0])
		if strings.Contains(strings.ToLower(om.Detail), "sampl") {
			t.Errorf("every source was used on a graph this size, and the disclosure claims a sample: %q", om.Detail)
		}
	})
}

// TestSelfLoopAndFloorCountEachEdgeOnce: metricsDropped's own doc says an edge
// can be counted in only one of the published reasons, because two counts over
// one population read as two populations. The self-loop tally broke that rule.
// It counted every self-loop whose EDGE was at or above the floor, while the
// projection had already counted that same edge under below_confidence_floor
// whenever the floor took the SYMBOL with it — one edge, published twice, under
// two remedies that contradict each other.
func TestSelfLoopAndFloorCountEachEdgeOnce(t *testing.T) {
	t.Parallel()

	syms := []captest.Sym{
		captest.S(1, "Recurse", "pkg/a/one.go").Export(),
		captest.S(2, "Callee", "pkg/a/two.go").Export(),
	}
	// The self-loop is `exact`, so it clears an exact floor that its own symbol
	// (seeded `high`, as every captest symbol is) does not.
	edges := []captest.Edge{
		captest.E(1, 1).WithConf(model.ConfidenceExact),
		captest.E(1, 2).WithConf(model.ConfidenceHigh),
	}
	deps, _ := captest.Build(t, syms, edges, captest.Refs{Resolved: 2})
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	srv := newServer(t, reg, deps, config.Config{ProjectName: "self-loops"})

	count := func(r MetricsResponse, reason string) int {
		n := 0
		for _, om := range r.Omissions.Omitted {
			if om.Reason == reason {
				n += om.Count
			}
		}
		return n
	}

	// At the default floor both the edge and its symbol survive, so the
	// self-loop is the graph model's limit and nothing else has counted it.
	var atMedium MetricsResponse
	if rec := get(t, srv, "/api/metrics?level=symbol", &atMedium); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if n := count(atMedium, ReasonSelfLoop); n != 1 {
		t.Errorf("self_loop = %d at the default floor, want the one recursive edge", n)
	}

	// At an exact floor the symbol is gone, so the projection removed the edge
	// as a floor casualty. It must not ALSO be published as recursion.
	var atExact MetricsResponse
	if rec := get(t, srv, "/api/metrics?level=symbol&min_confidence=exact", &atExact); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	floor, self := count(atExact, ReasonBelowConfidenceFloor), count(atExact, ReasonSelfLoop)
	if floor+self > len(edges) {
		t.Errorf("the index holds %d edge(s) and the response publishes %d under the confidence floor "+
			"plus %d as recursion; an edge counted under two reasons reads as two edges",
			len(edges), floor, self)
	}
	if self != 0 {
		t.Errorf("self_loop = %d for an edge the floor already removed with its symbol", self)
	}
}

// TestRowFilterRemedyNamesOnlyTheFiltersThatAreSet: "Clear the kinds and
// exclude_tests filters" told a reader holding one of them to clear two. The
// same defect this screen already fixed once, one size down: a remedy naming a
// control that is not set costs the reader an attempt to discover it was never
// the reason.
func TestRowFilterRemedyNamesOnlyTheFiltersThatAreSet(t *testing.T) {
	t.Parallel()
	srv := metricsScreenFixture(t)

	cases := []struct {
		query    string
		wantSaid string
		notSaid  []string
	}{
		{query: "kinds=callable", wantSaid: "kinds", notSaid: []string{"exclude_tests"}},
		{query: "exclude_tests=1", wantSaid: "exclude_tests", notSaid: []string{"kinds"}},
		{query: "kinds=callable&exclude_tests=1", wantSaid: "kinds and exclude_tests"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			var got MetricsResponse
			rec := get(t, srv, "/api/metrics?level=symbol&"+tc.query, &got)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			found := false
			for _, sec := range got.Sections {
				for _, om := range sec.Omissions.Omitted {
					if om.Reason != ReasonRowFilter {
						continue
					}
					found = true
					if !strings.Contains(om.Remedy, tc.wantSaid) {
						t.Errorf("remedy %q does not name the filter that is set (%s)", om.Remedy, tc.wantSaid)
					}
					for _, n := range tc.notSaid {
						if strings.Contains(om.Remedy, n) {
							t.Errorf("remedy %q tells the reader to clear %s, which is not set", om.Remedy, n)
						}
					}
				}
			}
			if !found {
				t.Fatalf("no row_filter omission on a request that sets %s", tc.query)
			}
		})
	}
}

// pollingContext counts ctx.Err() calls and can start returning Canceled after
// a given number of them, so a test can tell a projection that polls DURING its
// work from one that polls only on the way out.
type pollingContext struct {
	context.Context
	calls       *int
	cancelAfter int
}

func (c pollingContext) Err() error {
	*c.calls++
	if c.cancelAfter > 0 && *c.calls >= c.cancelAfter {
		return context.Canceled
	}
	return nil
}

// TestMetricsProjectionHonoursCancellationWhileItRuns: metricsGraph's doc says
// a reader who navigates away mid-request should not pay for a graph nobody
// will read, and the only ctx.Err() was after the whole projection had already
// run. The sentence was true of the return value and false of the work.
func TestMetricsProjectionHonoursCancellationWhileItRuns(t *testing.T) {
	t.Parallel()
	srv := metricsScreenFixture(t)
	_, run, ok, err := srv.run(context.Background())
	if err != nil || !ok {
		t.Fatalf("run: %v (ok=%t)", err, ok)
	}
	idx, err := srv.structural(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("structural: %v", err)
	}

	for _, level := range []MetricsLevel{LevelSymbol, LevelFile, LevelDirectory} {
		calls := 0
		if _, _, _, err := idx.metricsGraph(
			pollingContext{Context: context.Background(), calls: &calls}, level, DefaultFloor,
		); err != nil {
			t.Fatalf("level=%s: %v", level, err)
		}
		if calls <= 2 {
			t.Errorf("level=%s: the projection polled ctx %d time(s), which is the exits and nothing "+
				"in between: a cancelled reader still pays for the whole graph", level, calls)
		}

		cancelled := 0
		_, _, _, err := idx.metricsGraph(
			pollingContext{Context: context.Background(), calls: &cancelled, cancelAfter: 1}, level, DefaultFloor)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("level=%s: a cancelled projection returned %v, want a wrapped context.Canceled", level, err)
		}
	}
}
