package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The graph explorer.
//
// # Level of detail is a server concern, not a zoom trick
//
// A whole-repository symbol graph is a hairball at any zoom, and a client that
// receives 90,000 nodes and then decides to draw 200 of them has already paid
// for the other 89,800. So the graph is addressable at THREE levels and each
// one is a bounded answer in its own right:
//
//	containers  declared components where the repository declares them,
//	            directories where it does not, with every relationship between
//	            two containers collapsed into one aggregated edge.
//	files       one container's files, with the same aggregation between them.
//	symbols     one container's symbols, capped by a stated rule.
//
// A client drills from one to the next by changing `level` and `container`,
// which is what lets it expand a single container without refetching the
// repository. Each level carries its own community assignment, its own
// aggregated edges and its own budget accounting.
//
// # Aggregation must not launder confidence
//
// Collapsing four hundred edges into one line is only honest if the line reports
// the bundle's WEAKEST surviving tier and carries the full distribution behind
// it — see AggregatedEdge. A bundle drawn at its best member's strength would be
// the exact overclaim this product exists to avoid.
//
// The aggregated-edge table (from, to, edges, weakest, kinds, example evidence)
// is the single most differentiated artifact this product ships: no competitor
// renders a relationship with a measured tier and a checkable file:line beside
// it. It used to be reachable only through a collapsed `<details>` labelled as
// an accessibility fallback. It is a first-class array on this response now, at
// every level, and it is the same type at every level so one renderer draws all
// three.
//
// # Selection, when a bound bites
//
// Symbol-level views are capped. The cap is applied by DEGREE DESCENDING, THEN
// SYMBOL ID — a mechanical, stated, reproducible rule, not a relevance score.
// This layer must not invent a ranking: relevance ranking is internal/context's
// job and it is measured there. Whatever the cap excludes is counted in Budget
// AND in the omission block: never a silent cap.

// GraphLevel names which of the three addressable levels a response carries.
type GraphLevel string

const (
	// LevelContainers is the default: the aggregate picture of the repository.
	LevelContainers GraphLevel = "containers"

	// LevelFiles is one container's files. The middle rung a reader needs
	// between "the whole system" and "one function", and the one an expanding
	// container node should fetch.
	LevelFiles GraphLevel = "files"

	// LevelSymbols is one container's symbols.
	LevelSymbols GraphLevel = "symbols"
)

func levelParam(r *http.Request, container string) GraphLevel {
	switch GraphLevel(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level")))) {
	case LevelContainers:
		return LevelContainers
	case LevelFiles:
		return LevelFiles
	case LevelSymbols:
		return LevelSymbols
	}
	// No level asked for. Naming a container has always meant "drill into it",
	// and that stays the default so an existing link keeps its meaning.
	if container != "" {
		return LevelSymbols
	}
	return LevelContainers
}

// GraphFilters echoes the filters actually applied, after defaults and after
// unrecognised values were dropped.
type GraphFilters struct {
	Level         GraphLevel `json:"level"`
	MinConfidence string     `json:"min_confidence"`
	Kinds         []string   `json:"kinds,omitempty"`
	Language      string     `json:"language,omitempty"`
	Container     string     `json:"container,omitempty"`
	Limit         int        `json:"limit"`

	// Selection states the rule by which a capped view chose what to keep.
	Selection string `json:"selection"`
}

// GraphOptions is what the client needs to render its filter controls without a
// second request.
type GraphOptions struct {
	Containers    []string `json:"containers"`
	ContainerKind string   `json:"container_kind"`
	Languages     []string `json:"languages"`
	Kinds         []string `json:"kinds"`

	// Levels is the closed vocabulary of `?level=`, so a client builds its
	// drill control from the API rather than from a hardcoded list that can
	// drift out of step with it.
	Levels []GraphLevel `json:"levels"`
}

// Budget is one cap, stated in full.
//
// # Why a cap is a payload field and not a footnote
//
// A node budget that silently drops the 400 nodes past its limit produces a
// picture that is wrong in the one way a reader cannot detect: it looks
// complete. So every level reports the budget it applied, how many nodes were
// AVAILABLE before it, how many it DREW, and how many it EXCLUDED — and the
// same figure is recorded as a counted omission, because the omission block is
// where a reader already knows to look for what a view left out.
//
// Excluded is derived from Available and Drawn rather than accumulated, so it
// cannot disagree with them.
type Budget struct {
	// Parameter is the query parameter that raises this budget.
	Parameter string `json:"parameter"`

	Requested int `json:"requested"`
	Maximum   int `json:"maximum"`

	Available int `json:"available"`
	Drawn     int `json:"drawn"`
	Excluded  int `json:"excluded"`

	// Rule is how the budget chose what to keep, in the reader's words.
	Rule string `json:"rule"`

	// Remedy names the control that widens it.
	Remedy string `json:"remedy,omitempty"`
}

func newBudget(param string, requested, maximum, available, drawn int, rule, remedy string) Budget {
	excluded := available - drawn
	if excluded < 0 {
		excluded = 0
	}
	return Budget{
		Parameter: param, Requested: requested, Maximum: maximum,
		Available: available, Drawn: drawn, Excluded: excluded,
		Rule: rule, Remedy: remedy,
	}
}

// ContainerNode is one aggregated grouping.
type ContainerNode struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Symbols   int      `json:"symbols"`
	Exported  int      `json:"exported_symbols"`
	Languages []string `json:"languages"`

	// FilesWithSymbols and FilesMatched are two different facts and were
	// previously both called "files" — by this screen and by governance, which
	// reported 551 and 3,425 for the same component in the same session with
	// nothing to reconcile them.
	//
	// FilesWithSymbols is files that produced at least one symbol: the
	// population this graph can draw, because a file with no symbol has no
	// node. FilesMatched is every file the grouping matched, symbol-bearing or
	// not: the population an architecture include pattern selects. Both come
	// from structuralIndex, computed once, so every screen quoting either is
	// quoting the same arithmetic.
	FilesWithSymbols int `json:"files_with_symbols"`
	FilesMatched     int `json:"files_matched"`

	// Cluster is the community this node was assigned to. See cluster.go for
	// what that does and does not claim.
	Cluster string `json:"cluster,omitempty"`

	// Unassigned marks the bucket holding symbols no declared component
	// matches. Rendered distinctly because every architecture rule is blind to
	// it, and a reader must be able to see how much code that is.
	Unassigned bool `json:"unassigned,omitempty"`
}

// FileNode is one file in a container-level drill-down.
type FileNode struct {
	ID       int64  `json:"id"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	Language string `json:"language"`

	Container string `json:"container"`
	Symbols   int    `json:"symbols"`
	Exported  int    `json:"exported_symbols"`
	Degree    int    `json:"degree"`

	// PartialParse marks a file whose parse did not complete, so the client can
	// say on the node itself that this file's symbol count is a floor. The run's
	// aggregate parse-failure count says how many exist; this says which.
	PartialParse bool `json:"partial_parse"`

	Cluster string `json:"cluster,omitempty"`

	// External marks a file pulled in from outside the requested container, so
	// the reader can see the boundary rather than infer it.
	External bool `json:"external,omitempty"`
}

// AggregatedEdge is every relationship between two drawn nodes, collapsed —
// at any level.
//
// This is the product's differentiating artifact. Count, the WEAKEST surviving
// tier, the kind breakdown under the same floor as Count, and one real
// file:line that can be opened and checked.
type AggregatedEdge struct {
	From string `json:"from"`
	To   string `json:"to"`

	// FromCluster / ToCluster let a client fold this edge into a cluster-level
	// bundle without a second request or a second lookup table.
	FromCluster string `json:"from_cluster,omitempty"`
	ToCluster   string `json:"to_cluster,omitempty"`

	// Count is the number of underlying relationships AT OR ABOVE the floor.
	// Kinds sums to exactly this figure — the two were once conditioned
	// differently and rendered side by side, which produced rows reading
	// `edges 50 · CALLS×572`.
	Count int `json:"count"`

	// Confidence is the WEAKEST surviving tier in the bundle — the tier the
	// line is drawn at. A bundle is no stronger than its worst member.
	Confidence string `json:"confidence"`

	// Histogram is the full spread, so a thick line can be read as "four
	// hundred guesses" rather than mistaken for four hundred proofs.
	Histogram Histogram `json:"histogram"`

	Kinds []KindCount `json:"kinds"`

	// Evidence is one real file:line from the bundle, so it stays checkable.
	Evidence Evidence `json:"evidence"`
}

// ContainerEdge is retained as the name the container level uses. Same type:
// one renderer draws every level.
type ContainerEdge = AggregatedEdge

// KindCount is one edge kind's share of an aggregated bundle, under the same
// floor as the bundle's Count.
type KindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// Evidence is the source location that justifies a relationship. Without it an
// edge is an assertion; with it the reader can go and check.
type Evidence struct {
	FilePath string `json:"file_path,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// SymbolNode is one symbol in a drilled-in view.
type SymbolNode struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	FQN       string `json:"fqn"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Language  string `json:"language"`
	Container string `json:"container"`
	Exported  bool   `json:"exported"`
	Degree    int    `json:"degree"`

	// Line is where this symbol is declared, so a reader can open it. Zero means
	// the index holds no position for it — SCIP-only symbols have none — and the
	// client must say so rather than printing line 0.
	Line int `json:"line"`

	// Cluster is the community this symbol was assigned to within the drilled
	// container.
	Cluster string `json:"cluster,omitempty"`

	// Confidence is how reliably the SYMBOL ITSELF was extracted, which is a
	// different fact from the confidence of any edge touching it.
	Confidence string `json:"confidence"`

	// External marks a neighbour pulled in from outside the requested
	// container, so the reader can see the boundary rather than infer it.
	External bool `json:"external,omitempty"`
}

// SymbolEdge is one relationship between two symbols, aggregated over the
// evidence locations that recorded it.
type SymbolEdge struct {
	Src        int64    `json:"src"`
	Dst        int64    `json:"dst"`
	Kind       string   `json:"kind"`
	Confidence string   `json:"confidence"`
	Count      int      `json:"count"`
	Evidence   Evidence `json:"evidence"`

	// SrcCluster / DstCluster mirror AggregatedEdge, so symbol-level edges fold
	// into cluster bundles the same way.
	SrcCluster string `json:"src_cluster,omitempty"`
	DstCluster string `json:"dst_cluster,omitempty"`
}

// GraphResponse is the explorer's payload.
type GraphResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Level is which of the three addressable levels this body carries.
	Level   GraphLevel   `json:"level"`
	Filters GraphFilters `json:"filters"`
	Options GraphOptions `json:"options"`

	// NodeBudget is the cap on the nodes of THIS level, stated in full.
	NodeBudget Budget `json:"node_budget"`

	// Clusters partitions the drawn nodes of this level. Always present, even
	// when every node is its own community — a client must not have to guess
	// whether an absent array means "no clusters" or "not computed".
	Clusters []Cluster `json:"clusters"`

	// ClusterEdges is the cluster-to-cluster bundle: the top of the semantic
	// zoom, so a client can draw the collapsed picture without folding the
	// edges itself and without disagreeing with the server about the weakest
	// tier when it does.
	ClusterEdges []AggregatedEdge `json:"cluster_edges"`

	// The container level. Present on every response, because the drilled-in
	// levels still need the aggregate picture around them.
	Containers     []ContainerNode  `json:"containers"`
	ContainerEdges []AggregatedEdge `json:"container_edges"`

	// The file level, when asked for.
	Files     []FileNode       `json:"files"`
	FileEdges []AggregatedEdge `json:"file_edges"`

	// The symbol level, when asked for.
	Nodes []SymbolNode `json:"nodes"`
	Edges []SymbolEdge `json:"edges"`

	Omissions Omissions `json:"omissions"`
}

// maxExternalNeighbours bounds how many out-of-container symbols a drilled-in
// view pulls in to show the boundary. Small on purpose: the point is to show
// that a boundary is crossed, not to redraw the neighbouring container.
const maxExternalNeighbours = 120

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.writeJSON(w, http.StatusOK, emptyGraph(meta))
		return
	}
	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	floor := confidenceParam(r, "min_confidence", DefaultFloor)
	kinds := kindsParam(r, "kinds")
	language := strings.TrimSpace(r.URL.Query().Get("language"))
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	level := levelParam(r, container)
	limit := intParam(r, "limit", s.maxNodes, 10, DefaultMaxNodes)
	// How many containers to DRAW. Separate from `limit`, which bounds symbols
	// in a drilled-in view: one is a readability bound on the aggregate picture,
	// the other a cost bound on an expanded one.
	budget := intParam(r, "nodes", defaultContainers, 1, maxContainers)

	var om Omissions
	resp := GraphResponse{
		Meta: meta, Legend: Legend(), Level: level,
		Filters: GraphFilters{
			Level:         level,
			MinConfidence: string(floor),
			Kinds:         kindStrings(kinds),
			Language:      language,
			Container:     container,
			Limit:         limit,
			Selection:     "all container nodes; no selection rule was needed",
		},
		Options:      idx.options(),
		Clusters:     []Cluster{},
		ClusterEdges: []AggregatedEdge{},
		Containers:   []ContainerNode{}, ContainerEdges: []AggregatedEdge{},
		Files: []FileNode{}, FileEdges: []AggregatedEdge{},
		Nodes: []SymbolNode{}, Edges: []SymbolEdge{},
	}

	switch level {
	case LevelFiles:
		resp.Filters.Selection = "files with the most edges first, then path — a mechanical rule, not a relevance score"
	case LevelSymbols:
		resp.Filters.Selection = "highest edge degree first, then symbol id — a mechanical rule, not a relevance score"
	}

	if err := s.buildContainerView(ctx, idx, floor, kinds, language, budget, level, &resp, &om); err != nil {
		s.fail(w, r, err)
		return
	}
	var drillErr error
	switch level {
	case LevelFiles:
		drillErr = s.buildFileView(ctx, idx, container, floor, kinds, language, budget, &resp, &om)
	case LevelSymbols:
		drillErr = s.buildSymbolView(ctx, idx, container, floor, kinds, language, limit, &resp, &om)
	}
	if drillErr != nil {
		s.fail(w, r, drillErr)
		return
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// emptyGraph is the not-indexed answer, with every array non-null so a client
// never has to distinguish "absent" from "empty".
func emptyGraph(meta Meta) GraphResponse {
	return GraphResponse{
		Meta: meta, Legend: Legend(), Level: LevelContainers,
		Filters:  GraphFilters{Level: LevelContainers},
		Options:  GraphOptions{Levels: []GraphLevel{LevelContainers, LevelFiles, LevelSymbols}},
		Clusters: []Cluster{}, ClusterEdges: []AggregatedEdge{},
		Containers: []ContainerNode{}, ContainerEdges: []AggregatedEdge{},
		Files: []FileNode{}, FileEdges: []AggregatedEdge{},
		Nodes: []SymbolNode{}, Edges: []SymbolEdge{},
	}
}

func (idx *structuralIndex) options() GraphOptions {
	langs := make([]string, 0, len(idx.symLangs))
	for l := range idx.symLangs {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	kinds := make([]string, 0, len(idx.byKind))
	for k := range idx.byKind {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)

	return GraphOptions{
		Containers:    idx.containerOrder,
		ContainerKind: idx.containerKind,
		Languages:     langs,
		Kinds:         kinds,
		Levels:        []GraphLevel{LevelContainers, LevelFiles, LevelSymbols},
	}
}

// buildContainerView fills in the default, always-present aggregated view.
func (s *Server) buildContainerView(
	ctx context.Context,
	idx *structuralIndex,
	floor model.Confidence,
	kinds []model.EdgeKind,
	language string,
	budget int,
	level GraphLevel,
	resp *GraphResponse,
	om *Omissions,
) error {
	keep := kindSet(kinds)

	nodes := make([]ContainerNode, 0, len(idx.containers))
	for _, name := range idx.containerOrder {
		c := idx.containers[name]
		if language != "" && c.Languages[language] == 0 {
			continue
		}
		langs := make([]string, 0, len(c.Languages))
		for l := range c.Languages {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		nodes = append(nodes, ContainerNode{
			ID: name, Label: name, Symbols: len(c.Symbols),
			FilesWithSymbols: len(c.SymbolFiles), FilesMatched: c.AllFiles,
			Exported: c.Exported, Languages: langs,
			Unassigned: name == unassignedContainer,
		})
	}
	if language != "" {
		om.Note(ReasonRowBudget,
			fmt.Sprintf("Containers holding no %s symbols are not drawn. They still exist in the index "+
				"and relationships may run through them.", language),
			"clear the language filter")
	}
	available := len(nodes)
	// Ranked by SIZE before the cap, not left in alphabetical order.
	//
	// The cap was documented as firing "only on a pathological tree". It fires
	// on an ordinary large one: a 537-directory repository loses 137 containers,
	// and alphabetical order chose which — so the graph opened on `.claude/`,
	// `.github/` and `.worktrees/` while the largest packages in the system fell
	// off the end. Which containers are dropped is a real decision and it was
	// being made by the filesystem.
	//
	// Symbols descending, then name ascending so the order is total and two runs
	// cannot disagree. Name order is restored after the cut, so the DRAWING is
	// still alphabetical — only the SELECTION is by size.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Symbols != nodes[j].Symbols {
			return nodes[i].Symbols > nodes[j].Symbols
		}
		return nodes[i].ID < nodes[j].ID
	})
	drawn := capped(nodes, budget, om, ReasonNodeBudget,
		fmt.Sprintf("Containers beyond the largest %d are not drawn — a READABILITY bound, not a "+
			"cost one: %d nodes on this canvas is a hairball with no legible label. Selection is by "+
			"symbol count, so what is missing is the smallest %d, not an alphabetical tail.",
			budget, available, available-min(available, budget)),
		fmt.Sprintf("?nodes= up to %d to draw more, or declare an architecture in .repocortex.yml so the "+
			"explorer groups by component instead of directory", maxContainers))
	sort.Slice(drawn, func(i, j int) bool { return drawn[i].ID < drawn[j].ID })
	resp.Containers = drawn
	if level == LevelContainers {
		resp.NodeBudget = newBudget("nodes", budget, maxContainers, available, len(drawn),
			"largest by symbol count first, then container name — a mechanical rule, not a relevance score",
			fmt.Sprintf("?nodes= up to %d", maxContainers))
	}

	present := make(map[string]struct{}, len(resp.Containers))
	for _, n := range resp.Containers {
		present[n.ID] = struct{}{}
	}

	var hiddenByFloor, hiddenByKind int
	edges := make([]AggregatedEdge, 0, len(idx.containerEdges))
	for _, agg := range idx.containerEdges {
		if _, ok := present[agg.From]; !ok {
			continue
		}
		if _, ok := present[agg.To]; !ok {
			continue
		}
		kept, kindRows, dropped := agg.underFilter(keep, floor)
		hiddenByKind += dropped
		if kept.Total == 0 {
			continue
		}
		below := kept.Below(floor)
		hiddenByFloor += below
		surviving := kept.AtOrAbove(floor)
		if surviving == 0 {
			// Every edge in this bundle is below the floor. The bundle is not
			// drawn, and the count above is what says so.
			continue
		}
		edges = append(edges, AggregatedEdge{
			From: agg.From, To: agg.To,
			Count:      surviving,
			Confidence: weakestAtOrAbove(kept, floor),
			Histogram:  kept,
			Kinds:      kindRows,
			Evidence:   Evidence{FilePath: agg.Example.File, Line: int(agg.Example.Line)},
		})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	resp.ContainerEdges = capped(edges, DefaultMaxEdges, om, ReasonEdgeBudget,
		fmt.Sprintf("Combined container relationships beyond the first %d are not drawn.", DefaultMaxEdges),
		"filter by edge kind or by container")

	if level == LevelContainers {
		keys := make([]string, 0, len(resp.Containers))
		for _, n := range resp.Containers {
			keys = append(keys, n.ID)
		}
		if err := assignClusters(ctx, keys, resp.ContainerEdges, resp); err != nil {
			return err
		}
		for i := range resp.Containers {
			resp.Containers[i].Cluster = resp.clusterOf(resp.Containers[i].ID)
		}
	}

	om.Hide(ReasonBelowConfidenceFloor, hiddenByFloor,
		fmt.Sprintf("Relationships weaker than %s are not drawn at this standard of proof. They remain in the index; "+
			"docs/accuracy.md measures the tiers below it at 0.0–37.5%% precision, so most describe a "+
			"relationship that does not exist — most, not all.", tierWord(floor)),
		"lower the standard of proof to `low`")
	om.Hide(ReasonRowBudget, hiddenByKind,
		"Relationships whose edge kind is outside the current kind filter.", "clear the kind filter")
	return nil
}

// clusterOf reads the assignment back off the summarised clusters.
//
// Linear over a list that is small by construction (one community per drawn
// node at worst) and read a bounded number of times, which is cheaper than
// carrying a second map through the response purely to answer it.
func (resp *GraphResponse) clusterOf(key string) string {
	for _, c := range resp.Clusters {
		for _, m := range c.Members {
			if m == key {
				return c.ID
			}
		}
	}
	return ""
}

// assignClusters runs community detection over the drawn nodes and edges of one
// level and fills in Clusters and ClusterEdges.
//
// Weight is the aggregated edge's Count — the number of underlying
// relationships at or above the floor — so two containers joined by 400 calls
// pull together harder than two joined by one. Direction is discarded: a
// community is about who talks to whom, not who started.
func assignClusters(ctx context.Context, keys []string, edges []AggregatedEdge, resp *GraphResponse) error {
	assignment, err := detectClusters(ctx, keys, edges)
	if err != nil {
		return err
	}
	resp.Clusters = summariseClusters(assignment, edges)

	// Fold the level's own aggregated edges into cluster-to-cluster bundles,
	// preserving the same invariants: weakest tier wins, kinds sum to count,
	// one real file:line survives.
	type pair struct{ From, To string }
	agg := map[pair]*AggregatedEdge{}
	var order []pair
	for i := range edges {
		e := edges[i]
		from, to := assignment[e.From], assignment[e.To]
		if from == "" || to == "" || from == to {
			continue
		}
		k := pair{from, to}
		got, ok := agg[k]
		if !ok {
			cp := AggregatedEdge{From: from, To: to, Confidence: e.Confidence, Evidence: e.Evidence}
			agg[k] = &cp
			order = append(order, k)
			got = &cp
		}
		got.Count += e.Count
		addHistogram(&got.Histogram, e.Histogram)
		got.Kinds = mergeKindCounts(got.Kinds, e.Kinds)
		if model.Confidence(e.Confidence).Rank() < model.Confidence(got.Confidence).Rank() {
			got.Confidence = e.Confidence
		}
		// The strongest member's evidence is the one worth pointing at, matching
		// structuralIndex.aggregate. Weakest still reports the bundle's floor.
		if e.Evidence.FilePath != "" && got.Evidence.FilePath == "" {
			got.Evidence = e.Evidence
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].From != order[j].From {
			return order[i].From < order[j].From
		}
		return order[i].To < order[j].To
	})
	out := make([]AggregatedEdge, 0, len(order))
	for _, k := range order {
		e := *agg[k]
		sortKindCounts(e.Kinds)
		out = append(out, e)
	}
	resp.ClusterEdges = out

	// Stamp the cluster on every drawn edge, so a client folding edges itself
	// reaches the same bundles the server did.
	for i := range edges {
		edges[i].FromCluster = assignment[edges[i].From]
		edges[i].ToCluster = assignment[edges[i].To]
	}
	return nil
}

func addHistogram(dst *Histogram, src Histogram) {
	dst.Exact += src.Exact
	dst.High += src.High
	dst.Medium += src.Medium
	dst.Low += src.Low
	dst.Unknown += src.Unknown
	dst.Total += src.Total
}

func mergeKindCounts(dst, src []KindCount) []KindCount {
	for _, s := range src {
		found := false
		for i := range dst {
			if dst[i].Kind == s.Kind {
				dst[i].Count += s.Count
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, s)
		}
	}
	return dst
}

func sortKindCounts(in []KindCount) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Count != in[j].Count {
			return in[i].Count > in[j].Count
		}
		return in[i].Kind < in[j].Kind
	})
}

// buildFileView draws one container's FILES, with the same aggregation the
// container level uses between them.
//
// The rung that was missing. Between "19 containers" and "1,620 symbols" there
// was nothing, so expanding a container went straight to a barcode: 1,620 nodes
// at three screen pixels each. A file graph of a container is the size a reader
// can actually hold, and it is the level at which an aggregated edge still
// carries a meaningful example file:line.
func (s *Server) buildFileView(
	ctx context.Context,
	idx *structuralIndex,
	container string,
	floor model.Confidence,
	kinds []model.EdgeKind,
	language string,
	budget int,
	resp *GraphResponse,
	om *Omissions,
) error {
	c, ok := idx.containers[container]
	if !ok {
		om.Note(ReasonRowBudget,
			fmt.Sprintf("No container named %q exists in this index, so no files are drawn.", container),
			"pick one of options.containers")
		resp.NodeBudget = newBudget("nodes", budget, maxContainers, 0, 0,
			"no container was named, so nothing was selected", "?container= one of options.containers")
		return nil
	}
	keep := kindSet(kinds)

	// File membership, and the per-file counts, from the symbols already held.
	type fileAgg struct {
		ID       int64
		Symbols  int
		Exported int
		Degree   int
	}
	byFile := map[int64]*fileAgg{}
	langFiltered := 0
	symbolFile := make(map[int64]int64, len(c.Symbols))
	for _, id := range c.Symbols {
		if !idx.matchesLanguage(id, language) {
			langFiltered++
			continue
		}
		sym := idx.syms[id]
		symbolFile[id] = sym.FileID
		f, ok := byFile[sym.FileID]
		if !ok {
			f = &fileAgg{ID: sym.FileID}
			byFile[sym.FileID] = f
		}
		f.Symbols++
		f.Degree += sym.Degree
		if sym.Exported {
			f.Exported++
		}
	}
	om.Hide(ReasonRowBudget, langFiltered,
		fmt.Sprintf("Symbols in %s that are not %s, and the files holding only those.", container, language),
		"clear the language filter")

	ids := make([]int64, 0, len(byFile))
	for id := range byFile {
		ids = append(ids, id)
	}
	// Degree descending, then PATH ascending — path, not file id, because a
	// file id is an insertion counter and ordering on it would make the cut
	// depend on indexing order.
	sort.Slice(ids, func(i, j int) bool {
		a, b := byFile[ids[i]], byFile[ids[j]]
		if a.Degree != b.Degree {
			return a.Degree > b.Degree
		}
		return idx.files[ids[i]].Path < idx.files[ids[j]].Path
	})
	available := len(ids)
	kept := capped(ids, budget, om, ReasonNodeBudget,
		fmt.Sprintf("Files in %s beyond the %d with the most edges are not drawn. Selection is by summed "+
			"symbol degree, then path — a mechanical rule, not a relevance ranking.", container, budget),
		fmt.Sprintf("?nodes= up to %d, or drill to ?level=symbols on one file's container", maxContainers))
	resp.NodeBudget = newBudget("nodes", budget, maxContainers, available, len(kept),
		"summed symbol degree first, then path — a mechanical rule, not a relevance score",
		fmt.Sprintf("?nodes= up to %d", maxContainers))

	inView := make(map[int64]struct{}, len(kept))
	for _, id := range kept {
		inView[id] = struct{}{}
	}

	// Aggregate the run's edges by (source file, destination file). One scan,
	// the same one the symbol view uses, over a slice already in memory.
	type filePair struct{ From, To int64 }
	type fileBundle struct {
		Conf    Histogram
		Kinds   map[model.EdgeKind]*Histogram
		Example compactEdge
		HasEx   bool
	}
	bundles := map[filePair]*fileBundle{}
	externals := map[int64]struct{}{}
	var hiddenByFloor, hiddenByKind int

	for i := range idx.edges {
		e := idx.edges[i]
		srcSym, srcOK := idx.syms[e.Src]
		dstSym, dstOK := idx.syms[e.Dst]
		if !srcOK || !dstOK {
			continue
		}
		srcFile, dstFile := srcSym.FileID, dstSym.FileID
		if srcFile == dstFile {
			continue
		}
		_, srcIn := inView[srcFile]
		_, dstIn := inView[dstFile]
		if !srcIn && !dstIn {
			continue
		}
		if len(keep) > 0 {
			if _, want := keep[e.Kind]; !want {
				hiddenByKind++
				continue
			}
		}
		if !e.Confidence.AtLeast(floor) {
			hiddenByFloor++
			continue
		}
		if !srcIn {
			if len(externals) >= maxExternalNeighbours {
				continue
			}
			externals[srcFile] = struct{}{}
		}
		if !dstIn {
			if len(externals) >= maxExternalNeighbours {
				continue
			}
			externals[dstFile] = struct{}{}
		}
		k := filePair{srcFile, dstFile}
		b, ok := bundles[k]
		if !ok {
			b = &fileBundle{Kinds: map[model.EdgeKind]*Histogram{}}
			bundles[k] = b
		}
		b.Conf.add(e.Confidence)
		histogramFor(b.Kinds, e.Kind).add(e.Confidence)
		if !b.HasEx || e.Confidence.Rank() > b.Example.Confidence.Rank() {
			b.Example, b.HasEx = e, true
		}
	}

	nodes := make([]FileNode, 0, len(kept)+len(externals))
	for _, id := range kept {
		f := byFile[id]
		nodes = append(nodes, idx.fileNode(id, container, f.Symbols, f.Exported, f.Degree, false))
	}
	extIDs := make([]int64, 0, len(externals))
	for id := range externals {
		extIDs = append(extIDs, id)
	}
	sort.Slice(extIDs, func(i, j int) bool { return idx.files[extIDs[i]].Path < idx.files[extIDs[j]].Path })
	for _, id := range extIDs {
		nodes = append(nodes, idx.fileNode(id, "", 0, 0, 0, true))
	}
	resp.Files = nodes

	pairs := make([]filePair, 0, len(bundles))
	for k := range bundles {
		pairs = append(pairs, k)
	}
	sort.Slice(pairs, func(i, j int) bool {
		pi, pj := idx.files[pairs[i].From].Path, idx.files[pairs[j].From].Path
		if pi != pj {
			return pi < pj
		}
		return idx.files[pairs[i].To].Path < idx.files[pairs[j].To].Path
	})
	edges := make([]AggregatedEdge, 0, len(pairs))
	for _, k := range pairs {
		b := bundles[k]
		rows := make([]KindCount, 0, len(b.Kinds))
		for kind, h := range b.Kinds {
			if n := h.AtOrAbove(floor); n > 0 {
				rows = append(rows, KindCount{Kind: string(kind), Count: n})
			}
		}
		sortKindCounts(rows)
		edges = append(edges, AggregatedEdge{
			From: idx.files[k.From].Path, To: idx.files[k.To].Path,
			Count:      b.Conf.AtOrAbove(floor),
			Confidence: weakestAtOrAbove(b.Conf, floor),
			Histogram:  b.Conf,
			Kinds:      rows,
			Evidence:   Evidence{FilePath: b.Example.File, Line: int(b.Example.Line)},
		})
	}
	resp.FileEdges = capped(edges, DefaultMaxEdges, om, ReasonEdgeBudget,
		fmt.Sprintf("Combined file relationships beyond the first %d are not drawn.", DefaultMaxEdges),
		"narrow the kind filter, or raise the standard of proof")

	keys := make([]string, 0, len(resp.Files))
	for _, n := range resp.Files {
		keys = append(keys, n.Path)
	}
	if err := assignClusters(ctx, keys, resp.FileEdges, resp); err != nil {
		return err
	}
	for i := range resp.Files {
		resp.Files[i].Cluster = resp.clusterOf(resp.Files[i].Path)
	}

	om.Hide(ReasonBelowConfidenceFloor, hiddenByFloor,
		fmt.Sprintf("File relationships weaker than %s, inside or crossing %s.", tierWord(floor), container),
		"lower the standard of proof to `low`")
	om.Hide(ReasonRowBudget, hiddenByKind,
		"File relationships outside the current kind filter.", "clear the kind filter")
	return nil
}

func (idx *structuralIndex) fileNode(id int64, container string, symbols, exported, degree int, external bool) FileNode {
	f := idx.files[id]
	name := f.Path
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if container == "" {
		container = containerOfPath(idx, id)
	}
	return FileNode{
		ID: id, Path: f.Path, Name: name, Language: f.Language,
		Container: container, Symbols: symbols, Exported: exported, Degree: degree,
		PartialParse: f.PartialParse,
		External:     external,
	}
}

// containerOfPath answers which container a file belongs to, for the external
// neighbours a drill-down pulls in. Read off a symbol in the file rather than
// re-running the glob resolver, because structuralIndex already resolved it.
func containerOfPath(idx *structuralIndex, fileID int64) string {
	best := ""
	for _, c := range idx.containerOrder {
		if _, ok := idx.containers[c].SymbolFiles[fileID]; ok {
			// containerOrder is sorted, so the first match is a total rule.
			best = c
			break
		}
	}
	return best
}

// buildSymbolView fills in the drilled-in symbol graph for one container.
func (s *Server) buildSymbolView(
	ctx context.Context,
	idx *structuralIndex,
	container string,
	floor model.Confidence,
	kinds []model.EdgeKind,
	language string,
	limit int,
	resp *GraphResponse,
	om *Omissions,
) error {
	c, ok := idx.containers[container]
	if !ok {
		om.Note(ReasonRowBudget,
			fmt.Sprintf("No container named %q exists in this index, so no symbols are drawn.", container),
			"pick one of options.containers")
		resp.NodeBudget = newBudget("limit", limit, DefaultMaxNodes, 0, 0,
			"no container was named, so nothing was selected", "?container= one of options.containers")
		return nil
	}
	keep := kindSet(kinds)

	// Candidate members, filtered by language, ordered by the stated rule.
	members := make([]int64, 0, len(c.Symbols))
	langFiltered := 0
	for _, id := range c.Symbols {
		if !idx.matchesLanguage(id, language) {
			langFiltered++
			continue
		}
		members = append(members, id)
	}
	sort.Slice(members, func(i, j int) bool {
		a, b := idx.syms[members[i]], idx.syms[members[j]]
		if a.Degree != b.Degree {
			return a.Degree > b.Degree
		}
		return members[i] < members[j]
	})
	om.Hide(ReasonRowBudget, langFiltered,
		fmt.Sprintf("Symbols in %s that are not %s.", container, language), "clear the language filter")

	available := len(members)
	members = capped(members, limit, om, ReasonNodeBudget,
		fmt.Sprintf("Symbols in %s beyond the %d with the most edges. Selection is by degree then id — "+
			"a mechanical rule, not a relevance ranking, because ranking belongs to the context engine "+
			"where it is measured.", container, limit),
		"raise ?limit= (up to "+fmt.Sprint(DefaultMaxNodes)+"), drop to ?level=files, or filter by language")
	resp.NodeBudget = newBudget("limit", limit, DefaultMaxNodes, available, len(members),
		"highest edge degree first, then symbol id — a mechanical rule, not a relevance score",
		fmt.Sprintf("?limit= up to %d, or ?level=files for a picture this size can be read at", DefaultMaxNodes))

	inView := make(map[int64]struct{}, len(members))
	for _, id := range members {
		inView[id] = struct{}{}
	}

	// One scan over the run's edges. An adjacency map would be faster per query
	// and would cost a permanent per-symbol allocation on a 100k-symbol index;
	// the scan is linear over a slice already in memory and is what keeps the
	// cached index compact.
	type edgeKey struct {
		Src, Dst int64
		Kind     model.EdgeKind
	}
	agg := map[edgeKey]*SymbolEdge{}
	externals := map[int64]struct{}{}
	var hiddenByFloor, hiddenByKind int

	for i := range idx.edges {
		e := idx.edges[i]
		_, srcIn := inView[e.Src]
		_, dstIn := inView[e.Dst]
		if !srcIn && !dstIn {
			continue
		}
		if len(keep) > 0 {
			if _, want := keep[e.Kind]; !want {
				hiddenByKind++
				continue
			}
		}
		if !e.Confidence.AtLeast(floor) {
			hiddenByFloor++
			continue
		}
		// A neighbour outside the container is shown so the boundary is
		// visible, but only up to a bound — the point is that a boundary is
		// crossed, not to redraw the neighbouring container.
		if !srcIn {
			if len(externals) >= maxExternalNeighbours {
				continue
			}
			externals[e.Src] = struct{}{}
		}
		if !dstIn {
			if len(externals) >= maxExternalNeighbours {
				continue
			}
			externals[e.Dst] = struct{}{}
		}
		key := edgeKey{Src: e.Src, Dst: e.Dst, Kind: e.Kind}
		got, ok := agg[key]
		if !ok {
			agg[key] = &SymbolEdge{
				Src: e.Src, Dst: e.Dst, Kind: string(e.Kind),
				Confidence: string(e.Confidence), Count: 1,
				Evidence: Evidence{FilePath: e.File, Line: int(e.Line)},
			}
			continue
		}
		got.Count++
		// Several evidence rows for one relationship: report the WEAKEST, for
		// the same reason an aggregated container edge does.
		if model.Confidence(got.Confidence).Rank() > e.Confidence.Rank() {
			got.Confidence = string(e.Confidence)
			got.Evidence = Evidence{FilePath: e.File, Line: int(e.Line)}
		}
	}

	nodes := make([]SymbolNode, 0, len(members)+len(externals))
	for _, id := range members {
		nodes = append(nodes, idx.symbolNode(id, false))
	}
	extIDs := make([]int64, 0, len(externals))
	for id := range externals {
		extIDs = append(extIDs, id)
	}
	sort.Slice(extIDs, func(i, j int) bool { return extIDs[i] < extIDs[j] })
	for _, id := range extIDs {
		nodes = append(nodes, idx.symbolNode(id, true))
	}
	resp.Nodes = nodes

	edges := make([]SymbolEdge, 0, len(agg))
	for _, e := range agg {
		edges = append(edges, *e)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Src != edges[j].Src {
			return edges[i].Src < edges[j].Src
		}
		if edges[i].Dst != edges[j].Dst {
			return edges[i].Dst < edges[j].Dst
		}
		return edges[i].Kind < edges[j].Kind
	})
	resp.Edges = capped(edges, DefaultMaxEdges, om, ReasonEdgeBudget,
		fmt.Sprintf("Symbol relationships beyond the first %d are not drawn.", DefaultMaxEdges),
		"narrow the kind filter, or drill into a smaller container")

	// Communities over the drawn symbols. Keyed by symbol id rendered as a
	// string, so the same detector serves all three levels; the ids are already
	// a total order.
	keys := make([]string, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		keys = append(keys, itoa64(n.ID))
	}
	folded := make([]AggregatedEdge, 0, len(resp.Edges))
	for _, e := range resp.Edges {
		folded = append(folded, AggregatedEdge{From: itoa64(e.Src), To: itoa64(e.Dst), Count: e.Count})
	}
	if err := assignClusters(ctx, keys, folded, resp); err != nil {
		return err
	}
	for i := range resp.Nodes {
		resp.Nodes[i].Cluster = resp.clusterOf(itoa64(resp.Nodes[i].ID))
	}
	for i := range resp.Edges {
		resp.Edges[i].SrcCluster = resp.clusterOf(itoa64(resp.Edges[i].Src))
		resp.Edges[i].DstCluster = resp.clusterOf(itoa64(resp.Edges[i].Dst))
	}

	om.Hide(ReasonBelowConfidenceFloor, hiddenByFloor,
		fmt.Sprintf("Symbol relationships weaker than %s, inside or crossing %s.", tierWord(floor), container),
		"lower the standard of proof to `low`")
	om.Hide(ReasonRowBudget, hiddenByKind,
		"Symbol relationships outside the current kind filter.", "clear the kind filter")
	return nil
}

func itoa64(n int64) string {
	if n >= 0 && n < 1<<31 {
		return itoa(int(n))
	}
	return fmt.Sprint(n)
}

func (idx *structuralIndex) symbolNode(id int64, external bool) SymbolNode {
	s, ok := idx.syms[id]
	if !ok {
		// The graph and the symbol table disagree. Report the id rather than
		// dropping the node: silently losing a symbol is the failure mode this
		// whole package is built to avoid, and `repocortex doctor --verify-index`
		// exists to find the cause.
		return SymbolNode{ID: id, Name: fmt.Sprintf("symbol#%d", id), Kind: "unknown", External: external}
	}
	return SymbolNode{
		ID: id, Name: s.Name, FQN: s.FQN, Kind: string(s.Kind),
		Path: idx.symbolPath(id), Language: idx.symbolLanguage(id),
		Container: s.Container, Exported: s.Exported, Degree: s.Degree,
		Line:       s.Line,
		Confidence: tierOf(s.Confidence), External: external,
	}
}

// underFilter recomputes a bundle under a kind filter and a floor, EXACTLY.
//
// The aggregate keeps a full kind×tier cross-tabulation (see aggregatedEdge), so
// the surviving histogram and the per-kind rows are sums over the same cells:
// the kind rows add up to the bundle's Count by construction. The previous
// implementation kept only per-kind totals and subtracted a kind filter's
// removals from the weakest tiers first — an approximation that made a filtered
// bundle read weaker than it was, and that put a floored Count beside an
// unfloored kind histogram in the same rendered row.
func (a *aggregatedEdge) underFilter(keep map[model.EdgeKind]struct{}, floor model.Confidence) (Histogram, []KindCount, int) {
	var kept Histogram
	rows := make([]KindCount, 0, len(a.Kinds))
	dropped := 0
	// Iterated over a map only to accumulate; rows are sorted before return and
	// kept is a sum, so no ordering decision depends on the traversal.
	for kind, h := range a.Kinds {
		if len(keep) > 0 {
			if _, want := keep[kind]; !want {
				dropped += h.Total
				continue
			}
		}
		addHistogram(&kept, *h)
		if n := h.AtOrAbove(floor); n > 0 {
			rows = append(rows, KindCount{Kind: string(kind), Count: n})
		}
	}
	sortKindCounts(rows)
	return kept, rows, dropped
}

func kindSet(kinds []model.EdgeKind) map[model.EdgeKind]struct{} {
	if len(kinds) == 0 {
		return nil
	}
	m := make(map[model.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		m[k] = struct{}{}
	}
	return m
}

func kindStrings(kinds []model.EdgeKind) []string {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// weakestAtOrAbove reports the weakest tier that SURVIVES the floor, which is
// the tier the line is drawn at. Reporting the bundle's absolute weakest after a
// floor has removed it would draw a line weaker than anything it contains.
func weakestAtOrAbove(h Histogram, floor model.Confidence) string {
	for i := len(confidenceOrder) - 1; i >= 0; i-- {
		c := confidenceOrder[i]
		if !c.AtLeast(floor) {
			continue
		}
		var n int
		switch c {
		case model.ConfidenceExact:
			n = h.Exact
		case model.ConfidenceHigh:
			n = h.High
		case model.ConfidenceMedium:
			n = h.Medium
		case model.ConfidenceLow:
			n = h.Low
		}
		if n > 0 {
			return string(c)
		}
	}
	return "unknown"
}
