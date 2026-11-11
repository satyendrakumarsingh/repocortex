package observe

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collect gathers everything the manual reader has, keyed by instrument name.
func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// collectSum reduces an instrument to one number: the total count for a
// histogram, the total value for a counter. Enough to assert "this was
// recorded" without coupling the test to aggregation internals.
func collectSum(t *testing.T, reader *sdkmetric.ManualReader, name string) float64 {
	t.Helper()
	m, ok := collect(t, reader)[name]
	if !ok {
		return 0
	}
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		var total int64
		for _, dp := range d.DataPoints {
			total += dp.Value
		}
		return float64(total)
	case metricdata.Histogram[float64]:
		var total uint64
		for _, dp := range d.DataPoints {
			total += dp.Count
		}
		return float64(total)
	case metricdata.Histogram[int64]:
		var total uint64
		for _, dp := range d.DataPoints {
			total += dp.Count
		}
		return float64(total)
	default:
		t.Fatalf("metric %s has unexpected aggregation %T", name, m.Data)
		return 0
	}
}

// TestAllSpecMetricsRecord walks the whole spec §35 list through one enabled
// provider. It is deliberately exhaustive: a recorder wired to the wrong
// instrument still compiles and still looks right at the call site.
func TestAllSpecMetricsRecord(t *testing.T) {
	_, reader := enableForTest(t)
	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	ctx := context.Background()

	m.IndexDuration(ctx, 12*time.Second, AccuracyMode("auto"))
	m.FilesIndexed(ctx, 400, Language("go"))
	m.ParseFailure(ctx, "python")
	m.SymbolsIndexed(ctx, 9000, Language("go"))
	m.RelationshipsIndexed(ctx, 1200, Confidence("high"))
	m.SearchLatency(ctx, 3*time.Millisecond)
	m.SearchResultCount(ctx, 17)
	m.ContextLatency(ctx, 40*time.Millisecond)
	m.ContextCacheHit(ctx, true)
	m.ContextCacheHit(ctx, false)
	m.MCPRequest(ctx, "find_symbol", "ok")
	m.MCPLatency(ctx, "find_symbol", 8*time.Millisecond)
	m.GraphTraversalDepth(ctx, 3)
	m.DBQueryLatency(ctx, "select", 900*time.Microsecond)

	want := map[string]float64{
		"cortex.index.duration":         1,
		"cortex.index.files":            400,
		"cortex.index.parse_failures":   1,
		"cortex.index.symbols":          9000,
		"cortex.index.relationships":    1200,
		"cortex.search.latency":         1,
		"cortex.search.result_count":    1,
		"cortex.context.latency":        1,
		"cortex.context.cache_hit":      2,
		"cortex.mcp.request_count":      1,
		"cortex.mcp.latency":            1,
		"cortex.graph.traversal.depth":  1,
		"cortex.database.query.latency": 1,
	}
	for name, wantVal := range want {
		if got := collectSum(t, reader, name); got != wantVal {
			t.Errorf("%s = %v, want %v", name, got, wantVal)
		}
	}
}

// TestDurationsAreSeconds pins the unit. Recording milliseconds into an
// instrument declared in seconds is invisible in code review and produces a
// dashboard that is wrong by three orders of magnitude.
func TestDurationsAreSeconds(t *testing.T) {
	_, reader := enableForTest(t)
	m, _ := NewMetrics()
	m.SearchLatency(context.Background(), 250*time.Millisecond)

	metrics := collect(t, reader)
	got, ok := metrics["cortex.search.latency"]
	if !ok {
		t.Fatal("search latency not recorded")
	}
	if got.Unit != "s" {
		t.Errorf("unit = %q, want %q", got.Unit, "s")
	}
	hist, ok := got.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("search latency is %T, want a float64 histogram", got.Data)
	}
	if sum := hist.DataPoints[0].Sum; sum < 0.24 || sum > 0.26 {
		t.Errorf("recorded %v, want ~0.25 (seconds, not milliseconds)", sum)
	}
}

// TestLatencyBucketsAreUsable proves the explicit boundaries took effect. With
// the SDK defaults every RepoCortex latency lands in the first bucket and the
// histogram answers no question at all.
func TestLatencyBucketsAreUsable(t *testing.T) {
	_, reader := enableForTest(t)
	m, _ := NewMetrics()
	ctx := context.Background()
	for _, d := range []time.Duration{time.Millisecond, 30 * time.Millisecond, 400 * time.Millisecond} {
		m.SearchLatency(ctx, d)
	}

	hist := collect(t, reader)["cortex.search.latency"].Data.(metricdata.Histogram[float64])
	dp := hist.DataPoints[0]
	nonEmpty := 0
	for _, c := range dp.BucketCounts {
		if c > 0 {
			nonEmpty++
		}
	}
	if nonEmpty < 3 {
		t.Errorf("three latencies an order of magnitude apart landed in %d bucket(s); boundaries are not calibrated for seconds", nonEmpty)
	}
}

// TestParseFailureNormalisesLanguage: the language usually comes from a file
// extension, and an untrusted repository controls those. An unrecognised one
// must collapse to "other" rather than become its own metric dimension.
func TestParseFailureNormalisesLanguage(t *testing.T) {
	_, reader := enableForTest(t)
	m, _ := NewMetrics()
	ctx := context.Background()

	m.ParseFailure(ctx, "go")
	m.ParseFailure(ctx, "totally-made-up-language")
	m.ParseFailure(ctx, "another-made-up-one")

	sum, ok := collect(t, reader)["cortex.index.parse_failures"].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatal("parse failures not recorded as a counter")
	}
	seen := map[string]int64{}
	for _, dp := range sum.DataPoints {
		v, _ := dp.Attributes.Value(AttrLanguage)
		seen[v.String()] = dp.Value
	}
	if seen["go"] != 1 {
		t.Errorf("go failures = %d, want 1", seen["go"])
	}
	if seen[unknownToken] != 2 {
		t.Errorf("unknown languages = %d, want 2 collapsed into %q", seen[unknownToken], unknownToken)
	}
	if len(seen) != 2 {
		t.Errorf("parse failures produced %d language series (%v); unknown input must not create dimensions", len(seen), seen)
	}
}

// TestMetricsRejectUnsafeAttributes: the guard covers metrics, not only spans.
// A high-cardinality path label is worse on a metric than on a span — it is
// retained forever in a time series.
func TestMetricsRejectUnsafeAttributes(t *testing.T) {
	enableForTest(t)
	m, _ := NewMetrics()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("recording a file path as a metric attribute did not panic under strict mode")
		}
	}()
	m.FilesIndexed(context.Background(), 1, badAttr())
}
