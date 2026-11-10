package export

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestSVGIsByteStableAcrossRuns(t *testing.T) {
	e, _ := ForFormat(FormatSVG)
	var first bytes.Buffer
	if err := e.Write(context.Background(), &first, bridgeExportGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i := 0; i < 20; i++ {
		var again bytes.Buffer
		if err := e.Write(context.Background(), &again, bridgeExportGraph()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if again.String() != first.String() {
			t.Fatalf("run %d produced a different picture; the layout is not seeded", i)
		}
	}
}

func TestSVGHasViewBoxAndOneLinePerEdge(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatSVG)
	if err := e.Write(context.Background(), &buf, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "viewBox=") {
		t.Error("no viewBox: the picture will not scale")
	}
	if n := strings.Count(out, "<line "); n != 1 {
		t.Errorf("got %d <line> elements, want 1", n)
	}
}

// TestSVGLossyPinsExactValue pins the SVG exporter's Lossy() string so a
// future edit that silently drops (or silently adds) a represented field
// changes the test, not just the docstring. See svg.go's Lossy for the field
// audit this string reports.
func TestSVGLossyPinsExactValue(t *testing.T) {
	e, _ := ForFormat(FormatSVG)
	const want = "a picture, not a serialization: Key is drawn as a label, and From/To " +
		"as line endpoints with an arrowhead marker showing direction; Confidence is drawn " +
		"only as a two-way solid/dashed split (exact and high draw solid, medium and low draw " +
		"dashed), so the exact confidence level does not survive; Node.Kind, Edge.Kind and " +
		"Edge.Weight are not drawn anywhere and cannot be recovered from the output; and a " +
		"label is XML text, so runes XML 1.0 cannot encode are DELETED from it rather than " +
		"drawn (see stripXMLIllegal), which the GraphML exporter states for the same reason"
	if got := e.Lossy(); got != want {
		t.Fatalf("Lossy() =\n%q\nwant\n%q", got, want)
	}
}

// assertAllCoordinatesInCanvas parses every <circle cx cy> and <line x1 y1
// x2 y2> in an emitted SVG and fails if any coordinate falls outside
// [0, Width] x [0, Height]. This is the general form of the single-node
// reproducer below: a coordinate outside the viewBox is not an XML error —
// the document stays well-formed — it is just silently clipped by the
// viewer, so nothing but an explicit bounds check catches it.
func assertAllCoordinatesInCanvas(t *testing.T, svg string, width, height int) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(svg))
	checked := 0
	attrFloat := func(attrs []xml.Attr, name string) float64 {
		for _, a := range attrs {
			if a.Name.Local == name {
				v, err := strconv.ParseFloat(a.Value, 64)
				if err != nil {
					t.Fatalf("attribute %s=%q is not a float: %v", name, a.Value, err)
				}
				return v
			}
		}
		t.Fatalf("element missing attribute %s", name)
		return 0
	}
	checkBounds := func(label string, x, y float64) {
		if x < 0 || x > float64(width) || y < 0 || y > float64(height) {
			t.Errorf("%s at (%.2f, %.2f) is outside the canvas [0,%d]x[0,%d]", label, x, y, width, height)
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "circle":
			checkBounds("circle", attrFloat(se.Attr, "cx"), attrFloat(se.Attr, "cy"))
			checked++
		case "line":
			checkBounds("line start", attrFloat(se.Attr, "x1"), attrFloat(se.Attr, "y1"))
			checkBounds("line end", attrFloat(se.Attr, "x2"), attrFloat(se.Attr, "y2"))
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no <circle> or <line> elements found; the test fixture is empty")
	}
}

// singleNodeGraph is the minimal reproducer for the off-canvas bug: a graph
// with exactly one node has nothing to repel it and no edge to attract it,
// so if its starting position is ever off-canvas, nothing in the force
// simulation moves it back.
func singleNodeGraph() metrics.Graph {
	return metrics.NewGraph([]metrics.Node{{Key: "only"}}, nil)
}

// TestSVGSingleNodeStaysInCanvas is the direct reproducer: before the fix, a
// single-node graph places its one node with initialSpiralPositions' t
// starting at the fixed offset opts.Seed%97 (42) regardless of n=1, so
// r overshoots the intended radius and the node is emitted at cx > Width.
func TestSVGSingleNodeStaysInCanvas(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatSVG)
	if err := e.Write(context.Background(), &buf, singleNodeGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	opts := DefaultLayoutOptions()
	assertAllCoordinatesInCanvas(t, buf.String(), opts.Width, opts.Height)
}

// TestSVGSingleNodeWithSelfLoopStaysInCanvas checks the self-loop variant of
// the reproducer. metrics.NewGraph drops any edge where From == To, so this
// graph in practice carries zero edges once constructed — the same shape as
// singleNodeGraph above. The test is kept anyway to document that fact
// (verified with g.Edges() below) rather than leave it as an assumption, and
// because it is the exact shape the original bug report reproduced with.
func TestSVGSingleNodeWithSelfLoopStaysInCanvas(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: "only"}},
		[]metrics.Edge{{From: "only", To: "only", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}},
	)
	if edges := g.Edges(); len(edges) != 0 {
		t.Fatalf("expected metrics.NewGraph to drop the self-loop, got %d edges: %+v", len(edges), edges)
	}
	var buf bytes.Buffer
	e, _ := ForFormat(FormatSVG)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	opts := DefaultLayoutOptions()
	assertAllCoordinatesInCanvas(t, buf.String(), opts.Width, opts.Height)
}

// TestSVGTwoNodesNoEdgesStaysInCanvas covers pure repulsion with no
// attraction pulling nodes back toward each other or the center.
func TestSVGTwoNodesNoEdgesStaysInCanvas(t *testing.T) {
	g := metrics.NewGraph([]metrics.Node{{Key: "a"}, {Key: "b"}}, nil)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatSVG)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	opts := DefaultLayoutOptions()
	assertAllCoordinatesInCanvas(t, buf.String(), opts.Width, opts.Height)
}

// TestSVGBridgeFixtureStaysInCanvas pins the general bounds property against
// the shared bridge fixture already used by TestSVGIsByteStableAcrossRuns.
func TestSVGBridgeFixtureStaysInCanvas(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatSVG)
	if err := e.Write(context.Background(), &buf, bridgeExportGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	opts := DefaultLayoutOptions()
	assertAllCoordinatesInCanvas(t, buf.String(), opts.Width, opts.Height)
}

// TestSVGLargeGraphStaysInCanvas pins the same property at a scale (200
// nodes, a chain of edges) closer to what the O(n^2) doc comment on Write
// describes, so the bounds guarantee isn't only proven for tiny fixtures.
func TestSVGLargeGraphStaysInCanvas(t *testing.T) {
	const n = 200
	nodes := make([]metrics.Node, n)
	edges := make([]metrics.Edge, 0, n-1)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("node-%03d", i)
		nodes[i] = metrics.Node{Key: key}
		if i > 0 {
			edges = append(edges, metrics.Edge{
				From: fmt.Sprintf("node-%03d", i-1), To: key,
				Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
			})
		}
	}
	g := metrics.NewGraph(nodes, edges)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatSVG)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	opts := DefaultLayoutOptions()
	assertAllCoordinatesInCanvas(t, buf.String(), opts.Width, opts.Height)
}

// TestSVGRejectsHostileNodeKeys mirrors graphml_test.go's
// TestGraphMLRejectsHostileNodeKeys: a node key containing XML metacharacters
// must not corrupt the document. The output must still parse as well-formed
// XML.
func TestSVGRejectsHostileNodeKeys(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: `<script>&"'`}, {Key: "plain"}},
		[]metrics.Edge{{From: `<script>&"'`, To: "plain", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatSVG)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc any
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not well-formed XML: %v\ngot:\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "<script>") {
		t.Fatalf("hostile key rendered unescaped.\ngot:\n%s", buf.String())
	}
}
