package observe

// The spec §35 metric set, as typed recorders.
//
// Typed rather than a generic Record(name, value) for two reasons. It puts the
// unit and instrument choice in one place, where "is search latency a histogram
// or a gauge" is decided once instead of at each call site; and it removes the
// opportunity to pass a metric name built from data, which is the metric-side
// version of the leak redact.go guards against.
//
// Units follow the OTel convention: seconds, not milliseconds. Durations arrive
// as time.Duration and are converted here, so no call site ever has to remember
// which it was.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// latencyBuckets are explicit histogram boundaries in seconds, covering a
// sub-millisecond cache hit through a ten-second query.
//
// The SDK default boundaries (0, 5, 10, 25, ... 10000) assume milliseconds and
// would put every RepoCortex latency in the first bucket, making the histogram
// useless while still costing memory per series. Supplying boundaries is the
// difference between a p95 that means something and one that reads "< 5".
var latencyBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// indexDurationBuckets span a whole indexing run: a warm incremental pass on a
// small repo through a cold full index of a large monorepo.
var indexDurationBuckets = []float64{
	0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800,
}

// countBuckets describe result-set sizes and traversal breadth.
var countBuckets = []float64{0, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000}

// depthBuckets describe graph traversal depth. Depth is small by construction —
// the context engine bounds expansion — so the boundaries are the integers that
// matter rather than a logarithmic spread.
var depthBuckets = []float64{1, 2, 3, 4, 5, 6, 8, 10, 15, 20}

// instruments is one generation's worth of bound instruments.
type instruments struct {
	gen uint64

	indexDuration      metric.Float64Histogram
	indexFiles         metric.Int64Counter
	indexParseFailures metric.Int64Counter
	indexSymbols       metric.Int64Counter
	indexRelationships metric.Int64Counter

	searchLatency     metric.Float64Histogram
	searchResultCount metric.Int64Histogram

	contextLatency  metric.Float64Histogram
	contextCacheHit metric.Int64Counter

	mcpRequests metric.Int64Counter
	mcpLatency  metric.Float64Histogram

	graphTraversalDepth metric.Int64Histogram
	dbQueryLatency      metric.Float64Histogram
}

// Metrics records RepoCortex's spec §35 measurements.
//
// A *Metrics is safe for concurrent use and safe to construct before Init.
// Instruments are bound lazily against whichever providers are active and
// rebound if Init runs again, so wiring order at startup — and re-Init in
// tests — cannot silently produce a Metrics that records into a dead provider.
type Metrics struct {
	// bound is the instrument set for the current generation, or nil.
	// Loading it is the only work a recorder does when telemetry is on.
	bound atomic.Pointer[instruments]

	// bindMu serialises rebinding so a burst of concurrent recorders after
	// Init creates one instrument set, not one per goroutine.
	bindMu sync.Mutex

	// bindErr remembers a failed binding so a broken instrument definition is
	// reported once rather than on every measurement.
	bindErr atomic.Bool
}

// NewMetrics returns a recorder for the spec §35 metric set.
//
// It returns an error only when telemetry is already active and the SDK rejects
// an instrument definition — a programming error in this file, surfaced at
// startup rather than as silently missing data.
func NewMetrics() (*Metrics, error) {
	m := &Metrics{}
	if p := active.Load(); p != nil {
		if _, err := m.bind(p); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// inst returns the live instrument set, or nil when telemetry is off.
//
// This is the hot path shared by every recorder: one atomic load for the
// providers, one for the cached instruments, one integer compare. No lock, no
// allocation.
func (m *Metrics) inst() *instruments {
	p := active.Load()
	if p == nil {
		return nil
	}
	if i := m.bound.Load(); i != nil && i.gen == p.gen {
		return i
	}
	i, err := m.bind(p)
	if err != nil {
		return nil
	}
	return i
}

func (m *Metrics) bind(p *providers) (*instruments, error) {
	m.bindMu.Lock()
	defer m.bindMu.Unlock()

	if i := m.bound.Load(); i != nil && i.gen == p.gen {
		return i, nil
	}

	i, err := newInstruments(p)
	if err != nil {
		if !m.bindErr.Swap(true) {
			otel.Handle(fmt.Errorf("telemetry instruments unavailable, measurements will be dropped: %w", err))
		}
		return nil, err
	}
	m.bound.Store(i)
	return i, nil
}

func newInstruments(p *providers) (*instruments, error) {
	me := p.meter
	var errs []error
	f64hist := func(name, unit, desc string, bounds []float64) metric.Float64Histogram {
		h, err := me.Float64Histogram(name,
			metric.WithUnit(unit),
			metric.WithDescription(desc),
			metric.WithExplicitBucketBoundaries(bounds...))
		if err != nil {
			errs = append(errs, err)
		}
		return h
	}
	i64hist := func(name, unit, desc string, bounds []float64) metric.Int64Histogram {
		h, err := me.Int64Histogram(name,
			metric.WithUnit(unit),
			metric.WithDescription(desc),
			metric.WithExplicitBucketBoundaries(bounds...))
		if err != nil {
			errs = append(errs, err)
		}
		return h
	}
	i64count := func(name, unit, desc string) metric.Int64Counter {
		c, err := me.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		if err != nil {
			errs = append(errs, err)
		}
		return c
	}

	i := &instruments{
		gen: p.gen,

		indexDuration: f64hist("repocortex.index.duration", "s",
			"Wall time of one indexing run.", indexDurationBuckets),
		indexFiles: i64count("repocortex.index.files", "{file}",
			"Files indexed, by language."),
		indexParseFailures: i64count("repocortex.index.parse_failures", "{file}",
			"Files whose parse failed or degraded, by language."),
		indexSymbols: i64count("repocortex.index.symbols", "{symbol}",
			"Symbols extracted."),
		indexRelationships: i64count("repocortex.index.relationships", "{relationship}",
			"Graph edges recorded, by confidence."),

		searchLatency: f64hist("repocortex.search.latency", "s",
			"End-to-end search query latency.", latencyBuckets),
		searchResultCount: i64hist("repocortex.search.result_count", "{result}",
			"Results returned per search.", countBuckets),

		contextLatency: f64hist("repocortex.context.latency", "s",
			"Context assembly latency.", latencyBuckets),
		contextCacheHit: i64count("repocortex.context.cache_hit", "{lookup}",
			"Context cache lookups, labelled hit or miss."),

		mcpRequests: i64count("repocortex.mcp.request_count", "{request}",
			"MCP tool invocations, by tool and status."),
		mcpLatency: f64hist("repocortex.mcp.latency", "s",
			"MCP request handling latency, by tool.", latencyBuckets),

		graphTraversalDepth: i64hist("repocortex.graph.traversal.depth", "{hop}",
			"Depth reached by a graph traversal.", depthBuckets),
		dbQueryLatency: f64hist("repocortex.database.query.latency", "s",
			"SQLite query latency, by operation.", latencyBuckets),
	}

	if len(errs) > 0 {
		return nil, errs[0]
	}
	return i, nil
}

// safeAttrs copies and validates attributes for a measurement.
//
// Same escape-analysis reasoning as StartSpan: the copy is what keeps the
// caller's variadic slice on the stack. Recorders call this only after
// establishing that telemetry is on.
func safeAttrs(attrs []attribute.KeyValue) metric.MeasurementOption {
	if len(attrs) == 0 {
		return metric.WithAttributes()
	}
	safe := make([]attribute.KeyValue, len(attrs))
	copy(safe, attrs)
	guardAttrs(safe)
	return metric.WithAttributes(safe...)
}

// guardOnly validates without recording, for the disabled-but-strict path. It
// exists so a call site that would attach a path is caught by tests even though
// those tests run with telemetry off.
//
// It checks rather than scrubs: the slice belongs to the caller, and there is
// nothing to sanitise for when the measurement is being dropped anyway.
func guardOnly(attrs []attribute.KeyValue) {
	if len(attrs) == 0 || !strict.Load() {
		return
	}
	if err := CheckAttrs(attrs); err != nil {
		panic(err)
	}
}

// secs converts to the unit every duration metric here is declared in.
func secs(d time.Duration) float64 { return d.Seconds() }

// ---------------------------------------------------------------------------
// Indexing — spec §35: index.duration, index.files, index.parse_failures,
// index.symbols, index.relationships
// ---------------------------------------------------------------------------

// IndexDuration records the wall time of an indexing run. Attach AccuracyMode
// to separate a fast pass from an exact one.
func (m *Metrics) IndexDuration(ctx context.Context, d time.Duration, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.indexDuration.Record(ctx, secs(d), safeAttrs(attrs))
}

// FilesIndexed records files completed. Attach Language to get the per-language
// breakdown; the count is meaningless without it on a polyglot repository.
func (m *Metrics) FilesIndexed(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.indexFiles.Add(ctx, n, safeAttrs(attrs))
}

// ParseFailure records one file that failed to parse.
//
// It takes a language rather than attributes because that is the only dimension
// worth having, and because the tempting alternative — attaching the parse
// error — would export a message containing the path and often the offending
// source line. Which file failed belongs in the local log; how many files of
// which language failed belongs here.
func (m *Metrics) ParseFailure(ctx context.Context, lang string) {
	i := m.inst()
	if i == nil {
		return
	}
	i.indexParseFailures.Add(ctx, 1, metric.WithAttributes(Language(lang)))
}

// SymbolsIndexed records symbols extracted, ideally attributed by Language.
func (m *Metrics) SymbolsIndexed(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.indexSymbols.Add(ctx, n, safeAttrs(attrs))
}

// RelationshipsIndexed records edges written.
//
// Attach Confidence: RepoCortex's central claim is that a proven relationship and
// a guessed one are different kinds of fact (CONTRIBUTING, invariant 1), and a
// relationship count that does not distinguish them measures the wrong thing —
// an index that doubled its edges by lowering its standards would look like an
// improvement.
func (m *Metrics) RelationshipsIndexed(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.indexRelationships.Add(ctx, n, safeAttrs(attrs))
}

// ---------------------------------------------------------------------------
// Search — spec §35: search.latency, search.result_count
// ---------------------------------------------------------------------------

// SearchLatency records query latency. The query text is never an attribute:
// it is the user's own words about their own code.
func (m *Metrics) SearchLatency(ctx context.Context, d time.Duration, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.searchLatency.Record(ctx, secs(d), safeAttrs(attrs))
}

// SearchResultCount records how many results a search returned. A histogram
// rather than a counter: the distribution is the signal — a spike of zero-result
// searches means the index is wrong, and a total would hide it.
func (m *Metrics) SearchResultCount(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.searchResultCount.Record(ctx, n, safeAttrs(attrs))
}

// ---------------------------------------------------------------------------
// Context engine — spec §35: context.latency, context.cache_hit
// ---------------------------------------------------------------------------

// ContextLatency records context assembly latency.
func (m *Metrics) ContextLatency(ctx context.Context, d time.Duration, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.contextLatency.Record(ctx, secs(d), safeAttrs(attrs))
}

// ContextCacheHit records one cache lookup and its outcome.
//
// Both outcomes go to the same counter, labelled hit=true|false, because the
// number that matters is the ratio and a ratio cannot be computed from hits
// alone. A separate misses counter would work equally well and drift apart the
// first time someone instruments a new lookup path and updates only one.
func (m *Metrics) ContextCacheHit(ctx context.Context, hit bool) {
	i := m.inst()
	if i == nil {
		return
	}
	i.contextCacheHit.Add(ctx, 1, metric.WithAttributes(CacheHit(hit)))
}

// ---------------------------------------------------------------------------
// MCP server — spec §35: mcp.request_count, mcp.latency
// ---------------------------------------------------------------------------

// MCPRequest records one MCP tool invocation and its outcome. Both labels are
// normalised: the tool name arrives from the client over the wire, so it is
// untrusted input even though it should always be one of RepoCortex's own.
func (m *Metrics) MCPRequest(ctx context.Context, tool, status string) {
	i := m.inst()
	if i == nil {
		return
	}
	i.mcpRequests.Add(ctx, 1, metric.WithAttributes(Tool(tool), Status(status)))
}

// MCPLatency records MCP request handling latency, by tool.
func (m *Metrics) MCPLatency(ctx context.Context, tool string, d time.Duration) {
	i := m.inst()
	if i == nil {
		return
	}
	i.mcpLatency.Record(ctx, secs(d), metric.WithAttributes(Tool(tool)))
}

// ---------------------------------------------------------------------------
// Graph and storage — spec §35: graph.traversal.depth, database.query.latency
// ---------------------------------------------------------------------------

// GraphTraversalDepth records how deep a traversal went. Depth is a pure shape
// measurement: it says how far the context engine expanded, never through what.
func (m *Metrics) GraphTraversalDepth(ctx context.Context, depth int64, attrs ...attribute.KeyValue) {
	i := m.inst()
	if i == nil {
		guardOnly(attrs)
		return
	}
	i.graphTraversalDepth.Record(ctx, depth, safeAttrs(attrs))
}

// DBQueryLatency records SQLite query latency by operation.
//
// op is a verb ("select", "insert", "migrate"), never a statement. A statement
// carries table and column names, which for RepoCortex are derived from the
// indexed repository's schema — exactly the kind of structure the index exists
// to describe and telemetry must not disclose.
func (m *Metrics) DBQueryLatency(ctx context.Context, op string, d time.Duration) {
	i := m.inst()
	if i == nil {
		return
	}
	i.dbQueryLatency.Record(ctx, secs(d), metric.WithAttributes(Operation(op)))
}
