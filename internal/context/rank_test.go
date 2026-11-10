package context

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestWeightOrderingIsTheProduct pins the claim the package comment makes.
// Structure must outrank vocabulary; if these two ever swap, the golden test's
// decoy wins and nobody notices until a user does.
func TestWeightOrderingIsTheProduct(t *testing.T) {
	w := DefaultTuning().Weights
	switch {
	case !(w.SymbolNameMatch > w.GraphProximity):
		t.Error("a symbol the task named must outrank a symbol we inferred")
	case !(w.GraphProximity > w.Lexical):
		t.Error("graph proximity must outrank lexical similarity, or text decoys win")
	case !(w.TestRelation > w.Lexical), !(w.DataAccess > w.Lexical):
		t.Error("structural relationships must outrank text matches")
	case !(w.Lexical > w.ModuleAffinity), !(w.ModuleAffinity > w.Recency):
		t.Error("modifiers must stay below every admitting signal")
	}
}

// TestModifiersCannotAdmitAnything proves the base/modifier split is enforced
// rather than merely commented: a candidate with no evidence scores zero no
// matter how recently its file changed or how close it sits to a seed.
func TestModifiersCannotAdmitAnything(t *testing.T) {
	w := DefaultTuning().Weights

	c := &candidate{affinity: 1, recency: 1}
	c.computeScore(w)
	if c.score != 0 {
		t.Errorf("score = %.3f for a candidate with only modifier signals, want 0", c.score)
	}

	c.lexical = 0.1
	c.computeScore(w)
	if c.score <= 0 {
		t.Error("a candidate with real evidence should pick up its modifiers")
	}
}

// TestPathConfidenceDiscountsProximity checks that a chain of guesses ranks
// below a chain of proofs at the same distance.
func TestPathConfidenceDiscountsProximity(t *testing.T) {
	w := DefaultTuning().Weights

	strong := &candidate{proximity: proximity(2), depth: 2, pathConf: model.ConfidenceHigh}
	weak := &candidate{proximity: proximity(2), depth: 2, pathConf: model.ConfidenceLow}
	strong.computeScore(w)
	weak.computeScore(w)

	if !(strong.score > weak.score) {
		t.Errorf("high-confidence path %.3f should outrank low-confidence path %.3f",
			strong.score, weak.score)
	}
	if weak.score <= 0 {
		t.Error("a low-confidence path should be discounted, not erased")
	}
}

// TestSeedConfidenceIsNeverExact guards invariant 2 at the output boundary.
// graph.Traverse marks roots exact by convention ("a root is an input, not an
// inference"), and copying that into a pack would claim a compiler proved
// something no compiler ever saw.
func TestSeedConfidenceIsNeverExact(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range pack.Items {
		if it.Confidence == string(model.ConfidenceExact) {
			t.Errorf("item %s claims exact confidence, but no compiler-backed index exists: %s",
				it.Name, it.Why)
		}
		if it.Confidence != "" && !model.Confidence(it.Confidence).Valid() {
			t.Errorf("item %s carries invalid confidence %q", it.Name, it.Confidence)
		}
	}
}

// TestAdmissionGateRejectsSingleTermTextMatch is the decoy case in isolation,
// so a failure points at the gate rather than at the whole pipeline.
func TestAdmissionGateRejectsSingleTermTextMatch(t *testing.T) {
	th := DefaultTuning().Thresholds

	decoy := &candidate{lexical: 0.9, lexTerms: []string{"payment"}}
	if got := admit(decoy, th); got != admitNone {
		t.Errorf("admit(one text term) = %v, want admitNone", got)
	}

	corroborated := &candidate{lexical: 0.4, lexTerms: []string{"payment", "callback"}}
	if got := admit(corroborated, th); got != admitLexical {
		t.Errorf("admit(two text terms) = %v, want admitLexical", got)
	}

	connected := &candidate{proximity: proximity(2), depth: 2}
	if got := admit(connected, th); got != admitGraph {
		t.Errorf("admit(graph reachable) = %v, want admitGraph", got)
	}
}

// TestTuningIsConfigurable proves the weights are a policy and not a constant
// welded into the pipeline — and, by turning the gate off, shows exactly what
// the gate is buying.
func TestTuningIsConfigurable(t *testing.T) {
	reg, g, runID := paymentFixture(t)
	ctx := context.Background()

	permissive := DefaultTuning()
	permissive.MinLexicalTerms = 1
	permissive.MarginalFraction = 0
	permissive.AbsoluteFloor = 0

	loose, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask, Tuning: &permissive})
	if err != nil {
		t.Fatal(err)
	}
	leaked := false
	for _, it := range loose.Items {
		if strings.HasPrefix(it.Path, goldenMustExclude) {
			leaked = true
		}
	}
	if !leaked {
		t.Error("with the corroboration requirement relaxed the decoy should reappear; " +
			"if it does not, the golden test is passing for some other reason")
	}

	strict, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range strict.Items {
		if strings.HasPrefix(it.Path, goldenMustExclude) {
			t.Error("default tuning admitted the decoy")
		}
	}
}

func TestAffinityDecaysWithPathDistance(t *testing.T) {
	same := affinity("src/main/java/com/pay/service/A.java", "src/main/java/com/pay/service/B.java")
	sibling := affinity("src/main/java/com/pay/service/A.java", "src/main/java/com/pay/controller/B.java")
	stranger := affinity("src/main/java/com/pay/service/A.java", "unrelated/marketing/B.java")

	if same != 1 {
		t.Errorf("same directory affinity = %.3f, want 1", same)
	}
	if !(same > sibling && sibling > stranger) {
		t.Errorf("affinity should decay: same=%.3f sibling=%.3f stranger=%.3f", same, sibling, stranger)
	}
	if stranger != 0 {
		t.Errorf("unrelated tree affinity = %.3f, want 0", stranger)
	}
}
