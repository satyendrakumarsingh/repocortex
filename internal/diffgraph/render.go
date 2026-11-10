package diffgraph

import (
	"fmt"
	"io"
	"strings"
)

// Render writes a report as plain text.
//
// It lives here rather than in the CLI for one reason: byte-identical output is
// a property of this package and has to be testable without a terminal. The
// CLI is free to format differently, but if it does, this is the reference for
// what "deterministic" meant.
//
// Every line is derived from the already-sorted sections, so two calls on
// equal reports produce equal bytes.
func Render(w io.Writer, r *Report) error {
	var b strings.Builder

	// The header names the runs by whatever actually identifies them. A commit
	// SHA is the better name when there is one, but a repository indexed outside
	// git has none, and "diff -..-" reads as a broken command rather than as an
	// honest absence — so the run ids stand alone in that case instead of being
	// annotated with two placeholder dashes.
	if r.Base.GitRevision == "" && r.Head.GitRevision == "" {
		fmt.Fprintf(&b, "diff run %d..%d (no commit recorded on either run)\n",
			r.Base.RunID, r.Head.RunID)
	} else {
		fmt.Fprintf(&b, "diff %s..%s (runs %d..%d)\n",
			orNone(r.Base.GitRevision), orNone(r.Head.GitRevision), r.Base.RunID, r.Head.RunID)
	}
	fmt.Fprintf(&b, "  base: status=%s accuracy=%s digest=%s\n",
		r.Base.Status, orNone(r.Base.AccuracyMode), shortDigest(r.Base.Digest))
	fmt.Fprintf(&b, "  head: status=%s accuracy=%s digest=%s\n",
		r.Head.Status, orNone(r.Head.AccuracyMode), shortDigest(r.Head.Digest))

	if r.Empty() {
		b.WriteString("\nno semantic differences\n")
	}

	if len(r.Symbols) > 0 {
		b.WriteString("\nSYMBOLS\n")
		for _, c := range r.Symbols {
			switch c.Change {
			case SymbolMoved:
				if c.FromKey != nil {
					fmt.Fprintf(&b, "  moved    %s -> %s%s\n", *c.FromKey, c.Key, mark(c.Uncertain))
				} else {
					fmt.Fprintf(&b, "  moved    %s  line %d -> %d (%+d)%s\n",
						c.Key, c.From.Start.Line, c.To.Start.Line, c.LineDelta, mark(c.Uncertain))
				}
			case SymbolOrphaned:
				fmt.Fprintf(&b, "  orphaned %s  inbound %d -> 0%s\n", c.Key, c.From.Inbound, mark(c.Uncertain))
			case SymbolAdded:
				fmt.Fprintf(&b, "  added    %s  line %d%s\n", c.Key, c.To.Start.Line, mark(c.Uncertain))
			case SymbolRemoved:
				fmt.Fprintf(&b, "  removed  %s  line %d%s\n", c.Key, c.From.Start.Line, mark(c.Uncertain))
			}
		}
	}

	if len(r.Edges) > 0 {
		b.WriteString("\nEDGES\n")
		for _, c := range r.Edges {
			switch c.Change {
			case EdgeConfidenceChanged:
				fmt.Fprintf(&b, "  %-10s %s  %s -> %s%s\n", c.Direction, c.Key,
					c.From.Confidence, c.To.Confidence, mark(c.Uncertain))
				fmt.Fprintf(&b, "             was: %s\n", renderFact(c.From))
				fmt.Fprintf(&b, "             now: %s\n", renderFact(c.To))
			case EdgeAdded:
				fmt.Fprintf(&b, "  added      %s  %s%s\n", c.Key, renderFact(c.To), mark(c.Uncertain))
			case EdgeRemoved:
				fmt.Fprintf(&b, "  removed    %s  %s%s\n", c.Key, renderFact(c.From), mark(c.Uncertain))
			}
		}
	}

	if len(r.Endpoints) > 0 {
		b.WriteString("\nENDPOINTS\n")
		for _, c := range r.Endpoints {
			switch c.Change {
			case EndpointAdded:
				fmt.Fprintf(&b, "  added    %s -> %s\n", c.Key, handlers(c.To))
			case EndpointRemoved:
				fmt.Fprintf(&b, "  removed  %s -> %s\n", c.Key, handlers(c.From))
			case EndpointHandlerChanged:
				fmt.Fprintf(&b, "  handler  %s: %s -> %s\n", c.Key, handlers(c.From), handlers(c.To))
			}
		}
	}

	if len(r.Tables) > 0 {
		b.WriteString("\nTABLES\n")
		for _, c := range r.Tables {
			switch c.Change {
			case TableAccessAdded:
				fmt.Fprintf(&b, "  added    %s  readers=%d writers=%d\n", c.Table, c.To.Readers, c.To.Writers)
			case TableAccessRemoved:
				fmt.Fprintf(&b, "  removed  %s  readers=%d writers=%d\n", c.Table, c.From.Readers, c.From.Writers)
			case TableAccessChanged:
				fmt.Fprintf(&b, "  changed  %s  readers %d -> %d (%+d), writers %d -> %d (%+d)\n",
					c.Table, c.From.Readers, c.To.Readers, c.ReadersDelta,
					c.From.Writers, c.To.Writers, c.WritersDelta)
			}
		}
	}

	if len(r.Truncations) > 0 {
		b.WriteString("\nOMITTED\n")
		for _, t := range r.Truncations {
			fmt.Fprintf(&b, "  %s: showing %d of %d, %d omitted (ordered by %s)\n",
				t.Section, t.Shown, t.Total, t.Omitted, t.OrderedBy)
		}
	}

	// Always printed, even when empty, because "nothing limited this
	// comparison" is itself a claim the reader is entitled to see made.
	b.WriteString("\nCOULD NOT COMPARE\n")
	if len(r.Caveats.Notes) == 0 {
		b.WriteString("  nothing: both runs were complete and comparable\n")
	}
	for _, n := range r.Caveats.Notes {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	for _, p := range r.Caveats.UnreliablePaths {
		fmt.Fprintf(&b, "    unreliable: %s\n", p)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("diffgraph: render report: %w", err)
	}
	return nil
}

func renderFact(f *EdgeFact) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s/%s at ", f.Confidence, orNone(string(f.Source)))
	for i, s := range f.Sites {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s:%d:%d", s.Path, s.Line, s.Col)
	}
	if f.SiteCount > len(f.Sites) {
		fmt.Fprintf(&sb, " (+%d more sites)", f.SiteCount-len(f.Sites))
	}
	return sb.String()
}

func handlers(f *EndpointFact) string {
	if len(f.Handlers) == 0 {
		return "(handler not revealed by the framework's syntax)"
	}
	return strings.Join(f.Handlers, ", ")
}

func mark(uncertain bool) string {
	if uncertain {
		return "  [uncertain: see COULD NOT COMPARE]"
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortDigest(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return orNone(s)
}
