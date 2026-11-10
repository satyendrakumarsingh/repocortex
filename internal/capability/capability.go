// Package capability holds RepoCortex's query-layer capabilities: the questions
// an agent actually asks of an indexed repository.
//
// ImpactMind ("what breaks if I change this?") and TestMind ("which tests cover
// this?") live in subpackages here. They are QUERY LAYERS OVER THE SHARED
// GRAPH, not separate products — neither parses, neither writes, and neither
// owns a schema. Everything they know comes from registry and graph, which is
// what keeps a single index answering both.
//
// # Why this package exists at all
//
// Both capabilities need the same three things: a way to turn a user-supplied
// symbol name into a symbol id, a hydrated view of the symbol table (ids alone
// are useless in a report), and a shared, non-negotiable statement of what the
// answer does NOT cover. Duplicating any of those would let the two drift, and
// the one most dangerous to drift on is the last.
//
// # The honesty contract
//
// A static index resolves only part of any repository. An impact analysis that
// under-reports makes a developer confident and wrong; one that over-reports
// gets ignored. So every report produced under this package carries a
// Completeness value, and Completeness is constructed so that the lower-bound
// statement and the dynamic-dispatch statement CANNOT be omitted — they are
// appended by NewCompleteness, not by each caller remembering to. Spec §20.
//
// # Testability
//
// Deps is deliberately two narrow interfaces rather than *registry.Registry and
// *graph.Graph. That is not ceremony: it means a capability test can assert
// "confidence compounds along a path" against a hand-built fake in
// microseconds, instead of standing up SQLite and hoping the fixture expresses
// what the assertion is about. The concrete types satisfy the interfaces
// structurally, checked at compile time below.
package capability

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Registry is the read surface the capabilities need from the registry.
//
// Narrow on purpose. Every method added here is a method a fake must implement,
// and a fake that has grown to mirror the real registry has stopped being a
// test aid. Nothing here writes.
type Registry interface {
	FindSymbolsByName(ctx context.Context, runID int64, name, kind string, limit int) ([]registry.SymbolHit, error)
	LoadSymbolIndex(ctx context.Context, runID int64) (map[string][]registry.SymbolIndexEntry, error)
	ResolutionStats(ctx context.Context, runID int64) (registry.ResolutionStats, error)
}

// Graph is the traversal surface the capabilities need.
//
// Note what is absent: there is no method here that returns edges in bulk, and
// no capability implements its own breadth-first search. Traversal bounds,
// cycle termination and path-confidence arithmetic all live in internal/graph
// and are used, not reimplemented — a second BFS would be a second place for
// "reports depth-3 reachability as high confidence" to be reintroduced.
type Graph interface {
	Traverse(ctx context.Context, q graph.Query) (graph.Result, error)
	Neighbours(ctx context.Context, symbolID int64, dir graph.Direction,
		kinds []model.EdgeKind, minConf model.Confidence) ([]graph.Neighbour, error)
}

// Compile-time proof that the production types satisfy the interfaces. If a
// signature in registry or graph changes, this file fails to build rather than
// the wiring failing at run time.
var (
	_ Registry = (*registry.Registry)(nil)
	_ Graph    = (*graph.Graph)(nil)
)

// Deps is what a capability is given instead of a database handle.
//
// It carries a memoised symbol index because both capabilities need the whole
// symbol table hydrated (names, paths, export status) and re-reading ~100k rows
// per query would dominate every measurement in this package. The cache is
// keyed by run id, so an index swap invalidates it automatically rather than
// serving names from a run that CommitRun has already retired.
//
// A zero Deps (struct literal) is usable; it simply loads the symbol index on
// every call. Long-lived callers — the MCP server especially — should use
// NewDeps.
type Deps struct {
	Registry Registry
	Graph    Graph

	// cache is a pointer so Deps stays copyable by value while still sharing
	// one index between the copies. Guarded internally; safe for concurrent use.
	cache *symbolCache
}

// NewDeps returns Deps with symbol-index memoisation enabled.
func NewDeps(r Registry, g Graph) Deps {
	return Deps{Registry: r, Graph: g, cache: newSymbolCache()}
}

// Validate reports whether the dependencies needed to answer a query are
// present, so a misconfigured server fails with a sentence rather than a nil
// dereference three frames down.
func (d Deps) Validate() error {
	if d.Registry == nil {
		return errors.New("capability: Deps.Registry is nil")
	}
	if d.Graph == nil {
		return errors.New("capability: Deps.Graph is nil")
	}
	return nil
}

// SymbolIndex returns the hydrated symbol table for runID, loading it once.
func (d Deps) SymbolIndex(ctx context.Context, runID int64) (*SymbolIndex, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	load := func() (*SymbolIndex, error) {
		byName, err := d.Registry.LoadSymbolIndex(ctx, runID)
		if err != nil {
			return nil, fmt.Errorf("capability: load symbol index for run %d: %w", runID, err)
		}
		return buildSymbolIndex(runID, byName), nil
	}
	if d.cache == nil {
		return load()
	}
	return d.cache.get(runID, load)
}

// ResolutionStats returns the run's reference-resolution outcomes, loading them
// once. Capabilities must read them through here rather than calling the
// registry directly — see symbolCache.getStats for the measured reason.
func (d Deps) ResolutionStats(ctx context.Context, runID int64) (registry.ResolutionStats, error) {
	if err := d.Validate(); err != nil {
		return registry.ResolutionStats{}, err
	}
	load := func() (registry.ResolutionStats, error) {
		stats, err := d.Registry.ResolutionStats(ctx, runID)
		if err != nil {
			return registry.ResolutionStats{}, fmt.Errorf("capability: resolution stats for run %d: %w", runID, err)
		}
		return stats, nil
	}
	if d.cache == nil {
		return load()
	}
	return d.cache.getStats(runID, load)
}

// ErrTargetNotFound means no symbol in the run matches the requested name or
// FQN. It is a normal outcome (a typo, or a symbol the index could not extract)
// and not a fault, so callers should test for it.
var ErrTargetNotFound = errors.New("capability: target symbol not found in index")

// AmbiguousTargetError reports that a bare name matched several definitions.
//
// Resolving it by picking the exported one, or the first, would be exactly the
// silent overclaim this package exists to prevent: an impact analysis of the
// wrong `Close` is not a partial answer, it is a wrong one. The candidates are
// returned so the caller can re-ask with an FQN.
type AmbiguousTargetError struct {
	Query      string
	Candidates []Target
}

func (e *AmbiguousTargetError) Error() string {
	names := make([]string, 0, len(e.Candidates))
	for i, c := range e.Candidates {
		if i == 5 {
			names = append(names, fmt.Sprintf("… and %d more", len(e.Candidates)-5))
			break
		}
		names = append(names, c.FQN)
	}
	return fmt.Sprintf("capability: %q matches %d symbols; re-ask with a fully-qualified name (%s)",
		e.Query, len(e.Candidates), strings.Join(names, ", "))
}

// Target is the resolved subject of a capability query.
type Target struct {
	// Query is the string the caller supplied, preserved so a report can say
	// what was asked as well as what was found.
	Query string `json:"query"`

	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	FQN      string `json:"fqn"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	Exported bool   `json:"exported"`

	// Confidence and Source describe how reliably the SYMBOL ITSELF was
	// extracted — a Tier-1 generic extraction is not the same fact as a
	// compiler-produced one, and a report built on a shaky definition should
	// say so before it says anything about callers.
	Confidence model.Confidence `json:"confidence"`
	Source     model.SourceType `json:"source"`
}

// maxTargetCandidates bounds the ambiguity report. A name matching more than
// this is a name the caller must qualify regardless of the exact count.
const maxTargetCandidates = 50

// ResolveTarget turns a symbol name or FQN into exactly one Target.
//
// The name lookup is tried first because it is a single indexed query, and it
// is the overwhelmingly common case. Only when it finds nothing does this fall
// back to the FQN map, which costs a full symbol-index load — worth it for a
// disambiguating re-ask, wasteful as the default path.
func ResolveTarget(ctx context.Context, deps Deps, runID int64, target string) (Target, error) {
	if err := deps.Validate(); err != nil {
		return Target{}, err
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return Target{}, fmt.Errorf("capability: %w: empty target", ErrTargetNotFound)
	}

	hits, err := deps.Registry.FindSymbolsByName(ctx, runID, target, "", maxTargetCandidates)
	if err != nil {
		return Target{}, fmt.Errorf("capability: resolve %q: %w", target, err)
	}
	switch {
	case len(hits) == 1:
		return targetFromHit(target, hits[0]), nil
	case len(hits) > 1:
		cands := make([]Target, len(hits))
		for i, h := range hits {
			cands[i] = targetFromHit(target, h)
		}
		return Target{}, &AmbiguousTargetError{Query: target, Candidates: cands}
	}

	// No name match: the caller may have supplied an FQN, which is the answer
	// to a previous AmbiguousTargetError.
	idx, err := deps.SymbolIndex(ctx, runID)
	if err != nil {
		return Target{}, err
	}
	sym, ok := idx.ByFQN(target)
	if !ok {
		return Target{}, fmt.Errorf("capability: %q: %w", target, ErrTargetNotFound)
	}

	// Re-read by name to recover the fields the (deliberately narrow) symbol
	// index does not carry — position and extraction provenance — and keep only
	// the row whose FQN is the one asked for.
	byName, err := deps.Registry.FindSymbolsByName(ctx, runID, sym.Name, "", maxTargetCandidates)
	if err != nil {
		return Target{}, fmt.Errorf("capability: resolve %q: %w", target, err)
	}
	for _, h := range byName {
		if h.FQN == target {
			return targetFromHit(target, h), nil
		}
	}
	// The index knows the symbol but the name query did not return it (more
	// than maxTargetCandidates share the name). Report what the index has
	// rather than failing: an id, name and path are enough to run the query.
	return Target{
		Query: target, SymbolID: sym.ID, Name: sym.Name, FQN: sym.FQN,
		Kind: sym.Kind, FilePath: sym.FilePath, Exported: sym.Exported,
	}, nil
}

func targetFromHit(query string, h registry.SymbolHit) Target {
	return Target{
		Query: query, SymbolID: h.ID, Name: h.Name, FQN: h.FQN, Kind: h.Kind,
		FilePath: h.FilePath, Line: h.StartLine,
		Exported:   h.Exported,
		Confidence: model.Confidence(h.Confidence),
		Source:     model.SourceType(h.Source),
	}
}
