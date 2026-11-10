//go:build cortex_nootlp

package observe

// Built with -tags cortex_nootlp: OTLP is excluded to keep ~13 MB of gRPC out
// of the binary. See exporter_otlp.go for the measurement and the reasoning.
//
// Asking for OTLP in such a build is a configuration error, not a silent
// downgrade to "none". Someone who set telemetry.exporter=otlp wants export;
// quietly giving them nothing would be worse than failing.

import (
	"context"
	"errors"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var errNoOTLP = errors.New(
	"this cortex build was compiled without OTLP support (-tags cortex_nootlp); " +
		"use the stdout exporter, or rebuild without the tag")

func newOTLPSpanExporter(context.Context, TelemetryOptions) (sdktrace.SpanExporter, error) {
	return nil, errNoOTLP
}

func newOTLPMetricExporter(context.Context, TelemetryOptions) (sdkmetric.Exporter, error) {
	return nil, errNoOTLP
}
