//go:build !repocortex_nootlp

package observe

// OTLP export is compiled in by default, but it is not free at the binary
// level. go.opentelemetry.io/otel/exporters/otlp/.../otlpconfig imports
// google.golang.org/grpc even for the HTTP transport, which drags the whole
// gRPC stack into the link. Measured on darwin/amd64, CGO_ENABLED=0, Go 1.25:
//
//	stdout exporters only .................. 7.06 MB
//	stdout + OTLP/HTTP exporters ........... 20.45 MB
//
// 13.4 MB for a feature that is off by default is worth an escape hatch, so
// this file and its stub in exporter_otlp_disabled.go are guarded by the
// `repocortex_nootlp` build tag. Default builds keep full functionality;
// `go build -tags repocortex_nootlp` gets the small binary and a clear error if
// someone asks for OTLP anyway.
//
// HTTP rather than gRPC is chosen for the wire protocol regardless: every OTLP
// collector accepts it, it traverses proxies a developer machine is likely to
// sit behind, and it needs no keepalive tuning.

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func newOTLPSpanExporter(ctx context.Context, opts TelemetryOptions) (sdktrace.SpanExporter, error) {
	if opts.Endpoint == "" {
		return nil, errNoEndpoint
	}
	o := []otlptracehttp.Option{}
	if isURL(opts.Endpoint) {
		o = append(o, otlptracehttp.WithEndpointURL(opts.Endpoint))
	} else {
		o = append(o, otlptracehttp.WithEndpoint(opts.Endpoint))
		if opts.Insecure {
			o = append(o, otlptracehttp.WithInsecure())
		}
	}
	if len(opts.Headers) > 0 {
		o = append(o, otlptracehttp.WithHeaders(opts.Headers))
	}
	exp, err := otlptracehttp.New(ctx, o...)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	return exp, nil
}

func newOTLPMetricExporter(ctx context.Context, opts TelemetryOptions) (sdkmetric.Exporter, error) {
	if opts.Endpoint == "" {
		return nil, errNoEndpoint
	}
	o := []otlpmetrichttp.Option{}
	if isURL(opts.Endpoint) {
		o = append(o, otlpmetrichttp.WithEndpointURL(opts.Endpoint))
	} else {
		o = append(o, otlpmetrichttp.WithEndpoint(opts.Endpoint))
		if opts.Insecure {
			o = append(o, otlpmetrichttp.WithInsecure())
		}
	}
	if len(opts.Headers) > 0 {
		o = append(o, otlpmetrichttp.WithHeaders(opts.Headers))
	}
	exp, err := otlpmetrichttp.New(ctx, o...)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	return exp, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
