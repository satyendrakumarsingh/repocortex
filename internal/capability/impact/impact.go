// Package impact answers "what breaks if I change this?" over the shared code
// knowledge graph.
//
// This is ImpactMind from the product spec, implemented as a query layer rather
// than a separate product: it owns no schema, parses nothing, and every fact it
// reports comes from internal/graph and internal/registry with its provenance
// intact.
//
// # The honesty requirements are the feature
//
// An impact analysis is only worth having if a developer can act on it, and
// they can only act on it if it is calibrated. Under-report and they change the
// symbol confidently and break production. Over-report and they learn to skip
// the tool. Three design decisions follow from that, and none of them is
// negotiable:
//
//  1. DIRECT, INDIRECT and UNKNOWN are separate sets, never one list. Spec §20.
//     A depth-1 caller is a different claim from something reachable at depth 3
//     through two medium-confidence edges, which is a different claim again
//     from "this symbol implements an interface and we cannot see who calls
//     through it". Merging them into one "impacted" count destroys exactly the
//     information the reader needs.
//
//  2. Confidence compounds along the path; it never resets. internal/graph
//     already records the MINIMUM confidence along the path that reached each
//     node, and this package surfaces that number unchanged. Reporting a
//     depth-3 result as "high" because the last hop happened to be high is a
//     lie, and it is the specific lie that makes an impact tool dangerous.
//
//  3. Nothing here claims completeness. Every Report carries a
//     capability.Completeness with the run's unresolved and ambiguous reference
//     counts, and states plainly that the result is a lower bound. The
//     mechanisms static analysis cannot see — dynamic dispatch, reflection,
//     dependency injection — are named, not gestured at.
//
//  4. Dispatch is EXPANDED but never merged. A call to an interface method
//     reaches every implementation at run time, and a caller graph stops at the
//     interface — so when an IMPLEMENTS or EXTENDS edge records the
//     relationship, this package walks it and reports what it found in
//     Report.Dispatch, labelled. It does not put those symbols in Direct. An
//     implementation is a symbol the runtime MAY execute when something names
//     the target; a direct caller is a recorded reference to it. Both are worth
//     reporting and they are not the same claim, so the caller walk runs over
//     CALLS/REFERENCES/DEPENDS_ON/READS/WRITES only and the subtype edges drive
//     a second, separately labelled expansion.
//
//  5. The traversal has a confidence floor by default, and says what the floor
//     hid. docs/accuracy.md measures `low` edges at 0.0–2.8% precision, so a
//     default report that walked them would be mostly wrong — 97 of every 100
//     `low` edges name a relationship that does not exist. The floor is
//     DefaultMinConfidence, and the edges below it are still in the database
//     and still one explicit MinConfidence away; what changed is which set the
//     report presents without being asked. Report.LowConfidenceHidden and the
//     below_confidence_floor unknown size what the floor kept out, so the
//     narrower blast radius is stated rather than silently smaller.
//
// # Risk scoring
//
// Risk is reported with its inputs. A bare "Risk: HIGH" is unactionable: the
// reader cannot tell whether it fired because the symbol is public API, because
// it has 300 callers, or because nothing tests it — and those call for
// different responses. Every Risk therefore carries the Factors that produced
// the score, each with its own points and the evidence behind it, plus a
// separate Certainty describing how much the score itself should be trusted.
// Spec §2.8.
//
// # Bounds, and measured performance
//
// Traversal is bounded explicitly (Options) and truncation is always reported,
// never silent.
//
// From bench_test.go on a 100,000-symbol / 503,491-edge fixture (Apple
// M-series, -benchtime 50x), p95 wall time for one complete Analyze — target
// resolution, traversal, evidence, unknowns, risk and completeness. Both
// columns are from the same machine and the same fixture, differing only in
// whether the confidence floor of design decision 4 is in force:
//
//	                                             unfloored -> floored
//	ordinary symbol (4 callers), depth 1 / 2 / 3   0.25 / 0.47 / 0.81 ms
//	                                            -> 0.52 / 0.79 / 1.70 ms
//	hub symbol (1,002 callers),  depth 1 / 2 / 3   2.03 / 11.2 / 10.6 ms
//	                                            -> 4.30 / 18.1 / 18.1 ms
//
// The floor costs about 2x, and it is worth knowing exactly what buys that.
// Filtering weak edges is free — internal/graph does it in the WHERE clause —
// but COUNTING what the filter removed is not, because an edge excluded in SQL
// was never seen by the walk. LowConfidenceHidden is therefore measured by
// walking a second time without the floor (countHiddenByFloor). A default query
// pays for a report that states its own filter; a caller who passes
// MinConfidence: low explicitly pays nothing extra, because nothing is hidden
// from that walk and the probe is skipped.
//
// Sweeping the node budget on the hub at depth 3, floored, shows what actually
// governs the cost — MaxNodes, not depth, exactly as internal/graph documents:
//
//	MaxNodes    500 ->   2.1 ms  (  499 impacted)
//	MaxNodes  2,000 ->  19.6 ms  (1,999 impacted)  <- the default
//	MaxNodes 10,000 -> 105.1 ms  (9,999 impacted)
//	MaxNodes 50,000 -> 301.5 ms  (26,221 impacted)
//
// One cost is deliberately excluded from those figures and measured on its own:
// hydrating the symbol table. The first query after an index swap pays 334.2 ms
// p95 to build a 100k-entry index; every subsequent query against the same run
// reuses it (capability.Deps). Folding that into the numbers above would
// misdescribe a long-lived server, and omitting it entirely would misdescribe a
// cold one, so BenchmarkAnalyzeColdSymbolIndex reports it separately. The floor
// does not touch this one — it is one traversal-free query against symbols.
//
// A second fixed cost was found the same way and removed rather than published:
// registry.ResolutionStats is a GROUP BY over every reference row in the run,
// and at 45,600 rows it cost 7.1 ms — 95% of a small query's total, paid again
// on every call to recompute a number that cannot change once a run is
// committed. It is now memoised per run alongside the symbol index, which is
// what took the ordinary-symbol p95 from 8.4 ms to sub-millisecond.
package impact

import (
	"context"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Default bounds. Small on purpose: an impact query runs in front of an
// interactive agent, and a bounded answer that says it is bounded beats an
// unbounded one that arrives too late to use. See internal/graph's package
// comment for why MaxNodes rather than depth is the knob that matters.
const (
	DefaultMaxDepth = 3
	DefaultMaxNodes = 2000

	// DefaultMaxDirect bounds how many direct callers are reported WITH
	// evidence. Above it the evidence lookup is skipped entirely rather than
	// loading a hub symbol's entire fan-in to then throw most of it away.
	DefaultMaxDirect = 200

	// maxUnknownProbes bounds how many supertypes are examined for the
	// dynamic-dispatch unknown. A symbol implementing more interfaces than this
	// is unusual, and the unknown is reported qualitatively anyway.
	maxUnknownProbes = 8
)

// DefaultMinConfidence is the confidence floor a traversal gets when the caller
// does not choose one.
//
// medium rather than low because of the measurement, not a preference:
// docs/accuracy.md adjudicates 30,975 heuristic edges against compiler-backed
// indexes and puts `low` at 0.0–2.8% precision across Go, TypeScript and
// Python. An edge that is wrong ~97% of the time is not weak evidence of
// impact, it is noise with a symbol name attached, and a default blast radius
// built mostly from noise is the thing that teaches a developer to stop reading
// the report.
//
// This is a floor on the DEFAULT WALK, not a judgement that the edges are
// worthless: `MinConfidence: model.ConfidenceLow` restores every one of them,
// nothing is deleted from the index, and what the floor excluded is counted in
// Report.LowConfidenceHidden rather than dropped in silence.
const DefaultMinConfidence = model.ConfidenceMedium

// DefaultKinds are the edge kinds that mean "a change here can affect there".
//
// CONTAINS is deliberately absent. It is the structural parent edge — file
// contains symbol, package contains file — so including it would make every
// symbol "impact" its own file, then its package, then everything in it, and
// the report would say that changing anything affects everything. That is true
// and useless.
var DefaultKinds = []model.EdgeKind{
	model.EdgeCalls,
	model.EdgeReferences,
	model.EdgeImplements,
	model.EdgeExtends,
	model.EdgeDependsOn,
	model.EdgeReads,
	model.EdgeWrites,
}

// Options bounds one analysis.
type Options struct {
	// MaxDepth is how many hops out from the target to walk. <= 0 uses
	// DefaultMaxDepth.
	MaxDepth int

	// MaxNodes caps the traversal result. <= 0 uses DefaultMaxNodes. Hitting it
	// sets Report.Truncated and is stated in the completeness caveats.
	MaxNodes int

	// MaxDirect caps evidence-bearing direct callers. <= 0 uses
	// DefaultMaxDirect.
	MaxDirect int

	// MinConfidence excludes weaker edges from the walk entirely. Unset uses
	// DefaultMinConfidence.
	//
	// This used to be left at the zero value — no threshold — on the argument
	// that filtering weak edges out silently shrinks the reported blast radius.
	// The premise still holds, and it is the SILENTLY that the floor answers,
	// not the filtering: a walk over `low` edges reports a wider radius made
	// mostly of relationships that do not exist (0.0–2.8% precision,
	// docs/accuracy.md), which shrinks the blast radius a reader can act on
	// while inflating the one printed. So the walk is floored, the excluded
	// edges stay in the index, and Report.LowConfidenceHidden plus the
	// below_confidence_floor unknown state how much the floor kept out and how
	// to get it back.
	//
	// An explicit model.ConfidenceLow is honoured and admits everything, which
	// is what makes this a default rather than a deletion.
	MinConfidence model.Confidence

	// Kinds restricts traversed edge kinds. Empty uses DefaultKinds.
	Kinds []model.EdgeKind

	// HubDegreeLimit refuses to expand THROUGH a symbol wider than this many
	// edges. Zero, the default, disables it and is what every existing caller
	// gets: an optional bound must not change an answer nobody asked to change.
	//
	// It is a bound on the WALK, not on the report. A depth-3 impact walk that
	// crosses a logging helper reaches most of the repository and reports a
	// blast radius that is technically correct and useless; trimming that at
	// render time still pays for the walk and keeps whichever rows arrived
	// first. The pruned symbol is still reported — it is genuinely impacted —
	// and internal/graph names the refusal in the truncation reason, which
	// reaches Report.TruncatedReason unchanged.
	HubDegreeLimit int
}

func (o Options) withDefaults() Options {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = DefaultMaxNodes
	}
	if o.MaxDirect <= 0 {
		o.MaxDirect = DefaultMaxDirect
	}
	// Valid() is false for the zero value and for anything unrecognised, which
	// is the same test internal/graph applies before building its confidence
	// filter. An explicitly weak floor (low) is valid and survives untouched.
	if !o.MinConfidence.Valid() {
		o.MinConfidence = DefaultMinConfidence
	}
	if len(o.Kinds) == 0 {
		o.Kinds = DefaultKinds
	}
	return o
}

// Impacted is one symbol a change to the target could affect.
type Impacted struct {
	capability.Symbol

	// Depth is hops from the target. 1 for direct.
	Depth int `json:"depth"`

	// Confidence is the MINIMUM confidence along the path from the target to
	// this symbol — how strongly the REACHABILITY is believed, not how strongly
	// the final hop is. A path high -> low -> high reports low.
	Confidence model.Confidence `json:"confidence"`

	// Via is the edge kind of the final hop that reached this symbol.
	Via model.EdgeKind `json:"via,omitempty"`

	// Evidence is the source location(s) proving the relationship. Populated
	// for direct impact only, and only when the fan-in was small enough to load
	// (see Report.DirectEvidenceOmitted). Indirect impact has no single
	// location that justifies it, so claiming one would be a fabrication.
	Evidence []model.Evidence `json:"evidence,omitempty"`

	// IsTest marks the symbol as test code, so a reader can tell "40 callers"
	// from "40 callers, 38 of them tests".
	IsTest bool `json:"is_test"`

	// cost is graph.Node.Cost for the path that reached this symbol: the key
	// the traversal ranked by, carried so a caller that merges several
	// traversals can rank the union the same way rather than inventing a
	// second order. Unexported for the same reason graph keeps it unexported —
	// the units are arbitrary and it is not a score to publish.
	//
	// Zero means "not produced by a caller traversal". Dispatch rows are built
	// from a separate walk and leave it unset; every traversal-derived row is
	// at depth 1 or more and so costs at least 1.
	cost int
}

// DispatchReason names why a symbol appears in the dispatch expansion rather
// than among the callers.
//
// Two reasons, and they are different claims: one symbol IS an implementation
// of the target, the other is merely reachable from one. Both are weaker than
// "references the target", which is why neither is allowed into Direct.
type DispatchReason string

const (
	// ReasonImplementsTarget: the symbol implements or extends the target, so a
	// call that names the target can execute this symbol instead. The edge is
	// recorded; the dispatch is inferred from it.
	ReasonImplementsTarget DispatchReason = "implements_target"

	// ReasonThroughImplementation: the symbol was reached from one of those
	// implementations. Every path to it from the target crosses a subtype edge,
	// so it is dispatch reach, not caller reach.
	ReasonThroughImplementation DispatchReason = "reachable_through_implementation"
)

// Dispatched is one symbol reached by expanding through a declared subtype
// relationship.
//
// It is a separate type from Impacted, in a separate field of Report, for the
// same reason Direct and Indirect are separate: they are different claims. A
// direct caller is a recorded reference to the target. An implementation is a
// symbol the language runtime MAY execute when something names the target — the
// IMPLEMENTS edge proves the relationship exists, and nothing proves any
// particular call site takes it. Merging the two would let a possibility be
// read as a fact, and that is precisely the overclaim this package exists to
// prevent.
type Dispatched struct {
	Impacted

	// Reason distinguishes the implementation itself from what it reaches.
	Reason DispatchReason `json:"reason"`

	// Detail states the claim in one sentence, so a reader who sees only this
	// row still knows it is a dispatch claim.
	Detail string `json:"detail"`
}

// UnknownKind names a class of impact static analysis cannot determine.
type UnknownKind string

const (
	// UnknownDynamicDispatch: the target implements or extends something, so
	// callers of the supertype may reach this implementation at run time. The
	// index records the IMPLEMENTS edge but cannot record which call sites
	// actually dispatch here.
	UnknownDynamicDispatch UnknownKind = "dynamic_dispatch"

	// UnknownUnexpandedFrontier: the traversal stopped at a bound with nodes
	// still queued. Whatever lies beyond them is missing from this report.
	UnknownUnexpandedFrontier UnknownKind = "unexpanded_frontier"

	// UnknownUnresolvedReferences: the run contains references that were never
	// bound to any definition. Any of them could be a use of the target.
	UnknownUnresolvedReferences UnknownKind = "unresolved_references"

	// UnknownBelowConfidenceFloor: symbols the walk reached only across edges
	// weaker than Options.MinConfidence. They are excluded from Direct and
	// Indirect because at `low` precision most of them are not really impacted
	// — but "most" is not "none", so they are reported as a sized gap rather
	// than dropped.
	UnknownBelowConfidenceFloor UnknownKind = "below_confidence_floor"

	// UnknownStaleIndex: the index run being queried was not built at either
	// end of the range being analysed, or the two could not be compared. Every
	// count in the report is then about code that is not the code in the diff —
	// which is the one failure in this package that produces a confident wrong
	// answer rather than a small one.
	UnknownStaleIndex UnknownKind = "stale_index"

	// UnknownUnindexedChanges: the diff changed files, and the index holds a
	// symbol for none of them. Occasionally true (a docs-only commit); also
	// exactly what a path-space mismatch between the git root and the index
	// root looks like, and the two are indistinguishable from here.
	UnknownUnindexedChanges UnknownKind = "unindexed_changes"

	// UnknownPathSpace: a DiffRequest named an index root, and the rebasing
	// that root exists to drive could not be performed. Distinct from
	// UnknownUnindexedChanges, which is a property of the diff: this one is a
	// property of the CALLER'S WIRING, and it can produce a partly-correct
	// report — some paths matching, others silently in the wrong space — which
	// never trips the all-unindexed guard at all.
	UnknownPathSpace UnknownKind = "path_space"
)

// Unknown is a stated gap in the analysis.
//
// These are not warnings appended for politeness. Each is a specific, counted
// region of the answer that is missing, and each carries the evidence that it
// exists — which is what makes it actionable rather than a disclaimer.
type Unknown struct {
	Kind UnknownKind `json:"kind"`

	// Detail explains the gap in one sentence.
	Detail string `json:"detail"`

	// Count sizes the gap where it can be sized. Zero means "unquantified",
	// which is itself information.
	Count int64 `json:"count,omitempty"`

	// CountIsLowerBound marks a Count that was itself measured under a bound.
	CountIsLowerBound bool `json:"count_is_lower_bound,omitempty"`

	// Related names the symbols the gap hangs off — the interfaces implemented,
	// for dynamic dispatch.
	Related []capability.Symbol `json:"related,omitempty"`
}

// Report is one impact analysis.
//
// Direct, Indirect and Unknown are three separate fields rather than one list
// with a depth column, because they are three different claims and a consumer
// that flattens them has thrown away the distinction the report exists to make.
type Report struct {
	Target capability.Target `json:"target"`

	// Direct is depth 1: symbols that reference the target themselves. The
	// strongest claim this report makes.
	Direct []Impacted `json:"direct"`

	// Indirect is depth >= 2: reachable through other symbols. Ordered by
	// depth then symbol id.
	Indirect []Impacted `json:"indirect"`

	// Dispatch is the interface-dispatch expansion: symbols reachable only
	// because a call naming the target can land on an implementation at run
	// time. Ordered by depth then symbol id.
	//
	// Direct and Indirect are walked over CALLS/REFERENCES/DEPENDS_ON/READS/
	// WRITES only, so nothing in them arrived across a subtype edge. Everything
	// that did is here, labelled — an implementation of a changed interface
	// method is a real finding and must be reported, but it is not the same
	// claim as "this code calls the target" and is not printed as one.
	//
	// The expansion is bounded by the same Options.MaxDepth (counted from the
	// target, so the subtype hop spends one of them) and by its own
	// Options.MaxNodes budget, and its truncation is merged into
	// Report.Truncated rather than kept where the risk score would not see it.
	Dispatch []Dispatched `json:"dispatch,omitempty"`

	// DispatchThrough names the implementations the expansion went through, so
	// the reader can see the hinge of the claim rather than inferring it from
	// the rows.
	DispatchThrough []capability.Symbol `json:"dispatch_through,omitempty"`

	// Unknown is what could not be determined. Never empty in practice — the
	// resolution gap alone guarantees an entry on any real index.
	Unknown []Unknown `json:"unknown"`

	// Risk is the score WITH its inputs.
	Risk Risk `json:"risk"`

	// Completeness is the mandatory honesty block.
	Completeness capability.Completeness `json:"completeness"`

	// Truncated and TruncatedReason mirror the traversal's own bound reporting
	// so a consumer does not have to dig into Completeness to find it.
	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncated_reason,omitempty"`

	// DirectEvidenceOmitted says the direct callers are reported without
	// file:line evidence because there were too many to load. Stated rather
	// than silently returning evidence-free results.
	DirectEvidenceOmitted bool   `json:"direct_evidence_omitted,omitempty"`
	DirectEvidenceReason  string `json:"direct_evidence_reason,omitempty"`

	// ConfidenceFloor is the floor the walk actually ran with, after defaults.
	// Printed with the report so the numbers above are interpretable without
	// knowing which options the caller passed.
	ConfidenceFloor model.Confidence `json:"confidence_floor,omitempty"`

	// LowConfidenceHidden counts the symbols an identical walk WITHOUT the
	// floor reaches that this report does not list, because every path to them
	// crosses an edge weaker than ConfidenceFloor.
	//
	// It exists so the floor is a stated filter rather than an invisible one: a
	// reader who sees 4 direct callers and 12 hidden knows both that the
	// reported radius is narrower than the graph's, and that widening it means
	// re-asking with MinConfidence: low — not that 12 relationships were
	// deleted. Zero means the floor changed nothing.
	//
	// A count of symbols, not of edges: a symbol reached by three sub-floor
	// edges is one thing the reader might have to look at, not three, and the
	// traversal port (capability.Graph) reports nodes.
	LowConfidenceHidden int `json:"low_confidence_hidden,omitempty"`
}

// TotalImpacted is the number of symbols reported as affected THROUGH RECORDED
// REFERENCES. A lower bound, like everything else here.
//
// It deliberately excludes the dispatch expansion: those symbols are reported
// as possibly reached, and folding them into the same total would restore the
// conflation Report.Dispatch exists to remove. TotalReached is the number that
// includes them.
func (r Report) TotalImpacted() int { return len(r.Direct) + len(r.Indirect) }

// TotalReached is every symbol this report names as affected or possibly
// affected, callers and dispatch alike.
func (r Report) TotalReached() int { return r.TotalImpacted() + len(r.Dispatch) }

// Analyze reports what a change to target could break.
//
// target is a symbol name or a fully-qualified name. A bare name matching
// several definitions returns *capability.AmbiguousTargetError with the
// candidates rather than guessing, because an impact analysis of the wrong
// symbol is not a partial answer.
func Analyze(ctx context.Context, deps capability.Deps, runID int64, target string) (Report, error) {
	return AnalyzeWith(ctx, deps, runID, target, Options{})
}

// AnalyzeWith is Analyze under explicit bounds.
func AnalyzeWith(ctx context.Context, deps capability.Deps, runID int64, target string, opts Options) (Report, error) {
	if err := deps.Validate(); err != nil {
		return Report{}, err
	}
	opts = opts.withDefaults()

	tgt, err := capability.ResolveTarget(ctx, deps, runID, target)
	if err != nil {
		return Report{}, err
	}
	return analyzeTarget(ctx, deps, runID, tgt, opts)
}

// analyzeTarget is AnalyzeWith once the subject is already a resolved symbol.
//
// It is split out for the caller that has one: AnalyzeDiff seeds from symbols
// it read out of the symbol index by file path, and forcing those back through
// ResolveTarget would re-query the registry for an answer it already holds —
// and, worse, could fail with AmbiguousTargetError on a name the diff never
// asked about by name in the first place.
//
// opts must already have been through withDefaults; every caller here does that
// before resolving, because a resolution failure should not depend on which
// bounds were requested.
func analyzeTarget(ctx context.Context, deps capability.Deps, runID int64,
	tgt capability.Target, opts Options) (Report, error) {
	idx, err := deps.SymbolIndex(ctx, runID)
	if err != nil {
		return Report{}, err
	}

	// The caller walk and the dispatch expansion are two traversals because they
	// are two claims. Walking CALLS and IMPLEMENTS together — which is what this
	// used to do, back when no IMPLEMENTS edge existed to notice — produces one
	// undifferentiated list in which an implementation of the target sits at
	// depth 1 looking exactly like a symbol that calls it.
	callerKinds, subKinds := splitKinds(opts.Kinds)

	// One traversal produces both the direct and the indirect set. Splitting it
	// into a depth-1 query plus a depth-N query would double the work and, worse,
	// could disagree with itself: a node reachable at both depth 1 and depth 3
	// must appear exactly once, at its shallowest depth, and only a single
	// visited set guarantees that.
	var res graph.Result
	if len(callerKinds) > 0 {
		// Empty Kinds means "all kinds" to internal/graph, so a caller who asked
		// for subtype edges ONLY must not reach this call — it would silently
		// walk everything.
		res, err = deps.Graph.Traverse(ctx, graph.Query{
			Roots:          []int64{tgt.SymbolID},
			Kinds:          callerKinds,
			MinConfidence:  opts.MinConfidence,
			MaxDepth:       opts.MaxDepth,
			MaxNodes:       opts.MaxNodes,
			HubDegreeLimit: opts.HubDegreeLimit,
			// Incoming: changing a symbol affects the things that USE it, not the
			// things it uses.
			Direction: graph.Incoming,
		})
		if err != nil {
			return Report{}, fmt.Errorf("impact: traverse from %s: %w", tgt.FQN, err)
		}
	}

	rep := Report{
		Target:          tgt,
		Truncated:       res.Truncated,
		TruncatedReason: res.TruncatedReason,
		ConfidenceFloor: opts.MinConfidence,
	}

	var frontier int
	for _, n := range res.Nodes {
		if n.SymbolID == tgt.SymbolID && n.Depth == 0 {
			continue // the target is not impacted by itself
		}
		sym, ok := idx.Lookup(n.SymbolID)
		if !ok {
			// A node id with no symbol row means the graph and the symbol table
			// disagree, which doctor --verify-index exists to catch. Report the
			// id rather than dropping it: silently losing an impacted symbol is
			// the failure mode this package must not have.
			sym = capability.Symbol{ID: n.SymbolID, Name: fmt.Sprintf("symbol#%d", n.SymbolID)}
		}
		imp := Impacted{
			Symbol:     sym,
			Depth:      n.Depth,
			Confidence: n.Confidence,
			Via:        n.Via,
			IsTest:     capability.IsTestSymbol(sym.Kind, sym.Name, sym.FilePath),
			cost:       n.Cost(),
		}
		if n.Depth == 1 {
			rep.Direct = append(rep.Direct, imp)
		} else {
			rep.Indirect = append(rep.Indirect, imp)
		}
		if n.Depth == opts.MaxDepth {
			frontier++
		}
	}
	// NOT re-sorted. res.Nodes arrives ordered cheapest-first — accumulated
	// EdgeCost, then depth, then id (graph.Result.finish) — so a symbol reached
	// through a chain of proofs outranks one reached through a chain of guesses
	// even when the guesses got there in fewer hops. Appending in that order
	// preserves it; the (Depth, ID) sort that used to run here discarded it and
	// handed the reader an ordering keyed on a database rowid, which is exactly
	// the ranking internal/graph/cost.go exists to replace. It is still fully
	// deterministic: the traversal's own comparator is a total order.

	if err := attachDirectEvidence(ctx, deps, &rep, opts); err != nil {
		return Report{}, err
	}

	// reported is every symbol already claimed as a caller. The dispatch
	// expansion never re-lists one of them: a symbol that both calls the target
	// AND implements it is reported under the stronger claim.
	reported := map[int64]struct{}{tgt.SymbolID: {}}
	for _, list := range [][]Impacted{rep.Direct, rep.Indirect} {
		for _, imp := range list {
			reported[imp.ID] = struct{}{}
		}
	}

	disp, err := expandDispatch(ctx, deps, idx, tgt, subKinds, reported, opts)
	if err != nil {
		return Report{}, err
	}
	rep.Dispatch, rep.DispatchThrough = disp.rows, disp.through
	for _, d := range rep.Dispatch {
		reported[d.ID] = struct{}{}
	}
	// A bound hit by either walk makes every count in the report a floor, so
	// truncation is merged rather than kept in a corner the risk score and the
	// completeness block would not see.
	res.Truncated, res.TruncatedReason = mergeTruncation(
		res.Truncated, res.TruncatedReason, disp.truncated, disp.truncatedReason)
	rep.Truncated, rep.TruncatedReason = res.Truncated, res.TruncatedReason

	unknowns, err := collectUnknowns(ctx, deps, idx, tgt, res, frontier, opts)
	if err != nil {
		return Report{}, err
	}
	rep.Unknown = unknowns

	hidden, hiddenBounded, err := countHiddenByFloor(ctx, deps, tgt, reported, res.Truncated, opts)
	if err != nil {
		return Report{}, err
	}
	rep.LowConfidenceHidden = hidden
	if hidden > 0 {
		rep.Unknown = append(rep.Unknown, belowFloorUnknown(opts.MinConfidence, hidden, hiddenBounded))
	}

	stats, err := deps.ResolutionStats(ctx, runID)
	if err != nil {
		return Report{}, err
	}
	rep.Unknown = append(rep.Unknown, unresolvedUnknown(stats))
	rep.Completeness = capability.NewCompleteness(stats, idx.Len(), res.Truncated, res.TruncatedReason)
	if len(rep.Dispatch) > 0 {
		// Appended, not substituted: NewCompleteness has already put the two
		// mandatory caveats in place, and this one only says what the extra
		// section means. It is added exactly when there is a dispatch section to
		// explain.
		rep.Completeness.Caveats = append(rep.Completeness.Caveats, capability.DispatchExpansionCaveat)
	}
	rep.Risk = scoreRisk(rep)
	return rep, nil
}

// splitKinds separates the edge kinds that mean "uses the target" from the ones
// that mean "is a subtype of the target".
//
// The split is the whole mechanism behind Report.Dispatch. Traversing both sets
// in one walk cannot distinguish them afterwards, because graph.Node records
// only the LAST hop's kind: a symbol three hops away, reached across an
// IMPLEMENTS edge and then two CALLS edges, reports Via=CALLS and is
// indistinguishable from a plain caller chain.
func splitKinds(kinds []model.EdgeKind) (caller, subtype []model.EdgeKind) {
	for _, k := range kinds {
		switch k {
		case model.EdgeImplements, model.EdgeExtends:
			subtype = append(subtype, k)
		default:
			caller = append(caller, k)
		}
	}
	return caller, subtype
}

func mergeTruncation(aTrunc bool, aReason string, bTrunc bool, bReason string) (bool, string) {
	switch {
	case aTrunc && bTrunc && aReason != bReason:
		return true, aReason + "; " + bReason
	case aTrunc:
		return true, aReason
	case bTrunc:
		return true, bReason
	default:
		return false, aReason
	}
}

// dispatchResult is the outcome of one dispatch expansion.
type dispatchResult struct {
	rows            []Dispatched
	through         []capability.Symbol
	truncated       bool
	truncatedReason string
}

// expandDispatch walks the implementations of the target and what they reach.
//
// # Why this is a second walk and not a wider first one
//
// The caller walk answers "what references the target". This answers a
// different question — "what can run instead of, or because of, the target when
// something calls it through an interface" — and the answers must not be
// concatenated. Running it from the implementations as ROOTS, rather than
// letting the main traversal cross subtype edges, is what makes every row here
// provably dispatch-reached: the expansion starts on the far side of the
// subtype edge, so nothing that could have been reached without crossing one
// can appear (those symbols are in `reported` already and are skipped).
//
// # Direction
//
// IMPLEMENTS is recorded src=implementor, dst=interface, so implementations are
// found on INCOMING subtype edges. That is the asymmetry that makes this
// expansion possible at all in one direction and impossible in the other:
// asking "who implements this interface" is an edge lookup, while asking "which
// call site dispatches to this implementation" is not answerable from the
// index, and remains the UnknownDynamicDispatch gap rather than becoming a row
// here.
//
// # Confidence
//
// A dispatch row's confidence is the weaker of the subtype edge and the path
// from the implementation. With several implementations sharing one traversal
// the exact subtype edge behind a deep row is not recoverable, so the WEAKEST
// implementation edge is used for all of them. That under-claims when
// implementations differ in edge strength, which is the same one-directional
// bias internal/graph documents: never report a path as stronger than it is.
func expandDispatch(
	ctx context.Context,
	deps capability.Deps,
	idx *capability.SymbolIndex,
	tgt capability.Target,
	subKinds []model.EdgeKind,
	reported map[int64]struct{},
	opts Options,
) (dispatchResult, error) {
	var out dispatchResult
	if len(subKinds) == 0 {
		return out, nil
	}

	impls, err := deps.Graph.Neighbours(ctx, tgt.SymbolID, graph.Incoming, subKinds, opts.MinConfidence)
	if err != nil {
		return out, fmt.Errorf("impact: implementations of %s: %w", tgt.FQN, err)
	}
	if len(impls) == 0 {
		// No recorded subtype relationship, so there is nothing to expand. The
		// report simply carries no dispatch section — an empty labelled set
		// would suggest the question was asked and answered "none", which is a
		// stronger statement than "no edge said so".
		return out, nil
	}

	type implInfo struct {
		conf     model.Confidence
		via      model.EdgeKind
		evidence []model.Evidence
	}
	byID := make(map[int64]*implInfo, len(impls))
	order := make([]int64, 0, len(impls))
	weakest := model.Confidence("")
	for _, n := range impls {
		info, ok := byID[n.SymbolID]
		if !ok {
			info = &implInfo{conf: n.Confidence, via: n.Kind}
			byID[n.SymbolID] = info
			order = append(order, n.SymbolID)
		}
		// Several edges to the same implementation: keep the strongest as the
		// claim, and every location as evidence.
		if n.Confidence.Rank() > info.conf.Rank() {
			info.conf, info.via = n.Confidence, n.Kind
		}
		info.evidence = append(info.evidence, n.Evidence)
		weakest = weaker(weakest, n.Confidence)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	roots := make([]int64, 0, len(order))
	for _, id := range order {
		roots = append(roots, id)
		sym, ok := idx.Lookup(id)
		if !ok {
			sym = capability.Symbol{ID: id, Name: fmt.Sprintf("symbol#%d", id)}
		}
		out.through = append(out.through, sym)
		if _, dup := reported[id]; dup {
			// Already claimed as a caller. It stays a traversal root — what it
			// reaches is still dispatch reach — but it is not re-listed under the
			// weaker claim.
			continue
		}
		info := byID[id]
		out.rows = append(out.rows, Dispatched{
			Impacted: Impacted{
				Symbol:     sym,
				Depth:      1,
				Confidence: info.conf,
				Via:        info.via,
				Evidence:   info.evidence,
				IsTest:     capability.IsTestSymbol(sym.Kind, sym.Name, sym.FilePath),
			},
			Reason: ReasonImplementsTarget,
			Detail: fmt.Sprintf(
				"%s %s %s. A call that names %s can execute this implementation at run time; "+
					"the subtype edge is recorded, the dispatch to it is not.",
				sym.Name, verbFor(info.via), tgt.Name, tgt.Name),
		})
	}

	// Hops still available after the one spent crossing the subtype edge. Depth
	// is counted from the TARGET throughout the report, so the expansion must
	// not quietly buy itself an extra level.
	remaining := opts.MaxDepth - 1
	if remaining <= 0 {
		sortDispatched(out.rows)
		return out, nil
	}

	res, err := deps.Graph.Traverse(ctx, graph.Query{
		Roots:          roots,
		Kinds:          opts.Kinds,
		MinConfidence:  opts.MinConfidence,
		MaxDepth:       remaining,
		MaxNodes:       opts.MaxNodes,
		HubDegreeLimit: opts.HubDegreeLimit,
		Direction:      graph.Incoming,
	})
	if err != nil {
		return out, fmt.Errorf("impact: dispatch expansion from implementations of %s: %w", tgt.FQN, err)
	}
	out.truncated, out.truncatedReason = res.Truncated, res.TruncatedReason
	if res.Truncated && res.TruncatedReason != "" {
		out.truncatedReason = "dispatch expansion: " + res.TruncatedReason
	}

	for _, n := range res.Nodes {
		if n.Depth == 0 {
			continue // the implementations themselves, already listed
		}
		if _, dup := reported[n.SymbolID]; dup {
			continue
		}
		if _, dup := byID[n.SymbolID]; dup {
			continue
		}
		sym, ok := idx.Lookup(n.SymbolID)
		if !ok {
			sym = capability.Symbol{ID: n.SymbolID, Name: fmt.Sprintf("symbol#%d", n.SymbolID)}
		}
		out.rows = append(out.rows, Dispatched{
			Impacted: Impacted{
				Symbol:     sym,
				Depth:      n.Depth + 1,
				Confidence: weaker(weakest, n.Confidence),
				Via:        n.Via,
				IsTest:     capability.IsTestSymbol(sym.Kind, sym.Name, sym.FilePath),
			},
			Reason: ReasonThroughImplementation,
			Detail: fmt.Sprintf(
				"reached %d hop(s) beyond an implementation of %s. Every path from %s to it crosses a subtype "+
					"edge, so it is dispatch reach rather than a caller of %s.",
				n.Depth, tgt.Name, tgt.Name, tgt.Name),
		})
	}
	sortDispatched(out.rows)
	return out, nil
}

// verbFor renders the subtype edge in the sentence a reader expects.
func verbFor(k model.EdgeKind) string {
	if k == model.EdgeExtends {
		return "extends"
	}
	return "implements"
}

// weaker returns the lower-ranked of two confidences, treating the empty value
// as absent rather than as the weakest possible.
func weaker(a, b model.Confidence) model.Confidence {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case a.Rank() <= b.Rank():
		return a
	default:
		return b
	}
}

func sortDispatched(in []Dispatched) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Depth != in[j].Depth {
			return in[i].Depth < in[j].Depth
		}
		return in[i].ID < in[j].ID
	})
}

// rankImpacted imposes the order graph.Result.finish already produced, on a
// slice that lost it.
//
// analyzeTarget never calls this: its rows arrive from one traversal, already
// ranked, and re-sorting them is exactly the bug this replaced. The diff path
// does, because it unions one traversal per changed symbol through a map and a
// Go map has no order at all. The keys are finish()'s keys, in finish()'s
// order, so a merged report ranks the way each seed's report did rather than
// by a second rule the other surface does not use.
func rankImpacted(in []Impacted) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].cost != in[j].cost {
			return in[i].cost < in[j].cost
		}
		if in[i].Depth != in[j].Depth {
			return in[i].Depth < in[j].Depth
		}
		return in[i].ID < in[j].ID
	})
}

// attachDirectEvidence fills in file:line for depth-1 callers.
//
// Only when the fan-in is small. graph.Neighbours has no limit parameter — it
// returns one row per EDGE — so calling it on a hub symbol with 100,000 callers
// would materialise all of them just to keep 200. The traversal has already
// been bounded, so its depth-1 count is a safe proxy for deciding whether the
// unbounded call is affordable, and when it is not the report says so instead
// of quietly shipping evidence-free rows that look identical to bounded ones.
func attachDirectEvidence(ctx context.Context, deps capability.Deps, rep *Report, opts Options) error {
	if len(rep.Direct) == 0 {
		return nil
	}
	if len(rep.Direct) > opts.MaxDirect {
		rep.DirectEvidenceOmitted = true
		rep.DirectEvidenceReason = fmt.Sprintf(
			"%d direct callers exceeds the evidence budget of %d; symbols are listed without file:line "+
				"because loading every edge for a symbol with this fan-in is not bounded work",
			len(rep.Direct), opts.MaxDirect)
		return nil
	}

	ns, err := deps.Graph.Neighbours(ctx, rep.Target.SymbolID, graph.Incoming, opts.Kinds, opts.MinConfidence)
	if err != nil {
		return fmt.Errorf("impact: direct callers of %s: %w", rep.Target.FQN, err)
	}
	byID := make(map[int64][]model.Evidence, len(ns))
	for _, n := range ns {
		byID[n.SymbolID] = append(byID[n.SymbolID], n.Evidence)
	}
	for i := range rep.Direct {
		rep.Direct[i].Evidence = byID[rep.Direct[i].ID]
	}
	return nil
}

// collectUnknowns enumerates the gaps this analysis knows it has.
//
// Only gaps that can be justified from the graph are emitted. "There might be
// reflection somewhere" is a caveat, and lives in Completeness; "this symbol
// implements interface X, and X has callers we cannot attribute" is a counted
// unknown with an edge behind it, and lives here.
func collectUnknowns(
	ctx context.Context,
	deps capability.Deps,
	idx *capability.SymbolIndex,
	tgt capability.Target,
	res graph.Result,
	frontier int,
	opts Options,
) ([]Unknown, error) {
	var out []Unknown

	if res.Truncated {
		out = append(out, Unknown{
			Kind: UnknownUnexpandedFrontier,
			Detail: "The traversal stopped before exhausting the reachable set (" + res.TruncatedReason +
				"). Symbols beyond that boundary are affected-status unknown, not unaffected.",
			Count:             int64(frontier),
			CountIsLowerBound: true,
		})
	}

	// Dynamic dispatch: does the target participate in a subtype relationship?
	supers, err := deps.Graph.Neighbours(ctx, tgt.SymbolID, graph.Outgoing,
		[]model.EdgeKind{model.EdgeImplements, model.EdgeExtends}, "")
	if err != nil {
		return nil, fmt.Errorf("impact: supertypes of %s: %w", tgt.FQN, err)
	}
	if len(supers) > 0 {
		seen := make(map[int64]struct{}, len(supers))
		var related []capability.Symbol
		var viaSuper int64
		var bounded bool
		for _, s := range supers {
			if _, dup := seen[s.SymbolID]; dup {
				continue
			}
			seen[s.SymbolID] = struct{}{}
			if sym, ok := idx.Lookup(s.SymbolID); ok {
				related = append(related, sym)
			}
			if len(seen) > maxUnknownProbes {
				bounded = true
				continue
			}
			// Count callers of the supertype under a bound. Any of them may
			// dispatch to this implementation at run time; none of them can be
			// proven to.
			sr, err := deps.Graph.Traverse(ctx, graph.Query{
				Roots:     []int64{s.SymbolID},
				Kinds:     []model.EdgeKind{model.EdgeCalls, model.EdgeReferences},
				MaxDepth:  1,
				MaxNodes:  opts.MaxDirect,
				Direction: graph.Incoming,
			})
			if err != nil {
				return nil, fmt.Errorf("impact: callers of supertype %d: %w", s.SymbolID, err)
			}
			if n := len(sr.Nodes) - 1; n > 0 { // minus the root
				viaSuper += int64(n)
			}
			if sr.Truncated {
				bounded = true
			}
		}
		out = append(out, Unknown{
			Kind: UnknownDynamicDispatch,
			Detail: fmt.Sprintf(
				"%s implements or extends %d type(s). Callers that go through the supertype dispatch at run time, "+
					"so static analysis cannot tell which of them actually reach this implementation. "+
					"Treat these as possibly affected.",
				tgt.Name, len(related)),
			Count:             viaSuper,
			CountIsLowerBound: bounded,
			Related:           related,
		})
	}
	return out, nil
}

// countHiddenByFloor sizes what Options.MinConfidence kept out of the walk.
//
// The floor lives in internal/graph's SQL, which filters weak edges before they
// are ever expanded — that is what makes it cheap, and it is also why the
// traversal cannot report what it skipped: an edge excluded by the WHERE clause
// was never seen by the walk, and capability.Graph deliberately exposes no
// bulk-edge read that would let this package count them itself. So the count is
// taken the only way the port allows, by walking again with no floor under the
// SAME bounds and diffing the reachable sets. That doubles the traversal cost
// of a default query, which is the price of the filter not being silent; it is
// paid only when a floor is actually in effect, so a caller that asks for
// `low` explicitly pays nothing.
//
// Nodes are counted, not edges — see Report.LowConfidenceHidden — and only
// nodes whose unfloored path confidence is itself below the floor. Without that
// second test a truncated probe could report nodes the floored walk would have
// reached had its own node budget lasted, attributing a bound to the floor.
//
// `reported` is every symbol the finished report names — callers AND dispatch
// rows. It is passed in rather than derived from the caller walk because the
// two walks together are what the reader was shown, and diffing the probe
// against only one of them would report every implementation as "hidden by the
// floor" when the floor had nothing to do with it.
func countHiddenByFloor(
	ctx context.Context,
	deps capability.Deps,
	tgt capability.Target,
	reported map[int64]struct{},
	resTruncated bool,
	opts Options,
) (int, bool, error) {
	// A floor at the weakest tier excludes nothing, so there is nothing to
	// count and no reason to pay for a second walk.
	if !opts.MinConfidence.Valid() || opts.MinConfidence == model.ConfidenceLow {
		return 0, false, nil
	}

	probe, err := deps.Graph.Traverse(ctx, graph.Query{
		Roots:    []int64{tgt.SymbolID},
		Kinds:    opts.Kinds,
		MaxDepth: opts.MaxDepth,
		MaxNodes: opts.MaxNodes,
		// The hub limit is carried over even though the floor is not. This walk
		// exists to isolate what the FLOOR excluded, so it has to differ from
		// the reported walk in exactly one respect; a probe that expanded
		// through hubs the real walk refused would count their descendants as
		// hidden-by-the-floor and blame the wrong bound.
		HubDegreeLimit: opts.HubDegreeLimit,
		// MinConfidence unset: this walk is the one that admits everything.
		Direction: graph.Incoming,
	})
	if err != nil {
		return 0, false, fmt.Errorf("impact: unfloored probe from %s: %w", tgt.FQN, err)
	}

	var hidden int
	for _, n := range probe.Nodes {
		if n.Depth == 0 {
			continue // the target is not impacted by itself
		}
		if _, ok := reported[n.SymbolID]; ok {
			continue
		}
		if n.Confidence.AtLeast(opts.MinConfidence) {
			continue // absent for some other reason than the floor
		}
		hidden++
	}
	return hidden, probe.Truncated || resTruncated, nil
}

// belowFloorUnknown states the filter, its size and how to undo it.
//
// It is an Unknown rather than a footnote because that is what it is: a region
// of the graph this report did not walk. The remedy is named in the same
// sentence as the count, so a reader who needs the wider radius does not have
// to know the option exists.
func belowFloorUnknown(floor model.Confidence, hidden int, bounded bool) Unknown {
	return Unknown{
		Kind: UnknownBelowConfidenceFloor,
		Detail: fmt.Sprintf(
			"%d further symbol(s) reach the target only through edges weaker than %q, and are excluded "+
				"from the counts above. They are still in the index: re-run with MinConfidence=low "+
				"(--min-confidence low) to see them. They are excluded by default because heuristic `low` "+
				"edges are measured at 0.0-2.8%% precision (docs/accuracy.md), so most of them describe a "+
				"relationship that does not exist — most, not all, which is why this number is reported "+
				"rather than the edges quietly dropped.",
			hidden, floor),
		Count:             int64(hidden),
		CountIsLowerBound: bounded,
	}
}

// unresolvedUnknown turns the run-wide resolution gap into a counted unknown.
//
// Run-wide rather than per-symbol, because an unresolved reference is by
// definition not attributed to any symbol — that is what makes it unresolved.
// Reporting it as a bound on the whole answer is the only honest framing
// available; pretending the number is zero, or omitting it, is not.
//
// IT STATES COUNTS AND NO PERCENTAGE, deliberately, and it is the one place in
// this alignment that does. Everywhere else a rate answers "of what could be
// bound, how much was" — a claim about the index. This is not that claim. It
// bounds THIS answer: how many uses of this symbol might be missing from the
// impact set above. The quantity a reader needs for that is a count, because it
// is compared against the number of impacted symbols reported, not against
// another coverage figure. A percentage here would have to pick a denominator,
// and whichever it picked would invite comparison with the rate on the
// completeness block beside it — a third number for a fourth question, which is
// how a reader loses track of which one describes their risk. See
// registry.ResolutionStats.BindingRate for the rate, and Completeness for where
// it is published.
func unresolvedUnknown(stats registry.ResolutionStats) Unknown {
	unaccounted := stats.Unresolved + stats.Ambiguous + stats.Pending
	if stats.Total == 0 {
		return Unknown{
			Kind: UnknownUnresolvedReferences,
			Detail: "This index run recorded no references at all, so nothing is known about how much of the " +
				"repository was resolvable. The relationships above may be a small fraction of what exists.",
		}
	}
	return Unknown{
		Kind: UnknownUnresolvedReferences,
		Detail: fmt.Sprintf(
			"%d reference(s) in this run were never bound to a definition: %d matched no definition "+
				"in this index at all — mostly calls into libraries, though some are code this index "+
				"did not cover — %d were too ambiguous to choose between, and %d are still pending. "+
				"Any one of them could be a use of this symbol that does not appear above.",
			unaccounted, stats.Unresolved, stats.Ambiguous, stats.Pending),
		Count: unaccounted,
	}
}
