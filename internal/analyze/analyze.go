// Package analyze turns one parsed file into API and database facts.
//
// It sits between parse and the registry: parse says what symbols a file
// declares, and this says what those symbols EXPOSE and what data they TOUCH.
// The two extractors underneath (analyze/api, analyze/db) are pure functions
// over text; this package is where their output is attached to the symbol table
// so it can become graph nodes and edges.
//
// # Why attribution happens here
//
// A SQL statement is only useful if you know which function ran it. "The
// repository touches `transactions`" is nearly worthless; "TransactionRepository.save
// writes `transactions`" is the answer to a real question. The attribution is
// line containment against the file's own symbols — the innermost definition
// whose range covers the statement — which is the same rule internal/resolve
// uses for references, and for the same reason: it is the one relationship the
// source states directly rather than implying.
//
// # Endpoints and tables are symbols
//
// Not a side table. An endpoint is a node in the same graph as the handler it
// dispatches to, and a table is a node in the same graph as the method that
// writes it, because the questions worth asking cross those boundaries: "what
// does this endpoint eventually write?" is a traversal, and it is only a
// traversal if endpoints, functions and tables are all nodes.
package analyze

import (
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	cfg "github.com/satyendrakumarsingh/repocortex/internal/analyze/config"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/db"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/deps"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/events"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Result is everything one file contributed beyond its ordinary symbols.
type Result struct {
	// Symbols are the endpoint and table nodes this file declares. They carry
	// negative placeholder ids, matching the contract registry.ReplaceFile
	// defines for symbols that do not exist yet.
	Symbols []model.Symbol

	// Exposures are endpoint -> handler relationships. Both ends are in this
	// file, so they resolve without a second pass.
	Exposures []Exposure

	// Accesses are table reads and writes. The table may be declared anywhere,
	// so these are staged and bound later — see migration 0003.
	Accesses []Access

	// ConfigReads are configuration lookups in code. Bound to declared keys in
	// the same second pass, for the same reason — see migration 0004.
	ConfigReads []ConfigRead

	// EventBindings are discovered event bus publications and subscriptions.
	EventBindings []events.EventBinding
}

// ConfigRead is one place in code that reads a configuration key.
//
// There is no value here and there must never be one. See
// internal/analyze/config.
type ConfigRead struct {
	Key          string
	Mechanism    string
	SrcSymbolFQN string
	Line         int
}

// Exposure is an endpoint dispatching to a handler.
type Exposure struct {
	// EndpointFQN identifies the endpoint symbol in Result.Symbols.
	EndpointFQN string
	// HandlerName is the function or method the framework names. Empty when the
	// framework's syntax did not reveal one, which is reported rather than
	// guessed at.
	HandlerName string
	Line        int
	Confidence  model.Confidence
	Detail      string
}

// Access is one statement's use of one table.
type Access struct {
	Table string
	Mode  db.Access
	Verb  string
	// SrcSymbolFQN is the definition whose body contained the statement, or ""
	// when the statement sits outside every definition.
	SrcSymbolFQN string
	Line         int
	Certain      bool
}

// tableFQN is how a table node is named.
//
// Prefixed rather than bare so a table called `user` cannot collide with a
// class called `user` in the symbol table, and stable across files so every
// access to `transactions` binds to one node no matter which migration
// declared it.
func tableFQN(name string) string { return "table:" + strings.ToLower(name) }

// TableName recovers a table name from its FQN, for callers rendering reports.
func TableName(fqn string) string { return strings.TrimPrefix(fqn, "table:") }

// IsTableFQN reports whether an FQN names a table node.
func IsTableFQN(fqn string) bool { return strings.HasPrefix(fqn, "table:") }

// endpointFQN is how an endpoint node is named: the route as a person would
// write it in a ticket, qualified by file so two services in one repository
// exposing the same path remain distinct nodes.
func endpointFQN(path, signature string) string { return path + "#" + signature }

// File analyses one parsed file.
//
// symbols are the file's ordinary symbols, used for line-containment
// attribution. They are not modified.
func File(path string, src []byte, symbols []model.Symbol) Result {
	var res Result

	enclosing := newEnclosureIndex(path, symbols)

	for _, route := range api.Extract(path, src) {
		sig := route.Signature()
		fqn := endpointFQN(path, sig)
		res.Symbols = append(res.Symbols, model.Symbol{
			ID:   placeholderID(len(res.Symbols)),
			Kind: model.KindEndpoint,
			Name: sig,
			FQN:  fqn,
			Range: model.Range{
				Start: model.Position{Line: route.Line},
				End:   model.Position{Line: route.Line},
			},
			// An endpoint is the public face of a service; treating it as
			// unexported would hide it from every report that filters on
			// exported symbols, which is most of them.
			Exported:   true,
			Confidence: route.Confidence,
			Source:     model.SourceSourceCode,
		})
		res.Exposures = append(res.Exposures, Exposure{
			EndpointFQN: fqn,
			HandlerName: route.Handler,
			Line:        route.Line,
			Confidence:  route.Confidence,
			Detail:      string(route.Framework) + " route",
		})
	}

	for _, stmt := range statementsIn(path, src) {
		for _, table := range stmt.Tables {
			if stmt.Access == db.AccessDDL {
				// A DDL statement DECLARES the table, so this file is where the
				// node lives. Declaring it from a query instead would put the
				// table's canonical location in whichever file happened to be
				// parsed first.
				res.Symbols = append(res.Symbols, tableSymbol(table, stmt, len(res.Symbols)))
			}
			res.Accesses = append(res.Accesses, Access{
				Table:        table,
				Mode:         stmt.Access,
				Verb:         stmt.Verb,
				SrcSymbolFQN: enclosing.at(stmt.Line),
				Line:         stmt.Line,
				Certain:      stmt.Certain,
			})
		}
		for _, col := range stmt.Columns {
			if stmt.Access != db.AccessDDL || len(stmt.Tables) == 0 {
				continue
			}
			res.Symbols = append(res.Symbols, columnSymbol(stmt.Tables[0], col, stmt, len(res.Symbols)))
		}
	}

	// Configuration keys a file DECLARES become nodes; reads become staged
	// usages. A file is one or the other — a .env declares and does not read,
	// and source reads and does not declare — so these never both fire.
	for _, key := range cfg.Declarations(path, src) {
		res.Symbols = append(res.Symbols, model.Symbol{
			ID:   placeholderID(len(res.Symbols)),
			Kind: model.KindConfiguration,
			Name: key.Name,
			FQN:  ConfigFQN(key.Name),
			Range: model.Range{
				Start: model.Position{Line: key.Line},
				End:   model.Position{Line: key.Line},
			},
			Exported:   true,
			Confidence: model.ConfidenceHigh,
			Source:     model.SourceSourceCode,
		})
	}
	for _, use := range cfg.Usages(path, src) {
		res.ConfigReads = append(res.ConfigReads, ConfigRead{
			Key:          use.Name,
			Mechanism:    use.Mechanism,
			SrcSymbolFQN: enclosing.at(use.Line),
			Line:         use.Line,
		})
	}

	// A dependency manifest declares nodes but no relationships. Usage is
	// answered at query time by joining against file_imports, which the
	// indexer already records — building a second copy of that data here would
	// be a cache with nothing to gain.
	if m, ok := deps.Parse(path, src); ok {
		for _, d := range m.Dependencies {
			res.Symbols = append(res.Symbols, model.Symbol{
				ID:   placeholderID(len(res.Symbols)),
				Kind: model.KindDependency,
				Name: d.Name,
				FQN:  DependencyFQN(d.Name),
				Range: model.Range{
					Start: model.Position{Line: d.Line},
					End:   model.Position{Line: d.Line},
				},
				// The declared version, carried on the symbol so a report can
				// state it without re-reading the manifest. Signature is the
				// field that already means "how this thing is declared".
				Signature:  string(d.Ecosystem) + " " + d.Version,
				Doc:        string(d.Scope),
				Exported:   !d.Indirect,
				Confidence: model.ConfidenceHigh,
				Source:     model.SourceSourceCode,
			})
		}
	}

	for _, b := range events.Extract(path, src) {
		if b.SrcSymbolFQN == "" {
			b.SrcSymbolFQN = enclosing.at(b.Line)
		}
		res.EventBindings = append(res.EventBindings, b)
		res.Symbols = append(res.Symbols, model.Symbol{
			ID:   placeholderID(len(res.Symbols)),
			Kind: model.KindEvent,
			Name: b.Topic,
			FQN:  EventFQN(b.Broker, b.Topic),
			Range: model.Range{
				Start: model.Position{Line: b.Line},
				End:   model.Position{Line: b.Line},
			},
			Signature:  string(b.Broker) + " " + string(b.Direction),
			Doc:        string(b.Broker) + " " + string(b.Direction) + " " + b.Topic,
			Exported:   true,
			Confidence: b.Confidence,
			Source:     model.SourceSourceCode,
		})
	}

	res.Symbols = dedupeSymbols(res.Symbols)
	return res
}

// ConfigFQN and DependencyFQN namespace their nodes so a configuration key
// called `server` cannot collide with a class of the same name.
func ConfigFQN(name string) string     { return "config:" + name }
func DependencyFQN(name string) string { return "dep:" + name }

// EventFQN produces a namespaced FQN for an event bus topic.
func EventFQN(broker events.Broker, topic string) string {
	return "event:" + string(broker) + ":" + strings.ToLower(topic)
}

// IsEventFQN reports whether an FQN names an event topic.
func IsEventFQN(fqn string) bool {
	return strings.HasPrefix(fqn, "event:")
}

func tableSymbol(name string, stmt located, ordinal int) model.Symbol {
	return model.Symbol{
		ID:   placeholderID(ordinal),
		Kind: model.KindTable,
		Name: strings.ToLower(name),
		FQN:  tableFQN(name),
		Range: model.Range{
			Start: model.Position{Line: stmt.Line},
			End:   model.Position{Line: stmt.Line},
		},
		Exported:   true,
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceSourceCode,
	}
}

func columnSymbol(table, col string, stmt located, ordinal int) model.Symbol {
	return model.Symbol{
		ID:   placeholderID(ordinal),
		Kind: model.KindColumn,
		Name: strings.ToLower(col),
		FQN:  tableFQN(table) + "." + strings.ToLower(col),
		Range: model.Range{
			Start: model.Position{Line: stmt.Line},
			End:   model.Position{Line: stmt.Line},
		},
		Exported:   true,
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceSourceCode,
	}
}

// placeholderID returns the negative id the registry expects for a symbol that
// does not exist yet. Offset well below the ordinary range so a collision with
// a caller's own placeholders is impossible.
func placeholderID(ordinal int) int64 { return -int64(ordinal) - 1000 }

// located is a statement with the line it was found on.
type located struct {
	db.Statement
	Line int
}

// enclosureIndex answers "which definition contains this line".
type enclosureIndex struct {
	// spans are sorted by start line, then by descending end line, so scanning
	// forward and keeping the last match yields the INNERMOST enclosing symbol.
	// A statement inside a method inside a class belongs to the method: the
	// class is true but useless, and "TransactionService writes transactions"
	// hides which of its six methods does.
	spans []span
}

type span struct {
	fqn        string
	start, end int
}

func newEnclosureIndex(path string, symbols []model.Symbol) *enclosureIndex {
	idx := &enclosureIndex{}
	for _, s := range symbols {
		if s.Range.Start.Line == 0 {
			continue
		}
		// A parser leaves FQN empty and the sink fills it in on write, so the
		// name to attribute against is the one the sink WILL assign, not the
		// empty one visible here. Sharing model.DefaultFQN is what keeps the
		// two ends agreeing; when they did not, every access resolved to no
		// symbol and no edge was ever written.
		fqn := s.FQN
		if fqn == "" {
			fqn = model.DefaultFQN(path, s.Name)
		}
		end := s.Range.End.Line
		if end < s.Range.Start.Line {
			end = s.Range.Start.Line
		}
		idx.spans = append(idx.spans, span{fqn: fqn, start: s.Range.Start.Line, end: end})
	}
	sort.Slice(idx.spans, func(i, j int) bool {
		if idx.spans[i].start != idx.spans[j].start {
			return idx.spans[i].start < idx.spans[j].start
		}
		return idx.spans[i].end > idx.spans[j].end
	})
	return idx
}

func (e *enclosureIndex) at(line int) string {
	best := ""
	bestWidth := 1 << 30
	for _, s := range e.spans {
		if s.start > line {
			break
		}
		if line > s.end {
			continue
		}
		if w := s.end - s.start; w < bestWidth {
			best, bestWidth = s.fqn, w
		}
	}
	return best
}

// dedupeSymbols collapses repeats by FQN, keeping the first.
//
// A migration that creates a table and then indexes it names the table twice,
// and a repository with six queries against `transactions` would otherwise
// declare six identical nodes.
func dedupeSymbols(in []model.Symbol) []model.Symbol {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s.FQN]; ok {
			continue
		}
		seen[s.FQN] = struct{}{}
		out = append(out, s)
	}
	return out
}

// Wants reports whether a file is worth reading for analysis even when no
// tree-sitter grammar claims it.
//
// This exists because of a real gap. The indexer returns early for a file with
// no grammar — sensible, since there are no symbols to extract — and that early
// return skipped the source read entirely. But the files that carry
// configuration and dependency declarations are exactly the ones no grammar
// claims: .env, .properties, most YAML. The result was a `config` report that
// found nine keys read by code and zero declared, on a repository with a .env
// sitting in its root.
//
// Analysis here needs TEXT, not a syntax tree. So the pipeline asks this before
// giving up, and reads the file when the answer is yes.
func Wants(path string) bool {
	return cfg.IsConfigFile(path) || deps.IsManifest(path)
}
