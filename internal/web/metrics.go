package web

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// MetricsLevel is the unit a metric is computed over.
//
// The same three levels the graph explorer draws, deliberately: a reader who
// has learned "containers, files, symbols" there should not have to learn a
// second vocabulary to read a centrality score. The level changes what the
// number MEANS — "the most central symbol" and "the most central directory"
// are different claims — so it is a parameter, never an internal default.
type MetricsLevel string

const (
	LevelSymbol    MetricsLevel = "symbol"
	LevelFile      MetricsLevel = "file"
	LevelDirectory MetricsLevel = "directory"
)

// metricsLevelParam reads ?level=, defaulting to symbol.
//
// Symbol is the default because it is the only level where the next action on a
// row ("open blast radius on this") is a real action rather than a gesture at a
// directory.
func metricsLevelParam(r *http.Request) MetricsLevel {
	switch MetricsLevel(r.URL.Query().Get("level")) {
	case LevelFile:
		return LevelFile
	case LevelDirectory:
		return LevelDirectory
	default:
		return LevelSymbol
	}
}

// metricsNode is what a graph key refers to, so a rendered row can name a real
// thing the reader can open. A key alone is a string; a reader needs the file
// and line to go and look.
type metricsNode struct {
	Key      string
	Label    string
	Path     string
	Line     int
	Kind     model.EntityKind
	Exported bool
	SymbolID int64
}

// metricsDropped is every edge a projection removed, by CAUSE.
//
// Two counts and not one sum, because the reader's remedy differs: the floor is
// undone by lowering min_confidence, the collapse by asking for level=symbol.
// They are also different populations — an edge can only be counted in one of
// them — so publishing them separately can never double-count an edge.
//
// A struct rather than a fourth and fifth return value: at four unnamed ints a
// caller's `dropped` and `collapsed` are interchangeable at the call site, and
// a transposition would compile and publish the wrong number under the wrong
// sentence. That is precisely the defect this fix exists to remove.
type metricsDropped struct {
	// BelowFloor is edges the confidence floor removed, directly or by taking
	// an endpoint with it.
	BelowFloor int

	// IntraGroup is edges between two DISTINCT symbols that a path-level
	// projection collapsed onto one node. Always zero at symbol level, where
	// nothing is collapsed. Self-loops are excluded: they are disclosed as
	// recursion, and counting them here would publish one edge twice.
	IntraGroup int
}

// metricsGraph projects the cached structural index into a metrics.Graph.
//
// Returns the graph, a key -> node lookup for rendering, and what the
// projection dropped — those counts are published, because a metric computed
// over a filtered graph while reporting the unfiltered total would overstate
// how much of the code it looked at.
//
// ctx is honoured even though nothing here touches disk or network: the
// projection is O(symbols + edges) over a whole-repository index, and it is the
// same computation repocortex export cancels, so a reader who navigates away
// mid-request should not pay for a graph nobody will read.
func (idx *structuralIndex) metricsGraph(
	ctx context.Context, level MetricsLevel, floor model.Confidence,
) (metrics.Graph, map[string]metricsNode, metricsDropped, error) {
	switch level {
	case LevelFile:
		return idx.metricsGraphByPath(ctx, floor, func(f fileInfo) string { return f.Path }, model.KindFile)
	case LevelDirectory:
		return idx.metricsGraphByPath(ctx, floor, metricsDirOf, model.KindDirectory)
	default:
		return idx.metricsGraphBySymbol(ctx, floor)
	}
}

// metricsCancelStride is how often the projections poll ctx.
//
// The doc on metricsGraph says a reader who navigates away does not pay for a
// graph nobody will read, and until this existed the only poll was after the
// whole projection had already run — the sentence was true of the return value
// and false of the work. Polling per row would put a context mutex acquisition
// on every one of a half-million edges; a stride bounds abandoned work to at
// most this many rows while costing nothing measurable.
const metricsCancelStride = 64

// metricsCancelled polls ctx once per stride and names the stage in the error,
// so a cancelled projection says which of the two loops it was in.
func metricsCancelled(ctx context.Context, i int, stage string) error {
	if i%metricsCancelStride != 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", stage, err)
	}
	return nil
}

// metricsDirOf names a file's directory the way the graph explorer names it, so
// the root bucket does not appear as "." on one screen and "(repository root)"
// on the other. See containerFor, which is the other caller of that label.
func metricsDirOf(f fileInfo) string {
	d := path.Dir(f.Path)
	if d == "." || d == "/" || d == "" {
		return repositoryRootLabel
	}
	return d
}

// metricsGraphBySymbol keys one node per symbol.
//
// KEYS ARE NOT FQNs. repocortex export measured 1,178 symbols sharing a
// fully-qualified name on one 1,008-file repository. metrics.NewGraph keeps the
// first node for a key and drops the rest, so keying on FQN alone would not
// merely omit those symbols — it would re-parent their edges onto the survivor
// and assert relationships that do not exist.
//
// The disambiguation rule is NOT invented here. It is byte-for-byte the one
// internal/cli/export.go's buildExportGraph applies, and it is duplicated
// rather than shared only because that function is bound to model.Symbol rows
// and this one to the portal's compact index. A key printed on a centrality row
// must be the same string the exported artifact uses for the same symbol, or
// the two cannot be joined. Concretely: bare base key when it is unique;
// "<key>@<path>" when the collision is across files; "<key>@<path>#L<line>"
// when two symbols share both the base key AND the file; "#<n>" last, for rows
// the index cannot tell apart at all. Whether "@path" alone suffices is decided
// per (base key, path) — never by which symbol happened to be visited first,
// which would make one member of a tie keyless and the other line-qualified.
//
// THE FLOOR APPLIES TO SYMBOLS, NOT ONLY EDGES, and it is applied BEFORE the
// collision counting — again because export does. A below-floor symbol is not
// in the node set at all: it is not a symbol RepoCortex is confident exists, and
// export drops it, so keeping it here would make the screen count nodes the
// exported artifact does not contain. Order matters as much as the rule. Count
// collisions first and filter after, and a below-floor symbol still inflates
// its base key's collision count: at floor=high over a fixture where an
// out-of-floor twin shares "Floor", export emits the bare key `Floor` while a
// filter-last projection emits `Floor@a/one.go` — a key export never mints, on
// a node it cannot be joined to.
func (idx *structuralIndex) metricsGraphBySymbol(
	ctx context.Context, floor model.Confidence,
) (metrics.Graph, map[string]metricsNode, metricsDropped, error) {
	// Sorted ids so every loop below runs in a fixed sequence. metrics.Graph
	// guarantees byte-identical output for identical input, and map range order
	// must not be what decides which member of a tie gets the "#n" suffix.
	ids := make([]int64, 0, len(idx.syms))
	for id := range idx.syms {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// First pass: whether a base key needs disambiguating is a property of the
	// whole run, not of the symbol in hand.
	base := make(map[int64]string, len(ids))
	shared := make(map[string]int, len(ids))
	keyed := make([]int64, 0, len(ids))
	belowFloor := make(map[int64]bool)
	for i, id := range ids {
		if err := metricsCancelled(ctx, i, "keying symbols for the metrics graph"); err != nil {
			return metrics.Graph{}, nil, metricsDropped{}, err
		}
		s := idx.syms[id]
		if !s.Confidence.AtLeast(floor) {
			belowFloor[id] = true
			continue
		}
		k := metricsBaseKey(s, idx.files[s.FileID].Path)
		if k == "" {
			continue
		}
		base[id] = k
		shared[k]++
		keyed = append(keyed, id)
	}

	// A disambiguated key must not land on a base key some other symbol will
	// claim later, so the unshared ones are reserved before any is minted.
	taken := make(map[string]bool, len(keyed))
	for k, n := range shared {
		if n == 1 {
			taken[k] = true
		}
	}

	// Second pass counts, within each colliding base key, how many symbols share
	// both that key and a file. That count decides whether "@path" resolves the
	// collision or whether line is needed too.
	pathTies := make(map[string]map[string]int, len(shared))
	for _, id := range keyed {
		k := base[id]
		if shared[k] <= 1 {
			continue
		}
		byPath := pathTies[k]
		if byPath == nil {
			byPath = make(map[string]int)
			pathTies[k] = byPath
		}
		byPath[idx.files[idx.syms[id].FileID].Path]++
	}

	nodes := make([]metrics.Node, 0, len(keyed))
	keyOf := make(map[int64]string, len(keyed))
	lookup := make(map[string]metricsNode, len(keyed))
	for _, id := range keyed {
		s := idx.syms[id]
		p := idx.files[s.FileID].Path
		k := base[id]
		if shared[k] > 1 {
			want := fmt.Sprintf("%s@%s", k, p)
			if pathTies[k][p] > 1 {
				want = fmt.Sprintf("%s@%s#L%d", k, p, s.Line)
			}
			k = metricsUniqueKey(taken, want)
		}
		taken[k] = true
		keyOf[id] = k
		nodes = append(nodes, metrics.Node{Key: k, Kind: s.Kind})
		lookup[k] = metricsNode{
			Key: k, Label: s.Name, Path: p, Line: s.Line,
			Kind: s.Kind, Exported: s.Exported, SymbolID: id,
		}
	}

	edges := make([]metrics.Edge, 0, len(idx.edges))
	dropped := 0
	for i, e := range idx.edges {
		if err := metricsCancelled(ctx, i, "projecting edges for the metrics graph"); err != nil {
			return metrics.Graph{}, nil, metricsDropped{}, err
		}
		if !e.Confidence.AtLeast(floor) {
			dropped++
			continue
		}
		src, okS := keyOf[e.Src]
		dst, okD := keyOf[e.Dst]
		if !okS || !okD {
			// An edge orphaned because the floor took an endpoint was removed
			// BY the floor, and the published count says so. An edge orphaned
			// for any other reason (an unkeyable symbol, an id the index does
			// not hold) is not the floor's doing and is not counted as such.
			if belowFloor[e.Src] || belowFloor[e.Dst] {
				dropped++
			}
			continue
		}
		edges = append(edges, metrics.Edge{
			From: src, To: dst, Kind: e.Kind, Confidence: e.Confidence, Weight: 1,
		})
	}
	if err := ctx.Err(); err != nil {
		return metrics.Graph{}, nil, metricsDropped{}, fmt.Errorf("metrics graph by symbol: %w", err)
	}
	return metrics.NewGraph(nodes, edges), lookup, metricsDropped{BelowFloor: dropped}, nil
}

// metricsBaseKey is the undisambiguated key, matching export's exportKey: the
// FQN where the extractor produced one, model.DefaultFQN otherwise. The
// collision suffix is deliberately not applied here — whether a key collides is
// a property of the whole run, which only the caller can see.
func metricsBaseKey(s symInfo, filePath string) string {
	if s.FQN != "" {
		return s.FQN
	}
	if s.Name == "" {
		return ""
	}
	return model.DefaultFQN(filePath, s.Name)
}

// metricsUniqueKey returns want, or want with the lowest "#n" nobody holds.
//
// The last resort for rows the index cannot tell apart at all — the same name at
// the same line of the same file, which happens for config keys and SQL DDL
// parsed by more than one extractor. Dropping them is the defect this keying
// exists to remove, so they get a number instead.
func metricsUniqueKey(taken map[string]bool, want string) string {
	if !taken[want] {
		return want
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s#%d", want, n)
		if !taken[c] {
			return c
		}
	}
}

// metricsGraphByPath collapses symbols onto their file or directory.
//
// Several symbol edges between the same pair become ONE edge whose Weight is
// the count, so a file pair joined by 40 calls pulls harder on a centrality
// score than one joined by a single import — the same aggregation the graph
// explorer's container edges already use.
//
// The bundle's Confidence is its WEAKEST member, the rule aggregatedEdge
// already states: a bundle is no stronger than its worst member, and the
// metrics package filters on that field. Stamping a fixed tier would invent
// evidence — forty proven edges and one guess would both come out "medium",
// and the guess would then survive a high floor it never earned.
//
// The floor filters symbols here too, for the reason it does at symbol level:
// a file whose only symbols are below the floor is not a node the run is
// confident about, and the two levels must not disagree about which symbols
// exist.
func (idx *structuralIndex) metricsGraphByPath(
	ctx context.Context, floor model.Confidence,
	group func(fileInfo) string, kind model.EntityKind,
) (metrics.Graph, map[string]metricsNode, metricsDropped, error) {
	// Ranging the symbol map is safe here only because nothing in this loop
	// decides an ordering: it fills two maps, and the keys are sorted below
	// before any slice is built from them.
	groupOf := make(map[int64]string, len(idx.syms))
	belowFloor := make(map[int64]bool)
	keys := map[string]struct{}{}
	grouped := 0
	for id, s := range idx.syms {
		if err := metricsCancelled(ctx, grouped, "grouping symbols for the metrics graph"); err != nil {
			return metrics.Graph{}, nil, metricsDropped{}, err
		}
		grouped++
		if !s.Confidence.AtLeast(floor) {
			belowFloor[id] = true
			continue
		}
		g := group(idx.files[s.FileID])
		if g == "" {
			continue
		}
		groupOf[id] = g
		keys[g] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	nodes := make([]metrics.Node, 0, len(ordered))
	lookup := make(map[string]metricsNode, len(ordered))
	for _, k := range ordered {
		nodes = append(nodes, metrics.Node{Key: k, Kind: kind})
		lookup[k] = metricsNode{Key: k, Label: k, Path: k, Kind: kind}
	}

	type pair struct{ from, to string }
	type bundle struct {
		weight  int
		weakest model.Confidence
	}
	bundles := map[pair]*bundle{}
	order := make([]pair, 0, len(idx.edges))
	dropped := metricsDropped{}
	for i, e := range idx.edges {
		if err := metricsCancelled(ctx, i, "bundling edges for the metrics graph"); err != nil {
			return metrics.Graph{}, nil, metricsDropped{}, err
		}
		if !e.Confidence.AtLeast(floor) {
			dropped.BelowFloor++
			continue
		}
		f, okF := groupOf[e.Src]
		t, okT := groupOf[e.Dst]
		if !okF || !okT {
			if belowFloor[e.Src] || belowFloor[e.Dst] {
				dropped.BelowFloor++
			}
			continue
		}
		if f == t {
			// A relationship between two distinct symbols that this level
			// cannot express: both endpoints are the same node, and
			// metrics.NewGraph holds no self-loop, so the edge joins nothing
			// and every number here is computed without it. On one 11,356-edge
			// index that was 25.9% of the graph at file level and 49.2% at
			// directory level, none of it disclosed. It is counted so the
			// screen can say so and name level=symbol, where the edge is a
			// real pair again.
			//
			// A genuinely recursive edge is NOT counted here. It is already
			// published as a self-loop — a limit of the model at every level,
			// not of this one — and one edge under two disclosures would be
			// one edge reported twice.
			if e.Src != e.Dst {
				dropped.IntraGroup++
			}
			continue
		}
		p := pair{f, t}
		b := bundles[p]
		if b == nil {
			b = &bundle{weakest: e.Confidence}
			bundles[p] = b
			order = append(order, p)
		}
		b.weight++
		if e.Confidence.Rank() < b.weakest.Rank() {
			b.weakest = e.Confidence
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].from != order[j].from {
			return order[i].from < order[j].from
		}
		return order[i].to < order[j].to
	})
	edges := make([]metrics.Edge, 0, len(order))
	for _, p := range order {
		b := bundles[p]
		edges = append(edges, metrics.Edge{
			From: p.from, To: p.to, Kind: model.EdgeDependsOn,
			Confidence: b.weakest, Weight: b.weight,
		})
	}
	if err := ctx.Err(); err != nil {
		return metrics.Graph{}, nil, metricsDropped{}, fmt.Errorf("metrics graph by %s: %w", kind, err)
	}
	return metrics.NewGraph(nodes, edges), lookup, dropped, nil
}

// accessorRule and testRule are printed on the screen beside the tags they
// produce. A tag whose rule the reader cannot see is a judgement wearing the
// costume of a measurement.
const accessorRule = "name begins get/set/is/has followed by an upper-case letter, and the symbol is a method or function"

const testRule = "path matches RepoCortex's test-file conventions (capability.IsTestPath) — a naming convention, not proof"

// noiseTags annotates a row a reader may want to discount. It never removes it.
//
// Symbol-level centrality is dominated by accessors and framework hooks: a
// getter called from everywhere is genuinely central and genuinely uninteresting.
// The comparable tool this design was learned from published a top-10 "core
// abstractions" list that was mostly test factory functions. Tagging says so
// without deciding for the reader.
func noiseTags(n metricsNode) []string {
	var out []string
	if isAccessorName(n.Label) && (n.Kind == model.KindMethod || n.Kind == model.KindFunction) {
		out = append(out, "accessor")
	}
	if capability.IsTestPath(n.Path) {
		out = append(out, "test")
	}
	return out
}

// isAccessorName implements accessorRule. The upper-case requirement is what
// separates getUserDetails from gettysburg.
//
// This project indexes Java, Kotlin and C# as well as Go, and all three
// permit non-ASCII identifiers — getÜber is an ordinary accessor in any of
// them. Indexing name[len(p)] as a byte instead of decoding the rune after
// the prefix would silently withhold the tag for any such name, which the
// published accessorRule string never says it does. Decoding keeps the code
// answering exactly the question the rule states.
func isAccessorName(name string) bool {
	for _, p := range []string{"get", "set", "is", "has"} {
		if len(name) > len(p) && strings.HasPrefix(name, p) {
			r, _ := utf8.DecodeRuneInString(name[len(p):])
			if unicode.IsUpper(r) {
				return true
			}
		}
	}
	return false
}

// MetricsFilters are the reader's, and both default to off.
type MetricsFilters struct {
	// Callable ranks only function/method/class/interface.
	Callable bool `json:"callable"`
	// ExcludeTests drops symbols in test files.
	ExcludeTests bool `json:"exclude_tests"`
}

func metricsFiltersParam(r *http.Request) MetricsFilters {
	q := r.URL.Query()
	return MetricsFilters{
		Callable:     q.Get("kinds") == "callable",
		ExcludeTests: q.Get("exclude_tests") == "1",
	}
}

// callableKinds is the kind set "callable" keeps: the things that can appear in
// a call graph as something other than a leaf.
var callableKinds = map[model.EntityKind]bool{
	model.KindFunction: true, model.KindMethod: true,
	model.KindClass: true, model.KindInterface: true,
}

// clearRemedy names the filters that are actually set and no others.
//
// The sentence was fixed — "Clear the kinds and exclude_tests filters" — so a
// reader holding one of the two was told to clear a control they had never
// touched. That is the same defect the remedy walk was built for, one size
// down: a remedy naming a control that is not set costs the reader an attempt
// to find out it was never the reason. This is only ever reached with at least
// one filter set, because with both off keep() removes nothing.
func (f MetricsFilters) clearRemedy() string {
	switch {
	case f.Callable && f.ExcludeTests:
		return "Clear the kinds and exclude_tests filters."
	case f.Callable:
		return "Clear the kinds filter."
	default:
		return "Clear the exclude_tests filter."
	}
}

func (f MetricsFilters) keep(n metricsNode) bool {
	if f.Callable && !callableKinds[n.Kind] {
		return false
	}
	if f.ExcludeTests && capability.IsTestPath(n.Path) {
		return false
	}
	return true
}

// metricsTopN bounds every ranked list. A centrality ranking has a long, flat
// tail: past the first page the differences are smaller than the confidence of
// the edges they are computed from, so a longer list would imply a precision
// the input does not have.
const metricsTopN = 20

// MetricRow is one ranked node.
type MetricRow struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Path  string  `json:"path"`
	Line  int     `json:"line,omitempty"`
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`

	// FanIn and FanOut are carried on every row, not only the coupling
	// section, because a centrality score without them is unfalsifiable: a
	// reader cannot tell a hub from a bottleneck.
	FanIn  int `json:"fan_in"`
	FanOut int `json:"fan_out"`

	// Instability is Martin's I = FanOut/(FanIn+FanOut). Zero for an isolated
	// node rather than NaN.
	Instability float64 `json:"instability"`

	// Tags names what the index suspects about this row. Never hides it.
	Tags []string `json:"tags,omitempty"`

	SymbolID int64 `json:"symbol_id,omitempty"`
}

// CycleRow is one dependency cycle.
type CycleRow struct {
	Members []string `json:"members"`
	Length  int      `json:"length"`
}

// MetricsSection is one question, one measure, one action.
type MetricsSection struct {
	Title    string `json:"title"`
	Question string `json:"question"`

	// Measure names the algorithm, so a reader can tell which claim they are
	// reading and two sections cannot silently become the same list.
	Measure string `json:"measure"`

	// Caveat is what this measure does NOT say. Non-empty on every section.
	Caveat string `json:"caveat"`

	Rows   []MetricRow `json:"rows"`
	Cycles []CycleRow  `json:"cycles,omitempty"`

	// Considered is how many ranked candidates this section looked at. Rows is
	// what survived metricsTopN, the zero-skip and the reader's filters, and
	// once take has returned, the difference is unrecoverable — no later stage
	// can see a row that was never appended. So the figure is captured where
	// the removal happens, not where it is rendered.
	Considered int `json:"considered"`

	// Omissions accounts for every Considered candidate absent from Rows, by
	// cause, with the control that undoes each. It lives here, once, for the
	// reason the Omissions block states: a truncation stated nowhere reads as
	// a complete answer.
	Omissions Omissions `json:"omissions"`

	Next NextAction `json:"next"`
}

// zeroPolicy is what a section does with a zero score, and what it tells the
// reader a zero MEANT there.
//
// The wording travels with the decision because zero means something different
// per measure, and the two must not drift apart. It did drift: the betweenness
// section hid 11,086 of 11,100 rows on the live apm index under "a zero score
// is not hidden data" while the response-level sampled_betweenness omission on
// the same screen said the ranking came from 70 sources of 11,100. Under a
// 0.6% stride a zero is overwhelmingly an artefact of which sources were drawn,
// which is also why that section renders 14 rows against a 20-row budget.
type zeroPolicy struct {
	skip           bool
	detail, remedy string
}

// betweennessZero says what a zero betweenness means on THIS graph.
//
// Below the sample budget every source was used and a zero is a measurement:
// the node lies on no shortest path. Above it a zero is a statement about the
// sample, and the screen says so rather than presenting it as a reading. The
// rows are still not ranked — ranking a value the sample never measured a path
// through would publish the artefact instead of merely describing it — but the
// reader is told which of the two they are looking at.
func betweennessZero(bo metrics.BetweennessOptions, order int) zeroPolicy {
	if bo.SampleSize <= 0 || order <= bo.SampleSize {
		return zeroPolicy{
			skip: true,
			detail: "Rows scoring zero on this measure lie on no shortest path in this graph. Every " +
				"source node was used, so that is a measurement, not an absence.",
			remedy: "None — a zero score is not hidden data.",
		}
	}
	return zeroPolicy{
		skip: true,
		detail: fmt.Sprintf("Rows scoring zero here lay on no shortest path FROM ONE OF THE %d SAMPLED "+
			"SOURCE NODES of %d. That is a statement about the sample, not about the graph: under a "+
			"stride this coarse a zero is usually an artefact of which sources were drawn rather than "+
			"evidence the row is on no path at all.", bo.SampleSize, order),
		remedy: "None — the sample is what keeps this screen interactive, and ranking rows the sample " +
			"never measured a path through would publish that artefact as a reading.",
	}
}

// buildSections computes the three sections. Order is fixed: risk, structure,
// reading path.
//
// Three sections, three different measures, on purpose. Betweenness answers
// "what does every path cross", PageRank answers "what does everything point
// at", and cycles plus instability answer "what resists change" — three
// questions a reader asks separately. Ranking two of them by the same score
// would publish one leaderboard twice under two headings, which reads as
// corroboration and is not.
//
// Every section ends in a NextAction for the reason overview.go states: a
// number without a next step is wallpaper, and a centrality table is the
// easiest number in this portal to admire and never act on.
func buildSections(
	ctx context.Context, level MetricsLevel, g metrics.Graph, lookup map[string]metricsNode,
	f MetricsFilters, budget metrics.CycleBudget,
) ([3]MetricsSection, metrics.CycleReport, error) {
	var (
		out    [3]MetricsSection
		cycles metrics.CycleReport
	)

	coup, err := metrics.CouplingOf(ctx, g)
	if err != nil {
		return out, cycles, fmt.Errorf("coupling: %w", err)
	}
	byKey := make(map[string]metrics.Coupling, len(coup))
	for _, c := range coup {
		byKey[c.Key] = c
	}

	row := func(s metrics.Score) MetricRow {
		n := lookup[s.Key]
		c := byKey[s.Key]
		return MetricRow{
			Key: s.Key, Label: n.Label, Path: n.Path, Line: n.Line,
			Kind: string(n.Kind), Value: s.Value,
			FanIn: c.FanIn, FanOut: c.FanOut, Instability: c.Instability,
			Tags: noiseTags(n), SymbolID: n.SymbolID,
		}
	}
	// take preserves the order the metrics package returned. That order is part
	// of that package's contract (descending, ties broken on key); re-sorting
	// here would replace a guarantee with a second, unrelated one.
	//
	// zeroPolicy belongs to the CALLER, not to take, because zero means different
	// things to different measures. A betweenness or PageRank of 0 is a
	// non-answer — the node lies on no shortest path, nothing points at it — so
	// listing it pads the table. Instability 0.0 is the opposite: a real
	// reading, the maximally stable node, which is precisely what a section
	// asking "what resists change?" exists to name. Skipping it there deleted
	// every pure sink from the structure section while the reading-path section
	// on the same screen ranked one of them first.
	//
	// Nothing after take can see a candidate it did not append, so the loop runs
	// to the end of the ranking rather than breaking at the cap: the counts are
	// only recoverable here.
	//
	// scope is the SECTION's own restriction and is counted apart from the
	// reader's filters. One branch counting both filed the section's restriction
	// under the filters' label and handed the reader a remedy that could not
	// work: with both filters off, "clear the kinds and exclude_tests filters"
	// clears nothing, and on the apm index that sentence carried 7,948 rows at
	// symbol level and every row at the two path levels. A row a reader removed
	// and a row this section never ranks are different facts.
	take := func(scores []metrics.Score, scope func(metricsNode) bool, zero zeroPolicy) ([]MetricRow, int, Omissions) {
		rows := make([]MetricRow, 0, metricsTopN)
		considered, filtered, outOfScope, zeroed, capped := 0, 0, 0, 0, 0
		for _, s := range scores {
			n, ok := lookup[s.Key]
			if !ok {
				// A score for a key this screen cannot name is not a row the
				// reader could have been shown, so it was never a candidate.
				continue
			}
			considered++
			switch {
			case !f.keep(n):
				filtered++
			case scope != nil && !scope(n):
				outOfScope++
			case zero.skip && s.Value == 0:
				zeroed++
			case len(rows) >= metricsTopN:
				capped++
			default:
				rows = append(rows, row(s))
			}
		}
		var om Omissions
		om.Hide(ReasonRowBudget, capped,
			fmt.Sprintf("This listing shows the top %d of %d ranked rows.", metricsTopN, considered),
			rowBudgetRemedy)
		om.Hide(ReasonZeroScore, zeroed, zero.detail, zero.remedy)
		om.Hide(ReasonRowFilter, filtered,
			"Rows removed by the kind and test-file filters currently set.",
			f.clearRemedy())
		om.Hide(ReasonSectionScope, outOfScope,
			"Rows this section does not rank: it is restricted to symbols this index calls exported, "+
				"which is a restriction of the section and not a filter you set.",
			"Ask the same question at level=file or level=directory, where the nodes are files and "+
				"directories and no exported-only restriction applies.")
		return rows, considered, om
	}

	bo := metrics.DefaultBetweennessOptions()
	btw, err := metrics.Betweenness(ctx, g, bo)
	if err != nil {
		return out, cycles, fmt.Errorf("betweenness: %w", err)
	}
	out[0] = MetricsSection{
		Title:    "Where change is dangerous",
		Question: "What does every path cross?",
		Measure:  "betweenness",
		Caveat: "Betweenness counts shortest paths through a node. It measures position, " +
			"not importance — and it is SAMPLED here, so a rank near the boundary can " +
			"differ from a full computation.",
		Next: NextAction{
			Label: "Analyse the blast radius", Screen: "blast",
			Detail: "A chokepoint is where a change propagates furthest. Open the blast radius before editing one.",
		},
	}
	out[0].Rows, out[0].Considered, out[0].Omissions = take(btw, nil, betweennessZero(bo, g.Order()))

	comps, err := metrics.Components(ctx, g)
	if err != nil {
		return out, cycles, fmt.Errorf("components: %w", err)
	}
	cycles, err = metrics.Cycles(ctx, g, budget)
	if err != nil {
		return out, cycles, fmt.Errorf("cycles: %w", err)
	}
	// A node with no edge in either direction is not ranked here: Martin's
	// instability is FanOut/(FanIn+FanOut), and over a node with neither there
	// is nothing to divide — a printed 0.0 would read as "maximally stable",
	// which is the one thing an unconnected node has not been measured to be.
	//
	// It is COUNTED, though, and here rather than in take, because take can
	// only account for candidates it is handed: on the live apm index this skip
	// removed 7,291 symbols before the accounting saw them, so section 1
	// reported 3,809 considered while the sections either side of it reported
	// 11,100 over the same graph, with no omission anywhere to explain the gap.
	unstable := make([]metrics.Score, 0, len(coup))
	isolated := 0
	for _, c := range coup {
		if c.FanIn+c.FanOut == 0 {
			if _, ok := lookup[c.Key]; ok {
				isolated++
			}
			continue
		}
		unstable = append(unstable, metrics.Score{Key: c.Key, Value: c.Instability})
	}
	// This is the one ranking built here rather than returned by the metrics
	// package, so the total order is this function's job: instability alone
	// ties constantly (every sink is 0, every source is 1), and without the key
	// as tiebreak two equal rows would swap between runs.
	sort.Slice(unstable, func(i, j int) bool {
		if unstable[i].Value != unstable[j].Value {
			return unstable[i].Value > unstable[j].Value
		}
		return unstable[i].Key < unstable[j].Key
	})
	crows := make([]CycleRow, 0, len(cycles.Cycles))
	for _, c := range cycles.Cycles {
		crows = append(crows, CycleRow{Members: c, Length: len(c)})
	}
	out[1] = MetricsSection{
		Title:    "Where the architecture is weak",
		Question: "What resists change?",
		Measure:  "cycles+instability",
		Caveat:   structureCaveat(comps, crows, cycles, budget),
		Cycles:   crows,
		Next: NextAction{
			Label: "Check the architecture rules", Screen: "governance",
			Detail: "A cycle is only a violation against a declared component model. Governance says whether one is declared.",
		},
	}
	// skipZero is false here alone: instability 0.0 is the most stable node in
	// the graph, and unstable already excluded the isolated ones.
	out[1].Rows, out[1].Considered, out[1].Omissions = take(unstable, nil, zeroPolicy{})
	// Counted into Considered, not left beside it: Considered is what the
	// section's own omissions have to add up against, and a candidate removed
	// before the ranking is still a candidate this section did not show.
	out[1].Considered += isolated
	out[1].Omissions.Hide(ReasonIsolatedNode, isolated,
		fmt.Sprintf("%d node(s) have no relationship at this level in either direction, so instability "+
			"— a ratio over their edge counts — has nothing to divide for them. A printed 0.0 would read "+
			"as the most stable node in the graph, which is the one thing an unconnected node has not "+
			"been measured to be.", isolated),
		isolatedRemedy(level))

	pr, err := metrics.PageRank(ctx, g, metrics.DefaultPageRankOptions())
	if err != nil {
		return out, cycles, fmt.Errorf("pagerank: %w", err)
	}
	scope, caveat := readingPathScope(level)
	out[2] = MetricsSection{
		Title:    "What to read first",
		Question: "What does the rest of the code point at?",
		Measure:  "pagerank",
		Caveat:   caveat,
		Next: NextAction{
			Label: "Open it in the graph explorer", Screen: "graph",
			Detail: "Start from the most-pointed-at symbol and walk outwards.",
		},
	}
	out[2].Rows, out[2].Considered, out[2].Omissions = take(pr, scope, zeroPolicy{
		skip: true,
		detail: "Rows scoring zero on this measure answer its question with a non-answer: nothing in " +
			"the graph points at them, and PageRank looked at every node to say so.",
		remedy: "None — a zero score is not hidden data.",
	})

	return out, cycles, nil
}

// isolatedRemedy offers only what THIS level leaves the reader, the rule
// cycleBudgetRemedy already follows.
//
// Coarsening is a real remedy here and a measured one: on the live apm index
// 7,291 symbols have no relationship of their own, but the files they sit in
// mostly do — 333 files and 47 directories are isolated at those levels. A
// symbol the index records nothing about is still part of a file that is
// called. At the coarsest level there is nothing left to fold into, and saying
// so is the honest answer rather than naming a control that returns the same
// number.
func isolatedRemedy(level MetricsLevel) string {
	switch level {
	case LevelDirectory:
		return "Nothing coarser to ask for — this is the smallest graph this screen builds, and a " +
			"directory with no relationship here has none this index recorded at the standard of proof."
	case LevelFile:
		return "Ask the same question at level=directory, where a file with no relationship of its own " +
			"is folded into a directory that may have one."
	default:
		return "Ask the same question at level=file or level=directory, where a symbol with no " +
			"relationship of its own is folded into a file or directory that may have one."
	}
}

// structureCaveat states the relationship between two numbers computed by two
// different algorithms over two different objects: how many nodes sit in a
// strongly connected component, and the bounded enumeration printed underneath
// them. Printed adjacently without that said, an 8-node ring renders "8 node(s)
// sit in a cycle" above an EMPTY list, and the only available reading is that
// one of the two is broken.
//
// # Every clause here is derived from what the run measured
//
// The sentence used to assert, on every response with a non-empty component,
// that longer cycles "exist unreported" and that the list "does not account
// for" the component total. On the live apm index both were false: the two
// components hold two members each, so no cycle longer than two can exist, and
// the two listed cycles cover all four members. The screen was stating one fact
// in two vocabularies — MetricsLimits.Cycles says the same thing correctly —
// and the stronger vocabulary was the wrong one.
//
// So each clause is now a measurement:
//
//   - An elementary cycle lies inside one strongly connected component and
//     cannot be longer than that component. If the largest component is no
//     longer than MaxLen, the length bound provably removed nothing; only when
//     it is longer can a longer cycle exist, and then only MAY — a six-member
//     component whose every cycle is a two-cycle is an ordinary shape.
//   - Whether the list accounts for the total is a set comparison against the
//     members actually listed, not an assumption.
//   - The work budget is named when it bit. It did not exist when this sentence
//     was written, so a run stopped by MaxCycles or MaxSteps blamed MaxLen for
//     it while the response-level cycle_budget omission said otherwise.
func structureCaveat(
	comps []metrics.Component, listed []CycleRow, rep metrics.CycleReport, budget metrics.CycleBudget,
) string {
	const instability = "Instability is a ratio of edge counts, not a judgement about design."

	inCycle, largest := 0, 0
	inComponent := make(map[string]bool)
	for _, c := range comps {
		if len(c.Members) < 2 {
			continue
		}
		inCycle += len(c.Members)
		if len(c.Members) > largest {
			largest = len(c.Members)
		}
		for _, m := range c.Members {
			inComponent[m] = true
		}
	}
	if inCycle == 0 {
		return "No node here sits in a strongly connected component, so this graph holds no " +
			"dependency cycle at any length. " + instability
	}

	covered := make(map[string]bool, inCycle)
	for _, c := range listed {
		for _, m := range c.Members {
			if inComponent[m] {
				covered[m] = true
			}
		}
	}

	parts := []string{fmt.Sprintf("%d node(s) sit in a strongly connected component, so each is on a "+
		"cycle of some length.", inCycle)}
	if largest > budget.MaxLen {
		parts = append(parts, fmt.Sprintf("The %d cycle(s) listed below are only those of length %d or "+
			"less, and the largest component here holds %d node(s), so longer ones may exist unreported.",
			len(listed), budget.MaxLen, largest))
	} else {
		parts = append(parts, fmt.Sprintf("The %d cycle(s) listed below were bounded at length %d, which "+
			"bound nothing here: the largest component holds %d node(s), and no elementary cycle can be "+
			"longer than the component it lies in.", len(listed), budget.MaxLen, largest))
	}
	if rep.Truncated() {
		parts = append(parts, "The search also stopped at its work budget before it finished, so cycles "+
			"within that length are missing from the list too — see cycle_budget below.")
	}
	if missing := inCycle - len(covered); missing > 0 {
		parts = append(parts, fmt.Sprintf("The list does not account for %d of those %d node(s).", missing, inCycle))
	} else {
		parts = append(parts, fmt.Sprintf("The listed cycles account for all %d of them.", inCycle))
	}
	return strings.Join(append(parts, instability), " ")
}

// readingPathScope is the reading-path section's restriction AT THIS LEVEL, and
// the sentence that describes it.
//
// # Why the restriction is symbol-level only
//
// The section used to filter every level on metricsNode.Exported, a field only
// the symbol projection ever sets, so at file and directory level it ranked
// nothing at all: 0 rows of 963 files and of 197 directories on the apm index,
// under a caveat announcing a list of exported symbols. Two fixes were
// available — invent a meaning for an "exported directory", or stop asking a
// question that has no answer at that level. This is the second.
//
// Exported is a property of a declaration: a symbol is exported or it is not,
// because a compiler says so. A directory is not exported by anything; the
// nearest available meaning ("contains at least one exported symbol") holds for
// very nearly every directory in every repository, so it would filter almost
// nothing while implying a selection had been made. A caveat that names a
// restriction which removes nothing is the same defect in the other direction.
//
// So at a path level every node is ranked, and the caveat says which level the
// restriction belongs to rather than claiming it applied here.
func readingPathScope(level MetricsLevel) (func(metricsNode) bool, string) {
	const measure = "PageRank measures inbound attention, which is a description of the call graph " +
		"and not a claim that these are the best-designed parts."
	if level == LevelSymbol {
		return func(n metricsNode) bool { return n.Exported },
			"Restricted to symbols this index calls exported. " + measure
	}
	return nil, fmt.Sprintf("Every %s in the graph is ranked here. Exported is a property of a "+
		"declaration, not of a %s, so the exported-only restriction this section applies at "+
		"level=symbol is not applied at this level and no row was removed by it. %s",
		level, level, measure)
}

// rowBudgetRemedy is the only honest answer to "how do I see row 21".
//
// The obvious remedy — "narrow the ranking with a level or kind filter" — was
// measured and does not deliver. Narrowing moves candidates between the reasons
// they were dropped for rather than revealing any: on this package's own
// fixture, switching kinds=callable on left the betweenness section hiding the
// same 28 rows, because the kinds it removed had scored zero and were already
// accounted for under zero_score. A filter changes WHICH twenty rows fill the
// listing; it does not lift the cap, and metricsTopN says why the cap is there.
//
// So the remedy names the one place the complete order exists, and does not
// name a control on this screen. That it cannot then be verified from here is
// the honest position: promising a reader a control that changes the number is
// the defect this constant exists to remove.
const rowBudgetRemedy = "No control on this screen lifts the cap, and narrowing the ranking only " +
	"changes which rows fill it. The complete order is in the exported artifact: repocortex export."

// MetricsLimits states the five things no number on this screen can see.
//
// # Why these are fields and not only Omissions entries
//
// Omissions.Hide ignores a zero or negative count by design, which is right for
// a truncation — "0 rows were dropped" is noise. It is wrong for a limit of the
// MODEL. A repository with no direct recursion today would publish no self-loop
// entry, and a reader would conclude the screen accounts for recursion; it
// never does. So the limit is stated unconditionally here and SIZED in
// Omissions only when it actually removed something. The two do not disagree:
// this block says what the screen cannot see, Omissions says how much it cost
// on this run.
type MetricsLimits struct {
	Betweenness string `json:"betweenness"`
	Cycles      string `json:"cycles"`
	SelfLoops   string `json:"self_loops"`

	// IntraGroup is stated at every level, including symbol level where it
	// collapses nothing, for the reason this type's comment gives: a sentence
	// a reader sees only when it bites teaches them the screen has no such
	// limit on the runs where it is silent.
	IntraGroup string `json:"intra_group"`

	ConfidenceFloor string `json:"confidence_floor"`
}

// MetricsResponse is the screen's payload.
type MetricsResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Level   MetricsLevel     `json:"level"`
	Filters MetricsFilters   `json:"filters"`
	Floor   model.Confidence `json:"min_confidence"`

	// Rules are the tag definitions, published so a reader can check a tag
	// against the rule that produced it.
	Rules map[string]string `json:"tag_rules"`

	// Limits is absent, not empty, before there is an index: an unindexed run
	// has no graph for any of the five sentences to be true of.
	Limits *MetricsLimits `json:"limits,omitempty"`

	Sections []MetricsSection `json:"sections"`

	Omissions Omissions  `json:"omissions"`
	Help      *Guidance  `json:"guidance,omitempty"`
	Next      NextAction `json:"next,omitempty"`
}

// handleMetrics serves the centrality screen.
//
// The empty case returns 200 with guidance rather than an error, for the reason
// handleDeps does: "no index" is a state of this repository, not a failure of
// this request, and a 500 teaches a reader to distrust the portal instead of
// running the one command that fixes it.
//
// # Measured cost
//
// The whole metric suite runs per request, so its cost is this screen's cost.
// From BenchmarkBuildSections in metrics_bench_test.go, on an Apple M4 Pro
// (darwin/arm64, go1.25.13, -benchtime 5x), over a synthetic graph of uniform
// out-degree 5, re-measured after the cycle budget landed:
//
//	 12,000 nodes /  60,000 edges ->   0.52s   (was 0.90s)
//	100,000 nodes / 500,000 edges ->   1.92s   (was 11.51s)
//
// Broken down at 100k from the metrics package's own benchmarks: betweenness
// 1.03s, cycles 0.87s, components 0.15s, PageRank 0.09s. Cycles was 10.13s of
// the old 11.51s; it is now bounded by metrics.CycleBudget, which caps DFS
// steps. Sampled betweenness is the largest term again, which is the shape the
// screen's own caveat already describes.
//
// # The synthetic figures are a ceiling, not a forecast
//
// A real call graph is hub-heavy and mostly acyclic, and it is dramatically
// cheaper at the same size. Measured live against the apm index (11,100
// symbols, 8,000+ edges) through this endpoint, warm, on the same machine:
//
//	level=symbol     ->  38-50ms
//	level=file       ->   8-24ms
//	level=directory  ->   8-25ms
//
// Both numbers are honest and they measure different things — the synthetic one
// bounds what this screen can cost, the live one says what it does cost.
//
// # Where the bound lives, and why it is not here
//
// This handler used to refuse level=symbol above 25,000 nodes and answer at
// file level instead. That bound did not bound. metricsGraphBySymbol keys one
// node per symbol at or above the floor REGARDLESS of its edges, so the gated
// quantity was the symbol count while the costly quantity is edge density: a
// 20,000-node graph at out-degree 10 passed the limit and cost 26s in cycle
// enumeration alone, more than twice the 100k fixture that motivated the limit.
// Gating a cost on a variable it is independent of is not a bound.
//
// The bound is now metrics.CycleBudget, applied where the cost is, so every
// consumer of that package gets it and no request is answered at a level it did
// not ask for. What the budget refused is disclosed through ReasonCycleBudget.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	level := metricsLevelParam(r)
	filters := metricsFiltersParam(r)
	floor := confidenceParam(r, "min_confidence", DefaultFloor)

	resp := MetricsResponse{
		Meta: meta, Legend: Legend(),
		Level: level, Filters: filters, Floor: floor,
		Rules:    map[string]string{"accessor": accessorRule, "test": testRule},
		Sections: []MetricsSection{},
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label: "Index this repository", Command: "repocortex index",
			Detail: "Centrality is computed over the indexed call graph; there is nothing to measure yet.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	g, lookup, dropped, err := idx.metricsGraph(ctx, level, floor)
	if err != nil {
		s.fail(w, r, fmt.Errorf("metrics graph at %s level: %w", level, err))
		return
	}

	secs, cycles, err := buildSections(ctx, level, g, lookup, filters, s.cycleBudget)
	if err != nil {
		s.fail(w, r, fmt.Errorf("metrics sections at %s level: %w", level, err))
		return
	}
	resp.Sections = secs[:]

	// metrics.NewGraph drops self-loops, so direct recursion is invisible to
	// every number here. Counting them from the SOURCE edges is the only way to
	// say how much is missing — the projected graph no longer holds them.
	//
	// The SYMBOL has to clear the floor as well as the edge, and not merely for
	// tidiness: when the floor takes the symbol, the projection has already
	// counted that edge under BelowFloor, and metricsDropped's own doc forbids
	// publishing one edge under two reasons. It is the same disjointness
	// IntraGroup and SelfLoop already keep between themselves — a reader given
	// two counts over one population reads two populations, under two remedies
	// that contradict each other.
	selfLoops := 0
	for _, e := range idx.edges {
		if e.Src != e.Dst || !e.Confidence.AtLeast(floor) {
			continue
		}
		if s, ok := idx.syms[e.Src]; ok && !s.Confidence.AtLeast(floor) {
			continue
		}
		selfLoops++
	}

	bo := metrics.DefaultBetweennessOptions()
	order := g.Order()
	unsampled := 0
	if bo.SampleSize > 0 && bo.SampleSize < order {
		unsampled = order - bo.SampleSize
	}

	resp.Limits = &MetricsLimits{
		Betweenness:     betweennessLimit(bo, order),
		Cycles:          fmt.Sprintf("Cycles longer than %d nodes were not searched. Their absence from the list is not evidence that none exist.", s.cycleBudget.MaxLen),
		SelfLoops:       fmt.Sprintf("An edge from a symbol to itself is a self-loop, and the graph model does not hold one, so direct recursion is absent from every number on this screen. %d such edge(s), and the symbol at each end of them, are at or above the standard of proof (%s and better) in this run; one whose symbol the standard removed is counted below as hidden by it instead, never as both.", selfLoops, tierWord(floor)),
		IntraGroup:      intraGroupLimit(level, dropped.IntraGroup),
		ConfidenceFloor: fmt.Sprintf("%d edge(s) are below the standard of proof (%s and better) and were counted in no metric here.", dropped.BelowFloor, tierWord(floor)),
	}

	var om Omissions
	// Note, not Hide: how many cycles a complete walk would have found is
	// exactly what the budget stopped anyone from computing, and a count here
	// would be invented. This is the one entry on the screen that reports a COST
	// bound rather than a filter, and it is stated for the same reason the
	// length bound is — a list that was cut short and does not say so reads as
	// the complete set.
	if cycles.Truncated() {
		om.Note(ReasonCycleBudget,
			cycleBudgetLimit(cycles, s.cycleBudget),
			cycleBudgetRemedy(level))
	}
	om.Hide(ReasonBelowConfidenceFloor, dropped.BelowFloor,
		resp.Limits.ConfidenceFloor,
		"Lower min_confidence to include them.")
	// The package's own doc proves a small deterministic sample can INVERT two
	// ranks relative to an exact computation, so the screen says the ranking is
	// approximate rather than presenting it as a measurement.
	om.Hide(ReasonSampledBetweenness, unsampled,
		resp.Limits.Betweenness,
		"Nothing to do here — the sample is what keeps this screen interactive.")
	om.Hide(ReasonSelfLoop, selfLoops,
		resp.Limits.SelfLoops,
		"Read the symbol directly; this is a limit of the graph model, not of this index.")
	// Sized only where the collapse happened. At symbol level the count is 0,
	// Hide ignores it, and the limits sentence above still tells the reader
	// that this level collapses nothing — which is the honest pair.
	// Sending the reader to level=symbol is a remedy they can always take now:
	// nothing on this screen refuses a level any more.
	om.Hide(ReasonIntraGroup, dropped.IntraGroup,
		resp.Limits.IntraGroup,
		"Ask the same question at level=symbol, where these edges join two distinct nodes again.")
	// Note, not Hide: how many longer cycles exist is precisely what was not
	// computed, and a count here would be invented.
	om.Note(ReasonCycleLengthBound,
		resp.Limits.Cycles,
		"Nothing to do — the bound is what keeps enumeration finite.")

	resp.Omissions = om
	// The screen's closing action. It shipped as an empty label and an empty
	// detail on every indexed response, so the one place a reader lands after
	// three rankings rendered nothing at all. What this screen cannot give them
	// is the tail: it publishes the top 20 per measure, by design, and the
	// exported artifact carries every node and edge these numbers were computed
	// from, keyed identically so the two can be joined.
	resp.Next = NextAction{
		Label:   "Export the whole ranking",
		Command: "repocortex export",
		Detail: fmt.Sprintf("This screen ranks the top %d rows per measure. The exported artifact carries "+
			"every node and edge these numbers were computed from, under the same keys.", metricsTopN),
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// intraGroupLimit describes the collapse in the terms that apply to THIS level.
//
// Symbol level collapses nothing, and a sentence claiming it did would be a
// caveat about a thing that did not happen; saying nothing at all would leave a
// reader unable to tell which of the two levels they are on.
func intraGroupLimit(level MetricsLevel, count int) string {
	if level == LevelSymbol {
		return "This level keys one node per symbol, so no relationship between two distinct " +
			"symbols is collapsed inside a node. The file and directory levels do collapse them."
	}
	return fmt.Sprintf("This level keys one node per %s, so %d edge(s) between two distinct symbols "+
		"of the same %s were collapsed into a node and counted in no metric on this screen. They are "+
		"not weak or unproven edges — they are real relationships this level cannot express, and they "+
		"reappear at level=symbol.", level, count, level)
}

// cycleBudgetLimit says which bound stopped the enumeration, in its own terms.
//
// The two bounds fail differently and a reader acts on them differently. A
// count bound means the graph holds more cycles than a report can carry; a step
// bound means the search itself was too expensive to finish, so not even the
// size of what is missing is known. Collapsing both into "some cycles were
// omitted" would hide that difference.
func cycleBudgetLimit(r metrics.CycleReport, b metrics.CycleBudget) string {
	switch {
	case r.CycleBudgetHit:
		return fmt.Sprintf("Cycle enumeration stopped at its budget of %d cycle(s), so the %d listed are "+
			"the first found in the search's canonical order and not the complete set. How many more "+
			"exist was not computed.", b.MaxCycles, len(r.Cycles))
	case r.StepBudgetHit:
		return fmt.Sprintf("Cycle enumeration stopped after %d search step(s) — one step is one edge "+
			"examined — so the %d cycle(s) listed are not the complete set. This graph is cyclic enough "+
			"that enumerating it completely is not an interactive operation, and how many cycles it "+
			"holds was not computed.", r.Steps, len(r.Cycles))
	default:
		return ""
	}
}

// cycleBudgetRemedy offers only what THIS level leaves the reader.
//
// A remedy naming a control the reader cannot usefully operate is worse than
// none: it costs them an attempt and returns the same answer. Notably absent is
// "raise min_confidence" — on every index measured so far every symbol carries
// the same confidence (the apm index: 11,100 symbols, all `high`), so raising
// the floor either removes no symbol at all or removes every one of them.
//
// It no longer promises that the enumeration FINISHES at a coarser level. It
// cannot: MaxCycles and MaxSteps are level-independent counts, not proportions
// of the graph, and a densely cyclic repository exhausts them wherever it is
// asked — apm already lists 154 cycles at file level and 199 at directory
// level against a default cap of 2,000, and a repository an order of magnitude
// more tangled would hit that cap at every level. What coarsening reliably
// gives is a smaller projection, so that is what the sentence claims, and the
// remedy walk measures exactly that.
func cycleBudgetRemedy(level MetricsLevel) string {
	const bound = "The bound itself is what keeps this screen interactive, and these are counts of " +
		"cycles and of search steps rather than proportions of the graph, so a repository cyclic " +
		"enough can exhaust them at any level."
	switch level {
	case LevelDirectory:
		return "Nothing coarser to ask for — this is the smallest graph this screen builds. " + bound
	case LevelFile:
		// Naming level=file here, to a reader already at level=file, costs them
		// a request and returns the same answer. Only the levels BELOW the one
		// in hand are remedies.
		return "Ask the same question at level=directory, where the projection is smaller and the same " +
			"budget covers more of it. " + bound
	default:
		return "Ask the same question at level=file or level=directory, where the projection is smaller " +
			"and the same budget covers more of it. " + bound
	}
}

// betweennessLimit describes the sampling in the terms that apply to THIS
// graph. A graph at or under the sample budget was ranked exactly, and saying
// "sampled" there would be a caveat about a thing that did not happen; saying
// nothing would leave a reader unable to tell which of the two they have.
func betweennessLimit(bo metrics.BetweennessOptions, order int) string {
	if bo.SampleSize <= 0 || order <= bo.SampleSize {
		return fmt.Sprintf("Betweenness is sampled above %d source nodes. This graph has %d, so every "+
			"source was used and this ranking is complete.", bo.SampleSize, order)
	}
	return fmt.Sprintf("Betweenness is SAMPLED: it was computed from %d source node(s) of %d, seeded so "+
		"reruns agree. The sample strides deterministically over sorted keys rather than drawing at "+
		"random, so a sampled ranking can order two nodes differently from a full computation, not "+
		"merely blur the gap between them.", bo.SampleSize, order)
}
