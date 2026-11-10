// Package trace reads distributed-tracing exports and reduces them to the few
// facts RepoCortex can line up against the static graph: which endpoints were
// really called, which functions really ran, which tables were really touched,
// and how long each took.
//
// # What this is, and what it deliberately is not
//
// It is a READER, not a tracing system.
//
// It does not sample. Sampling is a decision the producer already made, before
// these bytes existed; re-sampling here would silently change the denominator
// under every count this package reports, and a call rate computed from a
// sample the caller did not choose is worse than no call rate.
//
// It does not talk to a collector, an agent, a Jaeger query API or any other
// network endpoint. The only input is a byte slice the caller already holds.
// That boundary is what makes the whole package testable against raw exporter
// output, and it is why nothing here can leak a repository's trace data
// outward.
//
// It does not correlate. Nothing in this file knows what a symbol, an endpoint
// or a table row in the index looks like, and it never will. A [CodeHint] is
// the raw material for correlation — a function name, a route template, a file
// and line — handed back verbatim for the caller to match against the graph.
// Keeping the matching rules out of here is deliberate: those rules differ per
// language and per instrumentation library, they change often, and burying them
// in a parser would make both halves untestable.
//
// It does not retain the export. Spans carry arbitrary user attributes — SQL
// text with embedded credentials, URLs with bearer tokens, request bodies. Only
// a small allow-list of keys survives parsing; see [allowedAttributes].
package trace

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Span is one operation in a distributed trace, reduced to what RepoCortex can
// correlate with code.
type Span struct {
	TraceID    string
	SpanID     string
	ParentID   string
	Name       string
	Service    string
	Kind       string // server | client | internal | producer | consumer | ""
	StartNanos int64
	EndNanos   int64
	// Status is "ok" | "error" | "" (unset).
	Status string
	// Attributes holds only the KEYS RepoCortex correlates on; see CodeHint.
	Attributes map[string]string
}

// DurationNanos is End-Start, or 0 when either is missing.
//
// A negative difference is also reported as 0 rather than as a negative
// duration. It happens for real — a span whose start and end were stamped on
// two machines with skewed clocks — and letting it through would drag a
// percentile below zero, which is a latency no reader can act on.
func (s Span) DurationNanos() int64 {
	if s.StartNanos <= 0 || s.EndNanos <= 0 || s.EndNanos < s.StartNanos {
		return 0
	}
	return s.EndNanos - s.StartNanos
}

// CodeHint is what this span says about the code that produced it.
type CodeHint struct {
	// Function is from code.function / code.function.name.
	Function string
	// Namespace is from code.namespace (a class or package).
	Namespace string
	// File and Line are from code.filepath / code.lineno.
	File string
	Line int
	// Route is the HTTP route template from http.route (NOT http.target,
	// which carries real path parameters and would explode the cardinality).
	Route  string
	Method string // http.method / http.request.method
	// DBTable is from db.sql.table / db.collection.name.
	DBTable string
	// DBOperation is from db.operation / db.operation.name.
	DBOperation string
}

// Empty reports whether the hint says nothing about code. Callers use it to
// skip operations that cannot be matched to the graph at all, rather than
// reporting them as "unmatched code".
func (h CodeHint) Empty() bool {
	return h == CodeHint{}
}

// Attribute keys, spelled once so the allow-list and the extractor cannot drift
// apart. Both the current OpenTelemetry semantic-convention name and the older
// one are listed where the convention was renamed: exports in the wild are
// produced by whichever SDK version a service happens to run, and dropping the
// old spelling would make every span from a year-old service look like it
// carries no code information.
const (
	attrCodeFunction    = "code.function"
	attrCodeFunctionNew = "code.function.name"
	attrCodeNamespace   = "code.namespace"
	attrCodeFilepath    = "code.filepath"
	attrCodeLineno      = "code.lineno"
	attrHTTPRoute       = "http.route"
	attrHTTPMethod      = "http.method"
	attrHTTPMethodNew   = "http.request.method"
	attrDBSQLTable      = "db.sql.table"
	attrDBCollection    = "db.collection.name"
	attrDBOperation     = "db.operation"
	attrDBOperationNew  = "db.operation.name"
)

// allowedAttributes is the complete set of attribute keys this package will
// hold in memory. Everything else is dropped at parse time and never stored.
//
// THIS IS A SECURITY BOUNDARY, not a memory optimisation, though it is both.
//
// A span's attributes are whatever the instrumented service chose to attach.
// In practice that routinely includes `db.statement` with the full SQL and, in
// badly configured services, credentials interpolated into it; `http.url` and
// `http.target` with session tokens and API keys in the query string; and
// `messaging.message.payload` with an entire request body. RepoCortex persists
// what it parses and serves it back to an agent, so anything retained here
// leaves this process. A deny-list cannot hold that line — the next SDK release
// adds a key nobody deny-listed — so the rule is inverted: a key is dropped
// unless it appears below, and each entry below is here because a CodeHint
// field needs it.
//
// The secondary effect matters too. A single trace export can carry millions of
// attributes; keeping twelve keys instead of all of them is the difference
// between a bounded map per span and holding the whole document resident.
var allowedAttributes = map[string]struct{}{
	attrCodeFunction:    {},
	attrCodeFunctionNew: {},
	attrCodeNamespace:   {},
	attrCodeFilepath:    {},
	attrCodeLineno:      {},
	attrHTTPRoute:       {},
	attrHTTPMethod:      {},
	attrHTTPMethodNew:   {},
	attrDBSQLTable:      {},
	attrDBCollection:    {},
	attrDBOperation:     {},
	attrDBOperationNew:  {},
}

// Hint extracts the correlation hints from a span's attributes.
func (s Span) Hint() CodeHint {
	h := CodeHint{
		Function:    firstNonEmpty(s.Attributes[attrCodeFunction], s.Attributes[attrCodeFunctionNew]),
		Namespace:   s.Attributes[attrCodeNamespace],
		File:        s.Attributes[attrCodeFilepath],
		Route:       s.Attributes[attrHTTPRoute],
		Method:      firstNonEmpty(s.Attributes[attrHTTPMethod], s.Attributes[attrHTTPMethodNew]),
		DBTable:     firstNonEmpty(s.Attributes[attrDBSQLTable], s.Attributes[attrDBCollection]),
		DBOperation: firstNonEmpty(s.Attributes[attrDBOperation], s.Attributes[attrDBOperationNew]),
	}
	// code.lineno is an integer attribute, but OTLP/JSON renders int64 as a
	// STRING (protobuf's JSON mapping does this to survive JavaScript's 53-bit
	// numbers), so it arrives here as text either way. A value that will not
	// parse leaves Line at 0 rather than failing the span: a file with no line
	// still locates code, and a made-up line number does not.
	if n, err := strconv.Atoi(strings.TrimSpace(s.Attributes[attrCodeLineno])); err == nil && n > 0 {
		h.Line = n
	}
	// An uppercased method is the only normalisation applied to any hint.
	// `get` and `GET` are the same endpoint, and leaving both would split every
	// route in the report in two.
	h.Method = strings.ToUpper(h.Method)
	return h
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Format identifies which exporter produced a file.
type Format string

const (
	FormatOTLPJSON Format = "otlp-json"
	FormatJaeger   Format = "jaeger"
	FormatUnknown  Format = ""
)

// Bounds. Every one of these exists because the input is a file someone
// downloaded from a tracing UI, and the failure mode of an unbounded parser fed
// such a file is an OOM kill that takes the whole indexer with it.
const (
	// defaultMaxBytes bounds one document. 64 MB is roughly an hour of a busy
	// service at full trace detail — far more than anyone correlates by hand,
	// and small enough that decoding it cannot exhaust a container's memory.
	defaultMaxBytes int64 = 64 << 20

	// maxSpans bounds the returned slice. The byte limit alone does not: a
	// document of minimal `{}` spans stays under 64 MB while producing tens of
	// millions of Span values, each with a map header. Half a million spans is
	// already more than any report renders.
	maxSpans = 500_000

	// maxAttrValueBytes bounds one retained attribute value. Every allow-listed
	// key holds a route, a symbol, a file path or a table name; none of those is
	// half a kilobyte. A longer value is not the thing the key claims to be, so
	// it is DROPPED rather than truncated — a truncated file path or route is a
	// wrong fact presented as a right one, and this index is read by an agent
	// that cannot tell the difference.
	maxAttrValueBytes = 512

	// detectPrefixBytes bounds format sniffing. Detect must stay O(1) on a
	// 64 MB document; both formats put their discriminating key in the first
	// object, so scanning further would only cost time.
	detectPrefixBytes = 64 << 10

	// maxIDChars bounds an identifier this package could not decode but keeps
	// verbatim so that parent/child links still join. Without the bound a
	// crafted export supplies a megabyte "span id" per span.
	maxIDChars = 64

	// maxUnixMicros rejects a Jaeger timestamp that cannot be a real one before
	// the microsecond-to-nanosecond multiply overflows int64 and produces a
	// negative instant. 2^53 microseconds is the year 287396.
	maxUnixMicros = int64(1) << 53
)

// Detect sniffs the format from the first bytes of a document.
func Detect(src []byte) Format {
	head := src
	if len(head) > detectPrefixBytes {
		head = head[:detectPrefixBytes]
	}
	s := string(head)

	// OTLP is checked first because its key is unambiguous, and because a
	// document can legitimately contain both spellings when it was produced by
	// a collector bridging SDK versions.
	if strings.Contains(s, `"resourceSpans"`) || strings.Contains(s, `"instrumentationLibrarySpans"`) {
		return FormatOTLPJSON
	}
	// Jaeger's UI export capitalises the D in traceID/spanID where OTLP writes
	// traceId/spanId. That single letter is the most reliable discriminator
	// between the two, and it is case-sensitive on purpose.
	if strings.Contains(s, `"traceID"`) || strings.Contains(s, `"processes"`) {
		return FormatJaeger
	}
	return FormatUnknown
}

// Parse reads spans from an OTLP/JSON or Jaeger-UI JSON export.
// Returns the format it used. maxBytes <= 0 means the package default.
//
// Every bound is reported as an error naming the limit. The span list is never
// silently truncated: a caller that asked "which endpoints are never called"
// and got a quietly shortened span set would conclude that live endpoints are
// dead, which is the single most damaging wrong answer this package could give.
func Parse(src []byte, maxBytes int64) (spans []Span, format Format, err error) {
	// A tracing export is third-party bytes of unknown provenance, and this
	// function sits in the indexer's critical path. The decoders below are
	// written not to panic and the tests exercise them adversarially, but a
	// panic escaping into the pipeline would abort an entire index run over one
	// bad file. This backstop converts that into a rejected document.
	defer func() {
		if r := recover(); r != nil {
			spans, format, err = nil, FormatUnknown, fmt.Errorf("trace: rejected malformed export: %v", r)
		}
	}()

	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if int64(len(src)) > maxBytes {
		return nil, FormatUnknown, fmt.Errorf("trace: document is %d bytes, limit is %d bytes", len(src), maxBytes)
	}

	format = Detect(src)
	switch format {
	case FormatOTLPJSON:
		spans, err = parseOTLP(src)
	case FormatJaeger:
		spans, err = parseJaeger(src)
	default:
		return nil, FormatUnknown, fmt.Errorf("trace: unrecognised export format: expected OTLP/JSON (resourceSpans) or Jaeger UI JSON (data/traceID)")
	}
	if err != nil {
		return nil, format, err
	}
	return spans, format, nil
}

// ---------------------------------------------------------------- OTLP/JSON

// The OTLP/JSON shapes below use json.RawMessage for every leaf rather than a
// concrete type, because protobuf's JSON mapping makes the wire type of a leaf
// unpredictable: an enum is a name OR a number, an int64 is a string OR a
// number, and which one a document uses depends on whether it was written by
// the OTLP exporter, by protojson, or by hand. Decoding into `string` or
// `int64` fails the WHOLE document on the first leaf of the other flavour, and
// that — timestamps arriving as strings against a struct expecting numbers — is
// the single most common real-world OTLP/JSON parsing failure.
type otlpDocument struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
	// The pre-1.0 spelling. Collectors still emit it, and a service pinned to
	// an old SDK produces nothing else; refusing it would make those exports
	// parse to zero spans with no error, which looks exactly like a service
	// that received no traffic.
	InstrumentationLibrarySpans []otlpScopeSpans `json:"instrumentationLibrarySpans"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScopeSpans struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId"`
	Name              string          `json:"name"`
	Kind              json.RawMessage `json:"kind"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
	Status            otlpStatus      `json:"status"`
	Attributes        []otlpAttribute `json:"attributes"`
}

type otlpStatus struct {
	Code json.RawMessage `json:"code"`
}

type otlpAttribute struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue json.RawMessage `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	BoolValue   json.RawMessage `json:"boolValue"`
	DoubleValue json.RawMessage `json:"doubleValue"`
}

// text renders whichever variant of an AnyValue was populated. Array and kvlist
// values return "" — no allow-listed key is ever a composite, so a composite
// under one of those keys is a producer bug and flattening it would invent a
// route or a table name out of a data structure.
func (v otlpAnyValue) text() string {
	return firstNonEmpty(
		scalarText(v.StringValue),
		scalarText(v.IntValue),
		scalarText(v.BoolValue),
		scalarText(v.DoubleValue),
	)
}

func parseOTLP(src []byte) ([]Span, error) {
	var doc otlpDocument
	if err := json.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("trace: decoding OTLP/JSON: %w", err)
	}

	var out []Span
	for _, rs := range doc.ResourceSpans {
		service := attributeValue(rs.Resource.Attributes, "service.name")
		// Both spellings are walked rather than one-or-the-other: a collector
		// merging streams from two SDK generations puts each under its own key
		// inside the same resourceSpans entry.
		for _, groups := range [][]otlpScopeSpans{rs.ScopeSpans, rs.InstrumentationLibrarySpans} {
			for _, ss := range groups {
				for _, s := range ss.Spans {
					if len(out) >= maxSpans {
						return nil, fmt.Errorf("trace: export contains more than %d spans, limit is %d spans", maxSpans, maxSpans)
					}
					out = append(out, Span{
						TraceID:    decodeID(s.TraceID),
						SpanID:     decodeID(s.SpanID),
						ParentID:   decodeID(s.ParentSpanID),
						Name:       s.Name,
						Service:    service,
						Kind:       otlpKind(scalarText(s.Kind)),
						StartNanos: parseInt64(scalarText(s.StartTimeUnixNano)),
						EndNanos:   parseInt64(scalarText(s.EndTimeUnixNano)),
						Status:     otlpStatusText(scalarText(s.Status.Code)),
						Attributes: filterAttributes(s.Attributes),
					})
				}
			}
		}
	}
	return out, nil
}

func attributeValue(attrs []otlpAttribute, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return clampValue(a.Value.text())
		}
	}
	return ""
}

// filterAttributes applies the allow-list. It returns nil rather than an empty
// map when nothing survived, so a span carrying only user data costs one nil
// pointer instead of a map header — which, multiplied by half a million spans,
// is the difference the bound above exists to protect.
func filterAttributes(attrs []otlpAttribute) map[string]string {
	var out map[string]string
	for _, a := range attrs {
		if _, ok := allowedAttributes[a.Key]; !ok {
			continue
		}
		v := clampValue(a.Value.text())
		if v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, 4)
		}
		out[a.Key] = v
	}
	return out
}

// otlpKind maps the SpanKind enum to the lowercase names Span.Kind uses.
// Accepts both the enum NAME and its NUMBER: protojson writes the name,
// hand-rolled exporters and some collectors write the number.
func otlpKind(v string) string {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "1", "SPAN_KIND_INTERNAL", "INTERNAL":
		return "internal"
	case "2", "SPAN_KIND_SERVER", "SERVER":
		return "server"
	case "3", "SPAN_KIND_CLIENT", "CLIENT":
		return "client"
	case "4", "SPAN_KIND_PRODUCER", "PRODUCER":
		return "producer"
	case "5", "SPAN_KIND_CONSUMER", "CONSUMER":
		return "consumer"
	}
	// SPAN_KIND_UNSPECIFIED, and anything a future OTLP version adds. Reported
	// as unset rather than guessed, because Kind decides whether a span is an
	// inbound endpoint or an outbound call, and guessing that backwards inverts
	// the direction of every edge a caller derives from it.
	return ""
}

// otlpStatusText maps the Status enum. UNSET is not "ok": a span with no status
// was never judged, and counting it as a success would understate the error
// rate of every service whose instrumentation does not set status.
func otlpStatusText(v string) string {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "1", "STATUS_CODE_OK", "OK":
		return "ok"
	case "2", "STATUS_CODE_ERROR", "ERROR":
		return "error"
	}
	return ""
}

// ---------------------------------------------------------------- Jaeger UI

type jaegerDocument struct {
	Data []jaegerTrace `json:"data"`
}

type jaegerTrace struct {
	TraceID   string                   `json:"traceID"`
	Spans     []jaegerSpan             `json:"spans"`
	Processes map[string]jaegerProcess `json:"processes"`
}

type jaegerProcess struct {
	ServiceName string `json:"serviceName"`
}

type jaegerSpan struct {
	TraceID       string            `json:"traceID"`
	SpanID        string            `json:"spanID"`
	ParentSpanID  string            `json:"parentSpanID"`
	OperationName string            `json:"operationName"`
	References    []jaegerReference `json:"references"`
	// StartTime and Duration are MICROSECONDS. This is the one unit difference
	// between the two formats and the only place a silent factor of 1000 can
	// enter the numbers.
	StartTime json.RawMessage `json:"startTime"`
	Duration  json.RawMessage `json:"duration"`
	Tags      []jaegerTag     `json:"tags"`
	ProcessID string          `json:"processID"`
}

type jaegerReference struct {
	RefType string `json:"refType"`
	SpanID  string `json:"spanID"`
}

type jaegerTag struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func parseJaeger(src []byte) ([]Span, error) {
	var doc jaegerDocument
	if err := json.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("trace: decoding Jaeger JSON: %w", err)
	}

	var out []Span
	for _, tr := range doc.Data {
		for _, s := range tr.Spans {
			if len(out) >= maxSpans {
				return nil, fmt.Errorf("trace: export contains more than %d spans, limit is %d spans", maxSpans, maxSpans)
			}
			startMicros := parseInt64(scalarText(s.StartTime))
			durMicros := parseInt64(scalarText(s.Duration))

			span := Span{
				// A span's own traceID wins over the enclosing trace's, but the
				// enclosing one is the fallback: some exporters omit it per
				// span, and a span with no trace id cannot be joined to its
				// siblings at all.
				TraceID:    firstNonEmpty(decodeID(s.TraceID), decodeID(tr.TraceID)),
				SpanID:     decodeID(s.SpanID),
				ParentID:   jaegerParent(s),
				Name:       s.OperationName,
				Service:    tr.Processes[s.ProcessID].ServiceName,
				Attributes: filterJaegerTags(s.Tags),
			}
			span.StartNanos, span.EndNanos = microsToNanos(startMicros, durMicros)
			// Jaeger has no kind or status field; both live in tags, which the
			// allow-list drops. Reading a tag and storing a tag are separate
			// things: these two are read here, mapped into typed fields, and
			// the tag itself is never retained.
			span.Kind = jaegerKind(s.Tags)
			span.Status = jaegerStatus(s.Tags)
			out = append(out, span)
		}
	}
	return out, nil
}

// microsToNanos converts Jaeger's microsecond start/duration into the absolute
// nanosecond start/end the rest of the package uses.
//
// Out-of-range inputs collapse to 0/0 rather than wrapping: 2^53 microseconds
// multiplied by 1000 overflows int64 into a negative instant, and a negative
// instant produces a negative duration that poisons every percentile it lands
// in.
func microsToNanos(startMicros, durMicros int64) (start, end int64) {
	if startMicros <= 0 || startMicros > maxUnixMicros {
		return 0, 0
	}
	start = startMicros * 1000
	if durMicros < 0 || durMicros > maxUnixMicros {
		// A start with no usable duration is still a usable timestamp; only the
		// end is unknown, and DurationNanos reports 0 for it.
		return start, 0
	}
	end = start + durMicros*1000
	if end < start {
		// The SUM overflowed, which the per-field bounds above do not prevent:
		// each of a start and a duration at the top of the permitted range fits
		// in int64 on its own, and together they wrap into a negative instant.
		// An export carrying both really does reach here, and the result was a
		// span whose end lay before the Unix epoch — DurationNanos then read 0
		// and the span silently left every percentile, looking like a span
		// nobody timed rather than like the bad timestamp it is.
		return start, 0
	}
	return start, end
}

// jaegerParent resolves the parent span.
//
// CHILD_OF is preferred over FOLLOWS_FROM because only CHILD_OF means "this ran
// inside that", which is the relationship a call graph needs. The legacy
// parentSpanID field is the last resort: Jaeger deprecated it in favour of
// references, but exports from older backends still carry only that.
func jaegerParent(s jaegerSpan) string {
	for _, r := range s.References {
		if strings.EqualFold(r.RefType, "CHILD_OF") {
			return decodeID(r.SpanID)
		}
	}
	for _, r := range s.References {
		if r.SpanID != "" {
			return decodeID(r.SpanID)
		}
	}
	return decodeID(s.ParentSpanID)
}

func filterJaegerTags(tags []jaegerTag) map[string]string {
	var out map[string]string
	for _, t := range tags {
		if _, ok := allowedAttributes[t.Key]; !ok {
			continue
		}
		v := clampValue(scalarText(t.Value))
		if v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, 4)
		}
		out[t.Key] = v
	}
	return out
}

func jaegerTagValue(tags []jaegerTag, key string) string {
	for _, t := range tags {
		if t.Key == key {
			return clampValue(scalarText(t.Value))
		}
	}
	return ""
}

func jaegerKind(tags []jaegerTag) string {
	switch strings.ToLower(jaegerTagValue(tags, "span.kind")) {
	case "server":
		return "server"
	case "client":
		return "client"
	case "producer":
		return "producer"
	case "consumer":
		return "consumer"
	case "internal", "local":
		return "internal"
	}
	return ""
}

// jaegerStatus reads the two ways a Jaeger span records failure. The OTel
// bridge tag is checked first because it is the more specific statement: it can
// say OK, whereas `error=true` can only ever say error.
func jaegerStatus(tags []jaegerTag) string {
	switch strings.ToUpper(jaegerTagValue(tags, "otel.status_code")) {
	case "ERROR", "STATUS_CODE_ERROR", "2":
		return "error"
	case "OK", "STATUS_CODE_OK", "1":
		return "ok"
	}
	if strings.EqualFold(jaegerTagValue(tags, "error"), "true") {
		return "error"
	}
	return ""
}

// ---------------------------------------------------------------- decoding

// scalarText renders a JSON leaf as text without committing to its type.
//
// Numbers are returned as their ORIGINAL source text rather than round-tripped
// through float64. A Jaeger duration or an OTLP nanosecond timestamp exceeds
// float64's 53 bits of integer precision, and a round trip quietly shifts such
// a value by hundreds of nanoseconds.
func scalarText(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return ""
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	case '{', '[':
		// A composite where a scalar was expected. Never flattened: see
		// otlpAnyValue.text.
		return ""
	}
	return t
}

func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		// A timestamp that will not parse leaves the field at 0 and the span in
		// the result. Dropping the span instead would remove a call that really
		// happened from a "never called" report over a formatting problem.
		return 0
	}
	return n
}

// clampValue applies maxAttrValueBytes. Over-long values are dropped, not cut;
// see the constant.
func clampValue(v string) string {
	if len(v) > maxAttrValueBytes {
		return ""
	}
	return v
}

// decodeID normalises a trace or span identifier to lowercase hex.
//
// OTLP/JSON specifies hex, but protobuf's JSON mapping encodes a `bytes` field
// as BASE64, so any document produced by protojson — which includes most
// collector exports — carries base64 instead. Both must produce the same string
// or the same trace splits into two.
//
// Length is what disambiguates them, and it does so cleanly: a hex trace id is
// 32 characters and a hex span id is 16, while their base64 forms are 24 and 12
// (22 and 11 unpadded). No length is shared, so a string of exactly 16 or 32
// hex digits is hex and nothing else needs to be guessed.
func decodeID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if isHexID(s) {
		return blankIfZero(strings.ToLower(s))
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		b, err := enc.DecodeString(s)
		if err != nil || (len(b) != 8 && len(b) != 16) {
			continue
		}
		return blankIfZero(hex.EncodeToString(b))
	}
	// Neither spelling. Kept verbatim rather than discarded so that parent and
	// child still join on the same string — a wrongly-formatted id is still a
	// consistent one within a single export. Bounded, because otherwise a
	// crafted document supplies a megabyte per identifier.
	if len(s) > maxIDChars {
		return ""
	}
	return blankIfZero(strings.ToLower(s))
}

func isHexID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// blankIfZero maps the all-zero identifier to the empty string. OTLP spells "no
// parent" as sixteen zero bytes, not as an absent field; leaving it as
// "0000000000000000" would make every root span appear to be the child of one
// imaginary span, joining unrelated traces into a single tree.
func blankIfZero(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- summary

// Summary aggregates spans for reporting.
type Summary struct {
	Traces     int
	Spans      int
	Services   []string // sorted
	Errors     int
	Operations []Operation // sorted by Calls descending, then Name
}

// Operation is one span name aggregated across traces.
type Operation struct {
	Name    string
	Service string
	Calls   int
	Errors  int
	// P50Nanos and P95Nanos are computed from the observed durations. They are
	// exact percentiles over the spans present, not an estimate.
	P50Nanos int64
	P95Nanos int64
	Hint     CodeHint
}

// Summarise aggregates a span set.
//
// Grouping is by (Name, Service), not by Name alone. Two services routinely
// name an operation `GET /health`, and merging them would report one latency
// distribution for two unrelated pieces of code.
func Summarise(spans []Span) Summary {
	sum := Summary{Spans: len(spans)}

	traces := make(map[string]struct{})
	services := make(map[string]struct{})
	type bucket struct {
		op        Operation
		durations []int64
	}
	buckets := make(map[string]*bucket)
	var order []string

	for _, s := range spans {
		if s.TraceID != "" {
			traces[s.TraceID] = struct{}{}
		}
		if s.Service != "" {
			services[s.Service] = struct{}{}
		}
		if s.Status == "error" {
			sum.Errors++
		}

		key := s.Name + "\x00" + s.Service
		b := buckets[key]
		if b == nil {
			b = &bucket{op: Operation{Name: s.Name, Service: s.Service}}
			buckets[key] = b
			order = append(order, key)
		}
		b.op.Calls++
		if s.Status == "error" {
			b.op.Errors++
		}
		if d := s.DurationNanos(); d > 0 {
			// Only measured durations enter the sample. A span whose end
			// timestamp was missing has a duration of 0, and admitting those
			// zeros would pull the reported p50 toward a latency the system
			// never achieved.
			b.durations = append(b.durations, d)
		}
		// Hints are merged across the group rather than taken from the first
		// span. Instrumentation commonly attaches code.* on the span that
		// entered a function and http.route on the one that handled the
		// request, even when both share a name; taking only the first would
		// throw away half of what the export said about the same operation.
		mergeHint(&b.op.Hint, s.Hint())
	}

	sum.Traces = len(traces)
	sum.Services = sortedKeys(services)

	sum.Operations = make([]Operation, 0, len(order))
	for _, key := range order {
		b := buckets[key]
		sort.Slice(b.durations, func(i, j int) bool { return b.durations[i] < b.durations[j] })
		b.op.P50Nanos = percentile(b.durations, 0.50)
		b.op.P95Nanos = percentile(b.durations, 0.95)
		sum.Operations = append(sum.Operations, b.op)
	}
	sort.SliceStable(sum.Operations, func(i, j int) bool {
		a, b := sum.Operations[i], sum.Operations[j]
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		// Service is the final tiebreak. Not required by the ordering the
		// caller asked for, but without it two services sharing an operation
		// name come back in map-iteration order, and a report that reorders
		// itself between identical runs cannot be diffed.
		return a.Service < b.Service
	})
	return sum
}

// mergeHint fills empty fields of dst from src, never overwriting. First
// non-empty wins so the result is independent of how many later spans carried
// nothing.
func mergeHint(dst *CodeHint, src CodeHint) {
	if dst.Function == "" {
		dst.Function = src.Function
	}
	if dst.Namespace == "" {
		dst.Namespace = src.Namespace
	}
	if dst.File == "" {
		dst.File = src.File
		// Line travels with File. A line number from one span pinned to a file
		// path from another points at an arbitrary line of an unrelated file.
		dst.Line = src.Line
	}
	if dst.Route == "" {
		dst.Route = src.Route
	}
	if dst.Method == "" {
		dst.Method = src.Method
	}
	if dst.DBTable == "" {
		dst.DBTable = src.DBTable
	}
	if dst.DBOperation == "" {
		dst.DBOperation = src.DBOperation
	}
}

// percentile returns the exact p-th percentile of an ALREADY SORTED sample,
// using the nearest-rank definition: rank = ceil(p*n), value = sorted[rank-1].
//
// Exact, not estimated: there is no histogram, no bucketing and no
// interpolation, so the value returned is always one that was really observed.
//
// The rank is clamped into [1,n], which is what keeps n=1 and n=2 from indexing
// out of range — the arithmetic alone yields rank 1 for n=1 and rank 2 for n=2,
// and the clamp guards the boundary rather than papering over it. Note that for
// n<=3 the p95 rank legitimately lands on the largest sample; that is what the
// definition says about three data points, and it is not the same thing as
// reporting max in place of p95, which this never does — at n=100 the answer is
// the 95th value, not the 100th.
func percentile(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
