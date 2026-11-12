package web

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/runtime"
)

// otlpExport builds a minimal OTLP/JSON document.
//
// Written out rather than generated so the attributes under test — the ones
// internal/trace's allow-list keeps — are visible in the test.
const otlpExport = `{
  "resourceSpans": [{
    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "orders-api"}}]},
    "scopeSpans": [{
      "spans": [
        {"traceId": "aa", "spanId": "01", "name": "GET /orders",
         "startTimeUnixNano": "1000000000", "endTimeUnixNano": "1004000000",
         "attributes": [
           {"key": "http.route", "value": {"stringValue": "/orders"}},
           {"key": "http.method", "value": {"stringValue": "GET"}}
         ]},
        {"traceId": "aa", "spanId": "02", "parentSpanId": "01", "name": "hub.work",
         "startTimeUnixNano": "1001000000", "endTimeUnixNano": "1002000000",
         "attributes": [
           {"key": "code.function", "value": {"stringValue": "hub"}}
         ]},
        {"traceId": "aa", "spanId": "03", "parentSpanId": "01", "name": "redis GET session",
         "startTimeUnixNano": "1002000000", "endTimeUnixNano": "1002500000",
         "attributes": []}
      ]
    }]
  }]
}`

// TestTraceCorrelatesAnExportWithTheIndex is the populated case: an export in
// the repository, correlated against the seeded index.
func TestTraceCorrelatesAnExportWithTheIndex(t *testing.T) {
	t.Parallel()
	srv, _, _, root := repoFixture(t)
	writeRepoFile(t, root, traceExportFile, otlpExport)

	var got TraceResponse
	pillarGet(t, srv.handleTrace, "/api/trace", &got)

	if !got.Correlated {
		t.Fatalf("correlated is false: export %+v, guidance %+v", got.Export, got.Help)
	}
	if got.Format != "otlp-json" {
		t.Errorf("format = %q, want otlp-json", got.Format)
	}
	if got.Spans != 3 {
		t.Errorf("spans = %d, want 3", got.Spans)
	}
	if !got.Export.Found || !got.Export.Defaulted {
		t.Errorf("export = %+v, want found and defaulted to the conventional name", got.Export)
	}

	// Every match must say HOW it was made: that field is the difference
	// between a correlation and a guess.
	byMatch := map[runtime.MatchKind]int{}
	for _, o := range got.Exercised {
		if o.Match == "" {
			t.Errorf("%s was correlated with no match kind", o.Name)
		}
		if o.Name == "" {
			t.Error("an exercised row carries no entity name")
		}
		byMatch[o.Match]++
	}
	if byMatch[runtime.MatchRoute] == 0 {
		t.Errorf("the http.route span did not match the indexed endpoint: %+v", got.Exercised)
	}
	if byMatch[runtime.MatchCodeAttribute] == 0 {
		t.Errorf("the code.function span did not match the indexed symbol: %+v", got.Exercised)
	}

	if got.Coverage.IndexedEndpoints == 0 {
		t.Error("coverage.indexed_endpoints is 0; the fixture declares GET /orders")
	}
	if !strings.Contains(got.Coverage.Detail, "in this window") &&
		!strings.Contains(got.Coverage.Detail, "THIS export") {
		t.Errorf("coverage detail does not scope coldness to the window: %q", got.Coverage.Detail)
	}
	if len(got.Caveats) == 0 {
		t.Error("no caveats: cold would then read as dead")
	}

	// The unmatched span is stated and counted, never dropped.
	if got.Coverage.Unmatched == 0 {
		t.Errorf("the uninstrumented span matched nothing and was not reported: %+v", got.Unmatched)
	}
	var noted bool
	for _, om := range got.Omissions.Omitted {
		if strings.Contains(om.Detail, "matched nothing indexed") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("unmatched operations are not in the omission block: %+v", got.Omissions.Omitted)
	}
}

// TestTraceWithoutAnExportReportsCoverageRatherThanDeadCode is the empty path,
// and it is the one a real repository takes.
//
// The failure it prevents: rendering every indexed endpoint as cold because
// nothing has been imported. Unobserved is a third state.
func TestTraceWithoutAnExportReportsCoverageRatherThanDeadCode(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := repoFixture(t)

	var got TraceResponse
	pillarGet(t, srv.handleTrace, "/api/trace", &got)

	if got.Correlated {
		t.Fatal("correlated is true with no export in the repository")
	}
	if len(got.ColdEndpoints) != 0 {
		t.Error("endpoints were reported COLD with no trace to prove it; that is the misreading " +
			"this screen exists to prevent")
	}
	if got.Coverage.IndexedEndpoints == 0 {
		t.Error("coverage.indexed_endpoints is 0: an empty screen must report what the index holds")
	}
	if got.Help == nil {
		t.Fatal("no guidance for a missing export")
	}
	if !strings.Contains(got.Help.Title, "No trace export") {
		t.Errorf("guidance title = %q", got.Help.Title)
	}
	if !strings.Contains(got.Help.Detail, "UNOBSERVED") {
		t.Errorf("guidance does not name the third state: %q", got.Help.Detail)
	}
	steps := strings.Join(got.Help.Steps, "\n")
	if !strings.Contains(steps, "repocortex trace") {
		t.Errorf("guidance names no command that populates the screen: %q", steps)
	}
	if got.Export.Path != traceExportFile || !got.Export.Defaulted {
		t.Errorf("export = %+v, want the conventional name, marked defaulted", got.Export)
	}
	if got.Export.DefaultedFrom == "" {
		t.Error("a defaulted path must say what it defaulted to")
	}
}

// TestTraceRejectsAnUnreadableExport covers the two bad-file paths.
func TestTraceRejectsAnUnreadableExport(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		body      string
		query     string
		wantTitle string
	}{
		{name: "not a trace at all", body: `{"hello":"world"}`, wantTitle: "is not a trace export"},
		{name: "a valid export of an empty window", body: `{"resourceSpans":[]}`, wantTitle: "contains no spans"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _, _, root := repoFixture(t)
			writeRepoFile(t, root, traceExportFile, tc.body)

			var got TraceResponse
			pillarGet(t, srv.handleTrace, "/api/trace"+tc.query, &got)

			if got.Correlated {
				t.Fatal("correlated is true over an unusable export")
			}
			if got.Help == nil || !strings.Contains(got.Help.Title, tc.wantTitle) {
				t.Fatalf("guidance = %+v, want a title containing %q", got.Help, tc.wantTitle)
			}
			if got.Coverage.IndexedEndpoints == 0 {
				t.Error("the index's own coverage was dropped along with the export")
			}
		})
	}
}

// TestTraceRefusesAPathOutsideTheRepository: the containment rule, at the
// handler rather than at the helper.
func TestTraceRefusesAPathOutsideTheRepository(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := repoFixture(t)

	var got TraceResponse
	pillarGet(t, srv.handleTrace, "/api/trace?export=../../../../etc/hosts", &got)

	if got.Correlated {
		t.Fatal("a path outside the repository was read")
	}
	if got.Export.Problem == "" {
		t.Fatal("the refusal was not stated")
	}
	if !strings.Contains(got.Export.Problem, "outside the indexed repository") {
		t.Errorf("problem = %q, want it to name the containment rule", got.Export.Problem)
	}
}

// TestTraceOnAnUnindexedStoreAnswersWithGuidance.
func TestTraceOnAnUnindexedStoreAnswersWithGuidance(t *testing.T) {
	t.Parallel()
	srv := pillarUnindexedServer(t)

	var got TraceResponse
	pillarGet(t, srv.handleTrace, "/api/trace", &got)
	if got.Help == nil {
		t.Fatal("no guidance over a store with no committed run")
	}
	if got.Correlated {
		t.Error("correlated is true with no index")
	}
}
