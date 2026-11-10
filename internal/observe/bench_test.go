package observe

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// Benchmarks for the disabled path.
//
// These matter more than the enabled ones. Telemetry is off for essentially
// every RepoCortex invocation, so whatever they cost is what instrumentation
// costs the product — and if it were not close to free, the honest response
// would be to remove the instrumentation, not to ship a tax on people who
// declined the feature.
//
// Allocations are the number to watch. Every disabled-path benchmark here must
// report 0 allocs/op: a single heap allocation per instrumented call would
// dominate the timing and add GC pressure inside the indexer's hot loop. Two
// design decisions exist purely to hold that line — Span being a struct rather
// than an interface, and the attribute slice being copied rather than passed
// through — and these benchmarks are what catches a regression in either.
//
//	go test -run '^$' -bench . -benchmem ./internal/observe/
//
// Strict mode is off in these benchmarks. It defaults on inside test binaries
// and would otherwise measure the validator, which never runs in a release
// build.

func benchDisabled(b *testing.B) {
	b.Helper()
	Reset()
	prev := SetStrict(false)
	b.Cleanup(func() { SetStrict(prev) })
}

func BenchmarkStartSpanDisabled(b *testing.B) {
	benchDisabled(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, span := StartSpan(ctx, "index.file")
		span.End()
	}
}

// BenchmarkStartSpanDisabledWithAttrs constructs its attributes inside the loop
// on purpose: that is what a real call site does, and it is the only way to
// exercise the escape-analysis property. Subtract
// BenchmarkSafeAttrConstructors to see the cost attributable to this package.
func BenchmarkStartSpanDisabledWithAttrs(b *testing.B) {
	benchDisabled(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, span := StartSpan(ctx, "index.file", Language("go"), SizeBucket(4096))
		span.End()
	}
}

// BenchmarkSafeAttrConstructors isolates the cost of building safe attributes.
// It is the larger half of the number above, it is paid whether or not
// telemetry is on, and it is the price of the API being safe by construction:
// Language normalises against a closed vocabulary and SizeBucket bins a size,
// rather than each call site pasting in a raw string.
func BenchmarkSafeAttrConstructors(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Language("go")
		_ = SizeBucket(4096)
	}
}

func BenchmarkSpanSetAttrDisabled(b *testing.B) {
	benchDisabled(b)
	_, span := StartSpan(context.Background(), "index.file")
	attr := ParseStatus("ok")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		span.SetAttr(attr)
	}
}

func BenchmarkSpanRecordErrorDisabled(b *testing.B) {
	benchDisabled(b)
	_, span := StartSpan(context.Background(), "index.file")
	err := io.EOF
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		span.RecordError(err)
	}
}

func BenchmarkMetricRecordDisabled(b *testing.B) {
	benchDisabled(b)
	m, err := NewMetrics()
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.SearchLatency(ctx, time.Millisecond)
	}
}

func BenchmarkMetricRecordDisabledWithAttrs(b *testing.B) {
	benchDisabled(b)
	m, err := NewMetrics()
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	attrs := []attribute.KeyValue{Language("go")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.FilesIndexed(ctx, 1, attrs...)
		m.DBQueryLatency(ctx, "select", time.Microsecond)
	}
}

// BenchmarkLogHandlerDisabled and BenchmarkLogHandlerBaseline are a pair: the
// difference between them is the entire cost of the trace-correlation wrapper
// that New applies to every logger. It has to be small enough that applying it
// unconditionally is obviously right.
func BenchmarkLogHandlerDisabled(b *testing.B) {
	benchDisabled(b)
	logger := New(Options{JSON: true, Writer: io.Discard})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.InfoContext(ctx, "indexed file", slog.Int("files", i))
	}
}

func BenchmarkLogHandlerBaseline(b *testing.B) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.InfoContext(ctx, "indexed file", slog.Int("files", i))
	}
}

// The enabled-path benchmarks exist for comparison. The gap between these and
// the disabled numbers is what a user opts into when they turn telemetry on,
// and it is the number to quote if anyone proposes enabling it by default.
//
// Read the span number as an upper bound: the test seam wires the exporter
// synchronously and the in-memory exporter retains every span, so it includes
// export work that a real batching pipeline amortises off the hot path.

func benchEnabled(b *testing.B) {
	b.Helper()
	spans, reader := newTestExporters()
	shutdown, err := Init(context.Background(), TelemetryOptions{
		spanExporter: spans, metricReader: reader, ShutdownTimeout: time.Second,
	})
	if err != nil {
		b.Fatal(err)
	}
	prev := SetStrict(false)
	b.Cleanup(func() {
		SetStrict(prev)
		_ = shutdown(context.Background())
		Reset()
	})
	b.Cleanup(spans.Reset)
}

func BenchmarkStartSpanEnabled(b *testing.B) {
	benchEnabled(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, span := StartSpan(ctx, "index.file", Language("go"))
		span.End()
	}
}

func BenchmarkMetricRecordEnabled(b *testing.B) {
	benchEnabled(b)
	m, err := NewMetrics()
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.SearchLatency(ctx, time.Millisecond)
	}
}
