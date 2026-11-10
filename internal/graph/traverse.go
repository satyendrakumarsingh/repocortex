package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Default bounds applied when a Query leaves them unset.
//
// The defaults are deliberately small. An unbounded traversal over a real
// repository's edge table is not a slow query, it is an out-of-memory
// condition, and a caller that forgot to set a limit should get a usable answer
// with Truncated set rather than take the process down.
const (
	DefaultMaxDepth = 3
	DefaultMaxNodes = 1000
)

// Query describes one bounded traversal.
type Query struct {
	// Roots are the symbols to start from. They are returned as depth-0 nodes.
	// Duplicates and unknown ids are harmless.
	Roots []int64

	// Kinds restricts which edge kinds may be traversed. Empty means all.
	Kinds []model.EdgeKind

	// MinConfidence excludes weaker edges from expansion entirely, so they
	// neither appear in the result nor contribute paths through themselves.
	// Zero value means no threshold.
	MinConfidence model.Confidence

	// MaxDepth bounds hops from the nearest root. <= 0 uses DefaultMaxDepth.
	MaxDepth int

	// MaxNodes is a hard cap on returned nodes, roots included. <= 0 uses
	// DefaultMaxNodes. Hitting it sets Truncated.
	MaxNodes int

	// Direction selects which way edges are followed.
	Direction Direction

	// HubDegreeLimit stops the walk from expanding THROUGH a node whose degree
	// exceeds it. The node is still reported — it is reachable and the caller
	// should see it — but its neighbours are not explored.
	//
	// This is the cheapest accuracy win available here. A depth-3 walk that
	// crosses a logging helper reaches most of the repository and returns an
	// answer that is technically correct and practically useless. Cutting the
	// traversal beats cutting the response, because by the time the response is
	// cut the walk has already been paid for.
	//
	// Zero disables pruning, and disabling it is the default: an existing
	// caller's results must not change under it. Roots are never pruned — a
	// caller asking about a hub must still get its neighbours.
	HubDegreeLimit int

	// HubPercentile derives HubDegreeLimit from the run's own degree
	// distribution when HubDegreeLimit is zero — 0.99 means "wider than 99% of
	// nodes in this index". A fixed limit does not transfer between a
	// 5k-symbol service and a 500k-symbol monolith; a percentile does.
	//
	// The derived value is floored at minHubDegree, because the widest node in
	// a narrow graph is not a hub. Values outside (0,1] are an error rather
	// than a silent clamp: a caller who wrote 99 meaning 99% has a bug, and
	// quietly pruning at the maximum degree would hide it.
	HubPercentile float64
}

// Node is one reachable symbol.
//
// It deliberately carries no evidence. A node is usually reachable by many
// edges, so there is no single location that justifies it; use Neighbours or
// Paths when the caller needs to show its work.
type Node struct {
	SymbolID int64 `json:"symbol_id"`

	// Depth is hops from the nearest root. Roots are 0.
	Depth int `json:"depth"`

	// Via is the edge kind of the final hop that reached this node. Empty for
	// roots.
	Via model.EdgeKind `json:"via,omitempty"`

	// Confidence is the MINIMUM confidence along the path that reached this
	// node — how strongly the reachability itself is believed, not how strongly
	// any one edge is. Roots are exact because a root is an input, not an
	// inference.
	Confidence model.Confidence `json:"confidence"`

	// cost is the accumulated EdgeCost of the path recorded for this node — the
	// same route that determined Via and Confidence above. It is not exported:
	// it exists only to order Result.Nodes so that a chain of proofs outranks a
	// shorter chain of guesses, not to be presented to callers as a score.
	//
	// KNOWN LIMITATION: this only reorders the RESULT, not the search. Traverse
	// still expands the BFS frontier unweighted, level by level, and only sorts
	// by cost once the walk (or MaxNodes) stops it — see finish(). Under
	// MaxNodes truncation the two are not equivalent: BFS discovers nodes in
	// hop order, so a low-confidence node reached in 2 hops is already in
	// res.Nodes and counted against MaxNodes before a high-confidence node 3
	// hops away is ever visited. If the cap is hit first, the shallow guess
	// survives in the result and the deeper proof is never discovered at all —
	// sorting what was kept cannot recover a node that BFS never reached. This
	// is exactly the failure cost.go's package comment says weighting exists to
	// fix; today it fixes the fixed-size case (ties among nodes all found
	// within budget) but not the truncated case.
	cost int
}

// Cost is the accumulated EdgeCost of the path recorded for this node.
//
// It is exposed as a read-only accessor, not a field, because it is not a
// score to present to a reader: the units are arbitrary and deliberately
// uncalibrated (see cost.go). It exists so a caller that re-groups or merges
// Nodes — impact's diff path merges one traversal per changed symbol — can
// reproduce Result.finish's ranking instead of inventing a second one. A
// caller that only consumes Nodes in the order given never needs it.
func (n Node) Cost() int { return n.cost }

// Result is a traversal's answer plus an honest account of its completeness.
type Result struct {
	// Nodes is ordered by (Depth, SymbolID) so output is stable across runs.
	Nodes []Node `json:"nodes"`

	// Truncated reports that the reachable set is larger than what Nodes
	// contains. It is set for the caller's own MaxDepth as well as for
	// MaxNodes: a bound the caller chose still produces an incomplete answer,
	// and anything computing a risk score from this needs to know.
	//
	// TruncatedReason states every bound that fired, joined by "; ". That
	// separator is RESERVED: it separates independent reasons and appears
	// nowhere inside one. A caller that aggregates many walks — impact's diff
	// path merges one per changed symbol — splits on it to count each fact
	// once, and a "; " inside a single sentence would split that sentence in
	// half and report the two halves as separate facts with different counts.
	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncated_reason,omitempty"`

	// PrunedHubs names every node whose expansion was refused by
	// Query.HubDegreeLimit, ordered by (Depth, SymbolID) so runs agree. A
	// traversal that quietly declined to explore somewhere is the same failure
	// as one that quietly hit a limit: the caller cannot tell a complete answer
	// from a bounded one. Empty unless hub pruning was asked for.
	PrunedHubs []PrunedHub `json:"pruned_hubs,omitempty"`
}

// Traverse walks the graph outward from q.Roots within the bounds in q.
//
// Semantics worth knowing before relying on the output:
//
//   - Each node is visited once, at its SHALLOWEST depth. Cycles therefore
//     terminate and diamonds are not double-counted.
//
//   - Node.Confidence is the strongest path-minimum among the shortest paths to
//     that node. A longer path built from stronger edges is not preferred, so a
//     node's reported confidence can be weaker than the best that exists in the
//     graph. That bias is intentional and one-directional: this package will
//     under-claim certainty, never over-claim it.
func (g *Graph) Traverse(ctx context.Context, q Query) (Result, error) {
	scans, err := q.Direction.scans()
	if err != nil {
		return Result{}, err
	}
	if q.MaxDepth <= 0 {
		q.MaxDepth = DefaultMaxDepth
	}
	if q.MaxNodes <= 0 {
		q.MaxNodes = DefaultMaxNodes
	}
	if len(q.Roots) == 0 {
		return Result{}, nil
	}

	runID, err := g.run(ctx)
	if err != nil {
		return Result{}, err
	}

	if g.useCSR && g.runID == 0 && g.afterLevel == nil {
		c, err := g.csrFor(ctx, runID)
		if err != nil {
			return Result{}, err
		}
		return c.Traverse(ctx, q)
	}

	// Resolved once per traversal, and only when asked for: hubLimit returns
	// immediately without touching the database unless HubDegreeLimit or
	// HubPercentile is set, so the default path issues exactly the queries it
	// always did and the published benchmark still describes it.
	hubLimit, err := g.hubLimit(ctx, runID, q, scans)
	if err != nil {
		return Result{}, err
	}

	var res Result
	// index maps symbol id to its position in res.Nodes. It doubles as the
	// visited set, which is what makes cycles terminate: a node already present
	// is never queued again.
	index := make(map[int64]int, len(q.Roots)*8)
	frontier := make([]int64, 0, len(q.Roots))
	// Seeds are exempt from pruning, so they have to stay identifiable after
	// they leave the frontier: a root can be rediscovered at depth > 0 through
	// a cycle, and pruning it then would still withhold what was asked for.
	roots := make(map[int64]struct{}, len(q.Roots))

	for _, root := range q.Roots {
		if _, ok := index[root]; ok {
			continue
		}
		if len(res.Nodes) >= q.MaxNodes {
			res.truncate(fmt.Sprintf(
				"node limit %d reached while seeding roots, and %d root(s) were dropped and nothing was expanded",
				q.MaxNodes, len(q.Roots)-len(res.Nodes)))
			return res.finish(), nil
		}
		index[root] = len(res.Nodes)
		roots[root] = struct{}{}
		res.Nodes = append(res.Nodes, Node{
			SymbolID: root,
			Depth:    0,
			// A root was named by the caller, so its presence is not an
			// inference and must not dilute the path minimum of its descendants.
			Confidence: model.ConfidenceExact,
			// A root is not reached through any edge, so it costs nothing.
			cost: 0,
		})
		frontier = append(frontier, root)
	}

	for depth := 1; depth <= q.MaxDepth && len(frontier) > 0; depth++ {
		if hubLimit > 0 {
			// Filtering the frontier — rather than testing each node as an
			// edge reaches it — is what keeps this level-synchronous: the whole
			// level's degrees come back in one grouped query per direction.
			keep, hubs, err := g.partitionHubs(ctx, runID, frontier, roots, depth, hubLimit, q, scans)
			if err != nil {
				return Result{}, err
			}
			res.PrunedHubs = append(res.PrunedHubs, hubs...)
			frontier = keep
			if len(frontier) == 0 {
				// Every node at this level was a hub. Nothing is left to
				// expand, and noteHubPruning below records why.
				break
			}
		}

		var next []int64

		visit := func(h hop) error {
			// The source's own path confidence is read fresh each time because
			// it may have been upgraded earlier in this same level.
			parent := &res.Nodes[index[h.from]]
			pathConf := weaker(parent.Confidence, h.conf)
			pathCost := parent.cost + EdgeCost(h.conf)

			if at, ok := index[h.to]; ok {
				// Already reached. Only an alternative route at the SAME depth
				// may improve the recorded provenance; a deeper route never
				// overrides a shallower one, which is what keeps the walk
				// finite in a cyclic graph.
				n := &res.Nodes[at]
				if n.Depth == depth && betterVia(pathConf, pathCost, h.kind, n.Confidence, n.cost, n.Via) {
					n.Confidence, n.Via, n.cost = pathConf, h.kind, pathCost
				}
				return nil
			}
			if len(res.Nodes) >= q.MaxNodes {
				return errNodeCap
			}
			index[h.to] = len(res.Nodes)
			res.Nodes = append(res.Nodes, Node{
				SymbolID:   h.to,
				Depth:      depth,
				Via:        h.kind,
				Confidence: pathConf,
				cost:       pathCost,
			})
			next = append(next, h.to)
			return nil
		}

		for _, dir := range scans {
			err := g.forEachHop(ctx, runID, frontier, q.Kinds, q.MinConfidence, dir, visit)
			if errors.Is(err, errNodeCap) {
				res.truncate(fmt.Sprintf(
					"node limit %d reached at depth %d, and the reachable set is larger than this result",
					q.MaxNodes, depth))
				res.noteHubPruning(hubLimit)
				return res.finish(), nil
			}
			if err != nil {
				return Result{}, err
			}
		}

		if g.afterLevel != nil {
			g.afterLevel(depth)
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		frontier = next
	}

	// Nodes discovered at the depth limit were never expanded, so whatever lies
	// beyond them is missing from this answer. Saying so is the difference
	// between "nothing else is reachable" and "we stopped looking".
	if len(frontier) > 0 {
		res.truncate(fmt.Sprintf(
			"depth limit %d reached, and %d node(s) at the limit were not expanded",
			q.MaxDepth, len(frontier)))
	}
	// A walk can be incomplete for both reasons at once, so this appends rather
	// than replaces; see noteHubPruning.
	res.noteHubPruning(hubLimit)
	return res.finish(), nil
}

func (r *Result) truncate(reason string) {
	r.Truncated = true
	r.TruncatedReason = reason
}

// finish imposes a stable order. Discovery order is already grouped by depth,
// but within a level it follows whatever order SQLite returned rows in, which
// is an implementation detail callers must not depend on. Sorting costs
// O(result log result) — bounded by MaxNodes, never by graph size.
func (r Result) finish() Result {
	sort.Slice(r.Nodes, func(i, j int) bool {
		// Accumulated cost is the primary key: a node reached only through a
		// chain of guesses must rank behind one reached through proofs, even
		// when the guesses got there in fewer hops. An exact edge costs the
		// same as a plain hop (see EdgeCost), so for an all-exact walk cost
		// equals Depth and this comparison is a no-op — the tie-breakers below
		// decide exactly as they did before cost existed.
		if r.Nodes[i].cost != r.Nodes[j].cost {
			return r.Nodes[i].cost < r.Nodes[j].cost
		}
		// Existing deterministic tie-breakers stay exactly as they were.
		if r.Nodes[i].Depth != r.Nodes[j].Depth {
			return r.Nodes[i].Depth < r.Nodes[j].Depth
		}
		return r.Nodes[i].SymbolID < r.Nodes[j].SymbolID
	})
	// PrunedHubs is discovered in whatever order SQLite returned frontier rows
	// in, which is not an order callers may depend on. (Depth, SymbolID) is
	// total here because a node is pruned at most once, at one depth.
	sort.Slice(r.PrunedHubs, func(i, j int) bool {
		if r.PrunedHubs[i].Depth != r.PrunedHubs[j].Depth {
			return r.PrunedHubs[i].Depth < r.PrunedHubs[j].Depth
		}
		return r.PrunedHubs[i].SymbolID < r.PrunedHubs[j].SymbolID
	})
	return r
}

// betterVia reports whether a newly found same-depth route should replace the
// recorded one. The keys, in order: stronger path confidence, then cheaper
// accumulated cost, then edge kind.
//
// Cost has to be compared here, not only in finish(). Result.finish ranks the
// whole result by Node.cost, and callers truncate in that rank order, so the
// cost this function chooses to keep decides which symbols a bounded answer
// shows. Two same-depth routes can tie on confidence and kind and still differ
// in cost — root -(exact)-> P1 -(medium)-> X costs 6, root -(medium)-> P2
// -(medium)-> X costs 10, and both are medium CALLS. Without this comparison
// the recorded cost was whichever route the level scan happened to return
// first, which is a database row order, so re-indexing identical source could
// reorder the answer. Cheaper wins: it is the route with the better evidence,
// by the same argument cost.go makes.
func betterVia(newConf model.Confidence, newCost int, newKind model.EdgeKind, oldConf model.Confidence, oldCost int, oldKind model.EdgeKind) bool {
	if newConf.Rank() != oldConf.Rank() {
		return newConf.Rank() > oldConf.Rank()
	}
	if newCost != oldCost {
		return newCost < oldCost
	}
	return newKind < oldKind
}
