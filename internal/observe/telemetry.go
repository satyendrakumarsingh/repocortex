package observe

// Telemetry policy for RepoCortex, in one place, because every decision below
// follows from it:
//
// RepoCortex is a tool a developer runs on their own machine, over their own
// (often private, often employer-owned) source code. That makes it categorically
// different from a service you operate. A service may emit telemetry because the
// operator owns both the code and the data; a developer tool that emits
// telemetry has taken something that was not offered.
//
// Three rules follow, and they are enforced by code in this package rather than
// left to reviewer discipline:
//
//  1. OFF unless explicitly turned on. The zero value of TelemetryOptions
//     creates no provider, no background goroutine and no socket. There is no
//     "anonymous usage statistics" default, because there is no such thing as
//     anonymous when the payload describes a private repository.
//
//  2. Free when off. Call sites are unconditional — StartSpan and the metric
//     recorders are always called — so the cost of the disabled path is the cost
//     the whole product pays. It is one atomic load and a zero-size return
//     value; see the benchmarks in bench_test.go.
//
//  3. Counts and coarse categories only. No paths, no identifiers, no source
//     text, no config values, ever, on any code path. redact.go enforces this
//     mechanically and panics in tests when a call site gets it wrong.
//
// Spec §35.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ServiceName is the only identity RepoCortex reports. It is a constant, not the
// binary name or the repository name, so that two different users' traces are
// indistinguishable at the resource level.
const ServiceName = "repocortex"

// instrumentationScope names this package as the origin of spans and metrics.
const instrumentationScope = "github.com/satyendrakumarsingh/repocortex/internal/observe"

// Exporter selects where telemetry goes.
type Exporter string

const (
	// ExporterNone is the zero value and the default: telemetry is off.
	// Nothing is constructed, so there is nothing to leak and nothing to flush.
	ExporterNone Exporter = ""

	// ExporterStdout writes spans and metrics as JSON to a local writer. This
	// is the debugging mode: the data never leaves the machine, so it is the
	// safe way to inspect what RepoCortex would emit before enabling OTLP.
	ExporterStdout Exporter = "stdout"

	// ExporterOTLP sends to a collector over OTLP/HTTP. This is the only mode
	// that puts bytes on a network, and it requires an explicit endpoint —
	// there is deliberately no default endpoint to fall back to.
	ExporterOTLP Exporter = "otlp"
)

// ParseExporter maps a config string to an Exporter, rejecting anything it does
// not recognise.
//
// Unknown values fail loudly rather than degrading to a default. Silently
// falling back to "none" would hide a typo from someone who wanted telemetry;
// silently falling back to anything else would enable export for someone who
// did not.
func ParseExporter(s string) (Exporter, error) {
	switch s {
	case "", "none", "off", "disabled":
		return ExporterNone, nil
	case "stdout", "debug":
		return ExporterStdout, nil
	case "otlp":
		return ExporterOTLP, nil
	default:
		return ExporterNone, fmt.Errorf("unknown telemetry exporter %q (want none, stdout or otlp)", s)
	}
}

// TelemetryOptions configures Init.
//
// The name is not Options because that is already the logger's configuration in
// log.go, and the two are separate concerns: logging is local and on by
// default, telemetry is exportable and off by default. Conflating them is
// exactly the mistake that leads to a log field ending up in an OTLP payload.
type TelemetryOptions struct {
	// Exporter selects the destination. The zero value disables telemetry.
	Exporter Exporter

	// Endpoint is the OTLP collector, as "host:port" or a full URL. Required
	// for ExporterOTLP and ignored otherwise.
	Endpoint string

	// Insecure permits plain HTTP to the collector. Defaults to false, so a
	// misconfigured endpoint fails rather than sending in the clear.
	Insecure bool

	// Headers are attached to OTLP requests, typically a collector auth token.
	// These are credentials: they are never used as resource or span
	// attributes, and never logged.
	Headers map[string]string

	// ServiceVersion is the RepoCortex build version. Empty reports "dev".
	ServiceVersion string

	// SampleRatio is head sampling for traces, in [0,1]. Zero means 1.0
	// (sample everything), which is the right default for a tool that runs
	// interactively a few times a minute rather than thousands of times a
	// second.
	SampleRatio float64

	// MetricInterval is the metric export period. Defaults to 60s.
	MetricInterval time.Duration

	// ShutdownTimeout bounds flush-on-exit. Defaults to 5s. See Init's
	// returned shutdown function for why this is a hard bound.
	ShutdownTimeout time.Duration

	// Writer receives ExporterStdout output. Defaults to stderr, so that
	// stdout stays parseable for --json consumers (same rule as log.go).
	Writer io.Writer

	// Logger receives SDK-internal errors (export failures, dropped batches).
	// Defaults to slog.Default. Export failures must be visible but must never
	// be fatal: telemetry is an observation of the work, not the work.
	Logger *slog.Logger

	// spanExporter and metricReader are test seams. They bypass exporter
	// construction so tests can assert on recorded data without a collector.
	// A supplied spanExporter is wired synchronously, so a span is visible to
	// the test the moment it ends.
	spanExporter sdktrace.SpanExporter
	metricReader sdkmetric.Reader
}

// providers is the live telemetry state. A nil *providers means "off", which is
// what makes the disabled path a single atomic load and a nil comparison.
type providers struct {
	// gen identifies this activation. Metrics instruments cache their
	// binding and compare generations to notice a re-Init; see metrics.go.
	gen uint64

	tracer trace.Tracer
	meter  metric.Meter

	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider
}

var (
	// active holds the current providers, or nil when telemetry is off.
	//
	// This is package-level global state, which is normally worth avoiding.
	// It is correct here because the alternative — threading a telemetry
	// handle through every layer of the indexer — is precisely the pressure
	// that makes contributors skip instrumentation, and because there is
	// exactly one RepoCortex process per invocation.
	active atomic.Pointer[providers]

	// generation issues monotonic ids to activations.
	generation atomic.Uint64

	// initMu serialises Init and shutdown so a re-Init cannot interleave with
	// a teardown and leave a half-installed provider visible.
	initMu sync.Mutex
)

// Enabled reports whether telemetry is currently active. Call sites should not
// need this: StartSpan and the metric recorders are already free when off.
// It exists for `repocortex doctor` to report configuration back to the user.
func Enabled() bool { return active.Load() != nil }

// noopShutdown is returned when there is nothing to tear down.
func noopShutdown(context.Context) error { return nil }

// errNoEndpoint rejects OTLP without a destination. There is no default
// collector address on purpose: guessing localhost:4318 would mean a typo in a
// config file silently starts sending data somewhere.
var errNoEndpoint = errors.New("telemetry exporter is otlp but no endpoint is configured")

// ErrShutdownTimeout is returned when telemetry could not be flushed within the
// configured budget. It wraps context.DeadlineExceeded so callers can classify
// it with errors.Is.
var ErrShutdownTimeout = fmt.Errorf("telemetry shutdown exceeded its timeout: %w", context.DeadlineExceeded)

// Init installs telemetry providers and returns a shutdown function.
//
// With the zero value of TelemetryOptions this is close to a no-op: it installs
// nothing, allocates nothing meaningful and returns a shutdown that does
// nothing. That is the default path, and it must stay cheap.
//
// The returned shutdown is idempotent and bounded. Spec §60: `repocortex` exiting
// must not wait on a collector. See shutdownFunc.
//
// Call it once, during startup. A second Init replaces the installed providers
// but cannot tear the previous ones down — doing so here would make startup
// block on a flush — so the caller still owns the earlier shutdown function. A
// re-Init without it leaks the first provider's exporter goroutine, which is
// why the case is warned about rather than tolerated silently.
func Init(ctx context.Context, opts TelemetryOptions) (func(context.Context) error, error) {
	initMu.Lock()
	defer initMu.Unlock()

	if opts.Exporter == ExporterNone && opts.spanExporter == nil && opts.metricReader == nil {
		// Deliberately does not touch `active`: a caller who disables
		// telemetry gets the disabled fast path, not a provider that happens
		// to drop data. The difference is measurable; see BenchmarkStartSpan.
		return noopShutdown, nil
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if active.Load() != nil {
		logger.Warn("telemetry re-initialised without shutting the previous providers down; " +
			"the earlier exporter will keep running until its shutdown function is called")
	}

	// SDK-internal problems (a collector refusing connections, a full queue)
	// arrive here. They are logged at warn and never propagated: losing
	// telemetry must not fail an index.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("telemetry error", "err", err)
	}))

	res := newResource(opts.ServiceVersion)

	tp, err := newTracerProvider(ctx, opts, res)
	if err != nil {
		return noopShutdown, err
	}
	mp, err := newMeterProvider(ctx, opts, res)
	if err != nil {
		// The tracer provider is already built and owns a goroutine and
		// possibly a connection; drop it rather than leaking it because a
		// later step failed.
		_ = tp.Shutdown(ctx)
		return noopShutdown, err
	}

	p := &providers{
		gen:    generation.Add(1),
		tracer: tp.Tracer(instrumentationScope),
		meter:  mp.Meter(instrumentationScope),
		tp:     tp,
		mp:     mp,
	}

	// Installing the globals lets any OTel-instrumented dependency join
	// RepoCortex's traces. It is a deliberate side effect, undone on shutdown.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	active.Store(p)

	return shutdownFunc(p, opts.ShutdownTimeout), nil
}

// newResource builds the resource attributes attached to every export.
//
// Service name and version only. resource.Default and the WithHost, WithProcess
// and WithFromEnv detectors are all deliberately unused: they contribute
// hostname, OS username, process command line and working directory, each of
// which identifies the developer or the repository they are indexing. A trace
// that says "an index of 4,000 files took 12s" is telemetry; the same trace
// carrying `host.name` and `process.command_args` is a disclosure.
func newResource(version string) *resource.Resource {
	if version == "" {
		version = "dev"
	}
	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version),
	)
}

func newTracerProvider(ctx context.Context, opts TelemetryOptions, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	sampler := sdktrace.AlwaysSample()
	if r := opts.SampleRatio; r > 0 && r < 1 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(r))
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}

	switch {
	case opts.spanExporter != nil:
		// Synchronous on purpose: the test seam exists so a test can end a
		// span and immediately assert on it, with no flush dance.
		tpOpts = append(tpOpts, sdktrace.WithSyncer(opts.spanExporter))
	case opts.Exporter == ExporterStdout:
		exp, err := newStdoutSpanExporter(opts.writer())
		if err != nil {
			return nil, fmt.Errorf("stdout trace exporter: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	case opts.Exporter == ExporterOTLP:
		exp, err := newOTLPSpanExporter(ctx, opts)
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	}

	return sdktrace.NewTracerProvider(tpOpts...), nil
}

func newMeterProvider(ctx context.Context, opts TelemetryOptions, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	interval := opts.MetricInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	switch {
	case opts.metricReader != nil:
		mpOpts = append(mpOpts, sdkmetric.WithReader(opts.metricReader))
	case opts.Exporter == ExporterStdout:
		exp, err := newStdoutMetricExporter(opts.writer())
		if err != nil {
			return nil, fmt.Errorf("stdout metric exporter: %w", err)
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))))
	case opts.Exporter == ExporterOTLP:
		exp, err := newOTLPMetricExporter(ctx, opts)
		if err != nil {
			return nil, err
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))))
	}

	return sdkmetric.NewMeterProvider(mpOpts...), nil
}

func (o TelemetryOptions) writer() io.Writer {
	if o.Writer != nil {
		return o.Writer
	}
	return os.Stderr
}

// shutdownFunc builds the bounded, idempotent teardown returned by Init.
//
// Two properties matter, and neither is free:
//
// Idempotent, because shutdown is reachable from both a normal exit path and a
// signal handler, and calling Shutdown twice on an SDK provider returns an error
// the second time that would look like a real failure in the logs.
//
// Bounded by wall clock, not merely by context. The SDK propagates the context
// to the exporter, but an exporter is third-party code that can ignore it — a
// TCP connect to an unreachable collector, or an HTTP client with no timeout.
// So the flush runs on its own goroutine and we stop waiting when the budget is
// spent. That goroutine may outlive this call; that is the intended trade. The
// process is exiting, and a leaked goroutine for a few hundred milliseconds is
// strictly better than a developer's shell hanging because a collector went
// away. Spec §60.
func shutdownFunc(p *providers, timeout time.Duration) func(context.Context) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var (
		once sync.Once
		err  error
	)
	return func(ctx context.Context) error {
		once.Do(func() { err = doShutdown(ctx, p, timeout) })
		return err
	}
}

func doShutdown(ctx context.Context, p *providers, timeout time.Duration) error {
	initMu.Lock()
	// Uninstall first. Anything that starts a span while the flush is in
	// flight then takes the disabled path instead of feeding work into a
	// provider that is being torn down.
	active.CompareAndSwap(p, nil)
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	initMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- errors.Join(p.tp.Shutdown(ctx), p.mp.Shutdown(ctx))
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ErrShutdownTimeout
	}
}

// Reset tears telemetry down without flushing. Tests only: it exists so a test
// that installed providers cannot leak them into the next test.
func Reset() {
	initMu.Lock()
	defer initMu.Unlock()
	active.Store(nil)
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
}
