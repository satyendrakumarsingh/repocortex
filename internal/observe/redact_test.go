package observe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// badAttr is the mistake this whole file exists to prevent.
func badAttr() attribute.KeyValue {
	return attribute.String("file_path", "/Users/alice/work/acme-billing/internal/auth/token.go")
}

// TestRejectsSensitiveAttributes is the redaction proof. Every case is
// something a well-meaning contributor might plausibly attach.
func TestRejectsSensitiveAttributes(t *testing.T) {
	cases := []struct {
		name string
		attr attribute.KeyValue
	}{
		{"absolute path", attribute.String("file_path", "/Users/alice/repo/main.go")},
		{"relative path", attribute.String("target", "internal/store/sqlite.go")},
		{"windows path", attribute.String("target", `C:\Users\alice\repo`)},
		{"home path", attribute.String("target", "~/work/secret-project")},
		{"parent path", attribute.String("target", "../../etc/shadow")},
		{"key names a path", attribute.String("srcPath", "go")},
		{"key names source", attribute.String("source_content", "go")},
		{"key names a symbol", attribute.String("symbol", "updateStatus")},
		{"key names a query", attribute.String("query", "auth")},
		{"repo name", attribute.String("repo", "acme-billing")},
		{"github token", attribute.String("kind", "ghp_0123456789abcdefghijklmnopqrstuvwxyz")},
		{"aws key", attribute.String("kind", "AKIAIOSFODNN7EXAMPLE")},
		{"jwt", attribute.String("kind", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9")},
		{"pem block", attribute.String("kind", "-----BEGIN RSA PRIVATE KEY")},
		{"bearer header", attribute.String("kind", "Bearer abc123")},
		{"sha256 digest", attribute.String("kind", "a3f5c2e1b8d94f7a0c6e2b1d8f4a9c3e")},
		{"email", attribute.String("kind", "alice@example.com")},
		{"source snippet", attribute.String("kind", "func main() { return }")},
		{"free text", attribute.String("kind", "failed to open the file")},
		{"oversized", attribute.String("kind", strings.Repeat("a", maxValueLen+1))},
		{"unsafe element in a slice", attribute.StringSlice("kinds", []string{"go", "/etc/passwd"})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckAttr(tc.attr)
			if err == nil {
				t.Fatalf("CheckAttr accepted %v", tc.attr)
			}
			var unsafeErr *UnsafeAttrError
			if !errors.As(err, &unsafeErr) {
				t.Fatalf("err is %T, want *UnsafeAttrError", err)
			}
			// The error travels into CI logs and issue reports. It must not
			// carry the thing it just refused. Short values are skipped:
			// "go" appears in the message's own reference to redact.go, and
			// a two-character value discloses nothing anyway.
			if v := tc.attr.Value.Emit(); len(v) >= 8 && strings.Contains(err.Error(), v) {
				t.Errorf("error message quotes the rejected value: %s", err)
			}
		})
	}
}

// TestAcceptsSafeAttributes guards the other direction. A validator that
// rejects everything is easy and useless; these must pass.
func TestAcceptsSafeAttributes(t *testing.T) {
	safe := []attribute.KeyValue{
		Language("go"),
		Language("objective-c++"), // unknown, normalised, still fine
		SizeBucket(3000),
		SizeBucket(1 << 30),
		ParseStatus("partial"),
		Confidence("exact"),
		AccuracyMode("fast"),
		EdgeKind("CONFIGURED_BY"),
		EntityKind("function"),
		Tool("find_symbol"),
		Status("timeout"),
		CacheHit(true),
		Operation("select"),
		attribute.Int64("cortex.batch_size", 512),
		attribute.Float64("cortex.ratio", 0.93),
		attribute.Bool("cortex.incremental", true),
		attribute.String("cortex.stage", "resolve"),
	}
	for _, kv := range safe {
		if err := CheckAttr(kv); err != nil {
			t.Errorf("CheckAttr rejected a safe attribute %v: %v", kv, err)
		}
	}
}

// TestStartSpanPanicsOnUnsafeAttrEvenWhenDisabled is the property that makes
// the guard useful in practice. Almost every test in the repository runs with
// telemetry off; if the check only ran on the enabled path, a leaking call site
// would reach users unchallenged.
func TestStartSpanPanicsOnUnsafeAttrEvenWhenDisabled(t *testing.T) {
	Reset()
	if Enabled() {
		t.Fatal("precondition: telemetry must be off")
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("StartSpan accepted a file path attribute with telemetry disabled")
		}
		if _, ok := r.(*UnsafeAttrError); !ok {
			t.Fatalf("panicked with %T, want *UnsafeAttrError", r)
		}
	}()
	//nolint:staticcheck // the panic is the assertion
	StartSpan(context.Background(), "index.file", badAttr())
}

// TestStartSpanPanicsOnUnsafeName: a span name built from data leaks just as
// effectively as an attribute, and is easier to write by accident.
func TestStartSpanPanicsOnUnsafeName(t *testing.T) {
	Reset()
	defer func() {
		if recover() == nil {
			t.Fatal("StartSpan accepted a span name containing a path")
		}
	}()
	StartSpan(context.Background(), "parse internal/parse/lang/go/parser.go")
}

// TestNonStrictScrubsInsteadOfPanicking covers the release binary. Telemetry is
// an optional observation of the work; it must never be the reason the work
// crashes.
func TestNonStrictScrubsInsteadOfPanicking(t *testing.T) {
	prev := SetStrict(false)
	t.Cleanup(func() { SetStrict(prev) })

	attrs := []attribute.KeyValue{Language("go"), badAttr()}
	guardAttrs(attrs) // must not panic

	if got := attrs[0].Value.AsString(); got != "go" {
		t.Errorf("safe attribute was altered: %q", got)
	}
	scrubbed := attrs[1].Value.AsString()
	if scrubbed != redactedValue {
		t.Errorf("unsafe value = %q, want %q", scrubbed, redactedValue)
	}
	if strings.Contains(scrubbed, "alice") || strings.Contains(scrubbed, ".go") {
		t.Error("scrubbed value still contains fragments of the original")
	}
}

// TestFileAttrsExportNoPath is the positive form of the rule: the ergonomic
// helper a call site will reach for produces only category and size bucket.
func TestFileAttrsExportNoPath(t *testing.T) {
	const path = "/Users/alice/work/acme-billing/internal/auth/token.go"
	attrs := FileAttrs(path, 4096)

	if err := CheckAttrs(attrs); err != nil {
		t.Fatalf("FileAttrs produced something the guard rejects: %v", err)
	}
	for _, kv := range attrs {
		v := kv.Value.Emit()
		for _, fragment := range []string{"alice", "acme", "auth", "token", "/"} {
			if strings.Contains(v, fragment) {
				t.Errorf("attribute %s = %q leaks %q from the path", kv.Key, v, fragment)
			}
		}
	}
	if got := attrs[0].Value.AsString(); got != "go" {
		t.Errorf("language = %q, want go", got)
	}
	if got := attrs[1].Value.AsString(); got != "16k" {
		t.Errorf("size bucket for 4096 bytes = %q, want 16k", got)
	}
}

// TestSizeBucketHidesExactSize: an exact byte count plus a language is close to
// a fingerprint for a specific file.
func TestSizeBucketHidesExactSize(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{0, "0"},
		{-1, "0"},
		{1, "1k"},
		{1023, "1k"},
		{1024, "4k"},
		{5000, "16k"},
		{100000, "256k"},
		{300000, "1m"},
		{1 << 24, "1m+"},
	} {
		if got := sizeBucketLabel(tc.bytes); got != tc.want {
			t.Errorf("sizeBucketLabel(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
	// Two nearby sizes must be indistinguishable, or the bucket is not doing
	// its job.
	if sizeBucketLabel(2000) != sizeBucketLabel(3999) {
		t.Error("nearby file sizes fall in different buckets; the bucket still discriminates")
	}
}

// TestLanguageOfBoundsCardinality: a hostile repository names files, and the
// extension must never become a metric dimension of its own.
func TestLanguageOfBoundsCardinality(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"internal/store/sqlite.go", "go"},
		{"src/App.tsx", "typescript"},
		{"Dockerfile", "dockerfile"},
		{"secret.aws-key-AKIAIOSFODNN7EXAMPLE", unknownToken},
		{"weird.exfiltrate-this-value", unknownToken},
		{"noextension", unknownToken},
	} {
		got := LanguageOf(tc.path).Value.AsString()
		if got != tc.want {
			t.Errorf("LanguageOf(%q) = %q, want %q", tc.path, got, tc.want)
		}
		if err := CheckAttr(LanguageOf(tc.path)); err != nil {
			t.Errorf("LanguageOf(%q) produced an attribute the guard rejects: %v", tc.path, err)
		}
	}
}

// TestNormaliseTokenRefusesToTruncate: truncating a path leaves a path, and
// silently creates unbounded label cardinality.
func TestNormaliseTokenRefusesToTruncate(t *testing.T) {
	long := strings.Repeat("select", 10)
	if got := normaliseToken(long); got != unknownToken {
		t.Errorf("normaliseToken of an overlong value = %q, want %q", got, unknownToken)
	}
	if got := normaliseToken("SELECT"); got != "select" {
		t.Errorf("normaliseToken(SELECT) = %q, want select", got)
	}
	if got := normaliseToken("/etc/passwd"); got != unknownToken {
		t.Errorf("normaliseToken of a path = %q, want %q", got, unknownToken)
	}
}

// TestRecordErrorExportsNoMessage: error strings in RepoCortex are full of paths.
func TestRecordErrorExportsNoMessage(t *testing.T) {
	spans, _ := enableForTest(t)

	_, span := StartSpan(context.Background(), "index.file")
	span.RecordError(errors.New("open /Users/alice/work/acme-billing/token.go: permission denied"))
	span.End()

	stubs := spans.GetSpans()
	if len(stubs) != 1 {
		t.Fatalf("got %d spans, want 1", len(stubs))
	}
	dump := stubs[0]
	for _, kv := range dump.Attributes {
		v := kv.Value.Emit()
		if strings.Contains(v, "alice") || strings.Contains(v, "permission denied") {
			t.Errorf("attribute %s exported the error message: %q", kv.Key, v)
		}
	}
	if dump.Status.Description != "" {
		t.Errorf("span status description = %q; it must stay empty because it would carry the error text", dump.Status.Description)
	}
	if len(dump.Events) > 0 {
		t.Errorf("RecordError added %d event(s); the OTel exception event carries exception.message", len(dump.Events))
	}
	var found bool
	for _, kv := range dump.Attributes {
		if string(kv.Key) == AttrErrorType {
			found = true
			if !strings.Contains(kv.Value.AsString(), "errorString") {
				t.Errorf("error.type = %q, want the Go type", kv.Value.AsString())
			}
		}
	}
	if !found {
		t.Errorf("RecordError exported no %s, so the failure is untyped in the trace", AttrErrorType)
	}
}
