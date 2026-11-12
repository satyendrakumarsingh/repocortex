package mcp

import (
	"context"
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
	// Transport is how the call arrived: "mcp" for a protocol client, "rest"
	// or "cli" for the two in-process front ends.
	Transport string `json:"transport"`
	Tool      string `json:"tool"`
	// Arguments are the call's arguments as sent, cut at auditArgBytes.
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	ArgsCut    bool            `json:"arguments_truncated,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	// Outcome is "ok", "tool_error" (the tool answered with isError) or
	// "protocol_error" (the call never reached a handler).
	Outcome     string `json:"outcome"`
	ResultBytes int    `json:"result_bytes"`
}

// auditArgBytes bounds the arguments kept per event. A task description or a
// pasted diff can be large, and an audit log that grows by a megabyte per call
// is one that gets turned off.
const auditArgBytes = 2048

// AuditLog appends events to a writer, one JSON line each.
//
// Safe for concurrent use: an HTTP server answers several sessions at once and
// interleaved partial lines would make the file unparseable.
type AuditLog struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewAuditLog returns a log writing to w. The caller owns w and closes it.
func NewAuditLog(w io.Writer) *AuditLog {
	return &AuditLog{w: w, now: time.Now}
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

// auditMiddleware records every tools/call.
func (s *Server) auditMiddleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		log := s.deps.Audit
		if log == nil || method != methodCallTool {
			return next(ctx, method, req)
		}
		call, ok := req.(*sdk.CallToolRequest)
		if !ok || call.Params == nil {
			return next(ctx, method, req)
		}

		start := log.now()
		res, err := next(ctx, method, req)

		ev := AuditEvent{
			Time:       start.UTC().Format(time.RFC3339),
			Transport:  s.transportOf(call),
			Tool:       call.Params.Name,
			DurationMS: log.now().Sub(start).Milliseconds(),
			Outcome:    "ok",
		}
		if call.Session != nil {
			ev.Session = call.Session.ID()
		}
		if args := call.Params.Arguments; len(args) > 0 {
			if len(args) > auditArgBytes {
				// A cut JSON document is not JSON, so it is stored as a string.
				cut, _ := json.Marshal(string(args[:auditArgBytes]))
				ev.Arguments, ev.ArgsCut = cut, true
			} else if json.Valid(args) {
				ev.Arguments = json.RawMessage(args)
			}
		}
		switch {
		case err != nil:
			ev.Outcome = "protocol_error"
		default:
			if tr, ok := res.(*sdk.CallToolResult); ok && tr != nil {
				if tr.IsError {
					ev.Outcome = "tool_error"
				}
				for _, c := range tr.Content {
					if t, ok := c.(*sdk.TextContent); ok {
						ev.ResultBytes += len(t.Text)
					}
				}
			}
		}
		log.Record(ev)
		return res, err
	}
}

// transportOf names the front end a call arrived through.
func (s *Server) transportOf(call *sdk.CallToolRequest) string {
	if call != nil && call.Session != nil {
		if name, ok := s.frontEnds.Load(call.Session); ok {
			return name.(string)
		}
	}
	return "mcp"
}
