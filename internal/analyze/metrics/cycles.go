package metrics

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Component is one strongly connected component.
//
// A component with more than one member is a dependency cycle: every member can
// reach every other. Singletons are included so callers can report "N of M
// nodes are in a cycle" without a second pass.
type Component struct {
	ID      int
	Members []string
}

// Components runs Tarjan's SCC algorithm.
//
// Nodes are visited in ascending key order and each node's successors are
// already in canonical edge order, so the component partition and the order of
// members within it are identical on every run.
func Components(ctx context.Context, g Graph) ([]Component, error) {
	var (
		index    = make(map[string]int, g.Order())
		low      = make(map[string]int, g.Order())
		onStack  = make(map[string]bool, g.Order())
		stack    []string
		next     int
		outComps [][]string
	)

	var strongConnect func(v string) error
	strongConnect = func(v string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true

		for _, e := range g.OutEdges(v) {
			w := e.To
			if _, seen := index[w]; !seen {
				if err := strongConnect(w); err != nil {
					return err
				}
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] {
				if index[w] < low[v] {
					low[v] = index[w]
				}
			}
		}

		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			sort.Strings(comp)
			outComps = append(outComps, comp)
		}
		return nil
	}

	for _, k := range g.keys {
		if _, seen := index[k]; seen {
			continue
		}
		if err := strongConnect(k); err != nil {
			return nil, fmt.Errorf("computing strongly connected components: %w", err)
		}
	}

	// Total order: larger components first, then lexicographic on the first
	// member. Without this the output order follows DFS discovery, which is
	// stable but reads arbitrarily in a report.
	sort.Slice(outComps, func(i, j int) bool {
		if len(outComps[i]) != len(outComps[j]) {
			return len(outComps[i]) > len(outComps[j])
		}
		return outComps[i][0] < outComps[j][0]
	})

	result := make([]Component, 0, len(outComps))
	for i, c := range outComps {
		result = append(result, Component{ID: i, Members: c})
	}
	return result, nil
}

// CycleBudget bounds cycle enumeration in three deterministic ways.
//
// Deliberately no timeout. A wall-clock deadline would make the returned list a
// function of how loaded the machine was, and this package's contract is
// byte-identical output for identical input — a caller diffing two index runs
// would read the difference as architectural change. Every bound here is a
// count evaluated in the canonical traversal order, so the same graph and the
// same budget always cut at the same place.
type CycleBudget struct {
	// MaxLen is the longest cycle searched for, in nodes. Below 2 no cycle can
	// exist and the search is skipped.
	MaxLen int
	// MaxCycles caps how many cycles are RETURNED. Zero means uncapped. The
	// search stops one cycle past it, so CycleBudgetHit means a further cycle
	// was actually found rather than that the count happened to land on the cap.
	MaxCycles int
	// MaxSteps caps the search's WORK: one step is one outbound edge examined
	// during the depth-first walk. Zero means uncapped, which is exhaustive and
	// unbounded in cost. This is the bound that keeps a caller interactive:
	// MaxCycles caps output, but a dense component can burn seconds of walking
	// without completing another cycle.
	MaxSteps int
}

// DefaultCycleBudget returns bounds measured to keep the worst shape this
// codebase has benchmarked well inside its 2s budget.
//
// The numbers are measurements, not estimates (Apple M4 Pro, arm64, over
// cycles_test.go's denseGraph). On 20,000 nodes at out-degree 10 — 200,000
// edges, nearly all of them inside one giant strongly connected component — an
// unbudgeted enumeration cost 26.4s and returned 24,101 cycles. That shape
// passed the web layer's former 25,000-node bound while costing more than twice
// the 100k-node fixture that motivated it, because cost follows DENSITY and a
// node count cannot see density.
//
// Both bounds are needed, and the step bound is the load-bearing one:
//
//	budget                          20k/deg-7   20k/deg-10
//	none                                5.76s        26.4s
//	MaxCycles 2,000 alone               3.51s        2.84s
//	MaxCycles 2,000 + MaxSteps 5M      405ms        369ms
//
// The MaxCycles-alone row was re-measured when the cap moved to stopping one
// cycle PAST its limit (see spent): proving the limit bound anything costs one
// further discovery, which on these shapes is 7-9%.
//
// MaxCycles alone happens to cut these two graphs short because they are cycle-
// rich, but a graph can walk for seconds between cycles, so a cap on output is
// not a cap on cost. MaxSteps is. 5,000,000 is the largest round step count that
// keeps both cases under half a second, leaving room for the rest of a caller's
// suite; it yields 221 and 244 cycles respectively, an order of magnitude more
// than any report lists. MaxLen stays 5: the count of elementary cycles is
// exponential in length, and a longer list answers no question anyone asked.
func DefaultCycleBudget() CycleBudget {
	return CycleBudget{MaxLen: 5, MaxCycles: 2_000, MaxSteps: 5_000_000}
}

// CycleReport is the enumeration plus what it did not do.
//
// The bounds are reported rather than applied silently for the same reason
// MaxLen always was: an incomplete list that does not admit it is
// indistinguishable from a complete one, and "no cycles beyond these" is a
// claim the search never made.
type CycleReport struct {
	// Cycles are the cycles found, shortest first then lexicographic.
	Cycles [][]string
	// Steps is how many outbound edges the walk examined.
	Steps int
	// CycleBudgetHit is true when MaxCycles stopped the search, which requires
	// a cycle past the cap to have been found: a graph holding exactly MaxCycles
	// cycles was enumerated completely and says so. The list holds the first
	// MaxCycles cycles in traversal order, which is not the same as the
	// MaxCycles shortest.
	CycleBudgetHit bool
	// StepBudgetHit is true when MaxSteps stopped the search. Cycles that a
	// complete walk would have found are missing, and how many is exactly what
	// was not computed.
	StepBudgetHit bool
}

// Truncated reports whether any budget stopped the search short.
func (r CycleReport) Truncated() bool { return r.CycleBudgetHit || r.StepBudgetHit }

// Cycles enumerates elementary cycles within the given budget.
//
// The length bound is not an optimisation, it is the feature: the number of
// elementary cycles in a dense graph is exponential, and a report listing
// hundreds of thousands of them answers no question anyone asked. Length alone
// is not enough of a bound, though — at a fixed MaxLen of 5 the walk still cost
// 26s on a 20,000-node graph — so the work bound in CycleBudget caps the search
// itself, in the canonical order, and says when it did.
//
// Cycles are searched only inside multi-member components, since no cycle can
// cross one. Each cycle is rotated so its smallest member is first, which makes
// the same cycle compare equal however it was discovered.
func Cycles(ctx context.Context, g Graph, b CycleBudget) (CycleReport, error) {
	var report CycleReport
	if b.MaxLen < 2 {
		return report, nil
	}
	comps, err := Components(ctx, g)
	if err != nil {
		return report, err
	}

	seen := map[string]struct{}{}
	var found [][]string

	// errBudget unwinds the recursion when a bound is reached. It never leaves
	// this function: a budget is a documented outcome reported in CycleReport,
	// not a failure the caller has to handle.
	errBudget := errors.New("cycle budget exhausted")

	// The cycle cap stops the search one cycle PAST MaxCycles, and the extra one
	// is dropped below. Stopping AT the cap cannot tell a truncated enumeration
	// from a complete one: a graph holding exactly MaxCycles cycles reported
	// CycleBudgetHit, because the same check runs again on the next edge of the
	// same walk, and Truncated() then said a complete list was cut short. Only
	// the existence of a further cycle proves anything was withheld, so that is
	// what is measured. The extra cycle costs one more discovery, which a graph
	// cyclic enough to reach the cap completes immediately and a graph that is
	// not was never the expensive case — MaxSteps still bounds the walk either
	// way.
	spent := func() bool {
		if b.MaxSteps > 0 && report.Steps >= b.MaxSteps {
			report.StepBudgetHit = true
			return true
		}
		if b.MaxCycles > 0 && len(found) > b.MaxCycles {
			report.CycleBudgetHit = true
			return true
		}
		return false
	}

search:
	for _, comp := range comps {
		if len(comp.Members) < 2 {
			continue
		}
		inComp := make(map[string]bool, len(comp.Members))
		for _, m := range comp.Members {
			inComp[m] = true
		}

		for _, start := range comp.Members {
			if err := ctx.Err(); err != nil {
				return report, fmt.Errorf("enumerating cycles: %w", err)
			}
			if spent() {
				break search
			}
			var walk func(path []string, onPath map[string]bool) error
			walk = func(path []string, onPath map[string]bool) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				cur := path[len(path)-1]
				for _, e := range g.OutEdges(cur) {
					if spent() {
						return errBudget
					}
					report.Steps++
					w := e.To
					if !inComp[w] {
						continue
					}
					if w == start && len(path) >= 2 {
						cyc := rotateSmallestFirst(path)
						key := joinPath(cyc)
						if _, dup := seen[key]; !dup {
							seen[key] = struct{}{}
							found = append(found, cyc)
						}
						continue
					}
					// Only extend forward past start to avoid rediscovering
					// every rotation of the same cycle.
					if w < start || onPath[w] || len(path) >= b.MaxLen {
						continue
					}
					onPath[w] = true
					if err := walk(append(path, w), onPath); err != nil {
						return err
					}
					delete(onPath, w)
				}
				return nil
			}
			if err := walk([]string{start}, map[string]bool{start: true}); err != nil {
				if errors.Is(err, errBudget) {
					break search
				}
				return report, fmt.Errorf("enumerating cycles: %w", err)
			}
		}
	}

	// Truncate before the presentation sort, so the kept list stays "the first
	// MaxCycles cycles in traversal order" that CycleReport.CycleBudgetHit
	// documents, not the MaxCycles shortest of the ones the walk happened to
	// reach.
	if b.MaxCycles > 0 && len(found) > b.MaxCycles {
		found = found[:b.MaxCycles]
	}
	sort.Slice(found, func(i, j int) bool {
		if len(found[i]) != len(found[j]) {
			return len(found[i]) < len(found[j])
		}
		return joinPath(found[i]) < joinPath(found[j])
	})
	report.Cycles = found
	return report, nil
}

func rotateSmallestFirst(path []string) []string {
	min := 0
	for i := 1; i < len(path); i++ {
		if path[i] < path[min] {
			min = i
		}
	}
	out := make([]string, 0, len(path))
	out = append(out, path[min:]...)
	out = append(out, path[:min]...)
	return out
}

func joinPath(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "\x00"
		}
		out += s
	}
	return out
}
