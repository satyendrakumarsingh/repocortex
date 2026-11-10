package export

import (
	"bytes"
	"context"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestGraphMLIsWellFormedXML(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatGraphML)
	if err := e.Write(context.Background(), &buf, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	dec := xml.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("not well-formed: %v\n%s", err, buf.String())
		}
	}
}

func TestGraphMLDeclaresEdgedefaultDirected(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatGraphML)
	if err := e.Write(context.Background(), &buf, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `edgedefault="directed"`) {
		t.Fatalf("graph not declared directed.\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `source="zebra" target="alpha"`) {
		t.Fatalf("direction lost or reversed.\n%s", buf.String())
	}
}

func TestGraphMLStripsXMLIllegalRunes(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: "bad\x00\x08key"}, {Key: "ok"}},
		[]metrics.Edge{{From: "bad\x00\x08key", To: "ok", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatGraphML)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.ContainsRune(buf.String(), '\x00') || strings.ContainsRune(buf.String(), '\x08') {
		t.Fatal("XML-illegal control rune survived into output")
	}
}

// TestGraphMLLossyDisclosesRuneStripping pins Lossy() to what this exporter
// actually does.
//
// Every metrics.Node and metrics.Edge FIELD is represented — that part has
// been true since Edge.Weight gained a <key id="weight">/<data key="weight">
// pair (see TestGraphMLCarriesEdgeWeight), and Lossy() said "" on the strength
// of it. But stripXMLIllegal deletes runes from every VALUE, node ids and edge
// endpoints included, so an id can leave here shorter than it arrived and
// different from the same id in the DOT export. Claiming faithfulness while
// silently rewriting a primary key is the failure this test exists to prevent.
//
// TestGraphMLStripsIllegalRunesFromNodeIDs below is the one that pins the
// behaviour; this one pins the disclosure of it.
func TestGraphMLLossyDisclosesRuneStripping(t *testing.T) {
	e, _ := ForFormat(FormatGraphML)
	got := e.Lossy()
	if got == "" {
		t.Fatal("Lossy() = \"\", but stripXMLIllegal deletes runes from node ids and edge endpoints")
	}
	for _, want := range []string{"node ids", "edge endpoints", "every field itself is written"} {
		if !strings.Contains(got, want) {
			t.Errorf("Lossy() must say %q; got %q", want, got)
		}
	}
}

// TestGraphMLStripsIllegalRunesFromNodeIDs is the behaviour Lossy() now
// discloses. The rune is dropped rather than escaped because no escape makes
// it legal — but the id that comes out is not the id that went in, and DOT,
// which does not strip, writes the original.
func TestGraphMLStripsIllegalRunesFromNodeIDs(t *testing.T) {
	const dirty = "a/x.go:One\x00Two"
	const clean = "a/x.go:OneTwo"
	g := metrics.NewGraph(
		[]metrics.Node{{Key: dirty, Kind: model.KindFunction}, {Key: "b/y.go:Two", Kind: model.KindFunction}},
		[]metrics.Edge{{From: dirty, To: "b/y.go:Two", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatGraphML)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, dirty) {
		t.Fatal("the NUL survived into the document; it would not parse")
	}
	if !strings.Contains(out, `<node id="`+clean+`">`) {
		t.Fatalf("want the stripped id %q as a node id; got:\n%s", clean, out)
	}
	if !strings.Contains(out, `source="`+clean+`"`) {
		t.Fatalf("the edge endpoint must be stripped the same way its node id was; got:\n%s", out)
	}
}

// TestGraphMLCarriesEdgeWeight is the field-level counterpart to
// TestGraphMLLossyIsEmpty: it does not trust the summary string, it checks
// the actual <key>/<data> pair Write emits for Edge.Weight. Without this
// test, Lossy() could be hand-edited back to "" while Write silently stopped
// emitting weight, and nothing would catch it.
func TestGraphMLCarriesEdgeWeight(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: "zebra", Kind: model.KindFunction}, {Key: "alpha", Kind: model.KindFunction}},
		[]metrics.Edge{{From: "zebra", To: "alpha", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 7}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatGraphML)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `<key id="weight" for="edge" attr.name="weight" attr.type="int"/>`) {
		t.Fatalf("no weight <key> declaration.\n%s", got)
	}
	if !strings.Contains(got, `<data key="weight">7</data>`) {
		t.Fatalf("edge weight not carried as <data key=\"weight\">.\n%s", got)
	}
}

// TestGraphMLRejectsHostileNodeKeys feeds node keys built from every byte
// sequence that can break an XML document if escaping is wrong: markup
// delimiters (<, >, &), both quote styles (", '), a NUL byte (illegal in XML
// outright, not just unescaped), a CDATA terminator (]]>), and an XML comment
// terminator (-->). A key containing any of these that reaches the output
// unescaped could close an attribute early, close an element early, or open
// a comment/CDATA section that swallows the rest of the document — turning
// one adversarial symbol name into a corrupted export for the whole graph.
// The reviewer who found this gap verified it manually in a scratch file
// that was deleted before being committed; this test is that verification,
// committed so it runs on every future change to xmlText/xmlAttr/Write.
func TestGraphMLRejectsHostileNodeKeys(t *testing.T) {
	hostile := "a<b>c&d\"e'f\x00g]]>h-->i"
	g := metrics.NewGraph(
		[]metrics.Node{{Key: hostile, Kind: model.KindFunction}, {Key: "ok", Kind: model.KindFunction}},
		[]metrics.Edge{{From: hostile, To: "ok", Kind: model.EdgeCalls, Confidence: model.ConfidenceLow, Weight: 3}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatGraphML)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()

	if strings.ContainsRune(out, '\x00') {
		t.Fatalf("NUL byte survived into output — an XML 1.0 parser cannot even tokenize this.\n%s", out)
	}

	dec := xml.NewDecoder(strings.NewReader(out))
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("hostile key broke out of its element/attribute: %v\n%s", err, out)
		}
	}
}
