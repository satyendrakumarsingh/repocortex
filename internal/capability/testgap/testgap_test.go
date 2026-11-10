package testgap

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

var realisticRefs = captest.Refs{Resolved: 80, Ambiguous: 5, Unresolved: 12, Pending: 3}

// blastRadius builds the fixture every test here uses.
//
//	covered   --CALLS--> Update        (depth 1, and TestCovered calls it)
//	naked     --CALLS--> Update        (depth 1, exported, nothing tests it)
//	deep      --CALLS--> naked         (depth 2, nothing tests it)
//	PgUpdate  --IMPLEMENTS--> Update   (dispatch, nothing tests it)
//
// The files are deliberately one symbol each so the co-location signal cannot
// quietly cover a symbol by sitting next to someone else's test file.
func blastRadius(tb testing.TB) (capability.Deps, int64) {
	tb.Helper()
	return captest.Build(tb,
		[]captest.Sym{
			captest.S(1, "Update", "svc/update.go").Export(),
			captest.S(2, "covered", "api/covered.go"),
			captest.S(3, "TestCovered", "api/covered_test.go"),
			captest.S(4, "naked", "api/naked.go").Export(),
			captest.S(5, "deep", "job/deep.go"),
			captest.S(6, "PgUpdate", "store/pg.go"),
		},
		[]captest.Edge{
			captest.E(2, 1),
			captest.E(3, 2),
			captest.E(4, 1),
			captest.E(5, 4),
			captest.E(6, 1).WithKind(model.EdgeImplements),
		},
		realisticRefs)
}

func names(in []Reached) []string {
	out := make([]string, len(in))
	for i, r := range in {
		out[i] = r.Name
	}
	return out
}

func find(t *testing.T, in []Reached, name string) Reached {
	t.Helper()
	for _, r := range in {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("%q not found in %v", name, names(in))
	return Reached{}
}

// TestGapReportNamesUncoveredReachedSymbolsAndRanksThem is the A36 assertion:
// the join must say how many of the reached symbols are covered, name the ones
// that are not, and order those by risk rather than by symbol id.
func TestGapReportNamesUncoveredReachedSymbolsAndRanksThem(t *testing.T) {
	t.Parallel()
	deps, runID := blastRadius(t)

	rep, err := Analyze(context.Background(), deps, runID, "Update")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	// 1. The denominator: everything the change reaches, callers and dispatch.
	if rep.Reached != 5 {
		t.Fatalf("Reached = %d, want 5 (covered, TestCovered, naked, deep, PgUpdate)", rep.Reached)
	}
	if rep.Examined != 5 || rep.NotExamined != 0 {
		t.Fatalf("Examined/NotExamined = %d/%d, want 5/0", rep.Examined, rep.NotExamined)
	}

	// 2. Covered, and covered by what. `covered` is reached from TestCovered by
	//    a recorded edge, which is the only signal that is evidence of
	//    execution.
	if rep.CoveredProven != 1 {
		t.Fatalf("CoveredProven = %d, want 1; covered = %v", rep.CoveredProven, names(rep.Covered))
	}
	c := find(t, rep.Covered, "covered")
	if c.Coverage != impact.CoverageProven {
		t.Fatalf("covered coverage = %q, want %q", c.Coverage, impact.CoverageProven)
	}
	if len(c.Tests) == 0 || c.Tests[0].Name != "TestCovered" {
		t.Fatalf("covered does not name the test that covers it: %+v", c.Tests)
	}

	// 3. The gap itself: named, not counted.
	got := names(rep.Uncovered)
	if len(got) != 3 {
		t.Fatalf("Uncovered = %v, want the three symbols nothing tests", got)
	}
	for _, want := range []string{"naked", "deep", "PgUpdate"} {
		find(t, rep.Uncovered, want)
	}
	for _, r := range rep.Uncovered {
		if r.Name == "covered" || r.Name == "TestCovered" {
			t.Fatalf("%s reported as uncovered", r.Name)
		}
		if r.Coverage != impact.CoverageNone {
			t.Fatalf("%s coverage = %q, want %q", r.Name, r.Coverage, impact.CoverageNone)
		}
	}

	// 4. A test symbol in the blast radius is not a coverage gap, and the
	//    exclusion is counted rather than silent.
	if rep.TestSymbolsReached != 1 {
		t.Fatalf("TestSymbolsReached = %d, want 1 (TestCovered)", rep.TestSymbolsReached)
	}

	// 5. Ranked by risk, descending, and every rank shows its work — the same
	//    factor machinery the impact report's own score uses.
	if got[0] != "naked" {
		t.Fatalf("Uncovered[0] = %q, want naked: exported and one hop away outranks the rest (%v)", got[0], got)
	}
	last := 101
	for _, r := range rep.Uncovered {
		if r.Risk == nil {
			t.Fatalf("%s carries no risk score, so nothing justifies its rank", r.Name)
		}
		if r.Risk.Score > last {
			t.Fatalf("Uncovered is not ranked by risk: %v", got)
		}
		last = r.Risk.Score
		if len(r.Risk.Factors) == 0 || r.Risk.Formula == "" {
			t.Fatalf("%s risk does not show its work: %+v", r.Name, r.Risk)
		}
		if !r.Risk.LowerBound {
			t.Fatalf("%s risk does not declare itself a lower bound", r.Name)
		}
		var sawTests bool
		for _, f := range r.Risk.Factors {
			if f.Name == "observed_tests" {
				sawTests = true
				if f.Points <= 0 {
					t.Fatalf("%s is uncovered but observed_tests scored %d", r.Name, f.Points)
				}
			}
		}
		if !sawTests {
			t.Fatalf("%s risk omits the observed_tests factor", r.Name)
		}
	}

	// 6. The dispatch label survives the join: an untested implementation of a
	//    changed symbol is a different finding from an untested caller, and is
	//    scored as one.
	pg := find(t, rep.Uncovered, "PgUpdate")
	if pg.Reach != impact.ReachDispatch {
		t.Fatalf("PgUpdate reach = %q, want %q", pg.Reach, impact.ReachDispatch)
	}
	var sawDispatchFactor bool
	for _, f := range pg.Risk.Factors {
		if f.Name == "dispatch_reach" {
			sawDispatchFactor = true
		}
	}
	if !sawDispatchFactor {
		t.Fatalf("PgUpdate's score does not record that it was reached by dispatch: %+v", pg.Risk.Factors)
	}
	if pg.Risk.Certainty != "low" {
		t.Fatalf("PgUpdate certainty = %q, want low: dispatch reach is possible, not proven", pg.Risk.Certainty)
	}

	// 7. Honesty block: the mandatory caveats plus the one this report needs.
	if !rep.Completeness.HasCaveat(capability.LowerBoundCaveat) ||
		!rep.Completeness.HasCaveat(capability.DynamicDispatchCaveat) {
		t.Fatalf("mandatory caveats missing: %v", rep.Completeness.Caveats)
	}
	if !rep.Completeness.HasCaveat(UncoveredCaveat) {
		t.Fatal("the gap report does not say what \"uncovered\" means")
	}
	if len(rep.Unknown) == 0 {
		t.Fatal("the impact report's unknowns were dropped on the way through the join")
	}
	if rep.ConfidenceFloor != impact.DefaultMinConfidence {
		t.Fatalf("ConfidenceFloor = %q, want the default floor %q", rep.ConfidenceFloor, impact.DefaultMinConfidence)
	}
}

// TestUnexaminedSymbolsAreNotReportedAsUncovered: when the examination budget
// runs out, the remaining symbols are "not examined", never "not covered".
// Rendering the two the same way would manufacture gaps out of a bound.
func TestUnexaminedSymbolsAreNotReportedAsUncovered(t *testing.T) {
	t.Parallel()
	deps, runID := blastRadius(t)

	rep, err := AnalyzeWith(context.Background(), deps, runID, "Update", Options{MaxExamine: 1})
	if err != nil {
		t.Fatalf("AnalyzeWith: %v", err)
	}
	if rep.Examined != 1 {
		t.Fatalf("Examined = %d, want 1", rep.Examined)
	}
	if rep.NotExamined != rep.Reached-1 {
		t.Fatalf("NotExamined = %d, want %d", rep.NotExamined, rep.Reached-1)
	}
	if len(rep.NotExaminedSymbols) != rep.NotExamined {
		t.Fatalf("NotExaminedSymbols lists %d of %d unexamined symbols",
			len(rep.NotExaminedSymbols), rep.NotExamined)
	}
	for _, r := range rep.NotExaminedSymbols {
		if r.Coverage != impact.CoverageNotExamined {
			t.Fatalf("%s coverage = %q, want %q", r.Name, r.Coverage, impact.CoverageNotExamined)
		}
		for _, u := range rep.Uncovered {
			if u.ID == r.ID {
				t.Fatalf("%s was never examined but is reported as uncovered", r.Name)
			}
		}
	}
}

// TestConventionOnlyCoverageIsNotCountedAsProven: a test matched by its NAME is
// not the same fact as a test that reaches the symbol through a recorded edge,
// and must not discharge the risk as if it were.
func TestConventionOnlyCoverageIsNotCountedAsProven(t *testing.T) {
	t.Parallel()
	// TestRename matches the naming convention for Rename and lives in a test
	// file, but no edge connects the two.
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Update", "svc/update.go").Export(),
			captest.S(2, "Rename", "api/rename.go").Export(),
			captest.S(3, "TestRename", "other/rename_test.go"),
		},
		[]captest.Edge{captest.E(2, 1)},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "Update")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.CoveredProven != 0 {
		t.Fatalf("CoveredProven = %d, want 0: no edge connects TestRename to Rename", rep.CoveredProven)
	}
	if rep.CoveredByConventionOnly != 1 {
		t.Fatalf("CoveredByConventionOnly = %d, want 1", rep.CoveredByConventionOnly)
	}
	r := find(t, rep.Covered, "Rename")
	if r.Coverage != impact.CoverageConventionOnly {
		t.Fatalf("Rename coverage = %q, want %q", r.Coverage, impact.CoverageConventionOnly)
	}
	if r.Risk == nil {
		t.Fatal("convention-only coverage carries no risk score; it is still a partial gap")
	}
	for _, f := range r.Risk.Factors {
		if f.Name == "observed_tests" && f.Points <= 0 {
			t.Fatalf("a name-matched test discharged the risk (%d points) as if it were a proven one", f.Points)
		}
	}
}
