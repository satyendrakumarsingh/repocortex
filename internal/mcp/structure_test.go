package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/hotspots"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fullServer is a fully wired server on the `full` profile, returned both as
// the *Server (for NewInvoker and Tools) and as a connected protocol client.
type fullServer struct {
	srv  *Server
	sess *sdk.ClientSession
}

type fullOption func(*Deps)

func withHistory(h hotspots.History) fullOption { return func(d *Deps) { d.History = h } }
func withAudit(a *AuditLog) fullOption          { return func(d *Deps) { d.Audit = a } }
func withProfile(p config.MCPProfile) fullOption {
	return func(d *Deps) { d.Config.MCPProfile = p }
}
func withJudgeable(f func(string) bool) fullOption { return func(d *Deps) { d.Judgeable = f } }

func newFullServer(t *testing.T, seed func(context.Context, *registry.Registry, int64), opts ...fullOption) fullServer {
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
	cfg.MCPProfile = config.MCPFull
	deps := Deps{Registry: reg, Graph: graph.New(db), Config: cfg}
	for _, o := range opts {
		o(&deps)
	}

	srv, err := New(deps)
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
	return fullServer{srv: srv, sess: sess}
}

// seedChain builds entry -> middle -> leaf, an uncalled unexported helper, and
// an exported function nothing calls.
func seedChain(ctx context.Context, reg *registry.Registry, runID int64) {
	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	f := model.File{Path: "svc/flow.go", Language: "go", ContentHash: "c1", ParseStatus: "ok", SizeBytes: 4000}
	fid, _ := w.AddFile(ctx, &f)
	g := model.File{Path: "svc/store.go", Language: "go", ContentHash: "c2", ParseStatus: "ok", SizeBytes: 800}
	gid, _ := w.AddFile(ctx, &g)

	sym := func(fileID int64, name string, line, end int, exported bool) int64 {
		s := model.Symbol{
			FileID: fileID, Kind: model.KindFunction, Name: name, FQN: "svc." + name, Exported: exported,
			Range:      model.Range{Start: model.Position{Line: line}, End: model.Position{Line: end}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		id, _ := w.AddSymbol(ctx, &s)
		return id
	}
	entry := sym(fid, "entry", 1, 10, true)
	middle := sym(fid, "middle", 20, 30, false)
	leaf := sym(gid, "leaf", 1, 5, false)
	sym(fid, "orphanHelper", 40, 90, false)
	sym(gid, "PublicUnused", 10, 12, true)
	_ = w.Flush(ctx)
	_ = w.Close()

	edge := func(src, dst int64, file string, line int, conf model.Confidence) model.Edge {
		return model.Edge{SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: conf,
			Evidence: model.Evidence{FilePath: file, Range: model.Range{Start: model.Position{Line: line}},
				Source: model.SourceSourceCode, Detail: "same package"}}
	}
	_, _ = reg.AddResolvedEdges(ctx, runID, []model.Edge{
		edge(entry, middle, f.Path, 5, model.ConfidenceHigh),
		edge(middle, leaf, f.Path, 25, model.ConfidenceMedium),
	})
}

func TestFullProfileExposesTheStructureTools(t *testing.T) {
	fs := newFullServer(t, seedChain)
	have := map[string]bool{}
	for _, name := range fs.srv.Tools() {
		have[name] = true
	}
	for _, want := range FullOnlyTools() {
		if !have[want] {
			t.Errorf("full profile does not expose %q", want)
		}
	}
}

// The standard profile is a size its users chose. Adding tools must not grow
// it behind their backs.
func TestStandardProfileDoesNotGainTheStructureTools(t *testing.T) {
	fs := newFullServer(t, seedChain, withProfile(config.MCPStandard))
	for _, name := range fs.srv.Tools() {
		for _, fullOnly := range FullOnlyTools() {
			if name == fullOnly {
				t.Errorf("standard profile exposes full-only tool %q", name)
			}
		}
	}
	_, standard := ProfileTools()
	if got := len(fs.srv.Tools()); got != len(standard) {
		t.Errorf("standard exposes %d tools, its list names %d", got, len(standard))
	}
}

func TestFindCalleesListsWhatASymbolCalls(t *testing.T) {
	fs := newFullServer(t, seedChain)
	res := callTool(t, fs.sess, "find_callees", map[string]any{"name": "entry"})
	text := resultText(res)

	if res.IsError {
		t.Fatalf("find_callees failed:\n%s", text)
	}
	if !strings.Contains(text, "middle") {
		t.Errorf("callee missing:\n%s", text)
	}
	if strings.Contains(text, "leaf") {
		t.Errorf("find_callees is one hop; leaf is two away:\n%s", text)
	}
	if !strings.Contains(text, "INSIDE this index") {
		t.Errorf("the library-call limit is not stated:\n%s", text)
	}
}

func TestFindCalleesHonoursTheConfidenceFloor(t *testing.T) {
	fs := newFullServer(t, seedChain)
	text := resultText(callTool(t, fs.sess, "find_callees", map[string]any{"name": "middle", "min_confidence": "high"}))
	if strings.Contains(text, "- calls leaf") {
		t.Errorf("a medium edge passed a high floor:\n%s", text)
	}
}

func TestFindImplementationsReportsBothDirections(t *testing.T) {
	fs := newFullServer(t, seedDispatchGraph)

	down := resultText(callTool(t, fs.sess, "find_implementations", map[string]any{"name": "persist"}))
	if !strings.Contains(down, "persistMySQL") || !strings.Contains(down, "subtype") {
		t.Errorf("the implementer is missing or unlabelled:\n%s", down)
	}

	up := resultText(callTool(t, fs.sess, "find_implementations", map[string]any{"name": "persistMySQL"}))
	if !strings.Contains(up, "supertype") || !strings.Contains(up, "persist") {
		t.Errorf("the supertype is missing or unlabelled:\n%s", up)
	}
	if !strings.Contains(up, "none declared") {
		t.Errorf("the structural-typing limit is not stated:\n%s", up)
	}
}

func TestTraceCallPathReturnsEveryHopWithEvidence(t *testing.T) {
	fs := newFullServer(t, seedChain)
	res := callTool(t, fs.sess, "trace_call_path", map[string]any{"from": "entry", "to": "leaf"})
	text := resultText(res)

	if res.IsError {
		t.Fatalf("trace_call_path failed:\n%s", text)
	}
	for _, want := range []string{"entry -> middle", "middle -> leaf", "2 hop(s)", "hop 1 of 2", "hop 2 of 2", "svc/flow.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	// The weakest hop is medium, and the agent must be able to see that.
	if !strings.Contains(text, "medium") {
		t.Errorf("the medium hop's confidence is not shown:\n%s", text)
	}
}

func TestTraceCallPathNoRouteIsNotProof(t *testing.T) {
	fs := newFullServer(t, seedChain)
	// The graph is directed: leaf does not reach entry.
	res := callTool(t, fs.sess, "trace_call_path", map[string]any{"from": "leaf", "to": "entry"})
	text := resultText(res)

	if res.IsError {
		t.Fatalf("no route is an answer, not a failure:\n%s", text)
	}
	if !strings.Contains(text, "No route found") || !strings.Contains(text, "not proof") {
		t.Errorf("an empty route must say it proves nothing:\n%s", text)
	}
}

func TestTraceCallPathRejectsBadArguments(t *testing.T) {
	fs := newFullServer(t, seedChain)
	if res := callTool(t, fs.sess, "trace_call_path", map[string]any{"from": "entry", "to": " "}); !res.IsError {
		t.Error("an empty 'to' must be rejected")
	}
	if res := callTool(t, fs.sess, "trace_call_path", map[string]any{"from": "entry", "to": "leaf", "max_depth": -1}); !res.IsError {
		t.Error("a negative max_depth must be rejected")
	}
	// A missing symbol is guidance, not a failure.
	res := callTool(t, fs.sess, "trace_call_path", map[string]any{"from": "entry", "to": "noSuchSymbol"})
	if res.IsError || !strings.Contains(resultText(res), "No symbol named") {
		t.Errorf("a missing symbol should be guidance:\n%s", resultText(res))
	}
}

func TestFindDeadCodeGradesAndWarns(t *testing.T) {
	fs := newFullServer(t, seedChain)
	res := callTool(t, fs.sess, "list_dead_code", map[string]any{})
	text := resultText(res)

	if res.IsError {
		t.Fatalf("list_dead_code failed:\n%s", text)
	}
	if !strings.Contains(text, "orphanHelper") || !strings.Contains(text, "likely unused") {
		t.Errorf("the unexported, uncalled helper should be listed as likely unused:\n%s", text)
	}
	if strings.Contains(text, "PublicUnused") {
		t.Errorf("exported definitions are off by default:\n%s", text)
	}
	if strings.Contains(text, "- function middle") || strings.Contains(text, "- function leaf") {
		t.Errorf("a called function was listed as dead:\n%s", text)
	}
	if !strings.Contains(text, "not proven unused") {
		t.Errorf("the warning against deleting on this evidence is missing:\n%s", text)
	}
	// The verdict grades edge confidence; a dead-code row has no edge.
	if strings.Contains(text, "VERDICT") {
		t.Errorf("a dead-code answer must not carry an edge-confidence verdict:\n%s", text)
	}
}

func TestFindDeadCodeIncludesExportedWhenAsked(t *testing.T) {
	fs := newFullServer(t, seedChain)
	text := resultText(callTool(t, fs.sess, "list_dead_code", map[string]any{"include_exported": true}))
	if !strings.Contains(text, "PublicUnused") || !strings.Contains(text, "possibly unused") {
		t.Errorf("an exported candidate should be listed as possibly unused:\n%s", text)
	}

	only := resultText(callTool(t, fs.sess, "list_dead_code", map[string]any{"include_exported": true, "min_certainty": "likely"}))
	if strings.Contains(only, "PublicUnused") {
		t.Errorf("min_certainty=likely must drop possible candidates:\n%s", only)
	}
	if res := callTool(t, fs.sess, "list_dead_code", map[string]any{"min_certainty": "certain"}); !res.IsError {
		t.Error("an unknown min_certainty must be rejected")
	}
}

func TestFindDeadCodeSkipsUnjudgedLanguages(t *testing.T) {
	fs := newFullServer(t, seedChain, withJudgeable(func(string) bool { return false }))
	text := resultText(callTool(t, fs.sess, "list_dead_code", map[string]any{}))
	if strings.Contains(text, "- function orphanHelper") {
		t.Errorf("a language the index cannot judge must not produce candidates:\n%s", text)
	}
	if !strings.Contains(text, "were not judged") {
		t.Errorf("the unjudged languages must be reported:\n%s", text)
	}
}

type fixedHistory []hotspots.Commit

func (f fixedHistory) Commits(_ context.Context, limit int) ([]hotspots.Commit, error) {
	if len(f) > limit {
		return f[:limit], nil
	}
	return f, nil
}

func TestFindHotspotsRanksAndReportsCoupling(t *testing.T) {
	hist := fixedHistory{
		{SHA: "a", When: 1_700_000_000, Paths: []string{"svc/flow.go", "svc/store.go"}},
		{SHA: "b", When: 1_700_100_000, Paths: []string{"svc/flow.go", "svc/store.go"}},
		{SHA: "c", When: 1_700_200_000, Paths: []string{"svc/flow.go", "svc/store.go"}},
		{SHA: "d", When: 1_700_300_000, Paths: []string{"svc/flow.go"}},
	}
	fs := newFullServer(t, seedChain, withHistory(hist))
	res := callTool(t, fs.sess, "list_hotspots", map[string]any{})
	text := resultText(res)

	if res.IsError {
		t.Fatalf("list_hotspots failed:\n%s", text)
	}
	flow, storeAt := strings.Index(text, "- file flow.go"), strings.Index(text, "- file store.go")
	if flow < 0 || storeAt < 0 || flow > storeAt {
		t.Errorf("svc/flow.go (4 commits, 4000 bytes) should rank above svc/store.go:\n%s", text)
	}
	for _, want := range []string{"score 100", "4 commit(s)", "- changes_with store.go  svc/store.go",
		"committed with flow.go in 3 of its 4 commit(s)", "analyze_impact cannot see"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
}

// A coupled file must survive long paths: it is a row of its own, so the
// per-item byte bound cannot cut it off the end of the hotspot's reason.
func TestFindHotspotsCouplingSurvivesLongPaths(t *testing.T) {
	long := strings.Repeat("deeply/nested/", 12)
	a, b := long+"alpha_service.go", long+"beta_repository.go"
	seed := func(ctx context.Context, reg *registry.Registry, runID int64) {
		w, err := reg.NewWriter(ctx, runID)
		if err != nil {
			panic(err)
		}
		for i, p := range []string{a, b} {
			f := model.File{Path: p, Language: "go", ContentHash: p, ParseStatus: "ok", SizeBytes: int64(1000 * (2 - i))}
			fid, _ := w.AddFile(ctx, &f)
			s := model.Symbol{FileID: fid, Kind: model.KindFunction, Name: "f", FQN: p + "#f",
				Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode}
			_, _ = w.AddSymbol(ctx, &s)
		}
		_ = w.Flush(ctx)
		_ = w.Close()
	}
	hist := fixedHistory{{SHA: "1", Paths: []string{a, b}}, {SHA: "2", Paths: []string{a, b}}, {SHA: "3", Paths: []string{a, b}}}
	fs := newFullServer(t, seed, withHistory(hist))
	text := resultText(callTool(t, fs.sess, "list_hotspots", map[string]any{}))
	if !strings.Contains(text, "- changes_with beta_repository.go  "+b) {
		t.Errorf("the coupled file's full path is missing:\n%s", text)
	}
}

func TestFindHotspotsWithoutHistoryIsGuidance(t *testing.T) {
	fs := newFullServer(t, seedChain)
	res := callTool(t, fs.sess, "list_hotspots", map[string]any{})
	if res.IsError {
		t.Fatalf("no history is a state of the world, not a malfunction:\n%s", resultText(res))
	}
	if !strings.Contains(resultText(res), "No commit history") {
		t.Errorf("the answer should say why nothing was ranked:\n%s", resultText(res))
	}
}

// ---------------------------------------------------------------- naming

func TestEveryToolNameFollowsTheConvention(t *testing.T) {
	fs := newFullServer(t, seedChain)
	for _, name := range fs.srv.Tools() {
		if !ValidToolName(name) {
			t.Errorf("tool %q breaks the naming convention: want <verb>_<noun> with a verb from %v",
				name, ToolVerbs())
		}
	}
	for _, name := range fs.srv.Prompts() {
		if !ValidToolName(name) {
			t.Errorf("prompt %q breaks the naming convention", name)
		}
	}
}

func TestValidToolName(t *testing.T) {
	for name, want := range map[string]bool{
		"find_callers": true, "get_task_context": true, "analyze_impact": true, "trace_call_path": true,
		"list_hotspots": true, "list_api_endpoints": true,
		"impact_analysis": false, "resolution_coverage": false, // noun phrases
		"detect_blindspots": false, // not one of the verbs
		"findCallers":       false, "find": false, "find_": false, "Find_callers": false,
		"find__callers": false, "find_callers_": false, "do_thing": false, "find-callers": false, "": false,
	} {
		if got := ValidToolName(name); got != want {
			t.Errorf("ValidToolName(%q) = %v, want %v", name, got, want)
		}
	}
}

// ---------------------------------------------------------------- guidance

func TestInstructionsAreShortAndNameOnlyAvailableTools(t *testing.T) {
	for _, p := range []config.MCPProfile{config.MCPMinimal, config.MCPStandard, config.MCPFull} {
		text := serverInstructions(p)
		if len(text) > maxInstructionBytes {
			t.Errorf("%s: instructions are %d bytes, over the %d budget every session pays", p, len(text), maxInstructionBytes)
		}
		for _, must := range []string{"get_task_context", "exact", "never that nothing exists"} {
			if !strings.Contains(text, must) {
				t.Errorf("%s: instructions do not mention %q", p, must)
			}
		}
	}
	if strings.Contains(serverInstructions(config.MCPMinimal), "analyze_impact") {
		t.Error("the minimal profile does not expose analyze_impact; its instructions must not name it")
	}
	if !strings.Contains(serverInstructions(config.MCPStandard), "analyze_impact") {
		t.Error("the standard profile exposes analyze_impact; its instructions should say when to use it")
	}
}

func TestInstructionsReachTheClient(t *testing.T) {
	fs := newFullServer(t, seedChain)
	init := fs.sess.InitializeResult()
	if init == nil || !strings.Contains(init.Instructions, "get_task_context") {
		t.Errorf("initialize result carries no instructions: %+v", init)
	}
}

func TestPromptsBuildWorkflowsFromAvailableTools(t *testing.T) {
	ctx := context.Background()

	full := newFullServer(t, seedChain)
	got, err := full.sess.GetPrompt(ctx, &sdk.GetPromptParams{Name: "plan_refactor", Arguments: map[string]string{"symbol": "leaf"}})
	if err != nil {
		t.Fatal(err)
	}
	text := got.Messages[0].Content.(*sdk.TextContent).Text
	for _, want := range []string{`"leaf"`, "find_callers", "analyze_impact", "find_implementations", "Do not edit any file yet"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan_refactor on full is missing %q:\n%s", want, text)
		}
	}

	minimal := newFullServer(t, seedChain, withProfile(config.MCPMinimal))
	got, err = minimal.sess.GetPrompt(ctx, &sdk.GetPromptParams{Name: "plan_refactor", Arguments: map[string]string{"symbol": "leaf"}})
	if err != nil {
		t.Fatal(err)
	}
	text = got.Messages[0].Content.(*sdk.TextContent).Text
	if strings.Contains(text, "analyze_impact") || strings.Contains(text, "find_implementations") {
		t.Errorf("plan_refactor on minimal names a tool that profile does not expose:\n%s", text)
	}

	if _, err := full.sess.GetPrompt(ctx, &sdk.GetPromptParams{Name: "plan_refactor"}); err == nil {
		t.Error("a missing required argument must be an error")
	}
	if len(full.srv.Prompts()) != len(promptSpecs()) {
		t.Errorf("registered %d prompts, specs define %d", len(full.srv.Prompts()), len(promptSpecs()))
	}
}

// ---------------------------------------------------------------- invoker

// The REST endpoint and the CLI go through the invoker, so its answer must be
// the protocol's answer.
func TestInvokerAnswersExactlyAsTheProtocolDoes(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()

	inv, err := fs.srv.NewInvoker(ctx, "rest")
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"find_callers", map[string]any{"name": "leaf"}},
		{"find_callees", map[string]any{"name": "entry"}},
		{"trace_call_path", map[string]any{"from": "entry", "to": "leaf"}},
		{"list_dead_code", map[string]any{}},
		{"get_project_overview", map[string]any{}},
	} {
		raw, _ := json.Marshal(tc.args)
		got, err := inv.Call(ctx, tc.tool, raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		// A fresh protocol session, so neither side has dedup history.
		want := resultText(callTool(t, newFullServer(t, seedChain).sess, tc.tool, tc.args))
		if stripVolatile(got.Text) != stripVolatile(want) {
			t.Errorf("%s: invoker and protocol disagree\n--- invoker\n%s\n--- protocol\n%s", tc.tool, got.Text, want)
		}
		if len(got.Structured) == 0 {
			t.Errorf("%s: no structured result", tc.tool)
		}
	}
}

// stripVolatile removes the index-age line, which differs between two servers
// built a moment apart.
func stripVolatile(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "second(s) ago") || strings.Contains(line, "indexed ") && strings.Contains(line, " ago") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// A front-end session is not a conversation: asking the same question twice
// must return the same full answer, never a pointer back at the first.
func TestInvokerIsStateless(t *testing.T) {
	fs := newFullServer(t, seedPaymentGraph)
	ctx := context.Background()
	inv, err := fs.srv.NewInvoker(ctx, "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()

	args := json.RawMessage(`{"query":"handleCallback"}`)
	first, err := inv.Call(ctx, "search_code", args)
	if err != nil {
		t.Fatal(err)
	}
	second, err := inv.Call(ctx, "search_code", args)
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != second.Text {
		t.Errorf("the second identical call was answered differently:\n--- first\n%s\n--- second\n%s", first.Text, second.Text)
	}
}

func TestInvokerToolsReportTheirCost(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()
	inv, err := fs.srv.NewInvoker(ctx, "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()

	tools, err := inv.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(fs.srv.Tools()) {
		t.Fatalf("invoker lists %d tools, server registered %d", len(tools), len(fs.srv.Tools()))
	}
	for i, tool := range tools {
		if i > 0 && tools[i-1].Name >= tool.Name {
			t.Errorf("tools are not sorted: %s before %s", tools[i-1].Name, tool.Name)
		}
		if tool.SchemaBytes <= len(tool.Description) || tool.EstimatedTokens == 0 {
			t.Errorf("%s: schema cost %d bytes / %d tokens is not plausible", tool.Name, tool.SchemaBytes, tool.EstimatedTokens)
		}
		if !json.Valid(tool.InputSchema) {
			t.Errorf("%s: input schema is not JSON", tool.Name)
		}
	}
}

func TestInvokerRejectsUnknownToolsAndBadArguments(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()
	inv, err := fs.srv.NewInvoker(ctx, "rest")
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()

	if _, err := inv.Call(ctx, "delete_everything", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("an unknown tool should return ErrUnknownTool, got %v", err)
	}
	if _, err := inv.Call(ctx, "find_callers", json.RawMessage(`["not","an","object"]`)); err == nil {
		t.Error("non-object arguments must be rejected")
	}
	// There are no aliases: a name the server never registered is unknown.
	if _, err := inv.Call(ctx, "impact_analysis", json.RawMessage(`{"symbol":"leaf"}`)); err == nil {
		t.Error("an unregistered name was accepted; tools have one name each")
	}
}

// ---------------------------------------------------------------- audit

func TestAuditLogRecordsEveryCallWithoutTheAnswer(t *testing.T) {
	var buf bytes.Buffer
	fs := newFullServer(t, seedPaymentGraph, withAudit(NewAuditLog(&buf)))
	ctx := context.Background()

	callTool(t, fs.sess, "analyze_impact", map[string]any{"symbol": "updateStatus"})
	callTool(t, fs.sess, "find_callers", map[string]any{"name": " "}) // rejected: a tool error

	inv, err := fs.srv.NewInvoker(ctx, "rest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.Call(ctx, "search_code", json.RawMessage(`{"query":"handleCallback"}`)); err != nil {
		t.Fatal(err)
	}
	_ = inv.Close()

	var events []AuditEvent
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit line is not JSON: %q: %v", line, err)
		}
		events = append(events, ev)
	}
	if len(events) != 3 {
		t.Fatalf("recorded %d events, want 3:\n%s", len(events), buf.String())
	}

	if events[0].Tool != "analyze_impact" {
		t.Errorf("first event names %q, want analyze_impact", events[0].Tool)
	}
	if events[0].Transport != "mcp" || events[0].Outcome != "ok" || events[0].ResultBytes == 0 {
		t.Errorf("first event = %+v", events[0])
	}
	if !strings.Contains(string(events[0].Arguments), "updateStatus") {
		t.Errorf("arguments were not recorded: %s", events[0].Arguments)
	}
	if events[1].Outcome != "tool_error" {
		t.Errorf("a rejected call should be recorded as tool_error, got %q", events[1].Outcome)
	}
	if events[2].Transport != "rest" {
		t.Errorf("an invoker call should be attributed to its front end, got %q", events[2].Transport)
	}
	// The answer is repository content and must not be copied into the log.
	if strings.Contains(buf.String(), "public class CallbackController") {
		t.Errorf("the audit log contains source text:\n%s", buf.String())
	}
}

func TestAuditLogTruncatesLargeArguments(t *testing.T) {
	var buf bytes.Buffer
	fs := newFullServer(t, seedChain, withAudit(NewAuditLog(&buf)))
	callTool(t, fs.sess, "get_task_context", map[string]any{"task": strings.Repeat("payment callback ", 400)})

	var ev AuditEvent
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &ev); err != nil {
		t.Fatalf("audit line is not JSON: %v\n%s", err, buf.String())
	}
	if !ev.ArgsCut {
		t.Error("oversized arguments should be marked truncated")
	}
	if len(ev.Arguments) > auditArgBytes+512 {
		t.Errorf("arguments were not cut: %d bytes kept", len(ev.Arguments))
	}
}

func TestNilAuditLogIsSafe(t *testing.T) {
	var a *AuditLog
	a.Record(AuditEvent{Tool: "x"}) // must not panic
}
