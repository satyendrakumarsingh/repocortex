package export

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

// csvExporter writes an edge list for spreadsheets and notebooks.
type csvExporter struct{}

func (csvExporter) Format() Format { return FormatCSV }

// Lossy is non-empty on purpose, and it names every field an edge list
// cannot carry:
//   - metrics.Node entries with no edges have no row to live in, so isolated
//     nodes are silently absent from the output — often the interesting
//     symbols are the ones with no callers.
//   - Node.Kind is not carried: the CSV has no node-level row at all, only
//     edge rows, so there is nowhere to put it.
//   - Edge.Weight is not carried: the header is fixed at
//     source,target,kind,confidence and adding a fifth column would break
//     every consumer relying on that shape, so it is dropped and named here
//     instead.
//   - A cell beginning with '=', '+', '-', or '@' is prefixed with a single
//     quote before it is written (see neutralizeFormula), because a
//     spreadsheet would otherwise interpret it as a formula. A key that
//     legitimately starts with one of those bytes — an npm-scoped package
//     path like "@babel/core/index.js:parse" is the realistic case, since
//     repocortex indexes third-party dependency trees — is therefore not
//     round-tripped byte-for-byte: a consumer reading the CSV back
//     programmatically sees "'@babel/core/index.js:parse", not the raw key.
func (csvExporter) Lossy() string {
	return "edge list only: nodes with no edges are omitted, node kind is not carried, edge weight is not carried, and a cell starting with =, +, -, or @ is prefixed with a quote to block spreadsheet formula injection"
}

// neutralizeFormula defuses CSV formula injection: a leading '=', '+', '-',
// or '@' makes most spreadsheet applications (Excel, LibreOffice, Google
// Sheets) evaluate the cell as a formula rather than display it as text, and
// encoding/csv's RFC 4180 quoting does nothing to stop that — quoting
// protects against the file format's own delimiters, not against how the
// opening application interprets a quoted cell's content.
//
// The values that reach here are not a closed enum: From/To derive from
// model.DefaultFQN(path, name), and path is a repository-relative path from
// an arbitrary, potentially third-party or hostile source tree. An
// npm-scoped package directory like "@babel/core" is an ordinary, common
// case, not an edge case — so this guard applies to every field written to
// the CSV, not only the ones a spreadsheet formula historically abuses.
//
// The fix is the standard one: prefix the value with a single quote. That
// is a real, visible cost — it changes the exact bytes emitted for such a
// value, so a program reading this CSV back programmatically must know to
// strip a leading quote to recover the original key. Lossy() documents that
// cost rather than hiding it.
func neutralizeFormula(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	default:
		return s
	}
}

func (csvExporter) Write(ctx context.Context, w io.Writer, g metrics.Graph) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"source", "target", "kind", "confidence"}); err != nil {
		return fmt.Errorf("writing csv header: %w", err)
	}
	// g.Edges() is in canonical order, so this loop needs no sort of its own
	// to stay byte-stable across runs.
	for _, e := range g.Edges() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing csv rows: %w", err)
		}
		// From and To, explicitly — never a canonicalised pair. See the
		// package doc for why that matters. Every field is passed through
		// neutralizeFormula: Kind and Confidence are enum-derived today, but
		// the guard must not depend on that staying true.
		row := []string{
			neutralizeFormula(e.From),
			neutralizeFormula(e.To),
			neutralizeFormula(string(e.Kind)),
			neutralizeFormula(string(e.Confidence)),
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("writing csv row for %s->%s: %w", e.From, e.To, err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flushing csv: %w", err)
	}
	return nil
}

// Counts reports the edge list's own totals, which are not the graph's.
//
// Every edge gets a row, so the edge count is g's. The node count is not: a
// node exists in this artifact only as an endpoint of some row, so an isolated
// node has nowhere to be written and is absent — the first thing Lossy says.
// Counting the distinct endpoints here is what lets the command's success line
// agree with the note printed above it instead of contradicting it.
func (csvExporter) Counts(g metrics.Graph) (int, int) {
	edges := g.Edges()
	seen := make(map[string]struct{}, 2*len(edges))
	for _, e := range edges {
		seen[e.From] = struct{}{}
		seen[e.To] = struct{}{}
	}
	return len(seen), len(edges)
}
