package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"time"
)

// A malformed line from the client must not end the session.
//
// MCP over stdio is newline-delimited JSON, and the SDK treats a line it cannot
// parse as a fatal transport error: `repocortex serve` exited on the first bad
// frame, and every request after it went unanswered. A client that emits one
// stray line — a debug print, a truncated write, a log statement that escaped
// onto stdout — killed the whole session.
//
// JSON-RPC's own answer is a parse error and a live connection, not a hangup.
// The SDK does not expose a hook for that, so the malformed line is filtered
// out before the decoder ever sees it: the session survives, and the drop is
// logged rather than silently swallowed so a client author can find their bug.
//
// A filter built with a stdioSession also ANSWERS what the SDK would have
// ended the session over, as JSON-RPC prescribes: -32700 (id null) for text
// that is not JSON, -32600 for an empty batch and for a message that is
// neither a request nor a response. A parse error has no id to reply to, so
// its reply carries id null. Without a session the line is only dropped.
type jsonLineFilter struct {
	src  *bufio.Reader
	log  *slog.Logger
	rest []byte
	sess *stdioSession
}

// stdioSession is what the reading and writing halves of one stdio MCP
// session share: a lock so a reply the filter writes never interleaves with
// one the SDK writes, and the ids of requests not yet answered.
//
// The second is what makes end of input safe. A client that writes its
// requests and closes stdin is a normal client (a script, a shell pipe); the
// SDK treats the closed stream as the end of the session and exits while the
// requests are still being worked on, so nothing was answered. The filter
// withholds the end of input until every request it passed on has a reply.
type stdioSession struct {
	mu      sync.Mutex
	out     io.Writer
	pending map[string]struct{}
	idle    *sync.Cond
}

// drainTimeout bounds how long end of input waits for unanswered requests.
const drainTimeout = 2 * time.Minute

func newStdioSession(out io.Writer) *stdioSession {
	s := &stdioSession{out: out, pending: map[string]struct{}{}}
	s.idle = sync.NewCond(&s.mu)
	return s
}

// Writer is the one writer both the SDK and the filter reply through.
func (s *stdioSession) Writer() io.Writer { return sessionWriter{s} }

type sessionWriter struct{ s *stdioSession }

func (w sessionWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	n, err := w.s.out.Write(p)
	for _, m := range splitMessages(bytes.TrimSpace(p)) {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(m, &r) == nil && len(r.ID) > 0 && r.Method == "" {
			delete(w.s.pending, string(r.ID))
		}
	}
	w.s.idle.Broadcast()
	return n, err
}

func (s *stdioSession) track(id json.RawMessage) {
	s.mu.Lock()
	s.pending[string(id)] = struct{}{}
	s.mu.Unlock()
}

// waitIdle blocks until every tracked request has been answered, or the
// timeout passes.
func (s *stdioSession) waitIdle(timeout time.Duration) {
	deadline := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.pending = map[string]struct{}{}
		s.idle.Broadcast()
		s.mu.Unlock()
	})
	defer deadline.Stop()
	s.mu.Lock()
	for len(s.pending) > 0 {
		s.idle.Wait()
	}
	s.mu.Unlock()
}

// reply writes one JSON-RPC error response.
func (s *stdioSession) reply(id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	_, _ = sessionWriter{s}.Write(append(b, '\n'))
}

// splitMessages returns the messages in one line: itself, or the elements of
// a batch.
func splitMessages(line []byte) []json.RawMessage {
	if len(line) > 0 && line[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(line, &batch) == nil {
			return batch
		}
		return nil
	}
	return []json.RawMessage{line}
}

// admit decides what to do with one valid-JSON line: it reports whether the
// line goes on to the SDK, having answered it here when it does not.
func (s *stdioSession) admit(line []byte) bool {
	if len(line) == 0 || line[0] != '[' && line[0] != '{' {
		s.reply(nil, -32600, "Invalid Request: a message is a JSON object or a batch of them")
		return false
	}
	if line[0] == '[' {
		msgs := splitMessages(line)
		if len(msgs) == 0 {
			s.reply(nil, -32600, "Invalid Request: empty batch")
			return false
		}
		for _, m := range msgs {
			s.trackRequest(m)
		}
		return true
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method *string         `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		s.reply(nil, -32600, "Invalid Request")
		return false
	}
	if m.Method == nil && len(m.Result) == 0 && len(m.Error) == 0 {
		s.reply(m.ID, -32600, "Invalid Request: no method")
		return false
	}
	s.trackRequest(line)
	return true
}

func (s *stdioSession) trackRequest(msg []byte) {
	var r struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(msg, &r) == nil && len(r.ID) > 0 && string(r.ID) != "null" && r.Method != "" {
		s.track(r.ID)
	}
}

// newJSONLineFilter wraps r, passing through only lines that are syntactically
// valid JSON.
//
// bufio.Reader's default buffer is 4096 bytes and an MCP frame is routinely
// larger — a tools/list response carries every schema — so the buffer is sized
// for a realistic frame. A line beyond even that is passed through untouched
// rather than truncated: handing the decoder half a frame would manufacture the
// exact corruption this filter exists to absorb.
func newJSONLineFilter(r io.Reader, log *slog.Logger) *jsonLineFilter {
	return &jsonLineFilter{src: bufio.NewReaderSize(r, maxFrameBytes), log: log}
}

// newSessionFilter is newJSONLineFilter for a live session: it answers what
// it rejects and holds back end of input until requests are answered.
func newSessionFilter(r io.Reader, sess *stdioSession, log *slog.Logger) *jsonLineFilter {
	f := newJSONLineFilter(r, log)
	f.sess = sess
	return f
}

// maxFrameBytes bounds one buffered frame.
const maxFrameBytes = 1 << 20

func (f *jsonLineFilter) Read(p []byte) (int, error) {
	for len(f.rest) == 0 {
		line, err := f.src.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := trimSpaceBytes(line)
			switch {
			case len(trimmed) == 0:
				f.rest = line
			case json.Valid(trimmed):
				if f.sess == nil || f.sess.admit(trimmed) {
					f.rest = line
				}
			default:
				if f.sess != nil {
					f.sess.reply(nil, -32700, "Parse error")
				}
			}
			if len(trimmed) > 0 && !json.Valid(trimmed) && f.log != nil {
				// Bounded in the log too: a hostile client could otherwise write
				// a megabyte of junk straight into the operator's journal.
				f.log.Warn("dropped a malformed line from the MCP client; the session continues",
					"bytes", len(trimmed), "prefix", string(firstN(trimmed, 120)))
			}
		}
		if err != nil {
			if len(f.rest) == 0 {
				if f.sess != nil {
					// The client has said all it will say. Let the server
					// finish answering before the session is allowed to end.
					f.sess.waitIdle(drainTimeout)
				}
				return 0, err
			}
			break
		}
	}
	n := copy(p, f.rest)
	f.rest = f.rest[n:]
	return n, nil
}

func trimSpaceBytes(b []byte) []byte {
	start := 0
	for start < len(b) && isSpaceByte(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

func firstN(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
