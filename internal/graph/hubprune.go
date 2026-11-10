package graph

// Refusing to expand THROUGH a hub.
//
// The expensive lesson this file encodes: a bounded walk is not the same as a
// useful one. A depth-3 traversal that happens to cross a logging helper with
// four thousand callers reaches most of the repository, satisfies every bound
// the caller set, and answers nothing. Cutting the traversal is both cheaper
// and more accurate than cutting the response, because by the time the response
// is trimmed the walk has already been paid for — and the trim keeps whichever
// thousand nodes came back first, not the thousand that matter.
//
// Two rules keep this from becoming a second kind of dishonesty. A pruned node
// is still REPORTED: it is genuinely reachable, and hiding it would misstate
// the graph rather than bound the walk. And every refusal is named in
// Result.PrunedHubs with the degree that caused it, so the caller can raise the
// limit knowingly instead of guessing at a hole they cannot see.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// minHubDegree floors any percentile-derived limit.
//
// A percentile is a statement about a distribution, and in a small or sparsely
// linked index the 99th percentile of out-degree can be 3. Pruning at 3 does
// not remove a hub, it removes the traversal. The floor says what the feature
// actually means: a hub is a node that is wide in absolute terms AND wide
// relative to its index, never merely the widest of a narrow field.
const minHubDegree = 50

// PrunedHub records one node whose expansion was refused.
//
// It is a struct of ints rather than a list of ids because a bare id does not
// let the caller act. Degree is what a caller needs to choose the next limit
// ("400 wide, and I asked for 50"), and Depth says where in the walk the hole
// is — a hub refused at depth 1 hid far more than one refused at the last
// level. SymbolID is int64 to match Node.SymbolID: identity in this package is
// the symbols table's primary key, not a rendered name, and formatting an id
// into a string here would force every caller to parse it back to look the
// symbol up.
type PrunedHub struct {
	SymbolID int64 `json:"symbol_id"`

	// Degree is the number of edges this node has in the direction(s) being
	// traversed, counted under the same kind and confidence filters as the
	// walk itself — so it is the number of hops actually declined, not a
	// context-free property of the node.
	Degree int `json:"degree"`

	// Depth is the hop count at which the node sat when it was refused.
	Depth int `json:"depth"`
}

// hubLimit resolves the effective degree limit for one run, or 0 for "no
// pruning".
//
// An explicit HubDegreeLimit wins outright: a caller who named a number meant
// it. Otherwise a percentile is resolved against this run's own degree
// distribution, because a constant does not transfer — 200 edges is a
// pathological hub in a 5k-symbol service and unremarkable in a 500k-symbol
// monolith.
func (g *Graph) hubLimit(ctx context.Context, runID int64, q Query, scans []edgeDir) (int, error) {
	if q.HubDegreeLimit > 0 {
		return q.HubDegreeLimit, nil
	}
	if q.HubPercentile <= 0 {
		return 0, nil
	}
	if q.HubPercentile > 1 {
		return 0, fmt.Errorf("graph: hub percentile %v out of range (0,1]", q.HubPercentile)
	}

	key := hubLimitKey{
		runID: runID,
		pct:   q.HubPercentile,
		scans: fmt.Sprint(scans),
		kinds: fmt.Sprint(q.Kinds),
		conf:  string(q.MinConfidence),
	}
	g.hubMu.RLock()
	cached, ok := g.hubLimits[key]
	g.hubMu.RUnlock()
	if ok {
		return cached, nil
	}

	deg, err := g.percentileDegree(ctx, runID, q, scans)
	if err != nil {
		return 0, err
	}
	// The floor is applied before caching so the stored value is the limit the
	// walk uses, not an intermediate a later reader could misread.
	if deg < minHubDegree {
		deg = minHubDegree
	}

	g.hubMu.Lock()
	defer g.hubMu.Unlock()
	if g.hubLimits == nil {
		g.hubLimits = make(map[hubLimitKey]int, 4)
	}
	// A long-lived Graph following the committed run accumulates one entry per
	// (run, filter set) it has ever seen. Dropping the whole map at a small
	// bound keeps that bounded without pretending to be an LRU: the cost of a
	// miss is one query, and the combinations in practice number a handful.
	if len(g.hubLimits) >= maxHubLimitCache {
		g.hubLimits = make(map[hubLimitKey]int, 4)
	}
	g.hubLimits[key] = deg
	return deg, nil
}

// maxHubLimitCache bounds the memo; see hubLimit.
const maxHubLimitCache = 64

// hubLimitKey identifies one degree distribution. Every field that changes
// which edges are counted has to be in it, or a filtered walk would inherit an
// unfiltered walk's threshold and prune the wrong nodes.
type hubLimitKey struct {
	runID int64
	pct   float64
	scans string
	kinds string
	conf  string
}

// percentileDegree reads the degree at q.HubPercentile in ONE statement.
//
// The one-query-per-level rule this package is built on forbids the obvious
// implementation — count each node's edges as the walk meets it — which would
// be one round trip per node and two orders of magnitude slower than the
// traversal it is supposed to protect. So the whole distribution is reduced
// inside SQLite: GROUP BY collapses the edge table to one row per node, a
// window function ranks those rows, and only the single row at the requested
// rank crosses the driver boundary. Nothing here is proportional to the graph
// in Go memory.
//
// Nodes with zero edges in the traversed direction never appear in the GROUP
// BY and so do not count toward the percentile. That is deliberate: the
// question is "wide compared to nodes that connect at all", and including the
// leaves would drag every percentile down to 1 in a graph that is mostly
// leaves.
func (g *Graph) percentileDegree(ctx context.Context, runID int64, q Query, scans []edgeDir) (int, error) {
	confs := allowedConfidences(q.MinConfidence)

	args := make([]any, 0, len(scans)*(1+len(q.Kinds)+len(confs))+1)
	for range scans {
		args = append(args, runID)
		for _, k := range q.Kinds {
			args = append(args, string(k))
		}
		for _, c := range confs {
			args = append(args, string(c))
		}
	}
	args = append(args, q.HubPercentile)

	row := g.db.QueryRowContext(ctx, percentileQuery(scans, len(q.Kinds), len(confs)), args...)
	var deg int
	if err := row.Scan(&deg); err != nil {
		// No rows means the run has no edges matching the filters, so there is
		// no distribution to take a percentile of and nothing to prune.
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("graph: resolve hub percentile %v: %w", q.HubPercentile, err)
	}
	return deg, nil
}

// percentileQuery builds the single grouped statement described above.
//
// Direction=Both is expressed as a UNION ALL of the two sides *inside* the
// grouping rather than as two statements, so a node's degree is its combined
// in- and out-degree — the number of hops the walk would actually take from it.
// Two statements would give two distributions that cannot be added.
//
// ORDER BY includes the node id so the rank is a total order and the resolved
// limit is reproducible run to run when degrees tie.
func percentileQuery(scans []edgeDir, nKinds, nConfs int) string {
	var b strings.Builder
	b.Grow(400 + len(scans)*2*(nKinds+nConfs))
	b.WriteString("SELECT deg FROM (SELECT COUNT(*) AS deg," +
		" ROW_NUMBER() OVER (ORDER BY COUNT(*), id) AS rn," +
		" COUNT(*) OVER () AS total FROM (")
	for i, dir := range scans {
		if i > 0 {
			b.WriteString(" UNION ALL ")
		}
		if dir == dirOut {
			b.WriteString("SELECT src_id AS id FROM edges INDEXED BY idx_edges_src WHERE index_run_id = ?")
		} else {
			b.WriteString("SELECT dst_id AS id FROM edges INDEXED BY idx_edges_dst WHERE index_run_id = ?")
		}
		writeEdgeFilters(&b, nKinds, nConfs)
	}
	b.WriteString(") GROUP BY id) WHERE rn >= MAX(1, CAST(total * ? AS INTEGER))" +
		" ORDER BY rn LIMIT 1")
	return b.String()
}

// partitionHubs splits one frontier into the nodes that may be expanded and the
// hubs that may not.
//
// Degrees are read with one grouped query per chunk per direction — the same
// shape and the same chunking as forEachHop, so enabling pruning costs one
// extra round trip per level, not one per node. When pruning is off this
// function is never called and the traversal issues exactly the queries it
// always did.
func (g *Graph) partitionHubs(
	ctx context.Context,
	runID int64,
	frontier []int64,
	roots map[int64]struct{},
	depth, limit int,
	q Query,
	scans []edgeDir,
) (keep []int64, pruned []PrunedHub, err error) {
	degrees, err := g.degrees(ctx, runID, frontier, q, scans)
	if err != nil {
		return nil, nil, err
	}

	// depth is the level Traverse is ABOUT TO expand into, but frontier holds
	// nodes already sitting at the level below it — the same nodes recorded in
	// res.Nodes with Node.Depth == depth-1. A pruned hub's Depth must match
	// that recorded Node.Depth, or a caller joining PrunedHubs to Nodes on
	// depth gets a wrong answer.
	nodeDepth := depth - 1

	keep = make([]int64, 0, len(frontier))
	for _, id := range frontier {
		// Roots are never pruned. A caller who asked about a hub is asking for
		// exactly the neighbours pruning would withhold, and returning a lone
		// node for that query answers nothing.
		if _, isRoot := roots[id]; isRoot {
			keep = append(keep, id)
			continue
		}
		if d := degrees[id]; d > limit {
			pruned = append(pruned, PrunedHub{SymbolID: id, Degree: d, Depth: nodeDepth})
			continue
		}
		keep = append(keep, id)
	}
	return keep, pruned, nil
}

// degrees counts, per frontier node, the edges the walk would follow from it.
//
// The count applies the query's own kind and confidence filters so it measures
// the traversal's fan-out rather than the node's raw popularity: a symbol with
// four thousand low-confidence references is not a hub for a walk that only
// follows compiler-proven calls, and pruning it there would withhold edges the
// walk was never going to be swamped by.
func (g *Graph) degrees(
	ctx context.Context,
	runID int64,
	ids []int64,
	q Query,
	scans []edgeDir,
) (map[int64]int, error) {
	confs := allowedConfidences(q.MinConfidence)
	out := make(map[int64]int, len(ids))

	for _, dir := range scans {
		for start := 0; start < len(ids); start += maxSQLVars {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := start + maxSQLVars
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]

			args := make([]any, 0, 1+len(chunk)+len(q.Kinds)+len(confs))
			args = append(args, runID)
			for _, id := range chunk {
				args = append(args, id)
			}
			for _, k := range q.Kinds {
				args = append(args, string(k))
			}
			for _, c := range confs {
				args = append(args, string(c))
			}

			rows, err := g.db.QueryContext(ctx,
				degreeQuery(dir, len(chunk), len(q.Kinds), len(confs)), args...)
			if err != nil {
				return nil, fmt.Errorf("graph: degree of %d node(s): %w", len(chunk), err)
			}
			// Both sums the two directions, which is why the map is added to
			// rather than assigned into.
			func() {
				defer rows.Close()
				for rows.Next() {
					var id int64
					var deg int
					if err = rows.Scan(&id, &deg); err != nil {
						err = fmt.Errorf("graph: scan degree: %w", err)
						return
					}
					out[id] += deg
				}
				if rerr := rows.Err(); rerr != nil {
					err = fmt.Errorf("graph: read degrees: %w", rerr)
				}
			}()
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// degreeQuery counts a chunk of frontier nodes' edges in one grouped statement.
//
// INDEXED BY for the same measured reason hopQuery names it: without ANALYZE
// statistics SQLite will happily pick idx_edges_conf and scan every edge of a
// confidence instead of seeking the frontier through idx_edges_src. The index
// also supplies the grouping order, so the GROUP BY costs no sort.
func degreeQuery(dir edgeDir, nIDs, nKinds, nConfs int) string {
	var b strings.Builder
	b.Grow(180 + 2*(nIDs+nKinds+nConfs))
	if dir == dirOut {
		b.WriteString("SELECT src_id, COUNT(*) FROM edges INDEXED BY idx_edges_src" +
			" WHERE index_run_id = ? AND src_id IN (")
	} else {
		b.WriteString("SELECT dst_id, COUNT(*) FROM edges INDEXED BY idx_edges_dst" +
			" WHERE index_run_id = ? AND dst_id IN (")
	}
	writePlaceholders(&b, nIDs)
	b.WriteString(")")
	writeEdgeFilters(&b, nKinds, nConfs)
	if dir == dirOut {
		b.WriteString(" GROUP BY src_id")
	} else {
		b.WriteString(" GROUP BY dst_id")
	}
	return b.String()
}

// writeEdgeFilters appends the kind and confidence predicates shared by the
// degree and percentile statements. Only placeholder counts are interpolated;
// no caller-supplied text reaches the SQL.
func writeEdgeFilters(b *strings.Builder, nKinds, nConfs int) {
	if nKinds > 0 {
		b.WriteString(" AND kind IN (")
		writePlaceholders(b, nKinds)
		b.WriteString(")")
	}
	if nConfs > 0 {
		b.WriteString(" AND confidence IN (")
		writePlaceholders(b, nConfs)
		b.WriteString(")")
	}
}

// noteHubPruning records the refusal in Truncated, appending to whatever reason
// is already there rather than replacing it.
//
// A walk can be incomplete for two independent reasons at once — it stopped at
// hubs AND it ran out of depth — and reporting only the last one to fire would
// send the caller to fix the wrong bound. The message names the count, the
// limit that produced it, and the two ways out, because "truncated: true" with
// no remedy is a dead end, not honesty.
func (r *Result) noteHubPruning(limit int) {
	if len(r.PrunedHubs) == 0 {
		return
	}
	reason := fmt.Sprintf(
		"stopped at %d hub(s) wider than %d edges; raise hub_degree_limit or start from a narrower symbol",
		len(r.PrunedHubs), limit)
	if r.Truncated && r.TruncatedReason != "" {
		r.TruncatedReason += "; " + reason
		return
	}
	r.truncate(reason)
}
