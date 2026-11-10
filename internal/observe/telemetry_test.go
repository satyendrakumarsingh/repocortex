package observe

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestExporters returns the in-memory pair used to assert on telemetry
// without a collector.
func newTestExporters() (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	return tracetest.NewInMemoryExporter(), sdkmetric.NewManualReader()
}

// enableForTest installs providers backed by in-memory exporters and guarantees
// they are removed again, so one test cannot leak telemetry into the next.
func enableForTest(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	shutdown, err := Init(context.Background(), TelemetryOptions{
		spanExporter:    spans,
		metricReader:    reader,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
		Reset()
	})
	return spans, reader
}

// TestDefaultIsOff is the headline guarantee: a developer tool that was not
// asked to emit telemetry emits none, and builds no machinery that could.
func TestDefaultIsOff(t *testing.T) {
	shutdown, err := Init(context.Background(), TelemetryOptions{})
	if err != nil {
		t.Fatalf("Init with zero options: %v", err)
	}
	if Enabled() {
		t.Fatal("telemetry is enabled after Init with the zero value; it must be opt-in")
	}
	if active.Load() != nil {
		t.Fatal("a provider was installed despite telemetry being disabled")
	}

	ctx, span := StartSpan(context.Background(), "index.run")
	if span.Recording() {
		t.Fatal("disabled StartSpan returned a recording span")
	}
	// The context must come back untouched: deriving one would allocate for a
	// value nothing will read.
	if ctx != context.Background() {
		t.Fatal("disabled StartSpan derived a new context")
	}
	span.SetAttr(Language("go"))
	span.RecordError(errors.New("boom"))
	span.End()

	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics while disabled: %v", err)
	}
	m.IndexDuration(context.Background(), time.Second)
	m.SearchLatency(context.Background(), time.Millisecond)
	m.ParseFailure(context.Background(), "go")

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown of disabled telemetry: %v", err)
	}
}

func TestParseExporter(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Exporter
		wantErr bool
	}{
		{"", ExporterNone, false},
		{"none", ExporterNone, false},
		{"off", ExporterNone, false},
		{"stdout", ExporterStdout, false},
		{"otlp", ExporterOTLP, false},
		{"OTLP", ExporterNone, true},   // case matters; a config typo must not enable export
		{"jaeger", ExporterNone, true}, // unknown must fail, not default
	} {
		got, err := ParseExporter(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseExporter(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("ParseExporter(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOTLPRequiresEndpoint proves there is no fallback collector address. A
// config that asks for OTLP without saying where must fail rather than pick one.
func TestOTLPRequiresEndpoint(t *testing.T) {
	_, err := Init(context.Background(), TelemetryOptions{Exporter: ExporterOTLP})
	if err == nil {
		t.Fatal("Init accepted otlp with no endpoint")
	}
	if Enabled() {
		t.Fatal("telemetry left enabled after a failed Init")
	}
}

// TestResourceCarriesNoIdentity is the privacy gate on exported resource
// attributes. It asserts what is present and, more importantly, that nothing
// resembling a hostname, username or working directory got in.
func TestResourceCarriesNoIdentity(t *testing.T) {
	res := newResource("1.2.3")
	got := map[string]string{}
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}

	if got["service.name"] != ServiceName {
		t.Errorf("service.name = %q, want %q", got["service.name"], ServiceName)
	}
	if got["service.version"] != "1.2.3" {
		t.Errorf("service.version = %q, want 1.2.3", got["service.version"])
	}
	if len(got) != 2 {
		t.Errorf("resource has %d attributes (%v); only service.name and service.version may be exported", len(got), got)
	}
	for key := range got {
		for _, banned := range []string{"host", "process", "os.", "user", "container", "device"} {
			if strings.Contains(key, banned) {
				t.Errorf("resource attribute %q identifies the machine or user", key)
			}
		}
	}
}

// TestStdoutExporterStaysLocal checks the debug mode writes where it was told,
// which is what makes it the safe way to audit telemetry before enabling OTLP.
func TestStdoutExporterStaysLocal(t *testing.T) {
	var buf bytes.Buffer
	shutdown, err := Init(context.Background(), TelemetryOptions{
		Exporter:        ExporterStdout,
		Writer:          &buf,
		ShutdownTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Init stdout: %v", err)
	}
	t.Cleanup(Reset)

	_, span := StartSpan(context.Background(), "index.run", Language("go"))
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "index.run") {
		t.Fatalf("stdout exporter wrote no span; got %q", out)
	}
	if strings.Contains(out, "unknown_service") {
		t.Error("stdout output carries a default service resource, suggesting resource.Default leaked in")
	}
}

// blockingExporter refuses to shut down. It stands in for the realistic
// failure: a collector that accepted a connection and then stopped answering.
type blockingExporter struct {
	sdktrace.SpanExporter
	release <-chan struct{}
}

func (b blockingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }

func (b blockingExporter) Shutdown(context.Context) error {
	// Deliberately ignores the context, exactly as a third-party exporter with
	// a missing timeout would.
	<-b.release
	return nil
}

// TestShutdownIsBounded is the Spec §60 guarantee: `cortex` exiting never
// waits on telemetry. Without the wall-clock bound in doShutdown this test
// hangs until the go test timeout, which is the failure mode being prevented.
func TestShutdownIsBounded(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the leaked goroutine finish

	shutdown, err := Init(context.Background(), TelemetryOptions{
		spanExporter:    blockingExporter{release: release},
		metricReader:    sdkmetric.NewManualReader(),
		ShutdownTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(Reset)

	start := time.Now()
	err = shutdown(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrShutdownTimeout) {
		t.Errorf("shutdown err = %v, want ErrShutdownTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("ErrShutdownTimeout must wrap context.DeadlineExceeded so callers can classify it")
	}
	if elapsed > 2*time.Second {
		t.Errorf("shutdown took %v despite a 50ms budget", elapsed)
	}
	if Enabled() {
		t.Error("telemetry still enabled after a timed-out shutdown; it must be uninstalled first")
	}
}

// TestShutdownIsIdempotent covers the real call pattern: a deferred shutdown
// plus a signal handler both firing.
func TestShutdownIsIdempotent(t *testing.T) {
	shutdown, err := Init(context.Background(), TelemetryOptions{
		spanExporter:    tracetest.NewInMemoryExporter(),
		metricReader:    sdkmetric.NewManualReader(),
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(Reset)

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	// A second Shutdown straight on the SDK provider returns an error; the
	// wrapper must not surface one, or every clean exit logs a false failure.
	for i := 0; i < 3; i++ {
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown call %d: %v", i+2, err)
		}
	}

	// Concurrent teardown must be safe too.
	shutdown2, err := Init(context.Background(), TelemetryOptions{
		spanExporter:    tracetest.NewInMemoryExporter(),
		metricReader:    sdkmetric.NewManualReader(),
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := shutdown2(context.Background()); err != nil {
				t.Errorf("concurrent shutdown: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestReInitRebindsMetrics guards the ordering hazard: a Metrics built against
// one activation must not keep recording into a provider that has been torn
// down.
func TestReInitRebindsMetrics(t *testing.T) {
	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics before Init: %v", err)
	}

	_, reader := enableForTest(t)
	m.SearchLatency(context.Background(), 5*time.Millisecond)

	if got := collectSum(t, reader, "cortex.search.latency"); got == 0 {
		t.Fatal("a Metrics created before Init recorded nothing after Init; instruments did not rebind")
	}
}
