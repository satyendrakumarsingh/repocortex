package observe

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// Correlating logs with traces.
//
// A trace tells you that indexing spent 4 seconds in resolve; the log tells you
// which file it choked on. Neither is much use without the other, and joining
// them by timestamp is guesswork the moment two runs overlap. So every log
// record emitted inside a span carries the ids that join them.
//
// This is also what makes the deliberate poverty of the trace acceptable.
// sdkSpan.RecordError exports only the error's Go type, because error strings
// contain paths; the full error stays in the local log, and trace_id is the
// thread back to it. The identifiers are random per run and mean nothing off
// the machine, so exporting them discloses nothing.

// TraceIDKey and SpanIDKey are the log field names. They match the OTel logging
// convention so that a collector ingesting these logs joins them automatically.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
)

// traceHandler adds trace correlation to any slog.Handler.
type traceHandler struct{ inner slog.Handler }

// WithTrace wraps a handler so records logged inside a span carry trace_id and
// span_id.
//
// The cost when tracing is off is one atomic load per record — cheaper than the
// handler's own level check, and far cheaper than formatting the record that
// follows it. That is why New applies this unconditionally instead of making it
// an option: an option would be off in exactly the situation where someone is
// debugging and needs it on.
func WithTrace(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	return traceHandler{inner: h}
}

func (h traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	// Checking `active` rather than only the span context matters: a caller
	// may pass a context carrying a span from some other library while
	// RepoCortex's telemetry is off, and inventing correlation ids for a trace
	// nobody is recording is noise in the terminal.
	if active.Load() != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			// Clone only when there is something to add. slog.Record shares
			// its backing array with the caller until cloned, and mutating it
			// in place would corrupt records held by a fan-out handler.
			r = r.Clone()
			r.AddAttrs(
				slog.String(TraceIDKey, sc.TraceID().String()),
				slog.String(SpanIDKey, sc.SpanID().String()),
			)
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	// Caveat worth knowing: ids are added as record attributes, so a logger
	// with an open group nests them inside it. RepoCortex's own loggers do not
	// use groups, and fixing it properly would mean reimplementing group
	// state here rather than delegating — not worth it until something needs
	// it.
	return traceHandler{inner: h.inner.WithGroup(name)}
}
