package observe

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span is the handle returned by StartSpan.
//
// It is a struct rather than an interface, and that is a measured decision
// rather than a stylistic one. With an interface, `span.SetAttr(...)` is a
// dynamic dispatch, so escape analysis cannot see that the disabled
// implementation ignores its arguments — the variadic slice is heap-allocated
// at every call site even when telemetry is off. Measured on darwin/amd64, Go
// 1.25, telemetry disabled, one attribute constructed and attached per
// iteration:
//
//	interface Span, SetAttr(ParseStatus("ok")) .... 72.3 ns, 64 B, 1 alloc
//	struct Span,    SetAttr(ParseStatus("ok")) .... 46.1 ns,  0 B, 0 allocs
//
// (BenchmarkSpanSetAttrDisabled reports 4.2 ns for the call alone, with the
// attribute built outside the loop; the rest is the constructor.)
//
// A per-file allocation in the indexer, paid by users who declined telemetry,
// is not a reasonable price for the shape of a type declaration. Call sites are
// unchanged either way: StartSpan, End, RecordError and SetAttr read the same.
//
// The zero Span is the disabled span and is safe to use, so no call site needs
// a nil check.
//
// The method set is deliberately narrower than trace.Span. Everything OTel
// offers that accepts free-form text — SetName, AddEvent, RecordError's message
// — is a way to get source content into an export. Anything genuinely missing
// should be added here with a redaction story, not worked around by reaching
// for the underlying span.
type Span struct {
	// span is nil when telemetry is off, which is what every method branches
	// on. A nil check is cheaper than a dispatch and, unlike a no-op
	// implementation, it lets the compiler keep arguments on the stack.
	span trace.Span
}

// End closes the span. Safe on a disabled span, and safe to defer.
func (s Span) End() {
	if s.span == nil {
		return
	}
	s.span.End()
}

// Recording reports whether this span will be exported. Call sites should not
// need it; it exists for tests and for `cortex doctor`.
func (s Span) Recording() bool { return s.span != nil }

// RecordError records that an operation failed, and deliberately not why.
//
// otel's own span.RecordError writes err.Error() into an `exception.message`
// attribute. In RepoCortex almost every error string contains a filesystem path
// ("open /home/alice/work/acme-billing/internal/auth/token.go: permission
// denied"), a symbol name, or the fragment of source that failed to parse.
// Exporting that would leak the shape of a private repository through the one
// field nobody thinks to audit.
//
// So the span carries the Go type of the error and nothing else. That is enough
// to answer "which failure mode, how often" from the trace, while the full
// error — with its path — stays in the local log, joined by trace_id (see
// slogtrace.go). The trace tells you that something failed and where in the
// code; the log on the developer's own machine tells you why.
func (s Span) RecordError(err error) {
	if s.span == nil || err == nil {
		return
	}
	s.span.SetStatus(codes.Error, "")
	// %T is a Go type name from this binary, never derived from user data.
	s.span.SetAttributes(attribute.String(AttrErrorType, fmt.Sprintf("%T", err)))
}

// SetAttr attaches attributes, subject to the same redaction rules as
// StartSpan.
func (s Span) SetAttr(attrs ...attribute.KeyValue) {
	if len(attrs) == 0 {
		return
	}
	if s.span == nil {
		// Validate anyway under strict mode, so a call site that attaches a
		// path is caught by the tests that run with telemetry off — which is
		// nearly all of them. CheckAttrs rather than guardAttrs: attrs is the
		// caller's backing array and must not be scrubbed in place.
		if strict.Load() {
			if err := CheckAttrs(attrs); err != nil {
				panic(err)
			}
		}
		return
	}
	safe := make([]attribute.KeyValue, len(attrs))
	copy(safe, attrs)
	guardAttrs(safe)
	s.span.SetAttributes(safe...)
}

// StartSpan begins a span, or does nothing at all if telemetry is off.
//
// The disabled path is one atomic load, a nil comparison and a two-word return
// value. It does not derive a context, because context.WithValue would allocate
// on every call for a value nothing will read. Callers get their own ctx back
// unchanged, which is exactly right: with no active tracer there is no span to
// propagate.
//
// The attribute slice is copied before it reaches the SDK. That is not
// defensive copying — it is what keeps the variadic backing array on the
// caller's stack. Passing `attrs` directly into trace.WithAttributes would make
// escape analysis mark the parameter as leaking, and every instrumented call
// site would then heap-allocate its attributes even with telemetry disabled.
// `copy` leaks only the elements' contents, so the array itself stays put.
// BenchmarkStartSpanDisabledWithAttrs asserts 0 allocs/op and is what catches a
// regression here.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, Span) {
	p := active.Load()
	if p == nil {
		if strict.Load() {
			guardName(name)
			if err := CheckAttrs(attrs); err != nil {
				panic(err)
			}
		}
		return ctx, Span{}
	}
	return startSpan(ctx, p, name, attrs)
}

func startSpan(ctx context.Context, p *providers, name string, attrs []attribute.KeyValue) (context.Context, Span) {
	guardName(name)

	var opts []trace.SpanStartOption
	if len(attrs) > 0 {
		safe := make([]attribute.KeyValue, len(attrs))
		copy(safe, attrs)
		guardAttrs(safe)
		opts = append(opts, trace.WithAttributes(safe...))
	}

	ctx, span := p.tracer.Start(ctx, name, opts...)
	return ctx, Span{span: span}
}

// SpanContextFrom returns the trace and span ids active in ctx, or empty
// strings when there is no recording span. Used by the log correlation in
// slogtrace.go and by `cortex doctor`.
func SpanContextFrom(ctx context.Context) (traceID, spanID string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}
