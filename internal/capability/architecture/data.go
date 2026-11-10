package architecture

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Sensitive-data reachability: which endpoints can reach a table classified as
// PII or PCI, and by what path.
//
// # Why the path is the finding
//
// "Endpoint X touches PCI data" is not actionable. Nobody can review it, nobody
// can fix it, and the first response is always "through what?". The answer this
// file produces is the route — endpoint → handler → service → repository →
// table — with a file and line on every hop, because the remediation is always
// on one specific hop and the reviewer has to be able to find it.
//
// This is also why reachability is a rule kind here rather than a report of its
// own. A direct write to a cardholder table and a four-hop route to the same
// table are the same regulatory fact, and only a check that follows edges can
// see the second one. An import linter cannot: the write happens inside a
// repository class the caller is entirely entitled to use.
//
// # Confidence is the MINIMUM along the path, never the average and never the end
//
// A path is only as believed as its least believed hop. A route that crosses
// one `low` heuristic edge — a call resolved by matching a method name in an
// unrelated file — is a LEAD: something worth an hour of a person's time, not
// something to tell an auditor. Reporting it at the confidence of its strongest
// hop would launder a guess into a compliance claim, which is the single
// fastest way to make a security team stop trusting this tool.
//
// The traversal is therefore run once per confidence level from strongest to
// weakest, and a table first reached at level L is reported with the confidence
// actually observed along the reported steps. That yields the STRONGEST
// available evidence for each (endpoint, table) pair rather than whichever route
// a breadth-first search happened to find first.
//
// # What is deliberately not traversed
//
// Only EXPOSES, CALLS, REFERENCES, READS and WRITES. Not CONTAINS — a file
// contains every symbol in it, so traversing it would make every endpoint in a
// file reach everything any of its neighbours touch, and the report would be
// uniformly true and uniformly useless. Not IMPORTS, which is module-level and
// says nothing about whether a code path exists. Not EXTENDS or IMPLEMENTS,
// which means a call dispatched through an interface to an implementation that
// reads a classified table is NOT found. That is an under-report, it is stated
// in DataCoverage.Caveats, and it is the right side to err on: this check
// invents no edges.

// Bounds on the traversal.
//
// These are cost limits, not judgements, and each one is reported when it
// binds — a silently truncated compliance answer is worse than a small one.
//
// maxDataHops is eight because a route longer than that is not a finding a
// person can act on, and because the reverse-reachability prune below uses the
// same bound to decide which nodes are worth visiting at all.
const (
	maxDataHops              = 8
	maxDataVisitsPerEndpoint = 20_000
	maxDataVisitsTotal       = 2_000_000
	maxDataPaths             = 5_000
)

// dataRanks are the confidence levels the traversal runs at, strongest first.
// The order is the whole mechanism: the first level at which a table becomes
// reachable is the best evidence that exists for that pair.
var dataRanks = []model.Confidence{
	model.ConfidenceExact,
	model.ConfidenceHigh,
	model.ConfidenceMedium,
	model.ConfidenceLow,
}

// PathStep is one hop of a reachability path: the edge that was followed and
// the symbol it arrived at.
//
// Every step carries its own confidence and its own source location. A reader
// who distrusts the finding needs to be able to check the exact hop they
// distrust, and a path that cites only its endpoints asks to be taken on faith.
type PathStep struct {
	Symbol     string `json:"symbol"`
	Component  string `json:"component,omitempty"`
	Kind       string `json:"kind"`       // edge kind followed to get here
	Confidence string `json:"confidence"` // confidence OF THIS HOP
	FilePath   string `json:"file_path,omitempty"`
	Line       int    `json:"line,omitempty"`
}

// DataPath is one endpoint's route to one classified table.
type DataPath struct {
	Endpoint          string `json:"endpoint"`
	EndpointComponent string `json:"endpoint_component,omitempty"`

	Table string `json:"table"`
	// Classifications is every label the table carries, sorted. A table is
	// routinely both `pii` and `pci`, and dropping one of them drops half of a
	// finding.
	Classifications []string `json:"classifications"`
	// Access is how the last hop touches the table: read or write. A write to
	// a cardholder table is a different regulatory fact from a read, so they
	// are reported as separate paths rather than merged.
	Access string `json:"access"`

	// Confidence is the MINIMUM confidence over Steps. See the package note:
	// a path is only as believed as its weakest hop.
	Confidence string `json:"confidence"`
	// Weakest indexes the step that set Confidence — the hop to verify first.
	Weakest int        `json:"weakest_step"`
	Steps   []PathStep `json:"steps"`
}

// Via names the intermediate symbols between the endpoint and the table, in
// order, which is the "via handler H → service S" half of the rendered finding.
func (p DataPath) Via() []string {
	if len(p.Steps) < 2 {
		return nil
	}
	out := make([]string, 0, len(p.Steps)-1)
	for _, s := range p.Steps[:len(p.Steps)-1] {
		out = append(out, s.Symbol)
	}
	return out
}

// WeakestHop returns the step that determined the path's confidence, and the
// name of the symbol it was reached FROM, so a reader can go straight to the
// one relationship the whole finding rests on.
func (p DataPath) WeakestHop() (step PathStep, from string, ok bool) {
	if p.Weakest < 0 || p.Weakest >= len(p.Steps) {
		return PathStep{}, "", false
	}
	from = p.Endpoint
	if p.Weakest > 0 {
		from = p.Steps[p.Weakest-1].Symbol
	}
	return p.Steps[p.Weakest], from, true
}

// String renders the path as the sentence a reviewer needs:
//
//	endpoint "GET /accounts/{id}" reaches pci table "card_tokens" (write) via
//	handle → AccountService.load [confidence medium; weakest hop CALLS
//	handle→AccountService.load at src/api/Controller.java:31]
//
// The confidence and the weakest hop are inside the sentence rather than in a
// column somewhere because this string is what gets pasted into a ticket, and
// the qualification has to travel with the claim.
func (p DataPath) String() string {
	var b strings.Builder
	class := strings.Join(p.Classifications, "+")
	if class == "" {
		class = "classified"
	}
	fmt.Fprintf(&b, "endpoint %q reaches %s table %q", p.Endpoint, class, p.Table)
	if p.Access != "" {
		fmt.Fprintf(&b, " (%s)", p.Access)
	}
	if via := p.Via(); len(via) > 0 {
		fmt.Fprintf(&b, " via %s", strings.Join(via, " → "))
	} else {
		b.WriteString(" directly")
	}
	fmt.Fprintf(&b, " [confidence %s", p.Confidence)
	if s, from, ok := p.WeakestHop(); ok {
		fmt.Fprintf(&b, "; weakest hop %s %s→%s", s.Kind, from, s.Symbol)
		if s.FilePath != "" {
			fmt.Fprintf(&b, " at %s:%d", s.FilePath, s.Line)
		}
	}
	b.WriteString("]")
	return b.String()
}

// DataViolation is one reachability path that a data rule forbids.
//
// It embeds the whole path rather than a summary of it. The path IS the
// finding; a violation that says only which endpoint and which table sends the
// reviewer back to the graph to work out the route for themselves.
type DataViolation struct {
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"`
	// Verdict distinguishes a rule someone wrote to stop exactly this
	// (`forbidden`) from an origin nobody thought to put on the allow-list
	// (`not_allowed`). They read very differently to whoever triages this.
	Verdict string `json:"verdict"`
	// Classification is the single label that triggered this rule. A table
	// carrying two labels governed by two rules produces two violations, each
	// citing the rule and the regulation it belongs to.
	Classification string   `json:"classification"`
	Path           DataPath `json:"path"`
}

func (v DataViolation) String() string {
	id := v.RuleID
	if id == "" {
		id = "(unnamed data rule)"
	}
	return fmt.Sprintf("%s (%s, %s): %s", id, v.Severity, v.Verdict, v.Path.String())
}

// DataCoverage records what the reachability check did NOT look at.
//
// This is not decoration. Every number here is a way the answer "no endpoint
// reaches cardholder data" can be wrong, and a compliance check that reports
// only its findings is claiming a completeness it does not have. The report
// already does this for dependency rules with Report.Unassigned; this is the
// same discipline for the data rules.
type DataCoverage struct {
	// ClassifiedTables and UnclassifiedTables split the indexed tables. A large
	// UnclassifiedTables is the most important number in this struct: those
	// tables were not checked at all, because nobody said what is in them.
	ClassifiedTables   int `json:"classified_tables"`
	UnclassifiedTables int `json:"unclassified_tables"`

	// EndpointsConsidered is how many endpoints the traversal started from.
	// Paths that do not start at an indexed endpoint — batch jobs, message
	// consumers, CLIs, migrations — are outside this rule kind entirely.
	EndpointsConsidered int `json:"endpoints_considered"`
	// UnassignedEndpoints counts endpoints that reach classified data but whose
	// file belongs to no declared component. No rule can name them, so their
	// paths are listed and not judged.
	UnassignedEndpoints int `json:"unassigned_endpoints"`

	// ConfidenceFloor is the caller's --min-confidence, and PathsBelowFloor is
	// how many real paths it hid. Applied to the path's MINIMUM confidence, not
	// to each edge: filtering edges first would silently reroute a path through
	// a longer, stronger-looking route and report a bottleneck that no single
	// relationship has.
	ConfidenceFloor string `json:"confidence_floor,omitempty"`
	PathsBelowFloor int    `json:"paths_below_confidence_floor"`

	// PathsOmitted and TruncatedTraversals are the cost limits binding.
	// TruncatedTraversals is the more serious of the two: those endpoints were
	// explored only partially, so their absence from the findings proves
	// nothing.
	PathsOmitted        int `json:"paths_omitted"`
	TruncatedTraversals int `json:"truncated_traversals"`
	MaxHops             int `json:"max_hops"`
}

// Caveats renders the gaps as lines a report can print under its findings.
//
// It lives here rather than in the CLI so that every renderer says the same
// thing: the sentence that qualifies a compliance claim is part of the claim.
func (c DataCoverage) Caveats() []string {
	var out []string
	if c.UnclassifiedTables > 0 {
		out = append(out, fmt.Sprintf(
			"%d of %d indexed tables carry no classification and were not checked; a table nobody labelled cannot produce a finding",
			c.UnclassifiedTables, c.UnclassifiedTables+c.ClassifiedTables))
	}
	if c.UnassignedEndpoints > 0 {
		out = append(out, fmt.Sprintf(
			"%d endpoints reaching classified data belong to no declared component, so no rule could judge them; their paths are listed, not judged",
			c.UnassignedEndpoints))
	}
	if c.PathsBelowFloor > 0 {
		out = append(out, fmt.Sprintf(
			"%d paths were hidden by --min-confidence %s; they exist, but every route to the table crosses a hop weaker than that",
			c.PathsBelowFloor, c.ConfidenceFloor))
	}
	if c.TruncatedTraversals > 0 {
		out = append(out, fmt.Sprintf(
			"%d endpoints hit the traversal budget and were explored only partially; their absence from the findings proves nothing",
			c.TruncatedTraversals))
	}
	if c.PathsOmitted > 0 {
		out = append(out, fmt.Sprintf(
			"%d further paths were found and not listed, over the %d-path report limit",
			c.PathsOmitted, maxDataPaths))
	}
	out = append(out, fmt.Sprintf(
		"only routes of at most %d hops over EXPOSES, CALLS, REFERENCES, READS and WRITES were followed; a call dispatched through an interface to an implementation that touches a classified table is not found",
		c.MaxHops))
	return out
}

// checkDataReachability fills the data half of the report.
//
// It is a no-op for a model that classifies no data — not a cheap traversal, no
// traversal at all and no graph built — so repositories that have not opted in
// pay nothing for this rule kind existing.
func checkDataReachability(
	ctx context.Context,
	rep *Report,
	m *arch.Model,
	info map[int64]symInfo,
	edges []model.Edge,
	minConfidence model.Confidence,
) error {
	rep.DataCoverage.MaxHops = maxDataHops
	rep.DataCoverage.ConfidenceFloor = string(minConfidence)
	if !m.DeclaresData() {
		return nil
	}

	classOf := make(map[int64][]string)
	var endpoints []int64
	for id, si := range info {
		switch si.kind {
		case model.KindTable:
			cls := m.ClassesOf(si.name)
			if len(cls) == 0 {
				rep.DataCoverage.UnclassifiedTables++
				continue
			}
			rep.DataCoverage.ClassifiedTables++
			classOf[id] = cls
		case model.KindEndpoint:
			endpoints = append(endpoints, id)
		}
	}
	// Endpoints are walked in ID order so that the budget, when it binds, binds
	// on the same endpoints every run. A report whose truncation point moves
	// between runs makes "did this get worse?" unanswerable.
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i] < endpoints[j] })
	rep.DataCoverage.EndpointsConsidered = len(endpoints)

	if len(classOf) == 0 || len(endpoints) == 0 {
		return nil
	}

	fwd, rev := buildDataGraph(info, edges)
	w := &dataWalker{
		rep:      rep,
		model:    m,
		info:     info,
		fwd:      fwd,
		classOf:  classOf,
		relevant: nodesReachingTables(rev, classOf),
		floor:    minConfidence,
		budget:   maxDataVisitsTotal,
	}

	for _, ep := range endpoints {
		// Cancellation is checked per endpoint rather than per edge: an
		// endpoint's traversal is already bounded, and checking a context once
		// per edge on a million-edge graph costs more than the work it guards.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("architecture: sensitive-data reachability: %w", err)
		}
		w.walkEndpoint(ep)
	}

	sortDataPaths(rep.DataPaths)
	sortDataViolations(rep.DataViolations)
	return nil
}

// outEdge is one traversable relationship, keeping the edge itself so a path
// step can cite its evidence.
type outEdge struct {
	dst  int64
	edge *model.Edge
}

// buildDataGraph builds the forward adjacency the traversal walks and the
// reverse adjacency the prune below needs.
//
// Only traversable kinds, and only edges whose both ends are known symbols: a
// dangling edge cannot be rendered as a step, and a step nobody can read is not
// evidence.
func buildDataGraph(info map[int64]symInfo, edges []model.Edge) (fwd map[int64][]outEdge, rev map[int64][]int64) {
	fwd = make(map[int64][]outEdge)
	rev = make(map[int64][]int64)
	for i := range edges {
		e := &edges[i]
		if !traversableForData(e.Kind) {
			continue
		}
		if _, ok := info[e.SrcID]; !ok {
			continue
		}
		if _, ok := info[e.DstID]; !ok {
			continue
		}
		fwd[e.SrcID] = append(fwd[e.SrcID], outEdge{dst: e.DstID, edge: e})
		rev[e.DstID] = append(rev[e.DstID], e.SrcID)
	}
	// Sorted so a breadth-first search that has two equally short routes always
	// reports the same one. Registry order is a query-plan detail, and a
	// finding whose cited file changes between runs looks like a regression.
	for src := range fwd {
		out := fwd[src]
		sort.Slice(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if a.dst != b.dst {
				return a.dst < b.dst
			}
			if a.edge.Kind != b.edge.Kind {
				return a.edge.Kind < b.edge.Kind
			}
			if a.edge.Evidence.FilePath != b.edge.Evidence.FilePath {
				return a.edge.Evidence.FilePath < b.edge.Evidence.FilePath
			}
			return a.edge.Evidence.Range.Start.Line < b.edge.Evidence.Range.Start.Line
		})
	}
	return fwd, rev
}

// traversableForData is the closed set of relationships that can carry a data
// path. See the package note for why CONTAINS, IMPORTS, EXTENDS and IMPLEMENTS
// are absent.
func traversableForData(k model.EdgeKind) bool {
	switch k {
	case model.EdgeExposes, model.EdgeCalls, model.EdgeReferences, model.EdgeReads, model.EdgeWrites:
		return true
	}
	return false
}

// nodesReachingTables is the prune that makes this affordable.
//
// One reverse breadth-first search from every classified table marks the only
// nodes that can possibly lie on a path to one. Everything else — the vast
// majority of a real repository — is then never enqueued by any endpoint's
// forward search, which turns a per-endpoint walk of the whole call graph into
// a walk of the data-access neighbourhood.
//
// The same maxDataHops bound is applied here, which is sound: a node whose
// shortest reverse distance to every table exceeds the hop cap cannot appear on
// a forward path within the cap either.
func nodesReachingTables(rev map[int64][]int64, classOf map[int64][]string) map[int64]bool {
	relevant := make(map[int64]bool, len(classOf))
	queue := make([]int64, 0, len(classOf))
	for id := range classOf {
		relevant[id] = true
		queue = append(queue, id)
	}
	// No sort: this computes a SET, so visit order changes nothing about the
	// result and nothing downstream reads it in order.
	for depth := 0; depth < maxDataHops && len(queue) > 0; depth++ {
		var next []int64
		for _, v := range queue {
			for _, u := range rev[v] {
				if relevant[u] {
					continue
				}
				relevant[u] = true
				next = append(next, u)
			}
		}
		queue = next
	}
	return relevant
}

// dataWalker holds one check's traversal state.
//
// A struct rather than a closure over eight variables because the budget and
// the report are mutated across calls and a reader has to be able to see where.
// It is used by one goroutine at a time and owns no lock; Check does not
// parallelise this walk.
type dataWalker struct {
	rep      *Report
	model    *arch.Model
	info     map[int64]symInfo
	fwd      map[int64][]outEdge
	classOf  map[int64][]string
	relevant map[int64]bool
	floor    model.Confidence
	budget   int
}

// crumb is a parent pointer, kept with the edge so the path can be rebuilt with
// its evidence intact.
type crumb struct {
	parent int64
	edge   *model.Edge
}

// walkEndpoint finds, for one endpoint, the strongest route to every classified
// table it can reach.
//
// Strongest, not shortest: the searches run from the highest confidence level
// down, and a (table, access) pair found at a level is never looked for again at
// a weaker one. So the reported path for each pair is the best evidence that
// exists, and its confidence is the honest minimum along it.
func (w *dataWalker) walkEndpoint(ep int64) {
	found := make(map[string]bool)
	epComp := w.info[ep].component
	var recorded, truncated bool

	for _, c := range dataRanks {
		w.search(ep, epComp, c.Rank(), found, &recorded, &truncated)
	}

	if truncated {
		w.rep.DataCoverage.TruncatedTraversals++
	}
	if recorded && epComp == "" {
		w.rep.DataCoverage.UnassignedEndpoints++
	}
}

// search is a breadth-first walk from one endpoint over edges of at least
// minRank confidence.
func (w *dataWalker) search(ep int64, epComp string, minRank int, found map[string]bool, recorded, truncated *bool) {
	visited := map[int64]int{ep: 0}
	prev := make(map[int64]crumb)
	queue := []int64{ep}

	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		hops := visited[u]
		if hops >= maxDataHops {
			continue
		}
		for _, oe := range w.fwd[u] {
			if oe.edge.Confidence.Rank() < minRank {
				continue
			}
			if cls, ok := w.classOf[oe.dst]; ok {
				key := fmt.Sprintf("%d\x00%s", oe.dst, oe.edge.Kind)
				if found[key] {
					continue
				}
				found[key] = true
				w.record(ep, epComp, oe, cls, prev, recorded)
				// A table is a destination, never a waypoint. Routing a path
				// "through" a table would claim a data flow the graph never
				// recorded.
				continue
			}
			if !w.relevant[oe.dst] || w.info[oe.dst].kind == model.KindTable {
				continue
			}
			if _, seen := visited[oe.dst]; seen {
				continue
			}
			if w.budget <= 0 || len(visited) >= maxDataVisitsPerEndpoint {
				*truncated = true
				return
			}
			w.budget--
			visited[oe.dst] = hops + 1
			prev[oe.dst] = crumb{parent: u, edge: oe.edge}
			queue = append(queue, oe.dst)
		}
	}
}

// record turns a discovered route into a path, applies the confidence floor,
// and evaluates the data rules against it.
func (w *dataWalker) record(ep int64, epComp string, term outEdge, cls []string, prev map[int64]crumb, recorded *bool) {
	steps := w.buildSteps(ep, term, prev)
	conf, weakest := weakestOf(steps)

	// The floor is applied to the path's minimum, and the path recorded here is
	// the strongest one that exists for this pair — so if this one is below the
	// floor, every route to this table is.
	if w.floor != "" && !conf.AtLeast(w.floor) {
		w.rep.DataCoverage.PathsBelowFloor++
		return
	}
	if len(w.rep.DataPaths) >= maxDataPaths {
		w.rep.DataCoverage.PathsOmitted++
		return
	}

	p := DataPath{
		Endpoint:          w.info[ep].name,
		EndpointComponent: epComp,
		Table:             w.info[term.dst].name,
		Classifications:   append([]string(nil), cls...),
		Access:            accessOf(term.edge.Kind),
		Confidence:        string(conf),
		Weakest:           weakest,
		Steps:             steps,
	}
	w.rep.DataPaths = append(w.rep.DataPaths, p)
	*recorded = true

	for _, c := range cls {
		verdict, rule := w.model.EvaluateData(c, epComp)
		if verdict == arch.VerdictAllowed || rule == nil {
			continue
		}
		w.rep.DataViolations = append(w.rep.DataViolations, DataViolation{
			RuleID:         rule.ID,
			Severity:       rule.SeverityOrDefault(),
			Verdict:        string(verdict),
			Classification: c,
			Path:           p,
		})
	}
}

// buildSteps rebuilds the route from the parent pointers, forwards, ending at
// the table.
func (w *dataWalker) buildSteps(ep int64, term outEdge, prev map[int64]crumb) []PathStep {
	var back []PathStep
	for cur := term.edge.SrcID; cur != ep; {
		c, ok := prev[cur]
		if !ok {
			// Only reachable if the endpoint IS the source, which the loop
			// condition already excludes; defensive so a malformed graph
			// truncates a path rather than looping forever.
			break
		}
		back = append(back, w.step(cur, c.edge))
		cur = c.parent
	}
	steps := make([]PathStep, 0, len(back)+1)
	for i := len(back) - 1; i >= 0; i-- {
		steps = append(steps, back[i])
	}
	return append(steps, w.step(term.dst, term.edge))
}

func (w *dataWalker) step(id int64, e *model.Edge) PathStep {
	si := w.info[id]
	return PathStep{
		Symbol:     si.name,
		Component:  si.component,
		Kind:       string(e.Kind),
		Confidence: string(e.Confidence),
		FilePath:   e.Evidence.FilePath,
		Line:       e.Evidence.Range.Start.Line,
	}
}

// weakestOf is the house rule made executable: a path's confidence is the
// minimum over its hops, and the index of that hop is reported so the reader
// knows which single relationship the finding rests on.
func weakestOf(steps []PathStep) (model.Confidence, int) {
	worst, idx := model.Confidence(""), 0
	for i, s := range steps {
		c := model.Confidence(s.Confidence)
		if i == 0 || c.Rank() < worst.Rank() {
			worst, idx = c, i
		}
	}
	return worst, idx
}

// accessOf names what the last hop does to the table. Anything other than a
// read or a write is reported under its own edge kind rather than being folded
// into one of them, because a rule about writes must not fire on something that
// merely mentions the table.
func accessOf(k model.EdgeKind) string {
	switch k {
	case model.EdgeReads:
		return "read"
	case model.EdgeWrites:
		return "write"
	}
	return strings.ToLower(string(k))
}

func sortDataPaths(ps []DataPath) {
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.Endpoint != b.Endpoint {
			return a.Endpoint < b.Endpoint
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		return a.Access < b.Access
	})
}

// sortDataViolations puts the worst first and is total, so this report can sit
// in CI without its diff reordering between runs.
func sortDataViolations(vs []DataViolation) {
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	sort.Slice(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		if ra, rb := rank[a.Severity], rank[b.Severity]; ra != rb {
			return ra < rb
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.Classification != b.Classification {
			return a.Classification < b.Classification
		}
		if a.Path.Endpoint != b.Path.Endpoint {
			return a.Path.Endpoint < b.Path.Endpoint
		}
		if a.Path.Table != b.Path.Table {
			return a.Path.Table < b.Path.Table
		}
		return a.Path.Access < b.Path.Access
	})
}
