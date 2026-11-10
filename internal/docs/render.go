package docs

import (
	"fmt"
	"strings"
)

// The two documents.
//
// Both are Markdown with Mermaid fences, because that is what renders in the
// places these end up: a repository, a pull request, a wiki. Both are pure
// functions of the View, so `docs > ARCHITECTURE.md` in CI and a diff against
// the committed copy is a working architecture-drift gate.
//
// NO TIMESTAMP appears in either body. It is the one line that would make every
// regeneration a diff, and a document whose diff is noise is a document people
// stop reading diffs of. The run id is printed instead: it identifies the index
// exactly, and it only changes when the index does.

// ArchitectureMarkdown renders A27: the component view, the endpoint flows and
// the inheritance view, each with the evidence behind it.
func (v *View) ArchitectureMarkdown() string {
	var b strings.Builder
	b.WriteString("# Architecture\n\n")
	b.WriteString(preamble)
	v.writeIndexSummary(&b)

	b.WriteString("\n## Components\n\n")
	if !v.ComponentsDeclared {
		b.WriteString(noComponentsNote)
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "The repository declares %s and %s in `%s`.\n\n",
			plural(len(v.Components), "component", "components"),
			plural(v.RulesDeclared, "rule", "rules"), v.ConfigPath)
		b.WriteString("| Component | Files | Symbols | Declared include patterns |\n")
		b.WriteString("| --- | ---: | ---: | --- |\n")
		for _, c := range v.Components {
			fmt.Fprintf(&b, "| %s | %d | %d | %s |\n",
				cell(c.Name), c.Files, c.Symbols, cell(joinPatterns(c.Include)))
		}
		b.WriteString("\nA component with 0 files matched nothing: its patterns are wrong, " +
			"and every rule naming it has been silently inert.\n")
	}

	v.writeDiagram(&b, "### Component dependencies", v.ComponentDiagram())
	if v.ComponentsDeclared && len(v.Crossings) > 0 {
		b.WriteString("\n#### Evidence\n\n")
		b.WriteString("| From | To | Edges | Kinds | Weakest confidence | Verdict | Cited at |\n")
		b.WriteString("| --- | --- | ---: | --- | --- | --- | --- |\n")
		for _, cr := range v.Crossings {
			verdict := cr.Verdict
			if cr.RuleID != "" {
				verdict += " (" + cr.RuleID + ")"
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %s | %s |\n",
				cell(cr.From), cell(cr.To), cr.Count, cell(joinPatterns(cr.Kinds)),
				cell(string(cr.Weakest)), cell(verdict), cell(cr.Evidence.String()))
		}
		b.WriteString("\nOne representative citation per dependency — the earliest by file and line " +
			"among the edges behind it — chosen so the table does not move when unrelated code is added.\n")
	}

	b.WriteString("\n## Endpoint flows\n\n")
	if len(v.Endpoints) == 0 {
		b.WriteString("This index contains no endpoints. Either the repository exposes no HTTP routes, " +
			"or no framework this indexer recognises declares them.\n")
	} else {
		fmt.Fprintf(&b, "One diagram per HTTP endpoint, from the route through its handler and the calls "+
			"it makes to the tables it reaches. Calls are followed to a depth of %d.\n",
			v.Limits.MaxCallDepth)
		for _, f := range v.Endpoints {
			v.writeFlow(&b, f)
		}
	}

	b.WriteString("\n## Inheritance\n")
	v.writeDiagram(&b, "", v.InheritanceDiagram())
	if len(v.Inheritance) > 0 {
		b.WriteString("\n| Child | Relationship | Parent | Cited at |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, rel := range v.Inheritance {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
				cell(rel.Child.FQN), cell(strings.ToLower(string(rel.Kind))),
				cell(rel.Parent.FQN), cell(rel.Evidence.String()))
		}
	}

	v.writeOmissions(&b)
	return b.String()
}

// CapabilityMarkdown renders A26: candidate business capabilities.
func (v *View) CapabilityMarkdown() string {
	var b strings.Builder
	b.WriteString("# Candidate business capabilities\n\n")
	b.WriteString(capabilityPreamble)
	v.writeIndexSummary(&b)

	if !v.ComponentsDeclared {
		b.WriteString("\n## No capabilities can be derived\n\n")
		b.WriteString(noComponentsNote)
		b.WriteString("\n\nA capability model is a naming of the declared components by what the graph " +
			"says they expose and touch. With nothing declared there is nothing to name, and naming the " +
			"packages instead would be an inference dressed as a derivation.\n")
		// The endpoints and the tables are still facts, and they are the answer
		// to "what does this system do" that this index CAN give. Printing them
		// unnamed is the honest half of the refusal above: what is missing is a
		// declaration of which capability each belongs to, not the evidence.
		v.writeUnclaimed(&b, "## What the graph knows anyway",
			"Every endpoint and table below came out of an indexer with a citation attached. What no "+
				"declaration supplies is the name of the capability each one serves, so none is given.")
		v.writeOmissions(&b)
		return b.String()
	}

	fmt.Fprintf(&b, "\n%s, one per declared component.\n\n",
		plural(len(v.Capabilities), "candidate capability", "candidate capabilities"))
	b.WriteString("| Capability | Endpoints | Tables | Depends on | Files | Symbols |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |\n")
	for _, c := range v.Capabilities {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d |\n",
			cell(c.Component), len(c.Endpoints), len(c.Tables), len(c.DependsOn), c.Files, c.Symbols)
	}
	v.writeCoverage(&b)

	for _, c := range v.Capabilities {
		fmt.Fprintf(&b, "\n## %s\n\n", c.Component)
		fmt.Fprintf(&b, "Declared in `%s` as `%s`.\n\n", v.ConfigPath, joinPatterns(c.Include))
		if c.Silent {
			b.WriteString("**The graph is silent about this component.** It matches " +
				plural(c.Files, "file", "files") + " and " + plural(c.Symbols, "symbol", "symbols") +
				", but this index attributes no HTTP endpoint, no table access and no dependency on " +
				"another declared component to it. " +
				"That is not evidence it does nothing — it may be a library, a front end, or code whose " +
				"framework this indexer does not recognise — so nothing is claimed about it here.\n")
			continue
		}

		if len(c.Endpoints) > 0 {
			b.WriteString("### Exposes\n\n| Endpoint | Cited at |\n| --- | --- |\n")
			for _, e := range c.Endpoints {
				fmt.Fprintf(&b, "| %s | %s |\n", cell(e.display()), cell(e.Evidence.String()))
			}
			b.WriteString("\n")
		} else {
			b.WriteString("### Exposes\n\nNo HTTP endpoint in this index belongs to this component.\n\n")
		}

		if len(c.Tables) > 0 {
			b.WriteString("### Owns or uses\n\n| Table | Access | Through | Cited at |\n| --- | --- | --- | --- |\n")
			for _, t := range c.Tables {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
					cell(t.Table), cell(t.Access), cell(t.ViaName), cell(t.Evidence.String()))
			}
			b.WriteString("\nAccess is what the graph records, not ownership. A table this component writes " +
				"and another component also writes is shared; see \"Shared data\" below.\n\n")
		} else {
			b.WriteString("### Owns or uses\n\nNo table read or write in this index belongs to this component.\n\n")
		}

		if len(c.DependsOn) > 0 {
			b.WriteString("### Depends on\n\n| Capability | Edges | Kinds | Verdict | Cited at |\n" +
				"| --- | ---: | --- | --- | --- |\n")
			for _, d := range c.DependsOn {
				verdict := d.Verdict
				if d.RuleID != "" {
					verdict += " (" + d.RuleID + ")"
				}
				fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |\n",
					cell(d.To), d.Count, cell(joinPatterns(d.Kinds)), cell(verdict), cell(d.Evidence.String()))
			}
			b.WriteString("\nThese are the component crossings the graph records, in the same order the " +
				"architecture document lists them. A dependency the declared rules forbid is named as such: " +
				"it is still what the code does, which is the point of deriving this rather than writing it.\n\n")
		} else {
			b.WriteString("### Depends on\n\nNo relationship in this index crosses from this component " +
				"into another declared one.\n\n")
		}

		for _, om := range c.Omissions {
			b.WriteString("> Bounded: " + om.String() + "\n")
		}
	}

	b.WriteString("\n## Shared data\n\n")
	if len(v.SharedTables) == 0 {
		b.WriteString("No table in this index is reached by more than one capability. " +
			"That is a statement about what the index resolved, not a guarantee of isolation: " +
			"an access this indexer could not bind to a table is an access this section cannot see.\n")
	} else {
		fmt.Fprintf(&b, "%s reached by more than one capability. Two capabilities writing one table are "+
			"coupled through the database whatever the component boundaries say, so this is the section to "+
			"read before moving either of them.\n\n",
			plural(len(v.SharedTables), "table is", "tables are"))
		b.WriteString("| Table | Capability | Access | Cited at |\n| --- | --- | --- | --- |\n")
		// The table name is repeated on every row of its group rather than
		// blanked after the first. Markdown has no rowspan, and this column
		// already uses an em dash to mean "no value" — reusing it to mean "same
		// as above" would make one glyph mean two things in one table.
		for _, st := range v.SharedTables {
			for _, by := range st.By {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
					cell(st.Table), cell(by.Component), cell(by.Access), cell(by.Evidence.String()))
			}
		}
	}

	v.writeUnclaimed(&b, "\n## Not accounted for by any capability",
		"The capability model above covers what the declarations cover. These are the endpoints and "+
			"tables the index found outside it. Each is either a declaration that needs widening or code "+
			"that genuinely belongs to no capability, and this document cannot tell which — but leaving "+
			"them out would let the model above read as a description of the whole system.")

	v.writeOmissions(&b)
	return b.String()
}

// writeCoverage states how much of the index the capability model accounts for.
//
// A fraction, never a bare percentage: "62% of endpoints" hides whether the
// denominator is eight or eight hundred, and this number is the reader's only
// handle on how much of the document to trust.
func (v *View) writeCoverage(b *strings.Builder) {
	fmt.Fprintf(b, "\nThe capabilities above account for %d of %d %s and %d of %d %s in this index.\n",
		v.Coverage.EndpointsClaimed, v.Coverage.Endpoints,
		noun(v.Coverage.Endpoints, "endpoint", "endpoints"),
		v.Coverage.TablesClaimed, v.Coverage.Tables,
		noun(v.Coverage.Tables, "table", "tables"))
	if v.Coverage.EndpointsClaimed == v.Coverage.Endpoints && v.Coverage.TablesClaimed == v.Coverage.Tables {
		return
	}
	b.WriteString("The remainder is listed at the end of this document rather than dropped: a capability " +
		"model that silently omits what it does not cover reads as a description of the whole system.\n")
}

// writeUnclaimed emits the two unclaimed listings under one heading, or says
// there is nothing to list.
//
// Shared between the declared and the undeclared case because the two documents
// must make the same claim about the same rows; the heading and the framing
// sentence differ, the evidence does not.
func (v *View) writeUnclaimed(b *strings.Builder, heading, intro string) {
	b.WriteString("\n" + strings.TrimPrefix(heading, "\n") + "\n\n")
	if len(v.UnclaimedEndpoints) == 0 && len(v.UnclaimedTables) == 0 {
		b.WriteString("Every endpoint and every table in this index is accounted for.\n")
		return
	}
	b.WriteString(intro + "\n")

	if len(v.UnclaimedEndpoints) > 0 {
		fmt.Fprintf(b, "\n### Endpoints (%d)\n\n", len(v.UnclaimedEndpoints))
		b.WriteString("| Endpoint | Cited at | Why |\n| --- | --- | --- |\n")
		for _, u := range v.UnclaimedEndpoints {
			fmt.Fprintf(b, "| %s | %s | %s |\n",
				cell(u.Symbol.display()), cell(u.Symbol.Evidence.String()), cell(u.Detail))
		}
	}
	if len(v.UnclaimedTables) > 0 {
		fmt.Fprintf(b, "\n### Tables (%d)\n\n", len(v.UnclaimedTables))
		b.WriteString("| Table | Cited at | Why |\n| --- | --- | --- |\n")
		for _, u := range v.UnclaimedTables {
			fmt.Fprintf(b, "| %s | %s | %s |\n",
				cell(u.Symbol.display()), cell(u.Symbol.Evidence.String()), cell(u.Detail))
		}
	}
}

// noun is plural without the count, for sentences that print the number itself.
func noun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

const preamble = "Derived from the RepoCortex index. Every node and every arrow below is a fact " +
	"recorded by an indexer, and every one cites the file and line it came from — nothing here is prose " +
	"about the code.\n\nThe same index regenerates this document byte for byte, so its diff is a real " +
	"diff: the lines that change are the architecture that changed.\n\n"

const capabilityPreamble = "DERIVED from the graph, not inferred from it. A capability here is a " +
	"DECLARED component named by what the index says it exposes, what it touches, and what it needs from " +
	"the other capabilities. Nothing below reads an identifier and decides what business the system is in; " +
	"where the graph is silent, this document says so instead of filling the gap.\n\n" +
	"Three claims a per-capability listing cannot make on its own are made separately, because each is the " +
	"kind of thing a reader would otherwise assume: how much of the index the model accounts for, which " +
	"tables more than one capability reaches, and which endpoints and tables it accounts for not at all.\n\n"

func (v *View) writeIndexSummary(b *strings.Builder) {
	fmt.Fprintf(b, "Index run %d: %d files, %d symbols, %d edges — of which %d EXPOSES, "+
		"%d table read/write, %d IMPLEMENTS/EXTENDS. %d endpoints and %d tables are declared.\n",
		v.RunID, v.Counts.Files, v.Counts.Symbols, v.Counts.Edges,
		v.Counts.ExposesEdges, v.Counts.TableAccessEdg, v.Counts.InheritEdges,
		v.Counts.Endpoints, v.Counts.Tables)
}

func (v *View) writeFlow(b *strings.Builder, f EndpointFlow) {
	fmt.Fprintf(b, "\n### %s\n\n", f.Endpoint.display())
	fmt.Fprintf(b, "Declared at %s.\n", f.Endpoint.Evidence.String())
	v.writeDiagram(b, "", v.FlowDiagram(f))

	b.WriteString("\n| Step | Element | Cited at |\n| --- | --- | --- |\n")
	fmt.Fprintf(b, "| endpoint | %s | %s |\n", cell(f.Endpoint.display()), cell(f.Endpoint.Evidence.String()))
	if f.Handler != nil {
		fmt.Fprintf(b, "| handler | %s | %s |\n", cell(f.Handler.FQN), cell(f.Handler.Evidence.String()))
		fmt.Fprintf(b, "| exposes edge | %s -> %s | %s |\n",
			cell(f.Endpoint.display()), cell(f.Handler.display()), cell(f.Exposes.String()))
	}
	for _, hop := range f.Calls {
		fmt.Fprintf(b, "| calls (depth %d) | %s | %s |\n",
			hop.Depth, cell(hop.Symbol.FQN), cell(hop.Evidence.String()))
	}
	for _, t := range f.Tables {
		fmt.Fprintf(b, "| %s table | %s (via %s) | %s |\n",
			cell(t.Access), cell(t.Table), cell(t.ViaName), cell(t.Evidence.String()))
	}
}

// writeDiagram emits the fence, the note and the bounding disclosure together,
// so a truncated diagram cannot be copied out of the document without the
// sentence saying it is truncated.
func (v *View) writeDiagram(b *strings.Builder, heading string, d Diagram) {
	if heading != "" {
		b.WriteString("\n" + heading + "\n")
	}
	b.WriteString("\n```mermaid\n")
	b.WriteString(d.Mermaid())
	b.WriteString("```\n")
	if d.Note != "" {
		b.WriteString("\n" + d.Note + "\n")
	}
	for _, om := range d.Omissions {
		b.WriteString("\n> Bounded: " + om.String() + "\n")
	}
}

func (v *View) writeOmissions(b *strings.Builder) {
	if len(v.Omissions) == 0 {
		b.WriteString("\n---\n\nNothing was omitted: every section fitted inside its cap.\n")
		return
	}
	b.WriteString("\n---\n\n## What this document leaves out\n\n")
	b.WriteString("Every section is capped so a large repository produces a readable document rather " +
		"than an unreadable one. What the caps dropped:\n\n")
	for _, om := range v.Omissions {
		b.WriteString("- " + om.String() + "\n")
	}
}

// plural renders a count with the right noun.
//
// Worth the six lines: this document is meant to be read by a person, and
// "1 files" in the first sentence is how a reader learns the rest was generated
// carelessly too.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// cell escapes a value for a Markdown table.
//
// A pipe inside a symbol name — Go generics and TypeScript unions both produce
// them — silently splits the row into extra columns, which corrupts every column
// after it. Newlines end the row outright.
func cell(s string) string {
	if s == "" {
		return "—"
	}
	r := strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ")
	return r.Replace(s)
}
