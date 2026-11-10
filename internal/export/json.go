package export

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

// JSONNode is one node on the wire.
type JSONNode struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// JSONEdge is one directed edge on the wire.
//
// Weight rides alongside Kind and Confidence for the same reason it rides in
// DOT and GraphML: a user exporting the same graph three ways should not get
// three different fidelities, and JSON has no competing semantic (the way
// DOT's `weight` attribute doubles as a layout hint) that would make carrying
// it a tradeoff instead of a straight win.
type JSONEdge struct {
	Source     string `json:"source"`
	Target     string `json:"target"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
	Weight     int    `json:"weight"`
}

// JSONGraph is the node-link document.
//
// Directed is stated rather than implied. A consumer that has to guess will
// guess wrong on exactly the graphs where it matters.
type JSONGraph struct {
	Directed bool       `json:"directed"`
	Nodes    []JSONNode `json:"nodes"`
	Edges    []JSONEdge `json:"edges"`
}

// jsonExporter writes a node-link JSON document: the shape D3, NetworkX and
// most notebook tooling expect out of the box.
type jsonExporter struct{}

func (jsonExporter) Format() Format { return FormatJSON }

// Lossy is empty because every field on metrics.Node (Key, Kind) and
// metrics.Edge (From, To, Kind, Confidence, Weight) is written somewhere in
// the output: Key as node id, Kind as node "kind", From/To as edge
// source/target, Edge.Kind and Confidence as their own fields, and Weight as
// the edge "weight" field. If a future field is added to either struct and
// not wired into Write below, this comment goes stale before the code does —
// update both together.
func (jsonExporter) Lossy() string { return "" }

// Counts: every node and every edge in g reaches the artifact.
func (jsonExporter) Counts(g metrics.Graph) (int, int) { return g.Order(), len(g.Edges()) }

func (jsonExporter) Write(ctx context.Context, w io.Writer, g metrics.Graph) error {
	doc := JSONGraph{Directed: true, Nodes: []JSONNode{}, Edges: []JSONEdge{}}
	// g.Keys() is sorted and g.Edges() is in canonical order (per the
	// metrics package contract), so ranging over them directly — never a
	// map — is what keeps this output byte-stable across runs.
	for _, k := range g.Keys() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("encoding json nodes: %w", err)
		}
		n, _ := g.Node(k)
		doc.Nodes = append(doc.Nodes, JSONNode{ID: k, Kind: string(n.Kind)})
	}
	for _, e := range g.Edges() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("encoding json edges: %w", err)
		}
		// Source and Target come from From/To, explicitly — never a
		// canonicalised pair. See the package doc for why that matters.
		doc.Edges = append(doc.Edges, JSONEdge{
			Source: e.From, Target: e.To,
			Kind: string(e.Kind), Confidence: string(e.Confidence),
			Weight: e.Weight,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encoding json graph: %w", err)
	}
	return nil
}
