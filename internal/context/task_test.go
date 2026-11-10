package context

import (
	"reflect"
	"testing"
)

func TestParseTaskExtractsWeightedConcepts(t *testing.T) {
	in := ParseTask(goldenTask)

	got := map[string]float64{}
	for _, term := range in.Terms {
		got[term.Text] = term.Weight
	}

	for _, want := range []string{"idempotency", "payment", "callback", "processing"} {
		if got[want] != weightNoun {
			t.Errorf("term %q weight = %v, want %v (domain noun)", want, got[want], weightNoun)
		}
	}
	if got["add"] != weightVerb {
		t.Errorf("term \"add\" weight = %v, want %v (task verb, demoted not dropped)", got["add"], weightVerb)
	}
	if _, ok := got["to"]; ok {
		t.Error("stop word \"to\" survived extraction")
	}
	if !reflect.DeepEqual(in.Verbs, []string{"add"}) {
		t.Errorf("Verbs = %v, want [add]", in.Verbs)
	}
}

// TestParseTaskPrefersWrittenIdentifiers checks the strongest signal a task can
// carry: the author typing a name in code form.
func TestParseTaskPrefersWrittenIdentifiers(t *testing.T) {
	in := ParseTask("fix TransactionRepository.save so it does not double-write")

	byText := map[string]Term{}
	for _, term := range in.Terms {
		byText[term.Text] = term
	}
	if byText["transactionrepository"].Weight != weightIdentifier {
		t.Errorf("identifier weight = %v, want %v", byText["transactionrepository"].Weight, weightIdentifier)
	}
	// The parts must also be present, or "TransactionRepository" would find the
	// class and nothing else it collaborates with.
	for _, part := range []string{"transaction", "repository"} {
		if _, ok := byText[part]; !ok {
			t.Errorf("identifier part %q missing from terms %v", part, termTexts(in))
		}
	}
	if !contains(in.Identifiers, "TransactionRepository") {
		t.Errorf("Identifiers = %v, want it to include TransactionRepository", in.Identifiers)
	}
}

func TestParseTaskLiftsQuotedNames(t *testing.T) {
	in := ParseTask(`the column "merchant_id" is never set`)

	byText := map[string]Term{}
	for _, term := range in.Terms {
		byText[term.Text] = term
	}
	if byText["merchant_id"].Weight != weightIdentifier {
		t.Errorf("quoted name weight = %v, want %v", byText["merchant_id"].Weight, weightIdentifier)
	}
}

// TestParseTaskSurvivesStrayQuote guards a failure mode that silently deletes
// half a task: an apostrophe treated as an opening quote swallows the rest of
// the sentence.
func TestParseTaskSurvivesStrayQuote(t *testing.T) {
	in := ParseTask("don't dedupe callbacks in the notification service")

	for _, want := range []string{"callback", "notification", "service"} {
		found := false
		for _, term := range in.Terms {
			if term.Text == want || term.Stem == want {
				found = true
			}
		}
		if !found {
			t.Errorf("term %q lost after a stray apostrophe; terms = %v", want, termTexts(in))
		}
	}
}

func TestParseTaskIsEmptyForStopWordsOnly(t *testing.T) {
	if in := ParseTask("it is not for we"); !in.Empty() {
		t.Errorf("Empty() = false for a stop-word task; terms = %v", termTexts(in))
	}
}

func TestStemBridgesProseAndCode(t *testing.T) {
	cases := map[string]string{
		"processing":   "process",
		"transactions": "transaction",
		"events":       "event",
		"retries":      "retry",
		// Left alone: stripping these invents words that match nothing.
		"status":      "",
		"idempotency": "",
		"has":         "",
	}
	for word, want := range cases {
		if got := stem(word); got != want {
			t.Errorf("stem(%q) = %q, want %q", word, got, want)
		}
	}
}

func TestNameSignalRanksWholeNameOverPart(t *testing.T) {
	terms := ParseTask("callback handling").Terms

	whole, _ := nameSignal("callback", terms)
	part, _ := nameSignal("handleCallback", terms)
	none, _ := nameSignal("MarketingReport", terms)

	if !(whole > part) {
		t.Errorf("whole-name match %.3f should beat part match %.3f", whole, part)
	}
	if part <= 0 {
		t.Error("handleCallback should match the term \"callback\" through its identifier parts")
	}
	if none != 0 {
		t.Errorf("MarketingReport scored %.3f against an unrelated task", none)
	}
}

func TestIsTestNameAndSubject(t *testing.T) {
	cases := map[string]string{
		"CallbackControllerTest": "CallbackController",
		"TransactionServiceTest": "TransactionService",
		"UserServiceSpec":        "UserService",
		"TestPayments":           "Payments",
		"CallbackController":     "",
	}
	for name, want := range cases {
		if got := testSubject(name); got != want {
			t.Errorf("testSubject(%q) = %q, want %q", name, got, want)
		}
		if want != "" && !isTestName(name) {
			t.Errorf("isTestName(%q) = false", name)
		}
	}
}
