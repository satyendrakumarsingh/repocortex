package mcp

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------- audit log
//
// WHO ASKED THE INDEX WHAT, AND WHEN.
//
// An agent session reads the structure of a private codebase. A team that has
// to answer "what did the agent look at before it made this change?" needs a
// record that does not depend on the agent's own transcript, and a security
// review needs one before it will approve the tool at all.
//
// The log is opt-in and local: one JSON object per line, appended to a file
// the user names, never sent anywhere. It records the call, not the answer —
// the answer is repository content, and copying it into a second file would
// double the places private source can leak from.

// AuditEvent is one tool call.
type AuditEvent struct {
	Time string `json:"time"` // RFC 3339, UTC
	// Session distinguishes concurrent agents on one server. It is the
	// transport's session id when there is one (HTTP), otherwise empty.
	Session string `json:"session,omitempty"`
	// Transport is how the call arrived: "mcp" for a protocol client, or the
	// name of an in-process front end ("rest", "cli", "portal").
	Transport string `json:"transport"`
	// TokenName is the NAME of the token that made the call, on a server
	// started with a tokens file: the label the operator gave the entry. The
	// token is never recorded and neither is its hash, because a log holding
	// either would be a second place to take a credential from.
	TokenName string `json:"token_name,omitempty"`
	Tool      string `json:"tool"`
	// Arguments are the call's arguments as sent, cut at auditArgBytes.
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	ArgsCut    bool            `json:"arguments_truncated,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	// Outcome is "ok", "tool_error" (the tool answered with isError) or
	// "protocol_error" (the call never reached a handler).
	Outcome string `json:"outcome"`
	// ResultBytes is the size of the answer's text. StructuredBytes is the
	// size of the structured copy of the same answer. Both are sizes only:
	// the answer itself is never recorded.
	ResultBytes     int64 `json:"result_bytes"`
	StructuredBytes int64 `json:"structured_bytes,omitempty"`
	// CitedFiles and CitedFileBytes are how many indexed files this answer
	// named that no earlier answer in the same session had named, and their
	// total size when indexed. Numbers only; the paths are not recorded.
	// Summed over a session they are the size of the distinct files its
	// answers pointed at, which is an upper bound on reading those files whole
	// and not a saving. See usage.go.
	CitedFiles     int64 `json:"cited_files,omitempty"`
	CitedFileBytes int64 `json:"cited_file_bytes,omitempty"`
	// Empty is an answer with no rows. With the next event of the same
	// session it shows whether the agent believed it (see
	// SessionStats.SearchAfterEmpty).
	Empty bool `json:"empty,omitempty"`
}

// auditArgBytes bounds the arguments kept per event. A task description or a
// pasted diff can be large, and an audit log that grows by a megabyte per call
// is one that gets turned off.
const auditArgBytes = 2048

// auditToolBytes bounds the tool name kept per event. A registered name is a
// few words; a longer one is something a client invented, and the name is the
// one field of an event the client writes in full.
const auditToolBytes = 128

// AuditLog appends events to a writer, one JSON line each.
//
// Safe for concurrent use: an HTTP server answers several sessions at once and
// interleaved partial lines would make the file unparseable.
type AuditLog struct {
	mu sync.Mutex
	w  io.Writer
}

// NewAuditLog returns a log writing to w. The caller owns w and closes it.
func NewAuditLog(w io.Writer) *AuditLog {
	return &AuditLog{w: w}
}

// Record writes one event. A write failure is dropped: refusing to answer an
// agent because a log disk is full would turn an observability feature into an
// outage, and the caller has no better place to report it.
func (a *AuditLog) Record(ev AuditEvent) {
	if a == nil || a.w == nil {
		return
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.w.Write(append(line, '\n'))
}

// auditEvent builds the record of one measured call. The measuring is done
// once, in usageMiddleware, and shared with the session counters.
func (s *Server) auditEvent(call *sdk.CallToolRequest, m callMeasure, scratch *callScratch) AuditEvent {
	ev := AuditEvent{
		Time:            m.started.UTC().Format(time.RFC3339),
		Transport:       s.transportOf(call),
		TokenName:       s.tokenNameOf(call),
		Tool:            clip(m.tool, auditToolBytes),
		DurationMS:      m.elapsed.Milliseconds(),
		Outcome:         m.outcome,
		Empty:           m.empty,
		ResultBytes:     m.resultBytes,
		StructuredBytes: m.structuredBytes,
	}
	if scratch != nil {
		ev.CitedFiles, ev.CitedFileBytes = scratch.citedFiles, scratch.citedFileBytes
	}
	if call.Session != nil {
		ev.Session = call.Session.ID()
	}
	ev.Arguments, ev.ArgsCut = auditArguments(call.Params.Arguments)
	return ev
}

// auditArguments is a call's arguments as the log keeps them: whole when
// small, cut and marked when not, and dropped when they are not JSON.
func auditArguments(args json.RawMessage) (kept json.RawMessage, cut bool) {
	switch {
	case len(args) == 0:
		return nil, false
	case len(args) > auditArgBytes:
		// A cut JSON document is not JSON, so it is stored as a string.
		s, _ := json.Marshal(string(args[:auditArgBytes]))
		return s, true
	case json.Valid(args):
		return args, false
	}
	return nil, false
}

// recordUnknownTool records a call from an in-process front end that named no
// tool this server has.
//
// Over the protocol such a call reaches the middleware above and is recorded
// as a protocol_error. A front end answers it before the protocol is involved
// (Invoker.Call), which used to leave no trace: the one kind of call missing
// from the log was the one that asked for something the caller may not have.
// On a shared server that is a token probing outside its scope, which is
// exactly what the log is read for.
func (s *Server) recordUnknownTool(ss *sdk.ServerSession, name string, args json.RawMessage) {
	log := s.deps.Audit
	if log == nil {
		return
	}
	ev := AuditEvent{
		Time:      s.now().UTC().Format(time.RFC3339),
		Transport: "mcp",
		Tool:      clip(name, auditToolBytes),
		Outcome:   "protocol_error",
	}
	if fe, ok := s.frontEnds.Load(ss); ok {
		if f, ok := fe.(frontEndInfo); ok {
			ev.Transport, ev.TokenName = f.name, f.tokenName
		}
	}
	ev.Arguments, ev.ArgsCut = auditArguments(args)
	log.Record(ev)
}

// transportOf names the front end a call arrived through.
func (s *Server) transportOf(call *sdk.CallToolRequest) string {
	if call != nil && call.Session != nil {
		if fe, ok := s.frontEnds.Load(call.Session); ok {
			if f, ok := fe.(frontEndInfo); ok {
				return f.name
			}
		}
	}
	return "mcp"
}

// tokenNameOf names the token behind a call, or "" on a server with no named
// tokens.
//
// Both sources are set by this process and neither by the caller. An
// in-process session carries the name it was opened with (NewInvokerAs). A
// call over HTTP carries the identity the HTTP guard attached after checking
// the bearer token; it is not a field of the request a client sends, and over
// stdio there is none.
func (s *Server) tokenNameOf(call *sdk.CallToolRequest) string {
	if call == nil {
		return ""
	}
	if call.Session != nil {
		if fe, ok := s.frontEnds.Load(call.Session); ok {
			if f, ok := fe.(frontEndInfo); ok {
				return f.tokenName
			}
			return ""
		}
	}
	if call.Extra != nil && call.Extra.TokenInfo != nil {
		return call.Extra.TokenInfo.UserID
	}
	return ""
}
