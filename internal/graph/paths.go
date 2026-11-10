package graph

import (
	"context"
	"errors"
	"sort"
)

// DefaultMaxPaths bounds Paths when the caller passes no limit. An agent asking
// "how does A reach B" wants a handful of concrete routes, not an enumeration.
const DefaultMaxPaths = 10

// pathsNodeCap bounds how much of the graph the search may materialise.
//
// Paths keeps a predecessor list per discovered node, so unlike Traverse its
// memory is a function of the explored region rather than the returned paths.
// The cap is what stops "how does A reach B" from becoming a whole-graph load
// when B is unreachable and the frontier just keeps growing.
const pathsNodeCap = 100_000

// Paths returns up to maxPaths shortest routes from `from` to `to`, each a
// slice of symbol ids beginning with from and ending with to.
//
// Only SHORTEST paths are returned. This is a bound, not an oversight: the
// number of distinct paths between two nodes in a dense code graph is
// exponential, so any complete answer is unusable, and the most direct routes
// are what actually explains a relationship. A longer, more circuitous route
// that happens to exist will not appear.
//
// An unreachable target is not an error; it returns no paths.
//
// The search is the same level-synchronous expansion Traverse uses — one query
// per depth, not one per node — with the predecessors of each newly discovered
// node recorded. Enumeration then walks that layered DAG backwards from `to`.
// Because every node in it was discovered by BFS, every backward step reaches a
// strictly shallower node and every walk terminates at `from`: the enumeration
// never backtracks out of a dead end, so producing maxPaths routes costs
// O(maxPaths x depth) rather than an exponential search.
func (g *Graph) Paths(ctx context.Context, from, to int64, maxPaths, maxDepth int) ([][]int64, error) {
	if maxPaths <= 0 {
		maxPaths = DefaultMaxPaths
	}
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if from == to {
		return [][]int64{{from}}, nil
	}

	runID, err := g.run(ctx)
	if err != nil {
		return nil, err
	}

	depthOf := map[int64]int{from: 0}
	preds := make(map[int64][]int64)
	frontier := []int64{from}
	found := false

	for depth := 1; depth <= maxDepth && len(frontier) > 0 && !found; depth++ {
		var next []int64

		visit := func(h hop) error {
			if d, ok := depthOf[h.to]; ok {
				// Only same-level predecessors are kept. A predecessor from a
				// deeper level would create a longer path, and one from a
				// shallower level would create a cycle in the DAG the
				// enumeration walks.
				if d == depth {
					preds[h.to] = append(preds[h.to], h.from)
				}
				return nil
			}
			if len(depthOf) >= pathsNodeCap {
				return errNodeCap
			}
			depthOf[h.to] = depth
			preds[h.to] = append(preds[h.to], h.from)
			next = append(next, h.to)
			if h.to == to {
				// Do not break out here: the rest of this level may hold more
				// shortest paths to the same target, and dropping them would
				// silently bias the answer toward whichever row SQLite
				// happened to return first.
				found = true
			}
			return nil
		}

		err := g.forEachHop(ctx, runID, frontier, nil, "", dirOut, visit)
		if errors.Is(err, errNodeCap) {
			break
		}
		if err != nil {
			return nil, err
		}
		if g.afterLevel != nil {
			g.afterLevel(depth)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frontier = next
	}

	if !found {
		return nil, nil
	}
	return enumeratePaths(preds, from, to, maxPaths), nil
}

func enumeratePaths(preds map[int64][]int64, from, to int64, maxPaths int) [][]int64 {
	// Parallel edges (two kinds between the same pair) record the same
	// predecessor twice and would otherwise yield duplicate paths. Sorting also
	// fixes the output order, which would otherwise depend on row order.
	for node, list := range preds {
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
		preds[node] = compactSorted(list)
	}

	out := make([][]int64, 0, maxPaths)
	reversed := []int64{to}

	var walk func(node int64)
	walk = func(node int64) {
		if len(out) >= maxPaths {
			return
		}
		if node == from {
			path := make([]int64, len(reversed))
			for i, id := range reversed {
				path[len(reversed)-1-i] = id
			}
			out = append(out, path)
			return
		}
		for _, p := range preds[node] {
			reversed = append(reversed, p)
			walk(p)
			reversed = reversed[:len(reversed)-1]
			if len(out) >= maxPaths {
				return
			}
		}
	}
	walk(to)
	return out
}

func compactSorted(ids []int64) []int64 {
	if len(ids) < 2 {
		return ids
	}
	n := 1
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[n-1] {
			ids[n] = ids[i]
			n++
		}
	}
	return ids[:n]
}
