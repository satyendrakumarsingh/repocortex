package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// CSREdge is one directed hop in the in-memory graph index.
type CSREdge struct {
	Target     int32
	Kind       model.EdgeKind
	Confidence model.Confidence
}

// EdgeRecord is a raw edge loaded from storage or memory.
type EdgeRecord struct {
	SrcID      int64
	DstID      int64
	Kind       model.EdgeKind
	Confidence model.Confidence
}

// CSRGraph is an in-memory Compressed Sparse Row graph for high-throughput traversals.
// It maps symbol IDs to dense contiguous 32-bit indices and stores edge arrays in
// flat contiguous slices, achieving sub-millisecond graph traversals over hundreds
// of thousands of edges with zero garbage-collector overhead.
type CSRGraph struct {
	runID int64

	// Symbol ID mapping
	idToNode map[int64]int32
	nodeToID []int64

	// Outgoing edges (forward direction)
	outOffsets []int32
	outEdges   []CSREdge

	// Incoming edges (reverse direction)
	inOffsets []int32
	inEdges   []CSREdge
}

// maxCSRIndex is the largest node or edge count a CSRGraph can hold: node
// indices and edge offsets are stored as int32 to halve the index's memory.
const maxCSRIndex = math.MaxInt32

// checkCSRBounds reports an error when a graph of this size cannot be indexed
// with int32 node ids and offsets, instead of letting the conversions wrap.
func checkCSRBounds(nodes, edges int) error {
	if edges > maxCSRIndex {
		return fmt.Errorf("csr: %d edges exceeds the int32 index limit of %d", edges, maxCSRIndex)
	}
	if nodes > maxCSRIndex {
		return fmt.Errorf("csr: %d nodes exceeds the int32 index limit of %d", nodes, maxCSRIndex)
	}
	return nil
}

// NewCSRGraph constructs an in-memory Compressed Sparse Row representation from a slice of edges.
// It returns an error if the graph is too large for int32 indexing.
func NewCSRGraph(runID int64, records []EdgeRecord) (*CSRGraph, error) {
	if err := checkCSRBounds(0, len(records)); err != nil {
		return nil, err
	}
	idToNode := make(map[int64]int32, len(records))
	var nodeToID []int64

	nodeOf := func(id int64) int32 {
		if idx, ok := idToNode[id]; ok {
			return idx
		}
		// Safe: the caller stops registering nodes once the count reaches
		// maxCSRIndex (see the loop below), so len(nodeToID) fits in int32.
		idx := int32(len(nodeToID)) //nolint:gosec // bounded by the maxCSRIndex check in the registration loop
		idToNode[id] = idx
		nodeToID = append(nodeToID, id)
		return idx
	}

	for _, e := range records {
		// Each edge can add two nodes; refuse before the index could wrap.
		if err := checkCSRBounds(len(nodeToID)+2, len(records)); err != nil {
			return nil, err
		}
		nodeOf(e.SrcID)
		nodeOf(e.DstID)
	}

	numNodes := len(nodeToID)
	outDegree := make([]int32, numNodes)
	inDegree := make([]int32, numNodes)

	for _, e := range records {
		src := idToNode[e.SrcID]
		dst := idToNode[e.DstID]
		outDegree[src]++
		inDegree[dst]++
	}

	outOffsets := make([]int32, numNodes+1)
	inOffsets := make([]int32, numNodes+1)
	for i := 0; i < numNodes; i++ {
		outOffsets[i+1] = outOffsets[i] + outDegree[i]
		inOffsets[i+1] = inOffsets[i] + inDegree[i]
	}

	outEdges := make([]CSREdge, len(records))
	inEdges := make([]CSREdge, len(records))

	outCursor := make([]int32, numNodes)
	copy(outCursor, outOffsets[:numNodes])

	inCursor := make([]int32, numNodes)
	copy(inCursor, inOffsets[:numNodes])

	for _, e := range records {
		src := idToNode[e.SrcID]
		dst := idToNode[e.DstID]

		posOut := outCursor[src]
		outCursor[src]++
		outEdges[posOut] = CSREdge{
			Target:     dst,
			Kind:       e.Kind,
			Confidence: e.Confidence,
		}

		posIn := inCursor[dst]
		inCursor[dst]++
		inEdges[posIn] = CSREdge{
			Target:     src,
			Kind:       e.Kind,
			Confidence: e.Confidence,
		}
	}

	return &CSRGraph{
		runID:      runID,
		idToNode:   idToNode,
		nodeToID:   nodeToID,
		outOffsets: outOffsets,
		outEdges:   outEdges,
		inOffsets:  inOffsets,
		inEdges:    inEdges,
	}, nil
}

// LoadCSR reads all edges for runID from the graph's database into an in-memory CSRGraph snapshot.
// It streams edges in two passes directly from SQLite:
//  1. Pass 1 streams (src_id, dst_id) to map node IDs and compute degree counts,
//     calculating offsets and pre-allocating exact-size edge buffers.
//  2. Pass 2 streams (src_id, dst_id, kind, confidence) directly into outEdges and inEdges,
//     avoiding holding all intermediate EdgeRecord structs in memory simultaneously with CSRGraph.
func (g *Graph) LoadCSR(ctx context.Context, runID int64) (*CSRGraph, error) {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("csr: begin tx for run %d: %w", runID, err)
	}
	defer tx.Rollback()

	// Pass 1: Stream node IDs and compute degree counts.
	rows, err := tx.QueryContext(ctx, `
		SELECT src_id, dst_id
		FROM edges
		WHERE index_run_id = ?
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("csr: query edges pass 1 for run %d: %w", runID, err)
	}
	defer rows.Close()

	idToNode := make(map[int64]int32)
	var nodeToID []int64
	var outDegree []int32
	var inDegree []int32
	var edgeCount int

	nodeOf := func(id int64) (int32, error) {
		if idx, ok := idToNode[id]; ok {
			return idx, nil
		}
		if len(nodeToID) >= maxCSRIndex {
			return 0, fmt.Errorf("csr: %d nodes exceeds the int32 index limit of %d", len(nodeToID)+1, maxCSRIndex)
		}
		idx := int32(len(nodeToID))
		idToNode[id] = idx
		nodeToID = append(nodeToID, id)
		outDegree = append(outDegree, 0)
		inDegree = append(inDegree, 0)
		return idx, nil
	}

	for rows.Next() {
		var srcID, dstID int64
		if err := rows.Scan(&srcID, &dstID); err != nil {
			return nil, fmt.Errorf("csr: scan edge pass 1: %w", err)
		}
		edgeCount++
		src, err := nodeOf(srcID)
		if err != nil {
			return nil, err
		}
		dst, err := nodeOf(dstID)
		if err != nil {
			return nil, err
		}
		outDegree[src]++
		inDegree[dst]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("csr: rows iteration pass 1: %w", err)
	}
	_ = rows.Close()

	if err := checkCSRBounds(len(nodeToID), edgeCount); err != nil {
		return nil, err
	}

	numNodes := len(nodeToID)
	outOffsets := make([]int32, numNodes+1)
	inOffsets := make([]int32, numNodes+1)
	for i := 0; i < numNodes; i++ {
		outOffsets[i+1] = outOffsets[i] + outDegree[i]
		inOffsets[i+1] = inOffsets[i] + inDegree[i]
	}

	// Release degree counts before allocating edge arrays.
	outDegree = nil
	inDegree = nil

	if edgeCount == 0 {
		return &CSRGraph{
			runID:      runID,
			idToNode:   idToNode,
			nodeToID:   nodeToID,
			outOffsets: outOffsets,
			outEdges:   nil,
			inOffsets:  inOffsets,
			inEdges:    nil,
		}, nil
	}

	outEdges := make([]CSREdge, edgeCount)
	inEdges := make([]CSREdge, edgeCount)

	outCursor := make([]int32, numNodes)
	copy(outCursor, outOffsets[:numNodes])

	inCursor := make([]int32, numNodes)
	copy(inCursor, inOffsets[:numNodes])

	// Pass 2: Stream edge attributes directly into CSR edge buffers.
	rows2, err := tx.QueryContext(ctx, `
		SELECT src_id, dst_id, kind, confidence
		FROM edges
		WHERE index_run_id = ?
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("csr: query edges pass 2 for run %d: %w", runID, err)
	}
	defer rows2.Close()

	for rows2.Next() {
		var srcID, dstID int64
		var kind, conf sql.RawBytes
		if err := rows2.Scan(&srcID, &dstID, &kind, &conf); err != nil {
			return nil, fmt.Errorf("csr: scan edge pass 2: %w", err)
		}
		src := idToNode[srcID]
		dst := idToNode[dstID]

		k := internKind(kind)
		c := internConfidence(conf)

		posOut := outCursor[src]
		outCursor[src]++
		outEdges[posOut] = CSREdge{
			Target:     dst,
			Kind:       k,
			Confidence: c,
		}

		posIn := inCursor[dst]
		inCursor[dst]++
		inEdges[posIn] = CSREdge{
			Target:     src,
			Kind:       k,
			Confidence: c,
		}
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("csr: rows iteration pass 2: %w", err)
	}
	_ = rows2.Close()

	return &CSRGraph{
		runID:      runID,
		idToNode:   idToNode,
		nodeToID:   nodeToID,
		outOffsets: outOffsets,
		outEdges:   outEdges,
		inOffsets:  inOffsets,
		inEdges:    inEdges,
	}, nil
}

// NodeCount returns the total number of distinct symbols in the CSR graph.
func (c *CSRGraph) NodeCount() int {
	return len(c.nodeToID)
}

// EdgeCount returns the total number of directed edges in the CSR graph.
func (c *CSRGraph) EdgeCount() int {
	return len(c.outEdges)
}

// Degree returns the degree of a symbol in the given direction.
func (c *CSRGraph) Degree(symbolID int64, dir Direction) int {
	idx, ok := c.idToNode[symbolID]
	if !ok {
		return 0
	}
	switch dir {
	case Outgoing:
		return int(c.outOffsets[idx+1] - c.outOffsets[idx])
	case Incoming:
		return int(c.inOffsets[idx+1] - c.inOffsets[idx])
	case Both:
		return int((c.outOffsets[idx+1] - c.outOffsets[idx]) + (c.inOffsets[idx+1] - c.inOffsets[idx]))
	default:
		return 0
	}
}

// Traverse executes a level-synchronous in-memory traversal matching Graph.Traverse semantics.
func (c *CSRGraph) Traverse(ctx context.Context, q Query) (Result, error) {
	if len(q.Roots) == 0 {
		return Result{}, errors.New("graph: Query.Roots must contain at least one symbol id")
	}
	if q.HubPercentile != 0 && (q.HubPercentile <= 0 || q.HubPercentile > 1) {
		return Result{}, fmt.Errorf("graph: Query.HubPercentile must be in (0, 1], got %v", q.HubPercentile)
	}

	if q.MaxDepth <= 0 {
		q.MaxDepth = DefaultMaxDepth
	}
	if q.MaxNodes <= 0 {
		q.MaxNodes = DefaultMaxNodes
	}

	allowedKinds := make(map[model.EdgeKind]bool, len(q.Kinds))
	for _, k := range q.Kinds {
		allowedKinds[k] = true
	}

	hubLimit := q.HubDegreeLimit
	if hubLimit == 0 && q.HubPercentile > 0 {
		hubLimit = c.calculatePercentileDegree(q.HubPercentile, q.Direction)
	}

	res := Result{
		Nodes: make([]Node, 0, min(q.MaxNodes, len(q.Roots)*8)),
	}

	// truncatedAtNodeCap finishes the traversal when expand hit the node budget.
	// errNodeCap is the budget signal, not a failure: the result so far is valid
	// and is returned with its truncation reason set, exactly as the SQL
	// traversal does. Any other error is a real failure and is propagated.
	truncatedAtNodeCap := func(err error, depth int) (Result, error) {
		if !errors.Is(err, errNodeCap) {
			return Result{}, fmt.Errorf("csr: expand frontier at depth %d: %w", depth, err)
		}
		res.truncate(fmt.Sprintf(
			"node limit %d reached at depth %d, and the reachable set is larger than this result",
			q.MaxNodes, depth))
		res.noteHubPruning(hubLimit)
		return res.finish(), nil
	}

	index := make(map[int32]int, q.MaxNodes)
	var frontier []int32
	seenRoots := make(map[int64]bool, len(q.Roots))

	for _, r := range q.Roots {
		if seenRoots[r] {
			continue
		}
		seenRoots[r] = true

		nodeIdx, exists := c.idToNode[r]
		if !exists {
			continue
		}

		if len(res.Nodes) >= q.MaxNodes {
			res.truncate(fmt.Sprintf(
				"node limit %d reached while seeding roots, and %d root(s) were dropped and nothing was expanded",
				q.MaxNodes, len(q.Roots)-len(res.Nodes)))
			return res.finish(), nil
		}

		index[nodeIdx] = len(res.Nodes)
		res.Nodes = append(res.Nodes, Node{
			SymbolID:   r,
			Depth:      0,
			Confidence: model.ConfidenceExact,
			cost:       0,
		})
		frontier = append(frontier, nodeIdx)
	}

	rootSet := make(map[int32]bool, len(frontier))
	for _, r := range frontier {
		rootSet[r] = true
	}

	for depth := 1; depth <= q.MaxDepth && len(frontier) > 0; depth++ {
		var next []int32

		for _, curr := range frontier {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}

			// Check hub degree pruning
			if hubLimit > 0 && !rootSet[curr] {
				deg := c.degreeForQuery(curr, q.Direction, allowedKinds, q.MinConfidence)
				if deg > hubLimit {
					res.PrunedHubs = append(res.PrunedHubs, PrunedHub{
						SymbolID: c.nodeToID[curr],
						Degree:   deg,
						Depth:    depth - 1,
					})
					continue
				}
			}

			parent := &res.Nodes[index[curr]]
			parentConf := parent.Confidence
			parentCost := parent.cost

			// Expand neighbors based on Direction
			expand := func(edges []CSREdge) error {
				for _, edge := range edges {
					if len(allowedKinds) > 0 && !allowedKinds[edge.Kind] {
						continue
					}
					if q.MinConfidence != "" && edge.Confidence.Rank() < q.MinConfidence.Rank() {
						continue
					}

					target := edge.Target
					pathConf := weaker(parentConf, edge.Confidence)
					pathCost := parentCost + EdgeCost(edge.Confidence)

					if at, ok := index[target]; ok {
						n := &res.Nodes[at]
						if n.Depth == depth && betterVia(pathConf, pathCost, edge.Kind, n.Confidence, n.cost, n.Via) {
							n.Confidence, n.Via, n.cost = pathConf, edge.Kind, pathCost
						}
						continue
					}

					if len(res.Nodes) >= q.MaxNodes {
						return errNodeCap
					}

					index[target] = len(res.Nodes)
					res.Nodes = append(res.Nodes, Node{
						SymbolID:   c.nodeToID[target],
						Depth:      depth,
						Via:        edge.Kind,
						Confidence: pathConf,
						cost:       pathCost,
					})
					next = append(next, target)
				}
				return nil
			}

			if q.Direction == Outgoing || q.Direction == Both {
				start := c.outOffsets[curr]
				end := c.outOffsets[curr+1]
				if err := expand(c.outEdges[start:end]); err != nil {
					return truncatedAtNodeCap(err, depth)
				}
			}

			if q.Direction == Incoming || q.Direction == Both {
				start := c.inOffsets[curr]
				end := c.inOffsets[curr+1]
				if err := expand(c.inEdges[start:end]); err != nil {
					return truncatedAtNodeCap(err, depth)
				}
			}
		}

		frontier = next
	}

	if len(frontier) > 0 {
		res.truncate(fmt.Sprintf(
			"depth limit %d reached, and %d node(s) at the limit were not expanded",
			q.MaxDepth, len(frontier)))
	}

	res.noteHubPruning(hubLimit)
	return res.finish(), nil
}

func (c *CSRGraph) degreeForQuery(node int32, dir Direction, kinds map[model.EdgeKind]bool, minConf model.Confidence) int {
	deg := 0
	countEdges := func(edges []CSREdge) {
		for _, e := range edges {
			if len(kinds) > 0 && !kinds[e.Kind] {
				continue
			}
			if minConf != "" && e.Confidence.Rank() < minConf.Rank() {
				continue
			}
			deg++
		}
	}

	if dir == Outgoing || dir == Both {
		start := c.outOffsets[node]
		end := c.outOffsets[node+1]
		countEdges(c.outEdges[start:end])
	}
	if dir == Incoming || dir == Both {
		start := c.inOffsets[node]
		end := c.inOffsets[node+1]
		countEdges(c.inEdges[start:end])
	}
	return deg
}

func (c *CSRGraph) calculatePercentileDegree(pct float64, dir Direction) int {
	if len(c.nodeToID) == 0 {
		return minHubDegree
	}

	degrees := make([]int, len(c.nodeToID))
	for i := range c.nodeToID {
		degrees[i] = c.Degree(c.nodeToID[i], dir)
	}
	sort.Ints(degrees)

	idx := int(float64(len(degrees)-1) * pct)
	if idx >= len(degrees) {
		idx = len(degrees) - 1
	}

	limit := degrees[idx]
	if limit < minHubDegree {
		return minHubDegree
	}
	return limit
}
