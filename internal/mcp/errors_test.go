package mcp

import (
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// renderContent flattens a result's text content. Test-only: production code
// never needs a result's content back as a string, because failWith and
// failFrom hand the *sdk.CallToolResult straight to the go-sdk, which reads
// its Content directly. Its only callers are assertions in this package's
// tests.
func renderContent(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestHintForKindCoversEveryKindItClaims(t *testing.T) {
	for _, k := range []errs.Kind{
		errs.KindInvalid, errs.KindNotFound, errs.KindConflict, errs.KindCorrupt,
	} {
		code, hint := hintForKind(k)
		if code == "" {
			t.Errorf("kind %q produced an empty code", k)
		}
		if hint == "" {
			t.Errorf("kind %q produced an empty hint; an error an agent cannot act on is a dead end", k)
		}
	}
}

// Corruption must never suggest retrying. Telling a user to retry a corrupt
// index is actively harmful — errs documents exactly this at errs.go.
func TestCorruptHintSaysReindexNotRetry(t *testing.T) {
	_, hint := hintForKind(errs.KindCorrupt)
	lower := strings.ToLower(hint)
	if !strings.Contains(lower, "reindex") && !strings.Contains(lower, "repocortex index") {
		t.Errorf("corrupt hint must direct a reindex, got %q", hint)
	}
	if strings.Contains(lower, "retry") {
		t.Errorf("corrupt hint must not suggest retrying, got %q", hint)
	}
}

func TestNotFoundHintNamesAConcreteNextCall(t *testing.T) {
	_, hint := hintForKind(errs.KindNotFound)
	if !strings.Contains(hint, "search_code") && !strings.Contains(hint, "find_symbol") {
		t.Errorf("a not_found hint must name a tool to call next, got %q", hint)
	}
}

// TestTransientCancelledUnsupportedGetHonestHints guards against the
// default branch swallowing these three kinds into a generic "internal"
// code, which would contradict their documented semantics in errs.go:
// KindTransient is the retryable kind, and KindCancelled is explicitly
// "not a failure and must not be reported as one".
func TestTransientCancelledUnsupportedGetHonestHints(t *testing.T) {
	for _, tc := range []struct {
		kind errs.Kind
		want string
	}{
		{errs.KindTransient, "retry"},
		{errs.KindCancelled, "cancel"},
		{errs.KindUnsupported, ""},
	} {
		code, hint := hintForKind(tc.kind)
		if code == "" {
			t.Errorf("kind %q produced an empty code", tc.kind)
		}
		if hint == "" {
			t.Errorf("kind %q produced an empty hint", tc.kind)
		}
		if tc.want != "" && !strings.Contains(strings.ToLower(hint), tc.want) {
			t.Errorf("kind %q hint must mention %q, got %q", tc.kind, tc.want, hint)
		}
	}
}

// TestTransientHintMentionsRetryCorruptDoesNot pins the exact distinction
// the review flagged: KindTransient should tell the agent to retry, while
// KindCorrupt must never suggest that (see TestCorruptHintSaysReindexNotRetry).
func TestTransientHintMentionsRetryCorruptDoesNot(t *testing.T) {
	_, transientHint := hintForKind(errs.KindTransient)
	if !strings.Contains(strings.ToLower(transientHint), "retry") {
		t.Errorf("transient hint must mention retrying, got %q", transientHint)
	}
	_, corruptHint := hintForKind(errs.KindCorrupt)
	if strings.Contains(strings.ToLower(corruptHint), "retry") {
		t.Errorf("corrupt hint must not mention retrying, got %q", corruptHint)
	}
}

func TestFailWithRendersCodeAndHint(t *testing.T) {
	res, _, err := failWith("bad_argument", "Pass a non-empty name.", "name must not be empty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("failWith must produce an error result")
	}
	text := renderContent(res)
	for _, want := range []string{"bad_argument", "name must not be empty", "Pass a non-empty name."} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered error missing %q; got:\n%s", want, text)
		}
	}
}
