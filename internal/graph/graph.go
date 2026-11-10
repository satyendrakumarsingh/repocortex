// Package graph answers "what is reachable from here" over the code knowledge
// graph, under bounds the caller sets and the results honestly declare.
//
// It is the substrate for impact analysis, callers/callees and the context
// engine. Three properties shape every decision in this package:
//
// # Bounded, and honest about it
//
// A traversal that quietly stops at a limit and returns a partial answer as if
// it were complete is worse than an error: an impact analysis built on it
// under-reports risk, and nothing downstream can tell. So every Result carries
// Truncated and a reason, and both are set whenever the answer is not the
// complete reachable set — including when the caller's own MaxDepth is what cut
// it short.
//
// # Confidence survives the walk
//
// Edges carry confidence (see model.Confidence). Expanding through a guess and
// then presenting a depth-3 result as fact compounds uncertainty into a claim
// nothing supports. Every Node therefore reports the MINIMUM confidence along
// the path that reached it, so a caller can tell "definitely reachable" from
// "possibly reachable" without re-deriving it.
//
// # One query per level, never one per node
//
// The performance requirement is not decoration: a 10,920-file tree yields
// ~94k symbols and hundreds of thousands of edges, and this package sits in
// front of an interactive agent. The traversal is therefore level-synchronous —
// each BFS frontier becomes a single indexed `src_id IN (...)` query (chunked
// to stay inside SQLite's variable limit), so a depth-3 walk costs 3 round
// trips, not one per node discovered. A node-at-a-time DFS over the same data
// is the obvious implementation and is two orders of magnitude slower.
//
// Memory is bounded by the result, not the graph: nothing here materialises the
// edge table, and rows are consumed through a callback as they are scanned.
//
// # Measured performance
//
// From bench_test.go on a 100k-symbol / 500k-edge fixture (Apple M-series,
// -benchtime 30x), p95 wall time for a depth-3 walk from 100 roots:
//
//	MaxNodes  1,000 ->   2.9ms
//	MaxNodes 10,000 ->  39.6ms
//	MaxNodes 50,000 ->  86.5ms  (28,193 nodes reached)
//
// The 500ms budget is met with an order of magnitude to spare at any node cap a
// caller should be using. It is NOT met by the one shape that ignores the
// bound: Direction=Both, depth 3, no filters, no effective node cap reaches
// 99,821 of the 100,000 symbols in 1.6s. That is a whole-graph load wearing a
// traversal's clothes, and the honest conclusion is that MaxNodes, not depth,
// is what keeps this interactive — which is why it defaults to a small value
// rather than to unlimited.
//
// # Hub pruning
//
// Query.HubDegreeLimit refuses to expand THROUGH nodes wider than a limit,
// which is the largest accuracy win available here and, incidentally, a
// performance one. On the same fixture, the depth-3 impact walk above:
//
//	unpruned          -> 81.4ms p50, 28,193 nodes
//	HubDegreeLimit 50 -> 35.8ms p50,  7,699 nodes
//
// It is OFF by default, and the default path issues no extra queries.
//
// Query.HubPercentile derives that limit from the run's own distribution
// instead, in one grouped and ranked pass over the edge table. That pass costs
// ~476ms on 500k edges — alone more than this package's whole latency budget —
// so it is memoised per (run, filters) on the Graph and a warm percentile walk
// costs the same 36.1ms as an explicit limit. A caller that constructs a fresh
// Graph per request pays the 476ms every time and should pass an explicit
// HubDegreeLimit.
//
// A Graph performs no database writes and is safe for concurrent use.
package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Graph is a read-only traversal facade over one index run's edges.
type Graph struct {
	db  *store.DB
	reg *registry.Registry

	// runID pins which run to read. Zero means "resolve the committed run on
	// every call", which is what keeps a long-lived Graph correct across an
	// index swap rather than quietly serving a run that has been retired.
	runID int64

	// afterLevel runs after each expanded BFS level. It exists so a test can
	// cancel a context mid-traversal deterministically instead of racing a
	// timer against a query, in the same spirit as registry's injectable clock.
	afterLevel func(depth int)

	// hubMu guards hubLimits. A Graph still performs no writes to the database
	// and is still safe for concurrent use; this is process-local memoisation,
	// not shared mutable state escaping the struct.
	hubMu sync.RWMutex

	// hubLimits memoises percentile-derived hub thresholds per run and filter
	// set. Measured reason: reducing a 500k-edge table to a ranked degree
	// distribution costs ~460ms, which alone exceeds this package's whole
	// latency budget and would make Query.HubPercentile unusable on the second
	// call as much as the first. A committed run's edges never change, so the
	// answer is a constant of (run, filters) and paying for it once is correct
	// rather than merely convenient. Keyed by run id, so a Graph that follows
	// the committed run across an index swap recomputes rather than serving a
	// retired run's distribution.
	hubLimits map[hubLimitKey]int

	// useCSR routes Traverse through an in-memory CSRGraph snapshot of the run.
	// Off by default: loading costs one pass over the edge table, which only
	// pays back for a long-lived process (MCP server, portal) that traverses
	// repeatedly. csrMu guards csr, which holds a single run's snapshot; a
	// committed run's edges never change, so it is keyed by run id and dropped
	// when the committed run moves on.
	useCSR bool
	csrMu  sync.Mutex
	csr    *CSRGraph
}

// Option configures a Graph at construction.
type Option func(*Graph)

// WithCSR makes Traverse answer from an in-memory CSR snapshot of the run,
// built lazily on first use. Intended for long-lived readers of committed
// runs; it is ignored for a Graph pinned with NewForRun, whose staging run
// may still be written to.
func WithCSR() Option {
	return func(g *Graph) { g.useCSR = true }
}

// New returns a Graph that reads whichever run is currently committed.
//
// Resolving the run per call rather than at construction is deliberate: a
// Graph held by a long-lived MCP server would otherwise keep answering from a
// run that CommitRun has already deleted out from under it.
func New(db *store.DB, opts ...Option) *Graph {
	g := &Graph{db: db, reg: registry.New(db)}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// csrFor returns the CSR snapshot for runID, loading it if the cached one
// belongs to a different run.
func (g *Graph) csrFor(ctx context.Context, runID int64) (*CSRGraph, error) {
	g.csrMu.Lock()
	defer g.csrMu.Unlock()
	if g.csr != nil && g.csr.runID == runID {
		return g.csr, nil
	}
	c, err := g.LoadCSR(ctx, runID)
	if err != nil {
		return nil, err
	}
	g.csr = c
	return c, nil
}

// NewForRun pins a Graph to a specific run, committed or not. Intended for
// doctor-style tooling that must inspect a staging run; query paths should use
// New so invariant 3 (readers see only committed runs) holds automatically.
func NewForRun(db *store.DB, runID int64) *Graph {
	return &Graph{db: db, reg: registry.New(db), runID: runID}
}

// run resolves the run id to query. registry.CurrentRun is the only sanctioned
// way to discover it, so the committed-run visibility gate is enforced in one
// place rather than re-implemented here.
func (g *Graph) run(ctx context.Context) (int64, error) {
	if g.runID != 0 {
		return g.runID, nil
	}
	cur, err := g.reg.CurrentRun(ctx)
	if err != nil {
		return 0, err
	}
	return cur.ID, nil
}

// Direction selects which way edges are followed.
type Direction int

const (
	// Outgoing follows edges away from the root: "what does this reach".
	Outgoing Direction = iota

	// Incoming follows edges into the root: "what reaches this". This is the
	// direction impact analysis needs — changing a symbol affects its callers,
	// not its callees.
	Incoming

	// Both treats the graph as undirected. Useful for blast-radius questions
	// where a shared dependency matters regardless of arrow direction.
	Both
)

func (d Direction) String() string {
	switch d {
	case Outgoing:
		return "outgoing"
	case Incoming:
		return "incoming"
	case Both:
		return "both"
	default:
		return fmt.Sprintf("Direction(%d)", int(d))
	}
}

// edgeDir is one physical scan direction. Direction expands to one or two of
// these, which matters because each maps onto a different index: outgoing hits
// idx_edges_src, incoming hits idx_edges_dst. Expressing Both as an OR in a
// single statement would defeat both indexes, so it is issued as two queries.
type edgeDir int

const (
	dirOut edgeDir = iota
	dirIn
)

func (d Direction) scans() ([]edgeDir, error) {
	switch d {
	case Outgoing:
		return []edgeDir{dirOut}, nil
	case Incoming:
		return []edgeDir{dirIn}, nil
	case Both:
		return []edgeDir{dirOut, dirIn}, nil
	default:
		return nil, fmt.Errorf("graph: invalid direction %d", int(d))
	}
}

// maxSQLVars bounds how many symbol ids go into one IN (...) clause.
//
// SQLITE_MAX_VARIABLE_NUMBER is a compile-time limit: 999 on builds predating
// SQLite 3.32 and 32766 since. Chunking well below 999 makes the traversal
// independent of which build it happens to be linked against, and the headroom
// covers the run id plus the kind and confidence filters that share the budget.
const maxSQLVars = 900

// hop is one traversed edge: which frontier node it left from, where it leads,
// and the provenance of that single step.
type hop struct {
	from int64
	to   int64
	kind model.EdgeKind
	conf model.Confidence
}

// errNodeCap unwinds the row-scanning callback when a caller's node budget is
// exhausted. It never escapes this package; it is a control signal, not a
// failure, and callers learn about the cap through Result.Truncated instead.
var errNodeCap = errors.New("graph: node cap reached")

// forEachHop expands an entire frontier with one query per chunk per direction.
//
// This function is the reason the package meets its latency budget. The naive
// alternative — a query per frontier node — turns a depth-3 traversal over a
// few thousand nodes into a few thousand round trips through the driver, and no
// amount of indexing recovers that.
//
// Rows are delivered to visit as they are scanned rather than collected into a
// slice, so peak memory tracks what the caller keeps, not the fan-out of the
// level being expanded.
func (g *Graph) forEachHop(
	ctx context.Context,
	runID int64,
	ids []int64,
	kinds []model.EdgeKind,
	minConf model.Confidence,
	dir edgeDir,
	visit func(hop) error,
) error {
	confs := allowedConfidences(minConf)

	for start := 0; start < len(ids); start += maxSQLVars {
		// Checked per chunk rather than per row: QueryContext already aborts an
		// in-flight statement on cancellation, and this covers the gap between
		// statements without adding a check to the hot scan loop.
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + maxSQLVars
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		args := make([]any, 0, 1+len(chunk)+len(kinds)+len(confs))
		args = append(args, runID)
		for _, id := range chunk {
			args = append(args, id)
		}
		for _, k := range kinds {
			args = append(args, string(k))
		}
		for _, c := range confs {
			args = append(args, string(c))
		}

		rows, err := g.db.QueryContext(ctx, hopQuery(dir, len(chunk), len(kinds), len(confs)), args...)
		if err != nil {
			return fmt.Errorf("graph: expand %d node(s): %w", len(chunk), err)
		}
		if err := scanHops(rows, visit); err != nil {
			return err
		}
	}
	return nil
}

// scanHops reads one result set into visit.
//
// kind and confidence are scanned as RawBytes and interned rather than scanned
// as strings. Scanning into a *string allocates a fresh string per column per
// row, and a wide traversal reads millions of rows for a bounded result — the
// exact shape of "memory proportional to the graph" this package promises not
// to have. Both columns are closed enums, so a map lookup keyed on the raw
// bytes (which the compiler performs without allocating) returns the interned
// constant instead.
func scanHops(rows *sql.Rows, visit func(hop) error) error {
	defer rows.Close()
	var (
		h          hop
		kind, conf sql.RawBytes
	)
	for rows.Next() {
		if err := rows.Scan(&h.from, &h.to, &kind, &conf); err != nil {
			return fmt.Errorf("graph: scan edge: %w", err)
		}
		h.kind = internKind(kind)
		h.conf = internConfidence(conf)
		if err := visit(h); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("graph: read edges: %w", err)
	}
	return nil
}

var (
	edgeKinds = map[string]model.EdgeKind{
		string(model.EdgeContains): model.EdgeContains, string(model.EdgeImports): model.EdgeImports,
		string(model.EdgeReferences): model.EdgeReferences, string(model.EdgeCalls): model.EdgeCalls,
		string(model.EdgeExtends): model.EdgeExtends, string(model.EdgeImplements): model.EdgeImplements,
		string(model.EdgeDependsOn): model.EdgeDependsOn, string(model.EdgeExposes): model.EdgeExposes,
		string(model.EdgeReads): model.EdgeReads, string(model.EdgeWrites): model.EdgeWrites,
		string(model.EdgePublishes): model.EdgePublishes, string(model.EdgeConsumes): model.EdgeConsumes,
		string(model.EdgeTestedBy): model.EdgeTestedBy, string(model.EdgeConfigures): model.EdgeConfigures,
		string(model.EdgeTracesTo): model.EdgeTracesTo, string(model.EdgeViolates): model.EdgeViolates,
		string(model.EdgeOwnedBy): model.EdgeOwnedBy, string(model.EdgeChangedBy): model.EdgeChangedBy,
	}
	confidences = map[string]model.Confidence{
		string(model.ConfidenceExact):  model.ConfidenceExact,
		string(model.ConfidenceHigh):   model.ConfidenceHigh,
		string(model.ConfidenceMedium): model.ConfidenceMedium,
		string(model.ConfidenceLow):    model.ConfidenceLow,
	}
)

// internKind maps raw column bytes to a shared constant. An unrecognised kind
// falls back to a copy rather than being dropped: edges.kind is a free-text
// column, and a traversal must not silently lose relationships written by a
// newer analyzer than this binary knows about.
func internKind(b []byte) model.EdgeKind {
	if k, ok := edgeKinds[string(b)]; ok {
		return k
	}
	return model.EdgeKind(b)
}

func internConfidence(b []byte) model.Confidence {
	if c, ok := confidences[string(b)]; ok {
		return c
	}
	return model.Confidence(b)
}

// hopQuery builds the per-chunk expansion statement.
//
// The column order is (frontier side, neighbour side) so the caller does not
// care which direction produced the row. No ORDER BY: sorting hundreds of
// thousands of edge rows to impose an order the traversal then discards would
// dominate the query cost. Determinism is restored on the result set, which is
// bounded, in Traverse.
//
// INDEXED BY is not paranoia, it is a measured fix. RepoCortex never runs ANALYZE,
// so SQLite has no sqlite_stat1 and has to guess selectivity. Given the choice
// between idx_edges_src (the frontier lookup) and idx_edges_conf, it guesses
// idx_edges_conf and turns each level into a scan of every edge of that
// confidence — on a 500k-edge fixture that made adding MinConfidence roughly
// 400x SLOWER than omitting it. Naming the index removes the guess. If a future
// migration renames these indexes this query fails loudly, which is the right
// outcome: the alternative is a silent return to full scans.
//
// Everything interpolated here is a placeholder count derived from constants;
// no caller-supplied text reaches the SQL text.
func hopQuery(dir edgeDir, nIDs, nKinds, nConfs int) string {
	var b strings.Builder
	b.Grow(180 + 2*(nIDs+nKinds+nConfs))
	if dir == dirOut {
		b.WriteString("SELECT src_id, dst_id, kind, confidence FROM edges INDEXED BY idx_edges_src" +
			" WHERE index_run_id = ? AND src_id IN (")
	} else {
		b.WriteString("SELECT dst_id, src_id, kind, confidence FROM edges INDEXED BY idx_edges_dst" +
			" WHERE index_run_id = ? AND dst_id IN (")
	}
	writePlaceholders(&b, nIDs)
	b.WriteString(")")
	if nKinds > 0 {
		b.WriteString(" AND kind IN (")
		writePlaceholders(&b, nKinds)
		b.WriteString(")")
	}
	if nConfs > 0 {
		b.WriteString(" AND confidence IN (")
		writePlaceholders(&b, nConfs)
		b.WriteString(")")
	}
	return b.String()
}

func writePlaceholders(b *strings.Builder, n int) {
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("?")
	}
}

// allowedConfidences returns the confidence values at or above minConf, or nil
// meaning "no filter".
//
// The filter is pushed into SQL rather than applied after scanning because a
// weak edge that is never read is a row that never crosses the driver boundary.
// In a heuristically resolved graph low-confidence edges routinely outnumber
// proven ones, so this is most of the rows, not a trim at the margin.
func allowedConfidences(minConf model.Confidence) []model.Confidence {
	// ConfidenceLow admits everything, so a filter would cost work and exclude
	// nothing. An unset or unrecognised value is treated the same way: the
	// caller did not ask for a threshold.
	if !minConf.Valid() || minConf == model.ConfidenceLow {
		return nil
	}
	all := []model.Confidence{
		model.ConfidenceExact, model.ConfidenceHigh,
		model.ConfidenceMedium, model.ConfidenceLow,
	}
	out := make([]model.Confidence, 0, len(all))
	for _, c := range all {
		if c.AtLeast(minConf) {
			out = append(out, c)
		}
	}
	return out
}

// weaker returns the lower-ranked of two confidences. Path confidence is the
// minimum over its edges: a chain is only as trustworthy as its weakest link,
// and reporting anything stronger would be the exact overclaim this package
// exists to prevent.
func weaker(a, b model.Confidence) model.Confidence {
	if b.Rank() < a.Rank() {
		return b
	}
	return a
}
