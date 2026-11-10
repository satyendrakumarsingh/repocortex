package scip

import (
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// CALLS FROM TWO INDEPENDENT SOURCES
//
// This file is the one place in RepoCortex that produces a CALLS edge at
// confidence=exact, and it is worth being precise about why that is allowed
// here and nowhere else. See edgeKind in kinds.go for the objection this
// construction answers.
//
// A CALLS edge asserts two things at once:
//
//	IS THIS OCCURRENCE A CALL?   answered by RepoCortex's own tree-sitter parser,
//	                             which recorded a `reference.call` at this file
//	                             and line. A grammar recognising a call
//	                             expression is a syntactic fact about the source
//	                             text, not an inference about a symbol.
//
//	WHAT DOES IT RESOLVE TO?     answered by the compiler, via the SCIP
//	                             occurrence at that same file and line.
//
// Neither half is guessed, and neither half is derived from the other. What is
// new is only the join. Deriving call-ness from the SCIP side alone — "the
// target is a method, so this must be a call" — remains forbidden, because that
// IS an inference, and it is the thing kinds.go warns about.
//
// THE JOIN IS THE SAME ONE `cortex accuracy` ALREADY TREATS AS GROUND TRUTH.
// internal/cli/accuracy.go adjudicates the heuristic resolver's precision and
// recall against exactly this intersection: lines the parser recorded a call
// on, where the compiler resolved that line to EXACTLY ONE target. We already
// trust the construction enough to judge our own edges by it. PromoteCalls
// reuses it to create them, and mirrors accuracyCTE predicate for predicate so
// the two cannot drift apart:
//
//   - `unambiguous`: group the file's compiler edges by evidence line, keep
//     only lines with COUNT(DISTINCT dst_id) = 1.
//   - `truth`:       of those, keep the lines the parser recorded a call on.
//
// AND A SECOND: the target must not be a RECEIVER on that line. For
//
//	await svc.delete_line(...)
//
// the parser writes TWO reference.call rows at the same column — `delete_line`
// with receiver `svc`, and a bare `svc` — because its call query captures the
// callee expression and an attribute access has a base. The bare row passes the
// name check on its own and yields "CALLS svc", which is false: `svc` is not
// called, it is what the call was made against. Measured on an 8,500-file
// repository this was 4,784 of 35,226 promotions, 13.6%.
//
// Declining them is not this package overruling the parser. The parser has
// contradicted ITSELF — one row says a call named `svc` happens here, another
// says the call here is `delete_line` ON `svc` — and the second row is strictly
// more specific than the first. Preferring the record that names both the
// callee and the receiver over the one that names neither is reading the two
// rows together, which is the only way they can both be true. What is NOT
// available is stamping the contradiction `exact`: to a user reading a blast
// radius, exact means a compiler proved it, and no compiler proved that `svc`
// is called.
//
// AND ONE CONSTRAINT THE ADJUDICATOR DOES NOT NEED: the name must agree. The
// parser records WHAT each call is written as; a promoted edge is kept only
// where the compiler's single target on that line is a symbol RepoCortex indexed
// under exactly that name. accuracy.go can do without this because it scores a
// resolver that already had to pick a name; a producer cannot. Measured on an
// 8,500-file repository, dropping the check admitted 11,459 edges of which a
// hand-read sample was almost entirely wrong — `select(Device)` attributed to
// a call, `pytest.raises(AccountLockedError)` attributed to a call, and worst,
//
//	devices: Mapped[list["Device"]] = relationship(
//
// where the parser's call is `relationship`, the compiler's only in-repository
// target on the line is the type `Device`, and the line survives the
// unambiguity test purely because everything else on it resolves to a library.
// That is a type annotation stamped CALLS at exact. The name check rejects all
// three, and it is not a fourth source of truth: it is the parser's own record
// of what was called, held against RepoCortex's own record of what the target is
// named, with the compiler supplying only the link between them.
//
// THE UNAMBIGUITY RESTRICTION IS LOAD-BEARING, not caution. On the line
//
//	total = compute(order.subtotal)
//
// the parser records a call and the compiler resolves two symbols: `compute`
// and the field `order.subtotal`. Promoting both would stamp a field read as a
// call, at exact, which is precisely the lie this system must not tell. A line
// carrying more than one compiler target has no single answer to "what does the
// call on this line reach", so it is left as REFERENCES. That is conservative
// in the direction that costs recall rather than precision, and every line it
// declines is counted so the cost is visible rather than assumed.

// CallSites reports whether RepoCortex's parser recognised a call expression at a
// source position.
//
// Declared here, at the consumer, rather than in the registry that will satisfy
// it: this package performs no database access by design, and the whole point
// of the construction is that the call-ness fact arrives from somewhere other
// than the SCIP index. One method, so a test can implement it in a line.
//
// path is repo-relative with forward slashes, as FileFacts.Path is. line is
// 1-indexed, as model.Range is — the same space symbol_references.start_line
// and edges.evidence_line are compared in.
type CallSites interface {
	// HasCall reports whether any call was parsed on this line. It decides
	// whether a line is in the population at all, which is what makes a
	// declined line reportable rather than invisible.
	HasCall(path string, line int) bool
	// IsCallTo reports whether one of the calls parsed on this line is written
	// as name.
	IsCallTo(path string, line int, name string) bool
	// IsReceiver reports whether name is what some call on this line was
	// written AGAINST, rather than what was called. See PromoteCalls.
	IsReceiver(path string, line int, name string) bool
}

// callSite is one parser-recorded call position.
type callSite struct {
	path string
	line int
}

// namedCallSite is one parser-recorded call position and the name it is
// written as.
type namedCallSite struct {
	path string
	line int
	name string
}

// CallSiteSet is an in-memory CallSites.
//
// A real repository has a few hundred thousand parsed call sites, so this is
// tens of megabytes and held for the duration of one import — cheaper than a
// query per occurrence by three orders of magnitude, and the alternative
// (streaming both sides in path order) would couple the ingester's document
// order to the registry's, which SCIP does not guarantee.
type CallSiteSet struct {
	lines     map[callSite]struct{}
	named     map[namedCallSite]struct{}
	receivers map[namedCallSite]struct{}
}

// NewCallSiteSet returns an empty set sized for n entries.
func NewCallSiteSet(n int) *CallSiteSet {
	if n < 0 {
		n = 0
	}
	return &CallSiteSet{
		lines:     make(map[callSite]struct{}, n),
		named:     make(map[namedCallSite]struct{}, n),
		receivers: make(map[namedCallSite]struct{}, n),
	}
}

// Add records that a call written as name was parsed at path:line. Lines at or
// below zero are ignored: the parser writes 0 for a reference whose position it
// could not place, and a position-free call site cannot be joined to anything.
func (s *CallSiteSet) Add(path string, line int, name, receiver string) {
	if s == nil || line <= 0 {
		return
	}
	s.lines[callSite{path: path, line: line}] = struct{}{}
	if name != "" {
		s.named[namedCallSite{path: path, line: line, name: name}] = struct{}{}
	}
	if receiver != "" {
		s.receivers[namedCallSite{path: path, line: line, name: receiver}] = struct{}{}
	}
}

// HasCall implements CallSites.
func (s *CallSiteSet) HasCall(path string, line int) bool {
	if s == nil {
		return false
	}
	_, ok := s.lines[callSite{path: path, line: line}]
	return ok
}

// IsCallTo implements CallSites.
func (s *CallSiteSet) IsCallTo(path string, line int, name string) bool {
	if s == nil || name == "" {
		return false
	}
	_, ok := s.named[namedCallSite{path: path, line: line, name: name}]
	return ok
}

// IsReceiver implements CallSites.
func (s *CallSiteSet) IsReceiver(path string, line int, name string) bool {
	if s == nil || name == "" {
		return false
	}
	_, ok := s.receivers[namedCallSite{path: path, line: line, name: name}]
	return ok
}

// Len is how many distinct positions the set holds. Positions, not names: it
// answers "how much of the repository has a call parsed in it", which is the
// figure that explains a promotion count of zero.
func (s *CallSiteSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.lines)
}

// PromoteStats reports one call to PromoteCalls.
//
// Ambiguous and Unpromoted are the two reasons a compiler-proven reference
// stayed a REFERENCES edge. They are separated because they have different
// meanings and different fixes: Ambiguous is recall this construction
// deliberately declines to claim, while the bulk of Unpromoted is simply
// correct — most references are not calls.
type PromoteStats struct {
	// Promoted is REFERENCES edges rewritten to CALLS.
	Promoted int
	// CallLines is parser-recorded call lines that carried at least one
	// compiler edge — the population the promotion was decided over.
	CallLines int
	// Ambiguous is call lines left alone because the compiler resolved more
	// than one distinct target there, so no single call target is defined.
	Ambiguous int
	// Receivers is call lines left alone because the compiler's single target
	// there is what a call on that line was written AGAINST, not what was
	// called. See PromoteCalls.
	Receivers int
	// NameMismatch is call lines left alone because the compiler's single
	// target on the line is not named as any call the parser recorded there.
	// It is the count that separates "the compiler resolved the call" from
	// "the compiler resolved SOMETHING on the same line as a call" — and the
	// second, left unchecked, is where a type annotation becomes a call.
	NameMismatch int
	// Unpromoted is compiler REFERENCES edges left as REFERENCES — the parser
	// recorded no call at that line, or the line was ambiguous. Reported so
	// "how much of the compiler's output became calls" is a ratio the reader
	// can compute rather than a number they have to trust.
	Unpromoted int
}

// Add accumulates s2 into s.
func (s *PromoteStats) Add(s2 PromoteStats) {
	s.Promoted += s2.Promoted
	s.CallLines += s2.CallLines
	s.Ambiguous += s2.Ambiguous
	s.NameMismatch += s2.NameMismatch
	s.Receivers += s2.Receivers
	s.Unpromoted += s2.Unpromoted
}

// PromoteCalls rewrites the REFERENCES edges of one file into CALLS edges
// wherever the parser recorded a call at the same line and the compiler
// resolved that line to exactly one target.
//
// edges must be one file's compiler edges, already resolved to rowids —
// FileFacts.Edges output for a single FileFacts, which is what makes
// "COUNT(DISTINCT dst_id) = 1" computable here without a database. Edges whose
// destination could not be resolved to an indexed symbol never reached this
// slice, which is deliberate and matches the adjudicator: a line's ambiguity is
// judged over the targets that exist in RepoCortex's graph, exactly as
// accuracyCTE judges it over the rows in the edges table.
//
// It mutates edges in place and returns what it did. Only Kind changes:
// Confidence and Evidence.Source were set by Relation.Edge, which remains the
// single constructor for a model.Edge in this package, and every rewritten edge
// is re-validated so a kind the model does not accept fails here rather than at
// the SQL CHECK.
//
// calls may be nil, in which case nothing is promoted and the counts describe
// what was available. That is the pre-existing behaviour, kept reachable so an
// import with no parser facts to join against degrades to REFERENCES-only
// rather than failing.
//
// DETERMINISM: the decision for an edge depends only on its own line's target
// set and on calls, never on iteration order or on how many workers ran. The
// grouping map is only ever counted, never ranged over for a winner.
func PromoteCalls(
	path string,
	edges []model.Edge,
	names map[int64]string,
	calls CallSites,
) (PromoteStats, error) {
	var st PromoteStats

	// Pass 1: mirror accuracyCTE's `unambiguous` — distinct destinations per
	// evidence line, over every compiler edge the file produced.
	targets := make(map[int]map[int64]struct{}, len(edges))
	for i := range edges {
		line := edges[i].Evidence.Range.Start.Line
		if line <= 0 {
			continue
		}
		byDst, ok := targets[line]
		if !ok {
			byDst = make(map[int64]struct{}, 2)
			targets[line] = byDst
		}
		byDst[edges[i].DstID] = struct{}{}
	}

	// Pass 2: mirror `truth` — of those lines, the ones the parser called on —
	// and rewrite. Lines are visited in sorted order purely so the counting is
	// reproducible under -race and readable in a failure message; the verdict
	// per line does not depend on it.
	lines := make([]int, 0, len(targets))
	for line := range targets {
		lines = append(lines, line)
	}
	sort.Ints(lines)

	promote := make(map[int]bool, len(lines))
	for _, line := range lines {
		if calls == nil || !calls.HasCall(path, line) {
			continue
		}
		st.CallLines++
		if len(targets[line]) != 1 {
			st.Ambiguous++
			continue
		}
		name := names[onlyTarget(targets[line])]
		if !calls.IsCallTo(path, line, name) {
			st.NameMismatch++
			continue
		}
		if calls.IsReceiver(path, line, name) {
			st.Receivers++
			continue
		}
		promote[line] = true
	}

	for i := range edges {
		if edges[i].Kind != model.EdgeReferences {
			// IMPORTS and IMPLEMENTS are already specific claims the index
			// stated directly; a call site coinciding with one does not make
			// them calls.
			continue
		}
		if !promote[edges[i].Evidence.Range.Start.Line] {
			st.Unpromoted++
			continue
		}
		edges[i].Kind = model.EdgeCalls
		if err := edges[i].Validate(); err != nil {
			return st, fmt.Errorf("scip: promote call at %s:%d: %w",
				path, edges[i].Evidence.Range.Start.Line, err)
		}
		st.Promoted++
	}

	return st, nil
}

// onlyTarget returns the single member of a one-element set. The caller has
// already established the length, so this is a lookup and not a choice — which
// is why it may range over a map without threatening determinism.
func onlyTarget(set map[int64]struct{}) int64 {
	for id := range set {
		return id
	}
	return 0
}
