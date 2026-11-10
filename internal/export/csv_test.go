package export

import (
	"bytes"
	"context"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestCSVEmitsHeaderAndDirectedRow(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatCSV)
	if err := e.Write(context.Background(), &buf, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want header + 1 row:\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "source,target,kind,confidence") {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "zebra,alpha,") {
		t.Errorf("row = %q, want zebra,alpha first", lines[1])
	}
}

func TestCSVDeclaresItselfLossy(t *testing.T) {
	e, _ := ForFormat(FormatCSV)
	if e.Lossy() == "" {
		t.Fatal("CSV Lossy() = \"\", but an edge list cannot carry isolated nodes")
	}
}

// TestCSVLossyValue pins the exact Lossy() string. It must fail if the
// message drifts out of sync with what the CSV columns actually carry:
// source,target,kind,confidence — no node kind, no isolated nodes, and no
// weight.
func TestCSVLossyValue(t *testing.T) {
	e, _ := ForFormat(FormatCSV)
	want := "edge list only: nodes with no edges are omitted, node kind is not carried, edge weight is not carried, and a cell starting with =, +, -, or @ is prefixed with a quote to block spreadsheet formula injection"
	if got := e.Lossy(); got != want {
		t.Fatalf("Lossy() = %q, want %q", got, want)
	}
}

// formulaInjectionGraph builds a two-node, one-edge graph whose From key is
// the given hostile-or-benign value, so each neutralization test can focus
// on a single leading byte.
func formulaInjectionGraph(fromKey string) metrics.Graph {
	return metrics.NewGraph(
		[]metrics.Node{{Key: fromKey, Kind: model.KindFunction}, {Key: "ok", Kind: model.KindFunction}},
		[]metrics.Edge{{From: fromKey, To: "ok", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}},
	)
}

// writeCSV runs the CSV exporter and returns the raw output, failing the
// test on any write error.
func writeCSV(t *testing.T, g metrics.Graph) string {
	t.Helper()
	var buf bytes.Buffer
	e, _ := ForFormat(FormatCSV)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.String()
}

// firstField parses out with encoding/csv and returns the source column of
// the single data row, so tests assert on the value a spreadsheet or a
// programmatic CSV reader actually sees, not on a raw substring of the file.
func firstField(t *testing.T, out string) string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(out))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv.Reader could not parse exporter output: %v\n%s", err, out)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want header + 1 row:\n%s", len(records), out)
	}
	return records[1][0]
}

// TestCSVNeutralizesEqualsFormula is the case from the finding: a node key
// beginning with '=' passes through encoding/csv unquoted and unescaped,
// and a spreadsheet opening the file evaluates it as a formula.
func TestCSVNeutralizesEqualsFormula(t *testing.T) {
	out := writeCSV(t, formulaInjectionGraph(`=CMD('calc')!A0`))
	got := firstField(t, out)
	want := `'=CMD('calc')!A0`
	if got != want {
		t.Fatalf("source field = %q, want %q (leading '=' must be neutralized)\nraw output:\n%s", got, want, out)
	}
}

// TestCSVNeutralizesAtFormula is the realistic case named in the finding:
// cortex indexes dependency trees, and an npm-scoped package directory
// like "@babel/core" is an ordinary path component, not a contrived attack
// string, yet a leading '@' is still a spreadsheet formula trigger.
func TestCSVNeutralizesAtFormula(t *testing.T) {
	out := writeCSV(t, formulaInjectionGraph("@babel/core/index.js:parse"))
	got := firstField(t, out)
	want := "'@babel/core/index.js:parse"
	if got != want {
		t.Fatalf("source field = %q, want %q (leading '@' must be neutralized)\nraw output:\n%s", got, want, out)
	}
}

// TestCSVNeutralizesPlusMinusFormula covers the remaining two RFC-recognized
// formula-trigger bytes, '+' and '-'.
func TestCSVNeutralizesPlusMinusFormula(t *testing.T) {
	cases := []string{"+1+1", "-2+3"}
	for _, in := range cases {
		out := writeCSV(t, formulaInjectionGraph(in))
		got := firstField(t, out)
		want := "'" + in
		if got != want {
			t.Fatalf("key %q: source field = %q, want %q\nraw output:\n%s", in, got, want, out)
		}
	}
}

// TestCSVLeavesOrdinaryKeysUnchanged is the guard against the guard: a key
// that does not begin with =, +, -, or @ must be emitted exactly as given.
// A neutralizer that is too aggressive (e.g. quoting anything containing
// those bytes, not just a leading one) would corrupt normal symbol names.
func TestCSVLeavesOrdinaryKeysUnchanged(t *testing.T) {
	in := "pkg/model/model.go:DefaultFQN"
	out := writeCSV(t, formulaInjectionGraph(in))
	got := firstField(t, out)
	if got != in {
		t.Fatalf("source field = %q, want unchanged %q\nraw output:\n%s", got, in, out)
	}
}

// TestCSVRoundTripsSpecialRFC4180Chars pins the behavior the finding did NOT
// dispute: encoding/csv's own RFC 4180 quoting of commas, embedded double
// quotes, and embedded newlines already worked before this change, and the
// formula-injection guard must not break it.
func TestCSVRoundTripsSpecialRFC4180Chars(t *testing.T) {
	in := "a,b\"c\nd"
	out := writeCSV(t, formulaInjectionGraph(in))
	got := firstField(t, out)
	if got != in {
		t.Fatalf("source field = %q, want unchanged %q (RFC 4180 chars must still round-trip)\nraw output:\n%s", got, in, out)
	}
}
