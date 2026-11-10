package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestServer builds a server over a seeded in-memory index and connects a
// real MCP client to it.
//
// A real client over a real transport, not a direct handler call: the schemas,
// argument marshalling and result encoding are the contract an agent depends
// on, and calling handlers directly would test everything except that contract.
func newTestServer(t *testing.T, seed func(context.Context, *registry.Registry, int64)) *sdk.ClientSession {
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

	srv, err := New(Deps{Registry: reg, Config: cfg})
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

func seedPaymentGraph(ctx context.Context, reg *registry.Registry, runID int64) {
	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	defer w.Close()

	ctrl := model.File{Path: "api/CallbackController.java", Language: "java", ContentHash: "h1", ParseStatus: "ok"}
	ctrlID, _ := w.AddFile(ctx, &ctrl)
	svc := model.File{Path: "svc/TransactionService.java", Language: "java", ContentHash: "h2", ParseStatus: "ok"}
	svcID, _ := w.AddFile(ctx, &svc)

	handle := model.Symbol{
		FileID: ctrlID, Kind: model.KindMethod, Name: "handleCallback",
		FQN: "api.CallbackController#handleCallback", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 10}, End: model.Position{Line: 20}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	handleID, _ := w.AddSymbol(ctx, &handle)

	update := model.Symbol{
		FileID: svcID, Kind: model.KindMethod, Name: "updateStatus",
		FQN: "svc.TransactionService#updateStatus", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 30}, End: model.Position{Line: 45}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	updateID, _ := w.AddSymbol(ctx, &update)

	_ = w.IndexFileText(ctx, ctrlID, ctrl.Path, "public class CallbackController {\n  void handleCallback() { svc.updateStatus(); }\n}")
	_ = w.Flush(ctx)
	_ = w.Close()

	_, _ = reg.AddResolvedEdges(ctx, runID, []model.Edge{{
		SrcID: handleID, DstID: updateID, Kind: model.EdgeCalls,
		Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{
			FilePath: ctrl.Path,
			Range:    model.Range{Start: model.Position{Line: 11}},
			Source:   model.SourceSourceCode,
			Detail:   "receiver type matches",
		},
	}})
}

func callTool(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestMinimalProfileToolCount guards a performance property, not a preference.
//
// Every exposed tool's schema enters the agent's context on EVERY request. A
// server that quietly grows to forty tools spends the context RepoCortex exists
// to save, on every single call, forever.
// TestReplyAlwaysSetsProvenance is what makes "reply() is the single
// population site for Provenance" more than a comment. Without it, all the
// other tests here would keep passing even if reply() stopped setting the
// field: none of them assert on Provenance directly.
//
// It goes over the real in-memory transport, like every other test in this
// file, and inspects the actual wire-level StructuredContent an agent would
// receive — not a Go value never marshalled — so a regression in JSON tags
// would also be caught here.
func TestReplyAlwaysSetsProvenance(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)
	res := callTool(t, sess, "find_callers", map[string]any{"name": "updateStatus"})

	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %#v (%T), want a JSON object", res.StructuredContent, res.StructuredContent)
	}
	prov, ok := sc["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("structured content has no provenance object: %#v", sc)
	}
	caveat, _ := prov["caveat"].(string)
	if caveat == "" {
		t.Error("Provenance.Caveat is empty — reply() must populate it on every response")
	}
}

func TestMinimalProfileToolCount(t *testing.T) {
	sess := newTestServer(t, nil)
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) > 8 {
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("minimal profile exposes %d tools (%v); it must stay small", len(res.Tools), names)
	}
	for _, tool := range res.Tools {
		if tool.Description == "" {
			t.Errorf("tool %q has no description; an agent cannot choose it correctly", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}
	t.Logf("minimal profile: %d tools", len(res.Tools))
}

func TestProjectOverviewReportsCoverage(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)
	text := resultText(callTool(t, sess, "get_project_overview", map[string]any{}))

	if !strings.Contains(text, "SYSTEM METADATA") {
		t.Error("response is not framed with a trust section")
	}
	if !strings.Contains(text, "testproj") {
		t.Errorf("overview does not name the project:\n%s", text)
	}
	// An agent must learn that the call graph is incomplete, or it will read an
	// empty caller list as proof a symbol is unused.
	if !strings.Contains(strings.ToLower(text), "resolve") {
		t.Errorf("overview does not mention resolution coverage:\n%s", text)
	}
}

func TestFindCallersReturnsCallerNotFile(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)
	text := resultText(callTool(t, sess, "find_callers", map[string]any{"name": "updateStatus"}))

	if !strings.Contains(text, "handleCallback") {
		t.Errorf("caller not reported; want handleCallback:\n%s", text)
	}
	if !strings.Contains(text, "confidence=high") {
		t.Errorf("confidence not surfaced to the agent:\n%s", text)
	}
	if !strings.Contains(text, "receiver type matches") {
		t.Errorf("the reason for the binding was not surfaced:\n%s", text)
	}
}

// TestEmptyCallersIsNotPresentedAsProof is the most consequential honesty test
// in this package.
//
// An agent told "no callers" will delete the method. The index resolves only
// part of any repository, so an empty result is a LOWER BOUND, and the response
// must say so.
func TestEmptyCallersIsNotPresentedAsProof(t *testing.T) {
	sess := newTestServer(t, func(ctx context.Context, reg *registry.Registry, runID int64) {
		seedPaymentGraph(ctx, reg, runID)
		// A reference nothing could bind, so coverage is genuinely partial.
		w, _ := reg.NewWriter(ctx, runID)
		f := model.File{Path: "x/Other.java", Language: "java", ContentHash: "h3", ParseStatus: "ok"}
		fid, _ := w.AddFile(ctx, &f)
		_ = w.AddReference(ctx, fid, registry.Reference{Name: "mystery", Kind: "reference.call", Line: 3})
		_ = w.Flush(ctx)
		_ = w.Close()
		_ = reg.MarkResolutions(ctx, []registry.RefOutcome{
			{RefID: 1, Resolution: registry.ResolutionUnresolved},
		})
	})

	text := resultText(callTool(t, sess, "find_callers", map[string]any{"name": "handleCallback"}))
	lower := strings.ToLower(text)

	if !strings.Contains(lower, "lower bound") && !strings.Contains(lower, "unresolved") {
		t.Errorf("an empty caller list was presented without a coverage caveat; "+
			"an agent could conclude the symbol is unused:\n%s", text)
	}
}

func TestUnknownSymbolFailsUsefully(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)
	res := callTool(t, sess, "find_symbol", map[string]any{"name": "DoesNotExist"})
	text := resultText(res)
	if !strings.Contains(strings.ToLower(text), "not found") && len(res.Content) == 0 {
		t.Errorf("unhelpful response for a missing symbol:\n%s", text)
	}
}

func TestEmptyArgumentsRejected(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)
	for _, tool := range []string{"search_code", "find_symbol", "find_callers"} {
		res := callTool(t, sess, tool, map[string]any{"name": "", "query": ""})
		if !res.IsError {
			t.Errorf("%s accepted empty input without an error result", tool)
		}
	}
}

// TestNoIndexIsGuidanceNotAFailure: a missing index is information the agent
// can act on ("tell the user to run cortex index"), so it must arrive as a
// result, not as a transport failure that leaves the model nothing.
//
// THE isError ASSERTION IN THIS TEST WAS DELIBERATELY INVERTED. It previously
// required IsError to be TRUE. That is the exact defect A29 fixes: a first
// call against an unindexed project would flag a failure, the model would
// conclude the toolset is broken, and it would stop calling RepoCortex for the
// remainder of the session — losing every later answer, over a condition the
// user fixes with one command. "Not a protocol error" was necessary and is
// still asserted; it was not sufficient.
func TestNoIndexIsGuidanceNotAFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv, err := New(Deps{Registry: registry.New(db), Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), serverT) }()

	sess, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "get_project_overview", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("a missing index became a protocol error: %v", err)
	}
	if res.IsError {
		t.Errorf("a missing index was flagged isError; an agent that sees a tool fail stops "+
			"calling it for the rest of the session:\n%s", resultText(res))
	}
	if !strings.Contains(resultText(res), "cortex index") {
		t.Errorf("the response does not tell the agent how to fix it:\n%s", resultText(res))
	}
	// Success-SHAPED, not merely success-flagged: the agent must get the normal
	// framing it parses every other answer with.
	if !strings.Contains(resultText(res), "SYSTEM METADATA") {
		t.Errorf("guidance is not framed like a normal answer:\n%s", resultText(res))
	}
}

// TestFindCallersNoIndexIsGuidanceNotAFailure is the referencesFor-path sibling
// of TestNoIndexIsGuidanceNotAFailure above.
//
// find_callers, find_references and plan_rename all resolve through the
// shared referencesFor helper (rename.go) rather than through
// get_project_overview's own no-run check, so the same guidance-not-failure
// guarantee has to be verified on that path independently: a fix that only
// covers get_project_overview leaves this one broken.
func TestFindCallersNoIndexIsGuidanceNotAFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv, err := New(Deps{Registry: registry.New(db), Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), serverT) }()

	sess, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "find_callers", Arguments: map[string]any{"name": "anything"}})
	if err != nil {
		t.Fatalf("a missing index became a protocol error: %v", err)
	}
	if res.IsError {
		t.Errorf("a missing index was flagged isError; an agent that sees a tool fail stops "+
			"calling it for the rest of the session:\n%s", resultText(res))
	}
	if !strings.Contains(resultText(res), "cortex index") {
		t.Errorf("the response does not tell the agent how to fix it:\n%s", resultText(res))
	}
	if !strings.Contains(resultText(res), "SYSTEM METADATA") {
		t.Errorf("guidance is not framed like a normal answer:\n%s", resultText(res))
	}
}

// ------------------------------------------------------------- A29: isError policy
//
// The tests below pin the CLASSIFICATION, which is the whole substance of the
// change. Any handler can be made to return a message; what matters is which
// side of the isError line each condition lands on, because a wrong answer
// there costs every remaining call in the session rather than one call.

// TestRecoverableConditionsAreNotFailures walks the conditions an agent can
// fix by asking a better question, or by telling the user to run a command.
// None of them may set isError.
func TestRecoverableConditionsAreNotFailures(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)

	cases := []struct {
		name string
		tool string
		args map[string]any
		want string // guidance the agent needs in order to act
	}{
		{"symbol absent from find_symbol", "find_symbol", map[string]any{"name": "NoSuchThing"}, "search_code"},
		{"symbol absent from get_symbol", "get_symbol", map[string]any{"name": "NoSuchThing"}, "search_code"},
		{"symbol absent from find_callers", "find_callers", map[string]any{"name": "NoSuchThing"}, "search_code"},
		{"symbol absent from find_references", "find_references", map[string]any{"name": "NoSuchThing"}, "search_code"},
		{"search matches nothing", "search_code", map[string]any{"query": "zzzznotpresent"}, "No matches"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, sess, tc.tool, tc.args)
			if res.IsError {
				t.Errorf("%s flagged a recoverable condition as a tool failure; an early isError "+
					"teaches the agent the toolset is broken and it stops calling it:\n%s",
					tc.tool, resultText(res))
			}
			text := resultText(res)
			if !strings.Contains(text, "SYSTEM METADATA") {
				t.Errorf("%s: guidance is not success-SHAPED:\n%s", tc.tool, text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("%s: the response does not tell the agent what to do next "+
					"(want %q):\n%s", tc.tool, tc.want, text)
			}
		})
	}
}

// TestEmptyIndexIsGuidanceNotFailure: a committed but empty index is a state of
// the world, not a malfunction. The overview must say so and name the fix.
func TestEmptyIndexIsGuidanceNotFailure(t *testing.T) {
	sess := newTestServer(t, nil) // committed run, nothing in it
	res := callTool(t, sess, "get_project_overview", map[string]any{})
	if res.IsError {
		t.Errorf("an empty index was reported as a tool failure:\n%s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "EMPTY") || !strings.Contains(text, "cortex index") {
		t.Errorf("an empty index was not explained, or the fix was not named:\n%s", text)
	}
}

// TestGenuineMalfunctionStillSetsIsError is the other half of the policy, and
// the reason it is a policy rather than a blanket softening.
//
// A storage read that FAILED is not "no results". Presenting it as a
// success-shaped empty answer would let an agent conclude a symbol has no
// callers because the database was unreadable — which is precisely the
// silent-wrong-answer failure mode the whole package exists to prevent. A
// closed database stands in for any broken store.
func TestGenuineMalfunctionStillSetsIsError(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	seedPaymentGraph(ctx, reg, run.ID)
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Deps{Registry: reg, Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), serverT) }()
	sess, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Break the store underneath a working server.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tool := range []string{"get_project_overview", "find_symbol", "find_callers"} {
		res := callTool(t, sess, tool, map[string]any{"name": "updateStatus", "query": "x"})
		if !res.IsError {
			t.Errorf("%s reported a broken store as a successful empty answer; an agent would "+
				"conclude the symbol has no callers:\n%s", tool, resultText(res))
		}
		if strings.Contains(resultText(res), "cortex index") {
			t.Errorf("%s told the user to re-index over a broken store, which will not fix it:\n%s",
				tool, resultText(res))
		}
	}
}

// TestMalformedCallStillSetsIsError: an empty required argument is the AGENT's
// mistake, not a state of the repository. It must be flagged, so the agent
// corrects the call rather than believing the repository is empty.
func TestMalformedCallStillSetsIsError(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)
	for _, tool := range []string{"search_code", "find_symbol", "get_symbol", "find_callers", "find_references"} {
		res := callTool(t, sess, tool, map[string]any{"name": "  ", "query": "  "})
		if !res.IsError {
			t.Errorf("%s accepted a malformed call without flagging it:\n%s", tool, resultText(res))
		}
	}
}

// ------------------------------------------------------------- A35: coverage gate

// seedLowCoverage builds an index that left most of what it CAN bind unbound:
// 2 resolved and 8 ambiguous, so a 20% binding rate — below the floor.
//
// Ambiguous, not unresolved. The published rate is BindingRate (see
// capability.CoverageFloor), whose denominator is what this index claims to
// define, so ten references of which eight matched nothing here would be a
// 100% binding rate over a bindable population of two: a correct verdict on an
// index that bound everything it could, and the wrong fixture for a gate about
// indexes that did not.
func seedLowCoverage(ctx context.Context, reg *registry.Registry, runID int64) {
	seedPaymentGraph(ctx, reg, runID)
	seedReferences(ctx, reg, runID, 2, 8, 0)
}

// seedHighCoverage: 9 of 10 bindable references bound, comfortably above the
// floor, with one unresolved beside them so the rate has something to exclude.
func seedHighCoverage(ctx context.Context, reg *registry.Registry, runID int64) {
	seedPaymentGraph(ctx, reg, runID)
	seedReferences(ctx, reg, runID, 9, 1, 1)
}

func seedReferences(ctx context.Context, reg *registry.Registry, runID int64, resolved, ambiguous, unresolved int) {
	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	f := model.File{Path: "x/Other.java", Language: "java", ContentHash: "hrefs", ParseStatus: "ok"}
	fid, _ := w.AddFile(ctx, &f)
	total := resolved + ambiguous + unresolved
	for i := 0; i < total; i++ {
		_ = w.AddReference(ctx, fid, registry.Reference{
			Name: "ref", Kind: "reference.call", Line: i + 1,
		})
	}
	_ = w.Flush(ctx)
	_ = w.Close()

	var outcomes []registry.RefOutcome
	next := int64(1)
	add := func(n int, r registry.Resolution) {
		for i := 0; i < n; i++ {
			outcomes = append(outcomes, registry.RefOutcome{RefID: next, Resolution: r})
			next++
		}
	}
	add(resolved, registry.ResolutionResolved)
	add(ambiguous, registry.ResolutionAmbiguous)
	add(unresolved, registry.ResolutionUnresolved)
	_ = reg.MarkResolutions(ctx, outcomes)
}

// TestLowCoverageRelationshipAnswerCarriesTheCaveat is A35's point.
//
// A caller list drawn from an index that bound a fifth of its references looks
// identical to one drawn from complete data. The response must say which it is
// — naming the language and the actual rate — and it must say it BEFORE the
// results, because notes are read in order.
func TestLowCoverageRelationshipAnswerCarriesTheCaveat(t *testing.T) {
	sess := newTestServer(t, seedLowCoverage)
	res := callTool(t, sess, "find_callers", map[string]any{"name": "updateStatus"})
	text := resultText(res)

	if res.IsError {
		t.Fatalf("low coverage is not a malfunction:\n%s", text)
	}
	if !strings.Contains(text, "COVERAGE TOO LOW") {
		t.Errorf("a 20%%-coverage caller list was presented without a coverage verdict:\n%s", text)
	}
	// The language, so the reader knows which part of his repository the rate
	// describes, and the rate itself, so he can disagree with the threshold.
	if !strings.Contains(text, "Java") {
		t.Errorf("the caveat does not name the language it applies to:\n%s", text)
	}
	if !strings.Contains(text, "20%") {
		t.Errorf("the caveat does not state the actual resolution rate:\n%s", text)
	}
	// Extends internal/capability's mechanism rather than paraphrasing it.
	if !strings.Contains(text, capability.LowerBoundCaveat) {
		t.Errorf("the caveat does not carry capability.LowerBoundCaveat verbatim:\n%s", text)
	}
	// The verdict must precede the answer it is a verdict on.
	if verdict, list := strings.Index(text, "COVERAGE TOO LOW"), strings.Index(text, "INFERRED"); verdict > list {
		t.Errorf("the coverage verdict appears after the results it qualifies:\n%s", text)
	}
	// And the result itself is still delivered: a gate, not a refusal.
	if !strings.Contains(text, "handleCallback") {
		t.Errorf("the gate suppressed the answer instead of qualifying it:\n%s", text)
	}
}

// TestGoodCoverageIsSilent proves the gate is a gate.
//
// A caveat printed on every call is a caveat that is read on none, and it costs
// tokens on every single response. Above the floor the server says nothing.
func TestGoodCoverageIsSilent(t *testing.T) {
	sess := newTestServer(t, seedHighCoverage)
	text := resultText(callTool(t, sess, "find_callers", map[string]any{"name": "updateStatus"}))
	if strings.Contains(text, "COVERAGE TOO LOW") || strings.Contains(text, "COVERAGE UNKNOWN") {
		t.Errorf("a 90%%-coverage answer paid for a coverage caveat it did not need:\n%s", text)
	}
}

// TestUnresolvedIndexSaysCoverageIsUnknown: an index whose references are all
// pending has NOT been measured at 0%. "Never looked" and "looked and found
// nothing" are different claims, and only one of them is fixed by resolving.
func TestUnresolvedIndexSaysCoverageIsUnknown(t *testing.T) {
	sess := newTestServer(t, func(ctx context.Context, reg *registry.Registry, runID int64) {
		seedPaymentGraph(ctx, reg, runID)
		seedReferences(ctx, reg, runID, 0, 0, 0)
		w, _ := reg.NewWriter(ctx, runID)
		f := model.File{Path: "y/Pending.java", Language: "java", ContentHash: "hp", ParseStatus: "ok"}
		fid, _ := w.AddFile(ctx, &f)
		_ = w.AddReference(ctx, fid, registry.Reference{Name: "p", Kind: "reference.call", Line: 1})
		_ = w.Flush(ctx)
		_ = w.Close()
	})
	text := resultText(callTool(t, sess, "find_callers", map[string]any{"name": "updateStatus"}))
	if !strings.Contains(text, "COVERAGE UNKNOWN") {
		t.Errorf("an unresolved index reported a coverage verdict it had not measured:\n%s", text)
	}
	if !strings.Contains(text, "cortex resolve") {
		t.Errorf("the response does not name the command that fixes it:\n%s", text)
	}
}

// TestCoverageGateClassification is the table behind the prose above.
func TestCoverageGateClassification(t *testing.T) {
	cases := []struct {
		name         string
		stats        registry.ResolutionStats
		path         string
		wantReliable bool
		wantScope    string
	}{
		// The rate is Resolved over Bindable (Resolved+Ambiguous), so what
		// separates these rows is AMBIGUITY, not library calls. A row with
		// nothing but resolved and unresolved references bound everything it
		// could and is reliable however small that population was — which is
		// the whole reason this floor moved off ResolutionStats.Rate.
		{"well resolved", registry.ResolutionStats{Total: 100, Resolved: 90, Ambiguous: 5, Unresolved: 5}, "a/B.kt", true, "Kotlin"},
		{"exactly at the floor", registry.ResolutionStats{Total: 100, Resolved: 60, Ambiguous: 40}, "a/B.go", true, "Go"},
		{"just below the floor", registry.ResolutionStats{Total: 100, Resolved: 59, Ambiguous: 41}, "a/B.go", false, "Go"},
		{"the motivating case", registry.ResolutionStats{Total: 100, Resolved: 22, Ambiguous: 60, Unresolved: 18}, "a/B.kt", false, "Kotlin"},
		{"heavy library use, but everything bindable bound", registry.ResolutionStats{Total: 100, Resolved: 22, Unresolved: 78}, "a/B.kt", true, "Kotlin"},
		{"no references at all", registry.ResolutionStats{}, "a/B.java", false, "Java"},
		{"all pending", registry.ResolutionStats{Total: 40, Pending: 40}, "a/B.java", false, "Java"},
		{"unknown extension", registry.ResolutionStats{Total: 10, Resolved: 1, Ambiguous: 9}, "a/B.zzz", false, "this repository"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := coverageOf(tc.stats, tc.path)
			if c.Reliable != tc.wantReliable {
				t.Errorf("Reliable = %v, want %v (rate %.2f)", c.Reliable, tc.wantReliable, c.Rate)
			}
			if c.scope() != tc.wantScope {
				t.Errorf("scope = %q, want %q", c.scope(), tc.wantScope)
			}
			if got := c.caveat("impact"); tc.wantReliable && got != nil {
				t.Errorf("a reliable index paid for a caveat: %v", got)
			} else if !tc.wantReliable && len(got) == 0 {
				t.Error("an unreliable index produced no caveat")
			}
		})
	}
}

// TestCaveatAndVerdictReachTheModelAsText is the guard for the gap that eleven
// task-scoped reviews could not see individually.
//
// Every one of those tasks asserted that its new field MARSHALS. None asked
// whether a model ever reads it. Server.reply sets the result's Content to
// RenderText's output, and because Content is non-nil the go-sdk never falls
// back to marshalling the struct into the text block — so a host that does not
// surface structuredContent showed the agent a branch that had measured
// confidence and index freshness and then said neither out loud.
//
// It deliberately compares the TEXT against the STRUCTURED content rather than
// against literals: the assertion is that the two agree, which no amount of
// rewording either one can quietly break.
func TestCaveatAndVerdictReachTheModelAsText(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)
	res := callTool(t, sess, "find_callers", map[string]any{"name": "updateStatus"})

	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %#v (%T), want a JSON object", res.StructuredContent, res.StructuredContent)
	}
	text := resultText(res)

	prov, ok := sc["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("structured content has no provenance object: %#v", sc)
	}
	caveat, _ := prov["caveat"].(string)
	if caveat == "" {
		t.Fatal("Provenance.Caveat is empty; nothing to check reached the text")
	}
	if !strings.Contains(text, caveat) {
		t.Errorf("provenance caveat is absent from the TEXT a client receives.\ncaveat: %q\ntext:\n%s", caveat, text)
	}

	verdict, ok := sc["verdict"].(map[string]any)
	if !ok {
		t.Fatalf("find_callers returned confidence-carrying items but no verdict: %#v", sc)
	}
	next, _ := verdict["next_step"].(string)
	if next == "" {
		t.Fatal("Verdict.NextStep is empty; the verdict states no action")
	}
	if !strings.Contains(text, next) {
		t.Errorf("verdict next_step is absent from the TEXT a client receives.\nnext_step: %q\ntext:\n%s", next, text)
	}
	if status, _ := verdict["status"].(string); status == "" || !strings.Contains(text, "verdict: "+status) {
		t.Errorf("verdict status %q is absent from the TEXT a client receives:\n%s", status, text)
	}
}

// TestEveryConfidenceCarryingToolGetsAVerdict is the I2 guard: the field doc
// promised "any response whose items carry confidence" while two tools out of
// eight actually assigned it. Now that Server.reply is the only population
// site, the promise is testable — and this test is what keeps a tool added
// tomorrow from silently opting out.
func TestEveryConfidenceCarryingToolGetsAVerdict(t *testing.T) {
	sess := newTestServer(t, seedPaymentGraph)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"find_symbol", map[string]any{"name": "updateStatus"}},
		{"get_symbol", map[string]any{"name": "updateStatus"}},
		{"find_references", map[string]any{"name": "updateStatus"}},
		{"find_callers", map[string]any{"name": "updateStatus"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			res := callTool(t, sess, tc.tool, tc.args)
			sc, ok := res.StructuredContent.(map[string]any)
			if !ok {
				t.Fatalf("StructuredContent = %#v, want a JSON object", res.StructuredContent)
			}
			items, _ := sc["items"].([]any)
			carries := false
			for _, raw := range items {
				if it, ok := raw.(map[string]any); ok {
					if c, _ := it["confidence"].(string); c != "" {
						carries = true
						break
					}
				}
			}
			if !carries {
				t.Skipf("%s returned no confidence-carrying items in this fixture", tc.tool)
			}
			if _, ok := sc["verdict"].(map[string]any); !ok {
				t.Fatalf("%s returned confidence-carrying items and no verdict: %#v", tc.tool, sc)
			}
		})
	}
}

// TestRenamePlanListsItsSitesInText: the summary said "3 proven edits to apply,
// 7 to review" and then listed nothing, leaving the agent a count it could not
// act on.
func TestRenamePlanListsItsSitesInText(t *testing.T) {
	sess := newGraphTestServer(t, seedPaymentGraph)
	res := callTool(t, sess, "plan_rename", map[string]any{"name": "updateStatus", "new_name": "setStatus"})

	text := resultText(res)
	for _, want := range []string{"rename_apply:", "rename_review:"} {
		if !strings.Contains(text, want) {
			t.Errorf("rename plan text is missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "api/CallbackController.java:11") {
		t.Errorf("the one known rename site is not listed in the text:\n%s", text)
	}
}

// seedFilteredNonCallRefs builds a target with one high-confidence CALLS edge,
// one high-confidence REFERENCES edge and several low-confidence REFERENCES
// edges — the shape that makes find_callers' "further relationship(s)" note a
// filtered count.
func seedFilteredNonCallRefs(ctx context.Context, reg *registry.Registry, runID int64) {
	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	defer w.Close()

	tgtFile := model.File{Path: "svc/Target.java", Language: "java", ContentHash: "ht", ParseStatus: "ok"}
	tgtFileID, _ := w.AddFile(ctx, &tgtFile)
	target := model.Symbol{
		FileID: tgtFileID, Kind: model.KindMethod, Name: "process",
		FQN: "svc.Target#process", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 10}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	targetID, _ := w.AddSymbol(ctx, &target)

	mk := func(i int, conf model.Confidence, kind model.EdgeKind) model.Edge {
		path := fmt.Sprintf("refs/R%d.java", i)
		f := model.File{Path: path, Language: "java", ContentHash: fmt.Sprintf("h%d", i), ParseStatus: "ok"}
		fID, _ := w.AddFile(ctx, &f)
		sym := model.Symbol{
			FileID: fID, Kind: model.KindMethod, Name: fmt.Sprintf("ref%d", i),
			FQN:        fmt.Sprintf("refs.R%d#ref%d", i, i),
			Exported:   true,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
			Confidence: conf, Source: model.SourceSourceCode,
		}
		symID, _ := w.AddSymbol(ctx, &sym)
		return model.Edge{
			SrcID: symID, DstID: targetID, Kind: kind, Confidence: conf,
			Evidence: model.Evidence{
				FilePath: path,
				Range:    model.Range{Start: model.Position{Line: 1}},
				Source:   model.SourceSourceCode,
				Detail:   "reference",
			},
		}
	}

	edges := []model.Edge{
		mk(0, model.ConfidenceHigh, model.EdgeCalls),
		mk(1, model.ConfidenceHigh, model.EdgeReferences),
		mk(2, model.ConfidenceLow, model.EdgeReferences),
		mk(3, model.ConfidenceLow, model.EdgeReferences),
		mk(4, model.ConfidenceLow, model.EdgeReferences),
	}

	_ = w.IndexFileText(ctx, tgtFileID, tgtFile.Path, "class Target { void process() {} }")
	_ = w.Flush(ctx)
	_ = w.Close()

	if _, err := reg.AddResolvedEdges(ctx, runID, edges); err != nil {
		panic(err)
	}
}

// TestFilteredNonCallCountNamesItsFloor is the I8 guard.
//
// IncomingEdgeCount is called with the min_confidence floor applied, so at
// min_confidence="high" a symbol with 1 high and 3 low non-call references
// reports "1". That is a true statement about a filtered set stated with the
// grammar of a total, and the note has to say which it is.
func TestFilteredNonCallCountNamesItsFloor(t *testing.T) {
	sess := newTestServer(t, seedFilteredNonCallRefs)

	filtered := resultText(callTool(t, sess, "find_callers",
		map[string]any{"name": "process", "min_confidence": "high"}))
	if !strings.Contains(filtered, "further relationship(s)") {
		t.Fatalf("expected the non-call note, got:\n%s", filtered)
	}
	if !strings.Contains(filtered, `at confidence "high" and above`) {
		t.Errorf("a filtered count was stated without naming its floor:\n%s", filtered)
	}

	unfiltered := resultText(callTool(t, sess, "find_callers",
		map[string]any{"name": "process", "min_confidence": "low"}))
	if strings.Contains(unfiltered, "and above point at this symbol") {
		t.Errorf("an unfiltered count should not claim a floor:\n%s", unfiltered)
	}
}

// TestCoverageVerdictsAgreeAcrossTools is I6: two tools in one session gave
// contradictory verdicts about the same index.
//
// impact_analysis and find_tests judged coverage with ResolutionStats.Rate
// against 0.5; resolution_coverage judged it with BindingRate against 0.6. On
// a real index that is 37% and "COVERAGE TOO LOW" from one tool against 79%
// and silence from the other, in the same session, about the same number. An
// agent cannot act on both.
//
// BindingRate is the published number, for the reason Rate's own doc gives:
// its denominator counts references that matched zero candidates, which are
// overwhelmingly library calls, so it reads as a defect rate while mostly
// describing a dependency surface. The floor moves with the denominator.
func TestCoverageVerdictsAgreeAcrossTools(t *testing.T) {
	// The shape of this repository's own index: a minority of ALL references
	// bound, a large majority of the BINDABLE ones.
	cases := []registry.ResolutionStats{
		{Total: 100, Resolved: 37, Ambiguous: 10, Unresolved: 53},
		{Total: 100, Resolved: 90, Ambiguous: 5, Unresolved: 5},
		{Total: 100, Resolved: 20, Ambiguous: 60, Unresolved: 20},
		{Total: 40, Pending: 40},
		{},
	}
	for _, rs := range cases {
		c := coverageOf(rs, "a/B.go")
		if c.Rate != rs.BindingRate() {
			t.Errorf("stats %+v: the relationship tools publish %.4f but resolution_coverage publishes %.4f",
				rs, c.Rate, rs.BindingRate())
		}
		lowThere := rs.Total > 0 && rs.Pending < rs.Total && rs.BindingRate() < capability.CoverageFloor
		lowHere := c.Known && !c.Reliable
		if lowHere != lowThere {
			t.Errorf("stats %+v: relationship tools say low=%v, resolution_coverage says low=%v",
				rs, lowHere, lowThere)
		}
	}

	// Bindable's stated condition: the excluded references are printed BESIDE
	// the rate, never instead of it. A caveat that quotes a binding rate
	// without the count its denominator dropped is the dishonest half of this
	// convergence.
	low := registry.ResolutionStats{Total: 100, Resolved: 20, Ambiguous: 60, Unresolved: 20}
	got := strings.Join(coverageOf(low, "a/B.go").caveat("impact"), " ")
	if !strings.Contains(got, "20 matched no definition") {
		t.Errorf("the caveat quotes a binding rate without the unresolved count its denominator excluded:\n%s", got)
	}
}
