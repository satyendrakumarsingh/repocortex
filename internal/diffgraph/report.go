package diffgraph

import (
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ChangeKind names one class of semantic difference. Grouping is by the THING
// that changed — a symbol, an edge, an endpoint, a table — not by the file it
// lives in, because "this endpoint now writes refunds" is the sentence a
// reviewer needs and "12 lines changed in PaymentController.java" is not.
type ChangeKind string

const (
	SymbolAdded    ChangeKind = "symbol_added"
	SymbolRemoved  ChangeKind = "symbol_removed"
	SymbolMoved    ChangeKind = "symbol_moved"
	SymbolOrphaned ChangeKind = "symbol_orphaned" // lost its last inbound reference

	EdgeAdded             ChangeKind = "edge_added"
	EdgeRemoved           ChangeKind = "edge_removed"
	EdgeConfidenceChanged ChangeKind = "edge_confidence_changed"

	EndpointAdded          ChangeKind = "endpoint_added"
	EndpointRemoved        ChangeKind = "endpoint_removed"
	EndpointHandlerChanged ChangeKind = "endpoint_handler_changed"

	TableAccessAdded   ChangeKind = "table_access_added"
	TableAccessRemoved ChangeKind = "table_access_removed"
	TableAccessChanged ChangeKind = "table_access_changed"
)

// Direction records which way a confidence transition went. `exact -> high` is
// a downgrade and is the interesting one: a compiler stopped proving a
// relationship that it used to prove, and no line-based tool can see that.
type Direction string

const (
	Upgraded   Direction = "upgraded"
	Downgraded Direction = "downgraded"
)

// Site is one piece of evidence for an edge fact.
type Site struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Col    int    `json:"col"`
	Detail string `json:"detail,omitempty"`
}

// SymbolFact is everything the diff reports ABOUT a symbol, as opposed to the
// part of it that establishes identity (SymbolKey).
type SymbolFact struct {
	Path       string           `json:"path"`
	FQN        string           `json:"fqn"`
	Kind       model.EntityKind `json:"kind"`
	Name       string           `json:"name"`
	Start      model.Position   `json:"start"`
	End        model.Position   `json:"end"`
	Signature  string           `json:"signature,omitempty"`
	Exported   bool             `json:"exported"`
	Confidence model.Confidence `json:"confidence"`
	Source     model.SourceType `json:"source"`
	// Inbound counts distinct referencing edge facts pointing at this symbol.
	// Structural containment and provenance edges are excluded — see
	// isReferencingEdge.
	Inbound int `json:"inbound"`
}

// EdgeFact is everything the diff reports ABOUT a relationship.
//
// Confidence is the STRONGEST of the edges that collapsed into this fact. If a
// compiler proves one of two call sites, the relationship is proven; reporting
// the weaker value would understate what is known.
type EdgeFact struct {
	Confidence model.Confidence `json:"confidence"`
	Source     model.SourceType `json:"source"`
	Sites      []Site           `json:"sites,omitempty"` // bounded sample, sorted
	SiteCount  int              `json:"site_count"`
}

// EndpointFact is the mutable half of an endpoint.
type EndpointFact struct {
	Line int `json:"line"`
	// Handlers is the sorted set of "path:name" handlers the route exposes.
	// A set rather than a scalar because a route may resolve to more than one
	// EXPOSES edge, and an empty set is meaningful: the route is real, its
	// target simply was not revealed by the framework's syntax.
	Handlers []string `json:"handlers,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// TableFact aggregates one table's access profile across the whole run.
type TableFact struct {
	// Declared is false for a table this repository uses but does not own.
	Declared  bool `json:"declared"`
	Readers   int  `json:"readers"`
	Writers   int  `json:"writers"`
	Accesses  int  `json:"accesses"`
	Uncertain int  `json:"uncertain"`
}

// SymbolChange is one difference about one symbol.
type SymbolChange struct {
	Change ChangeKind `json:"change"`
	Key    SymbolKey  `json:"key"`
	// From and To are the evidence on each side. Exactly one is nil for an
	// addition or a removal; both are set for a move or an orphaning.
	From *SymbolFact `json:"from,omitempty"`
	To   *SymbolFact `json:"to,omitempty"`
	// LineDelta is set for a move: how far the definition shifted. A
	// cross-file move also sets FromKey.
	LineDelta int        `json:"line_delta,omitempty"`
	FromKey   *SymbolKey `json:"from_key,omitempty"`
	// Uncertain marks a change whose file did not parse cleanly in one of the
	// runs, so the difference may be an indexing gap rather than a code change.
	Uncertain bool   `json:"uncertain,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// EdgeChange is one difference about one relationship.
type EdgeChange struct {
	Change    ChangeKind `json:"change"`
	Key       EdgeKey    `json:"key"`
	From      *EdgeFact  `json:"from,omitempty"`
	To        *EdgeFact  `json:"to,omitempty"`
	Direction Direction  `json:"direction,omitempty"`
	Uncertain bool       `json:"uncertain,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// EndpointChange is one difference about one route.
type EndpointChange struct {
	Change ChangeKind    `json:"change"`
	Key    EndpointKey   `json:"key"`
	From   *EndpointFact `json:"from,omitempty"`
	To     *EndpointFact `json:"to,omitempty"`
}

// TableChange is one difference about one table's access profile.
type TableChange struct {
	Change       ChangeKind `json:"change"`
	Table        string     `json:"table"`
	From         *TableFact `json:"from,omitempty"`
	To           *TableFact `json:"to,omitempty"`
	ReadersDelta int        `json:"readers_delta,omitempty"`
	WritersDelta int        `json:"writers_delta,omitempty"`
}

// RunInfo is the identifying half of one side of the comparison.
type RunInfo struct {
	RunID        int64             `json:"run_id"`
	Status       model.IndexStatus `json:"status"`
	GitRevision  string            `json:"git_revision,omitempty"`
	AccuracyMode string            `json:"accuracy_mode,omitempty"`
	Digest       string            `json:"digest,omitempty"`
	Files        int64             `json:"files"`
	Symbols      int64             `json:"symbols"`
	Edges        int64             `json:"edges"`
	// ParseFailures is why a missing fact may not be a code change.
	ParseFailures int64 `json:"parse_failures"`
}

// Caveats is the report's statement of what it could NOT compare.
//
// This section is not decoration and it is not optional. If one run was
// `degraded`, a missing edge may be an indexing gap rather than a deleted call.
// If the two runs used different accuracy modes, every confidence transition in
// the report may describe the indexer rather than the code — a run with no
// compiler index cannot emit `exact` at all. A tool that presented those two
// situations identically to a clean comparison would be guessing.
type Caveats struct {
	// Comparable is false when a run is in a state where absence proves
	// nothing at all (failed, or still indexing).
	Comparable bool `json:"comparable"`
	// DigestsEqual means the two runs hashed identically under
	// registry.RegistryDigest, in which case the change sections MUST be
	// empty; if they are not, the comparison itself is wrong and Notes says so.
	DigestsEqual bool `json:"digests_equal"`
	// Notes are human-readable, emitted in a fixed order.
	Notes []string `json:"notes,omitempty"`
	// UnreliablePaths are files whose parse status was not "ok" in at least
	// one run. Bounded; UnreliableFiles is the true count.
	UnreliablePaths []string `json:"unreliable_paths,omitempty"`
	UnreliableFiles int      `json:"unreliable_files"`
	// DanglingEdges counts edges whose endpoint symbol was not present in the
	// same run. They are excluded from the comparison rather than silently
	// counted as removals.
	DanglingEdges int `json:"dangling_edges,omitempty"`
}

// Truncation records that a section was capped, and by how much. A bounded
// report that did not say so would be indistinguishable from a complete one.
type Truncation struct {
	Section   string `json:"section"`
	Shown     int    `json:"shown"`
	Total     int    `json:"total"`
	Omitted   int    `json:"omitted"`
	OrderedBy string `json:"ordered_by"`
}

// Report is the whole comparison. Identical inputs produce a byte-identical
// Report and a byte-identical Render.
type Report struct {
	Base RunInfo `json:"base"`
	Head RunInfo `json:"head"`

	Symbols   []SymbolChange   `json:"symbols,omitempty"`
	Edges     []EdgeChange     `json:"edges,omitempty"`
	Endpoints []EndpointChange `json:"endpoints,omitempty"`
	Tables    []TableChange    `json:"tables,omitempty"`

	Caveats     Caveats      `json:"caveats"`
	Truncations []Truncation `json:"truncations,omitempty"`
}

// Empty reports whether the two runs are semantically identical. Diffing a run
// against itself must return true; that is the single property this whole
// package is worthless without.
func (r *Report) Empty() bool {
	return len(r.Symbols) == 0 && len(r.Edges) == 0 &&
		len(r.Endpoints) == 0 && len(r.Tables) == 0
}
