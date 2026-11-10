package diffgraph

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// snapshot is one run reduced to content-keyed facts.
//
// Every id read from the store dies here: the maps below are keyed by content
// identity, and nothing downstream of load() can see a rowid. That is the
// property that makes a delta mean "the code changed" rather than "the
// allocator differed" — see the package doc.
type snapshot struct {
	info RunInfo

	symKeys []SymbolKey // sorted by compareSymbolKey
	syms    map[SymbolKey]*SymbolFact

	edgeKeys []EdgeKey // sorted by compareEdgeKey
	edges    map[EdgeKey]*EdgeFact

	epKeys []EndpointKey // sorted by compareEndpointKey
	eps    map[EndpointKey]*EndpointFact

	tableNames []string // sorted
	tables     map[string]*TableFact

	// badFiles holds paths whose parse status was not "ok", so a change in one
	// of them can be marked uncertain instead of asserted.
	badFiles map[string]string // path -> parse status
	badPaths []string          // sorted

	danglingEdges int
	orphanSymbols int // symbols whose file row was absent; excluded
}

// isReferencingEdge decides which inbound edges count as "somebody uses this".
//
// CONTAINS is structural — a file contains its functions whether or not anybody
// calls them — and OWNED_BY / CHANGED_BY are provenance from git. Counting any
// of them would mean no symbol could ever be reported as having lost its last
// caller, which is one of the facts this package exists to surface.
func isReferencingEdge(k model.EdgeKind) bool {
	switch k {
	case model.EdgeContains, model.EdgeOwnedBy, model.EdgeChangedBy:
		return false
	default:
		return true
	}
}

// loadSnapshot reads one run through the port and reduces it to facts.
//
// ctx is checked between stages as well as passed down: the reduction between
// two I/O calls is O(symbols+edges) and a cancelled `cortex diff` should stop
// there rather than at the next query.
func loadSnapshot(ctx context.Context, src IndexSource, runID int64, maxSites, maxBadPaths int) (*snapshot, error) {
	run, err := src.Run(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("diffgraph: load run %d: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("diffgraph: load run %d: registry returned no run", runID)
	}

	s := &snapshot{
		info: RunInfo{
			RunID:         run.ID,
			Status:        run.Status,
			GitRevision:   run.GitRevision,
			AccuracyMode:  run.AccuracyMode,
			Digest:        run.RegistryDigest,
			Files:         run.FilesIndexed,
			Symbols:       run.SymbolsFound,
			Edges:         run.EdgesFound,
			ParseFailures: run.ParseFailures,
		},
		syms:     map[SymbolKey]*SymbolFact{},
		edges:    map[EdgeKey]*EdgeFact{},
		eps:      map[EndpointKey]*EndpointFact{},
		tables:   map[string]*TableFact{},
		badFiles: map[string]string{},
	}

	files, err := src.Files(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("diffgraph: load files for run %d: %w", runID, err)
	}
	pathByID := make(map[int64]string, len(files))
	for _, f := range files {
		pathByID[f.ID] = f.Path
		if f.ParseStatus != "ok" && f.ParseStatus != "" {
			s.badFiles[f.Path] = f.ParseStatus
		}
	}
	for p := range s.badFiles {
		s.badPaths = append(s.badPaths, p)
	}
	slices.Sort(s.badPaths)
	if len(s.badPaths) > maxBadPaths {
		s.badPaths = s.badPaths[:maxBadPaths]
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("diffgraph: load run %d: %w", runID, err)
	}

	symbols, err := src.Symbols(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("diffgraph: load symbols for run %d: %w", runID, err)
	}
	keyByID, err := s.indexSymbols(symbols, pathByID)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("diffgraph: load run %d: %w", runID, err)
	}

	edges, err := src.Edges(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("diffgraph: load edges for run %d: %w", runID, err)
	}
	s.indexEdges(edges, keyByID, maxSites)

	endpoints, err := src.EndpointReport(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("diffgraph: load endpoints for run %d: %w", runID, err)
	}
	s.indexEndpoints(endpoints)

	tables, err := src.TableUsageReport(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("diffgraph: load table usage for run %d: %w", runID, err)
	}
	s.indexTables(tables)

	return s, nil
}

// indexSymbols builds the content-keyed symbol set and returns the id->key map
// the edge pass needs.
//
// The sort is what makes ordinal assignment reproducible: symbols arrive in
// whatever total order the store chose, are re-sorted here on content, and
// duplicates within one (path, fqn, kind) group are then numbered in position
// order so a uniform shift preserves the numbering.
func (s *snapshot) indexSymbols(symbols []model.Symbol, pathByID map[int64]string) (map[int64]SymbolKey, error) {
	type entry struct {
		id   int64
		path string
		sym  model.Symbol
	}
	entries := make([]entry, 0, len(symbols))
	for _, sym := range symbols {
		path, ok := pathByID[sym.FileID]
		if !ok {
			// A symbol whose file row is absent has no content identity at
			// all. Excluded and counted rather than keyed on the file id,
			// which is exactly the allocation artefact this package refuses
			// to compare on.
			s.orphanSymbols++
			continue
		}
		entries = append(entries, entry{id: sym.ID, path: path, sym: sym})
	}

	slices.SortFunc(entries, func(a, b entry) int {
		if c := cmp.Compare(a.path, b.path); c != 0 {
			return c
		}
		if c := cmp.Compare(a.sym.FQN, b.sym.FQN); c != 0 {
			return c
		}
		if c := cmp.Compare(string(a.sym.Kind), string(b.sym.Kind)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.sym.Range.Start.Line, b.sym.Range.Start.Line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.sym.Range.Start.Col, b.sym.Range.Start.Col); c != 0 {
			return c
		}
		return cmp.Compare(a.sym.Name, b.sym.Name)
	})

	keyByID := make(map[int64]SymbolKey, len(entries))
	var prev SymbolKey
	ordinal := 0
	for i, e := range entries {
		key := SymbolKey{Path: e.path, FQN: e.sym.FQN, Kind: e.sym.Kind}
		if i > 0 && key.Path == prev.Path && key.FQN == prev.FQN && key.Kind == prev.Kind {
			ordinal++
		} else {
			ordinal = 0
		}
		prev = key
		key.Ordinal = ordinal

		if _, dup := s.syms[key]; dup {
			return nil, fmt.Errorf("diffgraph: run %d: duplicate symbol key %s after ordinal assignment", s.info.RunID, key)
		}
		s.syms[key] = &SymbolFact{
			Path:       e.path,
			FQN:        e.sym.FQN,
			Kind:       e.sym.Kind,
			Name:       e.sym.Name,
			Start:      e.sym.Range.Start,
			End:        e.sym.Range.End,
			Signature:  e.sym.Signature,
			Exported:   e.sym.Exported,
			Confidence: e.sym.Confidence,
			Source:     e.sym.Source,
		}
		s.symKeys = append(s.symKeys, key)
		keyByID[e.id] = key
	}
	// entries is already in key order, so symKeys is sorted by construction;
	// the explicit sort documents the invariant and costs nothing on sorted
	// input.
	slices.SortFunc(s.symKeys, compareSymbolKey)
	return keyByID, nil
}

// indexEdges collapses edges into (src, dst, kind) facts.
func (s *snapshot) indexEdges(edges []model.Edge, keyByID map[int64]SymbolKey, maxSites int) {
	type entry struct {
		key  EdgeKey
		edge model.Edge
	}
	entries := make([]entry, 0, len(edges))
	for _, e := range edges {
		srcKey, okSrc := keyByID[e.SrcID]
		dstKey, okDst := keyByID[e.DstID]
		if !okSrc || !okDst {
			// An edge pointing at a symbol this run does not contain cannot be
			// given a content identity. Counted into Caveats.DanglingEdges
			// rather than dropped silently, because a dangling edge on one
			// side would otherwise read as a deletion on the other.
			s.danglingEdges++
			continue
		}
		entries = append(entries, entry{key: EdgeKey{Src: srcKey, Dst: dstKey, Kind: e.Kind}, edge: e})
	}

	slices.SortFunc(entries, func(a, b entry) int {
		if c := compareEdgeKey(a.key, b.key); c != 0 {
			return c
		}
		if c := cmp.Compare(a.edge.Evidence.FilePath, b.edge.Evidence.FilePath); c != 0 {
			return c
		}
		if c := cmp.Compare(a.edge.Evidence.Range.Start.Line, b.edge.Evidence.Range.Start.Line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.edge.Evidence.Range.Start.Col, b.edge.Evidence.Range.Start.Col); c != 0 {
			return c
		}
		if c := cmp.Compare(string(a.edge.Confidence), string(b.edge.Confidence)); c != 0 {
			return c
		}
		return cmp.Compare(a.edge.Evidence.Detail, b.edge.Evidence.Detail)
	})

	for _, e := range entries {
		fact, ok := s.edges[e.key]
		if !ok {
			fact = &EdgeFact{}
			s.edges[e.key] = fact
			s.edgeKeys = append(s.edgeKeys, e.key)
		}
		fact.SiteCount++
		if len(fact.Sites) < maxSites {
			fact.Sites = append(fact.Sites, Site{
				Path:   e.edge.Evidence.FilePath,
				Line:   e.edge.Evidence.Range.Start.Line,
				Col:    e.edge.Evidence.Range.Start.Col,
				Detail: e.edge.Evidence.Detail,
			})
		}
		// Strongest proof wins. Entries are in a total content order, so the
		// tie between two equally strong sources is broken deterministically
		// by that order rather than by arrival.
		if e.edge.Confidence.Rank() > fact.Confidence.Rank() {
			fact.Confidence = e.edge.Confidence
			fact.Source = e.edge.Evidence.Source
		}
	}

	slices.SortFunc(s.edgeKeys, compareEdgeKey)

	for _, k := range s.edgeKeys {
		if !isReferencingEdge(k.Kind) {
			continue
		}
		if f, ok := s.syms[k.Dst]; ok {
			f.Inbound++
		}
	}
}

// indexEndpoints merges the report's rows into one fact per (path, route).
//
// EndpointReport LEFT JOINs the EXPOSES edge, so a route resolving to two
// handlers arrives as two rows. Merging rather than keying on the row keeps
// "the handler set changed" as a single change instead of a spurious
// add-plus-remove pair.
func (s *snapshot) indexEndpoints(endpoints []endpointRow) {
	for _, e := range endpoints {
		key := EndpointKey{Path: e.FilePath, Route: e.Signature}
		fact, ok := s.eps[key]
		if !ok {
			fact = &EndpointFact{Line: e.Line, Detail: e.Detail}
			s.eps[key] = fact
			s.epKeys = append(s.epKeys, key)
		}
		if e.Line < fact.Line {
			fact.Line = e.Line
		}
		if e.Handler != "" {
			h := e.HandlerPath + ":" + e.Handler
			if !slices.Contains(fact.Handlers, h) {
				fact.Handlers = append(fact.Handlers, h)
			}
		}
	}
	for _, k := range s.epKeys {
		slices.Sort(s.eps[k].Handlers)
	}
	slices.SortFunc(s.epKeys, compareEndpointKey)
}

// indexTables aggregates the usage report by table name.
//
// TableUsageReport groups by (table_name, bound_symbol_id), so one table can
// arrive as two rows — a bound one and an unbound one. Summing them is what
// makes "this table gained a writer" answerable for tables this repository
// does not declare, which is the normal shape of a microservice estate.
func (s *snapshot) indexTables(tables []tableRow) {
	for _, t := range tables {
		fact, ok := s.tables[t.Table]
		if !ok {
			fact = &TableFact{}
			s.tables[t.Table] = fact
			s.tableNames = append(s.tableNames, t.Table)
		}
		fact.Declared = fact.Declared || t.Declared
		fact.Readers += t.Readers
		fact.Writers += t.Writers
		fact.Accesses += t.Accesses
		fact.Uncertain += t.Uncertain
	}
	slices.Sort(s.tableNames)
}

// unreliable reports whether any of the given paths failed to parse cleanly.
func (s *snapshot) unreliable(paths ...string) (string, bool) {
	for _, p := range paths {
		if st, ok := s.badFiles[p]; ok {
			return p + " parse status " + st, true
		}
	}
	return "", false
}
