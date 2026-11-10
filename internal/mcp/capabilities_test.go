package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newGraphTestServer is newTestServer with the graph wired in.
//
// The graph-backed tools register only when Deps.Graph is present — an index
// whose references were never resolved should still serve symbol lookups
// rather than failing to start — so impact_analysis is unreachable without it.
func newGraphTestServer(t *testing.T, seed func(context.Context, *registry.Registry, int64)) *sdk.ClientSession {
	t.Helper()
	return newGraphTestServerAt(t, "", seed)
}

// newGraphTestServerAt is newGraphTestServer with the project's RepoRoot set,
// for tests of tools that read files relative to it.
func newGraphTestServerAt(t *testing.T, repoRoot string, seed func(context.Context, *registry.Registry, int64)) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(ctx, reg, run.ID)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.ProjectName = "testproj"
	cfg.RepoRoot = repoRoot
	// impact_analysis is a standard-profile tool; the minimal default does not
	// expose it.
	cfg.MCPProfile = config.MCPStandard

	srv, err := New(Deps{Registry: reg, Graph: graph.New(db), Config: cfg})
	if err != nil {
		t.Fatal(err)
	}

	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), serverT) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// seedDispatchGraph builds an interface method with one caller and one
// implementation, which is the smallest graph that separates the two claims:
// callPersist references the target, MySQLStore#persist merely implements it.
func seedDispatchGraph(ctx context.Context, reg *registry.Registry, runID int64) {
	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	defer w.Close()

	iface := model.File{Path: "store/Store.java", Language: "java", ContentHash: "d1", ParseStatus: "ok"}
	ifaceID, _ := w.AddFile(ctx, &iface)
	impl := model.File{Path: "store/MySQLStore.java", Language: "java", ContentHash: "d2", ParseStatus: "ok"}
	implID, _ := w.AddFile(ctx, &impl)
	caller := model.File{Path: "svc/Checkout.java", Language: "java", ContentHash: "d3", ParseStatus: "ok"}
	callerID, _ := w.AddFile(ctx, &caller)

	target := model.Symbol{
		FileID: ifaceID, Kind: model.KindMethod, Name: "persist",
		FQN: "store.Store#persist", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 5}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	targetID, _ := w.AddSymbol(ctx, &target)

	implMethod := model.Symbol{
		FileID: implID, Kind: model.KindMethod, Name: "persistMySQL",
		FQN: "store.MySQLStore#persistMySQL", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 12}, End: model.Position{Line: 20}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	implID2, _ := w.AddSymbol(ctx, &implMethod)

	callSite := model.Symbol{
		FileID: callerID, Kind: model.KindMethod, Name: "checkout",
		FQN: "svc.Checkout#checkout", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 7}, End: model.Position{Line: 9}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	callerSymID, _ := w.AddSymbol(ctx, &callSite)

	_ = w.Flush(ctx)
	_ = w.Close()

	_, _ = reg.AddResolvedEdges(ctx, runID, []model.Edge{
		{
			SrcID: callerSymID, DstID: targetID, Kind: model.EdgeCalls,
			Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{
				FilePath: caller.Path,
				Range:    model.Range{Start: model.Position{Line: 8}},
				Source:   model.SourceSourceCode,
				Detail:   "receiver type matches",
			},
		},
		{
			// IMPLEMENTS is recorded src=implementor, dst=interface.
			SrcID: implID2, DstID: targetID, Kind: model.EdgeImplements,
			Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{
				FilePath: impl.Path,
				Range:    model.Range{Start: model.Position{Line: 12}},
				Source:   model.SourceSourceCode,
				Detail:   "declared implements",
			},
		},
	})
}

// TestImpactAnalysisSurfacesDispatchOverMCP is the end-to-end version of this
// file's claim: an agent calling the tool over the wire sees the implementation.
//
// Before this, a dispatch-reached symbol existed in the report and reached the
// CLI but was dropped at the MCP boundary, so an agent asking for a blast
// radius was silently shown a smaller one than RepoCortex had computed.
func TestImpactAnalysisSurfacesDispatchOverMCP(t *testing.T) {
	sess := newGraphTestServer(t, seedDispatchGraph)
	text := resultText(callTool(t, sess, "impact_analysis", map[string]any{"symbol": "store.Store#persist"}))

	if !strings.Contains(text, "checkout") {
		t.Errorf("direct caller missing:\n%s", text)
	}
	if !strings.Contains(text, "persistMySQL") {
		t.Fatalf("dispatch-reached implementation never reaches the agent:\n%s", text)
	}
	if !strings.Contains(text, "- dispatch persistMySQL") {
		t.Errorf("implementation is not labelled as dispatch-reached:\n%s", text)
	}
	if !strings.Contains(text, "NOT a direct call") {
		t.Errorf("dispatch row does not say how it was reached:\n%s", text)
	}
	if !strings.Contains(text, "1 reached by interface dispatch") {
		t.Errorf("summary does not count the dispatch reach:\n%s", text)
	}
}

// dispatched builds one dispatch row for a seeded report.
func dispatched(id int64, name, file string, depth int, reason impact.DispatchReason) impact.Dispatched {
	return impact.Dispatched{
		Impacted: impact.Impacted{
			Symbol:     capability.Symbol{ID: id, Name: name, FQN: "pkg." + name, FilePath: file},
			Depth:      depth,
			Confidence: model.ConfidenceHigh,
			Via:        model.EdgeImplements,
		},
		Reason: reason,
		Detail: "seeded",
	}
}

func impacted(id int64, name, file string, depth int) impact.Impacted {
	return impact.Impacted{
		Symbol:     capability.Symbol{ID: id, Name: name, FQN: "pkg." + name, FilePath: file},
		Depth:      depth,
		Confidence: model.ConfidenceHigh,
		Via:        model.EdgeCalls,
		Evidence: []model.Evidence{{
			FilePath: file,
			Range:    model.Range{Start: model.Position{Line: 12}},
		}},
	}
}

// renderReport mirrors what impactAnalysis does with a report, minus the server
// plumbing: items, then the dispatch notes, then the central bound.
func renderReport(rep impact.Report) *Response {
	items, reach := impactItems(rep)
	resp := &Response{
		Summary: fmt.Sprintf("target: %s.", reach.summary()),
		Items:   items,
		Notes:   reach.notes(rep),
	}
	resp.Bound(DefaultLimits())
	return resp
}

// TestDispatchIsRenderedAndSaysHowItWasReached is the reason this file exists.
//
// A dispatch row carries a name and a file:line exactly like a direct caller
// does. If the rendering does not state the mechanism, an agent reads "this
// code calls the target" off a row that means "the runtime MAY execute this
// when something names the target" — a possibility presented as a fact, which
// is the one failure mode the whole Direct/Indirect/Dispatch split exists to
// prevent.
func TestDispatchIsRenderedAndSaysHowItWasReached(t *testing.T) {
	rep := impact.Report{
		Direct: []impact.Impacted{impacted(1, "CallsTarget", "a.go", 1)},
		Dispatch: []impact.Dispatched{
			dispatched(2, "MySQLStore", "store/mysql.go", 1, impact.ReasonImplementsTarget),
			dispatched(3, "ReachedViaImpl", "svc/handler.go", 3, impact.ReasonThroughImplementation),
		},
		DispatchThrough: []capability.Symbol{
			{ID: 2, Name: "MySQLStore", FilePath: "store/mysql.go"},
		},
	}

	resp := renderReport(rep)
	text := resp.RenderText()

	// (a) The dispatch-reached symbols reach the agent at all.
	for _, name := range []string{"MySQLStore", "ReachedViaImpl"} {
		if !strings.Contains(text, name) {
			t.Errorf("rendered response omits dispatch-reached symbol %q:\n%s", name, text)
		}
	}

	// (b) Every dispatch row says HOW it was reached, on the row itself. A note
	// further up the response is not enough: rows are read one at a time.
	for _, it := range resp.Items {
		if it.Kind != kindDispatch {
			continue
		}
		if !strings.Contains(it.Why, "dispatch") {
			t.Errorf("dispatch item %q does not say how it was reached: why=%q", it.Name, it.Why)
		}
		if !strings.Contains(it.Why, "NOT a direct call") {
			t.Errorf("dispatch item %q does not distinguish itself from a direct call: why=%q",
				it.Name, it.Why)
		}
		// The mechanism must survive the Why clip, or the sentence that stops
		// the misreading is the part that gets cut off.
		if len(it.Why) > DefaultLimits().MaxWhyBytes {
			t.Errorf("dispatch why for %q is %d bytes, above MaxWhyBytes=%d and will be clipped",
				it.Name, len(it.Why), DefaultLimits().MaxWhyBytes)
		}
		if strings.HasSuffix(it.Why, "…") {
			t.Errorf("dispatch why for %q was clipped: %q", it.Name, it.Why)
		}
	}

	// The kinds are distinguishable in the rendered text, not only in the struct.
	if !strings.Contains(text, "- "+kindDispatch+" MySQLStore") {
		t.Errorf("dispatch rows are not labelled as such in the rendered text:\n%s", text)
	}

	// The summary counts them; an agent that reads only the summary must not
	// believe the blast radius is one direct caller.
	if !strings.Contains(resp.Summary, "2 reached by interface dispatch") {
		t.Errorf("summary hides the dispatch reach: %q", resp.Summary)
	}
}

// TestDispatchReasonsRenderDistinctly pins that the two dispatch reasons make
// different claims in the output: being an implementation of the target, and
// merely being reachable from one, are not the same finding.
func TestDispatchReasonsRenderDistinctly(t *testing.T) {
	cases := []struct {
		name   string
		row    impact.Dispatched
		want   string
		reject string
	}{
		{
			name: "implements the target",
			row:  dispatched(2, "Impl", "impl.go", 1, impact.ReasonImplementsTarget),
			want: "implements or overrides the target",
		},
		{
			name: "reached through an implementation",
			row:  dispatched(3, "Deep", "deep.go", 3, impact.ReasonThroughImplementation),
			want: "2 hop(s) beyond an implementation",
		},
		{
			name:   "unrecognised reason still says it is dispatch",
			row:    dispatched(4, "Odd", "odd.go", 1, impact.DispatchReason("future_reason")),
			want:   "future_reason",
			reject: "implements or overrides",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			why := whyDispatched(tc.row)
			if !strings.Contains(why, tc.want) {
				t.Errorf("why = %q, want it to contain %q", why, tc.want)
			}
			if !strings.Contains(why, "NOT a direct call") {
				t.Errorf("why = %q, must lead with the mechanism", why)
			}
			if tc.reject != "" && strings.Contains(why, tc.reject) {
				t.Errorf("why = %q, must not contain %q", why, tc.reject)
			}
		})
	}
}

// TestNewCapabilityToolsAreRegistered pins that endpoint, table and
// config-key discovery are actually wired into the server, not merely
// implemented as unreachable methods.
//
// newDescriptionsTestServer (descriptions_test.go) is reused rather than a
// second construction path: it already builds a *Server on the MCPFull
// profile with the graph wired in, which is what Tools() requires to report
// the full registered surface.
func TestNewCapabilityToolsAreRegistered(t *testing.T) {
	s := newDescriptionsTestServer(t)

	registered := make(map[string]bool)
	for _, name := range s.Tools() {
		registered[name] = true
	}

	for _, want := range []string{"find_api_endpoints", "find_db_access", "get_config_key"} {
		if !registered[want] {
			t.Errorf("tool %q is not registered; have %v", want, s.Tools())
		}
	}
}

// TestSymbolInBothDispatchAndDirectRendersOnce is the no-double-count rule.
//
// impact.Analyze already keeps callers out of the dispatch expansion, so this
// input should not arise from it. The rendering still has to hold the line: a
// symbol listed twice is a blast radius counted twice, and the count is what a
// developer decides on.
func TestSymbolInBothDispatchAndDirectRendersOnce(t *testing.T) {
	both := capability.Symbol{ID: 7, Name: "Overlap", FQN: "pkg.Overlap", FilePath: "both.go"}

	direct := impacted(7, "Overlap", "both.go", 1)
	direct.Symbol = both
	disp := dispatched(7, "Overlap", "both.go", 1, impact.ReasonImplementsTarget)
	disp.Symbol = both

	rep := impact.Report{
		Direct:   []impact.Impacted{direct},
		Dispatch: []impact.Dispatched{disp, dispatched(8, "OnlyDispatch", "d.go", 1, impact.ReasonImplementsTarget)},
	}

	resp := renderReport(rep)

	var count int
	for _, it := range resp.Items {
		if it.Name == "Overlap" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Overlap rendered %d times, want exactly 1: %+v", count, resp.Items)
	}

	// The surviving row must be the STRONGER claim. Keeping the dispatch row
	// instead would under-claim a proven caller.
	for _, it := range resp.Items {
		if it.Name == "Overlap" && it.Kind != kindDirect {
			t.Errorf("Overlap kept the weaker claim: kind=%q", it.Kind)
		}
	}

	// The counts must not include the suppressed row either.
	if !strings.Contains(resp.Summary, "1 direct") || !strings.Contains(resp.Summary, "1 reached by interface dispatch") {
		t.Errorf("summary double-counts or miscounts the overlap: %q", resp.Summary)
	}

	// The overlap is stated, not silently swallowed.
	if !strings.Contains(strings.Join(resp.Notes, "\n"), "listed once") {
		t.Errorf("nothing tells the reader the sets overlapped: %v", resp.Notes)
	}
}

// TestDispatchRespectsTheByteBudget checks that a report whose dispatch section
// is large is truncated by the existing bound rather than shipped oversized.
//
// The cap is not a preference: past it the host spills the tool result to a
// file the agent must Read back, which costs more than not calling RepoCortex.
func TestDispatchRespectsTheByteBudget(t *testing.T) {
	rep := impact.Report{Direct: []impact.Impacted{impacted(1, "Caller", "a.go", 1)}}
	for i := int64(2); i < 400; i++ {
		rep.Dispatch = append(rep.Dispatch, dispatched(i,
			fmt.Sprintf("Implementation%d", i),
			fmt.Sprintf("internal/some/deeply/nested/package/path/implementation_%d.go", i),
			1, impact.ReasonImplementsTarget))
	}
	for i := int64(0); i < 20; i++ {
		rep.DispatchThrough = append(rep.DispatchThrough,
			capability.Symbol{ID: i, Name: fmt.Sprintf("Through%d", i)})
	}

	resp := renderReport(rep)

	if got := len(resp.RenderText()); got > DefaultLimits().MaxTotalBytes {
		t.Fatalf("rendered response is %d bytes, above MaxTotalBytes=%d",
			got, DefaultLimits().MaxTotalBytes)
	}
	if !resp.Truncated {
		t.Fatal("response dropped dispatch rows without saying so")
	}
	if resp.TruncatedReason == "" {
		t.Error("truncation gives no reason, so the agent cannot tell it is seeing a partial answer")
	}
	// The bound states a count, so a reader knows how much is missing.
	if !strings.ContainsAny(resp.TruncatedReason, "0123456789") && !strings.Contains(resp.TruncatedReason, "rest") {
		t.Errorf("truncation reason sizes nothing: %q", resp.TruncatedReason)
	}
	// Dispatch is the weakest claim, so it is what gets shed: the direct caller
	// must still be there.
	if resp.Items[0].Kind != kindDirect {
		t.Errorf("truncation dropped the strongest claim first: %+v", resp.Items[0])
	}
	// The note naming implementations must stay bounded, or it eats the budget
	// the items need.
	for _, n := range resp.Notes {
		if strings.Contains(n, "Through") && strings.Contains(n, "Through5") {
			t.Errorf("dispatch note names every implementation: %q", n)
		}
	}
}

// TestNoDispatchSectionStaysSilent keeps the cost of the feature at zero for
// the reports that have no dispatch reach: an empty labelled section would
// spend tokens to say nothing, and "0 dispatch" is a stronger claim than the
// index can support.
func TestNoDispatchSectionStaysSilent(t *testing.T) {
	rep := impact.Report{
		Direct:   []impact.Impacted{impacted(1, "A", "a.go", 1)},
		Indirect: []impact.Impacted{impacted(2, "B", "b.go", 2)},
	}

	resp := renderReport(rep)

	if strings.Contains(resp.Summary, "dispatch") {
		t.Errorf("summary mentions dispatch with no dispatch reach: %q", resp.Summary)
	}
	for _, n := range resp.Notes {
		if strings.Contains(n, "dispatch") {
			t.Errorf("note mentions dispatch with no dispatch reach: %q", n)
		}
	}
	if len(resp.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(resp.Items))
	}
	if resp.Items[0].Kind != kindDirect || resp.Items[1].Kind != kindIndirect {
		t.Errorf("claims are out of order, strongest first: %+v", resp.Items)
	}
}
