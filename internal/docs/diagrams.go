package docs

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ComponentDiagram is the declared component model and the dependencies the
// graph actually contains between its parts.
//
// Returns a diagram with a Note and no nodes when the repository declares no
// components. That is not an empty result to be hidden: "this repository has not
// declared an architecture" is the most useful thing the section can say, and
// the alternative — clustering directories and calling the result the
// architecture — is the inference this package exists to refuse. See the package
// doc, and internal/capability/architecture's, which makes the same argument for
// the checker.
func (v *View) ComponentDiagram() Diagram {
	d := Diagram{
		ID:    "components",
		Title: "Component dependencies",
		Kind:  KindComponentGraph,
	}
	if !v.ComponentsDeclared {
		d.Note = noComponentsNote
		return d
	}

	ids := newIDAlloc()
	nodeID := make(map[string]string, len(v.Components))
	for _, c := range v.Components {
		id := ids.id("c", c.Name, c.Name)
		nodeID[c.Name] = id
		d.Nodes = append(d.Nodes, DiagramNode{
			ID:       id,
			Label:    fmt.Sprintf("%s (%s, %s)", c.Name, plural(c.Files, "file", "files"), plural(c.Symbols, "symbol", "symbols")),
			Role:     RoleComponent,
			Evidence: c.Evidence,
		})
	}
	for _, cr := range v.Crossings {
		from, ok := nodeID[cr.From]
		if !ok {
			continue
		}
		to, ok := nodeID[cr.To]
		if !ok {
			continue
		}
		d.Edges = append(d.Edges, DiagramEdge{
			From:     from,
			To:       to,
			Label:    crossingLabel(cr),
			Violates: cr.Verdict != "allowed",
			Evidence: cr.Evidence,
		})
	}
	if len(d.Edges) == 0 && len(d.Nodes) > 0 {
		d.Note = "The declared components exist, but no relationship in this index crosses a boundary between two of them. " +
			"Either the components do not depend on each other, or the edges that would show it were not resolved."
	}
	for _, om := range v.Omissions {
		if om.Subject == "declared components" || om.Subject == "component dependencies" {
			d.Omissions = append(d.Omissions, om)
		}
	}
	return d
}

// maxDiagramNodes is the hard ceiling on boxes in any one picture.
//
// It is not a Limits field on purpose. The section caps in Limits are an
// editorial choice a caller may reasonably disagree with; this is a property of
// the medium — Mermaid's layout is superlinear, code hosts stop rendering past a
// few hundred nodes, and a reader stops following a graph long before that. A
// caller who raises a section cap should still get a picture, so the picture
// bounds itself and the evidence tables carry the rest.
const maxDiagramNodes = 96

const noComponentsNote = "This repository declares no components. " +
	"RepoCortex does not infer them: a component model it guessed would be a description of the code's " +
	"current shape, so the diagram would be a picture of the code with fewer details, and calling that " +
	"\"the architecture\" is the unverifiable assertion this document exists to avoid. " +
	"Declare an `architecture:` block in the config to get this section."

func crossingLabel(cr Crossing) string {
	label := fmt.Sprintf("%d x %s", cr.Count, joinPatterns(cr.Kinds))
	if cr.Verdict != "allowed" {
		if cr.RuleID != "" {
			return label + " — violates " + cr.RuleID
		}
		return label + " — " + cr.Verdict
	}
	return label
}

// FlowDiagram renders one endpoint's path from route to data.
//
// The shape is fixed — endpoint, handler, calls, tables — because that is the
// question the diagram answers. A general reachability picture out of a handler
// is a different and much less useful drawing.
func (v *View) FlowDiagram(f EndpointFlow) Diagram {
	d := Diagram{
		ID:        "flow_" + sanitizeID(f.Endpoint.display()),
		Title:     f.Endpoint.display(),
		Kind:      KindFlow,
		Omissions: f.Omissions,
	}
	ids := newIDAlloc()
	byFQN := map[string]string{}

	epID := ids.id("ep", f.Endpoint.FQN, f.Endpoint.display())
	d.Nodes = append(d.Nodes, DiagramNode{
		ID: epID, Label: f.Endpoint.display(), Role: RoleEndpoint, Evidence: f.Endpoint.Evidence,
	})

	if f.Handler == nil {
		d.Note = "This route has no EXPOSES edge: the framework's syntax did not reveal which function serves it. " +
			"The endpoint is shown because it is real; no handler is drawn because guessing one would be an " +
			"invented edge, and the indexer declines to write it for the same reason."
		return d
	}

	hID := ids.id("h", f.Handler.FQN, f.Handler.display())
	byFQN[f.Handler.FQN] = hID
	d.Nodes = append(d.Nodes, DiagramNode{
		ID: hID, Label: f.Handler.display(), Role: RoleHandler, Evidence: f.Handler.Evidence,
	})
	d.Edges = append(d.Edges, DiagramEdge{
		From: epID, To: hID, Label: "exposes", Evidence: f.Exposes,
	})

	for _, hop := range f.Calls {
		id := ids.id("s", hop.Symbol.FQN, hop.Symbol.display())
		byFQN[hop.Symbol.FQN] = id
		d.Nodes = append(d.Nodes, DiagramNode{
			ID: id, Label: hop.Symbol.display(), Role: RoleCall, Evidence: hop.Symbol.Evidence,
		})
		from, ok := byFQN[hop.FromFQN]
		if !ok {
			// The caller was dropped by a cap. Attaching the hop to the handler
			// would draw an edge nobody's parser produced, so the hop is left
			// out of the picture instead; it stays in the evidence table.
			d.Nodes = d.Nodes[:len(d.Nodes)-1]
			delete(byFQN, hop.Symbol.FQN)
			continue
		}
		d.Edges = append(d.Edges, DiagramEdge{
			From: from, To: id, Label: "calls", Evidence: hop.Evidence,
		})
	}

	for _, t := range f.Tables {
		tID := ids.id("t", "table:"+t.Table, t.Table)
		if _, seen := byFQN["table:"+t.Table]; !seen {
			byFQN["table:"+t.Table] = tID
			d.Nodes = append(d.Nodes, DiagramNode{
				ID: tID, Label: t.Table, Role: RoleTable, Evidence: t.Evidence,
			})
		}
		from, ok := byFQN[t.ViaFQN]
		if !ok {
			from = hID
		}
		d.Edges = append(d.Edges, DiagramEdge{
			From: from, To: tID, Label: t.Access, Evidence: t.Evidence,
		})
	}
	if len(f.Tables) == 0 {
		d.Note = "No table read or write is reachable from this handler within " +
			itoa(v.Limits.MaxCallDepth) + " calls. The endpoint may not touch the database, or the " +
			"call that would show it was not resolved."
	}
	return d
}

// InheritanceDiagram renders IMPLEMENTS and EXTENDS.
//
// These edge kinds are recent; an index built before they were produced contains
// none at all. An empty diagram here therefore means "this index has none",
// which is a fact about the index and not about the code, and the Note says
// exactly that rather than letting a reader conclude the code has no interfaces.
func (v *View) InheritanceDiagram() Diagram {
	d := Diagram{ID: "inheritance", Title: "Types and their supertypes", Kind: KindInheritance}
	if len(v.Inheritance) == 0 {
		d.Note = "This index contains no IMPLEMENTS or EXTENDS edges. That is a statement about the index, " +
			"not about the code: these edge kinds are produced by the compiler-backed and type-inferring " +
			"resolvers, and an index built without them — or by the name-matching heuristic alone — has none."
		return d
	}
	ids := newIDAlloc()
	seen := map[string]string{}
	add := func(r SymbolRef) string {
		if id, ok := seen[r.FQN]; ok {
			return id
		}
		id := ids.id("", r.FQN, r.display())
		seen[r.FQN] = id
		d.Nodes = append(d.Nodes, DiagramNode{
			ID: id, Label: r.display(), Role: RoleType, Evidence: r.Evidence,
		})
		return id
	}
	drawn := 0
	for _, rel := range v.Inheritance {
		// A node budget as well as an edge cap. Each relationship can introduce
		// two new types, so the edge cap alone permits a diagram twice the size
		// a reader can take in — which is how a "bounded" generator still emits
		// something nobody opens. The evidence table below keeps every
		// relationship that survived the edge cap; only the picture is smaller,
		// and the omission says so.
		if _, haveParent := seen[rel.Parent.FQN]; !haveParent {
			if _, haveChild := seen[rel.Child.FQN]; !haveChild {
				if len(d.Nodes)+2 > maxDiagramNodes {
					break
				}
			} else if len(d.Nodes)+1 > maxDiagramNodes {
				break
			}
		} else if _, haveChild := seen[rel.Child.FQN]; !haveChild && len(d.Nodes)+1 > maxDiagramNodes {
			break
		}
		// Parent first so the node order matches the `direction BT` reading.
		parent := add(rel.Parent)
		child := add(rel.Child)
		label := "extends"
		if rel.Kind == model.EdgeImplements {
			label = "implements"
		}
		d.Edges = append(d.Edges, DiagramEdge{
			From: child, To: parent, Label: label, Evidence: rel.Evidence,
		})
		drawn++
	}
	if drawn < len(v.Inheritance) {
		d.Omissions = append(d.Omissions, Omission{
			Subject: "inheritance relationships drawn",
			Shown:   drawn, Total: len(v.Inheritance),
			Order: fmt.Sprintf("parent name, then child name, until the %d-node diagram budget is spent; "+
				"the evidence table below lists all %d", maxDiagramNodes, len(v.Inheritance)),
		})
	}
	for _, om := range v.Omissions {
		if om.Subject == "inheritance relationships" {
			d.Omissions = append(d.Omissions, om)
		}
	}
	return d
}

// Diagrams returns every diagram the architecture document contains, in
// document order.
//
// Exists so the invariant "every node and edge carries evidence" can be checked
// over the whole document at once rather than diagram by diagram, and so a
// caller rendering to some other format does not have to re-derive the set.
func (v *View) Diagrams() []Diagram {
	out := []Diagram{v.ComponentDiagram()}
	for _, f := range v.Endpoints {
		out = append(out, v.FlowDiagram(f))
	}
	out = append(out, v.InheritanceDiagram())
	return out
}
