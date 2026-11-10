package export

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// dotExporter writes Graphviz DOT.
//
// DOT is the format that makes a graph printable: `dot -Tpng` or `dot -Tsvg`
// turns this straight into a picture with a layered layout the canvas explorer
// does not offer. Confidence rides as an edge attribute and as a line style,
// and Weight rides as the `weight` attribute (see the Write comment below), so
// a printed diagram shows the same certainty gradient and traversal cost the
// UI does. Every field of metrics.Node and metrics.Edge is represented in the
// output, so Lossy() can truthfully answer "nothing is dropped".
type dotExporter struct{}

func (dotExporter) Format() Format { return FormatDOT }

// Lossy is empty because every field on metrics.Node (Key, Kind) and
// metrics.Edge (From, To, Kind, Confidence, Weight) is written somewhere in
// the output: Key/From/To as quoted IDs, Kind as a `kind` attribute,
// Confidence as both a `confidence` attribute and a line style, and Weight as
// a `weight` attribute. If a future field is added to either struct and not
// wired into Write below, this comment goes stale before the code does —
// update both together.
func (dotExporter) Lossy() string { return "" }

// Counts: every node and every edge in g reaches the artifact.
func (dotExporter) Counts(g metrics.Graph) (int, int) { return g.Order(), len(g.Edges()) }

func (dotExporter) Write(ctx context.Context, w io.Writer, g metrics.Graph) error {
	bw := &errWriter{w: w}
	bw.printf("digraph cortex {\n")
	bw.printf("  graph [rankdir=LR];\n")
	bw.printf("  node [shape=box, fontname=\"Helvetica\"];\n")

	for _, k := range g.Keys() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing dot nodes: %w", err)
		}
		n, _ := g.Node(k)
		bw.printf("  %s [kind=%s];\n", quoteDOTID(k), quoteDOTID(string(n.Kind)))
	}

	for _, e := range g.Edges() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing dot edges: %w", err)
		}
		// From and To, explicitly. Never a canonicalised pair.
		//
		// weight=N is written for two reasons at once: it is the only place
		// metrics.Edge.Weight is represented, and it happens to be the exact
		// attribute name Graphviz's own layout engines (dot, neato, ...) read
		// as an edge-length/rank hint. That reuse is intentional here — a
		// heavier call-graph edge pulling its endpoints closer together in
		// the rendered picture is a reasonable default, not a bug — but a
		// caller who wants the numeric weight represented WITHOUT feeding the
		// layout should not reuse this exporter unmodified.
		bw.printf("  %s -> %s [kind=%s, confidence=%s, weight=%d, style=%s];\n",
			quoteDOTID(e.From), quoteDOTID(e.To),
			quoteDOTID(string(e.Kind)), quoteDOTID(string(e.Confidence)), e.Weight, dotStyle(e.Confidence))
	}

	bw.printf("}\n")
	if bw.err != nil {
		return fmt.Errorf("writing dot: %w", bw.err)
	}
	return nil
}

// dotStyle renders confidence as line style so a printed graph shows certainty
// without a legend: solid for compiler-grade facts, dashed for guesses.
func dotStyle(c model.Confidence) string {
	switch c {
	case model.ConfidenceExact, model.ConfidenceHigh:
		return "solid"
	case model.ConfidenceMedium:
		return "dashed"
	default:
		return "dotted"
	}
}

// quoteDOTID renders s as a DOT double-quoted ID.
//
// Go's %q verb was tried here first and is wrong: %q implements Go string
// literal escaping, not the DOT grammar, and the two disagree on exactly the
// bytes most likely to show up in a real symbol key. That led to a first
// fix — leave `\` alone and escape only `"` — which was itself wrong and is
// the reason this comment now documents a second reasoning pass:
//
//   - Round 1 reasoning (superseded). The argument was: DOT's quoted-string
//     rule treats `\"` as the only recognised escape, so copying `\` through
//     unchanged and escaping only `"` should round-trip correctly, including
//     the case where an input `\` sits immediately before an input `"`
//     (`\"` in, the untouched `\` plus our `\"` reconstitute as `\"` on
//     read-back). That reasoning is correct for backslashes before an
//     embedded quote, but it missed a boundary case: quoteDOTID appends its
//     own closing `"` after the loop. If the input's LAST byte is `\`, that
//     trailing backslash sits immediately before the closing quote WE
//     append, not one from the input — and the DOT lexer cannot tell the
//     difference. `foo\` was rendered as `"foo\"`, which a DOT lexer reads
//     as an escaped quote, not a terminator: the identifier — and the rest
//     of the document — never closes. Any key ending in `\` silently
//     corrupted the whole output file. That is strictly worse than the
//     doubling bug round 1 replaced, because a doubled backslash is a
//     display nuisance in a still-valid document, while an unterminated
//     string can swallow everything after it.
//   - Round 2 fix (current). Escape both bytes that are structurally
//     meaningful to a DOT quoted ID, in a single left-to-right pass:
//     `\` -> `\\`, and `"` -> `\"`. This is the standard backslash-escaping
//     scheme, and it closes the round-1 gap directly: a trailing `\`
//     becomes `\\`, so the byte immediately before the closing `"` is
//     always the second backslash of a completed pair, never a lone
//     backslash reaching for the delimiter. A backslash immediately before
//     an embedded quote (`weird\"key`) still round-trips: it becomes
//     `weird\\\"key`, i.e. an escaped backslash followed by an escaped
//     quote — unambiguous under left-to-right pairing, unlike round 1's
//     `weird\\"key`, which relied on an untouched input backslash pairing
//     correctly by coincidence.
//   - The tradeoff, recorded because it was weighed, not just noticed: a
//     Graphviz reader that treats `\\` as "not a recognised escape, copy
//     both bytes through" (which is what the Graphviz docs describe) will
//     display a literal input backslash as two backslashes on round-trip.
//     That is a display/round-trip nuisance in an otherwise syntactically
//     VALID, parseable document. The alternative — leaving `\` unescaped —
//     risks an UNPARSEABLE document whenever a key ends in `\`. A
//     wrong-but-well-formed document is strictly preferable to a
//     possibly-unparseable one, so this trade is made deliberately in favor
//     of always-valid output.
//   - Control bytes. Go's %q escapes e.g. 0x01 as the four characters
//     `\x01`; DOT has no such escape, so those four characters would be
//     delivered verbatim instead of the original byte. DOT quoted IDs may
//     contain any byte other than an unescaped `"` or an unescaped trailing
//     `\`, including control bytes and embedded newlines, so the correct
//     handling is to pass them through unescaped exactly like any other
//     non-quote, non-backslash byte.
//   - Everything else, including multi-byte UTF-8 (accented letters, CJK,
//     etc.), needs no escaping in either %q or DOT and is left untouched.
//
// This was checked by hand-tracing the DOT lexer's left-to-right greedy `\`
// + next-char pairing against several inputs (bare backslash, double
// trailing backslash, backslash-before-quote, backslash-only key). It was
// NOT checked against a running Graphviz — Graphviz is not installed in
// this environment, so there is no empirical confirmation from the actual
// parser, only hand-verified reasoning about its documented grammar.
func quoteDOTID(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}

// errWriter collapses a run of writes into one error check. Reused by later
// exporters (GraphML, SVG) so each Write body reads as a flat sequence of
// printf calls instead of a chain of `if err != nil` guards.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
