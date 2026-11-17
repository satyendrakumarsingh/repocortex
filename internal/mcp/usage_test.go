package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionStatsOf calls get_session_stats and decodes the structured report an
// agent would receive, along with the text.
func sessionStatsOf(t *testing.T, sess *sdk.ClientSession) (SessionStats, string) {
	t.Helper()
	res := callTool(t, sess, "get_session_stats", map[string]any{})
	if res.IsError {
		t.Fatalf("get_session_stats reported a failure:\n%s", resultText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		SessionStats *SessionStats `json:"session_stats"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode structured content: %v\n%s", err, raw)
	}
	if env.SessionStats == nil {
		t.Fatalf("the answer carries no session_stats:\n%s", raw)
	}
	return *env.SessionStats, resultText(res)
}

func usageOf(st SessionStats, tool string) (ToolUsage, bool) {
	for _, u := range st.Tools {
		if u.Tool == tool {
			return u, true
		}
	}
	return ToolUsage{}, false
}

// oneLine collapses whitespace, so a phrase is found however the text wraps.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// The report is only worth having if its figures are the ones the session
// actually incurred, so every number is checked against what the client itself
// received.
func TestSessionStatsCountWhatTheSessionReceived(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()

	var wantBytes, wantTokens int64
	received := func(res *sdk.CallToolResult) {
		n := int64(len(resultText(res)))
		wantBytes += n
		wantTokens += (n + 3) / 4
	}

	received(callTool(t, fs.sess, "find_callers", map[string]any{"name": "leaf"}))
	received(callTool(t, fs.sess, "find_callers", map[string]any{"name": "middle"}))
	received(callTool(t, fs.sess, "analyze_impact", map[string]any{"symbol": "leaf"}))
	rejected := callTool(t, fs.sess, "find_callers", map[string]any{"name": " "})
	if !rejected.IsError {
		t.Fatal("an empty name should be rejected; the test needs one tool error")
	}
	received(rejected)
	if _, err := fs.sess.CallTool(ctx, &sdk.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{}}); err == nil {
		t.Fatal("an unknown tool should be a protocol error; the test needs one")
	}

	st, text := sessionStatsOf(t, fs.sess)

	if st.Calls != 5 || st.Failures != 2 {
		t.Errorf("calls=%d failures=%d, want 5 and 2\n%s", st.Calls, st.Failures, text)
	}
	if st.ResultBytes != wantBytes {
		t.Errorf("result_bytes = %d, the client received %d bytes of text", st.ResultBytes, wantBytes)
	}
	if st.EstimatedResultTokens != wantTokens {
		t.Errorf("estimated_result_tokens = %d, want %d (bytes / 4 rounded up, per answer)", st.EstimatedResultTokens, wantTokens)
	}
	if st.StructuredBytes == 0 {
		t.Error("structured_bytes is 0; the answers carried structured content")
	}
	if st.Transport != "mcp" || st.Since == "" {
		t.Errorf("transport=%q since=%q, want mcp and a start time", st.Transport, st.Since)
	}

	callers, ok := usageOf(st, "find_callers")
	if !ok || callers.Calls != 3 || callers.ToolErrors != 1 || callers.ProtocolErrors != 0 {
		t.Errorf("find_callers = %+v, want 3 calls with 1 tool error", callers)
	}
	if impact, ok := usageOf(st, "analyze_impact"); !ok || impact.Calls != 1 {
		t.Errorf("analyze_impact = %+v, want 1 call", impact)
	}
	if st.Tools[0].Tool != "find_callers" {
		t.Errorf("the most called tool should be first, got %q", st.Tools[0].Tool)
	}

	// The rows must add up to the totals, or one of the two is wrong.
	var sumCalls, sumBytes, sumTokens int64
	for _, u := range st.Tools {
		sumCalls += u.Calls
		sumBytes += u.ResultBytes
		sumTokens += u.EstimatedResultTokens
	}
	if sumCalls != st.Calls || sumBytes != st.ResultBytes || sumTokens != st.EstimatedResultTokens {
		t.Errorf("rows sum to calls=%d bytes=%d tokens=%d; totals say %d, %d, %d",
			sumCalls, sumBytes, sumTokens, st.Calls, st.ResultBytes, st.EstimatedResultTokens)
	}

	// The call that asks is not part of its own answer, and is part of the next.
	if _, ok := usageOf(st, "get_session_stats"); ok {
		t.Error("get_session_stats counted itself in its own answer")
	}
	again, _ := sessionStatsOf(t, fs.sess)
	if u, ok := usageOf(again, "get_session_stats"); !ok || u.Calls != 1 || again.Calls != 6 {
		t.Errorf("the second report should count the first: %+v, calls=%d", u, again.Calls)
	}
}

// A client chooses the tool name it sends. Counting under whatever it sent
// would let one client grow the server's memory without limit.
func TestUnregisteredToolNamesShareOneCounter(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()

	const bogus = 40
	for i := 0; i < bogus; i++ {
		_, _ = fs.sess.CallTool(ctx, &sdk.CallToolParams{Name: fmt.Sprintf("invented_tool_%d", i), Arguments: map[string]any{}})
	}
	st, text := sessionStatsOf(t, fs.sess)
	if len(st.Tools) != 1 {
		t.Fatalf("%d rows for %d invented names, want 1:\n%s", len(st.Tools), bogus, text)
	}
	if st.Tools[0].Tool != unregisteredTool || st.Tools[0].Calls != bogus || st.Tools[0].ProtocolErrors != bogus {
		t.Errorf("row = %+v, want %d protocol errors under %q", st.Tools[0], bogus, unregisteredTool)
	}
	if strings.Contains(text, "invented_tool_") {
		t.Errorf("a name the client invented reached the report:\n%s", text)
	}
}

// The tool name is the one field of an audit event a client writes in full,
// so it is bounded like the arguments are.
func TestAuditLogBoundsTheToolName(t *testing.T) {
	var log bytes.Buffer
	fs := newFullServer(t, seedChain, withAudit(NewAuditLog(&log)))
	_, _ = fs.sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: strings.Repeat("x", 50_000), Arguments: map[string]any{},
	})
	var ev AuditEvent
	if err := json.Unmarshal(bytes.TrimSpace(log.Bytes()), &ev); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	if len(ev.Tool) > auditToolBytes || ev.Outcome != outcomeProtocolError {
		t.Errorf("tool name kept at %d bytes with outcome %q; want at most %d and a protocol error",
			len(ev.Tool), ev.Outcome, auditToolBytes)
	}
}

// The project's one agent benchmark found no token advantage, and its docs say
// not to quote a saving. This is the test that keeps the report from growing
// one.
func TestSessionStatsNeverClaimASaving(t *testing.T) {
	fs := newFullServer(t, seedChain)
	callTool(t, fs.sess, "find_callers", map[string]any{"name": "leaf"})
	callTool(t, fs.sess, "search_code", map[string]any{"query": "leaf"})

	st, text := sessionStatsOf(t, fs.sess)
	flat := oneLine(text)
	lower := strings.ToLower(flat)

	for _, banned := range []string{"saved", "savings", "reduction", "fewer tokens", "%", "instead of reading"} {
		if strings.Contains(lower, banned) {
			t.Errorf("the report contains %q, which reads as a saving claim:\n%s", banned, text)
		}
	}
	for _, want := range []string{
		"This is usage, not a saving",
		"Token figures are estimates: bytes / 4, rounded up",
		"no tokenizer is run",
		"est_answer_tokens=",
		"est_tokens_per_request=",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the report does not say %q:\n%s", want, text)
		}
	}
	if st.TokenBasis != BasisEstimated || st.TokenEstimate != EstimateMethod {
		t.Errorf("token_basis=%q token_estimate=%q; token figures must be labelled as estimates", st.TokenBasis, st.TokenEstimate)
	}

	// No field of the structured report may be a difference or a ratio either.
	raw, _ := json.Marshal(st)
	for _, key := range jsonKeys(t, raw) {
		for _, banned := range []string{"saved", "saving", "ratio", "percent", "baseline", "without"} {
			if key == banned || strings.HasPrefix(key, banned+"_") || strings.HasSuffix(key, "_"+banned) ||
				strings.Contains(key, "_"+banned+"_") {
				t.Errorf("the structured report has a field %q, which is a comparison", key)
			}
		}
	}
}

// jsonKeys lists every object key in a JSON document, at any depth.
func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				keys = append(keys, k)
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	return keys
}

// The fixed cost must be the number `repocortex tool list` prints, on every
// profile that can report it, or two screens disagree about one fact.
func TestToolDefinitionCostMatchesToolList(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()

	inv, err := fs.srv.NewInvoker(ctx, "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inv.Close() }()
	tools, err := inv.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantTokens, wantBytes := 0, 0
	for _, tool := range tools {
		wantTokens += tool.EstimatedTokens
		wantBytes += tool.SchemaBytes
	}

	st, text := sessionStatsOf(t, fs.sess)
	d := st.ToolDefinitions
	if d == nil {
		t.Fatalf("no tool_definitions in the report:\n%s", text)
	}
	if d.Profile != "full" || d.Tools != len(tools) || d.SchemaBytes != wantBytes || d.EstimatedTokensPerRequest != wantTokens {
		t.Errorf("tool_definitions = %+v, want full, %d tools, %d bytes, %d tokens", *d, len(tools), wantBytes, wantTokens)
	}
	if !strings.Contains(oneLine(text), "paid on every model request") {
		t.Errorf("the report does not say the definitions are a per-request cost:\n%s", text)
	}
}

// A tool about cost must not raise the cost of sessions that never ask.
func TestSessionStatsToolIsFullProfileOnly(t *testing.T) {
	minimal, standard := ProfileTools()
	for _, name := range append(minimal, standard...) {
		if name == "get_session_stats" {
			t.Fatal("get_session_stats is in a default profile; its schema would be charged to every session")
		}
	}
	found := false
	for _, name := range FullOnlyTools() {
		found = found || name == "get_session_stats"
	}
	if !found {
		t.Fatal("get_session_stats is not a full-profile tool")
	}

	// The counting still runs where the tool is not exposed.
	fs := newFullServer(t, seedChain, withProfile(config.MCPStandard))
	callTool(t, fs.sess, "find_callers", map[string]any{"name": "leaf"})
	total := int64(0)
	for ss := range fs.srv.SDK().Sessions() {
		usage, _ := fs.srv.usage.peek(ss)
		total += usage.snapshot().Calls
	}
	if total != 1 {
		t.Errorf("the standard profile counted %d call(s), want 1", total)
	}
}

// citedPaths are the distinct file paths an answer named.
func citedPaths(t *testing.T, res *sdk.CallToolResult, into map[string]bool) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	for _, it := range resp.Items {
		if it.Path != "" {
			into[it.Path] = true
		}
	}
}

// seedChain's two files and their indexed sizes.
var chainFileSizes = map[string]int64{"svc/flow.go": 4000, "svc/store.go": 800}

func TestCitedFilesAreCountedOncePerSessionAtTheirIndexedSize(t *testing.T) {
	var log bytes.Buffer
	fs := newFullServer(t, seedChain, withAudit(NewAuditLog(&log)))

	cited := map[string]bool{}
	for i := 0; i < 3; i++ { // the same answers, three times over
		citedPaths(t, callTool(t, fs.sess, "find_callers", map[string]any{"name": "leaf"}), cited)
		citedPaths(t, callTool(t, fs.sess, "find_symbol", map[string]any{"name": "leaf"}), cited)
	}
	var wantFiles, wantBytes int64
	for path := range cited {
		if size, ok := chainFileSizes[path]; ok {
			wantFiles++
			wantBytes += size
		}
	}
	if wantFiles == 0 {
		t.Fatal("the answers named no indexed file; the test would pass vacuously")
	}

	st, text := sessionStatsOf(t, fs.sess)
	if st.CitedFiles == nil {
		t.Fatalf("no cited_files in the report:\n%s", text)
	}
	if st.CitedFiles.Files != wantFiles || st.CitedFiles.Bytes != wantBytes {
		t.Errorf("cited_files = %+v, want %d file(s) and %d bytes: each file once, however many answers named it",
			*st.CitedFiles, wantFiles, wantBytes)
	}
	if st.CitedFiles.EstimatedTokens != (wantBytes+3)/4 {
		t.Errorf("cited estimated_tokens = %d, want %d", st.CitedFiles.EstimatedTokens, (wantBytes+3)/4)
	}
	if !strings.Contains(st.CitedFiles.Meaning, "an upper bound on reading those files whole, not a saving") {
		t.Errorf("the structured figure does not say what it is: %q", st.CitedFiles.Meaning)
	}

	// The comparison is stated as what it is, with the sentence that stops it
	// being read as a saving.
	flat := oneLine(text)
	for _, want := range []string{
		"upper bound on what reading those files whole would have cost",
		"An agent with search would not necessarily have read them",
		"size, when indexed, of the distinct indexed files these answers named",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the cited-files note does not say %q:\n%s", want, text)
		}
	}

	// The audit log carries the same figure as numbers, and summing it over
	// the session gives the session's figure.
	var sumFiles, sumBytes int64
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit line is not JSON: %q", line)
		}
		if ev.Tool == "get_session_stats" {
			continue
		}
		if ev.StructuredBytes == 0 {
			t.Errorf("event for %s has no structured_bytes", ev.Tool)
		}
		sumFiles += ev.CitedFiles
		sumBytes += ev.CitedFileBytes
	}
	if sumFiles != wantFiles || sumBytes != wantBytes {
		t.Errorf("audit events sum to %d file(s), %d bytes; the session report says %d, %d",
			sumFiles, sumBytes, wantFiles, wantBytes)
	}
	// Paths are part of the answer, and the answer is never recorded.
	for path := range cited {
		if strings.Contains(log.String(), path) {
			t.Errorf("the audit log contains the cited path %q:\n%s", path, log.String())
		}
	}
}

func TestCitedFileCountStopsAtItsBoundAndSaysSo(t *testing.T) {
	u := newSessionUsage()
	sizes := make(map[string]int64, maxCitedFilesPerSession+50)
	for i := 0; i < maxCitedFilesPerSession+50; i++ {
		sizes[fmt.Sprintf("pkg/file%d.go", i)] = 10
	}
	files, sizeBytes := u.claimCited(sizes)
	if files != maxCitedFilesPerSession || sizeBytes != 10*maxCitedFilesPerSession {
		t.Errorf("claimed %d file(s), %d bytes; want the bound of %d", files, sizeBytes, maxCitedFilesPerSession)
	}
	if len(u.cited) != maxCitedFilesPerSession {
		t.Errorf("the session remembers %d fingerprints, over the bound of %d", len(u.cited), maxCitedFilesPerSession)
	}
	st := u.snapshot()
	if st.CitedFiles == nil || !st.CitedFiles.Capped {
		t.Errorf("a capped count must be reported as capped: %+v", st.CitedFiles)
	}
	// Claiming the same files again adds nothing.
	if again, _ := u.claimCited(sizes); again != 0 {
		t.Errorf("a second claim of the same files counted %d more", again)
	}
}

// Each session is its own account, and a front end's account says whose it is.
func TestSessionStatsAreKeptPerSession(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()

	callTool(t, fs.sess, "find_callers", map[string]any{"name": "leaf"})

	rest, err := fs.srv.NewInvoker(ctx, "rest")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := rest.Call(ctx, "search_code", json.RawMessage(`{"query":"leaf"}`)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := rest.Call(ctx, "get_session_stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		SessionStats SessionStats `json:"session_stats"`
	}
	if err := json.Unmarshal(res.Structured, &env); err != nil {
		t.Fatal(err)
	}
	if env.SessionStats.Calls != 3 || env.SessionStats.Transport != "rest" {
		t.Errorf("the rest front end reports calls=%d transport=%q, want 3 and rest", env.SessionStats.Calls, env.SessionStats.Transport)
	}
	if !strings.Contains(oneLine(res.Text), `every call made through the "rest" front end since this server started`) {
		t.Errorf("a front end's report must say it is not one conversation:\n%s", res.Text)
	}

	st, text := sessionStatsOf(t, fs.sess)
	if st.Calls != 1 {
		t.Errorf("the MCP session reports %d call(s), want its own 1", st.Calls)
	}
	if !strings.Contains(oneLine(text), "Counts cover this MCP session only") {
		t.Errorf("an MCP session's report must say what it covers:\n%s", text)
	}

	// Closing a front end releases its counters.
	server := rest.server
	_ = rest.Close()
	if usage, _ := fs.srv.usage.peek(server); usage != nil {
		t.Error("a closed front end's counters were kept")
	}
}

// A shell call is a session of one, so its report must not look like a broken
// counter: it says why it is empty and where the history is.
func TestSessionStatsFromTheShellExplainTheEmptySession(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()
	inv, err := fs.srv.NewInvoker(ctx, "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inv.Close() }()

	res, err := inv.Call(ctx, "get_session_stats", nil)
	if err != nil || res.IsError {
		t.Fatalf("get_session_stats: %v\n%s", err, res.Text)
	}
	// An empty session's rows are an empty array, not a missing key.
	if !strings.Contains(string(res.Structured), `"tools":[]`) {
		t.Errorf("an empty session should report \"tools\":[]: %s", res.Structured)
	}
	flat := oneLine(res.Text)
	for _, want := range []string{
		"This session has made no tool calls before this one.",
		"Each 'repocortex tool call' is a session of one call",
		"repocortex stats <file>",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the answer does not say %q:\n%s", want, res.Text)
		}
	}
}

// The counters do not come from the index, so they answer without one.
func TestSessionStatsAnswerWithoutAnIndex(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Defaults()
	cfg.ProjectName = "testproj"
	cfg.MCPProfile = config.MCPFull
	srv, err := New(Deps{Registry: registry.New(db), Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := srv.NewInvoker(ctx, "rest")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inv.Close() }()

	if _, err := inv.Call(ctx, "search_code", json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	res, err := inv.Call(ctx, "get_session_stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a missing index must not fail the usage report:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "calls=1") {
		t.Errorf("the call made before the report was not counted:\n%s", res.Text)
	}
}

// With a clock that advances a fixed step per reading, each call measures one
// step, so the durations can be asserted exactly.
func TestSessionStatsReportTimeSpent(t *testing.T) {
	var log bytes.Buffer
	fs := newFullServer(t, seedChain, withAudit(NewAuditLog(&log)))

	var mu sync.Mutex
	clock := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	fs.srv.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(2500 * time.Microsecond)
		return clock
	}

	callTool(t, fs.sess, "find_callers", map[string]any{"name": "leaf"})
	callTool(t, fs.sess, "find_callers", map[string]any{"name": "middle"})

	st, text := sessionStatsOf(t, fs.sess)
	u, _ := usageOf(st, "find_callers")
	if u.DurationMS != 5 || u.SlowestMS != 2.5 || st.DurationMS != 5 {
		t.Errorf("find_callers took %.3f ms in total, slowest %.3f, session %.3f; want 5, 2.5 and 5\n%s",
			u.DurationMS, u.SlowestMS, st.DurationMS, text)
	}
	if st.Since != "2026-11-12T09:00:00Z" {
		t.Errorf("since = %q, want the start of the first call", st.Since)
	}

	var ev AuditEvent
	first := strings.SplitN(log.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.DurationMS != 2 || ev.Time != "2026-11-12T09:00:00Z" {
		t.Errorf("audit event = %+v, want 2 ms starting at 09:00:00", ev)
	}
}

// A server holds many sessions for a long time. It keeps the active ones.
func TestUsageBookKeepsTheMostRecentlyActiveSessions(t *testing.T) {
	book := newUsageBook()

	first := &sdk.ServerSession{}
	book.forSession(first).record(callMeasure{tool: "find_callers", outcome: outcomeOK})

	const extra = 10
	for i := 0; i < maxSessions+extra; i++ {
		book.forSession(&sdk.ServerSession{})
		// The first session keeps working throughout, so it is never idle.
		book.forSession(first)
	}

	if got := len(book.byID); got != maxSessions {
		t.Errorf("the book holds %d sessions, over the bound of %d", got, maxSessions)
	}
	usage, dropped := book.peek(first)
	if usage == nil || usage.snapshot().Calls != 1 {
		t.Error("the session that stayed active lost its counters to idle ones")
	}
	if dropped != extra+1 {
		t.Errorf("dropped = %d, want %d", dropped, extra+1)
	}

	book.forget(first)
	if usage, _ := book.peek(first); usage != nil {
		t.Error("forget left the session's counters in place")
	}
}

func TestDroppedSessionsAreDisclosed(t *testing.T) {
	fs := newFullServer(t, seedChain)
	for i := 0; i < maxSessions+1; i++ {
		fs.srv.usage.forSession(&sdk.ServerSession{})
	}
	st, text := sessionStatsOf(t, fs.sess)
	if st.DroppedSessions == 0 {
		t.Fatal("the report does not disclose that counters were dropped")
	}
	if !strings.Contains(oneLine(text), "has dropped") {
		t.Errorf("the text does not say counters were dropped:\n%s", text)
	}
}

// Many calls finishing at once in one session must all be counted, with the
// report read while they do. Run under -race.
func TestConcurrentCallsAreAllCounted(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()

	const workers, each = 8, 10
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				name := "find_callers"
				if i%2 == 1 {
					name = "get_session_stats"
				}
				args := map[string]any{}
				if name == "find_callers" {
					args["name"] = "leaf"
				}
				if _, err := fs.sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args}); err != nil {
					t.Errorf("%s: %v", name, err)
				}
			}
		}()
	}
	wg.Wait()

	st, _ := sessionStatsOf(t, fs.sess)
	if st.Calls != workers*each {
		t.Errorf("counted %d call(s), want %d", st.Calls, workers*each)
	}
	if u, _ := usageOf(st, "find_callers"); u.Calls != workers*each/2 {
		t.Errorf("find_callers counted %d, want %d", u.Calls, workers*each/2)
	}
}

func TestEstimateTokensRoundsUp(t *testing.T) {
	for size, want := range map[int64]int64{-5: 0, 0: 0, 1: 1, 4: 1, 5: 2, 8: 2, 4001: 1001} {
		if got := EstimateTokens(size); got != want {
			t.Errorf("EstimateTokens(%d) = %d, want %d", size, got, want)
		}
	}
}

// The report is rendered into every answer's byte budget, so it has to stay
// small however long the session runs.
func TestSessionStatsAnswerStaysSmall(t *testing.T) {
	fs := newFullServer(t, seedChain)
	ctx := context.Background()
	for _, name := range fs.srv.Tools() {
		_, _ = fs.sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: map[string]any{}})
	}
	_, text := sessionStatsOf(t, fs.sess)
	if limit := DefaultLimits().MaxTotalBytes / 2; len(text) > limit {
		t.Errorf("with every tool called the report is %d bytes, over %d", len(text), limit)
	}
	if strings.Contains(text, "truncated:") {
		t.Errorf("the report was truncated:\n%s", text)
	}
}

// An empty answer followed by search_code, or by the same tool asked again,
// is an agent that did not believe "nothing found". The session counts both,
// per tool, and an answer with rows clears the trail.
func TestSessionStatsCountWhatAgentsDoAfterAnEmptyAnswer(t *testing.T) {
	fs := newFullServer(t, seedChain)
	sess := fs.sess
	call := func(name string, args map[string]any) {
		t.Helper()
		callTool(t, sess, name, args)
	}
	call("find_callers", map[string]any{"name": "NoSuchThing"}) // empty
	call("search_code", map[string]any{"query": "NoSuchThing"}) // -> search after empty
	call("find_symbol", map[string]any{"name": "NoSuchThing"})  // empty
	call("find_symbol", map[string]any{"name": "NoSuchOther"})  // -> retry after empty (and empty again)
	call("find_symbol", map[string]any{"name": "leaf"})

	st, text := sessionStatsOf(t, sess)
	// The last find_symbol follows an empty find_symbol too: two retries.
	if st.SearchAfterEmpty != 1 || st.RetryAfterEmpty != 2 {
		t.Errorf("search after empty %d, retry after empty %d; want 1 and 2\n%s", st.SearchAfterEmpty, st.RetryAfterEmpty, text)
	}
	if !strings.Contains(text, "empty_answers: 4, then search_code 1, then the same tool again 2") {
		t.Errorf("the text does not report it:\n%s", text)
	}
	if fc, _ := usageOf(st, "find_callers"); fc.EmptyAnswers != 1 || fc.FollowedUp != 1 {
		t.Errorf("find_callers: %+v; want one empty answer, followed up", fc)
	}
	if fs, _ := usageOf(st, "find_symbol"); fs.EmptyAnswers != 2 || fs.FollowedUp != 2 {
		t.Errorf("find_symbol: %+v; want two empty answers, both followed up", fs)
	}
}
