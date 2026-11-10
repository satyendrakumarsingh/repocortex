package docs

import (
	"sort"
	"strings"
)

// Diagram is one renderable picture plus the evidence behind every part of it.
//
// The evidence is not a footnote bolted on afterwards — it is carried on the
// node and the edge, and Validate refuses a diagram where any part lacks it.
// That refusal is the whole difference between this and a generated picture: an
// arrow a reader cannot check is an assertion, and a page of unverifiable
// assertions is what the competing tools already ship.
type Diagram struct {
	ID    string
	Title string
	Kind  DiagramKind
	Nodes []DiagramNode
	Edges []DiagramEdge
	// Note explains an empty or partial diagram, e.g. "this index contains no
	// IMPLEMENTS or EXTENDS edges". An empty diagram with no note is
	// indistinguishable from a broken generator.
	Note      string
	Omissions []Omission
}

// DiagramKind selects the Mermaid dialect.
type DiagramKind string

const (
	// KindComponentGraph renders declared components and the dependencies
	// between them.
	KindComponentGraph DiagramKind = "component"
	// KindFlow renders one endpoint's path to the data.
	KindFlow DiagramKind = "flow"
	// KindInheritance renders IMPLEMENTS and EXTENDS.
	KindInheritance DiagramKind = "inheritance"
)

// DiagramNode is one box. Shape is chosen by Role.
type DiagramNode struct {
	ID       string
	Label    string
	Role     NodeRole
	Evidence Evidence
}

// DiagramEdge is one arrow.
type DiagramEdge struct {
	From     string
	To       string
	Label    string
	Violates bool
	Evidence Evidence
}

// NodeRole picks a shape so the reader can tell an endpoint from a table
// without reading every label.
type NodeRole string

const (
	RoleComponent NodeRole = "component"
	RoleEndpoint  NodeRole = "endpoint"
	RoleHandler   NodeRole = "handler"
	RoleCall      NodeRole = "call"
	RoleTable     NodeRole = "table"
	RoleType      NodeRole = "type"
)

// Validate reports every part of the diagram that cannot be traced back to the
// index.
//
// Every part, not the first, because this is a correctness invariant of the
// generator rather than user input: if it ever fires, the useful output is the
// full list of what lost its provenance, not one example.
func (d Diagram) Validate() []string {
	var problems []string
	ids := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		if n.ID == "" {
			problems = append(problems, "node with empty id: "+n.Label)
			continue
		}
		if ids[n.ID] {
			problems = append(problems, "duplicate node id "+n.ID)
		}
		ids[n.ID] = true
		if n.Label == "" {
			problems = append(problems, "node "+n.ID+" has no label; a reader cannot look it up")
		}
		if !n.Evidence.Cited() {
			problems = append(problems, "node "+n.ID+" ("+n.Label+") carries no evidence")
		}
	}
	for _, e := range d.Edges {
		if !ids[e.From] {
			problems = append(problems, "edge from unknown node "+e.From)
		}
		if !ids[e.To] {
			problems = append(problems, "edge to unknown node "+e.To)
		}
		if !e.Evidence.Cited() {
			problems = append(problems, "edge "+e.From+" -> "+e.To+" carries no evidence")
		}
	}
	sort.Strings(problems)
	return problems
}

// Mermaid renders the diagram.
//
// Conservative syntax only: `graph`, `flowchart` and `classDiagram` with quoted
// labels. Newer Mermaid spellings are avoided on purpose — a diagram that
// renders only on the latest renderer does not render in the reader's code host,
// and a document whose diagrams show as a wall of text is worse than one with no
// diagrams.
func (d Diagram) Mermaid() string {
	var b strings.Builder
	switch d.Kind {
	case KindInheritance:
		d.writeClassDiagram(&b)
	case KindFlow:
		d.writeGraph(&b, "flowchart LR")
	default:
		d.writeGraph(&b, "graph LR")
	}
	return b.String()
}

func (d Diagram) writeGraph(b *strings.Builder, header string) {
	b.WriteString(header)
	b.WriteByte('\n')
	if len(d.Nodes) == 0 {
		// Mermaid rejects an empty graph. A single labelled node saying why is
		// both valid and the honest rendering of "the index had nothing here".
		b.WriteString("  empty[\"")
		b.WriteString(escapeLabel(emptyNote(d.Note)))
		b.WriteString("\"]\n")
		return
	}
	for _, n := range d.Nodes {
		b.WriteString("  ")
		b.WriteString(n.ID)
		open, close := shapeFor(n.Role)
		b.WriteString(open)
		b.WriteByte('"')
		b.WriteString(escapeLabel(n.Label))
		b.WriteByte('"')
		b.WriteString(close)
		b.WriteByte('\n')
	}
	for _, e := range d.Edges {
		b.WriteString("  ")
		b.WriteString(e.From)
		// A dotted arrow for a forbidden dependency. Mermaid's styling
		// directives are the part that varies most between renderers; arrow
		// SHAPE is in every version of the syntax, so the distinction survives
		// wherever the diagram is read.
		if e.Violates {
			b.WriteString(" -.->")
		} else {
			b.WriteString(" -->")
		}
		if e.Label != "" {
			b.WriteString("|\"")
			b.WriteString(escapeLabel(e.Label))
			b.WriteString("\"|")
		}
		b.WriteByte(' ')
		b.WriteString(e.To)
		b.WriteByte('\n')
	}
}

func (d Diagram) writeClassDiagram(b *strings.Builder) {
	b.WriteString("classDiagram\n")
	b.WriteString("  direction BT\n")
	if len(d.Nodes) == 0 {
		b.WriteString("  class NoInheritanceEdgesInThisIndex\n")
		return
	}
	for _, n := range d.Nodes {
		b.WriteString("  class ")
		b.WriteString(n.ID)
		b.WriteByte('\n')
	}
	for _, e := range d.Edges {
		b.WriteString("  ")
		b.WriteString(e.To) // parent
		if e.Label == "implements" {
			b.WriteString(" <|.. ")
		} else {
			b.WriteString(" <|-- ")
		}
		b.WriteString(e.From) // child
		b.WriteString(" : ")
		b.WriteString(escapeClassLabel(e.Label))
		b.WriteByte('\n')
	}
}

// emptyNote is the LABEL for the placeholder node, which is not the same text as
// the explanation printed beside the diagram.
//
// The note explaining why a section is empty is a full paragraph — it has to be,
// because "no components" and "we could not find components" have opposite
// meanings and the difference takes sentences to state. Rendering that paragraph
// inside a Mermaid node produces a box wider than the page containing prose the
// reader then reads a second time immediately below it. So the diagram gets the
// first sentence, and the prose beneath keeps the argument.
func emptyNote(note string) string {
	if note == "" {
		return "nothing in this index"
	}
	if i := strings.Index(note, ". "); i > 0 && i < len(note)-2 {
		return note[:i+1]
	}
	return note
}

// shapeFor is the bracket pair Mermaid draws for a role.
func shapeFor(r NodeRole) (string, string) {
	switch r {
	case RoleEndpoint:
		return "([", "])" // stadium: the entry point
	case RoleTable:
		return "[(", ")]" // cylinder: a table
	case RoleHandler:
		return "[[", "]]" // subroutine: the thing the route dispatches to
	default:
		return "[", "]"
	}
}

// escapeLabel makes a label safe inside Mermaid's quoted-string form.
//
// `#` goes first, because Mermaid reads `#nnn;` and `#quot;` as entity codes and
// escaping the quote afterwards would otherwise re-mangle it. Newlines and
// backticks are flattened: a label is one line, and a stray backtick inside a
// Markdown fence ends the fence.
func escapeLabel(s string) string {
	r := strings.NewReplacer(
		"#", "#35;",
		"\"", "#quot;",
		"\n", " ",
		"\r", " ",
		"`", "'",
	)
	return strings.TrimSpace(r.Replace(s))
}

// escapeClassLabel is the same job for a classDiagram relationship label, which
// is not quoted and therefore may not contain a colon or a newline.
func escapeClassLabel(s string) string {
	r := strings.NewReplacer(":", " ", "\n", " ", "\r", " ", "`", "'")
	return strings.TrimSpace(r.Replace(s))
}

// idAlloc hands out Mermaid-safe node identifiers that are stable and unique.
//
// Stable because the document is meant to be diffed: an identifier derived from
// a counter would renumber every node below an insertion and turn a one-line
// change into a whole-file diff. Derived from the name instead, so a node keeps
// its identifier as long as its name does not change.
//
// Uniqueness is resolved by suffixing, and the suffix is assigned in the order
// names are requested. Every caller in this package requests them from an
// explicitly sorted slice, which is what makes the suffixes deterministic.
type idAlloc struct {
	taken map[string]string // id -> the key that owns it
}

func newIDAlloc() *idAlloc { return &idAlloc{taken: map[string]string{}} }

// id returns the identifier for key, allocating one from name on first request.
//
// key is what identity means for this node — an FQN, a component name, a table
// name — and name is what the reader sees. They differ because two different
// symbols routinely share a short name, and collapsing them into one box would
// merge two things the graph says are separate.
func (a *idAlloc) id(prefix, key, name string) string {
	base := sanitizeID(name)
	if prefix != "" {
		base = prefix + "_" + base
	}
	if owner, ok := a.taken[base]; ok && owner == key {
		return base
	}
	if _, ok := a.taken[base]; !ok {
		a.taken[base] = key
		return base
	}
	for n := 2; ; n++ {
		cand := base + "_" + itoa(n)
		if owner, ok := a.taken[cand]; ok {
			if owner == key {
				return cand
			}
			continue
		}
		a.taken[cand] = key
		return cand
	}
}

// sanitizeID reduces a name to the identifier characters every Mermaid dialect
// accepts, including classDiagram, which is the strictest of them.
func sanitizeID(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "n"
	}
	// Cap it. FQNs run to hundreds of characters in Java and TypeScript, and a
	// 400-character identifier makes the Mermaid source unreadable without
	// making the diagram any more correct — the label carries the real name.
	if len(out) > 48 {
		out = strings.Trim(out[:48], "_")
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
