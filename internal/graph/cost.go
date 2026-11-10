package graph

// Confidence as a traversal cost, not merely a filter.
//
// Filtering is binary: an edge is in or out. That is the wrong shape for
// "reachable, but through a chain of guesses". A three-hop path of compiler-
// proven calls is better evidence than a two-hop path of name matches, and an
// unweighted BFS cannot say so — it ranks by hop count and the guesses win.
//
// The weights are deliberately coarse and deliberately super-linear. They are
// not calibrated probabilities and must not be presented as any: they encode
// one ordering decision, that two low-confidence hops are worse evidence than
// three proven ones, which the tests pin.
//
// KNOWN LIMITATION, NOT YET FIXED: cost only reorders results AFTER a
// traversal finishes; it does not steer the traversal itself. Traverse's BFS
// still expands the frontier unweighted, one hop-depth at a time, and only
// sorts the survivors by accumulated cost once the walk stops — see
// Node.cost and Result.finish in traverse.go. Under an ordinary MaxNodes cap
// this is enough, because every candidate is discovered before the cap is
// applied and cost merely picks the winners. But when the cap is hit DURING
// expansion, it is hit in BFS discovery order, not cost order: a
// low-confidence node two hops away is discovered, and counted against
// MaxNodes, before a high-confidence node three hops away is ever visited.
// The shallow guess can fill the last slot and the deeper proof is never
// reached at all — sorting the kept set afterward cannot recover a node BFS
// never found. That is precisely the failure this file exists to prevent; it
// prevents it only when nothing was truncated.

import "github.com/satyendrakumarsingh/repocortex/pkg/model"

// Edge costs in arbitrary units. An exact edge costs one unit — the same as a
// plain hop — so a fully proven walk behaves exactly like the unweighted BFS
// it replaces, and nothing about existing all-exact results changes.
const (
	costExact  = 1
	costHigh   = 2
	costMedium = 5
	costLow    = 13
)

// EdgeCost returns the traversal cost of crossing one edge of this confidence.
//
// An unset or unrecognised confidence costs as much as low. Treating unknown
// provenance as proven is the failure this guards: a new edge kind that forgets
// to set confidence would otherwise silently become the cheapest path in the
// graph.
func EdgeCost(c model.Confidence) int {
	switch c {
	case model.ConfidenceExact:
		return costExact
	case model.ConfidenceHigh:
		return costHigh
	case model.ConfidenceMedium:
		return costMedium
	default:
		// Covers ConfidenceLow, the empty value and anything unrecognised.
		return costLow
	}
}

// PathCost sums the cost of a walk. Zero edges cost zero.
func PathCost(confs []model.Confidence) int {
	total := 0
	for _, c := range confs {
		total += EdgeCost(c)
	}
	return total
}
