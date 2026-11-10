package impact

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// floorFixture is one target reached over one edge of each confidence tier,
// plus a symbol that can only be reached THROUGH a low edge.
//
//	strong  --high-->   target
//	medium  --medium--> target
//	weak    --low-->    target
//	distant --low-->    strong --high--> target
//
// The last one is the case a depth-1-only accounting would miss: `distant`
// calls a symbol that really does call the target, so it is not itself a weak
// edge — every PATH from it crosses one.
func buildFloorFixture(t *testing.T) (capability.Deps, int64) {
	t.Helper()
	return captest.Build(t,
		[]captest.Sym{
			captest.S(1, "target", "svc/status.go").Export(),
			captest.S(2, "strong", "svc/strong.go"),
			captest.S(3, "medium", "svc/medium.go"),
			captest.S(4, "weak", "svc/weak.go"),
			captest.S(5, "distant", "svc/distant.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithConf(model.ConfidenceHigh),
			captest.E(3, 1).WithConf(model.ConfidenceMedium),
			captest.E(4, 1).WithConf(model.ConfidenceLow),
			captest.E(5, 2).WithConf(model.ConfidenceLow),
		},
		realisticRefs)
}

// impactedNames collects every symbol the report claims is affected, at any
// depth, so a test can assert on the whole reported blast radius at once.
func impactedNames(r Report) []string {
	out := append([]string(nil), names(r.Direct)...)
	return append(out, names(r.Indirect)...)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestLowConfidenceEdgesAreFlooredByDefault is the whole change in one test.
//
// docs/accuracy.md measures `low` at 0.0-2.8% precision, so a default report
// built on those edges is wrong about roughly 97 of every 100 of them. The
// default therefore floors them out — and because filtering that a reader
// cannot see is its own dishonesty, the same report has to say how many it
// held back, and asking for them explicitly has to bring every one of them
// back.
func TestLowConfidenceEdgesAreFlooredByDefault(t *testing.T) {
	t.Parallel()
	deps, runID := buildFloorFixture(t)
	ctx := context.Background()

	// (a) The low edge is not in a default report.
	def, err := Analyze(ctx, deps, runID, "target")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got := impactedNames(def); contains(got, "weak") || contains(got, "distant") {
		t.Fatalf("default report = %v; a `low` edge (0.0-2.8%% precision) was traversed by default", got)
	}
	if !contains(impactedNames(def), "strong") || !contains(impactedNames(def), "medium") {
		t.Fatalf("default report = %v, want the high and medium callers kept", impactedNames(def))
	}
	if def.ConfidenceFloor != model.ConfidenceMedium {
		t.Fatalf("ConfidenceFloor = %q, want medium: Analyze must route through withDefaults", def.ConfidenceFloor)
	}

	// (b) The report counts what the floor hid, rather than shrinking in
	// silence. Both `weak` (a low edge) and `distant` (reachable only across
	// one) are held back, so a depth-1-only count would report 1 here.
	if def.LowConfidenceHidden != 2 {
		t.Fatalf("LowConfidenceHidden = %d, want 2 (weak, distant); the floor must be counted, not silent",
			def.LowConfidenceHidden)
	}

	// (c) An explicit low floor brings them back. This is a default, never a
	// deletion: the edges are still in the index.
	low, err := AnalyzeWith(ctx, deps, runID, "target", Options{MinConfidence: model.ConfidenceLow})
	if err != nil {
		t.Fatalf("AnalyzeWith(low): %v", err)
	}
	for _, want := range []string{"strong", "medium", "weak", "distant"} {
		if !contains(impactedNames(low), want) {
			t.Fatalf("MinConfidence=low report = %v, missing %q: an explicit floor must be honoured",
				impactedNames(low), want)
		}
	}
	if low.LowConfidenceHidden != 0 {
		t.Fatalf("LowConfidenceHidden = %d with the weakest floor, want 0: nothing is excluded",
			low.LowConfidenceHidden)
	}
	if got := find(t, low.Indirect, "distant").Confidence; got != model.ConfidenceLow {
		t.Fatalf("distant confidence = %q, want low: the path crossed a low edge", got)
	}
}

// TestFloorIsStatedInTheReport: the count alone is a number without a remedy.
// The report has to name the filter, size it, and say how to undo it, in the
// Unknown list the MCP layer already renders.
func TestFloorIsStatedInTheReport(t *testing.T) {
	t.Parallel()
	deps, runID := buildFloorFixture(t)

	rep, err := Analyze(context.Background(), deps, runID, "target")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	u, ok := findUnknown(rep, UnknownBelowConfidenceFloor)
	if !ok {
		t.Fatalf("no below_confidence_floor unknown; the floor filtered %d symbol(s) silently. got %+v",
			rep.LowConfidenceHidden, rep.Unknown)
	}
	if u.Count != int64(rep.LowConfidenceHidden) {
		t.Fatalf("unknown count = %d but LowConfidenceHidden = %d", u.Count, rep.LowConfidenceHidden)
	}
	if !strings.Contains(u.Detail, "low") {
		t.Fatalf("unknown does not tell the reader how to get the hidden edges back: %q", u.Detail)
	}
	if !strings.Contains(u.Detail, string(model.ConfidenceMedium)) {
		t.Fatalf("unknown does not state the floor that was applied: %q", u.Detail)
	}
}

// TestNoFloorUnknownWhenNothingIsHidden: a filter that excluded nothing is not
// a gap, and reporting one on every clean answer would train readers to skip
// the unknowns that do matter.
func TestNoFloorUnknownWhenNothingIsHidden(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "target", "a.go"),
			captest.S(2, "caller", "b.go"),
		},
		[]captest.Edge{captest.E(2, 1).WithConf(model.ConfidenceHigh)},
		realisticRefs)

	rep, err := Analyze(context.Background(), deps, runID, "target")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.LowConfidenceHidden != 0 {
		t.Fatalf("LowConfidenceHidden = %d over an all-high fixture", rep.LowConfidenceHidden)
	}
	if u, ok := findUnknown(rep, UnknownBelowConfidenceFloor); ok {
		t.Fatalf("below_confidence_floor unknown emitted with nothing hidden: %q", u.Detail)
	}
}

// TestExplicitFloorIsHonouredAtEveryTier walks the whole ladder. The default is
// one row of this table, not a special case, and each row states both what is
// reported and what was held back — the two numbers have to add up to the same
// graph however the floor moves.
func TestExplicitFloorIsHonouredAtEveryTier(t *testing.T) {
	t.Parallel()
	deps, runID := buildFloorFixture(t)

	cases := []struct {
		name       string
		opts       Options
		wantNames  []string
		wantHidden int
	}{
		{
			name:       "unset uses the medium default",
			opts:       Options{},
			wantNames:  []string{"strong", "medium"},
			wantHidden: 2,
		},
		{
			name:       "explicit low admits everything",
			opts:       Options{MinConfidence: model.ConfidenceLow},
			wantNames:  []string{"strong", "medium", "weak", "distant"},
			wantHidden: 0,
		},
		{
			name:       "explicit medium matches the default",
			opts:       Options{MinConfidence: model.ConfidenceMedium},
			wantNames:  []string{"strong", "medium"},
			wantHidden: 2,
		},
		{
			name:       "high excludes medium too",
			opts:       Options{MinConfidence: model.ConfidenceHigh},
			wantNames:  []string{"strong"},
			wantHidden: 3,
		},
		{
			name:       "exact keeps nothing this fixture has",
			opts:       Options{MinConfidence: model.ConfidenceExact},
			wantNames:  nil,
			wantHidden: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rep, err := AnalyzeWith(context.Background(), deps, runID, "target", tc.opts)
			if err != nil {
				t.Fatalf("AnalyzeWith: %v", err)
			}
			got := impactedNames(rep)
			if len(got) != len(tc.wantNames) {
				t.Fatalf("impacted = %v, want %v", got, tc.wantNames)
			}
			for _, want := range tc.wantNames {
				if !contains(got, want) {
					t.Fatalf("impacted = %v, want %v", got, tc.wantNames)
				}
			}
			if rep.LowConfidenceHidden != tc.wantHidden {
				t.Fatalf("LowConfidenceHidden = %d, want %d", rep.LowConfidenceHidden, tc.wantHidden)
			}
			// Reported plus hidden is the same graph at every floor: moving the
			// floor must move symbols between the two columns, never lose them.
			if total := len(got) + rep.LowConfidenceHidden; total != 4 {
				t.Fatalf("reported %d + hidden %d = %d, want 4: the floor lost a symbol entirely",
					len(got), rep.LowConfidenceHidden, total)
			}
		})
	}
}

// TestWithDefaultsIsTheOnlyPlaceTheFloorIsDecided pins the mechanism the change
// rests on: Analyze delegates to AnalyzeWith(Options{}), so one function
// decides the default for every caller. A second default introduced elsewhere
// would make this test disagree with the report above.
func TestWithDefaultsIsTheOnlyPlaceTheFloorIsDecided(t *testing.T) {
	t.Parallel()
	if got := (Options{}).withDefaults().MinConfidence; got != DefaultMinConfidence {
		t.Fatalf("withDefaults leaves MinConfidence = %q, want %q", got, DefaultMinConfidence)
	}
	if DefaultMinConfidence != model.ConfidenceMedium {
		t.Fatalf("DefaultMinConfidence = %q, want medium: `low` is 0.0-2.8%% precise (docs/accuracy.md)",
			DefaultMinConfidence)
	}
	// An explicitly chosen floor is never overwritten by the default.
	for _, c := range []model.Confidence{
		model.ConfidenceLow, model.ConfidenceMedium, model.ConfidenceHigh, model.ConfidenceExact,
	} {
		if got := (Options{MinConfidence: c}).withDefaults().MinConfidence; got != c {
			t.Fatalf("withDefaults turned an explicit %q into %q", c, got)
		}
	}
}
