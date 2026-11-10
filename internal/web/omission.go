package web

import (
	"encoding/json"
	"sort"
	"strings"
)

// Reasons a view left something out. Machine-readable, because a UI has to
// render a hidden count differently from a truncated list — one is undone by
// lowering a filter, the other by asking a narrower question.
const (
	// ReasonBelowConfidenceFloor: excluded by a confidence threshold. The one
	// a reader undoes by changing a single control, which is why Totals hoists
	// it — as a ROLLUP of these entries, never as a second population.
	ReasonBelowConfidenceFloor = "below_confidence_floor"

	// ReasonNodeBudget / ReasonEdgeBudget: the response body's own caps.
	ReasonNodeBudget = "node_budget"
	ReasonEdgeBudget = "edge_budget"

	// ReasonRowBudget: a listing was shortened.
	ReasonRowBudget = "row_budget"

	// ReasonTraversalBound: internal/graph stopped at MaxDepth or MaxNodes.
	// Distinct from the budgets above: this is work never done, not results
	// discarded after the fact.
	ReasonTraversalBound = "traversal_bound"

	// ReasonEvidenceBudget: rows are present but their file:line evidence was
	// not loaded. The rows are not missing; their proof is.
	ReasonEvidenceBudget = "evidence_budget"

	// ReasonOutsideDeclaredComponents: symbols no declared component matches,
	// so no architecture rule could have been evaluated against them.
	ReasonOutsideDeclaredComponents = "outside_declared_components"

	// ReasonZeroScore: the measure scored the row zero, and zero is a
	// non-answer to that section's question — a node on no shortest path.
	// Only for score-based measures. A RATIO of zero is a reading, not an
	// absence, and must never be hidden under this reason.
	ReasonZeroScore = "zero_score"

	// ReasonRowFilter: removed by a filter the reader themselves set, which is
	// why it is separate from the budgets above — the control that undoes it is
	// one the reader is already holding.
	ReasonRowFilter = "row_filter"

	// ReasonSectionScope: removed by a restriction the SECTION itself applies,
	// not by anything the reader set. Separate from ReasonRowFilter because the
	// two have different remedies and folding them together files one count
	// under the other's label: with both reader filters off, a screen counting
	// its own exported-only restriction as a filter count told readers to clear
	// filters that were already clear, on 7,948 rows at symbol level alone.
	ReasonSectionScope = "section_scope"

	// ReasonIsolatedNode: the node has no relationship in either direction at
	// this level, so a ratio OVER its edge counts has nothing to divide. Not
	// ReasonZeroScore: that reason says a measure looked at the row and
	// answered zero, while this one says the measure had no input at all.
	ReasonIsolatedNode = "isolated_node"

	// ReasonSampledBetweenness: betweenness was estimated from a sample of
	// source nodes rather than every one. Not a filter and not a budget — the
	// rows are all there — but the ORDER of them is approximate, which is
	// something withheld from the reader and so is stated here.
	ReasonSampledBetweenness = "sampled_betweenness"

	// ReasonSelfLoop: edges from a symbol to itself. metrics.NewGraph does not
	// hold one, so direct recursion contributes to no metric.
	ReasonSelfLoop = "self_loop"

	// ReasonIntraGroup: edges between two DISTINCT symbols that a file- or
	// directory-level projection collapses onto one node, so they join no pair
	// of nodes and are counted in no metric at that level. Deliberately not
	// ReasonSelfLoop: a self-loop is recursion the graph model holds at no
	// level, while these are real relationships that reappear at level=symbol.
	ReasonIntraGroup = "intra_group"

	// ReasonCycleLengthBound: cycle enumeration stopped at a length. Recorded
	// with Note, never Hide: how many longer cycles exist is exactly what was
	// not computed, and publishing a count would be inventing one.
	ReasonCycleLengthBound = "cycle_length_bound"

	// ReasonCycleBudget: cycle enumeration stopped at its WORK budget rather
	// than at a length. Distinct from ReasonCycleLengthBound, which says which
	// cycles were never searched for: this one says the search of the lengths
	// that WERE in scope did not finish. Recorded with Note, never Hide, for
	// the same reason — how many cycles a complete walk would have found is
	// precisely what the budget stopped anyone from knowing.
	ReasonCycleBudget = "cycle_budget"

	// ReasonUnresolvedReferences: names the index never bound to a definition.
	// Not a filter and not a bound — a gap in what was knowable — but it is
	// still something the view does not show, so it is stated here too.
	ReasonUnresolvedReferences = "unresolved_references"
)

// Omission is one thing a view left out, stated and counted.
//
// Count is the point. "Some results were filtered" is a disclaimer a reader
// learns to skip; "12 symbols reach this target only through edges weaker than
// `medium`" is a number they can act on, and it is checkable against the same
// query with the floor lowered.
type Omission struct {
	Reason string `json:"reason"`

	// Label is Reason in a sentence a person can read. It exists because the
	// portal rendered the raw snake_case keys — `low_confidence_hidden:`,
	// `row_budget:` — as UI labels, twice on one card with two unrelated
	// meanings. The machine key stays, because a client filtering on it needs a
	// stable identifier; the words a reader sees are now the API's job, so two
	// screens cannot invent two different phrasings for the same reason.
	Label string `json:"label"`

	Count  int    `json:"count"`
	Detail string `json:"detail"`

	// Remedy names the control that undoes this omission, so a reader who needs
	// the wider answer does not have to know the option exists.
	Remedy string `json:"remedy,omitempty"`
}

// OmissionTotal is one reason's ROLLUP over Omitted. Derived, never recorded.
//
// # Why this replaced a hoisted count
//
// Omissions used to carry a top-level `low_confidence_hidden` alongside the
// `below_confidence_floor` entries in Omitted. Both carried 8,015 on the same
// page and the portal rendered them as two lines, so the one number a reader
// most needs to trust — how much of the graph the floor is hiding — read as two
// separate populations totalling 16,030. Nothing in the payload said otherwise,
// because nothing in the payload SAID they were the same thing.
//
// A rollup cannot have that failure mode: Entries states how many recorded
// omissions it sums, Count is their sum, and both are computed at marshal time
// from Omitted. A client that renders Totals and Omitted together is showing a
// subtotal and its rows, which is a shape readers already know how to read. A
// client that renders only one of them is still correct.
type OmissionTotal struct {
	Reason string `json:"reason"`
	Label  string `json:"label"`

	// Count is the sum over every Omitted entry carrying Reason.
	Count int `json:"count"`

	// Entries is how many of those there were. Two entries summing to 8,015 is
	// one population counted in two places in the same view — a graph's floor
	// hiding container edges and symbol edges, say — and the reader can see
	// that rather than infer it.
	Entries int `json:"entries"`
}

// Omissions is the block every response in this package embeds.
//
// # Why this type has a custom marshaller
//
// The requirement is not "handlers should state what they hid" — it is that a
// response CANNOT be published without stating it. A plain struct would let a
// handler that forgot to record a truncation ship a body that reads as
// complete, and the omission would be invisible precisely because the data
// still looks well-formed. So `complete` is never assigned by a handler: it is
// derived at marshal time from what was actually recorded, `omitted` is never
// null, and `totals` is derived from `omitted` rather than maintained beside it
// — the two cannot disagree because there is only one of them.
//
// This mirrors capability.NewCompleteness, which is the single constructor for
// impact and test reports for exactly the same reason.
type Omissions struct {
	// Omitted is every stated omission, in the order recorded. THE TRUTH. Every
	// other number in this block is arithmetic over it.
	Omitted []Omission `json:"omitted"`

	// Totals is the per-reason rollup, derived at marshal time and sorted by
	// reason so two runs of the same query serialise identically. Must not be
	// assigned.
	Totals []OmissionTotal `json:"totals"`

	// Complete is derived at marshal time and must not be assigned.
	Complete bool `json:"complete"`
}

// Hide records an omission.
//
// A zero or negative count is ignored rather than recorded, so a handler can
// call this unconditionally with a computed remainder and not publish
// "0 items were hidden" noise that trains readers to ignore the block.
func (o *Omissions) Hide(reason string, count int, detail, remedy string) {
	if count <= 0 {
		return
	}
	o.Omitted = append(o.Omitted, Omission{
		Reason: reason, Label: OmissionLabel(reason), Count: count, Detail: detail, Remedy: remedy,
	})
}

// Note records an omission that cannot be sized.
//
// Unsized is not the same as absent, and rendering it as absent is the failure
// this whole block exists to prevent. Count stays 0, which the schema documents
// as "unquantified" — the same convention impact.Unknown uses.
func (o *Omissions) Note(reason, detail, remedy string) {
	o.Omitted = append(o.Omitted, Omission{
		Reason: reason, Label: OmissionLabel(reason), Detail: detail, Remedy: remedy,
	})
}

// HiddenByFloor is the confidence floor's own total, for Go callers that need
// the figure without re-deriving it. Same arithmetic the marshaller does.
func (o Omissions) HiddenByFloor() int { return o.totalFor(ReasonBelowConfidenceFloor) }

func (o Omissions) totalFor(reason string) int {
	n := 0
	for _, om := range o.Omitted {
		if om.Reason == reason {
			n += om.Count
		}
	}
	return n
}

// rollup folds Omitted into one entry per reason, ordered by reason so the
// serialisation is byte-identical across runs.
func (o Omissions) rollup() []OmissionTotal {
	if len(o.Omitted) == 0 {
		return []OmissionTotal{}
	}
	index := map[string]int{}
	out := make([]OmissionTotal, 0, len(o.Omitted))
	for _, om := range o.Omitted {
		i, ok := index[om.Reason]
		if !ok {
			index[om.Reason] = len(out)
			out = append(out, OmissionTotal{Reason: om.Reason, Label: OmissionLabel(om.Reason)})
			i = len(out) - 1
		}
		out[i].Count += om.Count
		out[i].Entries++
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reason < out[j].Reason })
	return out
}

// MarshalJSON emits the block with `omitted` always an array, `totals` always
// derived from it, and `complete` always recomputed. See the type comment for
// why none of that is left to handlers.
func (o Omissions) MarshalJSON() ([]byte, error) {
	type alias Omissions // breaks the recursion into this method
	a := alias(o)
	if a.Omitted == nil {
		a.Omitted = []Omission{}
	}
	for i := range a.Omitted {
		// A handler that built an Omission literally rather than through Hide
		// still gets the label. The API's words are the API's job.
		if a.Omitted[i].Label == "" {
			a.Omitted[i].Label = OmissionLabel(a.Omitted[i].Reason)
		}
	}
	a.Totals = Omissions(a).rollup()
	a.Complete = len(a.Omitted) == 0
	return json.Marshal(a)
}

// OmissionLabel is the one place a reason becomes words.
//
// Exported because the CLI and the portal must not phrase the same omission
// two different ways, and because a reason with no label here is a reason
// somebody added without deciding what it means to a reader — which is what
// the fallback makes visible instead of hiding behind the raw key.
func OmissionLabel(reason string) string {
	switch reason {
	case ReasonBelowConfidenceFloor:
		return "hidden below the standard of proof"
	case ReasonNodeBudget:
		return "nodes beyond the drawing budget"
	case ReasonEdgeBudget:
		return "relationships beyond the response budget"
	case ReasonRowBudget:
		return "rows beyond this listing's budget"
	case ReasonTraversalBound:
		return "not reached before the traversal stopped"
	case ReasonZeroScore:
		return "scored zero on this measure"
	case ReasonRowFilter:
		return "removed by the filters you set"
	case ReasonSectionScope:
		return "outside this section's own restriction"
	case ReasonEvidenceBudget:
		return "rows whose file:line evidence was not loaded"
	case ReasonOutsideDeclaredComponents:
		return "outside every declared component"
	case ReasonIsolatedNode:
		return "nodes with no relationship at this level"
	case ReasonSampledBetweenness:
		return "source nodes not sampled when ranking betweenness"
	case ReasonSelfLoop:
		return "direct recursion the graph model cannot hold"
	case ReasonIntraGroup:
		return "relationships collapsed inside a single node at this level"
	case ReasonCycleLengthBound:
		return "cycles longer than the enumeration bound"
	case ReasonCycleBudget:
		return "cycles a complete enumeration would have found"
	case ReasonUnresolvedReferences:
		return "references the index never bound"
	default:
		return strings.ReplaceAll(reason, "_", " ")
	}
}

// capped shortens a list to limit and RECORDS the remainder.
//
// Every truncation in this package goes through here. Re-slicing in a handler
// would compile and would look identical in the response body, which is exactly
// why the shortening and the accounting are the same call.
func capped[T any](in []T, limit int, o *Omissions, reason, detail, remedy string) []T {
	if limit <= 0 || len(in) <= limit {
		return in
	}
	o.Hide(reason, len(in)-limit, detail, remedy)
	return in[:limit]
}
