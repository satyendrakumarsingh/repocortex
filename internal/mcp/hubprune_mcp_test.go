package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// hubPruneSurface is the intended answer to "which tools take a hub degree
// limit", written down once, for the same reason minConfidenceSurface exists.
//
// A tool belongs here when its answer comes from a graph.Traverse call, because
// that is the only place HubDegreeLimit does anything: it refuses to expand
// THROUGH a wide node. A tool that answers from a single registry query has no
// expansion step to refuse, so advertising the parameter would tell an agent its
// blast radius was bounded when nothing was bounded at all.
var hubPruneSurface = map[string]bool{
	// Traversal-backed: each of these reaches graph.Traverse.
	"analyze_impact":   true, // impact.analyzeTarget + expandDispatch
	"find_tests":       true, // tests.FindTestsWith graph signal
	"get_task_context": true, // internal/context.expand
	"plan_change":      true, // planchange.Planner.Plan -> impact.AnalyzeWith

	// Not traversal-backed:
	//   find_callers / find_references / plan_rename — one registry query for
	//     the target's own edges. There is no second hop, so nothing to prune.
	//   find_symbol / get_symbol / search_code — name and full-text lookups.
	//   get_project_overview / get_resolution_coverage — counts about the index.
	//   list_api_endpoints / list_db_tables / get_config_key — registry reads.
	"find_callers":            false,
	"find_references":         false,
	"plan_rename":             false,
	"find_symbol":             false,
	"get_symbol":              false,
	"search_code":             false,
	"get_project_overview":    false,
	"get_resolution_coverage": false,
	"list_api_endpoints":      false,
	"list_db_tables":          false,
	"get_config_key":          false,
	"get_file_outline":        false,
	"minimize_diff":           false,
	"list_blindspots":         false,
}

// seedHubGraph builds the smallest graph in which hub pruning changes an
// answer: a target with one caller, and that caller with a fan-in far wider
// than any limit a test would set.
//
// Every edge is `high`, comfortably above analyze_impact's own medium floor,
// so the floor cannot be confused for the hub cut — the only reason a node can
// be missing here is the degree limit.
func seedHubGraph(ctx context.Context, reg *registry.Registry, runID int64) {
	const fanIn = 12

	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	defer w.Close()

	addSym := func(path, name string) int64 {
		f := model.File{Path: path, Language: "go", ContentHash: "h" + name, ParseStatus: "ok"}
		fileID, _ := w.AddFile(ctx, &f)
		s := model.Symbol{
			FileID: fileID, Kind: model.KindFunction, Name: name,
			FQN: "hub." + name, Exported: true,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		id, _ := w.AddSymbol(ctx, &s)
		return id
	}

	targetID := addSym("hub/target.go", "Target")
	hubID := addSym("hub/hub.go", "Hub")
	callers := make([]int64, 0, fanIn)
	for i := range fanIn {
		name := "Caller" + string(rune('A'+i))
		callers = append(callers, addSym("hub/"+name+".go", name))
	}

	_ = w.Flush(ctx)
	_ = w.Close()

	edges := []model.Edge{{
		SrcID: hubID, DstID: targetID, Kind: model.EdgeCalls,
		Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{
			FilePath: "hub/hub.go",
			Range:    model.Range{Start: model.Position{Line: 1}},
			Source:   model.SourceSourceCode,
		},
	}}
	for _, c := range callers {
		edges = append(edges, model.Edge{
			SrcID: c, DstID: hubID, Kind: model.EdgeCalls,
			Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{
				FilePath: "hub/caller.go",
				Range:    model.Range{Start: model.Position{Line: 1}},
				Source:   model.SourceSourceCode,
			},
		})
	}
	_, _ = reg.AddResolvedEdges(ctx, runID, edges)
}

// seedCostOrderGraph gives one target two direct callers whose traversal COST
// order is the reverse of their symbol-id order.
//
// Weak is inserted first, so it holds the lower id; Strong is reached across an
// exact edge and therefore costs less. Any ordering keyed on (depth, id) puts
// Weak first, which is exactly the bug this fixture exists to detect.
func seedCostOrderGraph(ctx context.Context, reg *registry.Registry, runID int64) {
	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	defer w.Close()

	addSym := func(name string) int64 {
		f := model.File{Path: "cost/" + name + ".go", Language: "go", ContentHash: "c" + name, ParseStatus: "ok"}
		fileID, _ := w.AddFile(ctx, &f)
		s := model.Symbol{
			FileID: fileID, Kind: model.KindFunction, Name: name,
			FQN: "cost." + name, Exported: true,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		id, _ := w.AddSymbol(ctx, &s)
		return id
	}

	targetID := addSym("Subject")
	weakID := addSym("WeakCaller")
	strongID := addSym("StrongCaller")

	_ = w.Flush(ctx)
	_ = w.Close()

	edge := func(src int64, conf model.Confidence, src2 model.SourceType) model.Edge {
		return model.Edge{
			SrcID: src, DstID: targetID, Kind: model.EdgeCalls, Confidence: conf,
			Evidence: model.Evidence{
				FilePath: "cost/edge.go",
				Range:    model.Range{Start: model.Position{Line: 1}},
				Source:   src2,
			},
		}
	}
	_, _ = reg.AddResolvedEdges(ctx, runID, []model.Edge{
		edge(weakID, model.ConfidenceMedium, model.SourceSourceCode),
		edge(strongID, model.ConfidenceExact, model.SourceCompiler),
	})
}

// advertisedParam reports, per registered tool, whether the JSON Schema an
// agent actually receives declares the named property. Same mechanism as
// advertisedMinConfidence, generalised over the property name.
func advertisedParam(t *testing.T, sess *sdk.ClientSession, param string) map[string]bool {
	t.Helper()
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("the test server registered no tools; this test would pass vacuously")
	}
	out := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatalf("unmarshal %s schema: %v", tool.Name, err)
		}
		_, declared := schema.Properties[param]
		out[tool.Name] = declared
	}
	return out
}

// TestHubDegreeLimitIsAdvertisedExactlyWhereItCanAct pins the surface in both
// directions: every registered tool is accounted for in hubPruneSurface, and
// the schema declares hub_degree_limit on exactly the traversal-backed ones.
//
// The false half is the load-bearing one. A parameter that an agent can pass
// and that changes nothing is worse than an absent parameter, because the agent
// believes it bounded a walk that was never bounded.
func TestHubDegreeLimitIsAdvertisedExactlyWhereItCanAct(t *testing.T) {
	sess := newGraphTestServer(t, seedHubGraph)
	advertised := advertisedParam(t, sess, "hub_degree_limit")

	for name, declared := range advertised {
		want, recorded := hubPruneSurface[name]
		if !recorded {
			t.Fatalf("tool %q is registered but hubPruneSurface records no decision for it — add one", name)
		}
		if declared != want {
			t.Errorf("%s: hub_degree_limit advertised=%v, want %v", name, declared, want)
		}
	}
	for name := range hubPruneSurface {
		if _, ok := advertised[name]; !ok {
			t.Errorf("hubPruneSurface names %q, which the server does not register", name)
		}
	}
}

// TestPruningIsReportedInTheResponse is the honesty requirement: a walk that
// declined to explore somewhere must say so, name the limit, and name the way
// out.
func TestPruningIsReportedInTheResponse(t *testing.T) {
	sess := newGraphTestServer(t, seedHubGraph)
	text := resultText(callTool(t, sess, "analyze_impact", map[string]any{
		"symbol": "Target", "hub_degree_limit": 3,
	}))

	if !strings.Contains(text, "truncated:") {
		t.Errorf("pruning cut the walk but the response is not marked truncated:\n%s", text)
	}
	for _, want := range []string{"hub(s) wider than 3 edges", "raise hub_degree_limit", "narrower symbol"} {
		if !strings.Contains(text, want) {
			t.Errorf("response does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "CallerA") {
		t.Errorf("a node beyond the pruned hub still reached the agent:\n%s", text)
	}
}

// TestOmittingHubDegreeLimitChangesNothing is the compatibility proof, and it
// is a proof rather than an assertion because the fixture is one where pruning
// demonstrably WOULD have fired: the un-pruned call is compared byte for byte
// against an explicit zero, and both are compared against the pruned form to
// show the fixture is not vacuous.
func TestOmittingHubDegreeLimitChangesNothing(t *testing.T) {
	sess := newGraphTestServer(t, seedHubGraph)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"analyze_impact", map[string]any{"symbol": "Target"}},
		{"find_tests", map[string]any{"symbol": "Target"}},
		{"get_task_context", map[string]any{"task": "change the hub target function"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			absent := resultText(callTool(t, sess, tc.tool, tc.args))

			explicitZero := make(map[string]any, len(tc.args)+1)
			for k, v := range tc.args {
				explicitZero[k] = v
			}
			explicitZero["hub_degree_limit"] = 0
			zero := resultText(callTool(t, sess, tc.tool, explicitZero))

			if absent != zero {
				t.Errorf("omitting hub_degree_limit differs from passing 0:\n--absent--\n%s\n--zero--\n%s",
					absent, zero)
			}
		})
	}

	// Not vacuous: the same fixture under a limit returns something different.
	pruned := resultText(callTool(t, sess, "analyze_impact", map[string]any{
		"symbol": "Target", "hub_degree_limit": 3,
	}))
	unpruned := resultText(callTool(t, sess, "analyze_impact", map[string]any{"symbol": "Target"}))
	if pruned == unpruned {
		t.Fatal("the fixture does not prune at all, so the comparison above proves nothing")
	}
	if len(pruned) >= len(unpruned) {
		t.Errorf("pruned response is not smaller: %d vs %d bytes", len(pruned), len(unpruned))
	}
}

// TestItemsFollowTraversalCostOrder checks that the cheapest-first ordering
// internal/graph establishes survives the journey into Response.Items.
func TestItemsFollowTraversalCostOrder(t *testing.T) {
	sess := newGraphTestServer(t, seedCostOrderGraph)
	text := resultText(callTool(t, sess, "analyze_impact", map[string]any{"symbol": "Subject"}))

	strong := strings.Index(text, "StrongCaller")
	weak := strings.Index(text, "WeakCaller")
	if strong < 0 || weak < 0 {
		t.Fatalf("both callers should be reported:\n%s", text)
	}
	if strong > weak {
		t.Errorf("items are not ordered cheapest-first: the exact-edge caller is listed after the "+
			"medium-edge one:\n%s", text)
	}
}

// TestNegativeHubDegreeLimitIsRejected: a negative limit cannot mean anything,
// and silently treating it as "off" would hide a caller's bug behind a wider
// answer than it asked for.
func TestNegativeHubDegreeLimitIsRejected(t *testing.T) {
	sess := newGraphTestServer(t, seedHubGraph)
	for _, tool := range []string{"analyze_impact", "find_tests", "get_task_context"} {
		t.Run(tool, func(t *testing.T) {
			args := map[string]any{"hub_degree_limit": -1}
			if tool == "get_task_context" {
				args["task"] = "change the hub target function"
			} else {
				args["symbol"] = "hub.Target"
			}
			text := resultText(callTool(t, sess, tool, args))
			if !strings.Contains(text, "hub_degree_limit") {
				t.Errorf("a negative limit was not rejected with an actionable message:\n%s", text)
			}
		})
	}
}
