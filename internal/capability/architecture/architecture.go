// Package architecture answers "does the code match the architecture we said
// we had?" — ArchMind from the product spec.
//
// # What makes this different from a linter
//
// A dependency-rule linter checks imports. This checks the GRAPH, which
// includes relationships no import reveals: an endpoint exposing a handler, a
// method writing a table, a service reading a configuration key. "The reporting
// module must not write the payments table" is an architectural rule that no
// import-based tool can see, because the write happens through a repository
// class that reporting is perfectly entitled to call.
//
// # Every violation cites its evidence
//
// A boundary crossing is reported with the file and line that causes it, the
// relationship kind, and the confidence of that relationship. That last part
// carries weight: a violation inferred from a `low`-confidence heuristic edge is
// a lead, and one on a compiler-proven edge is a fact. Presenting them the same
// way would make the report either alarmist or ignorable, and both end with it
// being switched off.
//
// # What this deliberately does not do
//
// It does not infer components. A component model that RepoCortex guessed would be
// a description of the code's current shape, and checking code against a
// description of itself finds nothing by construction. Components are declared,
// or there is no report.
//
// It does not read deployment manifests, service registries or runtime traces,
// all listed as sources in spec §26. Those describe a running system; this
// describes a repository.
package architecture

import (
	"context"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Violation is one relationship that breaks a declared rule.
type Violation struct {
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"`
	// Verdict distinguishes an explicitly forbidden dependency from one that
	// merely is not on an allow-list. They read very differently in a report:
	// the first is a rule someone wrote to stop exactly this, the second is
	// often a component nobody thought to list.
	Verdict string `json:"verdict"`

	From     string `json:"from_component"`
	To       string `json:"to_component"`
	FromName string `json:"from_symbol"`
	ToName   string `json:"to_symbol"`

	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
	FilePath   string `json:"file_path"`
	Line       int    `json:"line"`
	Detail     string `json:"detail,omitempty"`
}

// Crossing is one component-to-component dependency, with a count.
//
// Reported whether or not a rule covers it: the map of what actually depends on
// what is the useful half of an architecture review, and most crossings are
// unremarkable. Seeing them is how a missing rule gets noticed.
type Crossing struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Count     int    `json:"count"`
	Violation bool   `json:"violation"`
	// Weakest is the lowest confidence among the edges making this crossing.
	// A crossing that exists only on `low` edges may not exist at all.
	Weakest string `json:"weakest_confidence"`
}

// Report is one architecture check.
type Report struct {
	Components []ComponentSummary `json:"components"`
	Crossings  []Crossing         `json:"crossings"`
	Violations []Violation        `json:"violations"`

	// DataPaths is the sensitive-data reachability inventory: every path from
	// an endpoint to a classified table, whether or not a rule covers it.
	// Reported unconditionally for the same reason Crossings is — the map of
	// what can reach regulated data is the useful half of a compliance review,
	// and a rule nobody wrote is the usual reason an exposure is a surprise.
	DataPaths []DataPath `json:"data_paths"`
	// DataViolations are the paths a data rule forbids. Produced and explained
	// in data.go.
	DataViolations []DataViolation `json:"data_violations"`
	// DataCoverage says what the reachability check did NOT look at. A
	// compliance answer without it overstates itself.
	DataCoverage DataCoverage `json:"data_coverage"`

	// Unassigned counts symbols in no declared component. A large number means
	// the model describes only part of the repository, and every rule is
	// therefore weaker than it looks — a forbidden dependency routed through an
	// unassigned file is invisible.
	Unassigned int `json:"unassigned_symbols"`
	// EdgesConsidered is how many relationships were examined.
	EdgesConsidered int `json:"edges_considered"`
	// RulesEvaluated is how many rules the model declares.
	RulesEvaluated int `json:"rules_evaluated"`
}

// symInfo is everything the checks need about one symbol.
//
// Package-scoped rather than local to Check because the data-reachability
// traversal in data.go walks the same symbols and must not build a second,
// possibly differently-attributed, copy of this map.
type symInfo struct {
	name      string
	kind      model.EntityKind
	component string
}

// ComponentSummary is one component and its size.
type ComponentSummary struct {
	Name    string `json:"name"`
	Symbols int    `json:"symbols"`
	Files   int    `json:"files"`
}

// Registry is the read surface this capability needs.
//
// Narrow, matching the convention in internal/capability: every method here is
// one a fake must implement.
type Registry interface {
	Files(ctx context.Context, runID int64) ([]model.File, error)
	Symbols(ctx context.Context, runID int64) ([]model.Symbol, error)
	Edges(ctx context.Context, runID int64) ([]model.Edge, error)
}

var _ Registry = (*registry.Registry)(nil)

// Check evaluates a declared architecture against the indexed graph.
//
// minConfidence drops relationships weaker than the caller is willing to act
// on. It defaults to reporting everything, because the confidence is carried on
// each violation and a reader filtering for themselves is better served than
// one shown a filtered list they did not choose.
func Check(
	ctx context.Context,
	reg Registry,
	runID int64,
	m arch.Model,
	minConfidence model.Confidence,
) (Report, error) {
	var rep Report
	rep.RulesEvaluated = len(m.Rules)

	if len(m.Components) == 0 {
		// No model means no report, not an empty one. Returning "0 violations"
		// for a repository that declared no architecture would be a pass mark
		// nobody earned.
		return rep, ErrNoModel
	}

	files, err := reg.Files(ctx, runID)
	if err != nil {
		return rep, fmt.Errorf("architecture: read files: %w", err)
	}
	symbols, err := reg.Symbols(ctx, runID)
	if err != nil {
		return rep, fmt.Errorf("architecture: read symbols: %w", err)
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		return rep, fmt.Errorf("architecture: read edges: %w", err)
	}

	// Resolve every file once. ComponentOf walks glob patterns, and doing it
	// per symbol on a 100,000-symbol repository would repeat the same work for
	// every symbol in a file.
	compOfFile := make(map[int64]string, len(files))
	fileCount := map[string]int{}
	for _, f := range files {
		c := m.ComponentOf(f.Path)
		compOfFile[f.ID] = c
		if c != "" {
			fileCount[c]++
		}
	}

	symbolInfo := make(map[int64]symInfo, len(symbols))
	symCount := map[string]int{}
	for _, s := range symbols {
		c := compOfFile[s.FileID]
		symbolInfo[s.ID] = symInfo{name: s.Name, kind: s.Kind, component: c}
		if c == "" {
			rep.Unassigned++
			continue
		}
		symCount[c]++
	}

	crossings := map[string]*Crossing{}

	for i := range edges {
		e := edges[i]
		if minConfidence != "" && !e.Confidence.AtLeast(minConfidence) {
			continue
		}
		src, ok := symbolInfo[e.SrcID]
		if !ok {
			continue
		}
		dst, ok := symbolInfo[e.DstID]
		if !ok {
			continue
		}
		// A symbol outside every component cannot participate in a boundary
		// crossing: there is no boundary to cross. Counted in Unassigned above
		// so the gap is visible rather than silently narrowing the check.
		if src.component == "" || dst.component == "" || src.component == dst.component {
			continue
		}
		rep.EdgesConsidered++

		verdict, rule := m.Evaluate(src.component, dst.component)

		key := src.component + "\x00" + dst.component
		c, ok := crossings[key]
		if !ok {
			c = &Crossing{From: src.component, To: dst.component, Weakest: string(e.Confidence)}
			crossings[key] = c
		}
		c.Count++
		if weaker(e.Confidence, model.Confidence(c.Weakest)) {
			c.Weakest = string(e.Confidence)
		}
		if verdict != arch.VerdictAllowed {
			c.Violation = true
		}

		if verdict == arch.VerdictAllowed || rule == nil {
			continue
		}
		rep.Violations = append(rep.Violations, Violation{
			RuleID:     rule.ID,
			Severity:   rule.SeverityOrDefault(),
			Verdict:    string(verdict),
			From:       src.component,
			To:         dst.component,
			FromName:   src.name,
			ToName:     dst.name,
			Kind:       string(e.Kind),
			Confidence: string(e.Confidence),
			FilePath:   e.Evidence.FilePath,
			Line:       e.Evidence.Range.Start.Line,
			Detail:     e.Evidence.Detail,
		})
	}

	for name := range symCount {
		rep.Components = append(rep.Components, ComponentSummary{
			Name: name, Symbols: symCount[name], Files: fileCount[name],
		})
	}
	// A component that matched nothing is the single most useful thing this
	// report can surface: it means a pattern is wrong, and every rule naming
	// that component has been silently inert.
	for _, comp := range m.Components {
		if _, ok := symCount[comp.Name]; !ok {
			rep.Components = append(rep.Components, ComponentSummary{Name: comp.Name})
		}
	}
	sort.Slice(rep.Components, func(i, j int) bool {
		return rep.Components[i].Name < rep.Components[j].Name
	})

	for _, c := range crossings {
		rep.Crossings = append(rep.Crossings, *c)
	}
	sort.Slice(rep.Crossings, func(i, j int) bool {
		a, b := rep.Crossings[i], rep.Crossings[j]
		if a.Violation != b.Violation {
			return a.Violation // violations first
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})

	sortViolations(rep.Violations)

	// Sensitive-data reachability is a traversal, not a per-edge judgement, so
	// it runs after the crossing loop over the same symbols and edges. It is a
	// no-op — not even a graph build — for a model that classifies no data.
	if err := checkDataReachability(ctx, &rep, &m, symbolInfo, edges, minConfidence); err != nil {
		return rep, err
	}
	return rep, nil
}

// ErrNoModel means the repository declared no architecture.
var ErrNoModel = errNoModel{}

type errNoModel struct{}

func (errNoModel) Error() string {
	return "architecture: no components declared; add an `architecture:` block to .cortex.yml"
}

// sortViolations orders by severity, then by location, so the output is stable
// across runs and the worst thing is first.
//
// Stability matters more than it looks: this report is the kind of thing that
// ends up in CI, and a diff that reorders on every run makes "did this get
// worse?" unanswerable.
func sortViolations(vs []Violation) {
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	sort.Slice(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		if ra, rb := rank[a.Severity], rank[b.Severity]; ra != rb {
			return ra < rb
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.FromName < b.FromName
	})
}

func weaker(a, b model.Confidence) bool {
	if b == "" {
		return true
	}
	return a.Rank() < b.Rank()
}

// HighestSeverity returns the worst severity present, or "" when there are no
// violations. Used to decide a process exit code.
//
// Data violations rank alongside dependency violations rather than in a
// separate gate. A CI job that fails on a forbidden import but not on an
// endpoint reaching cardholder data has its priorities inverted, and a second
// `--fail-on` flag would be a second thing to forget to switch on.
func (r Report) HighestSeverity() string {
	worst := ""
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	for _, v := range r.Violations {
		if worst == "" || rank[v.Severity] < rank[worst] {
			worst = v.Severity
		}
	}
	for _, v := range r.DataViolations {
		if worst == "" || rank[v.Severity] < rank[worst] {
			worst = v.Severity
		}
	}
	return worst
}
