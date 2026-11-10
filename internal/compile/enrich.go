package compile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
)

// Bounds on what the two ports may add to a contract.
//
// The scope cap exists because an unbounded scope is not a scope; the same
// argument applies twice over to a blast radius, which is drawn from a
// traversal that can reach thousands of symbols. A brief listing 300 callers
// has told the agent nothing it could not have got from `cortex impact`, and
// has buried the twelve entries that were the point.
const (
	// maxImpactTargets is how many scope symbols are analysed. The scope is
	// already ranked and totally ordered, so this takes the top few rather than
	// an arbitrary few.
	maxImpactTargets = 3

	// maxBlastItems bounds the reported blast radius. The cut is named in
	// Unknowns, never silent.
	maxBlastItems = 20

	// maxContractTests bounds the covering tests listed.
	maxContractTests = 12
)

// Confidence vocabulary, mirrored from model rather than imported for the
// reason given above kindTest, and pinned by the same test.
const (
	confidenceExact  = "exact"
	confidenceHigh   = "high"
	confidenceMedium = "medium"
)

// reachSummary is everything the impact port contributed to one contract.
type reachSummary struct {
	// consulted records that a port was supplied at all, which is what
	// separates "nothing is affected" from "nobody looked".
	consulted bool

	targets    []string
	unresolved []string

	items []Reached
	tests []TestRef

	floor     string
	hidden    int
	truncated bool
	dropped   int
}

// gatherReach asks the impact port about the top scope symbols.
//
// Order is fixed by the scope, which is already a total sort, so the same pack
// asks the same questions in the same order on every run.
func gatherReach(ctx context.Context, view ImpactView, scope []ScopeItem) (reachSummary, error) {
	var sum reachSummary
	if view == nil {
		return sum, nil
	}
	sum.consulted = true

	asked := make(map[string]struct{}, maxImpactTargets)
	for _, s := range scope {
		if len(sum.targets) >= maxImpactTargets {
			break
		}
		name := strings.TrimSpace(s.Symbol)
		if name == "" || s.Kind == kindFile {
			// A file entity is not a symbol the graph can be walked from, and
			// asking for one by path would resolve to something else or to
			// nothing at all.
			continue
		}
		if _, seen := asked[name]; seen {
			continue
		}
		asked[name] = struct{}{}

		r, err := view.Reach(ctx, name)
		if err != nil {
			// Fail rather than degrade. A contract that quietly lost its impact
			// analysis looks exactly like one where the change reaches nothing,
			// and that is the single most dangerous thing this file could emit.
			return reachSummary{}, fmt.Errorf("compile: impact analysis for %q: %w", name, err)
		}
		sum.targets = append(sum.targets, name)
		if !r.Resolved {
			sum.unresolved = append(sum.unresolved, name)
			continue
		}
		sum.truncated = sum.truncated || r.Truncated
		sum.hidden += r.Hidden
		if r.Floor != "" && weakerConfidence(r.Floor, sum.floor) {
			// The weakest floor any walk ran with: the reader needs to know the
			// least filtered claim in the contract, not the average.
			sum.floor = r.Floor
		}

		resolved := r.Target
		if strings.TrimSpace(resolved) == "" {
			resolved = name
		}
		for _, c := range r.Callers {
			c.Target = resolved
			sum.items = append(sum.items, c)
		}
		for _, t := range r.Tests {
			t.Covers = resolved
			sum.tests = append(sum.tests, t)
		}
	}

	sum.items, sum.dropped = normaliseReached(sum.items)
	sum.tests = normaliseTests(sum.tests)
	return sum, nil
}

// normaliseReached sorts, de-duplicates and caps the blast radius.
//
// The sort is total and the de-duplication keeps the SHALLOWEST, strongest
// claim about each symbol: a symbol reached at depth 1 by one target and at
// depth 3 by another is one thing to look at, and reporting the depth-3 path
// for it would understate what is known.
func normaliseReached(in []Reached) ([]Reached, int) {
	if len(in) == 0 {
		return nil, 0
	}
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		switch {
		case a.Depth != b.Depth:
			return a.Depth < b.Depth
		case confidenceRank(a.Confidence) != confidenceRank(b.Confidence):
			return confidenceRank(a.Confidence) > confidenceRank(b.Confidence)
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Symbol != b.Symbol:
			return a.Symbol < b.Symbol
		default:
			return a.Target < b.Target
		}
	})

	out := make([]Reached, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, r := range in {
		if strings.TrimSpace(r.Symbol) == "" && strings.TrimSpace(r.Path) == "" {
			// Identifies nothing, so it cannot be checked and must not occupy a
			// line implying it can.
			continue
		}
		key := r.Path + "\x00" + r.Symbol
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	if len(out) <= maxBlastItems {
		return out, 0
	}
	return out[:maxBlastItems], len(out) - maxBlastItems
}

func normaliseTests(in []TestRef) []TestRef {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		switch {
		case confidenceRank(a.Confidence) != confidenceRank(b.Confidence):
			return confidenceRank(a.Confidence) > confidenceRank(b.Confidence)
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Name != b.Name:
			return a.Name < b.Name
		default:
			return a.Covers < b.Covers
		}
	})
	out := make([]TestRef, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, t := range in {
		if strings.TrimSpace(t.Name) == "" && strings.TrimSpace(t.Path) == "" {
			continue
		}
		key := t.Path + "\x00" + t.Name + "\x00" + t.Covers
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	if len(out) > maxContractTests {
		out = out[:maxContractTests]
	}
	return out
}

// architectureConstraints turns the declared rules that govern the scope into
// constraints.
//
// NOTHING HERE IS INFERRED. A rule is emitted only when a file already in scope
// resolves to a component the rule constrains, and the constraint text is the
// rule's own id, components and description. The compiler is not deciding that
// the change will violate anything — it cannot, the change does not exist yet —
// it is stating which written-down rules the change is now within reach of.
func architectureConstraints(view ArchitectureView, scope []ScopeItem) []Constraint {
	if view == nil || len(scope) == 0 {
		return nil
	}

	// Resolve every scope path once, and keep the refs per component so the
	// constraint can cite the file that put the rule in play.
	refsByComponent := map[string][]string{}
	var components []string
	for _, s := range scope {
		if s.Path == "" {
			continue
		}
		comp := view.ComponentOf(s.Path)
		if comp == "" {
			continue
		}
		if _, ok := refsByComponent[comp]; !ok {
			components = append(components, comp)
		}
		if r := s.ref(); r != "" {
			refsByComponent[comp] = append(refsByComponent[comp], r)
		}
	}
	if len(components) == 0 {
		return nil
	}
	sort.Strings(components)

	var out []Constraint
	// Declaration order, not rule id order: the config file is a document its
	// author reads top to bottom, and a contract that reorders their rules
	// makes the two harder to check against each other.
	for _, rule := range view.Rules() {
		var hit []string
		for _, comp := range components {
			if containsFold(rule.From, comp) {
				hit = append(hit, comp)
			}
		}
		if len(hit) == 0 {
			continue
		}
		var refs []string
		for _, comp := range hit {
			refs = append(refs, refsByComponent[comp]...)
		}
		text := ruleText(rule, hit)
		if text == "" {
			continue
		}
		out = append(out, Constraint{
			Text:     text,
			Evidence: architectureEvidence(rule, hit, refs),
		})
	}
	return out
}

// ruleText states one rule in the second person, without editorialising it.
func ruleText(rule arch.Rule, from []string) string {
	subject := strings.Join(from, ", ")
	var b strings.Builder
	switch {
	case len(rule.Forbidden) > 0:
		fmt.Fprintf(&b, "Declared architecture rule %q: %s must not depend on %s.",
			ruleLabel(rule), subject, strings.Join(rule.Forbidden, ", "))
	case len(rule.AllowedTo) > 0:
		fmt.Fprintf(&b, "Declared architecture rule %q: %s may depend only on %s.",
			ruleLabel(rule), subject, strings.Join(rule.AllowedTo, ", "))
	default:
		// A rule naming neither a forbidden nor an allowed set constrains
		// nothing, and printing it as a requirement would be this package
		// inventing the requirement.
		return ""
	}
	if d := strings.TrimSpace(rule.Description); d != "" {
		b.WriteString(" ")
		b.WriteString(d)
	}
	return b.String()
}

func ruleLabel(rule arch.Rule) string {
	if id := strings.TrimSpace(rule.ID); id != "" {
		return id
	}
	return "(unnamed rule)"
}

func architectureEvidence(rule arch.Rule, from, refs []string) string {
	parts := []string{
		fmt.Sprintf("architecture rule %s (severity %s) in the declared component model",
			ruleLabel(rule), rule.SeverityOrDefault()),
		fmt.Sprintf("scope in component %s", strings.Join(from, ", ")),
	}
	if ev := joinEvidence(refs); ev != "" {
		parts = append(parts, ev)
	}
	return strings.Join(parts, "; ")
}

func containsFold(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), want) {
			return true
		}
	}
	return false
}

// confidenceRank mirrors model.Confidence.Rank over the string form this
// package carries. Unknown values rank below `low`, which is the safe end: an
// unrecognised label must never sort as if it were proven.
func confidenceRank(c string) int {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case confidenceExact:
		return 4
	case confidenceHigh:
		return 3
	case confidenceMedium:
		return 2
	case confidenceLow:
		return 1
	default:
		return 0
	}
}

// weakerConfidence reports whether a is weaker than b, treating an unset b as
// "anything is weaker".
func weakerConfidence(a, b string) bool {
	if strings.TrimSpace(b) == "" {
		return true
	}
	return confidenceRank(a) < confidenceRank(b)
}

// ref renders one reached symbol as a citation, in the same shape ScopeItem
// uses so the two lists read as one document.
func (r Reached) ref() string {
	switch {
	case r.Symbol != "" && r.Path != "":
		return r.Symbol + " (" + r.Path + ")"
	case r.Path != "":
		return r.Path
	default:
		return r.Symbol
	}
}

func (t TestRef) ref() string {
	switch {
	case t.Name != "" && t.Path != "":
		return t.Name + " (" + t.Path + ")"
	case t.Path != "":
		return t.Path
	default:
		return t.Name
	}
}

// blastRefs renders the blast radius as citations.
func blastRefs(items []Reached, pred func(Reached) bool) []string {
	var out []string
	for _, r := range items {
		if pred != nil && !pred(r) {
			continue
		}
		if ref := r.ref(); ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

// outsideScope reports whether a reached symbol lands in a file the contract
// does not already list. This is the only interesting half of a blast radius in
// a task contract: reach INTO the scope is the work, reach OUTSIDE it is the
// part the author has not agreed to yet.
func outsideScope(scope []ScopeItem, r Reached) bool {
	if r.Path == "" {
		return false
	}
	for _, s := range scope {
		if s.Path == r.Path {
			return false
		}
	}
	return true
}
