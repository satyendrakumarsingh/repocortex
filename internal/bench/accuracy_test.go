package bench

import (
	"math"
	"strings"
	"testing"
)

// approx reports whether two rates agree to within a rounding step. Rates are
// compared, never asserted exactly, because they are ratios of counts and an
// exact-equality assertion on a float pins the compiler's arithmetic rather
// than the measurement.
func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.6f, want %.6f", what, got, want)
	}
}

// TestAccuracyArithmetic pins precision, recall and F1 over a hand-computed
// call-site set, including the one thing precision alone cannot express: a
// missed call site is in the recall denominator and in no tier's numerator.
//
//	go   high    5 right, 1 wrong  → P 5/6, R 5/10
//	     medium  1 right, 1 wrong  → P 1/2, R 1/10
//	     low     0 right, 1 wrong  → P 0/1, R 0/10
//	     missed  1                 → in the denominator, in no numerator
//	     all     6 right, 3 wrong  → P 6/9, R 6/10
func TestAccuracyArithmetic(t *testing.T) {
	langs, overall := AccuracyOf([]CallSiteOutcome{
		{Language: "go", Tier: "high", Correct: true, Count: 5},
		{Language: "go", Tier: "high", Correct: false, Count: 1},
		{Language: "go", Tier: "medium", Correct: true, Count: 1},
		{Language: "go", Tier: "medium", Correct: false, Count: 1},
		{Language: "go", Tier: "low", Correct: false, Count: 1},
		{Language: "go", Tier: "", Correct: false, Count: 1},
	})
	if len(langs) != 1 || langs[0].Language != "go" {
		t.Fatalf("languages = %+v, want one go row", langs)
	}
	got := langs[0]

	if got.Adjudicated != 10 {
		t.Errorf("adjudicated = %d, want 10 (the missed call site counts)", got.Adjudicated)
	}
	if got.Missed != 1 || got.Answered != 9 {
		t.Errorf("missed/answered = %d/%d, want 1/9", got.Missed, got.Answered)
	}

	high := tier(t, got, "high")
	approx(t, high.Precision, 5.0/6.0, "high precision")
	approx(t, high.Recall, 0.5, "high recall")
	approx(t, high.F1, 2*(5.0/6.0)*0.5/((5.0/6.0)+0.5), "high f1")
	if high.Relevant != 10 {
		t.Errorf("high relevant = %d, want the whole adjudicated set (10)", high.Relevant)
	}

	medium := tier(t, got, "medium")
	approx(t, medium.Precision, 0.5, "medium precision")
	approx(t, medium.Recall, 0.1, "medium recall")

	low := tier(t, got, "low")
	approx(t, low.Precision, 0, "low precision")
	approx(t, low.Recall, 0, "low recall")
	approx(t, low.F1, 0, "low f1")
	if !low.PrecisionMeasured {
		t.Error("low answered one call site, so its precision IS measured (and is 0)")
	}

	approx(t, got.All.Precision, 6.0/9.0, "all precision")
	approx(t, got.All.Recall, 0.6, "all recall")

	// The property that makes the tiers comparable at all: one shared recall
	// denominator, so the tier recalls sum to the all-tiers recall.
	sum := high.Recall + medium.Recall + low.Recall
	approx(t, sum, got.All.Recall, "tier recalls summed")

	// A single language and the aggregate must agree, or one of them is lying.
	approx(t, overall.All.Recall, got.All.Recall, "overall recall")
	if overall.Adjudicated != got.Adjudicated || overall.Missed != got.Missed {
		t.Errorf("overall = %d/%d, want the single language's %d/%d",
			overall.Adjudicated, overall.Missed, got.Adjudicated, got.Missed)
	}
}

// TestAccuracyRecallDenominatorIncludesMisses is the whole point of A7 in one
// assertion. A resolver that answers one call site correctly and stays silent
// on ninety-nine scores perfect precision; recall must refuse to.
func TestAccuracyRecallDenominatorIncludesMisses(t *testing.T) {
	_, overall := AccuracyOf([]CallSiteOutcome{
		{Language: "go", Tier: "high", Correct: true, Count: 1},
		{Language: "go", Tier: "", Correct: false, Count: 99},
	})
	high := tier(t, overall, "high")
	approx(t, high.Precision, 1, "precision of a silent resolver")
	approx(t, high.Recall, 0.01, "recall of a silent resolver")
	if high.F1 >= 0.03 {
		t.Errorf("f1 = %.4f, want F1 to collapse when recall does", high.F1)
	}
}

// TestAccuracyUnmeasuredLanguageIsNotZero guards the one failure a trust report
// cannot have. A language nothing adjudicated must be present and marked, never
// dropped and never rendered as a rate.
func TestAccuracyUnmeasuredLanguageIsNotZero(t *testing.T) {
	langs, _ := AccuracyOf([]CallSiteOutcome{
		{Language: "go", Tier: "high", Correct: true, Count: 4},
	})
	if len(langs) != 1 {
		t.Fatalf("languages = %+v", langs)
	}
	// A language with no outcomes at all never reaches this function; the CLI
	// carries it separately. What this pins is the rendering contract for a
	// zero-relevant row, which is what such a language becomes.
	empty := LanguageAccuracy{Language: "typescript", Tiers: []TierAccuracy{
		tierAccuracy("high", 0, 0, 0),
	}}
	if empty.Tiers[0].Measured {
		t.Error("a tier with nothing adjudicated must not report itself measured")
	}
	if got := Rate(empty.Tiers[0].Recall, empty.Tiers[0].Measured); got != "—" {
		t.Errorf("unmeasured rate rendered as %q, want a dash — 0%% reads as 'measured and wrong'", got)
	}
	if got := Rate(0, true); got != "0.0%" {
		t.Errorf("a measured zero rendered as %q, want 0.0%%", got)
	}
}

// TestAccuracyIsDeterministic pins the package's determinism rule: the report
// is sorted by weight then name, so two runs over the same facts diff clean
// whatever order the rows arrived in.
func TestAccuracyIsDeterministic(t *testing.T) {
	forward := []CallSiteOutcome{
		{Language: "go", Tier: "high", Correct: true, Count: 10},
		{Language: "python", Tier: "high", Correct: true, Count: 30},
		{Language: "typescript", Tier: "low", Correct: false, Count: 30},
	}
	reversed := []CallSiteOutcome{forward[2], forward[1], forward[0]}

	a, _ := AccuracyOf(forward)
	b, _ := AccuracyOf(reversed)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Language != b[i].Language {
			t.Fatalf("row %d: %q vs %q — input order reached the output", i, a[i].Language, b[i].Language)
		}
	}
	// python and typescript tie on 30; the tie breaks by name, and go is last.
	want := []string{"python", "typescript", "go"}
	for i, lang := range want {
		if a[i].Language != lang {
			t.Errorf("row %d = %q, want %q", i, a[i].Language, lang)
		}
	}
}

// TestAccuracyRestrictionIsStatedInline guards the soundness argument against
// being reduced to a documentation cross-reference. The number travels; the
// justification has to travel with it.
func TestAccuracyRestrictionIsStatedInline(t *testing.T) {
	for _, must := range []string{"call sites", "exactly one definition", "superset"} {
		if !strings.Contains(Restriction, must) {
			t.Errorf("Restriction does not mention %q:\n%s", must, Restriction)
		}
	}
	if len(NotMeasured) == 0 {
		t.Error("NotMeasured is empty: a measurement that never says what it excludes invites the reading it excludes")
	}
}

func tier(t *testing.T, la LanguageAccuracy, name string) TierAccuracy {
	t.Helper()
	for _, row := range la.Tiers {
		if row.Tier == name {
			return row
		}
	}
	t.Fatalf("no %q tier in %+v", name, la.Tiers)
	return TierAccuracy{}
}
