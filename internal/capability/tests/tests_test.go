package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

var realisticRefs = captest.Refs{Resolved: 80, Ambiguous: 5, Unresolved: 12, Pending: 3}

// signalFixture is built so that each of the three signals has EXACTLY ONE test
// it alone can find:
//
//	graphOnly     svc/other_test.go     calls the target; name unrelated,
//	                                    not the sibling test file
//	namingOnly    api/naming_test.go    conventional name; no edge, other dir
//	colocOnly     svc/status_test.go    sibling test file; no edge, plain name
//
// Anything less separated would let one signal's bug hide behind another's
// success, which is the whole reason this package records which signal matched.
func signalFixture(tb testing.TB) (capability.Deps, int64) {
	return captest.Build(tb,
		[]captest.Sym{
			captest.S(1, "updateStatus", "svc/status.go").Export(),
			captest.S(2, "TestSomethingElse", "svc/other_test.go"),
			captest.S(3, "TestUpdateStatus", "api/naming_test.go"),
			captest.S(4, "helperThing", "svc/status_test.go"),
			captest.S(5, "unrelated", "misc/other.go"),
		},
		[]captest.Edge{captest.E(2, 1)},
		realisticRefs)
}

func hit(t *testing.T, r TestReport, name string) TestHit {
	t.Helper()
	for _, h := range r.Tests {
		if h.Name == name {
			return h
		}
	}
	var got []string
	for _, h := range r.Tests {
		got = append(got, h.Name)
	}
	t.Fatalf("%q not among discovered tests %v", name, got)
	return TestHit{}
}

func hasSignal(h TestHit, s Signal) bool {
	for _, m := range h.Signals {
		if m.Signal == s {
			return true
		}
	}
	return false
}

// TestEachSignalFindsTestsIndependently runs discovery with exactly one signal
// enabled at a time. A test that only ever runs all three cannot tell whether a
// signal works or whether another signal covered for it.
func TestEachSignalFindsTestsIndependently(t *testing.T) {
	t.Parallel()
	deps, runID := signalFixture(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		opts   Options
		want   string
		signal Signal
	}{
		{"graph only", Options{SkipNaming: true, SkipColocation: true}, "TestSomethingElse", SignalGraph},
		{"naming only", Options{SkipGraph: true, SkipColocation: true}, "TestUpdateStatus", SignalNaming},
		{"co-location only", Options{SkipGraph: true, SkipNaming: true}, "helperThing", SignalColocation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rep, err := FindTestsWith(ctx, deps, runID, "updateStatus", tc.opts)
			if err != nil {
				t.Fatalf("FindTestsWith: %v", err)
			}
			if len(rep.Tests) != 1 {
				var got []string
				for _, h := range rep.Tests {
					got = append(got, h.Name+"/"+string(h.Strongest))
				}
				t.Fatalf("got %d tests %v, want exactly 1 (%s)", len(rep.Tests), got, tc.want)
			}
			h := rep.Tests[0]
			if h.Name != tc.want {
				t.Fatalf("found %q, want %q", h.Name, tc.want)
			}
			if h.Strongest != tc.signal {
				t.Fatalf("%s recorded strongest signal %q, want %q", h.Name, h.Strongest, tc.signal)
			}
			if !hasSignal(h, tc.signal) {
				t.Fatalf("%s does not record the %q signal at all", h.Name, tc.signal)
			}
			// Every match must justify itself. "naming_convention" with no
			// detail is not checkable by the reader.
			if h.Signals[0].Detail == "" {
				t.Fatalf("%s matched %q with no detail", h.Name, tc.signal)
			}
			if rep.SignalCounts[tc.signal] != 1 {
				t.Fatalf("SignalCounts[%s] = %d, want 1", tc.signal, rep.SignalCounts[tc.signal])
			}
		})
	}
}

// TestAllSignalsTogetherDoNotDoubleCount: a test found three ways is one test
// with three pieces of evidence, and the strongest signal wins.
func TestAllSignalsTogetherDoNotDoubleCount(t *testing.T) {
	t.Parallel()
	// TestUpdateStatus lives in the sibling test file AND calls the target AND
	// is conventionally named — all three signals fire on one symbol.
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "updateStatus", "svc/status.go").Export(),
			captest.S(2, "TestUpdateStatus", "svc/status_test.go"),
		},
		[]captest.Edge{captest.E(2, 1)}, realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "updateStatus")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if len(rep.Tests) != 1 {
		t.Fatalf("got %d tests, want 1: the same symbol must not be counted once per signal", len(rep.Tests))
	}
	h := rep.Tests[0]
	if len(h.Signals) != 3 {
		var got []string
		for _, m := range h.Signals {
			got = append(got, string(m.Signal))
		}
		t.Fatalf("signals = %v, want all three recorded", got)
	}
	if h.Strongest != SignalGraph {
		t.Fatalf("strongest = %q, want graph", h.Strongest)
	}
	// The overall confidence must come from the strongest signal, not be
	// inflated because three weak-to-medium signals agreed.
	if h.Confidence != h.Signals[0].Confidence {
		t.Fatalf("confidence %q does not match the strongest signal's %q", h.Confidence, h.Signals[0].Confidence)
	}
	if rep.ProvenByGraph != 1 {
		t.Fatalf("ProvenByGraph = %d, want 1", rep.ProvenByGraph)
	}
}

// TestGraphSignalConfidenceCompounds: a test that reaches the target through a
// low-confidence hop is low-confidence evidence, however strong the other hops.
func TestGraphSignalConfidenceCompounds(t *testing.T) {
	t.Parallel()
	// TestThing --high--> helper --low--> setup --high--> target
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "target", "svc/a.go"),
			captest.S(2, "setup", "svc/b.go"),
			captest.S(3, "helper", "svc/c.go"),
			captest.S(4, "TestThing", "svc/thing_test.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithConf(model.ConfidenceHigh),
			captest.E(3, 2).WithConf(model.ConfidenceLow),
			captest.E(4, 3).WithConf(model.ConfidenceHigh),
		}, realisticRefs)

	rep, err := FindTestsWith(context.Background(), deps, runID, "target",
		Options{SkipNaming: true, SkipColocation: true})
	if err != nil {
		t.Fatalf("FindTestsWith: %v", err)
	}
	h := hit(t, rep, "TestThing")
	if h.Confidence != model.ConfidenceLow {
		t.Fatalf("confidence = %q, want low: the path crossed a low-confidence edge", h.Confidence)
	}
	if h.Signals[0].Depth != 3 {
		t.Fatalf("depth = %d, want 3", h.Signals[0].Depth)
	}
}

// TestNoTestsIsReportedHonestly. Returning the nearest vaguely-related test
// file would convert "we don't know" into "you're covered".
func TestNoTestsIsReportedHonestly(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "lonely", "misc/lonely.go").Export(),
			// A real test exists in the repository — for something else, in
			// another directory. It must not be offered up.
			captest.S(2, "TestSomethingCompletelyDifferent", "other/thing_test.go"),
			captest.S(3, "somethingCompletelyDifferent", "other/thing.go"),
		},
		[]captest.Edge{captest.E(2, 3)}, realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "lonely")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if len(rep.Tests) != 0 {
		var got []string
		for _, h := range rep.Tests {
			got = append(got, h.Name+" via "+string(h.Strongest))
		}
		t.Fatalf("returned %v for a symbol with no tests", got)
	}
	if !rep.Untested {
		t.Fatal("Untested is false with zero tests found")
	}
	// "We found nothing" and "we did not look" must not render identically.
	var explained bool
	for _, c := range rep.Completeness.Caveats {
		if strings.Contains(c, "No test was found") && strings.Contains(c, "does NOT mean") {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("absence of tests was not explained; caveats = %v", rep.Completeness.Caveats)
	}
}

// TestSiblingColocationDoesNotClaimTheWholeDirectory guards the deliberately
// narrow definition of co-location.
func TestSiblingColocationDoesNotClaimTheWholeDirectory(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "parseHeader", "svc/parse.go"),
			// A test in the same directory, but for a different file.
			captest.S(2, "TestFormat", "svc/format_test.go"),
			captest.S(3, "format", "svc/format.go"),
		}, nil, realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "parseHeader")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if len(rep.Tests) != 0 {
		t.Fatalf("co-location claimed %d unrelated test(s) in the same directory", len(rep.Tests))
	}
}

// TestNamingSignalDoesNotOverreach: prefix matching must not let a short name
// claim a longer symbol's test.
func TestNamingSignalDoesNotOverreach(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Update", "svc/a.go").Export(),
			captest.S(2, "TestUpdateStatus", "svc/a_test.go"),
			captest.S(3, "TestUpdate_NotFound", "svc/b_test.go"),
		}, nil, realisticRefs)

	rep, err := FindTestsWith(context.Background(), deps, runID, "Update",
		Options{SkipGraph: true, SkipColocation: true})
	if err != nil {
		t.Fatalf("FindTestsWith: %v", err)
	}
	for _, h := range rep.Tests {
		if h.Name == "TestUpdateStatus" {
			t.Fatal("TestUpdateStatus claimed as a test of Update: prefix matching is unconstrained")
		}
	}
	// The conventional subtest form must still be found.
	hit(t, rep, "TestUpdate_NotFound")
}

func TestTestKindIsClassifiedOrLeftUnknown(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "checkout", "svc/checkout.go").Export(),
			captest.S(2, "TestCheckout", "svc/checkout_test.go"),
			captest.S(3, "TestCheckoutFlow", "tests/e2e/flow_test.go"),
			captest.S(4, "TestCheckoutDB", "tests/integration/db_test.go"),
		},
		[]captest.Edge{captest.E(2, 1), captest.E(3, 1), captest.E(4, 1)},
		realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "checkout")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	want := map[string]capability.TestKind{
		"TestCheckout":     capability.TestKindUnit,
		"TestCheckoutFlow": capability.TestKindE2E,
		"TestCheckoutDB":   capability.TestKindIntegration,
	}
	for name, kind := range want {
		if got := hit(t, rep, name).Kind; got != kind {
			t.Errorf("%s classified as %q, want %q", name, got, kind)
		}
	}
}

func TestEveryTestReportCarriesIncompletenessCaveat(t *testing.T) {
	t.Parallel()
	deps, runID := signalFixture(t)
	rep, err := FindTests(context.Background(), deps, runID, "updateStatus")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if !rep.Completeness.LowerBound {
		t.Fatal("LowerBound is false")
	}
	if !rep.Completeness.HasCaveat(capability.LowerBoundCaveat) {
		t.Fatalf("missing lower-bound caveat: %v", rep.Completeness.Caveats)
	}
	if !rep.Completeness.HasCaveat(capability.DynamicDispatchCaveat) {
		t.Fatalf("missing dynamic-dispatch caveat: %v", rep.Completeness.Caveats)
	}
	if rep.Completeness.UnresolvedReferences != 12 || rep.Completeness.AmbiguousReferences != 5 {
		t.Fatalf("resolution counts not carried: %+v", rep.Completeness)
	}
}

func TestTruncationIsReported(t *testing.T) {
	t.Parallel()
	syms := []captest.Sym{captest.S(1, "hub", "svc/hub.go")}
	var edges []captest.Edge
	for i := int64(2); i <= 300; i++ {
		syms = append(syms, captest.S(i, "TestHub", "svc/hub_test.go"))
		edges = append(edges, captest.E(i, 1))
	}
	deps, runID := captest.Build(t, syms, edges, realisticRefs)

	rep, err := FindTestsWith(context.Background(), deps, runID, "hub", Options{MaxNodes: 20})
	if err != nil {
		t.Fatalf("FindTestsWith: %v", err)
	}
	if !rep.Truncated || rep.TruncatedReason == "" {
		t.Fatalf("truncation not reported: truncated=%v reason=%q", rep.Truncated, rep.TruncatedReason)
	}
	if !rep.Completeness.Truncated {
		t.Fatal("completeness block does not record the truncation")
	}
}

func TestCyclesTerminateInDiscovery(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "a", "svc/a.go"),
			captest.S(2, "b", "svc/b.go"),
			captest.S(3, "TestLoop", "svc/a_test.go"),
		},
		[]captest.Edge{captest.E(2, 1), captest.E(1, 2), captest.E(3, 1), captest.E(1, 3)},
		realisticRefs)

	rep, err := FindTestsWith(context.Background(), deps, runID, "a", Options{MaxDepth: 12})
	if err != nil {
		t.Fatalf("FindTestsWith: %v", err)
	}
	seen := map[int64]int{}
	for _, h := range rep.Tests {
		seen[h.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("symbol %d reported %d times", id, n)
		}
	}
}

// --- SuggestMissingScenarios -------------------------------------------------

func kinds(in []Suggestion) []SuggestionKind {
	out := make([]SuggestionKind, len(in))
	for i, s := range in {
		out[i] = s.Kind
	}
	return out
}

func TestSuggestsPublicAPIWithNoTests(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "PublishInvoice", "billing/pub.go").Export()},
		nil, realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "PublishInvoice")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	sugg := SuggestMissingScenarios(rep.Target, rep)
	if len(sugg) == 0 {
		t.Fatal("no suggestion for an exported symbol with no tests at all")
	}
	var got Suggestion
	for _, s := range sugg {
		if s.Kind == GapPublicAPIUntested {
			got = s
		}
	}
	if got.Kind == "" {
		t.Fatalf("kinds = %v, want public_api_untested", kinds(sugg))
	}
	if got.Evidence == "" {
		t.Fatal("suggestion emitted with no evidence")
	}
	if !strings.Contains(got.Evidence, "exported") {
		t.Fatalf("evidence does not cite the fact it rests on: %q", got.Evidence)
	}
}

func TestSuggestsUntestedErrorPathOnlyWithAProvenCallEdge(t *testing.T) {
	t.Parallel()
	// The target provably calls Errorf, and no test reaches it.
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "process", "svc/process.go").Export(),
			captest.S(2, "Errorf", "fmt/print.go").Export(),
		},
		[]captest.Edge{captest.E(1, 2)}, realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "process")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if len(rep.ErrorPaths) != 1 {
		t.Fatalf("ErrorPaths = %+v, want the recorded CALLS edge to Errorf", rep.ErrorPaths)
	}
	sugg := SuggestMissingScenarios(rep.Target, rep)
	var found *Suggestion
	for i := range sugg {
		if sugg[i].Kind == GapErrorPathUntested {
			found = &sugg[i]
		}
	}
	if found == nil {
		t.Fatalf("kinds = %v, want error_path_untested", kinds(sugg))
	}
	if !strings.Contains(found.Evidence, "CALLS edge") {
		t.Fatalf("error-path suggestion does not cite the edge it rests on: %q", found.Evidence)
	}

	// The control: the SAME name with no call edge must produce nothing. This
	// is the §23 line — a suggestion may not be generated from a keyword alone.
	deps2, runID2 := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "process", "svc/process.go").Export(),
			captest.S(2, "Errorf", "fmt/print.go").Export(),
		}, nil, realisticRefs)
	rep2, err := FindTests(context.Background(), deps2, runID2, "process")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if len(rep2.ErrorPaths) != 0 {
		t.Fatal("an error constructor with no edge to it was treated as an error path")
	}
	for _, s := range SuggestMissingScenarios(rep2.Target, rep2) {
		if s.Kind == GapErrorPathUntested {
			t.Fatal("error-path suggestion generated from a name with no call edge behind it")
		}
	}
}

func TestSuggestsWhenCoverageIsNominalOnly(t *testing.T) {
	t.Parallel()
	// A conventionally named test exists, but nothing in the graph shows it
	// reaching the target.
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "updateStatus", "svc/status.go"),
			captest.S(2, "TestUpdateStatus", "svc/status_test.go"),
		}, nil, realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "updateStatus")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if len(rep.Tests) == 0 {
		t.Fatal("naming signal found nothing")
	}
	if rep.ProvenByGraph != 0 {
		t.Fatalf("ProvenByGraph = %d, want 0", rep.ProvenByGraph)
	}
	sugg := SuggestMissingScenarios(rep.Target, rep)
	var found bool
	for _, s := range sugg {
		if s.Kind == GapNoProvenExecution {
			found = true
			if !strings.Contains(s.Evidence, "0 by a recorded edge chain") {
				t.Fatalf("evidence does not state the gap: %q", s.Evidence)
			}
		}
	}
	if !found {
		t.Fatalf("kinds = %v, want no_proven_execution", kinds(sugg))
	}
}

// TestSuggestsNothingWhenNothingIsJustified. Returning nil is a correct answer,
// and emitting a plausible-sounding scenario instead would poison every other
// suggestion in the list.
func TestSuggestsNothingWhenNothingIsJustified(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			// Unexported, and a test provably reaches it.
			captest.S(1, "updateStatus", "svc/status.go"),
			captest.S(2, "TestUpdateStatus", "svc/status_test.go"),
		},
		[]captest.Edge{captest.E(2, 1)}, realisticRefs)

	rep, err := FindTests(context.Background(), deps, runID, "updateStatus")
	if err != nil {
		t.Fatalf("FindTests: %v", err)
	}
	if got := SuggestMissingScenarios(rep.Target, rep); len(got) != 0 {
		t.Fatalf("invented %d suggestion(s) with no gap to justify them: %+v", len(got), got)
	}
}
