package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
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
// This drops rather than replying -32700. A parse error has no id to reply to —
// the id is inside the text that would not parse — so a response could not be
// correlated to a request anyway, and inventing one would be worse than
// silence to the client and a line in the operator's log.
type jsonLineFilter struct {
	src  *bufio.Reader
	log  *slog.Logger
	rest []byte
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

// maxFrameBytes bounds one buffered frame.
const maxFrameBytes = 1 << 20

func (f *jsonLineFilter) Read(p []byte) (int, error) {
	for len(f.rest) == 0 {
		line, err := f.src.ReadBytes('\n')
		if len(line) > 0 {
			if trimmed := trimSpaceBytes(line); len(trimmed) == 0 || json.Valid(trimmed) {
				f.rest = line
			} else if f.log != nil {
				// Bounded in the log too: a hostile client could otherwise write
				// a megabyte of junk straight into the operator's journal.
				f.log.Warn("dropped a malformed line from the MCP client; the session continues",
					"bytes", len(trimmed), "prefix", string(firstN(trimmed, 120)))
			}
		}
		if err != nil {
			if len(f.rest) == 0 {
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
