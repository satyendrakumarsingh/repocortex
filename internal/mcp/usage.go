package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------- session usage
//
// WHAT A SESSION USED, COUNTED AND NOT INTERPRETED.
//
// Other code-graph tools answer "what did this session cost?" with a number of
// tokens saved. RepoCortex has run one agent benchmark, and it found no token
// advantage on simple lookups (docs/BENCHMARKING.md, section 5). A saving
// printed here would be a claim with no measurement behind it, so this file
// reports only what the server itself observed:
//
//   - how many calls each tool answered, and how many failed;
//   - how large the answers were, in bytes, and the bytes / 4 token estimate
//     of that size, labelled as an estimate;
//   - how long the handlers ran;
//   - what the exposed tool definitions cost on every model request;
//   - the size of the indexed files the answers named, stated as an upper
//     bound on reading them whole and never as a saving.
//
// The last figure is the only comparison, and it is deliberately one-sided. It
// says how much text the answers POINTED AT. It does not say what an agent
// without RepoCortex would have read, because nobody measured that: an agent
// with search may open none of those files, or a few lines of each.
//
// BOUNDED BY CONSTRUCTION. A server may run for weeks and hold many sessions.
// So the state is one small counter block per tool per session (a tool name
// the server did not register is counted under one shared label, since the
// name is the client's to choose), a capped set of file fingerprints per
// session, and a capped number of sessions. Nothing here keeps a list of calls;
// the audit log is the place for that, and it is a file the user asked for.
//
// PER SESSION, NEVER PERSISTED, for the reason dedup.go gives for its ledger:
// the counters describe one conversation and die with it.

const (
	// tokenBytes is the divisor of the token estimate: bytes / 4, rounded up.
	// It is the estimate `repocortex tool list` prints for a tool's schema,
	// and the divisor internal/context budgets a pack with. One divisor
	// everywhere, so two numbers on one screen never disagree about what a
	// token is.
	tokenBytes = 4

	// EstimateMethod says how every token figure in a usage report was
	// arrived at. It travels with the numbers so a reader does not have to
	// find the documentation to learn that no tokenizer was run.
	EstimateMethod = "bytes / 4, rounded up; no tokenizer is run and the true count depends on the model"

	// unregisteredTool is the one label for every call that named a tool this
	// profile does not expose. Keying the counters by whatever name a client
	// sent would let a client grow the map without limit.
	unregisteredTool = "(unregistered)"

	// maxCitedFilesPerSession bounds the fingerprints kept to count each cited
	// file once. Past it, a file the session has not seen before is no longer
	// counted, and the report says so: the figure then understates, which is
	// the safe direction for a number that could be mistaken for a benefit.
	maxCitedFilesPerSession = 4096
)

// EstimateTokens is the token estimate for a size in bytes: bytes / 4, rounded
// up. It is an estimate and every figure derived from it is labelled as one.
func EstimateTokens(sizeBytes int64) int64 {
	if sizeBytes <= 0 {
		return 0
	}
	return (sizeBytes + tokenBytes - 1) / tokenBytes
}

// SessionStats is what get_session_stats reports about the calling session.
type SessionStats struct {
	// Transport is how the session reaches the server: "mcp" for a protocol
	// client, otherwise the name of an in-process front end ("cli", "rest").
	Transport string `json:"transport"`
	// Since is when the first counted call started (RFC 3339, UTC). Absent
	// when nothing has been counted.
	Since string `json:"since,omitempty"`

	Calls    int64 `json:"calls"`
	Failures int64 `json:"failures"`

	// ResultBytes is the size of the text of every answer. StructuredBytes is
	// the size of the structured copy of the same answers, which a client may
	// forward to its model instead of, or as well as, the text.
	ResultBytes           int64 `json:"result_bytes"`
	EstimatedResultTokens int64 `json:"estimated_result_tokens"`
	StructuredBytes       int64 `json:"structured_bytes"`

	// DurationMS is the time the server spent answering, in milliseconds.
	DurationMS float64 `json:"duration_ms"`

	// Tools has one row per tool called, most calls first. Empty, never
	// absent, so a reader of the JSON needs no nil check.
	Tools []ToolUsage `json:"tools"`

	// ToolDefinitions is the fixed cost of exposing the active profile.
	ToolDefinitions *ToolDefinitionCost `json:"tool_definitions,omitempty"`

	// CitedFiles is the one comparison this report makes. See CitedFiles.
	CitedFiles *CitedFiles `json:"cited_files,omitempty"`

	// EmptyAnswers counts answers with no rows. SearchAfterEmpty counts the
	// ones the agent followed with search_code, and RetryAfterEmpty the ones
	// it followed by asking the same tool again: the two moves of an agent
	// that did not trust "nothing found" and went looking another way. A tool
	// whose empty answers are often followed like this is one whose answer,
	// or description, agents do not believe. An agent's own grep is outside
	// the server and not counted; the audit log carries the same flag per
	// call (`empty`) for that comparison to be made offline.
	EmptyAnswers     int64 `json:"empty_answers"`
	SearchAfterEmpty int64 `json:"search_after_empty"`
	RetryAfterEmpty  int64 `json:"retry_after_empty"`

	// DroppedSessions is how many idle sessions' counters this server has
	// discarded to bound its memory. When it is not zero, a session that was
	// idle for a long time may have had its counting restarted at Since.
	DroppedSessions int64 `json:"dropped_sessions,omitempty"`

	// TokenBasis and TokenEstimate say what standing every token figure above
	// has, and how it was computed.
	TokenBasis    Basis  `json:"token_basis"`
	TokenEstimate string `json:"token_estimate"`
}

// ToolUsage is one tool's share of a session.
type ToolUsage struct {
	Tool  string `json:"tool"`
	Calls int64  `json:"calls"`
	// ToolErrors are answers flagged isError; ProtocolErrors are calls that
	// never produced an answer (an unknown tool, a cancelled request).
	ToolErrors            int64   `json:"tool_errors"`
	ProtocolErrors        int64   `json:"protocol_errors"`
	ResultBytes           int64   `json:"result_bytes"`
	EstimatedResultTokens int64   `json:"estimated_result_tokens"`
	StructuredBytes       int64   `json:"structured_bytes"`
	DurationMS            float64 `json:"duration_ms"`
	SlowestMS             float64 `json:"slowest_ms"`
	// EmptyAnswers is how many of this tool's answers had no rows, and
	// FollowedUp how many of those the agent followed with search_code or
	// the same tool again (see SessionStats.SearchAfterEmpty).
	EmptyAnswers int64 `json:"empty_answers,omitempty"`
	FollowedUp   int64 `json:"followed_up,omitempty"`
}

// ToolDefinitionCost is what the exposed tools cost a model's context.
//
// It is a cost per MODEL REQUEST, not per tool call and not per session: a
// client places every exposed tool's name, description and input schema in the
// context of each request it sends. The server cannot see how many requests a
// client's model made, so it reports the rate and no total.
type ToolDefinitionCost struct {
	Profile                   string `json:"profile"`
	Tools                     int    `json:"tools"`
	SchemaBytes               int    `json:"schema_bytes"`
	EstimatedTokensPerRequest int    `json:"estimated_tokens_per_request"`
}

// CitedFiles is the size of the indexed files a session's answers named.
//
// IT IS AN UPPER BOUND ON READING THOSE FILES WHOLE, AND NOTHING ELSE. Each
// file is counted once however many answers named it, at the size recorded
// when it was indexed. It is not a saving and must not be presented as one:
// an agent with search would not necessarily have opened these files, and
// could have read a few lines of each.
type CitedFiles struct {
	Files           int64 `json:"files"`
	Bytes           int64 `json:"bytes"`
	EstimatedTokens int64 `json:"estimated_tokens"`
	// Capped is set when the session named more distinct files than the
	// server remembers, so later files were not counted and Bytes understates.
	Capped bool `json:"capped,omitempty"`
	// Meaning says what the figure is, inside the object that carries it. A
	// script that reads this JSON never sees the notes in the text or the
	// documentation, and a bare byte count beside result_bytes is exactly
	// what gets subtracted and called a saving.
	Meaning string `json:"meaning"`
}

// What a cited-files figure is, as it travels with the figure. The audit
// summary adds how its total differs from one session's.
const (
	citedFilesMeaning = "size when indexed of the distinct indexed files the answers named, " +
		"each counted once per session; an upper bound on reading those files whole, not a saving"
	citedFilesSummedMeaning = citedFilesMeaning + "; summed over every session in the log, " +
		"and each 'repocortex tool call' is a session of its own"
)

// callMeasure is one finished call, as the middleware observed it.
type callMeasure struct {
	tool            string
	outcome         string
	resultBytes     int64
	structuredBytes int64
	elapsed         time.Duration
	started         time.Time
	// empty is an answer that was given and had no rows.
	empty bool
}

// Outcomes a call can have, shared with the audit log.
const (
	outcomeOK            = "ok"
	outcomeToolError     = "tool_error"
	outcomeProtocolError = "protocol_error"
)

// toolCounters is one tool's aggregate within one session.
type toolCounters struct {
	calls, toolErrors, protocolErrors int64
	resultBytes, resultTokens         int64
	structuredBytes                   int64
	total, slowest                    time.Duration
	empty, followedUp                 int64
}

// sessionUsage is one session's counters.
//
// Guarded by its own mutex: two calls in one session legitimately finish at
// the same moment, and get_session_stats reads while they do.
type sessionUsage struct {
	mu     sync.Mutex
	since  time.Time
	byTool map[string]*toolCounters

	cited       map[uint64]struct{}
	citedFiles  int64
	citedBytes  int64
	citedCapped bool

	// lastEmpty is the tool whose answer, the session's previous call, had no
	// rows; "" when that answer had rows.
	lastEmpty                      string
	empty, searchAfter, retryAfter int64
}

func newSessionUsage() *sessionUsage {
	return &sessionUsage{byTool: make(map[string]*toolCounters), cited: make(map[uint64]struct{})}
}

// record adds one finished call. m.tool must already be a registered name or
// unregisteredTool; that is what keeps byTool bounded.
func (u *sessionUsage) record(m callMeasure) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.since.IsZero() || m.started.Before(u.since) {
		u.since = m.started
	}
	c, ok := u.byTool[m.tool]
	if !ok {
		c = &toolCounters{}
		u.byTool[m.tool] = c
	}
	c.calls++
	switch m.outcome {
	case outcomeToolError:
		c.toolErrors++
	case outcomeProtocolError:
		c.protocolErrors++
	}
	c.resultBytes += m.resultBytes
	// Estimated per call and summed, so the per-tool rows add up to the
	// session total exactly and to what `repocortex stats` computes from the
	// audit log for the same calls.
	c.resultTokens += EstimateTokens(m.resultBytes)
	c.structuredBytes += m.structuredBytes
	c.total += m.elapsed
	if m.elapsed > c.slowest {
		c.slowest = m.elapsed
	}

	// What the agent did next after an empty answer.
	if prev := u.lastEmpty; prev != "" {
		followed := false
		switch {
		case m.tool == searchCodeTool && prev != searchCodeTool:
			u.searchAfter++
			followed = true
		case m.tool == prev:
			u.retryAfter++
			followed = true
		}
		if pc, ok := u.byTool[prev]; ok && followed {
			pc.followedUp++
		}
	}
	u.lastEmpty = ""
	if m.empty {
		c.empty++
		u.empty++
		u.lastEmpty = m.tool
	}
}

// searchCodeTool is the text search an agent turns to when a structural
// answer comes back empty.
const searchCodeTool = "search_code"

// pathFingerprint identifies a cited path in eight bytes, so remembering four
// thousand of them costs kilobytes rather than their text. A collision makes
// two files count as one, which understates.
func pathFingerprint(path string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	return h.Sum64()
}

// uncited returns the paths this session has not counted yet, in order and
// without repeats.
func (u *sessionUsage) uncited(paths []string) []string {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	var out []string
	inCall := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, dup := inCall[p]; dup {
			continue
		}
		inCall[p] = struct{}{}
		if _, seen := u.cited[pathFingerprint(p)]; !seen {
			out = append(out, p)
		}
	}
	return out
}

// claimCited counts each file in sizes that the session has not counted
// before, and returns how many it counted and their total size.
//
// The check is repeated here, under the lock, because two concurrent calls can
// both find the same file uncited; only one of them may count it.
func (u *sessionUsage) claimCited(sizes map[string]int64) (files, sizeBytes int64) {
	if u == nil || len(sizes) == 0 {
		return 0, 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	for path, size := range sizes {
		fp := pathFingerprint(path)
		if _, seen := u.cited[fp]; seen {
			continue
		}
		if len(u.cited) >= maxCitedFilesPerSession {
			u.citedCapped = true
			continue
		}
		u.cited[fp] = struct{}{}
		files++
		sizeBytes += max(size, 0)
	}
	u.citedFiles += files
	u.citedBytes += sizeBytes
	return files, sizeBytes
}

// milliseconds renders a duration as milliseconds to the microsecond. Most
// answers take a few milliseconds and many take less than one, so whole
// milliseconds would report a session of fast calls as taking no time.
func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// snapshot copies the counters into a report. Nil reports an empty session.
func (u *sessionUsage) snapshot() SessionStats {
	out := SessionStats{Tools: []ToolUsage{}, TokenBasis: BasisTokenCount, TokenEstimate: EstimateMethod}
	if u == nil {
		return out
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	if !u.since.IsZero() {
		out.Since = u.since.UTC().Format(time.RFC3339)
	}
	var total time.Duration
	for name, c := range u.byTool {
		out.Tools = append(out.Tools, ToolUsage{
			Tool:                  name,
			Calls:                 c.calls,
			ToolErrors:            c.toolErrors,
			ProtocolErrors:        c.protocolErrors,
			ResultBytes:           c.resultBytes,
			EstimatedResultTokens: c.resultTokens,
			StructuredBytes:       c.structuredBytes,
			DurationMS:            milliseconds(c.total),
			SlowestMS:             milliseconds(c.slowest),
			EmptyAnswers:          c.empty,
			FollowedUp:            c.followedUp,
		})
		out.Calls += c.calls
		out.Failures += c.toolErrors + c.protocolErrors
		out.ResultBytes += c.resultBytes
		out.EstimatedResultTokens += c.resultTokens
		out.StructuredBytes += c.structuredBytes
		total += c.total
	}
	out.DurationMS = milliseconds(total)
	out.EmptyAnswers, out.SearchAfterEmpty, out.RetryAfterEmpty = u.empty, u.searchAfter, u.retryAfter
	sort.Slice(out.Tools, func(i, j int) bool {
		if out.Tools[i].Calls != out.Tools[j].Calls {
			return out.Tools[i].Calls > out.Tools[j].Calls
		}
		return out.Tools[i].Tool < out.Tools[j].Tool
	})
	if u.citedFiles > 0 || u.citedCapped {
		out.CitedFiles = &CitedFiles{
			Files:           u.citedFiles,
			Bytes:           u.citedBytes,
			EstimatedTokens: EstimateTokens(u.citedBytes),
			Capped:          u.citedCapped,
			Meaning:         citedFilesMeaning,
		}
	}
	return out
}

// usageBook holds one sessionUsage per live session, bounded.
//
// It keeps the maxSessions most recently active. The ledger book in dedup.go
// drops the OLDEST session; this one drops the IDLEST, because the session a
// long-running HTTP server opened first is its REST front end, which never
// closes and is exactly the one whose counters a person will ask for.
type usageBook struct {
	mu      sync.Mutex
	byID    map[*sdk.ServerSession]*usageEntry
	clock   int64
	dropped int64
}

type usageEntry struct {
	usage   *sessionUsage
	touched int64
}

func newUsageBook() *usageBook {
	return &usageBook{byID: make(map[*sdk.ServerSession]*usageEntry)}
}

// forSession returns the session's counters, creating them on first use. A nil
// session (a handler called directly) has none, and a nil *sessionUsage
// ignores every write.
func (b *usageBook) forSession(ss *sdk.ServerSession) *sessionUsage {
	if b == nil || ss == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.clock++
	if e, ok := b.byID[ss]; ok {
		e.touched = b.clock
		return e.usage
	}
	e := &usageEntry{usage: newSessionUsage(), touched: b.clock}
	b.byID[ss] = e
	for len(b.byID) > maxSessions {
		var idlest *sdk.ServerSession
		oldest := b.clock + 1
		for id, cand := range b.byID {
			if cand.touched < oldest {
				idlest, oldest = id, cand.touched
			}
		}
		delete(b.byID, idlest)
		b.dropped++
	}
	return e.usage
}

// peek returns the session's counters without creating any, and how many
// sessions' counters the book has dropped so far.
func (b *usageBook) peek(ss *sdk.ServerSession) (*sessionUsage, int64) {
	if b == nil {
		return nil, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.byID[ss]; ok && ss != nil {
		return e.usage, b.dropped
	}
	return nil, b.dropped
}

// forget releases a session's counters. Called when an in-process front end
// closes; a protocol session that ends without telling the server is released
// by the bound in forSession instead.
func (b *usageBook) forget(ss *sdk.ServerSession) {
	if b == nil || ss == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.byID, ss)
}

// ---------------------------------------------------------------- measuring

// callScratch carries what only the handler knows about one call out to the
// middleware that records it. The handler side is Server.reply.
type callScratch struct {
	citedFiles     int64
	citedFileBytes int64
	// answered and rows describe the Response the call returned, set by
	// noteCitations, which every answer passes through.
	answered bool
	rows     int
}

type callScratchKey struct{}

func scratchFrom(ctx context.Context) *callScratch {
	sc, _ := ctx.Value(callScratchKey{}).(*callScratch)
	return sc
}

// usageMiddleware measures every tools/call once, and hands the measurement to
// the session's counters and, when one is configured, to the audit log.
//
// One measurement feeding both is the point: a report that said a session made
// twelve calls beside a log that recorded eleven would make both useless. The
// counters are always kept, because they cost a map update; the log is written
// only when the user asked for one.
func (s *Server) usageMiddleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		if method != methodCallTool {
			return next(ctx, method, req)
		}
		call, ok := req.(*sdk.CallToolRequest)
		if !ok || call.Params == nil {
			return next(ctx, method, req)
		}

		scratch := &callScratch{}
		start := s.now()
		res, err := next(context.WithValue(ctx, callScratchKey{}, scratch), method, req)
		elapsed := s.now().Sub(start)

		m := callMeasure{tool: call.Params.Name, outcome: outcomeOK, elapsed: elapsed, started: start}
		switch {
		case err != nil:
			m.outcome = outcomeProtocolError
		default:
			if tr, ok := res.(*sdk.CallToolResult); ok && tr != nil {
				if tr.IsError {
					m.outcome = outcomeToolError
				}
				for _, c := range tr.Content {
					if t, ok := c.(*sdk.TextContent); ok {
						m.resultBytes += int64(len(t.Text))
					}
				}
				m.structuredBytes = structuredSize(tr.StructuredContent)
			}
		}

		m.empty = m.outcome == outcomeOK && scratch.answered && scratch.rows == 0
		counted := m
		if !s.hasTool(counted.tool) {
			counted.tool = unregisteredTool
		}
		s.usage.forSession(call.Session).record(counted)

		if log := s.deps.Audit; log != nil {
			log.Record(s.auditEvent(call, m, scratch))
		}
		return res, err
	}
}

// structuredSize is the size of a result's structured copy. The SDK has
// already marshalled it by the time the middleware sees the result, so this
// reads a length and encodes nothing.
func structuredSize(v any) int64 {
	if raw, ok := v.(json.RawMessage); ok {
		return int64(len(raw))
	}
	return 0
}

// noteCitations counts the indexed files an answer names, each once per
// session, and reports the count to the middleware for the audit event.
//
// Called by reply, on the response exactly as it is about to be sent: after
// the bounds, so a file that was trimmed out of the answer is not counted as
// cited by it.
//
// A failure here is swallowed. This is bookkeeping about an answer that is
// already correct, and refusing to send the answer because its size could not
// be tallied would put the report above the thing it reports on.
func (s *Server) noteCitations(ctx context.Context, req *sdk.CallToolRequest, r *Response) {
	if sc := scratchFrom(ctx); sc != nil && r != nil {
		sc.answered, sc.rows = true, len(r.Items)
	}
	ss := sessionOf(req)
	if ss == nil || r == nil || r.IndexRevision <= 0 || len(r.Items) == 0 {
		return
	}
	paths := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		paths = append(paths, it.Path)
	}
	usage := s.usage.forSession(ss)
	fresh := usage.uncited(paths)
	if len(fresh) == 0 {
		return
	}
	// A path that is not a file in this run (a directory, a name clipped by
	// the bounds) is absent from the result and so is not counted.
	sizes, err := s.deps.Registry.FileSizes(ctx, r.IndexRevision, fresh)
	if err != nil {
		return
	}
	files, sizeBytes := usage.claimCited(sizes)
	if sc := scratchFrom(ctx); sc != nil {
		sc.citedFiles += files
		sc.citedFileBytes += sizeBytes
	}
}

// ---------------------------------------------------------------- the tool

// registerSessionStats adds get_session_stats.
//
// IT IS A FULL-PROFILE TOOL, AND THAT IS THE DESIGN. Every exposed tool's
// definition is charged to every model request of every session. A tool that
// reports what a session cost would, on the minimal or standard profile, raise
// that cost for all the sessions that never ask. The counting itself runs on
// every profile; what is opt-in is spending context on the means to ask. A
// person does not need the tool at all: `repocortex stats` reads the audit
// log, and `repocortex tool call` uses the full profile by default.
func (s *Server) registerSessionStats(allowed map[string]bool) {
	addTool(s, allowed, &sdk.Tool{
		Name:        "get_session_stats",
		Description: toolDescriptions()["get_session_stats"],
	}, s.getSessionStats)
}

func (s *Server) getSessionStats(ctx context.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, *Response, error) {
	// The counters do not come from the index, so a missing index is not a
	// reason to withhold them. The revision is reported when there is one
	// because every answer carries it.
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		runID = 0
	}

	ss := sessionOf(req)
	usage, dropped := s.usage.peek(ss)
	stats := usage.snapshot()
	stats.DroppedSessions = dropped
	stats.Transport = "mcp"
	frontEnd := ""
	if ss != nil {
		if fe, ok := s.frontEnds.Load(ss); ok {
			if f, ok := fe.(frontEndInfo); ok {
				frontEnd = f.name
				stats.Transport = frontEnd
			}
		}
	}

	resp := &Response{IndexRevision: runID, IndexStatus: status, SessionStats: &stats}

	if cost, err := s.toolDefinitionCost(ctx); err == nil {
		stats.ToolDefinitions = &cost
	} else {
		resp.Notes = append(resp.Notes,
			"The cost of the tool definitions could not be computed for this answer; "+
				"'repocortex tool list' prints it.")
	}

	if stats.Calls == 0 {
		resp.Summary = "This session has made no tool calls before this one."
	} else {
		resp.Summary = fmt.Sprintf(
			"This session has made %d tool call(s) before this one, %d of which failed. "+
				"Their answers totalled %d bytes of text, an estimated %d tokens.",
			stats.Calls, stats.Failures, stats.ResultBytes, stats.EstimatedResultTokens)
	}

	resp.Notes = append(resp.Notes,
		"This is usage, not a saving. RepoCortex has no measurement that supports a token "+
			"saving, so none is reported and none should be inferred from these figures.",
		"Token figures are estimates: "+EstimateMethod+". Sizes count each answer's text; "+
			"structured_bytes is the structured copy of the same answers, which a client may "+
			"also forward to its model.")
	if stats.ToolDefinitions != nil {
		resp.Notes = append(resp.Notes,
			"tool_definitions is paid on every model request while this profile is exposed, not "+
				"once per call. The server cannot see how many requests the client's model made, "+
				"so no session total is given.")
	}
	if stats.CitedFiles != nil {
		note := "cited_files is the size, when indexed, of the distinct indexed files these answers " +
			"named. It is an upper bound on what reading those files whole would have cost. An " +
			"agent with search would not necessarily have read them, and could have read parts."
		if stats.CitedFiles.Capped {
			note += fmt.Sprintf(" Only the first %d distinct files were counted, so it understates.",
				maxCitedFilesPerSession)
		}
		resp.Notes = append(resp.Notes, note)
	}
	resp.Notes = append(resp.Notes, sessionScopeNote(frontEnd, ss != nil))
	if dropped > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"This server keeps counters for its %d most recently active sessions and has dropped "+
				"%d idle one(s). If this session was idle for long, its counting restarted at 'since'.",
			maxSessions, dropped))
	}
	return s.reply(ctx, req, resp)
}

// sessionScopeNote says what "this session" covers, which differs by front
// end: a conversation, one shell command, or everything a REST listener has
// served.
func sessionScopeNote(frontEnd string, hasSession bool) string {
	switch {
	case !hasSession:
		return "This call arrived without a session, so there is nothing to count."
	case frontEnd == "":
		return "Counts cover this MCP session only and are discarded when it ends. To keep a " +
			"record, start the server with --audit-log and summarise it with 'repocortex stats'."
	case frontEnd == "cli":
		return "Each 'repocortex tool call' is a session of one call, so there is nothing before " +
			"this one to count. To summarise calls across commands, record them with --audit-log " +
			"(or REPOCORTEX_AUDIT_LOG) and run 'repocortex stats <file>'."
	default:
		return fmt.Sprintf("Counts cover every call made through the %q front end since this server "+
			"started, from every caller, and not one conversation.", frontEnd)
	}
}

// toolDefinitionCost is the schema cost of the tools this server exposes.
//
// Computed by listing the tools through an in-process client, the same path
// `repocortex tool list` takes, so the two can never print different numbers
// for one profile. Done once: the tool set is fixed when the server is built.
func (s *Server) toolDefinitionCost(ctx context.Context) (ToolDefinitionCost, error) {
	s.definitionMu.Lock()
	defer s.definitionMu.Unlock()
	if s.definitionCost != nil {
		return *s.definitionCost, nil
	}

	inv, err := s.NewInvoker(ctx, "internal")
	if err != nil {
		return ToolDefinitionCost{}, err
	}
	defer func() { _ = inv.Close() }()
	tools, err := inv.Tools(ctx)
	if err != nil {
		return ToolDefinitionCost{}, err
	}

	cost := ToolDefinitionCost{Profile: string(s.activeProfile()), Tools: len(tools)}
	for _, t := range tools {
		cost.SchemaBytes += t.SchemaBytes
		cost.EstimatedTokensPerRequest += t.EstimatedTokens
	}
	s.definitionCost = &cost
	return cost, nil
}

// activeProfile names the profile this server was built with. An unset or
// unknown profile is served as minimal (see profileTools), so it is reported
// as minimal.
func (s *Server) activeProfile() config.MCPProfile {
	switch p := s.deps.Config.MCPProfile; p {
	case config.MCPStandard, config.MCPFull:
		return p
	default:
		return config.MCPMinimal
	}
}

// renderSessionStats is the text form of a usage report, one line per fact in
// key=value form: an agent reads it as easily as a person, and it stays inside
// SYSTEM METADATA because every figure is RepoCortex's own reporting.
func renderSessionStats(st *SessionStats) string {
	if st == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "session: transport=%s calls=%d failed=%d answer_bytes=%d est_answer_tokens=%d structured_bytes=%d time_ms=%.1f",
		st.Transport, st.Calls, st.Failures, st.ResultBytes, st.EstimatedResultTokens, st.StructuredBytes, st.DurationMS)
	if st.Since != "" {
		b.WriteString(" since=" + st.Since)
	}
	b.WriteString("\n")
	if d := st.ToolDefinitions; d != nil {
		fmt.Fprintf(&b, "tool_definitions: profile=%s tools=%d schema_bytes=%d est_tokens_per_request=%d\n",
			d.Profile, d.Tools, d.SchemaBytes, d.EstimatedTokensPerRequest)
	}
	for _, t := range st.Tools {
		fmt.Fprintf(&b, "usage: %s calls=%d failed=%d answer_bytes=%d est_answer_tokens=%d time_ms=%.1f slowest_ms=%.1f",
			t.Tool, t.Calls, t.ToolErrors+t.ProtocolErrors, t.ResultBytes, t.EstimatedResultTokens, t.DurationMS, t.SlowestMS)
		if t.EmptyAnswers > 0 {
			fmt.Fprintf(&b, " empty=%d followed_up=%d", t.EmptyAnswers, t.FollowedUp)
		}
		b.WriteString("\n")
	}
	if st.EmptyAnswers > 0 {
		fmt.Fprintf(&b, "empty_answers: %d, then search_code %d, then the same tool again %d\n",
			st.EmptyAnswers, st.SearchAfterEmpty, st.RetryAfterEmpty)
	}
	if c := st.CitedFiles; c != nil {
		fmt.Fprintf(&b, "cited_files: files=%d bytes=%d est_tokens=%d", c.Files, c.Bytes, c.EstimatedTokens)
		if c.Capped {
			b.WriteString(" capped=true")
		}
		b.WriteString("\n")
	}
	return b.String()
}
