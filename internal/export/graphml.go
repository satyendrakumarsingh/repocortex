package export

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

// graphMLExporter writes GraphML, the format Gephi, yEd and NetworkX read.
//
// Two details matter and are easy to get wrong. The graph is declared
// edgedefault="directed" and each edge names source and target explicitly, so
// direction survives a round trip. And every attribute value is stripped of
// runes XML cannot hold — a NUL that reaches a parser makes the whole document
// unreadable, which turns a cosmetic bug into total data loss.
type graphMLExporter struct{}

func (graphMLExporter) Format() Format { return FormatGraphML }

// Lossy names the one thing this format cannot round-trip: runes XML 1.0
// cannot encode at all.
//
// Every FIELD survives. metrics.Node (Key, Kind) and metrics.Edge (From, To,
// Kind, Confidence, Weight) are all written below: Key/From/To ride as node
// ids and edge source/target, Node.Kind as the "kind" node data, Edge.Kind as
// "ekind", Edge.Confidence as "conf", and Edge.Weight as "weight". Unlike DOT,
// GraphML's `weight` attribute name carries no competing layout semantic to
// trade off against — it is a plain typed <key>/<data> pair, so there was no
// reason for this exporter to report worse fidelity than DOT for the same
// graph. If a future field is added to either struct and not wired into Write,
// this comment goes stale before the code does — update both together.
//
// What does not survive is a VALUE containing an XML-illegal rune, because
// stripXMLIllegal deletes it — from node ids and edge endpoints included, not
// only from display text. There is no escape that would make such a rune
// legal, so dropping it is the right trade against an unparseable document;
// what was wrong was claiming this exporter was faithful while doing it. DOT
// does not strip, so an id carrying one of these runes differs between the two
// formats and this is the format whose ids changed.
func (graphMLExporter) Lossy() string {
	return "runes XML 1.0 cannot encode (NUL and most other control characters, unpaired " +
		"surrogates, U+FFFE and U+FFFF) are DELETED from every value, node ids and edge " +
		"endpoints included, because no escape makes them legal and a document containing " +
		"one does not parse at all; every field itself is written"
}

// Counts: every node and every edge in g reaches the artifact.
func (graphMLExporter) Counts(g metrics.Graph) (int, int) { return g.Order(), len(g.Edges()) }

func (graphMLExporter) Write(ctx context.Context, w io.Writer, g metrics.Graph) error {
	bw := &errWriter{w: w}
	bw.printf(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	bw.printf(`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">` + "\n")
	bw.printf(`  <key id="kind" for="node" attr.name="kind" attr.type="string"/>` + "\n")
	bw.printf(`  <key id="ekind" for="edge" attr.name="kind" attr.type="string"/>` + "\n")
	bw.printf(`  <key id="conf" for="edge" attr.name="confidence" attr.type="string"/>` + "\n")
	bw.printf(`  <key id="weight" for="edge" attr.name="weight" attr.type="int"/>` + "\n")
	bw.printf(`  <graph id="cortex" edgedefault="directed">` + "\n")

	for _, k := range g.Keys() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing graphml nodes: %w", err)
		}
		n, _ := g.Node(k)
		bw.printf("    <node id=%s>\n", xmlAttr(k))
		bw.printf("      <data key=\"kind\">%s</data>\n", xmlText(string(n.Kind)))
		bw.printf("    </node>\n")
	}

	for i, e := range g.Edges() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing graphml edges: %w", err)
		}
		// From and To, explicitly — never a canonicalised pair. See the
		// package doc for why this matters: a canonicalised pair silently
		// reverses any edge whose source sorts after its target.
		bw.printf("    <edge id=\"e%d\" source=%s target=%s>\n", i, xmlAttr(e.From), xmlAttr(e.To))
		bw.printf("      <data key=\"ekind\">%s</data>\n", xmlText(string(e.Kind)))
		bw.printf("      <data key=\"conf\">%s</data>\n", xmlText(string(e.Confidence)))
		bw.printf("      <data key=\"weight\">%d</data>\n", e.Weight)
		bw.printf("    </edge>\n")
	}

	bw.printf("  </graph>\n</graphml>\n")
	if bw.err != nil {
		return fmt.Errorf("writing graphml: %w", bw.err)
	}
	return nil
}

// stripXMLIllegal removes runes XML 1.0 cannot encode at all. They are
// dropped rather than escaped because there is no escape that makes them
// legal — a NUL or other control rune reaching a parser makes the whole
// document unreadable, turning a cosmetic bug into total data loss.
func stripXMLIllegal(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x09 || r == 0x0A || r == 0x0D:
			return r
		case r >= 0x20 && r <= 0xD7FF:
			return r
		case r >= 0xE000 && r <= 0xFFFD:
			return r
		case r >= 0x10000 && r <= 0x10FFFF:
			return r
		default:
			return -1
		}
	}, s)
}

// xmlText renders s as XML element text content. Reused by the SVG exporter
// (a later task) for the same reason it exists here: any text content
// destined for an XML document needs both illegal-rune stripping and entity
// escaping, and that pairing is easy to get half-right by hand each time.
func xmlText(s string) string {
	s = stripXMLIllegal(s)
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// xmlAttr renders s as a double-quoted XML attribute value, including the
// surrounding quotes. Reused by the SVG exporter for the same reason as
// xmlText.
func xmlAttr(s string) string {
	s = stripXMLIllegal(s)
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return `"` + r.Replace(s) + `"`
}
