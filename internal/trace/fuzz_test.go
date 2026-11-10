package trace

import (
	"sort"
	"testing"
)

// traceSeeds are exports of both formats, plus the shapes real exporters
// produce that a naive struct decoder gets wrong: an int64 written as a string,
// an enum written as a number, a base64 identifier, a composite where a scalar
// was expected, and timestamps at the edge of the range the bounds describe.
var traceSeeds = []string{
	`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"pay"}}]},"scopeSpans":[{"spans":[{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331","name":"GET /x","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2","status":{"code":"STATUS_CODE_ERROR"},"attributes":[{"key":"http.route","value":{"stringValue":"/x"}},{"key":"db.statement","value":{"stringValue":"SELECT secret FROM t"}}]}]}]}]}`,
	`{"resourceSpans":[{"instrumentationLibrarySpans":[{"spans":[{"traceId":"Cvdlod4tQ92ESOshHIAxnA==","spanId":"t61rcWkgMzE=","kind":"SPAN_KIND_SERVER","startTimeUnixNano":1,"endTimeUnixNano":9}]}]}]}`,
	`{"resourceSpans":[{"scopeSpans":[{"spans":[{"spanId":"0000000000000000","kind":{"a":1},"startTimeUnixNano":"x","attributes":[{"key":"code.lineno","value":{"intValue":"42"}}]}]}]}]}`,
	`{"data":[{"traceID":"abc","spans":[{"spanID":"1","operationName":"op","startTime":1,"duration":5,"processID":"p1","tags":[{"key":"span.kind","value":"server"},{"key":"error","value":true},{"key":"http.route","value":"/y"}]}],"processes":{"p1":{"serviceName":"pay"}}}]}`,
	// Boundary timestamps: the microsecond-to-nanosecond multiply is where a
	// Jaeger export turns into a negative instant if the bound is wrong.
	`{"data":[{"traceID":"a","spans":[{"spanID":"1","startTime":9007199254740992,"duration":9007199254740992}]}]}`,
	`{"data":[{"traceID":"a","spans":[{"spanID":"1","startTime":-1,"duration":-1}]}]}`,
	`{"processes":{}}`,
	`{"resourceSpans":`,
	`"resourceSpans"`,
	`{"traceID"`,
	`{}`,
	``,
}

// FuzzParse hunts a decoder that panics, and a span that carries more than the
// allow-list permits.
//
// The document is a file someone downloaded from a tracing UI, so the panic
// backstop in Parse is a real requirement rather than a formality — an escaping
// panic aborts an index run over one downloaded file.
//
// The attribute check is the security half. Spans carry `db.statement` with
// credentials interpolated into the SQL and `http.url` with tokens in the query
// string, and whatever this package retains is persisted and served back to an
// agent. The allow-list is the boundary, and a key outside it in the returned
// map means the boundary moved.
func FuzzParse(f *testing.F) {
	for _, s := range traceSeeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		spans, format, err := Parse([]byte(src), 0)

		// Detect must agree with itself: Parse dispatches on it, so a
		// non-deterministic sniff would route the same document to two
		// different decoders.
		// Two separate calls, held in variables — staticcheck reads
		// `f(x) != f(x)` as a tautology (SA4000) and cannot know the point is
		// that the two calls must AGREE.
		first := Detect([]byte(src))
		second := Detect([]byte(src))
		if first != second {
			t.Fatalf("Detect is not deterministic on %q: %q then %q", src, first, second)
		}
		switch format {
		case FormatOTLPJSON, FormatJaeger, FormatUnknown:
		default:
			t.Fatalf("format %q is not one of the declared constants", format)
		}
		if err != nil {
			// A rejected document must yield nothing at all. A caller that
			// logs the error and carries on must not find a partial span set.
			if spans != nil {
				t.Fatalf("error %v alongside %d spans", err, len(spans))
			}
			return
		}
		if format == FormatUnknown {
			t.Fatalf("no error but no format, from %q", src)
		}
		// The bound is never applied by silent truncation; over the limit is an
		// error, so a successful parse is always under it.
		if len(spans) > maxSpans {
			t.Fatalf("%d spans, over the %d limit", len(spans), maxSpans)
		}

		for _, s := range spans {
			for k, v := range s.Attributes {
				if _, ok := allowedAttributes[k]; !ok {
					t.Fatalf("attribute %q survived the allow-list, from %q", k, src)
				}
				if v == "" {
					t.Fatalf("attribute %q retained with an empty value", k)
				}
				if len(v) > maxAttrValueBytes {
					t.Fatalf("attribute %q is %d bytes, over the %d limit", k, len(v), maxAttrValueBytes)
				}
			}
			for _, id := range []string{s.TraceID, s.SpanID, s.ParentID} {
				if len(id) > maxIDChars {
					t.Fatalf("identifier of %d chars, over the %d limit, from %q", len(id), maxIDChars, src)
				}
			}
			// A negative duration drags a percentile below zero, which is a
			// latency nobody can act on.
			if s.DurationNanos() < 0 {
				t.Fatalf("negative duration from start=%d end=%d", s.StartNanos, s.EndNanos)
			}
			// Jaeger's end instant is COMPUTED here (start + duration), not
			// read, so unlike OTLP's two independently stamped timestamps it
			// cannot legitimately precede the start. When it does, the
			// microsecond multiply overflowed.
			if format == FormatJaeger && s.EndNanos != 0 && s.EndNanos < s.StartNanos {
				t.Fatalf("computed end %d precedes start %d, from %q", s.EndNanos, s.StartNanos, src)
			}
			if s.Kind != "" && !knownKind(s.Kind) {
				t.Fatalf("kind %q is not one of the documented values", s.Kind)
			}
			if s.Status != "" && s.Status != "ok" && s.Status != "error" {
				t.Fatalf("status %q is neither ok nor error", s.Status)
			}
			if h := s.Hint(); h.Line < 0 {
				t.Fatalf("negative line %d in hint", h.Line)
			}
		}

		sum := Summarise(spans)
		if sum.Spans != len(spans) {
			t.Fatalf("summary counts %d spans against %d", sum.Spans, len(spans))
		}
		if !sort.StringsAreSorted(sum.Services) {
			t.Fatalf("services are not sorted: %q", sum.Services)
		}
		calls := 0
		for _, op := range sum.Operations {
			// The rank of p50 is never above the rank of p95 over the same
			// sample, so a p50 above p95 means the two were computed from
			// different samples — which is how a latency report starts
			// disagreeing with itself.
			if op.P50Nanos > op.P95Nanos {
				t.Fatalf("p50 %d above p95 %d for %q", op.P50Nanos, op.P95Nanos, op.Name)
			}
			if op.Calls <= 0 {
				t.Fatalf("operation %q with %d calls", op.Name, op.Calls)
			}
			if op.Errors > op.Calls {
				t.Fatalf("operation %q has %d errors in %d calls", op.Name, op.Errors, op.Calls)
			}
			calls += op.Calls
		}
		if calls != len(spans) {
			t.Fatalf("operations account for %d calls against %d spans", calls, len(spans))
		}
	})
}

func knownKind(k string) bool {
	switch k {
	case "server", "client", "internal", "producer", "consumer":
		return true
	}
	return false
}

// FuzzDetect hunts a sniff that reads past its own bound.
//
// Detect must stay O(1) on a 64 MB document, so it looks at a prefix only. That
// makes it the one function here whose answer must not depend on anything after
// detectPrefixBytes — if it did, the bound would be a lie and the cost of
// sniffing would scale with the file.
func FuzzDetect(f *testing.F) {
	for _, s := range traceSeeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		got := Detect([]byte(src))
		switch got {
		case FormatOTLPJSON, FormatJaeger, FormatUnknown:
		default:
			t.Fatalf("format %q is not one of the declared constants", got)
		}
		if len(src) <= detectPrefixBytes {
			// Appending past the prefix cannot change the answer, because the
			// answer was decided inside it.
			padded := src + string(make([]byte, detectPrefixBytes))
			if again := Detect([]byte(padded)); again != got {
				t.Fatalf("format changed from %q to %q on bytes past the prefix", got, again)
			}
		}
	})
}
