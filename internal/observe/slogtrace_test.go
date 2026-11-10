package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// TestLogsCarryTraceIDsInsideSpan is the correlation guarantee. It is what
// makes it acceptable for the exported span to omit the error message: the
// full error stays local, and trace_id is the thread back to it.
func TestLogsCarryTraceIDsInsideSpan(t *testing.T) {
	enableForTest(t)

	var buf bytes.Buffer
	logger := New(Options{JSON: true, Writer: &buf})

	ctx, span := StartSpan(context.Background(), "index.run")
	logger.InfoContext(ctx, "indexing started")
	span.End()
	logger.InfoContext(context.Background(), "outside the span")

	records := decodeLines(t, &buf)
	if len(records) != 2 {
		t.Fatalf("got %d log records, want 2", len(records))
	}

	traceID, spanID := SpanContextFrom(ctx)
	if records[0][TraceIDKey] != traceID || records[0][SpanIDKey] != spanID {
		t.Errorf("in-span record has %v/%v, want %s/%s",
			records[0][TraceIDKey], records[0][SpanIDKey], traceID, spanID)
	}
	if _, ok := records[1][TraceIDKey]; ok {
		t.Error("a record logged outside any span was given a trace id")
	}
}

// TestNoTraceFieldsWhenDisabled: with telemetry off the log must look exactly
// as it did before this package existed. Empty correlation fields on every line
// would be a visible regression for a feature the user declined.
func TestNoTraceFieldsWhenDisabled(t *testing.T) {
	Reset()

	var buf bytes.Buffer
	logger := New(Options{JSON: true, Writer: &buf})
	logger.InfoContext(context.Background(), "indexing started")

	records := decodeLines(t, &buf)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if _, ok := records[0][TraceIDKey]; ok {
		t.Error("trace_id appeared with telemetry disabled")
	}
	if _, ok := records[0][SpanIDKey]; ok {
		t.Error("span_id appeared with telemetry disabled")
	}
}

// TestTraceHandlerPreservesHandlerBehaviour: the wrapper is applied to every
// logger New builds, so it must be transparent — level filtering, attributes
// and groups all still work.
func TestTraceHandlerPreservesHandlerBehaviour(t *testing.T) {
	Reset()

	var buf bytes.Buffer
	logger := New(Options{JSON: true, Level: "warn", Writer: &buf}).
		With("component", "indexer")

	logger.InfoContext(context.Background(), "should be filtered")
	logger.WarnContext(context.Background(), "kept")

	records := decodeLines(t, &buf)
	if len(records) != 1 {
		t.Fatalf("level filtering broke: got %d records, want 1", len(records))
	}
	if records[0]["component"] != "indexer" {
		t.Errorf("WithAttrs was dropped: %v", records[0])
	}
	if records[0]["msg"] != "kept" {
		t.Errorf("unexpected record: %v", records[0])
	}
}

// TestDiscardStaysSilent guards the existing --quiet contract through the new
// wrapper.
func TestDiscardStaysSilent(t *testing.T) {
	enableForTest(t)

	var buf bytes.Buffer
	logger := New(Options{JSON: true, Quiet: true, Writer: &buf})
	ctx, span := StartSpan(context.Background(), "index.run")
	logger.InfoContext(ctx, "indexing started")
	span.End()

	if buf.Len() != 0 {
		t.Errorf("quiet logger wrote %q", buf.String())
	}
}
