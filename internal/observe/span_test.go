package observe

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
)

// TestEnabledPathRecordsSpans covers the basics of the enabled path: the span
// reaches the exporter, carries its attributes, and nests under its parent.
func TestEnabledPathRecordsSpans(t *testing.T) {
	spans, _ := enableForTest(t)

	ctx, parent := StartSpan(context.Background(), "index.run", AccuracyMode("auto"))
	_, child := StartSpan(ctx, "index.file", Language("go"), SizeBucket(2048))
	child.SetAttr(ParseStatus("ok"))
	child.End()
	parent.End()

	stubs := spans.GetSpans()
	if len(stubs) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(stubs))
	}
	byName := map[string]int{}
	for _, s := range stubs {
		byName[s.Name]++
	}
	if byName["index.run"] != 1 || byName["index.file"] != 1 {
		t.Fatalf("unexpected span names: %v", byName)
	}

	var childStub, parentStub = stubs[0], stubs[1]
	if childStub.Name != "index.file" {
		childStub, parentStub = parentStub, childStub
	}
	if childStub.Parent.SpanID() != parentStub.SpanContext.SpanID() {
		t.Error("child span is not parented to index.run; the context was not propagated")
	}

	got := map[string]string{}
	for _, kv := range childStub.Attributes {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got[AttrLanguage] != "go" || got[AttrSizeBucket] != "4k" || got[AttrParseStatus] != "ok" {
		t.Errorf("child attributes = %v", got)
	}
}

// TestRecordErrorSetsErrorStatus: the span must still be recognisably a
// failure even though the message is withheld.
func TestRecordErrorSetsErrorStatus(t *testing.T) {
	spans, _ := enableForTest(t)

	_, span := StartSpan(context.Background(), "search.query")
	span.RecordError(context.DeadlineExceeded)
	span.End()

	stubs := spans.GetSpans()
	if len(stubs) != 1 {
		t.Fatalf("got %d spans, want 1", len(stubs))
	}
	if stubs[0].Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", stubs[0].Status.Code)
	}
}

// TestRecordNilError guards the ordinary defer-and-record shape, where err is
// usually nil.
func TestRecordNilError(t *testing.T) {
	spans, _ := enableForTest(t)

	_, span := StartSpan(context.Background(), "search.query")
	span.RecordError(nil)
	span.End()

	if code := spans.GetSpans()[0].Status.Code; code == codes.Error {
		t.Error("RecordError(nil) marked the span failed")
	}
}

// TestConcurrentUseIsRaceFree exercises the paths that share mutable state:
// the active-providers pointer, the lazily bound instruments, and the guard.
// Run under -race, which is what CONTRIBUTING requires of every test run.
func TestConcurrentUseIsRaceFree(t *testing.T) {
	_, reader := enableForTest(t)
	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	const workers, iterations = 16, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ctx, span := StartSpan(context.Background(), "index.file", Language("go"))
				span.SetAttr(SizeBucket(int64(i) * 512))
				m.FilesIndexed(ctx, 1, Language("go"))
				m.DBQueryLatency(ctx, "select", time.Microsecond)
				m.ContextCacheHit(ctx, i%2 == 0)
				span.End()
			}
		}(w)
	}
	wg.Wait()

	if got := collectSum(t, reader, "cortex.index.files"); got != workers*iterations {
		t.Errorf("index.files = %v, want %d", got, workers*iterations)
	}
}

// TestConcurrentInitAndRecord is the nastier race: measurements in flight while
// telemetry is switched on and off underneath them. A recorder must degrade to
// dropping data, never to a panic.
func TestConcurrentInitAndRecord(t *testing.T) {
	Reset()
	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, span := StartSpan(context.Background(), "search.query")
				m.SearchLatency(ctx, time.Millisecond)
				m.SearchResultCount(ctx, 3)
				span.End()
			}
		}()
	}

	for i := 0; i < 5; i++ {
		spans, reader := newTestExporters()
		shutdown, err := Init(context.Background(), TelemetryOptions{
			spanExporter:    spans,
			metricReader:    reader,
			ShutdownTimeout: time.Second,
		})
		if err != nil {
			t.Errorf("Init: %v", err)
			break
		}
		time.Sleep(2 * time.Millisecond)
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}

	close(stop)
	wg.Wait()
	Reset()
}

// TestSpanNameStaysCategorical documents the accepted shape by example, so the
// panic in TestStartSpanPanicsOnUnsafeName does not read as arbitrary.
func TestSpanNameStaysCategorical(t *testing.T) {
	Reset()
	for _, name := range []string{"index.run", "index.file", "parse.tree-sitter", "db.query", "mcp.find_symbol"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("span name %q was rejected: %v", name, r)
				}
			}()
			_, span := StartSpan(context.Background(), name)
			span.End()
		}()
	}
}

func TestSpanContextFrom(t *testing.T) {
	enableForTest(t)

	traceID, spanID := SpanContextFrom(context.Background())
	if traceID != "" || spanID != "" {
		t.Errorf("bare context reported ids %q/%q", traceID, spanID)
	}

	ctx, span := StartSpan(context.Background(), "index.run")
	defer span.End()
	traceID, spanID = SpanContextFrom(ctx)
	if len(traceID) != 32 || strings.Trim(traceID, "0") == "" {
		t.Errorf("trace id = %q, want a valid 32-hex id", traceID)
	}
	if len(spanID) != 16 || strings.Trim(spanID, "0") == "" {
		t.Errorf("span id = %q, want a valid 16-hex id", spanID)
	}
}
