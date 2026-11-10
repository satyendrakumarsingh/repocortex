// Package runtime correlates observed traces with the indexed graph.
//
// It answers the question static analysis cannot: not "what could run", but
// "what did". An endpoint the index knows about and no trace ever reached is
// either dead or untested, and those are different conversations — but you
// cannot start either from the static graph alone.
//
// # Why correlation is heuristic, and labelled as such
//
// A span says `http.route = "/payment/callback"`; the index says an endpoint
// symbol is named `POST /payment/callback`. Matching them is string work, not
// proof. OpenTelemetry's `code.function` attribute is better evidence and is
// preferred when present, but most services do not set it. So every correlation
// carries HOW it was made, and a report that cannot say why it matched says so
// rather than presenting a guess as a measurement.
//
// # What this deliberately does not do
//
// It does not sample, collect or receive telemetry. It reads an exported file.
// A service that must be running for RepoCortex to work is a service RepoCortex has
// no business requiring, and the export is the artefact CI already produces.
//
// It does not persist spans. Runtime data ages out in hours while an index
// describes a commit, and storing them together would produce a graph whose
// edges are true at different times — the single most confusing thing a
// knowledge graph can do.
package runtime

import (
	"context"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/trace"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// MatchKind records how a span was tied to code, so a reader can weigh it.
type MatchKind string

const (
	// MatchCodeAttribute is an explicit code.function/code.namespace attribute.
	// The strongest evidence available: the instrumentation named the symbol.
	MatchCodeAttribute MatchKind = "code-attribute"
	// MatchRoute is an http.route matching an endpoint the index extracted.
	MatchRoute MatchKind = "route"
	// MatchTable is a db.sql.table matching a table the index extracted.
	MatchTable MatchKind = "table"
	// MatchName is the span name matching a symbol name. Weakest, and reported
	// as such: span names are free text and collide freely.
	MatchName MatchKind = "name"
)

// Observed is one indexed entity that a trace reached.
type Observed struct {
	SymbolID int64     `json:"symbol_id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Path     string    `json:"path"`
	Match    MatchKind `json:"match"`
	Calls    int       `json:"calls"`
	Errors   int       `json:"errors"`
	P95Nanos int64     `json:"p95_nanos"`
}

// Report is one correlation pass.
type Report struct {
	Spans    int      `json:"spans"`
	Traces   int      `json:"traces"`
	Services []string `json:"services"`

	// Exercised are indexed entities a trace reached.
	Exercised []Observed `json:"exercised"`
	// ColdEndpoints are endpoints the index knows and no span reached. The most
	// actionable output here: an endpoint nothing calls is either dead code or
	// an untested path, and both are worth knowing before a refactor.
	ColdEndpoints []Observed `json:"cold_endpoints"`
	// Unmatched are span operation names that matched nothing indexed. A large
	// number means the correlation is weak, not that the code is absent, so it
	// is reported rather than quietly dropped.
	Unmatched []string `json:"unmatched"`
}

// Registry is the read surface this capability needs.
type Registry interface {
	Files(ctx context.Context, runID int64) ([]model.File, error)
	Symbols(ctx context.Context, runID int64) ([]model.Symbol, error)
}

var _ Registry = (*registry.Registry)(nil)

// maxUnmatchedSamples bounds the retained sample of unmatched operations. Long
// enough to see a pattern, short enough that a wholly-uncorrelated trace does
// not produce a report longer than the trace.
const maxUnmatchedSamples = 30

// Correlate ties observed spans to indexed symbols.
func Correlate(ctx context.Context, reg Registry, runID int64, spans []trace.Span) (Report, error) {
	var rep Report

	files, err := reg.Files(ctx, runID)
	if err != nil {
		return rep, err
	}
	pathOf := make(map[int64]string, len(files))
	for _, f := range files {
		pathOf[f.ID] = f.Path
	}

	symbols, err := reg.Symbols(ctx, runID)
	if err != nil {
		return rep, err
	}

	// Three indexes, because three different span attributes point at three
	// different kinds of node. Built once: a trace export routinely holds tens
	// of thousands of spans, and scanning the symbol table per span would make
	// the cost quadratic in a repository's size.
	byName := map[string][]model.Symbol{}
	endpoints := map[string]model.Symbol{}
	tables := map[string]model.Symbol{}
	for _, s := range symbols {
		byName[s.Name] = append(byName[s.Name], s)
		switch s.Kind {
		case model.KindEndpoint:
			// An endpoint node is named "POST /payment/callback"; a span
			// carries the route alone. Index by route so both spellings meet.
			if _, route, ok := splitEndpointName(s.Name); ok {
				endpoints[route] = s
			}
		case model.KindTable:
			tables[strings.ToLower(s.Name)] = s
		}
	}

	summary := trace.Summarise(spans)
	rep.Spans = summary.Spans
	rep.Traces = summary.Traces
	rep.Services = summary.Services

	type agg struct {
		sym   model.Symbol
		match MatchKind
		calls int
		errs  int
		p95   int64
	}
	hits := map[int64]*agg{}
	unmatched := map[string]struct{}{}

	record := func(s model.Symbol, kind MatchKind, op trace.Operation) {
		a, ok := hits[s.ID]
		if !ok {
			a = &agg{sym: s, match: kind}
			hits[s.ID] = a
		}
		// A stronger match upgrades a weaker one. An operation matched by name
		// and later by code.function is the same fact with better evidence, and
		// keeping the weaker label would understate what is known.
		if matchRank(kind) < matchRank(a.match) {
			a.match = kind
		}
		a.calls += op.Calls
		a.errs += op.Errors
		if op.P95Nanos > a.p95 {
			a.p95 = op.P95Nanos
		}
	}

	for _, op := range summary.Operations {
		h := op.Hint
		matched := false

		// Strongest first. Once an operation is tied to a symbol by an explicit
		// code attribute, matching it again by name would double-count it.
		if h.Function != "" {
			for _, s := range byName[h.Function] {
				if h.Namespace == "" || strings.Contains(s.FQN, h.Namespace) {
					record(s, MatchCodeAttribute, op)
					matched = true
					break
				}
			}
		}
		if !matched && h.Route != "" {
			if s, ok := endpoints[h.Route]; ok {
				record(s, MatchRoute, op)
				matched = true
			}
		}
		if !matched && h.DBTable != "" {
			if s, ok := tables[strings.ToLower(h.DBTable)]; ok {
				record(s, MatchTable, op)
				matched = true
			}
		}
		if !matched {
			// Span names are free text. Matching one to a symbol is the weakest
			// evidence available and is only attempted when nothing better
			// exists, and only when the name is unique — a span called "handle"
			// tells you nothing about which of nine handlers ran.
			if cands := byName[op.Name]; len(cands) == 1 {
				record(cands[0], MatchName, op)
				matched = true
			}
		}
		if !matched && len(unmatched) < maxUnmatchedSamples {
			unmatched[op.Name] = struct{}{}
		}
	}

	for _, a := range hits {
		rep.Exercised = append(rep.Exercised, Observed{
			SymbolID: a.sym.ID, Name: a.sym.Name, Kind: string(a.sym.Kind),
			Path: pathOf[a.sym.FileID], Match: a.match,
			Calls: a.calls, Errors: a.errs, P95Nanos: a.p95,
		})
	}
	sort.Slice(rep.Exercised, func(i, j int) bool {
		if rep.Exercised[i].Calls != rep.Exercised[j].Calls {
			return rep.Exercised[i].Calls > rep.Exercised[j].Calls
		}
		return rep.Exercised[i].Name < rep.Exercised[j].Name
	})

	for _, s := range symbols {
		if s.Kind != model.KindEndpoint {
			continue
		}
		if _, hit := hits[s.ID]; hit {
			continue
		}
		rep.ColdEndpoints = append(rep.ColdEndpoints, Observed{
			SymbolID: s.ID, Name: s.Name, Kind: string(s.Kind), Path: pathOf[s.FileID],
		})
	}
	sort.Slice(rep.ColdEndpoints, func(i, j int) bool {
		return rep.ColdEndpoints[i].Name < rep.ColdEndpoints[j].Name
	})

	for op := range unmatched {
		rep.Unmatched = append(rep.Unmatched, op)
	}
	sort.Strings(rep.Unmatched)
	return rep, nil
}

// splitEndpointName splits "POST /payment/callback" into its parts.
func splitEndpointName(name string) (method, route string, ok bool) {
	i := strings.IndexByte(name, ' ')
	if i <= 0 || i+1 >= len(name) {
		return "", "", false
	}
	return name[:i], name[i+1:], true
}

func matchRank(k MatchKind) int {
	switch k {
	case MatchCodeAttribute:
		return 0
	case MatchRoute:
		return 1
	case MatchTable:
		return 2
	case MatchName:
		return 3
	}
	return 4
}
