package docs

import (
	"context"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Generator derives documents from an index.
//
// Constructed with functional options per CLAUDE.md §2, and holds no mutable
// state: Derive takes everything that varies per call as an argument, so one
// Generator is safe to share and two concurrent Derives cannot see each other.
type Generator struct {
	reg        Registry
	limits     Limits
	configPath string
}

// Option configures a Generator.
type Option func(*Generator) error

// WithLimits overrides the output caps. Zero fields keep their defaults; see
// Limits.
func WithLimits(l Limits) Option {
	return func(g *Generator) error {
		if err := l.Validate(); err != nil {
			return err
		}
		g.limits = l.withDefaults()
		return nil
	}
}

// WithConfigPath names the file the component declarations were read from.
//
// It exists so the component section can cite something. A component's evidence
// is not a file in the repository — it is the line somebody wrote in
// .repocortex.yml — and a component node with no citation would be the one node in
// the document a reader could not check.
func WithConfigPath(path string) Option {
	return func(g *Generator) error {
		if path == "" {
			return fmt.Errorf("docs: config path is empty; declared components would have nothing to cite")
		}
		g.configPath = path
		return nil
	}
}

// DefaultConfigPath is where RepoCortex reads a repository's declarations from.
const DefaultConfigPath = ".repocortex.yml"

// New returns a Generator reading through reg.
func New(reg Registry, opts ...Option) (*Generator, error) {
	if reg == nil {
		return nil, fmt.Errorf("docs: registry port is nil")
	}
	g := &Generator{reg: reg, limits: DefaultLimits(), configPath: DefaultConfigPath}
	for i, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("docs: option %d is nil", i)
		}
		if err := opt(g); err != nil {
			return nil, fmt.Errorf("docs: apply option %d: %w", i, err)
		}
	}
	return g, nil
}

// symEntry is one symbol plus the two things every citation needs about it.
type symEntry struct {
	sym  model.Symbol
	path string
	comp string
}

// order is the sort key for anything a document lists symbol-first: where it is,
// then what it is called. Never the database id — ids are stable within an index
// but meaningless to a reader, and sorting a document by them makes the diff
// move when an unrelated file is added.
func (s symEntry) less(o symEntry) bool {
	if s.path != o.path {
		return s.path < o.path
	}
	if s.sym.Range.Start.Line != o.sym.Range.Start.Line {
		return s.sym.Range.Start.Line < o.sym.Range.Start.Line
	}
	if s.sym.FQN != o.sym.FQN {
		return s.sym.FQN < o.sym.FQN
	}
	return s.sym.ID < o.sym.ID
}

func (s symEntry) ref() SymbolRef {
	return SymbolRef{
		Name:      s.sym.Name,
		FQN:       s.sym.FQN,
		Kind:      s.sym.Kind,
		Component: s.comp,
		Evidence: Evidence{
			FilePath:   s.path,
			Line:       s.sym.Range.Start.Line,
			Source:     s.sym.Source,
			Confidence: s.sym.Confidence,
		},
	}
}

// maxFlowExplore bounds one endpoint's CALLS walk.
//
// Depth alone does not bound it: three hops out of a handler in a repository
// where everything calls a logger reaches most of the program. The cap is per
// endpoint and well above any flow a person would read, and hitting it is
// reported in the flow's omissions rather than silently truncating the walk.
const maxFlowExplore = 512

// Derive reads the index once and builds every view both documents need.
//
// m is the DECLARED component model, exactly as internal/capability/architecture
// takes it. An empty model is not an error here — unlike Check, which refuses to
// grade a repository that declared nothing — because the endpoint flows and the
// inheritance view are derived from edges and are worth rendering either way.
// What the documents must not do is invent components, and they do not: see
// View.ComponentsDeclared.
func (g *Generator) Derive(ctx context.Context, runID int64, m arch.Model) (*View, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("docs: derive run %d: %w", runID, err)
	}

	files, err := g.reg.Files(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("docs: read files for run %d: %w", runID, err)
	}
	symbols, err := g.reg.Symbols(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("docs: read symbols for run %d: %w", runID, err)
	}
	edges, err := g.reg.Edges(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("docs: read edges for run %d: %w", runID, err)
	}

	v := &View{
		RunID:              runID,
		ConfigPath:         g.configPath,
		Limits:             g.limits,
		ComponentsDeclared: len(m.Components) > 0,
		RulesDeclared:      len(m.Rules),
		Counts: Counts{
			Files:   len(files),
			Symbols: len(symbols),
			Edges:   len(edges),
		},
	}

	// Re-sort on entry rather than trusting the caller's order. The production
	// registry already returns these ordered, but the promise this package makes
	// is "identical index, byte-identical output", and a promise that depends on
	// an adapter's ORDER BY clause staying put is not a promise.
	edges = sortedEdges(edges)

	idx := indexSymbols(files, symbols, m)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("docs: derive run %d: %w", runID, err)
	}

	v.Components, v.Omissions = deriveComponents(m, idx, g.configPath, g.limits, v.Omissions)
	kept := keptComponents(v.Components)
	v.Crossings, v.Omissions = deriveCrossings(&m, idx, edges, kept, g.limits, v.Omissions)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("docs: derive run %d: %w", runID, err)
	}

	adj := buildAdjacency(idx, edges, &v.Counts)
	v.Endpoints, v.Omissions = deriveFlows(idx, adj, g.limits, v.Omissions)
	v.Inheritance, v.Omissions = deriveInheritance(idx, edges, g.limits, v.Omissions, &v.Counts)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("docs: derive run %d: %w", runID, err)
	}

	att := deriveAttribution(idx, adj)
	v.Coverage = Coverage{
		Endpoints:        len(idx.endpoints),
		EndpointsClaimed: len(idx.endpoints) - len(att.unclaimedEndpoints),
		Tables:           len(idx.tables),
		TablesClaimed:    att.claimedTables,
	}
	v.Capabilities, v.Omissions = deriveCapabilities(v.Components, v.Crossings, idx, adj, att, g.limits, v.Omissions)
	v.SharedTables, v.Omissions = deriveSharedTables(v.Capabilities, g.limits, v.Omissions)

	var om Omission
	v.UnclaimedEndpoints, om = truncate(att.unclaimedEndpoints, g.limits.MaxUnclaimed,
		"endpoints no declared component accounts for", "source order: file path, then line")
	v.Omissions = appendOmission(v.Omissions, om)
	v.UnclaimedTables, om = truncate(att.unclaimedTables, g.limits.MaxUnclaimed,
		"tables no declared component accounts for", "source order: file path, then line")
	v.Omissions = appendOmission(v.Omissions, om)

	return v, nil
}

// ---------------------------------------------------------------- indexing

type symbolIndex struct {
	byID map[int64]symEntry
	// endpoints and tables are pre-filtered and pre-ordered, because both are
	// walked repeatedly and both must be walked in a fixed order.
	endpoints []symEntry
	tables    []symEntry
	// fileCount and symCount are per declared component.
	fileCount map[string]int
	symCount  map[string]int
}

// indexSymbols resolves every file's component once and buckets the symbols the
// documents ask for by name.
//
// Once per FILE, not once per symbol: ComponentOf walks every declared glob, and
// a 100,000-symbol repository would otherwise repeat the same match for every
// symbol in a file. The Resolver memoises across the call for the same reason,
// and is local to it so nothing outlives the derivation.
func indexSymbols(files []model.File, symbols []model.Symbol, m arch.Model) *symbolIndex {
	resolver := arch.NewResolver(m)

	pathOf := make(map[int64]string, len(files))
	compOfFile := make(map[int64]string, len(files))
	idx := &symbolIndex{
		byID:      make(map[int64]symEntry, len(symbols)),
		fileCount: make(map[string]int),
		symCount:  make(map[string]int),
	}
	for _, f := range files {
		pathOf[f.ID] = f.Path
		c := resolver.ComponentOf(f.Path)
		compOfFile[f.ID] = c
		if c != "" {
			idx.fileCount[c]++
		}
	}
	for _, s := range symbols {
		e := symEntry{sym: s, path: pathOf[s.FileID], comp: compOfFile[s.FileID]}
		idx.byID[s.ID] = e
		if e.comp != "" {
			idx.symCount[e.comp]++
		}
		switch s.Kind {
		case model.KindEndpoint:
			idx.endpoints = append(idx.endpoints, e)
		case model.KindTable:
			idx.tables = append(idx.tables, e)
		}
	}
	sortEntries(idx.endpoints)
	sortEntries(idx.tables)
	return idx
}

func sortEntries(es []symEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].less(es[j]) })
}

// sortedEdges returns a copy ordered by evidence first.
//
// Evidence first, not src id first, because every ordering this package derives
// from edges ends up in a document, and "the first edge in this file" is a
// stable notion across re-indexes in a way "the lowest row id" is not.
func sortedEdges(edges []model.Edge) []model.Edge {
	out := make([]model.Edge, len(edges))
	copy(out, edges)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Evidence.FilePath != b.Evidence.FilePath {
			return a.Evidence.FilePath < b.Evidence.FilePath
		}
		if a.Evidence.Range.Start.Line != b.Evidence.Range.Start.Line {
			return a.Evidence.Range.Start.Line < b.Evidence.Range.Start.Line
		}
		if a.SrcID != b.SrcID {
			return a.SrcID < b.SrcID
		}
		if a.DstID != b.DstID {
			return a.DstID < b.DstID
		}
		return a.ID < b.ID
	})
	return out
}

// ---------------------------------------------------------------- components

func deriveComponents(
	m arch.Model, idx *symbolIndex,
	configPath string, lim Limits, oms []Omission,
) ([]Component, []Omission) {
	if len(m.Components) == 0 {
		return nil, oms
	}
	comps := make([]Component, 0, len(m.Components))
	for _, c := range m.Components {
		comps = append(comps, Component{
			Name:    c.Name,
			Include: append([]string(nil), c.Include...),
			Exclude: append([]string(nil), c.Exclude...),
			Files:   idx.fileCount[c.Name],
			Symbols: idx.symCount[c.Name],
			Evidence: Evidence{
				FilePath:   configPath,
				Source:     model.SourceConfig,
				Confidence: model.ConfidenceExact,
				Detail:     "declared component: include " + joinPatterns(c.Include),
			},
		})
	}
	// Biggest first so a cap keeps the parts of the system a reader is most
	// likely to be looking for; name breaks the tie so the order never depends
	// on the declaration order in a file two people edit.
	sort.Slice(comps, func(i, j int) bool {
		a, b := comps[i], comps[j]
		if a.Symbols != b.Symbols {
			return a.Symbols > b.Symbols
		}
		if a.Files != b.Files {
			return a.Files > b.Files
		}
		return a.Name < b.Name
	})
	comps, om := truncate(comps, lim.MaxComponents, "declared components",
		"most symbols first, then most files, then name; dependencies touching an omitted component are omitted with it")
	return comps, appendOmission(oms, om)
}

func keptComponents(comps []Component) map[string]bool {
	kept := make(map[string]bool, len(comps))
	for _, c := range comps {
		kept[c.Name] = true
	}
	return kept
}

func deriveCrossings(
	m *arch.Model, idx *symbolIndex, edges []model.Edge,
	kept map[string]bool, lim Limits, oms []Omission,
) ([]Crossing, []Omission) {
	if len(kept) == 0 {
		return nil, oms
	}
	type agg struct {
		c     Crossing
		kinds map[string]bool
	}
	// keys preserves first-seen order so the aggregation itself never depends
	// on map iteration; the slice is sorted explicitly below regardless.
	var keys []string
	byKey := map[string]*agg{}

	for i := range edges {
		e := edges[i]
		src, ok := idx.byID[e.SrcID]
		if !ok {
			continue
		}
		dst, ok := idx.byID[e.DstID]
		if !ok {
			continue
		}
		if src.comp == "" || dst.comp == "" || src.comp == dst.comp {
			continue
		}
		if !kept[src.comp] || !kept[dst.comp] {
			continue
		}
		key := src.comp + "\x00" + dst.comp
		a, ok := byKey[key]
		if !ok {
			verdict, rule := m.Evaluate(src.comp, dst.comp)
			ruleID := ""
			if rule != nil {
				ruleID = rule.ID
			}
			a = &agg{
				c: Crossing{
					From: src.comp, To: dst.comp,
					Weakest:  e.Confidence,
					Verdict:  string(verdict),
					RuleID:   ruleID,
					Evidence: evidenceOf(e.Evidence, e.Confidence),
				},
				kinds: map[string]bool{},
			}
			byKey[key] = a
			keys = append(keys, key)
		}
		a.c.Count++
		a.kinds[string(e.Kind)] = true
		if e.Confidence.Rank() < a.c.Weakest.Rank() {
			a.c.Weakest = e.Confidence
		}
		if ev := evidenceOf(e.Evidence, e.Confidence); ev.less(a.c.Evidence) {
			a.c.Evidence = ev
		}
	}

	out := make([]Crossing, 0, len(keys))
	for _, k := range keys {
		a := byKey[k]
		a.c.Kinds = sortedKeys(a.kinds)
		out = append(out, a.c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// Violations first: a diagram that renders a forbidden dependency the
		// same as a permitted one has hidden the only part anyone needed.
		av, bv := a.Verdict != string(arch.VerdictAllowed), b.Verdict != string(arch.VerdictAllowed)
		if av != bv {
			return av
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	out, om := truncate(out, lim.MaxCrossings, "component dependencies",
		"rule violations first, then most edges, then component name")
	return out, appendOmission(oms, om)
}

// ---------------------------------------------------------------- adjacency

type callRef struct {
	dst int64
	ev  Evidence
}

type accessRef struct {
	table int64
	kind  model.EdgeKind // READS or WRITES
	ev    Evidence
}

type exposeRef struct {
	handler int64
	ev      Evidence
}

type adjacency struct {
	calls    map[int64][]callRef
	accesses map[int64][]accessRef
	exposes  map[int64][]exposeRef
}

func buildAdjacency(idx *symbolIndex, edges []model.Edge, counts *Counts) *adjacency {
	a := &adjacency{
		calls:    map[int64][]callRef{},
		accesses: map[int64][]accessRef{},
		exposes:  map[int64][]exposeRef{},
	}
	counts.Endpoints = len(idx.endpoints)
	counts.Tables = len(idx.tables)

	for i := range edges {
		e := edges[i]
		if _, ok := idx.byID[e.SrcID]; !ok {
			continue
		}
		dst, ok := idx.byID[e.DstID]
		if !ok {
			continue
		}
		ev := evidenceOf(e.Evidence, e.Confidence)
		switch e.Kind {
		case model.EdgeExposes:
			a.exposes[e.SrcID] = append(a.exposes[e.SrcID], exposeRef{handler: e.DstID, ev: ev})
			counts.ExposesEdges++
		case model.EdgeCalls:
			a.calls[e.SrcID] = append(a.calls[e.SrcID], callRef{dst: e.DstID, ev: ev})
		case model.EdgeReads, model.EdgeWrites:
			// Only an edge whose target is a declared table is a data access.
			// READS/WRITES to anything else is not a table the document can
			// name, and drawing a cylinder for it would label a symbol as a
			// table on no evidence.
			if dst.sym.Kind != model.KindTable {
				continue
			}
			a.accesses[e.SrcID] = append(a.accesses[e.SrcID], accessRef{table: e.DstID, kind: e.Kind, ev: ev})
			counts.TableAccessEdg++
		}
	}

	// Sort every adjacency list by where its TARGET is declared. The walk below
	// is breadth-first and its output order is the document's order, so an
	// unordered adjacency list would be a map iteration hiding one level down.
	for id := range a.calls {
		list := a.calls[id]
		sort.Slice(list, func(i, j int) bool {
			return lessTarget(idx, list[i].dst, list[j].dst, list[i].ev, list[j].ev)
		})
	}
	for id := range a.accesses {
		list := a.accesses[id]
		sort.Slice(list, func(i, j int) bool {
			if list[i].table != list[j].table {
				return lessTarget(idx, list[i].table, list[j].table, list[i].ev, list[j].ev)
			}
			if list[i].kind != list[j].kind {
				return list[i].kind < list[j].kind
			}
			return list[i].ev.less(list[j].ev)
		})
	}
	for id := range a.exposes {
		list := a.exposes[id]
		sort.Slice(list, func(i, j int) bool {
			return lessTarget(idx, list[i].handler, list[j].handler, list[i].ev, list[j].ev)
		})
	}
	return a
}

func lessTarget(idx *symbolIndex, a, b int64, aev, bev Evidence) bool {
	ea, eb := idx.byID[a], idx.byID[b]
	if ea.less(eb) {
		return true
	}
	if eb.less(ea) {
		return false
	}
	return aev.less(bev)
}

// ---------------------------------------------------------------- flows

func deriveFlows(idx *symbolIndex, adj *adjacency, lim Limits, oms []Omission) ([]EndpointFlow, []Omission) {
	if len(idx.endpoints) == 0 {
		return nil, oms
	}
	// Endpoints are already ordered by file and line — the order they appear in
	// the source, which is the order the routes were written in and the one a
	// reader can follow with the file open.
	shown, om := truncate(idx.endpoints, lim.MaxEndpointFlows, "endpoint flows",
		"source order: file path, then line")
	oms = appendOmission(oms, om)

	out := make([]EndpointFlow, 0, len(shown))
	for _, ep := range shown {
		out = append(out, deriveFlow(idx, adj, ep, lim))
	}
	return out, oms
}

func deriveFlow(idx *symbolIndex, adj *adjacency, ep symEntry, lim Limits) EndpointFlow {
	flow := EndpointFlow{Endpoint: ep.ref()}

	exposed := adj.exposes[ep.sym.ID]
	if len(exposed) == 0 {
		// No EXPOSES edge. The endpoint is still real and still printed; what is
		// unknown is what serves it. See EndpointFlow's doc.
		return flow
	}
	h := idx.byID[exposed[0].handler]
	ref := h.ref()
	flow.Handler = &ref
	flow.Exposes = exposed[0].ev
	if len(exposed) > 1 {
		flow.Omissions = append(flow.Omissions, Omission{
			Subject: "handlers for " + ep.ref().display(),
			Shown:   1, Total: len(exposed),
			Order: "first by handler declaration site; the flow diagram is drawn for one handler",
		})
	}

	hops, capped := walkCalls(idx, adj, h.sym.ID, lim)
	order := "breadth-first from the handler, then by callee declaration site"
	if capped {
		order += fmt.Sprintf("; walk stopped at the %d-node exploration cap", maxFlowExplore)
	}
	hops, om := truncate(hops, lim.MaxCallsPerFlow, "calls from "+ref.display(), order)
	if om.Subject != "" {
		flow.Omissions = append(flow.Omissions, om)
	}
	flow.Calls = make([]CallHop, 0, len(hops))

	// Tables reached by the handler itself and by every call kept above. Only
	// the kept hops, so one endpoint's table list is bounded by the call cap
	// as well as by its own.
	touchers := make([]int64, 0, len(hops)+1)
	touchers = append(touchers, h.sym.ID)
	for _, hop := range hops {
		flow.Calls = append(flow.Calls, hop.hop)
		touchers = append(touchers, hop.id)
	}
	tables := collectTables(idx, adj, touchers)
	tables, om = truncate(tables, lim.MaxTablesPerFlow, "tables reached from "+ep.ref().display(),
		"table name")
	if om.Subject != "" {
		flow.Omissions = append(flow.Omissions, om)
	}
	flow.Tables = tables
	return flow
}

// walkHop is a hop plus the symbol id it came from.
//
// The id is kept out of CallHop on purpose: a database row id means nothing to a
// reader and must never reach a document, and a document sorted by one moves
// whenever an unrelated file is indexed. The walk needs it to look up the next
// level of adjacency, so it travels beside the hop rather than inside it.
type walkHop struct {
	hop CallHop
	id  int64
}

// walkCalls is the bounded breadth-first CALLS walk out of a handler.
//
// Returns the hops in visit order and whether the exploration cap was hit. Visit
// order is the document's order, which is why every adjacency list it reads was
// sorted in buildAdjacency: a breadth-first walk over unordered neighbours is a
// map iteration with extra steps.
func walkCalls(idx *symbolIndex, adj *adjacency, start int64, lim Limits) (hops []walkHop, capped bool) {
	if lim.MaxCallDepth <= 0 {
		return nil, false
	}
	type queued struct {
		id    int64
		depth int
	}
	seen := map[int64]bool{start: true}
	queue := []queued{{id: start, depth: 0}}
	explored := 0

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= lim.MaxCallDepth {
			continue
		}
		explored++
		if explored > maxFlowExplore {
			return hops, true
		}
		from := idx.byID[cur.id]
		for _, c := range adj.calls[cur.id] {
			if seen[c.dst] {
				continue
			}
			seen[c.dst] = true
			callee, ok := idx.byID[c.dst]
			if !ok {
				continue
			}
			hops = append(hops, walkHop{
				hop: CallHop{
					Symbol:   callee.ref(),
					FromFQN:  from.sym.FQN,
					Depth:    cur.depth + 1,
					Evidence: c.ev,
				},
				id: c.dst,
			})
			queue = append(queue, queued{id: c.dst, depth: cur.depth + 1})
		}
	}
	return hops, false
}

// collectTables merges every table access made by the given symbols.
//
// READ and WRITE to the same table collapse to one row reading "reads/writes",
// because two arrows between the same pair of boxes is a rendering artefact, not
// a fact — while "reads" and "writes" alone are genuinely different facts and
// stay apart.
func collectTables(idx *symbolIndex, adj *adjacency, srcIDs []int64) []TableRef {
	type acc struct {
		ref    TableRef
		reads  bool
		writes bool
	}
	var keys []string
	byKey := map[string]*acc{}

	for _, id := range srcIDs {
		if id == 0 {
			continue
		}
		src := idx.byID[id]
		for _, a := range adj.accesses[id] {
			t, ok := idx.byID[a.table]
			if !ok {
				continue
			}
			key := t.sym.Name
			e, ok := byKey[key]
			if !ok {
				e = &acc{ref: TableRef{
					Table:    t.sym.Name,
					ViaName:  src.sym.Name,
					ViaFQN:   src.sym.FQN,
					Evidence: a.ev,
				}}
				byKey[key] = e
				keys = append(keys, key)
			}
			if a.ev.less(e.ref.Evidence) {
				e.ref.Evidence = a.ev
				e.ref.ViaName = src.sym.Name
				e.ref.ViaFQN = src.sym.FQN
			}
			switch a.kind {
			case model.EdgeReads:
				e.reads = true
			case model.EdgeWrites:
				e.writes = true
			}
		}
	}

	out := make([]TableRef, 0, len(keys))
	for _, k := range keys {
		e := byKey[k]
		e.ref.Access = accessLabel(e.reads, e.writes)
		out = append(out, e.ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Access < out[j].Access
	})
	return out
}

func accessLabel(reads, writes bool) string {
	switch {
	case reads && writes:
		return "reads/writes"
	case writes:
		return "writes"
	case reads:
		return "reads"
	}
	return "touches"
}

// ---------------------------------------------------------------- inheritance

func deriveInheritance(
	idx *symbolIndex, edges []model.Edge, lim Limits, oms []Omission, counts *Counts,
) ([]Inheritance, []Omission) {
	var out []Inheritance
	for i := range edges {
		e := edges[i]
		if e.Kind != model.EdgeImplements && e.Kind != model.EdgeExtends {
			continue
		}
		child, ok := idx.byID[e.SrcID]
		if !ok {
			continue
		}
		parent, ok := idx.byID[e.DstID]
		if !ok {
			continue
		}
		counts.InheritEdges++
		out = append(out, Inheritance{
			Kind:     e.Kind,
			Child:    child.ref(),
			Parent:   parent.ref(),
			Evidence: evidenceOf(e.Evidence, e.Confidence),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Parent.FQN != b.Parent.FQN {
			return a.Parent.FQN < b.Parent.FQN
		}
		if a.Child.FQN != b.Child.FQN {
			return a.Child.FQN < b.Child.FQN
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Evidence.less(b.Evidence)
	})
	out, om := truncate(out, lim.MaxInheritance, "inheritance relationships",
		"parent name, then child name")
	return out, appendOmission(oms, om)
}

// ---------------------------------------------------------------- capabilities

// attribution answers, for every endpoint and every table in the index, which
// declared component accounts for it — and, where none does, why.
//
// It is computed once from the DECLARATIONS rather than from the capped
// capability slice. An endpoint whose capability was dropped by MaxCapabilities
// is claimed but omitted, which is a different fact from unclaimed and belongs
// in the omission list, not in a section headed "no capability accounts for
// this". Conflating the two would let a cap look like a coverage gap.
type attribution struct {
	// endpointsOf and accessSrcOf are the claimed side, keyed by component and
	// each in a fixed order.
	endpointsOf map[string][]symEntry
	accessSrcOf map[string][]int64

	// The unclaimed side, in source order, each with the sentence saying why.
	unclaimedEndpoints []Unclaimed
	unclaimedTables    []Unclaimed

	// claimedTables counts the tables at least one declared component reaches.
	// Counted rather than derived from the capability rows because a table two
	// capabilities touch must count once.
	claimedTables int
}

const (
	unclaimedEndpointNoHandler = "declared in a file no component's patterns match, and this index " +
		"records no EXPOSES edge naming a handler for it"
	unclaimedEndpointNoComponent = "declared in a file no component's patterns match, and served by a " +
		"handler in a file no component's patterns match"
	unclaimedTableUntouched = "no symbol in this index reads or writes it — it may be dead, or the code " +
		"that uses it may be in a language or framework this indexer does not bind to SQL"
	unclaimedTableOutside = "read or written, but only by symbols in files no component's patterns match"
)

// deriveAttribution resolves every endpoint and table to a declared component or
// to nothing, and records which.
//
// Endpoints are attributed to the component owning the ENDPOINT's file, falling
// back to the component owning its handler. The fallback matters: a route table
// declared in a routing file that no component claims is still that handler's
// endpoint, and dropping it would make a capability look like it exposes nothing.
//
// Tables are attributed by the component of the ACCESSING symbol, never by the
// file the table is declared in. A migration directory is not a capability, and
// saying a table "belongs" to whichever component's globs happen to cover
// migrations/ would be the one inference this document refuses.
func deriveAttribution(idx *symbolIndex, adj *adjacency) *attribution {
	a := &attribution{
		endpointsOf: map[string][]symEntry{},
		accessSrcOf: map[string][]int64{},
	}

	for _, ep := range idx.endpoints { // already in source order
		comp, detail := ep.comp, ""
		if comp == "" {
			if ex := adj.exposes[ep.sym.ID]; len(ex) > 0 {
				comp = idx.byID[ex[0].handler].comp
				detail = unclaimedEndpointNoComponent
			} else {
				detail = unclaimedEndpointNoHandler
			}
		}
		if comp == "" {
			a.unclaimedEndpoints = append(a.unclaimedEndpoints, Unclaimed{Symbol: ep.ref(), Detail: detail})
			continue
		}
		a.endpointsOf[comp] = append(a.endpointsOf[comp], ep)
	}

	// Walked in symbol order rather than by ranging adj.accesses, so both the
	// grouping and the two boolean sets below are built the same way whatever
	// order the registry returned rows in.
	touched := map[int64]bool{}    // table id -> something accesses it
	byDeclared := map[int64]bool{} // table id -> a declared component accesses it
	for _, id := range sortedAccessSources(idx, adj) {
		comp := idx.byID[id].comp
		for _, ac := range adj.accesses[id] {
			touched[ac.table] = true
			if comp != "" {
				byDeclared[ac.table] = true
			}
		}
		if comp != "" {
			a.accessSrcOf[comp] = append(a.accessSrcOf[comp], id)
		}
	}
	for _, t := range idx.tables { // already in source order
		if byDeclared[t.sym.ID] {
			a.claimedTables++
			continue
		}
		detail := unclaimedTableUntouched
		if touched[t.sym.ID] {
			detail = unclaimedTableOutside
		}
		a.unclaimedTables = append(a.unclaimedTables, Unclaimed{Symbol: t.ref(), Detail: detail})
	}
	return a
}

// deriveCapabilities builds the A26 view.
//
// One capability per DECLARED component, named by the component. Its endpoints
// are the endpoints the index places in that component; its tables are the
// tables symbols in that component read or write; its dependencies are the
// component crossings already derived for the architecture document. Nothing
// here reads an identifier and decides what business the code is in — that is
// the inference this design refuses, and the reason the section is titled
// "candidate".
func deriveCapabilities(
	comps []Component, crossings []Crossing,
	idx *symbolIndex, adj *adjacency, att *attribution, lim Limits, oms []Omission,
) ([]Capability, []Omission) {
	if len(comps) == 0 {
		return nil, oms
	}
	// Dependencies bucketed by source component, preserving the crossing order
	// deriveCrossings already fixed — violations first, then edge count, then
	// name — so no ordering decision is made twice and none is made by a map.
	linksOf := map[string][]CapabilityLink{}
	for _, cr := range crossings {
		linksOf[cr.From] = append(linksOf[cr.From], CapabilityLink{
			To: cr.To, Kinds: cr.Kinds, Count: cr.Count,
			Verdict: cr.Verdict, RuleID: cr.RuleID, Evidence: cr.Evidence,
		})
	}

	out := make([]Capability, 0, len(comps))
	for _, c := range comps {
		capab := Capability{
			Component: c.Name,
			Include:   c.Include,
			Symbols:   c.Symbols,
			Files:     c.Files,
			Evidence:  c.Evidence,
		}
		eps := att.endpointsOf[c.Name]
		refs := make([]SymbolRef, 0, len(eps))
		for _, e := range eps {
			refs = append(refs, e.ref())
		}
		refs, om := truncate(refs, lim.MaxEndpointsPerCapability, "endpoints of "+c.Name,
			"source order: file path, then line")
		capab.Omissions = appendOmission(capab.Omissions, om)
		capab.Endpoints = refs

		tables := collectTables(idx, adj, att.accessSrcOf[c.Name])
		tables, om = truncate(tables, lim.MaxTablesPerCapability, "tables of "+c.Name, "table name")
		capab.Omissions = appendOmission(capab.Omissions, om)
		capab.Tables = tables

		links, om := truncate(linksOf[c.Name], lim.MaxDependenciesPerCapability,
			"dependencies of "+c.Name,
			"the component-dependency order: rule violations first, then most edges, then component name")
		capab.Omissions = appendOmission(capab.Omissions, om)
		capab.DependsOn = links

		// Silent means the index has NOTHING to say, dependencies included. A
		// component that exposes nothing and stores nothing but that three
		// others call is a capability the graph describes perfectly well.
		capab.Silent = len(capab.Endpoints) == 0 && len(capab.Tables) == 0 && len(capab.DependsOn) == 0
		out = append(out, capab)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// Capabilities the graph has something to say about come first; a
		// reader scanning for "what does this system do" should not wade
		// through the components the index is silent on to reach them.
		if a.Silent != b.Silent {
			return !a.Silent
		}
		if n, m := len(a.Endpoints), len(b.Endpoints); n != m {
			return n > m
		}
		if n, m := len(a.Tables), len(b.Tables); n != m {
			return n > m
		}
		if n, m := len(a.DependsOn), len(b.DependsOn); n != m {
			return n > m
		}
		return a.Component < b.Component
	})
	out, om := truncate(out, lim.MaxCapabilities, "candidate capabilities",
		"components the index has endpoints, tables or dependencies for first, then most endpoints, then name")
	return out, appendOmission(oms, om)
}

// deriveSharedTables finds the tables more than one capability reaches.
//
// Derived from the capability rows rather than from the edges a second time, so
// the section cannot contradict the per-capability listings above it: a table
// dropped from a capability by MaxTablesPerCapability is dropped from here too,
// and the omission that reports the first reports the second.
func deriveSharedTables(caps []Capability, lim Limits, oms []Omission) ([]SharedTable, []Omission) {
	var names []string
	byTable := map[string][]TableAccess{}
	for _, c := range caps { // fixed order
		for _, t := range c.Tables { // fixed order
			if _, seen := byTable[t.Table]; !seen {
				names = append(names, t.Table)
			}
			byTable[t.Table] = append(byTable[t.Table], TableAccess{
				Component: c.Component, Access: t.Access, Evidence: t.Evidence,
			})
		}
	}
	sort.Strings(names)

	out := make([]SharedTable, 0, len(names))
	for _, n := range names {
		by := byTable[n]
		if len(by) < 2 {
			continue
		}
		sort.Slice(by, func(i, j int) bool {
			if by[i].Component != by[j].Component {
				return by[i].Component < by[j].Component
			}
			return by[i].Access < by[j].Access
		})
		out = append(out, SharedTable{Table: n, By: by})
	}
	sort.Slice(out, func(i, j int) bool {
		if n, m := len(out[i].By), len(out[j].By); n != m {
			return n > m
		}
		return out[i].Table < out[j].Table
	})
	out, om := truncate(out, lim.MaxSharedTables, "tables reached by more than one capability",
		"most capabilities first, then table name")
	return out, appendOmission(oms, om)
}

// sortedAccessSources returns every symbol that performs a table access, in
// declaration order. Derived by scanning the symbol index rather than the
// accesses map for the reason named at every other map in this file.
func sortedAccessSources(idx *symbolIndex, adj *adjacency) []int64 {
	entries := make([]symEntry, 0, len(adj.accesses))
	for id := range adj.accesses {
		if e, ok := idx.byID[id]; ok {
			entries = append(entries, e)
		}
	}
	sortEntries(entries)
	out := make([]int64, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.sym.ID)
	}
	return out
}

// ---------------------------------------------------------------- helpers

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinPatterns(ps []string) string {
	if len(ps) == 0 {
		return "(none)"
	}
	s := ""
	for i, p := range ps {
		if i > 0 {
			s += ", "
		}
		s += p
	}
	return s
}
