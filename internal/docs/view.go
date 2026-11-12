package docs

import (
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Evidence is the citation carried by every derived fact in this package.
//
// It is a narrowed copy of model.Evidence plus the confidence of the edge or
// symbol it came from, rather than model.Evidence itself, because a citation
// without a confidence is only half a citation: "controller writes payments" is
// a different claim when the edge behind it is compiler-proven than when it was
// guessed from a name, and a document that renders both as the same arrow is
// exactly the unverifiable artifact this package exists to avoid.
//
// The zero value means "no citation", and Cited reports that. Nothing that
// cannot answer Cited is allowed into a diagram; see View.Diagrams.
type Evidence struct {
	FilePath   string
	Line       int
	Source     model.SourceType
	Confidence model.Confidence
	Detail     string
}

// Cited reports whether this evidence can actually be checked by a reader.
//
// A file path is the load-bearing part — it is what someone opens. A line of 0
// is tolerated because several real sources (a config declaration, a table bound
// from a migration) legitimately point at a file rather than a line, and
// demanding a fake line number would make the citation less true, not more.
func (e Evidence) Cited() bool { return e.FilePath != "" && e.Source != "" }

// Location renders the citation the way a reader greps for it.
func (e Evidence) Location() string {
	if e.FilePath == "" {
		return ""
	}
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d", e.FilePath, e.Line)
	}
	return e.FilePath
}

// String is the full citation: where, from what kind of source, how strongly.
func (e Evidence) String() string {
	if !e.Cited() {
		return "(no evidence)"
	}
	var b strings.Builder
	b.WriteString(e.Location())
	b.WriteString(" (")
	b.WriteString(string(e.Source))
	if e.Confidence != "" {
		b.WriteString(", ")
		b.WriteString(string(e.Confidence))
	}
	b.WriteByte(')')
	return b.String()
}

// evidenceOf narrows a model.Evidence, attaching the confidence of the fact it
// justifies.
func evidenceOf(ev model.Evidence, conf model.Confidence) Evidence {
	return Evidence{
		FilePath:   ev.FilePath,
		Line:       ev.Range.Start.Line,
		Source:     ev.Source,
		Confidence: conf,
		Detail:     ev.Detail,
	}
}

// less orders two citations. Used to pick a stable representative among many
// edges making the same crossing, so the document does not change because the
// database returned rows in a different order.
func (e Evidence) less(o Evidence) bool {
	if e.FilePath != o.FilePath {
		return e.FilePath < o.FilePath
	}
	if e.Line != o.Line {
		return e.Line < o.Line
	}
	return e.Detail < o.Detail
}

// Omission records something the caps kept out of the document.
//
// Every truncation produces one and every one is printed. A bounded diagram is
// honest; a bounded diagram that does not say it is bounded reads as a complete
// picture of a system, which is worse than no picture at all.
type Omission struct {
	Subject string // what was truncated, e.g. "component dependencies"
	Shown   int
	Total   int
	Order   string // the rule that decided which ones survived
}

func (o Omission) String() string {
	return fmt.Sprintf("%s: showing %d of %d (%s); %d omitted",
		o.Subject, o.Shown, o.Total, o.Order, o.Total-o.Shown)
}

// SymbolRef is one symbol as a document cites it.
type SymbolRef struct {
	Name      string
	FQN       string
	Kind      model.EntityKind
	Component string // "" when no declared component owns its file
	Evidence  Evidence
}

// display is the label a diagram shows: the name when there is one, the FQN
// otherwise. An anonymous node labelled "" is a node a reader cannot look up.
func (s SymbolRef) display() string {
	if s.Name != "" {
		return s.Name
	}
	return s.FQN
}

// Component is one DECLARED component, with what the index found inside it.
//
// Include and Exclude are echoed from the declaration because they are the
// evidence for the component's existence in a way a file count is not: they are
// the text somebody wrote, and a component whose patterns match nothing is the
// single most useful thing this section can surface.
type Component struct {
	Name     string
	Include  []string
	Exclude  []string
	Files    int
	Symbols  int
	Evidence Evidence // the declaration itself, in the config file
}

// Crossing is one component-to-component dependency, aggregated.
type Crossing struct {
	From    string
	To      string
	Kinds   []string // distinct edge kinds behind it, sorted
	Count   int
	Weakest model.Confidence
	// Verdict and RuleID come from the declared rules. A crossing the rules
	// forbid is drawn differently, because a diagram that renders a violation
	// identically to a permitted dependency has hidden the only part of it
	// anybody needed to see.
	Verdict  string
	RuleID   string
	Evidence Evidence // a stable representative among Count edges
}

// TableRef is one table a piece of code touches, and how.
type TableRef struct {
	Table    string
	Access   string // "reads", "writes" or "reads/writes"
	ViaName  string // the symbol performing the access
	ViaFQN   string
	Evidence Evidence
}

// CallHop is one step along the path from a handler towards the data.
type CallHop struct {
	Symbol   SymbolRef
	FromFQN  string
	Depth    int
	Evidence Evidence // the CALLS edge, not the symbol
}

// EndpointFlow is one HTTP endpoint and everything the graph says it reaches.
//
// Handler is nil when the endpoint has no EXPOSES edge. That happens for real —
// a route whose handler the framework's syntax did not reveal — and the endpoint
// is still shown, because "this endpoint exists and we could not see what serves
// it" is a fact worth printing and inventing an edge to the nearest function
// would be a guess wearing an edge's clothes. internal/pipeline declines to
// write that edge for the same reason.
type EndpointFlow struct {
	Endpoint SymbolRef
	Handler  *SymbolRef
	// Exposes is the EXPOSES edge's own citation, which is a different fact
	// from the handler's declaration site.
	Exposes   Evidence
	Calls     []CallHop
	Tables    []TableRef
	Omissions []Omission
}

// Inheritance is one IMPLEMENTS or EXTENDS relationship.
type Inheritance struct {
	Kind     model.EdgeKind
	Child    SymbolRef
	Parent   SymbolRef
	Evidence Evidence
}

// Capability is one candidate business capability. A26.
//
// DERIVED, not inferred, and the distinction is the whole design. The name is
// the declared component's name — not a name generated from the code — and the
// contents are the endpoints the index says that component exposes and the
// tables the index says it touches. Nothing here reads a method name and decides
// the system does "payments".
//
// Silent is set when a declared component has neither endpoints nor tables in
// the index. That component is still listed, saying exactly that. Dropping it
// would leave a reader believing the model covers the repository when it covers
// the part of the repository with HTTP routes in it.
type Capability struct {
	Component string
	Include   []string
	Endpoints []SymbolRef
	Tables    []TableRef
	// DependsOn is what this capability needs from the others, read straight
	// off the component crossings. A capability described only by what it
	// exposes and what it stores is described as if it were alone in the
	// system, and the first question anyone asks of a capability model is which
	// other capabilities a change to this one reaches.
	DependsOn []CapabilityLink
	Symbols   int
	Files     int
	Silent    bool
	Evidence  Evidence
	Omissions []Omission
}

// CapabilityLink is one capability's dependency on another.
//
// A projection of the Crossing already derived for the architecture document,
// not a second derivation of it: two derivations of the same dependency are two
// chances for the two documents to disagree about the system.
type CapabilityLink struct {
	To       string
	Kinds    []string
	Count    int
	Verdict  string
	RuleID   string
	Evidence Evidence
}

// TableAccess is one capability's access to one table.
type TableAccess struct {
	Component string
	Access    string // "reads", "writes" or "reads/writes"
	Evidence  Evidence
}

// SharedTable is a table more than one capability reaches.
//
// This is the fact a per-capability listing cannot show and the one that most
// often decides whether a capability model is real: two capabilities writing the
// same table are coupled through the database whatever the component diagram
// says, and a reader planning a change needs to see it without cross-referencing
// every section by hand. Derived entirely from the READS/WRITES edges already
// attributed above — nothing here decides that a table "belongs" to anybody.
type SharedTable struct {
	Table string
	By    []TableAccess // sorted by component name
}

// Unclaimed is one endpoint or table the index knows about that no declared
// component accounts for.
//
// The reason this exists: a capability model derived only from what the
// declarations cover reads as a description of the whole system, and a reader
// has no way to see the part it left out. Listing what fell outside turns the
// gap into a finding — either the declarations are incomplete, or that code
// genuinely belongs to nobody, and both are worth knowing. It is also the only
// thing this document can honestly say about a repository that declared no
// components at all: the endpoints are still facts, the capability names are not.
type Unclaimed struct {
	Symbol SymbolRef
	// Detail states WHY it is unclaimed, in one sentence, chosen from a fixed
	// set at derivation time. "Unclaimed" alone is ambiguous between "nothing
	// declares it" and "nothing in the index touches it", and those call for
	// different responses.
	Detail string
}

// Coverage is how much of what the index knows the capability model names.
//
// Printed as a fraction, never as a percentage alone: "62%" of four endpoints is
// a number pretending to be a measurement, and the denominator is the part a
// reader needs in order to judge the rest of the document.
type Coverage struct {
	Endpoints        int
	EndpointsClaimed int
	Tables           int
	TablesClaimed    int
}

// Counts are the raw index totals, printed so a reader can see how much of the
// repository the document is a view of.
type Counts struct {
	Files          int
	Symbols        int
	Edges          int
	Endpoints      int
	Tables         int
	ExposesEdges   int
	InheritEdges   int
	TableAccessEdg int
}

// View is the whole derived model: one pass over the index, two documents.
//
// Architecture (A27) and Capabilities (A26) render from the same View rather
// than each deriving their own, so the two documents can never disagree about
// what the graph contains.
type View struct {
	RunID      int64
	ConfigPath string
	Limits     Limits

	// ComponentsDeclared is false when .repocortex.yml declares no components.
	// The documents then say so instead of inventing a component model.
	ComponentsDeclared bool
	RulesDeclared      int

	Components   []Component
	Crossings    []Crossing
	Endpoints    []EndpointFlow
	Inheritance  []Inheritance
	Capabilities []Capability

	// SharedTables, UnclaimedEndpoints, UnclaimedTables and Coverage complete
	// the capability model: what the capabilities share, what they do not
	// account for, and how much of the index they cover. The three are derived
	// from the same attribution the capabilities are, so they cannot disagree
	// with it.
	//
	// The unclaimed sets and Coverage are computed from the DECLARATIONS, not
	// from the capped Capabilities slice: an endpoint whose capability was
	// dropped by MaxCapabilities is claimed but omitted, which is a different
	// fact from unclaimed, and the omission list is where it is reported.
	SharedTables       []SharedTable
	UnclaimedEndpoints []Unclaimed
	UnclaimedTables    []Unclaimed
	Coverage           Coverage

	Counts    Counts
	Omissions []Omission
}
