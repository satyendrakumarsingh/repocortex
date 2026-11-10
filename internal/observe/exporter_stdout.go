package observe

import (
	"io"

	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The stdout exporters are always compiled in. They are small, dependency-free
// and, more importantly, they are how a user answers "what would RepoCortex
// actually send?" without sending anything. Making the auditable mode the one
// that is always available is the point.

func newStdoutSpanExporter(w io.Writer) (sdktrace.SpanExporter, error) {
	// Not pretty-printed: one span per line keeps the output greppable and
	// pipeable, which is how it will actually be read.
	return stdouttrace.New(stdouttrace.WithWriter(w))
}

func newStdoutMetricExporter(w io.Writer) (sdkmetric.Exporter, error) {
	return stdoutmetric.New(stdoutmetric.WithWriter(w))
}
