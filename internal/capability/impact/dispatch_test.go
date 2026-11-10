package impact

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func dispatchNames(in []Dispatched) []string {
	out := make([]string, len(in))
	for i, d := range in {
		out[i] = d.Name
	}
	return out
}

func findDispatched(t *testing.T, in []Dispatched, name string) Dispatched {
	t.Helper()
	for _, d := range in {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("%q not found in dispatch %v", name, dispatchNames(in))
	return Dispatched{}
}

// TestInterfaceMethodChangeReachesImplementorsDistinctly is the A2 assertion:
// changing an interface method must reach every implementation of it, and those
// implementations must be a DIFFERENT, labelled claim from the symbols that
// call the interface method directly.
//
// The fixture is the shape the change is about:
//
//	handler --CALLS--> Store.Save        (a direct caller of the interface)
//	PgSave  --IMPLEMENTS--> Store.Save   (an implementation)
//	job     --CALLS--> PgSave            (reachable only through the impl)
//
// Before IMPLEMENTS edges existed, PgSave and job were invisible to an impact
// analysis of Store.Save. With the traversal that produced them mixed into one
// walk they would be indistinguishable from handler, which is the failure this
// test pins: "reached" and "calls" are not the same claim.
func TestInterfaceMethodChangeReachesImplementorsDistinctly(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Save", "store/iface.go").WithFQN("store.Store#Save").Export(),
			captest.S(2, "PgSave", "store/pg.go").Export(),
			captest.S(3, "MemSave", "store/mem.go"),
			captest.S(4, "handler", "api/h.go"),
			captest.S(5, "job", "job/w.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithKind(model.EdgeImplements),
			captest.E(3, 1).WithKind(model.EdgeImplements),
			captest.E(4, 1), // handler calls the interface method
			captest.E(5, 2), // job calls the postgres implementation
		},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "store.Store#Save")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	// 1. The direct set is exactly the recorded callers. An implementation is
	//    not a caller and must not be listed as one.
	if got := names(rep.Direct); len(got) != 1 || got[0] != "handler" {
		t.Fatalf("Direct = %v, want [handler] only — an implementation must not be reported as a direct caller", got)
	}
	for _, list := range [][]Impacted{rep.Direct, rep.Indirect} {
		for _, imp := range list {
			if imp.Name == "PgSave" || imp.Name == "MemSave" {
				t.Fatalf("%s appears in the caller sets; implementations belong in Report.Dispatch", imp.Name)
			}
		}
	}

	// 2. Both implementations are reached, in the labelled expansion.
	pg := findDispatched(t, rep.Dispatch, "PgSave")
	mem := findDispatched(t, rep.Dispatch, "MemSave")
	for _, d := range []Dispatched{pg, mem} {
		if d.Reason != ReasonImplementsTarget {
			t.Fatalf("%s reason = %q, want %q", d.Name, d.Reason, ReasonImplementsTarget)
		}
		if d.Depth != 1 {
			t.Fatalf("%s depth = %d, want 1 (one subtype hop from the target)", d.Name, d.Depth)
		}
		if d.Via != model.EdgeImplements {
			t.Fatalf("%s via = %q, want IMPLEMENTS", d.Name, d.Via)
		}
		if d.Detail == "" {
			t.Fatalf("%s carries no detail; a dispatch row that does not state its claim reads as a caller", d.Name)
		}
		if len(d.Evidence) == 0 {
			t.Fatalf("%s carries no evidence for the subtype edge", d.Name)
		}
	}

	// 3. What the implementation reaches comes too, labelled separately again:
	//    `job` calls PgSave, and every path from the interface method to `job`
	//    crosses the IMPLEMENTS edge.
	j := findDispatched(t, rep.Dispatch, "job")
	if j.Reason != ReasonThroughImplementation {
		t.Fatalf("job reason = %q, want %q", j.Reason, ReasonThroughImplementation)
	}
	if j.Depth != 2 {
		t.Fatalf("job depth = %d, want 2 (subtype hop then call hop)", j.Depth)
	}

	// 4. The implementations are named as the hinge of the claim.
	if len(rep.DispatchThrough) != 2 {
		t.Fatalf("DispatchThrough = %v, want the two implementations", rep.DispatchThrough)
	}

	// 5. The totals keep the distinction: TotalImpacted counts recorded
	//    references only, TotalReached includes the dispatch expansion.
	if rep.TotalImpacted() != 1 {
		t.Fatalf("TotalImpacted = %d, want 1 (handler)", rep.TotalImpacted())
	}
	if rep.TotalReached() != 4 {
		t.Fatalf("TotalReached = %d, want 4 (handler + 2 implementations + job)", rep.TotalReached())
	}

	// 6. The report says what the extra section means, and the mandatory
	//    caveats are still there.
	if !rep.Completeness.HasCaveat(capability.DispatchExpansionCaveat) {
		t.Fatalf("a report with a dispatch expansion does not carry DispatchExpansionCaveat: %v", rep.Completeness.Caveats)
	}
	if !rep.Completeness.HasCaveat(capability.LowerBoundCaveat) ||
		!rep.Completeness.HasCaveat(capability.DynamicDispatchCaveat) {
		t.Fatal("the mandatory caveats went missing from a report with a dispatch expansion")
	}

	// 7. The expansion feeds the risk score, with its work shown.
	f, ok := factor(rep.Risk, "dispatch_reach")
	if !ok {
		t.Fatalf("dispatch expansion did not reach the risk score: %+v", rep.Risk.Factors)
	}
	if f.Evidence == "" {
		t.Fatal("dispatch_reach factor names no implementation")
	}
}

// TestDispatchExpansionIsAbsentWithoutAnImplementsEdge is the other half of the
// claim. The expansion must be driven by a recorded edge and by nothing else —
// no name matching, no "these two symbols look like an interface pair". With no
// IMPLEMENTS edge in the fixture there is no dispatch section at all, and the
// caller sets are unchanged from what they always were.
func TestDispatchExpansionIsAbsentWithoutAnImplementsEdge(t *testing.T) {
	t.Parallel()
	// The same shape as the fixture above with the two IMPLEMENTS edges
	// removed. PgSave still looks like an implementation of Save by name and by
	// package; nothing may be inferred from that.
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Save", "store/iface.go").WithFQN("store.Store#Save").Export(),
			captest.S(2, "PgSave", "store/pg.go").Export(),
			captest.S(3, "handler", "api/h.go"),
			captest.S(4, "job", "job/w.go"),
		},
		[]captest.Edge{
			captest.E(3, 1), // handler calls the interface method
			captest.E(4, 2), // job calls PgSave, which implements nothing recorded
		},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "store.Store#Save")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(rep.Dispatch) != 0 {
		t.Fatalf("Dispatch = %v, want empty: no IMPLEMENTS edge exists to expand through", dispatchNames(rep.Dispatch))
	}
	if len(rep.DispatchThrough) != 0 {
		t.Fatalf("DispatchThrough = %v, want empty", rep.DispatchThrough)
	}
	if got := names(rep.Direct); len(got) != 1 || got[0] != "handler" {
		t.Fatalf("Direct = %v, want [handler]", got)
	}
	if len(rep.Indirect) != 0 {
		t.Fatalf("Indirect = %v, want empty", names(rep.Indirect))
	}
	if _, ok := factor(rep.Risk, "dispatch_reach"); ok {
		t.Fatal("dispatch_reach scored on a report with no dispatch expansion")
	}
	// The caveat that explains a dispatch section must not appear when there is
	// no dispatch section — a caveat present when it does not apply is one a
	// reader learns to skip.
	if rep.Completeness.HasCaveat(capability.DispatchExpansionCaveat) {
		t.Fatal("DispatchExpansionCaveat carried by a report that expanded nothing")
	}
	if !rep.Completeness.HasCaveat(capability.DynamicDispatchCaveat) {
		t.Fatal("the dynamic-dispatch caveat is mandatory whether or not anything was expanded")
	}
}

// TestDispatchExpansionRespectsTheConfidenceFloor: a weak IMPLEMENTS edge is
// excluded from the default walk exactly like a weak CALLS edge, and is counted
// in LowConfidenceHidden rather than dropped.
func TestDispatchExpansionRespectsTheConfidenceFloor(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Save", "store/iface.go").Export(),
			captest.S(2, "PgSave", "store/pg.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithKind(model.EdgeImplements).WithConf(model.ConfidenceLow),
		},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "Save")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(rep.Dispatch) != 0 {
		t.Fatalf("a low-confidence IMPLEMENTS edge was expanded by default: %v", dispatchNames(rep.Dispatch))
	}
	if rep.LowConfidenceHidden != 1 {
		t.Fatalf("LowConfidenceHidden = %d, want 1: the floor must state what it hid", rep.LowConfidenceHidden)
	}

	// Asking for the weak edges brings the implementation back, still labelled
	// as dispatch and still carrying its own weak confidence.
	rep, err = AnalyzeWith(context.Background(), deps, runID, "Save",
		Options{MinConfidence: model.ConfidenceLow})
	if err != nil {
		t.Fatalf("AnalyzeWith: %v", err)
	}
	pg := findDispatched(t, rep.Dispatch, "PgSave")
	if pg.Confidence != model.ConfidenceLow {
		t.Fatalf("PgSave confidence = %q, want low — the subtype edge's own strength", pg.Confidence)
	}
}
