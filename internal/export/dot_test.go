package export

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// reversedPairGraph has one edge whose source sorts AFTER its target. Any
// exporter that writes a canonicalised pair instead of From/To will emit it
// backwards, which is exactly the bug this test exists to catch.
func reversedPairGraph() metrics.Graph {
	return metrics.NewGraph(
		[]metrics.Node{{Key: "zebra", Kind: model.KindFunction}, {Key: "alpha", Kind: model.KindFunction}},
		[]metrics.Edge{{From: "zebra", To: "alpha", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}},
	)
}

func TestDOTPreservesEdgeDirection(t *testing.T) {
	var buf bytes.Buffer
	e, err := ForFormat(FormatDOT)
	if err != nil {
		t.Fatalf("ForFormat: %v", err)
	}
	if err := e.Write(context.Background(), &buf, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"zebra" -> "alpha"`) {
		t.Fatalf("direction lost or reversed.\ngot:\n%s", got)
	}
	if strings.Contains(got, `"alpha" -> "zebra"`) {
		t.Fatalf("edge emitted backwards.\ngot:\n%s", got)
	}
}

func TestDOTCarriesConfidence(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `confidence="high"`) {
		t.Fatalf("confidence not carried.\ngot:\n%s", buf.String())
	}
}

func TestDOTEscapesQuotesInKeys(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `we"ird`}, {Key: "plain"}},
		[]metrics.Edge{{From: `we"ird`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `we\"ird`) {
		t.Fatalf("quote not escaped.\ngot:\n%s", buf.String())
	}
}

// TestDOTBackslashSurvivesAlone pins the current (round-2) rule: a lone
// backslash in the input is escaped to `\\` in the output. This test used to
// assert the opposite (backslash passed through unescaped) under the round-1
// reasoning that DOT only special-cases `\"`. That reasoning missed the
// boundary case where a trailing input backslash collides with the closing
// quote quoteDOTID itself appends (see quoteDOTID's doc comment) — so the
// rule changed to the standard `\`->`\\`, `"`->`\"` scheme, and this test now
// pins that scheme instead of the one that caused the regression.
func TestDOTBackslashSurvivesAlone(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `path\to`}, {Key: "plain"}},
		[]metrics.Edge{{From: `path\to`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `path\\to`) {
		t.Fatalf("single backslash not escaped to \\\\.\ngot:\n%s", got)
	}
}

// TestDOTBackslashBeforeQuote pins the trickiest case: a backslash sitting
// immediately before a quote in the input. Both bytes are escaped
// independently in a single left-to-right pass: the input backslash becomes
// `\\` and the input quote becomes `\"`, so the four output bytes `\\\"`
// decode unambiguously back to the original two input bytes.
func TestDOTBackslashBeforeQuote(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `weird\"key`}, {Key: "plain"}},
		[]metrics.Edge{{From: `weird\"key`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	// Expect the literal 4-byte sequence: backslash, backslash, backslash,
	// quote, "key - i.e. the escaped input backslash (`\\`) followed by the
	// escaped input quote (`\"`).
	if !strings.Contains(got, `weird\\\"key`) {
		t.Fatalf("backslash-before-quote not independently escaped.\ngot:\n%s", got)
	}
}

// dotQuotedID extracts the first `"..."` DOT-quoted identifier from s,
// respecting the DOT/quoteDOTID escaping rule (\" is an escaped quote, any
// other backslash is a literal byte copied through), and reports whether the
// identifier was properly terminated by an unescaped closing quote before
// end of string. This is the structural check the substring-only tests
// above cannot provide: it proves the identifier actually closes where the
// escaper intended, not merely that some expected byte sequence appears
// somewhere in the output.
func dotQuotedID(s string) (id string, terminated bool) {
	if len(s) == 0 || s[0] != '"' {
		return "", false
	}
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			// Escape pair: copy both bytes through, matching the DOT lexer's
			// greedy backslash + next-char pairing.
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		if c == '"' {
			return b.String(), true
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), false
}

// TestDOTTrailingBackslashTerminatesIdentifier pins the confirmed defect: a
// key ending in a single backslash. quoteDOTID must escape that trailing
// backslash to `\\` so it cannot pair with the closing quote the function
// appends. Against the pre-fix implementation (escapes only `"`, copies `\`
// through unchanged) this fails: the output ends `...foo\"`, which a DOT
// lexer reads as an escaped quote rather than a terminator, so the
// identifier — and the rest of the document — never closes.
func TestDOTTrailingBackslashTerminatesIdentifier(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `foo\`}, {Key: "plain"}},
		[]metrics.Edge{{From: `foo\`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `foo\\`) {
		t.Fatalf("trailing backslash not escaped as \\\\.\ngot:\n%s", got)
	}
	idx := strings.Index(got, `"foo`)
	if idx == -1 {
		t.Fatalf("quoted key not found.\ngot:\n%s", got)
	}
	id, terminated := dotQuotedID(got[idx:])
	if !terminated {
		t.Fatalf("identifier never terminated (closing quote consumed by escape).\ngot:\n%s", got)
	}
	if id != `foo\\` {
		t.Fatalf("decoded identifier = %q, want %q", id, `foo\\`)
	}
}

// TestDOTDoubleTrailingBackslash extends the trailing-backslash case to two
// backslashes: each must be independently escaped to `\\`, so the key
// becomes four backslashes before the closing quote, and the identifier
// must still terminate correctly.
func TestDOTDoubleTrailingBackslash(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `foo\\`}, {Key: "plain"}},
		[]metrics.Edge{{From: `foo\\`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	idx := strings.Index(got, `"foo`)
	if idx == -1 {
		t.Fatalf("quoted key not found.\ngot:\n%s", got)
	}
	id, terminated := dotQuotedID(got[idx:])
	if !terminated {
		t.Fatalf("identifier never terminated.\ngot:\n%s", got)
	}
	if id != `foo\\\\` {
		t.Fatalf("decoded identifier = %q, want %q", id, `foo\\\\`)
	}
}

// TestDOTLoneBackslashKey is the minimal case of the defect: a key that is
// nothing but a single backslash. It must escape to `\\` and still
// terminate.
func TestDOTLoneBackslashKey(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `\`}, {Key: "plain"}},
		[]metrics.Edge{{From: `\`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	idx := strings.Index(got, `"\`)
	if idx == -1 {
		t.Fatalf("quoted key not found.\ngot:\n%s", got)
	}
	id, terminated := dotQuotedID(got[idx:])
	if !terminated {
		t.Fatalf("identifier never terminated.\ngot:\n%s", got)
	}
	if id != `\\` {
		t.Fatalf("decoded identifier = %q, want %q", id, `\\`)
	}
}

// TestDOTBackslashBeforeEmbeddedQuoteStillWorks re-pins the previously
// correct case using the stronger structural check: a backslash immediately
// before an embedded quote must still decode to exactly one backslash
// followed by one quote, with the identifier terminating at the true end.
func TestDOTBackslashBeforeEmbeddedQuoteStillWorks(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `a\"b`}, {Key: "plain"}},
		[]metrics.Edge{{From: `a\"b`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	idx := strings.Index(got, `"a\`)
	if idx == -1 {
		t.Fatalf("quoted key not found.\ngot:\n%s", got)
	}
	id, terminated := dotQuotedID(got[idx:])
	if !terminated {
		t.Fatalf("identifier never terminated.\ngot:\n%s", got)
	}
	// dotQuotedID intentionally does not collapse escape pairs (see its own
	// comment) — it copies each recognised pair through raw, only using
	// pairing to find the true terminating quote. So the extracted content
	// is the encoded form: input `\` -> `\\`, input `"` -> `\"`, i.e. six
	// raw bytes: a, \, \, \, ", b.
	if want := "a" + `\\` + `\"` + "b"; id != want {
		t.Fatalf("extracted identifier = %q, want %q", id, want)
	}
}

// TestDOTNonASCIINotEscaped confirms multi-byte UTF-8 passes through
// untouched, matching both %q's behavior (so this test does not by itself
// distinguish old from new) and the DOT grammar, which requires no escaping
// for printable non-ASCII runes.
func TestDOTNonASCIINotEscaped(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: "café"}, {Key: "中文"}},
		[]metrics.Edge{{From: "café", To: "中文", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"café"`) {
		t.Fatalf("non-ASCII rune escaped or mangled.\ngot:\n%s", got)
	}
	if !strings.Contains(got, `"中文"`) {
		t.Fatalf("non-ASCII rune escaped or mangled.\ngot:\n%s", got)
	}
}

// TestDOTLossyIsEmpty pins the current Lossy() answer: every metrics.Node and
// metrics.Edge field is represented in the DOT output (see dot.go's Write and
// Lossy doc comments), so the exporter can truthfully claim zero loss.
func TestDOTLossyIsEmpty(t *testing.T) {
	e, _ := ForFormat(FormatDOT)
	if got := e.Lossy(); got != "" {
		t.Fatalf("Lossy() = %q, want empty", got)
	}
}

// TestDOTCarriesWeight pins that Edge.Weight — previously dropped entirely
// while Lossy() claimed nothing was lost — is now written as the `weight`
// edge attribute.
func TestDOTCarriesWeight(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: "zebra"}, {Key: "alpha"}},
		[]metrics.Edge{{From: "zebra", To: "alpha", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 7}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatDOT)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `weight=7`) {
		t.Fatalf("weight not carried.\ngot:\n%s", buf.String())
	}
}

func TestDOTIsByteStable(t *testing.T) {
	e, _ := ForFormat(FormatDOT)
	var first bytes.Buffer
	if err := e.Write(context.Background(), &first, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i := 0; i < 20; i++ {
		var again bytes.Buffer
		if err := e.Write(context.Background(), &again, reversedPairGraph()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if again.String() != first.String() {
			t.Fatalf("run %d differs", i)
		}
	}
}
