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
	"fmt"
	"path"
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
	locator := newCodeLocator(symbols, pathOf)
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
		//
		// The old check was strings.Contains(symbol.FQN, namespace). Measured
		// on a Java fixture: the FQN of a method is "path:name" (or a SCIP
		// string), and a namespace is "com.shop.OrderService" with dots, so the
		// namespace was never found and code.function never joined anything.
		ambiguous := 0
		if sym, n := locator.find(h); n == 1 {
			record(sym, MatchCodeAttribute, op)
			matched = true
		} else if n > 1 {
			// Two or more equally good symbols: picking one would attribute the
			// calls to code that may not have run, so say so instead.
			ambiguous = n
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
			label := op.Name
			if ambiguous > 1 {
				label = fmt.Sprintf("%s (ambiguous: %d symbols match code.function)", op.Name, ambiguous)
			}
			unmatched[label] = struct{}{}
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

// codeLocator resolves the code.* attributes of a span to indexed symbols.
type codeLocator struct {
	byName map[string][]model.Symbol
	byFile map[int64][]model.Symbol
	pathOf map[int64]string
	// filesFor caches which indexed files a span's code.filepath names: a
	// trace repeats the same few paths thousands of times.
	filesFor map[string][]int64
}

func newCodeLocator(symbols []model.Symbol, pathOf map[int64]string) *codeLocator {
	l := &codeLocator{
		byName:   map[string][]model.Symbol{},
		byFile:   map[int64][]model.Symbol{},
		pathOf:   pathOf,
		filesFor: map[string][]int64{},
	}
	for _, s := range symbols {
		if s.Kind == model.KindEndpoint || s.Kind == model.KindTable {
			continue
		}
		l.byName[s.Name] = append(l.byName[s.Name], s)
		l.byFile[s.FileID] = append(l.byFile[s.FileID], s)
	}
	return l
}

// find returns the symbol a hint names and how many symbols tie for it. A count
// of 1 is a join; 0 is no evidence; more than 1 is ambiguous and the caller must
// not pick one.
func (l *codeLocator) find(h trace.CodeHint) (model.Symbol, int) {
	// code.filepath + code.lineno is the most precise evidence: the line sits
	// inside exactly one innermost definition.
	if h.File != "" && h.Line > 0 {
		if sym, ok := l.byLine(h); ok {
			return sym, 1
		}
	}
	if h.Function == "" {
		return model.Symbol{}, 0
	}
	cands := l.byName[h.Function]
	if h.Namespace != "" {
		var narrowed []model.Symbol
		for _, s := range cands {
			if l.inNamespace(s, h.Namespace) {
				narrowed = append(narrowed, s)
			}
		}
		cands = narrowed
	}
	if len(cands) == 1 {
		return cands[0], 1
	}
	return model.Symbol{}, len(cands)
}

// byLine finds the innermost function or method in the hinted file whose line
// range holds the line. A function named by the hint must also match by name.
func (l *codeLocator) byLine(h trace.CodeHint) (model.Symbol, bool) {
	var best model.Symbol
	found := false
	bestSpan := 0
	fids, seen := l.filesFor[h.File]
	if !seen {
		for fid := range l.byFile {
			if samePath(l.pathOf[fid], h.File) {
				fids = append(fids, fid)
			}
		}
		l.filesFor[h.File] = fids
	}
	for _, fid := range fids {
		for _, s := range l.byFile[fid] {
			if s.Kind != model.KindFunction && s.Kind != model.KindMethod {
				continue
			}
			if h.Function != "" && s.Name != h.Function {
				continue
			}
			a, b := s.Range.Start.Line, s.Range.End.Line
			if h.Line < a || h.Line > b {
				continue
			}
			if span := b - a; !found || span < bestSpan {
				best, found, bestSpan = s, true, span
			}
		}
	}
	return best, found
}

// inNamespace reports whether a symbol belongs to a namespace such as
// "com.shop.OrderService", "shop::orders" or "pkg/payments". Only the last
// segment is compared against names (the leading package part is spelled
// differently per language), but the whole dotted path is also tried against
// the file path.
func (l *codeLocator) inNamespace(s model.Symbol, ns string) bool {
	last := lastSegment(ns)
	if last == "" {
		return true
	}
	if strings.Contains(s.FQN, last) {
		return true
	}
	// Owner: a class, interface, package or module in the same file whose range
	// encloses the symbol and is named like the namespace.
	for _, o := range l.byFile[s.FileID] {
		if o.ID == s.ID || o.Name != last {
			continue
		}
		switch o.Kind {
		case model.KindClass, model.KindInterface, model.KindPackage, model.KindModule, model.KindTypeAlias:
			if o.Range.Start.Line <= s.Range.Start.Line && o.Range.End.Line >= s.Range.End.Line {
				return true
			}
		}
	}
	// File path: the namespace as a package path (com.shop -> com/shop), the
	// file stem as the class name, or the directory as the Go/Python package.
	p := strings.TrimSuffix(l.pathOf[s.FileID], path.Ext(l.pathOf[s.FileID]))
	asPath := strings.NewReplacer(".", "/", "::", "/", "\\", "/", ":", "/").Replace(ns)
	if strings.HasSuffix(p, asPath) || strings.Contains(p+"/", asPath+"/") {
		return true
	}
	return path.Base(p) == last || path.Base(path.Dir(p)) == last
}

func lastSegment(ns string) string {
	f := func(r rune) bool { return r == '.' || r == '/' || r == ':' || r == '\\' || r == '#' }
	parts := strings.FieldsFunc(ns, f)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// samePath compares an indexed repo-relative path with a span's code.filepath,
// which is often absolute on the build machine: one must be a path suffix of the
// other.
func samePath(indexed, hinted string) bool {
	a := strings.TrimPrefix(path.Clean(strings.ReplaceAll(indexed, "\\", "/")), "./")
	b := strings.TrimPrefix(path.Clean(strings.ReplaceAll(hinted, "\\", "/")), "./")
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasSuffix(b, "/"+a) || strings.HasSuffix(a, "/"+b)
}
