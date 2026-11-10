package trace

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests are organised around the three things this package must not get
// wrong: the wire-format variations that make real exports fail to parse, the
// attributes it must refuse to keep, and the numbers it reports as exact.

// ------------------------------------------------------------- end to end

func TestParseReadsBothFormatsEndToEnd(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		wantFormat Format
		wantSpans  []Span
	}{
		{
			name:       "otlp span carries service, kind, status, times and allowed attributes",
			wantFormat: FormatOTLPJSON,
			src: `{"resourceSpans":[{
				"resource":{"attributes":[
					{"key":"service.name","value":{"stringValue":"payments"}},
					{"key":"host.name","value":{"stringValue":"box-17"}}
				]},
				"scopeSpans":[{"spans":[{
					"traceId":"0123456789abcdef0123456789abcdef",
					"spanId":"1122334455667788",
					"parentSpanId":"0000000000000000",
					"name":"GET /v1/charges/{id}",
					"kind":"SPAN_KIND_SERVER",
					"startTimeUnixNano":"1700000000000000000",
					"endTimeUnixNano":"1700000000012000000",
					"status":{"code":"STATUS_CODE_OK"},
					"attributes":[
						{"key":"http.route","value":{"stringValue":"/v1/charges/{id}"}},
						{"key":"http.method","value":{"stringValue":"get"}},
						{"key":"code.function","value":{"stringValue":"ChargeHandler"}},
						{"key":"code.filepath","value":{"stringValue":"internal/api/charge.go"}},
						{"key":"code.lineno","value":{"intValue":"142"}}
					]
				}]}]
			}]}`,
			wantSpans: []Span{{
				TraceID:    "0123456789abcdef0123456789abcdef",
				SpanID:     "1122334455667788",
				ParentID:   "", // all-zero parent means root, not a span called zero
				Name:       "GET /v1/charges/{id}",
				Service:    "payments",
				Kind:       "server",
				StartNanos: 1700000000000000000,
				EndNanos:   1700000000012000000,
				Status:     "ok",
				Attributes: map[string]string{
					"http.route":    "/v1/charges/{id}",
					"http.method":   "get",
					"code.function": "ChargeHandler",
					"code.filepath": "internal/api/charge.go",
					"code.lineno":   "142",
				},
			}},
		},
		{
			name:       "otlp accepts the pre-1.0 instrumentationLibrarySpans spelling",
			wantFormat: FormatOTLPJSON,
			src: `{"resourceSpans":[{
				"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"legacy"}}]},
				"instrumentationLibrarySpans":[{"spans":[{
					"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"spanId":"bbbbbbbbbbbbbbbb",
					"name":"work",
					"kind":2,
					"status":{"code":2}
				}]}]
			}]}`,
			wantSpans: []Span{{
				TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SpanID:  "bbbbbbbbbbbbbbbb",
				Name:    "work",
				Service: "legacy",
				Kind:    "server",
				Status:  "error",
			}},
		},
		{
			name:       "jaeger span resolves service from processes and parent from references",
			wantFormat: FormatJaeger,
			src: `{"data":[{
				"traceID":"0123456789abcdef0123456789abcdef",
				"spans":[{
					"traceID":"0123456789abcdef0123456789abcdef",
					"spanID":"1122334455667788",
					"operationName":"SELECT charges",
					"references":[{"refType":"CHILD_OF","spanID":"8877665544332211"}],
					"startTime":1700000000000000,
					"duration":2500,
					"tags":[
						{"key":"span.kind","type":"string","value":"client"},
						{"key":"db.sql.table","type":"string","value":"charges"},
						{"key":"db.operation","type":"string","value":"SELECT"},
						{"key":"error","type":"bool","value":true}
					],
					"processID":"p1"
				}],
				"processes":{"p1":{"serviceName":"payments"}}
			}]}`,
			wantSpans: []Span{{
				TraceID:    "0123456789abcdef0123456789abcdef",
				SpanID:     "1122334455667788",
				ParentID:   "8877665544332211",
				Name:       "SELECT charges",
				Service:    "payments",
				Kind:       "client",
				StartNanos: 1700000000000000000,
				EndNanos:   1700000000002500000,
				Status:     "error",
				Attributes: map[string]string{
					"db.sql.table": "charges",
					"db.operation": "SELECT",
				},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans, format, err := Parse([]byte(tc.src), 0)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if format != tc.wantFormat {
				t.Errorf("format = %q, want %q", format, tc.wantFormat)
			}
			assertSpans(t, spans, tc.wantSpans)
		})
	}
}

// ------------------------------------------------------------- wire variance

func TestParseAcceptsTimestampsAsStringOrNumber(t *testing.T) {
	// Timestamps arriving as JSON strings against a decoder expecting numbers
	// is the most common way real OTLP/JSON fails to parse: protobuf's JSON
	// mapping renders int64 as a string, but plenty of producers emit numbers.
	// Both must yield the same instant.
	cases := []struct {
		name       string
		start, end string
		wantStart  int64
		wantEnd    int64
	}{
		{name: "string encoded int64", start: `"1700000000000000000"`, end: `"1700000000005000000"`,
			wantStart: 1700000000000000000, wantEnd: 1700000000005000000},
		{name: "number encoded int64", start: `1700000000000000000`, end: `1700000000005000000`,
			wantStart: 1700000000000000000, wantEnd: 1700000000005000000},
		{name: "missing end leaves zero rather than dropping the span", start: `"1700000000000000000"`, end: `null`,
			wantStart: 1700000000000000000, wantEnd: 0},
		{name: "unparseable timestamp leaves zero rather than dropping the span", start: `"not-a-number"`, end: `"x"`,
			wantStart: 0, wantEnd: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[
				{"traceId":"0123456789abcdef0123456789abcdef","spanId":"1122334455667788",
				 "name":"op","startTimeUnixNano":%s,"endTimeUnixNano":%s}]}]}]}`, tc.start, tc.end)
			spans, _, err := Parse([]byte(src), 0)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want 1", len(spans))
			}
			if spans[0].StartNanos != tc.wantStart || spans[0].EndNanos != tc.wantEnd {
				t.Errorf("start/end = %d/%d, want %d/%d",
					spans[0].StartNanos, spans[0].EndNanos, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

func TestParseAcceptsIdsAsHexOrBase64(t *testing.T) {
	// protojson encodes the `bytes` trace/span ids as base64; the OTLP/JSON
	// spec says hex. Both must normalise to the same lowercase hex string, or
	// one trace becomes two.
	traceHex := "0123456789abcdef0123456789abcdef"
	spanHex := "1122334455667788"
	traceRaw := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	spanRaw := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	cases := []struct {
		name              string
		trace, span       string
		wantTrace, wantID string
	}{
		{name: "hex as written by the otlp json spec",
			trace: traceHex, span: spanHex, wantTrace: traceHex, wantID: spanHex},
		{name: "uppercase hex normalises to lowercase",
			trace: strings.ToUpper(traceHex), span: strings.ToUpper(spanHex), wantTrace: traceHex, wantID: spanHex},
		{name: "padded base64 as written by protojson",
			trace:     base64.StdEncoding.EncodeToString(traceRaw),
			span:      base64.StdEncoding.EncodeToString(spanRaw),
			wantTrace: traceHex, wantID: spanHex},
		{name: "unpadded base64",
			trace:     base64.RawStdEncoding.EncodeToString(traceRaw),
			span:      base64.RawStdEncoding.EncodeToString(spanRaw),
			wantTrace: traceHex, wantID: spanHex},
		{name: "all zero base64 span id is no parent, not a span named zero",
			trace:     base64.StdEncoding.EncodeToString(make([]byte, 16)),
			span:      base64.StdEncoding.EncodeToString(make([]byte, 8)),
			wantTrace: "", wantID: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[
				{"traceId":%q,"spanId":%q,"name":"op"}]}]}]}`, tc.trace, tc.span)
			spans, _, err := Parse([]byte(src), 0)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want 1", len(spans))
			}
			if spans[0].TraceID != tc.wantTrace {
				t.Errorf("TraceID = %q, want %q", spans[0].TraceID, tc.wantTrace)
			}
			if spans[0].SpanID != tc.wantID {
				t.Errorf("SpanID = %q, want %q", spans[0].SpanID, tc.wantID)
			}
		})
	}
}

func TestParseJaegerConvertsMicrosecondsToNanoseconds(t *testing.T) {
	// The one unit difference between the two formats. Getting it wrong is a
	// silent factor of 1000 on every latency this package reports.
	cases := []struct {
		name               string
		start, duration    string
		wantStart, wantEnd int64
		wantDuration       time.Duration
	}{
		{name: "one millisecond duration", start: "1700000000000000", duration: "1000",
			wantStart: 1700000000000000000, wantEnd: 1700000000001000000, wantDuration: time.Millisecond},
		{name: "sub millisecond duration keeps microsecond resolution", start: "1700000000000000", duration: "1",
			wantStart: 1700000000000000000, wantEnd: 1700000000000001000, wantDuration: time.Microsecond},
		{name: "string encoded micros", start: `"1700000000000000"`, duration: `"2000"`,
			wantStart: 1700000000000000000, wantEnd: 1700000000002000000, wantDuration: 2 * time.Millisecond},
		{name: "absurd start is rejected rather than overflowed into a negative instant",
			start: "9223372036854775807", duration: "1000",
			wantStart: 0, wantEnd: 0, wantDuration: 0},
		{name: "negative duration yields no end rather than a negative one",
			start: "1700000000000000", duration: "-5",
			wantStart: 1700000000000000000, wantEnd: 0, wantDuration: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(`{"data":[{"traceID":"0123456789abcdef0123456789abcdef","spans":[
				{"spanID":"1122334455667788","operationName":"op","startTime":%s,"duration":%s,"processID":"p1"}],
				"processes":{"p1":{"serviceName":"svc"}}}]}`, tc.start, tc.duration)
			spans, format, err := Parse([]byte(src), 0)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if format != FormatJaeger {
				t.Fatalf("format = %q, want %q", format, FormatJaeger)
			}
			if spans[0].StartNanos != tc.wantStart || spans[0].EndNanos != tc.wantEnd {
				t.Errorf("start/end = %d/%d, want %d/%d",
					spans[0].StartNanos, spans[0].EndNanos, tc.wantStart, tc.wantEnd)
			}
			if got := time.Duration(spans[0].DurationNanos()); got != tc.wantDuration {
				t.Errorf("DurationNanos = %v, want %v", got, tc.wantDuration)
			}
		})
	}
}

func TestDetectIdentifiesFormatFromTheFirstBytes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want Format
	}{
		{name: "otlp by resourceSpans", src: `{"resourceSpans":[]}`, want: FormatOTLPJSON},
		{name: "otlp by the pre-1.0 spelling", src: `{"resourceSpans":[{"instrumentationLibrarySpans":[]}]}`, want: FormatOTLPJSON},
		{name: "jaeger by the capital D in traceID", src: `{"data":[{"traceID":"abc"}]}`, want: FormatJaeger},
		{name: "jaeger by the processes map", src: `{"data":[{"processes":{}}]}`, want: FormatJaeger},
		{name: "an unrelated json document is not a trace export", src: `{"items":[1,2,3]}`, want: FormatUnknown},
		{name: "empty input", src: ``, want: FormatUnknown},
		{name: "nul bytes", src: "\x00\x00\x00\x00", want: FormatUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect([]byte(tc.src)); got != tc.want {
				t.Errorf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}

// ------------------------------------------------------------- allow-list

func TestParseDropsEveryAttributeOutsideTheAllowList(t *testing.T) {
	// The allow-list is a security boundary. A `db.statement` carrying a
	// credential must not survive parsing in ANY form — not under its own key,
	// not under another key, and not anywhere in the returned value.
	const secret = "postgres://admin:hunter2@db.internal:5432/prod"

	cases := []struct {
		name    string
		src     string
		wantKey string
		wantVal string
	}{
		{
			name: "otlp keeps db.sql.table and drops db.statement holding a credential",
			src: `{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"q","attributes":[
				{"key":"db.statement","value":{"stringValue":"SELECT * FROM charges -- ` + secret + `"}},
				{"key":"http.url","value":{"stringValue":"https://api/x?access_token=sk_live_9931"}},
				{"key":"http.target","value":{"stringValue":"/v1/charges/ch_3Nk1"}},
				{"key":"messaging.message.payload","value":{"stringValue":"` + secret + `"}},
				{"key":"db.sql.table","value":{"stringValue":"charges"}}
			]}]}]}]}`,
			wantKey: "db.sql.table", wantVal: "charges",
		},
		{
			name: "jaeger keeps http.route and drops db.statement holding a credential",
			src: `{"data":[{"traceID":"0123456789abcdef0123456789abcdef","spans":[
				{"spanID":"1122334455667788","operationName":"q","processID":"p1","tags":[
					{"key":"db.statement","type":"string","value":"SELECT 1 -- ` + secret + `"},
					{"key":"http.url","type":"string","value":"https://api/x?api_key=` + secret + `"},
					{"key":"http.route","type":"string","value":"/v1/charges/{id}"}
				]}],"processes":{"p1":{"serviceName":"svc"}}}]}`,
			wantKey: "http.route", wantVal: "/v1/charges/{id}",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans, _, err := Parse([]byte(tc.src), 0)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want 1", len(spans))
			}
			got := spans[0].Attributes
			if len(got) != 1 {
				t.Fatalf("retained %d attributes (%v), want exactly 1", len(got), got)
			}
			if got[tc.wantKey] != tc.wantVal {
				t.Errorf("%s = %q, want %q", tc.wantKey, got[tc.wantKey], tc.wantVal)
			}
			// The credential must be absent from the whole retained value, not
			// merely from the key it arrived under.
			if strings.Contains(fmt.Sprint(spans[0]), "hunter2") {
				t.Errorf("credential survived parsing: %v", spans[0])
			}
			if _, ok := got["db.statement"]; ok {
				t.Error("db.statement was retained")
			}
			if _, ok := got["http.target"]; ok {
				t.Error("http.target was retained; it carries real path parameters")
			}
		})
	}
}

func TestParseDropsOversizedAttributeValuesRatherThanTruncatingThem(t *testing.T) {
	// A truncated route or file path is a wrong fact dressed as a right one.
	long := strings.Repeat("a", maxAttrValueBytes+1)
	atLimit := strings.Repeat("b", maxAttrValueBytes)
	src := fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"op","attributes":[
		{"key":"http.route","value":{"stringValue":%q}},
		{"key":"code.filepath","value":{"stringValue":%q}}]}]}]}]}`, long, atLimit)

	spans, _, err := Parse([]byte(src), 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := spans[0].Attributes["http.route"]; ok {
		t.Errorf("over-limit value was kept: %q", spans[0].Attributes["http.route"])
	}
	if got := spans[0].Attributes["code.filepath"]; got != atLimit {
		t.Errorf("value exactly at the limit was dropped; len = %d", len(got))
	}
}

func TestHintReadsBothSemanticConventionSpellings(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]string
		want  CodeHint
	}{
		{
			name: "current spellings",
			attrs: map[string]string{
				"code.function.name":  "Handle",
				"code.namespace":      "api.Charges",
				"code.filepath":       "internal/api/charge.go",
				"code.lineno":         "142",
				"http.route":          "/v1/charges/{id}",
				"http.request.method": "POST",
				"db.collection.name":  "charges",
				"db.operation.name":   "insert",
			},
			want: CodeHint{Function: "Handle", Namespace: "api.Charges",
				File: "internal/api/charge.go", Line: 142, Route: "/v1/charges/{id}",
				Method: "POST", DBTable: "charges", DBOperation: "insert"},
		},
		{
			name: "older spellings",
			attrs: map[string]string{
				"code.function": "Handle",
				"http.method":   "get",
				"db.sql.table":  "charges",
				"db.operation":  "select",
			},
			want: CodeHint{Function: "Handle", Method: "GET",
				DBTable: "charges", DBOperation: "select"},
		},
		{
			name:  "a non-numeric line number leaves Line at zero rather than inventing one",
			attrs: map[string]string{"code.filepath": "a.go", "code.lineno": "many"},
			want:  CodeHint{File: "a.go"},
		},
		{
			name:  "no attributes at all",
			attrs: nil,
			want:  CodeHint{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Span{Attributes: tc.attrs}.Hint()
			if got != tc.want {
				t.Errorf("Hint = %+v, want %+v", got, tc.want)
			}
			if got.Empty() != (tc.want == CodeHint{}) {
				t.Errorf("Empty = %v", got.Empty())
			}
		})
	}
}

// ------------------------------------------------------------- percentiles

func TestSummarisePercentilesAreExactAtEverySampleSize(t *testing.T) {
	// Percentiles are nearest-rank over the sorted sample: rank = ceil(p*n).
	// The small-n rows exist because that is where an off-by-one indexes out of
	// range, and the n=100 row is where "p95" must be the 95th value and not
	// the largest one.
	cases := []struct {
		name      string
		durations []int64
		wantP50   int64
		wantP95   int64
	}{
		{name: "n=0 has no observations to report", durations: nil, wantP50: 0, wantP95: 0},
		{name: "n=1 both percentiles are the only sample", durations: []int64{7}, wantP50: 7, wantP95: 7},
		{name: "n=2 p50 is the lower sample", durations: []int64{10, 20}, wantP50: 10, wantP95: 20},
		{name: "n=3 p50 is the middle sample", durations: []int64{30, 10, 20}, wantP50: 20, wantP95: 30},
		{name: "n=100 p95 is the 95th value, not the 100th", durations: rangeDurations(100), wantP50: 50, wantP95: 95},
		{name: "input order does not change the answer", durations: reversed(rangeDurations(100)), wantP50: 50, wantP95: 95},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans := make([]Span, 0, len(tc.durations))
			for _, d := range tc.durations {
				spans = append(spans, Span{
					TraceID: "t", Name: "op", Service: "svc",
					StartNanos: 1000, EndNanos: 1000 + d,
				})
			}
			sum := Summarise(spans)
			if len(tc.durations) == 0 {
				if len(sum.Operations) != 0 {
					t.Fatalf("got %d operations for no spans", len(sum.Operations))
				}
				return
			}
			op := sum.Operations[0]
			if op.Calls != len(tc.durations) {
				t.Errorf("Calls = %d, want %d", op.Calls, len(tc.durations))
			}
			if op.P50Nanos != tc.wantP50 {
				t.Errorf("P50 = %d, want %d", op.P50Nanos, tc.wantP50)
			}
			if op.P95Nanos != tc.wantP95 {
				t.Errorf("P95 = %d, want %d", op.P95Nanos, tc.wantP95)
			}
			if len(tc.durations) >= 4 && op.P95Nanos == maxOf(tc.durations) {
				t.Errorf("P95 = %d is the maximum; a p95 must not be reported as max", op.P95Nanos)
			}
		})
	}
}

func TestSummariseExcludesUnmeasuredSpansFromThePercentileSample(t *testing.T) {
	// A span with no end timestamp has a duration of 0. Admitting it would pull
	// p50 toward a latency the service never achieved, so it counts as a call
	// but not as a measurement.
	spans := []Span{
		{TraceID: "t", Name: "op", StartNanos: 1000, EndNanos: 1100}, // 100
		{TraceID: "t", Name: "op", StartNanos: 1000, EndNanos: 1200}, // 200
		{TraceID: "t", Name: "op", StartNanos: 1000},                 // unmeasured
		{TraceID: "t", Name: "op", StartNanos: 1000, EndNanos: 900},  // skewed clocks
	}
	sum := Summarise(spans)
	op := sum.Operations[0]
	if op.Calls != 4 {
		t.Errorf("Calls = %d, want 4: every span is still a call", op.Calls)
	}
	if op.P50Nanos != 100 || op.P95Nanos != 200 {
		t.Errorf("P50/P95 = %d/%d, want 100/200", op.P50Nanos, op.P95Nanos)
	}
}

func TestSummariseAggregatesTracesServicesErrorsAndOrdersOperations(t *testing.T) {
	spans := []Span{
		{TraceID: "t1", Name: "GET /a", Service: "web", Status: "ok", StartNanos: 1, EndNanos: 11,
			Attributes: map[string]string{"http.route": "/a"}},
		{TraceID: "t1", Name: "GET /a", Service: "web", Status: "error", StartNanos: 1, EndNanos: 21,
			Attributes: map[string]string{"code.function": "GetA"}},
		{TraceID: "t2", Name: "GET /a", Service: "web", Status: "", StartNanos: 1, EndNanos: 31},
		{TraceID: "t2", Name: "GET /a", Service: "api", StartNanos: 1, EndNanos: 41},
		{TraceID: "t2", Name: "SELECT charges", Service: "api", StartNanos: 1, EndNanos: 51},
	}
	sum := Summarise(spans)

	if sum.Traces != 2 {
		t.Errorf("Traces = %d, want 2", sum.Traces)
	}
	if sum.Spans != 5 {
		t.Errorf("Spans = %d, want 5", sum.Spans)
	}
	if got := strings.Join(sum.Services, ","); got != "api,web" {
		t.Errorf("Services = %q, want sorted \"api,web\"", got)
	}
	if sum.Errors != 1 {
		t.Errorf("Errors = %d, want 1", sum.Errors)
	}
	if len(sum.Operations) != 3 {
		t.Fatalf("got %d operations, want 3: (name, service) is the grouping key", len(sum.Operations))
	}
	// Calls descending, then Name, then Service.
	want := []struct{ name, service string }{
		{"GET /a", "web"}, {"GET /a", "api"}, {"SELECT charges", "api"},
	}
	for i, w := range want {
		if sum.Operations[i].Name != w.name || sum.Operations[i].Service != w.service {
			t.Errorf("operation %d = (%q,%q), want (%q,%q)", i,
				sum.Operations[i].Name, sum.Operations[i].Service, w.name, w.service)
		}
	}
	// Hints merge across the group: route came from one span, function another.
	if h := sum.Operations[0].Hint; h.Route != "/a" || h.Function != "GetA" {
		t.Errorf("merged hint = %+v, want Route=/a Function=GetA", h)
	}
	if sum.Operations[0].Errors != 1 {
		t.Errorf("operation Errors = %d, want 1", sum.Operations[0].Errors)
	}
}

// ------------------------------------------------------------- adversarial

func TestParseRejectsAdversarialInputWithoutPanickingOrHanging(t *testing.T) {
	// Every input here is something a hostile or merely broken export can
	// contain. The property under test is uniform: an error or an empty result,
	// never a panic, never an unbounded run.
	deepArrays := strings.Repeat("[", 1_000_000) + strings.Repeat("]", 1_000_000)

	cases := []struct {
		name      string
		src       []byte
		maxBytes  int64
		wantError bool
	}{
		{name: "empty input", src: nil, wantError: true},
		{name: "nul bytes only", src: []byte{0, 0, 0, 0, 0}, wantError: true},
		{name: "truncated otlp document", wantError: true,
			src: []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"op"`)},
		{name: "truncated jaeger document", wantError: true,
			src: []byte(`{"data":[{"traceID":"abc","spans":[{"spanID"`)},
		{name: "otlp marker followed by a million nested arrays", wantError: true,
			src: []byte(`{"resourceSpans":` + deepArrays + `}`)},
		{name: "jaeger marker followed by a million nested arrays", wantError: true,
			src: []byte(`{"traceID":"a","data":` + deepArrays + `}`)},
		{name: "nul bytes inside an otherwise valid otlp string", wantError: false,
			src: []byte("{\"resourceSpans\":[{\"scopeSpans\":[{\"spans\":[{\"name\":\"a\\u0000b\"}]}]}]}")},
		{name: "span fields of entirely the wrong json type", wantError: true,
			src: []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":"not-an-array"}]}]}`)},
		{name: "attribute value is a nested composite where a scalar belongs", wantError: false,
			src: []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"op","attributes":[
				{"key":"http.route","value":{"stringValue":{"nested":["deep"]}}}]}]}]}]}`)},
		{name: "document over the byte limit", maxBytes: 32, wantError: true,
			src: []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"op"}]}]}]}`)},
		{name: "document over the default byte limit", maxBytes: -1, wantError: true,
			src: oversizedDocument()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var err error
			go func() {
				defer close(done)
				// A panic here fails the test rather than killing the process,
				// but the package's own backstop should have caught it first.
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("Parse panicked: %v", r)
					}
				}()
				_, _, err = Parse(tc.src, tc.maxBytes)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("Parse did not return within 30s")
			}
			if tc.wantError && err == nil {
				t.Error("want an error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("want no error, got %v", err)
			}
		})
	}
}

func TestParseNamesTheLimitItRejectedOn(t *testing.T) {
	// A bound that rejects without saying which bound it was leaves the caller
	// unable to tell "your file is too big" from "your file is malformed".
	t.Run("byte limit", func(t *testing.T) {
		_, _, err := Parse([]byte(`{"resourceSpans":[]}`), 4)
		if err == nil || !strings.Contains(err.Error(), "limit is 4 bytes") {
			t.Fatalf("error = %v, want it to name the 4 byte limit", err)
		}
	})
	t.Run("span limit is an error, never a silently shortened list", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"resourceSpans":[{"scopeSpans":[{"spans":[`)
		for i := 0; i <= maxSpans; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{}`)
		}
		b.WriteString(`]}]}]}`)

		spans, _, err := Parse([]byte(b.String()), 0)
		if err == nil {
			t.Fatalf("want an error over %d spans, got %d spans", maxSpans, len(spans))
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("limit is %d spans", maxSpans)) {
			t.Errorf("error = %v, want it to name the span limit", err)
		}
		if spans != nil {
			t.Errorf("got %d spans alongside the error; the list must not be truncated", len(spans))
		}
	})
	t.Run("unknown format", func(t *testing.T) {
		_, format, err := Parse([]byte(`{"hello":"world"}`), 0)
		if err == nil {
			t.Fatal("want an error for an unrecognised document")
		}
		if format != FormatUnknown {
			t.Errorf("format = %q, want %q", format, FormatUnknown)
		}
	})
}

// ------------------------------------------------------------- helpers

func assertSpans(t *testing.T, got, want []Span) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d spans, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.TraceID != w.TraceID || g.SpanID != w.SpanID || g.ParentID != w.ParentID ||
			g.Name != w.Name || g.Service != w.Service || g.Kind != w.Kind ||
			g.StartNanos != w.StartNanos || g.EndNanos != w.EndNanos || g.Status != w.Status {
			t.Errorf("span %d = %+v\nwant %+v", i, g, w)
		}
		if len(g.Attributes) != len(w.Attributes) {
			t.Errorf("span %d attributes = %v, want %v", i, g.Attributes, w.Attributes)
			continue
		}
		for k, v := range w.Attributes {
			if g.Attributes[k] != v {
				t.Errorf("span %d attribute %q = %q, want %q", i, k, g.Attributes[k], v)
			}
		}
	}
}

func rangeDurations(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}

func reversed(in []int64) []int64 {
	out := make([]int64, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

func maxOf(in []int64) int64 {
	m := in[0]
	for _, v := range in {
		if v > m {
			m = v
		}
	}
	return m
}

// oversizedDocument returns a document just over the package default limit, to
// prove the default is applied when the caller passes maxBytes <= 0.
func oversizedDocument() []byte {
	b := make([]byte, defaultMaxBytes+1)
	copy(b, `{"resourceSpans":[]}`)
	return b
}
