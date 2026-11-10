package context

import (
	"context"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// reach records how a symbol was reached from the seed set.
type reach struct {
	depth int
	via   model.EdgeKind

	// conf is the MINIMUM confidence along the path, as graph.Traverse
	// computes it. Carrying it forward is what stops a chain of guesses from
	// being ranked like a chain of proofs.
	conf model.Confidence
}

// expand walks outward from the seeds, bounded, and reports what it had to
// leave out.
//
// Direction is Both. "What does the callback path call" and "what calls into
// it" are both context for changing it: adding idempotency to
// CallbackController means knowing what handleCallback invokes AND what else
// invokes the same service. Restricting to Outgoing would have dropped
// TransactionService's other caller; restricting to Incoming would have dropped
// the repository and notification hops the golden test requires.
//
// MinConfidence is deliberately not set. Filtering weak edges out of the
// TRAVERSAL would silently shrink the reachable set; instead every edge is
// followed and confidence is applied as a discount during ranking, so a
// weakly-supported path can still surface and says so.
func expand(ctx context.Context, g Graph, seeds []seed, b Budget, th Thresholds) (map[int64]reach, []string, error) {
	if len(seeds) == 0 || g == nil {
		return map[int64]reach{}, nil, nil
	}

	roots := make([]int64, 0, len(seeds))
	for _, s := range seeds {
		roots = append(roots, s.symbolID)
	}

	res, err := g.Traverse(ctx, graph.Query{
		Roots:          roots,
		MaxDepth:       b.MaxGraphDepth,
		MaxNodes:       th.MaxGraphNodes,
		HubDegreeLimit: b.HubDegreeLimit,
		Direction:      graph.Both,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("context: expand from %d seed(s): %w", len(roots), err)
	}

	out := make(map[int64]reach, len(res.Nodes))
	for _, n := range res.Nodes {
		out[n.SymbolID] = reach{depth: n.Depth, via: n.Via, conf: n.Confidence}
	}

	var notes []string
	if res.Truncated {
		// graph.Result.Truncated is set for the caller's own depth limit too,
		// which is the normal case here and not worth alarming an agent about.
		// It is still reported: an agent told the neighbourhood was cut short
		// can ask a narrower question instead of assuming it saw everything.
		notes = append(notes, "Graph expansion was bounded: "+res.TruncatedReason+
			". Symbols beyond that boundary are not represented in this pack.")
	}
	return out, notes, nil
}

// proximity converts hop distance into a signal in (0,1].
//
// 1/(1+depth): a direct neighbour is worth half a seed, two hops a third. The
// decay is deliberately gentler than exponential — in real code the thing you
// must change is routinely two hops from the thing you searched for (controller
// -> service -> repository is the fixture's entire point) — and the depth bound
// rather than the decay is what stops the walk.
func proximity(depth int) float64 { return 1.0 / float64(1+depth) }

// confidenceFactor maps a path's minimum confidence to [0,1].
func confidenceFactor(c model.Confidence) float64 {
	if !c.Valid() {
		return 0
	}
	return float64(c.Rank()) / float64(model.ConfidenceExact.Rank())
}
