package multirepo

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Render writes a report as plain text.
//
// It lives here rather than in the CLI for the reason internal/diffgraph gives
// for the same choice: byte-identical output is a property of this package and
// has to be testable without a terminal. Every line is derived from an
// already-sorted section, so two calls on equal reports produce equal bytes.
//
// The ORDER of the sections is the argument the command makes, and it is not
// negotiable: members first, because a report assembled from a partial estate
// must be disbelieved before it is read; then the edges; then what could not be
// joined; then the caveats. Putting the findings first and the gaps last is how
// a federated view comes to read as a complete one.
func Render(w io.Writer, r *Report) error {
	var b strings.Builder

	renderMembers(&b, r)
	renderProvenance(&b, r)
	renderEdges(&b, r)
	renderTables(&b, r)
	renderUnjoined(&b, r)
	renderCaveats(&b, r)

	_, err := io.WriteString(w, b.String())
	return err
}

func renderMembers(b *strings.Builder, r *Report) {
	available := 0
	for _, m := range r.Members {
		if m.Available {
			available++
		}
	}
	fmt.Fprintf(b, "SYSTEM: %d member repositor%s, %d readable\n",
		len(r.Members), plural(len(r.Members), "y", "ies"), available)

	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  MEMBER\tREVISION\tINDEXED\tENDPOINTS\tOUTBOUND\tTABLES")
	for _, m := range r.Members {
		if !m.Available {
			fmt.Fprintf(w, "  %s\t—\t—\t—\t—\t—\n", m.Name)
			continue
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%d\t%d\t%d\n",
			m.Name, orNone(shortRev(m.GitRevision)), age(m.AgeHours),
			m.Endpoints, m.OutboundCalls, m.Tables)
	}
	_ = w.Flush()

	// Unavailable members are stated in prose beneath the table, not left as a
	// dash in it. A dash is a value; this is a hole in the report.
	for _, m := range r.Members {
		if m.Available {
			continue
		}
		fmt.Fprintf(b, "\n  ! %s IS MISSING FROM THIS GRAPH\n", strings.ToUpper(m.Name))
		fmt.Fprintf(b, "%s\n\n", wrapIndented("      ", "      ", m.Unavailable))
		fmt.Fprintf(b, "%s\n", wrapIndented("      ", "      ",
			"Every count below is a floor, not a total: whatever this member calls, "+
				"exposes or writes is absent from all of them."))
	}
	for _, m := range r.Members {
		if !m.Available || len(m.Warnings) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n  %s (%s)\n", m.Name, orNone(m.IndexPath))
		for _, warn := range m.Warnings {
			fmt.Fprintf(b, "%s\n", wrapIndented("    ! ", "      ", warn))
		}
	}
}

func renderProvenance(b *strings.Builder, r *Report) {
	if len(r.Provenance.Notes) == 0 {
		return
	}
	b.WriteString("\nPROVENANCE\n")
	for _, n := range r.Provenance.Notes {
		fmt.Fprintf(b, "%s\n", wrapIndented("  ! ", "    ", n))
	}
}

func renderEdges(b *strings.Builder, r *Report) {
	fmt.Fprintf(b, "\nCROSS-REPOSITORY EDGES (%d)\n", len(r.Edges))
	if len(r.Edges) == 0 {
		fmt.Fprintf(b, "%s\n", wrapIndented("  ", "  ",
			"No outbound call in any member matched an endpoint declared in another. "+
				"That is not the same as no coupling: see what could not be joined, below."))
		return
	}
	for _, e := range r.Edges {
		fmt.Fprintf(b, "\n  [%s] %s -> %s\n", e.Confidence, e.FromMember, e.ToMember)
		fmt.Fprintf(b, "    %s  %s:%d\n", orNone(e.FromSymbol), e.FromFile, e.FromLine)
		fmt.Fprintf(b, "    calls %s%s\n", callTarget(e.Method, e.Path, e.Dynamic), clientNote(e.Client))
		fmt.Fprintf(b, "    answered by %s  %s:%d\n", e.ToEndpoint, e.ToEndpointFile, e.ToEndpointLine)
		if e.ToHandler != "" {
			fmt.Fprintf(b, "    handler %s  %s\n", e.ToHandler, orNone(e.ToHandlerFile))
		}
		for _, l := range e.Legs {
			mark := " "
			if l.Heuristic {
				mark = "~"
			}
			fmt.Fprintf(b, "      %s %-26s %-6s %s\n", mark, l.Name, l.Confidence, l.Detail)
		}
		fmt.Fprintf(b, "      = %s (the weakest leg; %s)\n", e.Confidence, e.Match)
		if e.Note != "" {
			fmt.Fprintf(b, "%s\n", wrapIndented("      ! ", "        ", e.Note))
		}
	}
}

func renderTables(b *strings.Builder, r *Report) {
	fmt.Fprintf(b, "\nSHARED TABLES (%d)\n", len(r.SharedTables))
	if len(r.SharedTables) == 0 {
		fmt.Fprintf(b, "  No table is named by more than one member.\n")
		return
	}
	for _, t := range r.SharedTables {
		flag := ""
		if t.WriteWrite {
			flag = "  WRITE/WRITE"
		}
		fmt.Fprintf(b, "\n  [%s] %s%s\n", t.Confidence, t.Table, flag)
		for _, p := range t.Participants {
			owns := ""
			if p.Declared {
				owns = " (declares it)"
			}
			fmt.Fprintf(b, "    %s: %d read, %d write, %d access(es)%s\n",
				p.Member, p.Reads, p.Writes, p.Accesses, owns)
		}
		if t.WriteWrite {
			fmt.Fprintf(b, "%s\n", wrapIndented("    ! ", "      ", fmt.Sprintf(
				"%s write this table. They share an invariant that no repository among them states.",
				joinAnd(t.Writers))))
		}
		if t.Note != "" {
			fmt.Fprintf(b, "%s\n", wrapIndented("    ! ", "      ", t.Note))
		}
	}
}

// renderUnjoined is the section that keeps the rest of the report honest, and
// it is never omitted — an empty one prints its zero.
func renderUnjoined(b *strings.Builder, r *Report) {
	u := r.Unjoined
	b.WriteString("\nCOULD NOT JOIN\n")

	if len(u.UnavailableMembers) > 0 {
		fmt.Fprintf(b, "  members unreadable: %d — %s\n",
			len(u.UnavailableMembers), strings.Join(u.UnavailableMembers, ", "))
	}

	fmt.Fprintf(b, "  outbound calls matching no known endpoint: %d\n", len(u.Calls))
	for _, c := range u.Calls {
		fmt.Fprintf(b, "    %s  %s  %s:%d  — %s\n",
			c.Member, callTarget(c.Method, c.Path, c.Dynamic), c.FilePath, c.Line, c.Reason)
	}

	fmt.Fprintf(b, "  outbound calls matching more than one endpoint: %d\n", len(u.AmbiguousCalls))
	for _, c := range u.AmbiguousCalls {
		fmt.Fprintf(b, "    %s  %s  %s:%d\n", c.Member, callTarget(c.Method, c.Path, false), c.FilePath, c.Line)
		for _, cand := range c.Candidates {
			fmt.Fprintf(b, "      could be %s\n", cand)
		}
		fmt.Fprintf(b, "      refused: no router spans two repositories, so these are two answers\n")
	}

	fmt.Fprintf(b, "  endpoints no member of this system calls: %d\n", len(u.Endpoints))
	for _, e := range u.Endpoints {
		fmt.Fprintf(b, "    %s  %s  %s:%d\n", e.Member, e.Endpoint, e.FilePath, e.Line)
	}

	for _, section := range []string{
		"cross_repo_edges", "unjoined_calls", "ambiguous_calls",
		"unconsumed_endpoints", "shared_tables",
	} {
		if n := u.Truncated[section]; n > 0 {
			fmt.Fprintf(b, "  ! %s: %d more not shown (raise the corresponding limit)\n", section, n)
		}
	}
}

func renderCaveats(b *strings.Builder, r *Report) {
	// Deliberately not wrapped into prose paragraphs elsewhere: these are the
	// sentences that keep the whole report honest, and a caveat somebody greps
	// for has to be greppable.
	b.WriteString("\nWHAT THIS IS NOT\n")
	for _, c := range r.Caveats {
		fmt.Fprintf(b, "%s\n", wrapIndented("  ", "  ", c))
	}
	if !r.Complete() {
		fmt.Fprintf(b, "%s\n", wrapIndented("  ", "  ",
			"This graph is INCOMPLETE: "+strings.Join(r.Unjoined.UnavailableMembers, ", ")+
				" could not be read, so every count above is a floor."))
	}
}

// ------------------------------------------------------------------ helpers

func callTarget(method, path string, dynamic bool) string {
	m := method
	if m == "" {
		m = "ANY"
	}
	if dynamic {
		path += "<unresolved>"
	}
	return m + " " + path
}

func clientNote(client string) string {
	if client == "" {
		return ""
	}
	return "  via " + client
}

func shortRev(rev string) string {
	if len(rev) > 10 {
		return rev[:10]
	}
	return rev
}

func age(hours float64) string {
	switch {
	case hours <= 0:
		return "unknown"
	case hours < 48:
		return fmt.Sprintf("%.0fh ago", hours)
	default:
		return fmt.Sprintf("%.0fd ago", hours/24)
	}
}

// joinAnd renders a member list the way a sentence needs it, so a two-member
// coupling does not read as "a and b all write this table".
func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return "no member"
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// wrapWidth is where lines fold. Narrow enough to survive a split terminal,
// which is where an agent's output is usually read.
const wrapWidth = 78

// wrapIndented folds text to wrapWidth, prefixing the first line and indenting
// the rest. It never breaks a word, so a long path stays copyable.
func wrapIndented(prefix, indent, text string) string {
	var b strings.Builder
	line := prefix
	col := len([]rune(prefix))
	for i, word := range strings.Fields(text) {
		n := len([]rune(word))
		if i > 0 && col+1+n > wrapWidth {
			b.WriteString(line)
			b.WriteString("\n")
			line, col = indent, len([]rune(indent))
		} else if i > 0 {
			line += " "
			col++
		}
		line += word
		col += n
	}
	b.WriteString(line)
	return b.String()
}
