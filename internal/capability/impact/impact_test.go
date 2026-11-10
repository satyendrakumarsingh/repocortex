package impact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// realisticRefs gives every fixture a non-zero resolution picture, so a report's
// completeness block is exercised with real numbers rather than a 0/0 that
// would hide a formatting bug.
var realisticRefs = captest.Refs{Resolved: 80, Ambiguous: 5, Unresolved: 12, Pending: 3}

func names(in []Impacted) []string {
	out := make([]string, len(in))
	for i, imp := range in {
		out[i] = imp.Name
	}
	return out
}

func find(t *testing.T, in []Impacted, name string) Impacted {
	t.Helper()
	for _, imp := range in {
		if imp.Name == name {
			return imp
		}
	}
	t.Fatalf("%q not found in %v", name, names(in))
	return Impacted{}
}

// TestDirectIndirectUnknownAreSeparated is the core §20 requirement: a depth-1
// caller and a depth-3 one are different claims and must not share a list.
func TestDirectIndirectUnknownAreSeparated(t *testing.T) {
	t.Parallel()
	// far -> mid -> near -> target  (an edge A->B means A calls B, so impact
	// walks incoming).
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "target", "svc/status.go").Export(),
			captest.S(2, "near", "svc/near.go"),
			captest.S(3, "mid", "svc/mid.go"),
			captest.S(4, "far", "svc/far.go"),
			captest.S(5, "unrelated", "other/x.go"),
		},
		[]captest.Edge{captest.E(2, 1), captest.E(3, 2), captest.E(4, 3)},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "target")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if got := names(rep.Direct); len(got) != 1 || got[0] != "near" {
		t.Fatalf("Direct = %v, want [near]", got)
	}
	if got := names(rep.Indirect); len(got) != 2 || got[0] != "mid" || got[1] != "far" {
		t.Fatalf("Indirect = %v, want [mid far]", got)
	}
	if d := find(t, rep.Indirect, "far").Depth; d != 3 {
		t.Fatalf("far depth = %d, want 3", d)
	}
	// The unrelated symbol must not appear anywhere.
	for _, list := range [][]Impacted{rep.Direct, rep.Indirect} {
		for _, imp := range list {
			if imp.Name == "unrelated" {
				t.Fatal("unrelated symbol reported as impacted")
			}
		}
	}
	// UNKNOWN is a populated third category, never silently empty: the run's
	// resolution gap alone guarantees an entry.
	if _, ok := findUnknown(rep, UnknownUnresolvedReferences); !ok {
		t.Fatalf("no unresolved-references unknown; got %+v", rep.Unknown)
	}
}

// TestConfidenceCompoundsAlongPath is the assertion that matters most. A path
// whose middle hop is low must report low at the far end, no matter how strong
// the last hop was. Reporting `high` here would be the exact overclaim that
// makes an impact tool dangerous.
//
// The floor is lowered explicitly because this fixture is built out of the
// edges the default floor excludes. That is the point of the test — the path
// arithmetic has to stay correct for the weak edges too, since a caller can
// still ask for them — and it is a separate claim from which edges a default
// report walks (TestLowConfidenceEdgesAreFlooredByDefault).
func TestConfidenceCompoundsAlongPath(t *testing.T) {
	t.Parallel()
	// far --high--> mid --low--> near --high--> target
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "target", "svc/status.go"),
			captest.S(2, "near", "svc/near.go"),
			captest.S(3, "mid", "svc/mid.go"),
			captest.S(4, "far", "svc/far.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithConf(model.ConfidenceHigh),
			captest.E(3, 2).WithConf(model.ConfidenceLow),
			captest.E(4, 3).WithConf(model.ConfidenceHigh),
		},
		realisticRefs)

	rep, err := AnalyzeWith(context.Background(), deps, runID, "target",
		Options{MinConfidence: model.ConfidenceLow})
	if err != nil {
		t.Fatalf("AnalyzeWith: %v", err)
	}

	if got := find(t, rep.Direct, "near").Confidence; got != model.ConfidenceHigh {
		t.Fatalf("near confidence = %q, want high", got)
	}
	if got := find(t, rep.Indirect, "mid").Confidence; got != model.ConfidenceLow {
		t.Fatalf("mid confidence = %q, want low (the hop that reached it was low)", got)
	}
	// The one that would break a naive implementation: the final hop into `far`
	// is high, but the path crossed a low edge.
	far := find(t, rep.Indirect, "far")
	if far.Confidence != model.ConfidenceLow {
		t.Fatalf("far confidence = %q, want low: path was high->low->high and a chain is only "+
			"as trustworthy as its weakest link", far.Confidence)
	}
	if far.Via != model.EdgeCalls {
		t.Fatalf("far via = %q, want CALLS", far.Via)
	}
}

// TestConfidenceMixDrivesCertaintyNotScore checks the two axes stay separate: a
// weakly-evidenced result must not be reported as a confident one.
//
// Asked with an explicit low floor: the mix this is about only exists in a
// report that admitted the weak edges, and a caller who lowers the floor is
// exactly the caller who needs to be told the answer rests on them.
func TestConfidenceMixDrivesCertaintyNotScore(t *testing.T) {
	t.Parallel()
	syms := []captest.Sym{captest.S(1, "target", "svc/status.go").Export()}
	var edges []captest.Edge
	for i := int64(2); i <= 9; i++ {
		syms = append(syms, captest.S(i, "caller"+string(rune('a'+i-2)), "svc/c.go"))
		edges = append(edges, captest.E(i, 1).WithConf(model.ConfidenceLow))
	}
	deps, runID := captest.Build(t, syms, edges, realisticRefs)

	rep, err := AnalyzeWith(context.Background(), deps, runID, "target",
		Options{MinConfidence: model.ConfidenceLow})
	if err != nil {
		t.Fatalf("AnalyzeWith: %v", err)
	}
	if rep.Risk.Certainty != "low" {
		t.Fatalf("certainty = %q, want low: every impacted symbol was reached through a low-confidence edge",
			rep.Risk.Certainty)
	}
	if rep.Risk.CertaintyReason == "" {
		t.Fatal("certainty reported without a reason")
	}
	if !strings.Contains(rep.Risk.CertaintyReason, "8") {
		t.Fatalf("certainty reason does not state the counts it was derived from: %q", rep.Risk.CertaintyReason)
	}
}

// TestTruncationIsReportedNotSilent: a bound that quietly returns a partial
// answer is worse than an error, because nothing downstream can tell.
func TestTruncationIsReportedNotSilent(t *testing.T) {
	t.Parallel()
	syms := []captest.Sym{captest.S(1, "hub", "svc/hub.go").Export()}
	var edges []captest.Edge
	for i := int64(2); i <= 400; i++ {
		syms = append(syms, captest.S(i, "caller", "svc/callers.go"))
		edges = append(edges, captest.E(i, 1))
	}
	deps, runID := captest.Build(t, syms, edges, realisticRefs)

	rep, err := AnalyzeWith(context.Background(), deps, runID, "hub", Options{MaxNodes: 25})
	if err != nil {
		t.Fatalf("AnalyzeWith: %v", err)
	}

	if !rep.Truncated {
		t.Fatalf("Truncated = false with MaxNodes=25 over 399 callers (got %d direct)", len(rep.Direct))
	}
	if rep.TruncatedReason == "" {
		t.Fatal("truncated without a reason")
	}
	if !rep.Completeness.Truncated {
		t.Fatal("completeness block does not record the truncation")
	}
	u, ok := findUnknown(rep, UnknownUnexpandedFrontier)
	if !ok {
		t.Fatal("truncation did not produce an unexpanded_frontier unknown")
	}
	if !u.CountIsLowerBound {
		t.Fatal("frontier count is not marked as a lower bound")
	}
	// The truncation must also raise the score, not be a footnote: an
	// unmeasured region is a risk, not an absence of one.
	if _, ok := factor(rep.Risk, "truncated_traversal"); !ok {
		t.Fatalf("truncation is not a risk factor; factors = %+v", rep.Risk.Factors)
	}
	// And it must appear in the human-readable caveats.
	var found bool
	for _, c := range rep.Completeness.Caveats {
		if strings.Contains(c, "TRUNCATED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no truncation caveat in %v", rep.Completeness.Caveats)
	}
}

// TestEveryReportCarriesIncompletenessCaveat guards the requirement that a
// report can never present itself as complete.
func TestEveryReportCarriesIncompletenessCaveat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		refs captest.Refs
	}{
		{"with references", realisticRefs},
		// The dangerous case: 0/0 references could render as a reassuring
		// "100% resolved" if the caveat text were computed carelessly.
		{"no references at all", captest.Refs{}},
		{"perfectly resolved", captest.Refs{Resolved: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, runID := captest.Build(t,
				[]captest.Sym{captest.S(1, "target", "a.go"), captest.S(2, "caller", "b.go")},
				[]captest.Edge{captest.E(2, 1)}, tc.refs)

			rep, err := Analyze(context.Background(), deps, runID, "target")
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if !rep.Completeness.LowerBound {
				t.Fatal("LowerBound is false; every static answer is a floor")
			}
			if !rep.Completeness.HasCaveat(capability.LowerBoundCaveat) {
				t.Fatalf("missing the lower-bound caveat; got %v", rep.Completeness.Caveats)
			}
			if !rep.Completeness.HasCaveat(capability.DynamicDispatchCaveat) {
				t.Fatalf("missing the dynamic-dispatch caveat; got %v", rep.Completeness.Caveats)
			}
			if !rep.Risk.LowerBound {
				t.Fatal("risk score does not declare itself a lower bound")
			}
		})
	}
}

// TestUnresolvedReferencesAreAlwaysReported: the resolution gap is the single
// biggest reason an impact report under-reports, so it is never omitted.
func TestUnresolvedReferencesAreAlwaysReported(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "target", "a.go")}, nil,
		captest.Refs{Resolved: 40, Unresolved: 55, Ambiguous: 5})

	rep, err := Analyze(context.Background(), deps, runID, "target")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got := rep.Completeness.UnresolvedReferences; got != 55 {
		t.Fatalf("UnresolvedReferences = %d, want 55", got)
	}
	if got := rep.Completeness.AmbiguousReferences; got != 5 {
		t.Fatalf("AmbiguousReferences = %d, want 5", got)
	}
	u, ok := findUnknown(rep, UnknownUnresolvedReferences)
	if !ok {
		t.Fatal("no unresolved-references unknown")
	}
	if u.Count != 60 {
		t.Fatalf("unknown count = %d, want 60 (55 unresolved + 5 ambiguous)", u.Count)
	}
}

// TestCyclesTerminate: a mutually recursive cluster must not hang or
// double-count. Cycle safety lives in internal/graph; this asserts the
// capability actually inherits it rather than re-deriving reachability.
func TestCyclesTerminate(t *testing.T) {
	t.Parallel()
	// 1 <- 2 <- 3 <- 1 : a full cycle through the target.
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "target", "a.go"),
			captest.S(2, "b", "b.go"),
			captest.S(3, "c", "c.go"),
		},
		[]captest.Edge{captest.E(2, 1), captest.E(3, 2), captest.E(1, 3)},
		realisticRefs)

	rep, err := AnalyzeWith(context.Background(), deps, runID, "target", Options{MaxDepth: 10})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	seen := map[int64]int{}
	for _, list := range [][]Impacted{rep.Direct, rep.Indirect} {
		for _, imp := range list {
			seen[imp.ID]++
		}
	}
	if seen[1] != 0 {
		t.Fatalf("target reported as impacting itself %d time(s)", seen[1])
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("symbol %d appears %d times; a cycle was expanded more than once", id, n)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("got %d impacted symbols, want 2", len(seen))
	}
}

// TestRiskShowsItsInputs: a bare level is unactionable, so the factors that
// produced it must be present and must add up to the score.
func TestRiskShowsItsInputs(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "PublicThing", "svc/thing.go").Export(),
			captest.S(2, "callerA", "svc/a.go"),
			captest.S(3, "callerB", "svc/b.go"),
		},
		[]captest.Edge{captest.E(2, 1), captest.E(3, 1)},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "PublicThing")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(rep.Risk.Factors) == 0 {
		t.Fatal("risk reported with no factors: a bare level is unactionable")
	}
	sum := 0
	for _, f := range rep.Risk.Factors {
		sum += f.Points
		if f.Detail == "" {
			t.Fatalf("factor %q has no detail", f.Name)
		}
		if f.Name == "" {
			t.Fatal("factor with no name")
		}
	}
	if sum != rep.Risk.Score {
		t.Fatalf("factors sum to %d but score is %d; the score is not explained by its inputs", sum, rep.Risk.Score)
	}
	if rep.Risk.Formula == "" {
		t.Fatal("risk level reported without stating how it follows from the score")
	}

	// The specific inputs the spec calls out must all be present.
	for _, want := range []string{"public_api", "direct_callers", "indirect_reach", "observed_tests"} {
		f, ok := factor(rep.Risk, want)
		if !ok {
			t.Fatalf("risk is missing the %q input entirely", want)
		}
		if f.Evidence == "" {
			t.Fatalf("factor %q cites no evidence", want)
		}
	}
	// Exported target must be visible as a positive contributor.
	if f, _ := factor(rep.Risk, "public_api"); f.Points <= 0 {
		t.Fatalf("exported target scored %d for public_api", f.Points)
	}
	// Nothing tests it, and that must be stated rather than assumed benign.
	if f, _ := factor(rep.Risk, "observed_tests"); f.Points <= 0 {
		t.Fatalf("untested symbol scored %d for observed_tests, want a positive risk contribution", f.Points)
	}
}

// TestObservedTestsLowerTheScore proves the coverage input moves in the right
// direction, and that a test reaching the target is recognised as such.
func TestObservedTestsLowerTheScore(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Thing", "svc/thing.go").Export(),
			captest.S(2, "TestThing", "svc/thing_test.go"),
		},
		[]captest.Edge{captest.E(2, 1)}, realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "Thing")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !find(t, rep.Direct, "TestThing").IsTest {
		t.Fatal("TestThing in thing_test.go was not classified as test code")
	}
	f, ok := factor(rep.Risk, "observed_tests")
	if !ok {
		t.Fatal("no observed_tests factor")
	}
	if f.Points >= 0 {
		t.Fatalf("observed_tests scored %d with a test present, want a negative contribution", f.Points)
	}
	if f.Evidence == "" || !strings.Contains(f.Evidence, "TestThing") {
		t.Fatalf("observed_tests factor does not name the test it found: %q", f.Evidence)
	}
}

// TestDynamicDispatchIsAnUnknownNotAnAnswer: an implemented interface is a
// region of the graph static analysis cannot resolve, and it must be reported
// as such rather than omitted.
func TestDynamicDispatchIsAnUnknownNotAnAnswer(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Save", "store/pg.go").Export(),
			captest.S(2, "Store", "store/iface.go").WithKind("interface").Export(),
			captest.S(3, "handler", "api/h.go"),
			captest.S(4, "worker", "job/w.go"),
		},
		[]captest.Edge{
			// Save implements Store.
			captest.E(1, 2).WithKind(model.EdgeImplements),
			// Two symbols call the interface, not the implementation.
			captest.E(3, 2), captest.E(4, 2),
		},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "Save")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	u, ok := findUnknown(rep, UnknownDynamicDispatch)
	if !ok {
		t.Fatalf("no dynamic_dispatch unknown; got %+v", rep.Unknown)
	}
	if u.Count != 2 {
		t.Fatalf("dispatch unknown count = %d, want 2 callers of the interface", u.Count)
	}
	if len(u.Related) != 1 || u.Related[0].Name != "Store" {
		t.Fatalf("dispatch unknown does not name the interface: %+v", u.Related)
	}
	if _, ok := factor(rep.Risk, "dynamic_dispatch"); !ok {
		t.Fatal("dynamic dispatch did not feed the risk score")
	}
	// Crucially: the interface callers must NOT be claimed as impacted, because
	// nothing proves they reach this implementation.
	for _, imp := range rep.Direct {
		if imp.Name == "handler" || imp.Name == "worker" {
			t.Fatalf("%s claimed as a direct impact through dynamic dispatch", imp.Name)
		}
	}
}

// TestDirectEvidenceIsAttachedOrItsAbsenceExplained.
func TestDirectEvidenceIsAttachedOrItsAbsenceExplained(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "target", "a.go"), captest.S(2, "caller", "b.go")},
		[]captest.Edge{captest.E(2, 1)}, realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "target")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	ev := find(t, rep.Direct, "caller").Evidence
	if len(ev) == 0 {
		t.Fatal("direct caller reported without evidence")
	}
	if ev[0].FilePath == "" {
		t.Fatal("evidence carries no file path")
	}

	// Over the budget, evidence is dropped — but never silently.
	rep2, err := AnalyzeWith(context.Background(), deps, runID, "target", Options{MaxDirect: 0, MaxNodes: 10})
	if err != nil {
		t.Fatalf("AnalyzeWith: %v", err)
	}
	_ = rep2 // budget defaults to 200 when <= 0, so this path is exercised below.

	big := []captest.Sym{captest.S(1, "hub", "a.go")}
	var edges []captest.Edge
	for i := int64(2); i <= 12; i++ {
		big = append(big, captest.S(i, "c", "b.go"))
		edges = append(edges, captest.E(i, 1))
	}
	deps2, runID2 := captest.Build(t, big, edges, realisticRefs)
	rep3, err := AnalyzeWith(context.Background(), deps2, runID2, "hub", Options{MaxDirect: 3})
	if err != nil {
		t.Fatalf("AnalyzeWith: %v", err)
	}
	if !rep3.DirectEvidenceOmitted {
		t.Fatal("evidence was dropped without saying so")
	}
	if rep3.DirectEvidenceReason == "" {
		t.Fatal("evidence omission has no stated reason")
	}
	for _, imp := range rep3.Direct {
		if len(imp.Evidence) != 0 {
			t.Fatal("evidence present despite being reported as omitted")
		}
	}
}

// TestAmbiguousTargetRefusesToGuess. Analysing the wrong `Close` is not a
// partial answer, it is a wrong one.
func TestAmbiguousTargetRefusesToGuess(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Close", "a/x.go").Export(),
			captest.S(2, "Close", "b/y.go").Export(),
		}, nil, realisticRefs)

	_, err := Analyze(context.Background(), deps, runID, "Close")
	var amb *capability.AmbiguousTargetError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want AmbiguousTargetError", err)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(amb.Candidates))
	}
	// The error must be actionable: re-asking with an FQN has to work.
	rep, err := Analyze(context.Background(), deps, runID, amb.Candidates[0].FQN)
	if err != nil {
		t.Fatalf("re-ask by FQN: %v", err)
	}
	if rep.Target.FQN != amb.Candidates[0].FQN {
		t.Fatalf("resolved %q, want %q", rep.Target.FQN, amb.Candidates[0].FQN)
	}
}

func TestUnknownTargetIsNotFound(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t, []captest.Sym{captest.S(1, "a", "a.go")}, nil, realisticRefs)
	if _, err := Analyze(context.Background(), deps, runID, "nope"); !errors.Is(err, capability.ErrTargetNotFound) {
		t.Fatalf("err = %v, want ErrTargetNotFound", err)
	}
}

// TestNoImpactIsReportedHonestly: an empty result on a partially resolved index
// must not read as "nothing depends on this".
func TestNoImpactIsReportedHonestly(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "lonely", "a.go")}, nil,
		captest.Refs{Resolved: 10, Unresolved: 90})

	rep, err := Analyze(context.Background(), deps, runID, "lonely")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.TotalImpacted() != 0 {
		t.Fatalf("expected no impact, got %d", rep.TotalImpacted())
	}
	if rep.Risk.Certainty != "low" {
		t.Fatalf("certainty = %q; an empty result on a 10%%-resolved index deserves low certainty",
			rep.Risk.Certainty)
	}
	if !strings.Contains(rep.Risk.CertaintyReason, "not used") &&
		!strings.Contains(rep.Risk.CertaintyReason, "not indexed") {
		t.Fatalf("empty result is not explained: %q", rep.Risk.CertaintyReason)
	}
}

// TestContainsEdgesAreNotImpact guards the default kind set: if CONTAINS were
// traversed, every symbol would "impact" its file and then everything in it.
func TestContainsEdgesAreNotImpact(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "target", "a.go"),
			captest.S(2, "file", "a.go").WithKind("file"),
			captest.S(3, "sibling", "a.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithKind(model.EdgeContains),
			captest.E(2, 3).WithKind(model.EdgeContains),
		}, realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "target")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.TotalImpacted() != 0 {
		t.Fatalf("CONTAINS edges leaked into impact: %v / %v", names(rep.Direct), names(rep.Indirect))
	}
}

// TestContextCancellationPropagates: an abandoned agent request must not keep
// walking a 500k-edge graph.
func TestContextCancellationPropagates(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "target", "a.go")}, nil, realisticRefs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Analyze(ctx, deps, runID, "target"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func factor(r Risk, name string) (Factor, bool) {
	for _, f := range r.Factors {
		if f.Name == name {
			return f, true
		}
	}
	return Factor{}, false
}
