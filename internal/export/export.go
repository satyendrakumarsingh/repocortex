// Package export writes a code graph out in interchange formats.
//
// # Why this package exists
//
// The web explorer's /api/graph payload is view-shaped: it carries the LOD
// level, budgets and cluster assignments a drawing needs. That is the right
// shape for a viewer and the wrong shape for anything else. A reader who wants
// the graph in Gephi, in Graphviz, in a notebook or in a diff wants the graph,
// not a rendering of it.
//
// # The one rule every exporter here obeys
//
// Direction is read from Edge.From and Edge.To, never from the order two node
// keys happen to appear in. Writing a canonicalised pair into a directional
// format silently reverses every relationship whose source sorts after its
// target, and nothing downstream can detect it. Confidence is likewise carried
// into every format that can hold it, and an exporter that cannot hold it
// declares itself Lossy rather than dropping it quietly.
package export

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

// Format names one output encoding.
type Format string

const (
	FormatDOT     Format = "dot"
	FormatGraphML Format = "graphml"
	FormatJSON    Format = "json"
	FormatCSV     Format = "csv"
	FormatSVG     Format = "svg"
)

// Exporter writes a graph in one format.
//
// The interface is deliberately three methods: an exporter that needs more
// than this is doing something other than encoding.
type Exporter interface {
	// Format is the name this exporter is registered under.
	Format() Format
	// Lossy reports what this format cannot represent, or "" when it is
	// faithful. Callers surface it so a reader is never surprised by an export
	// that quietly dropped half the model.
	Lossy() string
	// Write encodes g into w.
	Write(ctx context.Context, w io.Writer, g metrics.Graph) error
	// Counts reports how many nodes and edges the artifact Write produces
	// will actually contain, which is not always what g holds.
	//
	// It exists because a caller reporting "wrote N nodes" had nothing to go
	// on but g.Order(), and for an edge-list format that number is a claim
	// about the graph contradicting the same command's own lossy note about
	// the file. A format that carries everything answers with g's own totals;
	// one that cannot says what it can. Keeping the answer beside the encoder
	// is what stops the two from drifting apart.
	Counts(g metrics.Graph) (nodes, edges int)
}

// registry maps a format to its exporter. It is built per call rather than
// held in a package-level variable: CLAUDE.md forbids global mutable state, and
// a registry that cannot be mutated has nothing to gain from being global.
func registry() map[Format]Exporter {
	return map[Format]Exporter{
		FormatDOT:     dotExporter{},
		FormatGraphML: graphMLExporter{},
		FormatJSON:    jsonExporter{},
		FormatCSV:     csvExporter{},
		FormatSVG:     svgExporter{},
	}
}

// Formats lists every supported format, ascending.
func Formats() []Format {
	reg := registry()
	out := make([]Format, 0, len(reg))
	for f := range reg {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ForFormat resolves a format name to its exporter.
func ForFormat(f Format) (Exporter, error) {
	if e, ok := registry()[f]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("unknown export format %q: supported formats are %v", f, Formats())
}
