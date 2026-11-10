// Package compile turns an informal request plus an assembled context pack into
// an explicit TASK CONTRACT: the brief an AI coding agent should be handed
// instead of a one-line ask.
//
// # The failure this addresses
//
// Most bad agent output is not a model failure, it is an UNDER-SPECIFIED TASK.
// "Add idempotency to payment callbacks" carries no boundary, so the agent
// edits files nobody asked it to touch; no statement of done, so it stops when
// the code compiles; no named unknowns, so it silently invents an answer to the
// one question that needed a human. A larger model does not fix any of those —
// it executes the same missing specification faster.
//
// So the output here is not a better prompt. It is the same four things a
// competent engineer would establish before starting: what is in scope and why,
// what is deliberately not, which rules the change must respect, and what is
// still unknown.
//
// # What this deliberately does NOT do
//
// It does not call a model. There is no network, no clock and no LLM anywhere
// in this package: a contract that changed between two runs of the same input
// could not be cached, tested, or trusted, and "the brief drifted" is a far
// worse failure than "the brief was thin".
//
// It does not write code, plan an implementation, or order the work. Deciding
// HOW is the agent's job and it has the source; deciding WHAT was asked for is
// the job nobody was doing.
//
// It does not guess at requirements the index cannot support. It has no idea
// what "idempotent" should mean for a given handler, and inventing an
// acceptance criterion for it would be indistinguishable, to the reader, from
// one that came from the repository. Those gaps are emitted as Unknowns
// instead, which is the whole difference between a brief and a hallucination.
//
// # Everything cites its source
//
// Every Constraint and every Risk carries Evidence naming a file, a symbol or a
// measured property of the pack, and one filter at the exit drops anything that
// does not. This is the rule the package exists to enforce: a constraint
// without evidence is an opinion, and an opinion typeset as a requirement is
// how an agent ends up confidently obeying something no one in the repository
// ever decided.
//
// # Determinism
//
// The same intent and the same pack produce a byte-identical Contract, ordering
// included. Every ordering in this file is an explicit total sort and no map is
// ever ranged over into output.
package compile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
)

// ErrEmptyIntent is returned when there is no request to compile. It is the one
// input this package rejects: an empty PACK is a legitimate answer ("the index
// matched nothing"), but an empty INTENT is always a caller bug, and producing
// a contract for it would put a plausible-looking brief in front of an agent
// with nothing behind it.
var ErrEmptyIntent = errors.New("compile: intent must not be empty")

// Contract is a compiled task brief.
type Contract struct {
	// Intent is the developer's request, verbatim. Deliberately not cleaned up:
	// a reader comparing it against Interpretation is checking whether a vague
	// ask was read the way it was meant, and normalising it away would hide the
	// vagueness that caused the misread.
	Intent string `json:"intent"`
	// Interpretation is what the system understood, carried from the context
	// pack so a misread task is visible at the top rather than inferred from a
	// wrong answer.
	Interpretation string `json:"interpretation"`

	Goal string `json:"goal"`
	// Scope is the files and symbols the work is expected to touch, each with
	// the reason it is in scope.
	Scope []ScopeItem `json:"scope"`
	// OutOfScope names things deliberately excluded, with the reason. This is
	// load-bearing: an agent told only what to do will wander.
	OutOfScope []ScopeItem `json:"out_of_scope,omitempty"`

	// Constraints are rules the change must respect, derived from observable
	// facts about the repository, never invented.
	Constraints []Constraint `json:"constraints,omitempty"`
	// Acceptance are checkable statements of done.
	Acceptance []string `json:"acceptance,omitempty"`
	// Risks are things that could go wrong, each with the evidence.
	Risks []Risk `json:"risks,omitempty"`

	// Unknowns are questions the compiler could not answer from the index.
	// Never empty in practice, and never suppressed: a contract that pretends
	// to be complete is worse than one that names its gaps.
	Unknowns []string `json:"unknowns,omitempty"`

	// BlastRadius is what an impact analysis said a change to the scope would
	// reach, when one was supplied (see Ports).
	//
	// It is NOT scope. Scope is where the work is expected to land; this is
	// what touching it reaches, and the difference is the whole reason both are
	// printed. Each entry carries the confidence of the path that reached it,
	// so a proven caller and an inferred one stay distinguishable, and the same
	// list is what `cortex verify` later checks the change against.
	BlastRadius []Reached `json:"blast_radius,omitempty"`

	// Tests are the tests the analysis found covering the scope symbols, each
	// with the signal that found it. Named here so that "the tests that cover
	// this" is a claim the contract makes and a later verification can check,
	// rather than something the agent decides afterwards.
	Tests []TestRef `json:"tests,omitempty"`

	// ImpactFloor is the confidence floor the impact walk ran with and
	// ImpactHidden counts the symbols that floor kept out of BlastRadius.
	//
	// Carried into the contract because the blast radius is uninterpretable
	// without them, and because a later check that accuses someone of missing a
	// caller must be able to state the same filter it inherited.
	ImpactFloor  string `json:"impact_floor,omitempty"`
	ImpactHidden int    `json:"impact_hidden_below_floor,omitempty"`

	TokenEstimate int `json:"token_estimate"`
}

// ScopeItem is one place the work is expected to land, or one place it is
// expected not to.
type ScopeItem struct {
	Path   string `json:"path"`
	Symbol string `json:"symbol,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Why    string `json:"why"`
}

// Constraint is a rule the change must respect.
type Constraint struct {
	Text string `json:"text"`
	// Evidence is where in the repository this constraint comes from. A
	// constraint with no evidence is an opinion and must not be emitted.
	Evidence string `json:"evidence"`
}

// Risk is something that could go wrong, with the reason to believe it might.
type Risk struct {
	Text     string `json:"text"`
	Severity string `json:"severity"` // high | medium | low
	Evidence string `json:"evidence"`
}

// Options tunes compilation.
type Options struct {
	// MaxScope bounds the scope list. Zero means a sensible default; a negative
	// value is treated as zero rather than as "unbounded", because an unbounded
	// scope is the thing this field exists to prevent.
	MaxScope int
}

const (
	severityHigh   = "high"
	severityMedium = "medium"
	severityLow    = "low"
)

// Entity kinds this package acts on.
//
// These MIRROR model.EntityKind rather than importing it, because Item.Kind is
// a plain string and this package only ever COMPARES kinds — it never
// constructs one, so it has no need of the domain type. The mirror is pinned by
// a test against the real constants, so a rename in the domain fails loudly
// instead of silently switching a rule off, which is the failure mode that
// would otherwise make this shortcut a bad trade.
const (
	kindTest          = "test"
	kindEndpoint      = "endpoint"
	kindTable         = "table"
	kindInterface     = "interface"
	kindConfiguration = "configuration"
	kindFile          = "file"
)

// confidenceLow mirrors model.ConfidenceLow, pinned by the same test.
const confidenceLow = "low"

// defaultMaxScope bounds the scope list when the caller does not.
//
// Twelve is a working set, not a search result. A contract listing forty
// symbols has not bounded anything: the agent still has to decide what matters,
// which is precisely the decision this package was supposed to have made for
// it. The cut is reported as an Unknown so the boundary stays visible.
const defaultMaxScope = 12

// maxEvidenceRefs bounds how many references one Evidence string names.
//
// Evidence has to be checkable at a glance, and a reader does not check a list
// of twenty paths — they skip it, which makes the citation decorative. The full
// list is in Scope either way; this is the pointer into it, not a copy of it.
const maxEvidenceRefs = 3

// wideScopeItems is the count at which "forgot one of them" becomes the likely
// failure rather than a possible one. Chosen as the point where the scope stops
// fitting on a screen alongside the code being edited; it is a heuristic about
// human attention, and the risk it raises is medium precisely because of that.
const wideScopeItems = 8

// bytesPerToken matches the divisor the context package uses for its own
// estimate, so a caller can add a pack's estimate and a contract's estimate
// without reconciling two different rules of thumb. It is an estimate for
// budgeting, never a billing figure.
const bytesPerToken = 4

// Compile turns an intent and its context pack into a contract, consulting
// nothing beyond the pack.
//
// Returns an error only for an empty intent — an empty pack is a valid input
// that produces a contract dominated by Unknowns, which is the honest answer.
//
// The background context is safe here and only here: with no ports there is no
// I/O to cancel, and every call this function makes is a pure function of its
// arguments. Callers with an indexed repository should use CompileWith, which
// takes the caller's context and the two sources this one cannot see.
func Compile(intent string, pack devctx.Pack, opts Options) (Contract, error) {
	return CompileWith(context.Background(), intent, pack, opts, Ports{})
}

// CompileWith turns an intent and its context pack into a contract, consulting
// the declared architecture and the impact of the change where the caller
// supplies them.
//
// Both ports are optional and a zero Ports makes this identical to Compile.
// What is NOT optional is saying which sources were consulted: a contract with
// no architecture constraints because no architecture was declared and one with
// none because nobody asked are different documents, and Unknowns distinguishes
// them.
//
// Returns an error when a supplied port fails. Degrading to a thinner contract
// on a failed impact analysis would produce a brief that looks exactly like one
// for a change that reaches nothing.
func CompileWith(ctx context.Context, intent string, pack devctx.Pack, opts Options, ports Ports) (Contract, error) {
	if strings.TrimSpace(intent) == "" {
		return Contract{}, ErrEmptyIntent
	}

	maxScope := opts.MaxScope
	if maxScope <= 0 {
		maxScope = defaultMaxScope
	}

	// selected and scope stay index-aligned: the rules below need Confidence,
	// which is a property of the pack item and deliberately not carried into
	// ScopeItem — a contract states what is in scope and why, and a raw
	// confidence label there would invite an agent to re-litigate the ranking.
	selected, dropped := selectItems(pack.Items, maxScope)
	scope := scopeItems(selected)

	reach, err := gatherReach(ctx, ports.Impact, scope)
	if err != nil {
		return Contract{}, err
	}

	c := Contract{
		Intent:         intent,
		Interpretation: strings.TrimSpace(pack.Interpretation),
		Goal:           goalFrom(intent),
		Scope:          scope,
		OutOfScope:     outOfScopeItems(pack.Excluded),
		Constraints:    constraintsFor(scope, ports.Architecture),
		Acceptance:     acceptanceFor(scope, reach),
		Risks:          risksFor(selected, scope, reach),
		Unknowns:       unknownsFor(pack, scope, dropped, ports, reach),
		BlastRadius:    reach.items,
		Tests:          reach.tests,
		ImpactFloor:    reach.floor,
		ImpactHidden:   reach.hidden,
	}

	// Measured over the rendered brief rather than over the struct, because the
	// brief is what an agent is actually charged for. Markdown deliberately does
	// not render TokenEstimate, which is what keeps this from being circular.
	c.TokenEstimate = len(c.Markdown()) / bytesPerToken
	return c, nil
}

// selectItems ranks the pack's items and cuts them to the budget, reporting how
// many were dropped so the cut can be named rather than hidden.
func selectItems(items []devctx.Item, maxScope int) ([]devctx.Item, int) {
	if len(items) == 0 {
		return nil, 0
	}

	ranked := make([]devctx.Item, len(items))
	copy(ranked, items)

	// The comparison is TOTAL, not just by relevance. Relevance ties constantly
	// (an item admitted by the same rule as another scores identically), and a
	// partial comparator would leave those ties resolved by the pack's
	// incidental order — which is exactly how "same input, different brief"
	// gets into a system that believes it is deterministic.
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		switch {
		case a.Relevance != b.Relevance:
			return a.Relevance > b.Relevance
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Name != b.Name:
			return a.Name < b.Name
		default:
			return a.Kind < b.Kind
		}
	})

	if len(ranked) <= maxScope {
		return ranked, 0
	}
	return ranked[:maxScope], len(ranked) - maxScope
}

func scopeItems(items []devctx.Item) []ScopeItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]ScopeItem, 0, len(items))
	for _, it := range items {
		s := ScopeItem{Path: it.Path, Symbol: it.Name, Kind: it.Kind, Why: it.Why}
		if it.Kind == kindFile {
			// The path IS the name for a file entity. Printing it twice reads as
			// two different things being in scope.
			s.Symbol = ""
		}
		out = append(out, s)
	}
	return out
}

// outOfScopeItems carries the pack's exclusions through with their reasons.
//
// The reason is copied verbatim and never rewritten. "Nothing connects
// MarketingReport to the callback flow" is a checkable claim made by the engine
// that did the connecting; restating it in this package's own words would put a
// second, unsourced opinion next to it.
func outOfScopeItems(excluded []devctx.Exclusion) []ScopeItem {
	if len(excluded) == 0 {
		return nil
	}
	out := make([]ScopeItem, 0, len(excluded))
	for _, ex := range excluded {
		out = append(out, ScopeItem{Path: ex.Path, Symbol: ex.Name, Why: ex.Reason})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Symbol != b.Symbol:
			return a.Symbol < b.Symbol
		default:
			return a.Why < b.Why
		}
	})
	return out
}

// goalFrom normalises the intent into a single statement.
//
// It normalises WHITESPACE AND PUNCTUATION AND NOTHING ELSE. The temptation
// here is to expand "add idempotency" into a paragraph about request keys and
// replay windows, and that paragraph would be this package inventing the
// requirement it was asked to record. When the intent is thin, the goal reads
// thin, and that visible thinness is the signal the author needs.
func goalFrom(intent string) string {
	g := strings.Join(strings.Fields(intent), " ")
	if g == "" {
		return ""
	}
	r := []rune(g)
	r[0] = unicode.ToUpper(r[0])
	g = string(r)
	if last := r[len(r)-1]; last != '.' && last != '!' && last != '?' {
		g += "."
	}
	return g
}

// constraintRules map an observable property of a scope item to the rule that
// property implies.
//
// Every rule fires on a fact the PACK STATES — a kind, or a path — and the text
// it emits is the consequence of that fact, not an inference about the change.
// "This is an endpoint" is observed; "the route is a public surface" follows
// from it; "you should version the API" would not, and is not here.
//
// Output order follows this slice, so the constraint list does not reshuffle
// when ranking changes.
var constraintRules = []struct {
	applies func(ScopeItem) bool
	text    string
}{
	{
		applies: func(s ScopeItem) bool { return s.Kind == kindTest || isTestPath(s.Path) },
		text:    "Existing tests cover this area; they must still pass, and new behaviour needs new cases.",
	},
	{
		applies: func(s ScopeItem) bool { return s.Kind == kindEndpoint },
		text:    "This is a public API surface; changing the route or the payload shape is a breaking change for its callers.",
	},
	{
		applies: func(s ScopeItem) bool { return s.Kind == kindTable },
		text:    "This touches persisted data; a schema change needs a migration, and rows written before the change must stay readable.",
	},
	{
		applies: func(s ScopeItem) bool { return s.Kind == kindInterface },
		text:    "This interface is a contract with every implementation of it; changing the signature changes all of them, including any this pack did not reach.",
	},
	{
		applies: func(s ScopeItem) bool { return s.Kind == kindConfiguration },
		text:    "Behaviour here is configuration-driven; a new setting needs a default that preserves the current behaviour of deployments that do not set it.",
	},
}

func constraintsFor(scope []ScopeItem, view ArchitectureView) []Constraint {
	var out []Constraint
	for _, rule := range constraintRules {
		var refs []string
		for _, s := range scope {
			if rule.applies(s) {
				refs = append(refs, s.ref())
			}
		}
		out = append(out, Constraint{Text: rule.text, Evidence: joinEvidence(refs)})
	}

	// The declared rules come last and go through the SAME exit filter below.
	// They arrive from a config file this package does not own, so a rule that
	// names nothing checkable has to be dropped by the same gate as everything
	// else rather than trusted because it was written down.
	out = append(out, architectureConstraints(view, scope)...)

	// The single exit filter. Every rule above is written to cite its trigger,
	// so in principle this never fires — which is exactly why it is here: the
	// rule "no constraint without evidence" is enforced at one place that a
	// future rule cannot forget to consult.
	kept := out[:0]
	for _, c := range out {
		if c.Text == "" || c.Evidence == "" {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func risksFor(selected []devctx.Item, scope []ScopeItem, reach reachSummary) []Risk {
	var out []Risk

	// Reach that lands OUTSIDE the scope is the risk an impact analysis exists
	// to surface: the agent has been told where to work, and this is what that
	// work touches which nobody listed. Severity follows the strength of the
	// relationship, not its size — one compiler-proven caller outside scope is
	// a harder fact than nine inferred ones.
	if outside := blastRefs(reach.items, func(r Reached) bool {
		return outsideScope(scope, r) && !r.IsTest
	}); len(outside) > 0 {
		severity := severityMedium
		for _, r := range reach.items {
			if outsideScope(scope, r) && !r.IsTest && r.Depth == 1 &&
				confidenceRank(r.Confidence) >= confidenceRank(confidenceHigh) {
				severity = severityHigh
				break
			}
		}
		out = append(out, Risk{
			Text: fmt.Sprintf(
				"The impact analysis reaches %d symbol(s) in files outside Scope; the change is either wider than this contract says, or those callers have to keep working unchanged.",
				len(outside)),
			Severity: severity,
			Evidence: joinEvidence(outside),
		})
	}

	// An item admitted by a weak signal is still worth sending — the context
	// engine already decided that — but an agent reading a flat list cannot
	// tell a proven call edge from a name that happened to match. Saying so is
	// the difference between bounded context and a confident wrong file.
	var inferred []string
	for i, it := range selected {
		if strings.EqualFold(it.Confidence, confidenceLow) && i < len(scope) {
			inferred = append(inferred, scope[i].ref())
		}
	}
	if ev := joinEvidence(inferred); ev != "" {
		out = append(out, Risk{
			Text:     "Some scope entries were admitted by an inferred relationship rather than a proven one, so they may not be involved in this change at all; confirm before editing them.",
			Severity: severityMedium,
			Evidence: ev,
		})
	}

	// A change that crosses both a public surface and stored data is the case
	// where a mistake escapes the repository in two directions at once: callers
	// see it immediately, and the data keeps the shape of the bug afterwards.
	endpoints := refsOfKind(scope, kindEndpoint)
	tables := refsOfKind(scope, kindTable)
	if len(endpoints) > 0 && len(tables) > 0 {
		out = append(out, Risk{
			Text:     "This change spans a public API surface and persisted data at once; an error is visible to callers immediately and cannot be rolled back by reverting the code alone.",
			Severity: severityHigh,
			Evidence: joinEvidence(append(append([]string{}, endpoints...), tables...)),
		})
	}

	if n := len(scope); n >= wideScopeItems {
		files := distinctPaths(scope)
		out = append(out, Risk{
			Text:     "The scope is wide enough that a change applied in one place and forgotten in another is the likely failure; work through the scope list explicitly rather than from memory.",
			Severity: severityMedium,
			Evidence: fmt.Sprintf("%d scope entries across %d file(s)", n, files),
		})
	}

	// Severity first so the reader meets the worst news first, then text, which
	// makes the order total and therefore reproducible.
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := severityRank(out[i].Severity), severityRank(out[j].Severity); a != b {
			return a > b
		}
		return out[i].Text < out[j].Text
	})

	kept := out[:0]
	for _, r := range out {
		if r.Text == "" || r.Evidence == "" {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func severityRank(s string) int {
	switch s {
	case severityHigh:
		return 3
	case severityMedium:
		return 2
	case severityLow:
		return 1
	default:
		return 0
	}
}

// acceptanceFor states done in terms of things already in the contract.
//
// Every entry names a scope entry, because an acceptance criterion that names
// nothing ("the feature works correctly") cannot be checked and its presence
// makes the list look complete. What the behaviour should BE is not in the
// index; that gap is an Unknown, not a criterion.
func acceptanceFor(scope []ScopeItem, reach reachSummary) []string {
	if len(scope) == 0 {
		return nil
	}
	var out []string

	if refs := blastRefs(reach.items, func(r Reached) bool {
		return outsideScope(scope, r) && !r.IsTest
	}); len(refs) > 0 {
		out = append(out, fmt.Sprintf(
			"Every caller under Blast radius that sits outside Scope — %s — still compiles and still behaves as before, or its update is part of this change.",
			joinEvidence(refs)))
	}
	if len(reach.tests) > 0 {
		refs := make([]string, 0, len(reach.tests))
		for _, t := range reach.tests {
			refs = append(refs, t.ref())
		}
		out = append(out, fmt.Sprintf(
			"The tests named under Tests — %s — still pass and still exercise the symbols they were listed against.",
			joinEvidence(refs)))
	}

	if tests := refsMatching(scope, func(s ScopeItem) bool {
		return s.Kind == kindTest || isTestPath(s.Path)
	}); len(tests) > 0 {
		out = append(out, fmt.Sprintf(
			"The existing tests in %s pass unchanged, and a new case there covers the behaviour this change adds.",
			joinEvidence(tests)))
	}
	if eps := refsOfKind(scope, kindEndpoint); len(eps) > 0 {
		out = append(out, fmt.Sprintf(
			"The route and payload shape of %s are unchanged, or the break is stated explicitly in the change description.",
			joinEvidence(eps)))
	}
	if tables := refsOfKind(scope, kindTable); len(tables) > 0 {
		out = append(out, fmt.Sprintf(
			"Any schema change to %s ships with a migration in the same change.",
			joinEvidence(tables)))
	}

	out = append(out,
		"Every entry under Scope is either changed, or left unchanged for a stated reason.",
		"No file outside Scope is modified without saying why it had to be.")
	return out
}

// unknownsFor names the gaps.
//
// The last entry is unconditional, and that is the design. A contract with an
// empty Unknowns section reads as "nothing is missing", and this package is
// never in a position to make that claim: it has the structure of the code and
// none of the intent behind it.
func unknownsFor(pack devctx.Pack, scope []ScopeItem, dropped int, ports Ports, reach reachSummary) []string {
	var out []string

	// Which sources were consulted, stated before anything derived from them.
	// "No architecture constraints" means two entirely different things
	// depending on this line, and a reader cannot tell which without it.
	if ports.Architecture == nil {
		out = append(out, "No declared architecture was consulted, so this contract states no component boundary the change must respect; if .cortex.yml declares one, compile with it rather than assuming the change is unconstrained.")
	}
	if !reach.consulted {
		out = append(out, "No impact analysis was consulted, so this contract says nothing about what the change reaches beyond the files listed in Scope. An empty Blast radius here is an absence of analysis, not an absence of callers.")
	}
	for _, name := range reach.unresolved {
		out = append(out, fmt.Sprintf(
			"The impact analysis could not resolve %q in the index, so nothing is known about what changing it reaches: either the symbol is named differently in the code, or the index is stale.", name))
	}
	if reach.floor != "" && reach.hidden > 0 {
		out = append(out, fmt.Sprintf(
			"The impact analysis ran with a %q confidence floor, and %d symbol(s) reachable only across weaker edges are not in Blast radius. They are excluded because edges below the floor are mostly wrong, not because they were shown to be irrelevant.",
			reach.floor, reach.hidden))
	}
	if reach.truncated {
		out = append(out, "The impact traversal stopped at a bound with work outstanding, so Blast radius is a lower bound on what the change reaches.")
	}
	if reach.dropped > 0 {
		out = append(out, fmt.Sprintf(
			"Blast radius was capped at %d entries and %d further reached symbol(s) were dropped; if the work turns out to reach past this list, the cap hid where.",
			len(reach.items), reach.dropped))
	}
	if reach.consulted && len(reach.targets) > 0 && len(reach.tests) == 0 {
		out = append(out, "The analysis found no test covering the scope symbols it examined, so there is no named test this change can be checked against.")
	}

	if len(pack.Items) == 0 {
		out = append(out, "The context pack matched nothing in the index to this intent, so this contract has no scope: either the task names concepts the index does not cover, or the index is stale. Do not treat the empty scope as 'nothing needs to change'.")
	}
	if strings.TrimSpace(pack.Interpretation) == "" {
		out = append(out, "The context pack records no interpretation of the task, so there is no way to check that the request was read the way it was meant.")
	}
	if len(scope) > 0 && len(refsMatching(scope, func(s ScopeItem) bool {
		return s.Kind == kindTest || isTestPath(s.Path)
	})) == 0 {
		out = append(out, "No test file appears in scope, so it is not clear how this change is verified: find the test that covers this area, or decide where a new one belongs, before editing.")
	}
	if dropped > 0 {
		out = append(out, fmt.Sprintf(
			"Scope was capped at %d entries and %d further ranked candidate(s) were dropped; if the work turns out to reach past this list, the cap hid where.",
			len(scope), dropped))
	}
	for _, note := range pack.Notes {
		// Carried, not summarised. These are the context engine's own coverage
		// caveats — truncated traversals, areas the index does not reach — and
		// they are the difference between a bounded answer and a partial one.
		if n := strings.TrimSpace(note); n != "" {
			out = append(out, "From the context pack: "+n)
		}
	}

	out = append(out, "The index records structure, not intent: what the behaviour should BE after this change — the definition of correct, as opposed to the list of code involved — is not in it and has to come from the author.")
	return out
}

// isTestPath recognises a test file from its path when the index did not label
// it one.
//
// A path heuristic is normally the wrong tool in this codebase, and the reason
// it is right here is that the two errors are not symmetric. A false NEGATIVE
// makes the contract assert "no test file appears in scope" about a repository
// where one plainly does — a wrong claim, stated confidently, with nothing in
// the output for the reader to check it against. A false POSITIVE emits a
// constraint that cites the path it fired on, so the reader sees "Foo.java" in
// the evidence and dismisses it in a second. Cheap visible error beats
// expensive invisible one, so this errs toward detection — the opposite bias to
// the SQL recogniser, for the opposite reason.
func isTestPath(path string) bool {
	if path == "" {
		return false
	}
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}

	// Case-sensitive on purpose: the Java convention capitalises the suffix, and
	// a case-insensitive check would classify "Latest.java" as a test.
	for _, suffix := range []string{"Test.java", "Tests.java", "Test.kt", "Tests.kt", "Test.cs", "Tests.cs"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}

	lower := strings.ToLower(base)
	for _, suffix := range []string{
		"_test.go", "_test.py", "_test.rb", "_test.ts", "_test.js",
		"_spec.rb", ".test.ts", ".test.tsx", ".test.js", ".test.jsx",
		".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
	} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	if strings.HasPrefix(lower, "test_") || (strings.HasPrefix(lower, "test") && strings.HasSuffix(lower, ".py")) {
		return true
	}

	lowerPath := strings.ToLower(path)
	for _, segment := range []string{"/test/", "/tests/", "/__tests__/", "/spec/", "/testing/"} {
		if strings.Contains(lowerPath, segment) {
			return true
		}
	}
	return strings.HasPrefix(lowerPath, "test/") || strings.HasPrefix(lowerPath, "tests/")
}

// ref renders one scope entry as a citation.
//
// Returns "" when the item names neither a path nor a symbol, which propagates
// into an empty Evidence and drops the constraint at the exit filter. That is
// the intended outcome: an entry that identifies nothing cannot support a rule.
func (s ScopeItem) ref() string {
	switch {
	case s.Path != "" && s.Symbol != "":
		return s.Symbol + " (" + s.Path + ")"
	case s.Path != "":
		return s.Path
	default:
		return s.Symbol
	}
}

// Ref is the exported form of ref, for consumers of a contract.
//
// internal/verify cites scope entries back to the reader, and it must cite them
// in the SAME shape the contract printed them: a verification report that named
// "Foo in bar.go" against a contract that said "Foo (bar.go)" leaves the reader
// checking whether the two are the same entry.
func (s ScopeItem) Ref() string { return s.ref() }

// Ref renders a blast-radius entry as a citation.
func (r Reached) Ref() string { return r.ref() }

// Ref renders a named test as a citation.
func (t TestRef) Ref() string { return t.ref() }

func refsOfKind(scope []ScopeItem, kind string) []string {
	return refsMatching(scope, func(s ScopeItem) bool { return s.Kind == kind })
}

func refsMatching(scope []ScopeItem, pred func(ScopeItem) bool) []string {
	var out []string
	for _, s := range scope {
		if pred(s) {
			if r := s.ref(); r != "" {
				out = append(out, r)
			}
		}
	}
	return out
}

// joinEvidence renders a citation list: sorted, de-duplicated, and capped.
func joinEvidence(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	uniq := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if r == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		uniq = append(uniq, r)
	}
	if len(uniq) == 0 {
		return ""
	}
	// Sorted rather than left in rank order: evidence is a citation, and a
	// citation that reorders itself when a score shifts is not a citation.
	sort.Strings(uniq)
	if len(uniq) <= maxEvidenceRefs {
		return strings.Join(uniq, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(uniq[:maxEvidenceRefs], ", "), len(uniq)-maxEvidenceRefs)
}

func distinctPaths(scope []ScopeItem) int {
	seen := make(map[string]struct{}, len(scope))
	for _, s := range scope {
		if s.Path != "" {
			seen[s.Path] = struct{}{}
		}
	}
	return len(seen)
}

// Markdown renders a contract as the brief a person or an agent reads.
//
// TokenEstimate is not rendered: it is metadata for whoever is budgeting the
// call, and putting the price of the brief inside the brief tells the agent
// nothing about the task.
func (c Contract) Markdown() string {
	var b strings.Builder
	b.WriteString("# Task Contract\n")

	writeSection(&b, "Intent", prose(c.Intent))
	writeSection(&b, "Interpretation", prose(c.Interpretation))
	writeSection(&b, "Goal", prose(c.Goal))

	writeSection(&b, "Scope", scopeLines(c.Scope))
	writeSection(&b, "Out of scope", scopeLines(c.OutOfScope))

	var constraints []string
	for _, con := range c.Constraints {
		constraints = append(constraints, fmt.Sprintf("- %s\n  - evidence: %s", escapeStructure(con.Text), escapeStructure(con.Evidence)))
	}
	writeSection(&b, "Constraints", constraints)

	// Between scope and the checkable statements of done, because it is the
	// answer to "and what else does that touch" — a question the reader asks
	// while still looking at Scope.
	var blast []string
	for _, r := range c.BlastRadius {
		ref := r.ref()
		if ref == "" {
			continue
		}
		line := fmt.Sprintf("- `%s`", ref)
		if r.Kind != "" {
			line += " (" + r.Kind + ")"
		}
		line += fmt.Sprintf(" — depth %d", r.Depth)
		if r.Confidence != "" {
			line += ", " + r.Confidence + " confidence"
		}
		if r.IsTest {
			line += ", test code"
		}
		if r.Target != "" {
			line += fmt.Sprintf("; reached from `%s`", r.Target)
		}
		if r.Evidence != "" {
			line += "\n  - evidence: " + r.Evidence
		}
		blast = append(blast, escapeStructure(line))
	}
	if len(blast) > 0 && c.ImpactFloor != "" {
		blast = append(blast, escapeStructure(fmt.Sprintf(
			"- _walked with a `%s` confidence floor; %d symbol(s) reachable only across weaker edges are not listed._",
			c.ImpactFloor, c.ImpactHidden)))
	}
	writeSection(&b, "Blast radius", blast)

	var tests []string
	for _, t := range c.Tests {
		ref := t.ref()
		if ref == "" {
			continue
		}
		line := fmt.Sprintf("- `%s`", ref)
		if t.Covers != "" {
			line += fmt.Sprintf(" covers `%s`", t.Covers)
		}
		if t.Signal != "" {
			line += fmt.Sprintf(" — found by %s", t.Signal)
		}
		if t.Confidence != "" {
			line += fmt.Sprintf(" (%s confidence)", t.Confidence)
		}
		tests = append(tests, escapeStructure(line))
	}
	writeSection(&b, "Tests", tests)

	var acceptance []string
	for _, a := range c.Acceptance {
		if strings.TrimSpace(a) == "" {
			continue
		}
		acceptance = append(acceptance, "- [ ] "+escapeStructure(a))
	}
	writeSection(&b, "Acceptance", acceptance)

	var risks []string
	for _, r := range c.Risks {
		risks = append(risks, fmt.Sprintf("- **%s** %s\n  - evidence: %s", r.Severity, escapeStructure(r.Text), escapeStructure(r.Evidence)))
	}
	writeSection(&b, "Risks", risks)

	var unknowns []string
	for _, u := range c.Unknowns {
		if strings.TrimSpace(u) == "" {
			continue
		}
		unknowns = append(unknowns, "- "+escapeStructure(u))
	}
	writeSection(&b, "Unknowns", unknowns)

	return b.String()
}

func scopeLines(items []ScopeItem) []string {
	var out []string
	for _, s := range items {
		var head string
		switch {
		case s.Path != "" && s.Symbol != "":
			head = fmt.Sprintf("`%s` in `%s`", s.Symbol, s.Path)
		case s.Path != "":
			head = fmt.Sprintf("`%s`", s.Path)
		case s.Symbol != "":
			head = fmt.Sprintf("`%s`", s.Symbol)
		default:
			// Identifies nothing, so it cannot be acted on and must not take a
			// line that implies it can.
			continue
		}
		if s.Kind != "" {
			head += " (" + s.Kind + ")"
		}
		if why := strings.TrimSpace(s.Why); why != "" {
			head += " — " + why
		}
		// The whole composed line, not just Why: a path or symbol carrying a
		// newline would otherwise escape its backticks and start a line of its
		// own, which is the same injection by a different door.
		out = append(out, "- "+escapeStructure(head))
	}
	return out
}

// writeSection emits a header only when something follows it.
//
// An empty section is not neutral: "## Risks" with nothing under it reads as
// "we looked and found no risks", a claim this package cannot make. Omitting
// the header says only that nothing was derived, which is the truth.
func writeSection(b *strings.Builder, title string, body []string) {
	if len(body) == 0 {
		return
	}
	b.WriteString("\n## ")
	b.WriteString(title)
	b.WriteString("\n\n")
	for _, line := range body {
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// prose wraps a free-text field as a section body, dropping it when blank so
// the header goes with it.
func prose(s string) []string {
	if t := strings.TrimSpace(s); t != "" {
		return []string{escapeStructure(t)}
	}
	return nil
}

// escapeStructure stops free text from becoming document structure.
//
// A contract interpolates text the author wrote and text taken from the
// repository, and Markdown gives a line beginning with '#' the same meaning
// wherever it came from. A task named "## \n0" produced a section header with
// no title; a task naming "## Scope" would have produced a SECOND scope
// section, and an agent reading the contract has no way to tell an injected
// section from a real one.
//
// That is the same rule the MCP payloads follow — indexed text arrives as
// evidence, never in instruction position — applied to the one output that is
// itself structured. Escaping with a backslash renders the character
// literally, so the text still reads as written.
//
// Only the leading '#' is escaped. A '#' inside a line cannot open a heading,
// and escaping every one of them would mangle ordinary prose about issue
// numbers and colour codes.
func escapeStructure(s string) string {
	if !strings.Contains(s, "#") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			indent := line[:len(line)-len(trimmed)]
			lines[i] = indent + "\\" + trimmed
		}
	}
	return strings.Join(lines, "\n")
}
