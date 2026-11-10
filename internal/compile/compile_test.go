package compile

import (
	"errors"
	"strings"
	"testing"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// richPack is the fixture most tests work from: an endpoint, a table, a test
// file, an inferred item and enough entries to exercise the width rule.
func richPack() devctx.Pack {
	return devctx.Pack{
		Task:           "Add idempotency to payment callback processing",
		Interpretation: "Change how payment callbacks are handled so a repeated delivery is a no-op.",
		Items: []devctx.Item{
			{Kind: "method", Name: "handleCallback", Path: "src/pay/CallbackController.java", Relevance: 0.9, Why: "named by the task", Confidence: "exact"},
			{Kind: "endpoint", Name: "POST /payments/callback", Path: "src/pay/CallbackController.java", Relevance: 0.85, Why: "the entry point the task describes", Confidence: "high"},
			{Kind: "table", Name: "payment_callbacks", Path: "", Relevance: 0.7, Why: "written by the handler", Confidence: "high"},
			{Kind: "test", Name: "CallbackControllerTest", Path: "src/test/java/pay/CallbackControllerTest.java", Relevance: 0.6, Why: "covers the handler", Confidence: "high"},
			{Kind: "interface", Name: "PaymentGateway", Path: "src/pay/PaymentGateway.java", Relevance: 0.5, Why: "called on the path", Confidence: "medium"},
			{Kind: "configuration", Name: "application.yml", Path: "src/main/resources/application.yml", Relevance: 0.4, Why: "holds the retry window", Confidence: "medium"},
			{Kind: "function", Name: "retryLoop", Path: "src/pay/Retry.java", Relevance: 0.3, Why: "name matched a task term", Confidence: "low"},
		},
		Excluded: []devctx.Exclusion{
			{Name: "MarketingReport", Path: "unrelated/marketing/MarketingReport.java", Reason: "shares no dependency edge with the callback flow"},
		},
		Notes: []string{"traversal stopped at depth 3"},
	}
}

func compileOrFail(t *testing.T, intent string, pack devctx.Pack, opts Options) Contract {
	t.Helper()
	c, err := Compile(intent, pack, opts)
	if err != nil {
		t.Fatalf("Compile(%q) returned an unexpected error: %v", intent, err)
	}
	return c
}

// TestOnlyAnEmptyIntentIsAnError pins the input contract: a caller with no
// request has a bug, a caller whose index matched nothing has an answer.
func TestOnlyAnEmptyIntentIsAnError(t *testing.T) {
	cases := []struct {
		name    string
		intent  string
		pack    devctx.Pack
		wantErr error
	}{
		{"empty string", "", richPack(), ErrEmptyIntent},
		{"whitespace only", "   \n\t ", richPack(), ErrEmptyIntent},
		{"real intent, empty pack", "Add idempotency", devctx.Pack{}, nil},
		{"real intent, full pack", "Add idempotency", richPack(), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.intent, tc.pack, Options{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Compile err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestNoConstraintOrRiskIsEmittedWithoutEvidence is the rule the package
// exists to enforce. It asserts over EVERY generated constraint and risk across
// a spread of packs, because the failure it guards is a future rule that
// forgets to cite — not a specific known case.
func TestNoConstraintOrRiskIsEmittedWithoutEvidence(t *testing.T) {
	cases := []struct {
		name string
		pack devctx.Pack
	}{
		{"rich pack", richPack()},
		{"empty pack", devctx.Pack{}},
		{"items with no paths", devctx.Pack{Items: []devctx.Item{
			{Kind: "endpoint", Name: "GET /health", Relevance: 1},
			{Kind: "table", Name: "sessions", Relevance: 0.9},
			{Kind: "test", Name: "HealthTest", Relevance: 0.8, Confidence: "low"},
		}}},
		{"items with no names", devctx.Pack{Items: []devctx.Item{
			{Kind: "file", Path: "internal/pay/callback.go", Relevance: 1},
			{Kind: "file", Path: "internal/pay/callback_test.go", Relevance: 0.9},
		}}},
		{"item identifying nothing", devctx.Pack{Items: []devctx.Item{
			{Kind: "endpoint", Relevance: 1, Why: "no path, no name"},
			{Kind: "table", Relevance: 0.9, Why: "no path, no name"},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileOrFail(t, "Add idempotency to payment callbacks", tc.pack, Options{})
			for i, con := range c.Constraints {
				if strings.TrimSpace(con.Evidence) == "" {
					t.Errorf("constraint %d %q carries no evidence", i, con.Text)
				}
				if strings.TrimSpace(con.Text) == "" {
					t.Errorf("constraint %d has evidence %q but no text", i, con.Evidence)
				}
			}
			for i, r := range c.Risks {
				if strings.TrimSpace(r.Evidence) == "" {
					t.Errorf("risk %d %q carries no evidence", i, r.Text)
				}
				switch r.Severity {
				case severityHigh, severityMedium, severityLow:
				default:
					t.Errorf("risk %d has severity %q, want high|medium|low", i, r.Severity)
				}
			}
		})
	}
}

// TestEvidenceNamesSomethingInScope checks the citations point at real entries
// rather than being well-formed prose. Evidence nobody can follow is the same
// as no evidence, only harder to spot.
func TestEvidenceNamesSomethingInScope(t *testing.T) {
	c := compileOrFail(t, "Add idempotency to payment callbacks", richPack(), Options{})

	inScope := map[string]bool{}
	for _, s := range c.Scope {
		inScope[s.ref()] = true
	}
	for _, con := range c.Constraints {
		found := false
		for ref := range inScope {
			if strings.Contains(con.Evidence, ref) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("constraint %q cites %q, which names nothing in scope", con.Text, con.Evidence)
		}
	}
}

// TestEmptyPackProducesUnknownsRatherThanAConfidentContract. A pack that
// matched nothing must not compile into a short, clean-looking brief: the
// emptiness is the finding.
func TestEmptyPackProducesUnknownsRatherThanAConfidentContract(t *testing.T) {
	c := compileOrFail(t, "Add idempotency to payment callbacks", devctx.Pack{}, Options{})

	if len(c.Scope) != 0 {
		t.Errorf("Scope = %v, want empty for an empty pack", c.Scope)
	}
	if len(c.Constraints) != 0 {
		t.Errorf("Constraints = %v, want none: nothing in the pack could support one", c.Constraints)
	}
	if len(c.Acceptance) != 0 {
		t.Errorf("Acceptance = %v, want none: every criterion must name a scope entry", c.Acceptance)
	}

	joined := strings.ToLower(strings.Join(c.Unknowns, "\n"))
	for _, want := range []string{"matched nothing", "no interpretation"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Unknowns do not mention %q; got:\n%s", want, strings.Join(c.Unknowns, "\n"))
		}
	}

	md := c.Markdown()
	if !strings.Contains(md, "## Unknowns") {
		t.Error("an empty pack must still render an Unknowns section")
	}
	for _, absent := range []string{"## Scope", "## Constraints", "## Acceptance", "## Risks"} {
		if strings.Contains(md, absent) {
			t.Errorf("markdown contains %q for an empty pack, which claims a finding that was never made", absent)
		}
	}
}

// TestUnknownsAreNeverEmpty holds across every input, including the best-case
// pack. This package is never entitled to say "nothing is missing".
func TestUnknownsAreNeverEmpty(t *testing.T) {
	cases := []struct {
		name string
		pack devctx.Pack
	}{
		{"rich pack", richPack()},
		{"empty pack", devctx.Pack{}},
		{"single well-covered item", devctx.Pack{
			Interpretation: "read as a change to the callback handler",
			Items: []devctx.Item{
				{Kind: "test", Name: "CallbackTest", Path: "internal/pay/callback_test.go", Relevance: 1, Why: "covers it", Confidence: "exact"},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileOrFail(t, "Add idempotency", tc.pack, Options{})
			if len(c.Unknowns) == 0 {
				t.Fatal("Unknowns is empty, which claims the contract is complete")
			}
		})
	}
}

// TestMarkdownIsByteIdenticalAcrossRuns. An agent that gets a different brief
// on a retry cannot build a cache, a test, or trust.
func TestMarkdownIsByteIdenticalAcrossRuns(t *testing.T) {
	const runs = 50
	want := compileOrFail(t, "Add idempotency to payment callback processing", richPack(), Options{}).Markdown()
	for i := 0; i < runs; i++ {
		got := compileOrFail(t, "Add idempotency to payment callback processing", richPack(), Options{}).Markdown()
		if got != want {
			t.Fatalf("run %d produced different markdown:\n--- want ---\n%s\n--- got ---\n%s", i, want, got)
		}
	}
}

// TestScopeOrderDoesNotDependOnInputOrder proves the sort is total rather than
// merely present: items tied on relevance must still land in a fixed order.
func TestScopeOrderDoesNotDependOnInputOrder(t *testing.T) {
	tied := []devctx.Item{
		{Kind: "function", Name: "beta", Path: "b.go", Relevance: 0.5, Why: "tie"},
		{Kind: "function", Name: "alpha", Path: "a.go", Relevance: 0.5, Why: "tie"},
		{Kind: "function", Name: "gamma", Path: "c.go", Relevance: 0.5, Why: "tie"},
		{Kind: "function", Name: "delta", Path: "a.go", Relevance: 0.5, Why: "tie"},
	}
	reversed := make([]devctx.Item, len(tied))
	for i := range tied {
		reversed[i] = tied[len(tied)-1-i]
	}

	first := compileOrFail(t, "Refactor the helpers", devctx.Pack{Items: tied}, Options{}).Markdown()
	second := compileOrFail(t, "Refactor the helpers", devctx.Pack{Items: reversed}, Options{}).Markdown()
	if first != second {
		t.Fatalf("scope order followed the input order:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestEndpointInScopeConstrainsThePublicSurface.
func TestEndpointInScopeConstrainsThePublicSurface(t *testing.T) {
	pack := devctx.Pack{
		Interpretation: "change the callback route",
		Items: []devctx.Item{
			{Kind: "endpoint", Name: "POST /payments/callback", Path: "src/pay/CallbackController.java", Relevance: 1, Why: "the entry point", Confidence: "high"},
		},
	}
	c := compileOrFail(t, "Add idempotency to the callback", pack, Options{})

	con, ok := constraintContaining(c, "breaking change")
	if !ok {
		t.Fatalf("no breaking-change constraint for an endpoint in scope; got %v", c.Constraints)
	}
	if !strings.Contains(con.Evidence, "POST /payments/callback") {
		t.Errorf("constraint evidence = %q, want it to name the endpoint", con.Evidence)
	}

	// The same fact must reach the checklist, or "done" and "the rules" disagree.
	if !strings.Contains(strings.Join(c.Acceptance, "\n"), "payload shape") {
		t.Errorf("Acceptance = %v, want a criterion about the endpoint's shape", c.Acceptance)
	}
}

// TestTableInScopeRequiresAMigration.
func TestTableInScopeRequiresAMigration(t *testing.T) {
	pack := devctx.Pack{
		Interpretation: "change what the handler stores",
		Items: []devctx.Item{
			{Kind: "table", Name: "payment_callbacks", Relevance: 1, Why: "written by the handler", Confidence: "high"},
		},
	}
	c := compileOrFail(t, "Store a request key on each callback", pack, Options{})

	con, ok := constraintContaining(c, "migration")
	if !ok {
		t.Fatalf("no migration constraint for a table in scope; got %v", c.Constraints)
	}
	if !strings.Contains(con.Evidence, "payment_callbacks") {
		t.Errorf("constraint evidence = %q, want it to name the table", con.Evidence)
	}
}

// TestKindRulesFireOnlyOnTheirOwnKind guards against a rule that matches
// everything: a constraint on every task is noise the reader learns to skip.
func TestKindRulesFireOnlyOnTheirOwnKind(t *testing.T) {
	cases := []struct {
		name       string
		item       devctx.Item
		wantText   string
		unwantText string
	}{
		{"endpoint", devctx.Item{Kind: "endpoint", Name: "GET /x", Path: "a.go", Relevance: 1}, "breaking change", "migration"},
		{"table", devctx.Item{Kind: "table", Name: "orders", Relevance: 1}, "migration", "breaking change"},
		{"interface", devctx.Item{Kind: "interface", Name: "Store", Path: "a.go", Relevance: 1}, "every implementation", "migration"},
		{"configuration", devctx.Item{Kind: "configuration", Name: "app.yml", Path: "app.yml", Relevance: 1}, "configuration-driven", "breaking change"},
		{"plain function", devctx.Item{Kind: "function", Name: "helper", Path: "a.go", Relevance: 1}, "", "breaking change"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileOrFail(t, "Change it", devctx.Pack{Items: []devctx.Item{tc.item}}, Options{})
			if tc.wantText != "" {
				if _, ok := constraintContaining(c, tc.wantText); !ok {
					t.Errorf("no constraint containing %q; got %v", tc.wantText, c.Constraints)
				}
			}
			if _, ok := constraintContaining(c, tc.unwantText); ok {
				t.Errorf("constraint containing %q fired for kind %q", tc.unwantText, tc.item.Kind)
			}
		})
	}
}

// TestTestFilesAreRecognisedFromTheirPath. The index does not always label a
// test file, and a missed one turns into a confident, wrong "no test in scope".
func TestTestFilesAreRecognisedFromTheirPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"internal/pay/callback_test.go", true},
		{"src/test/java/pay/CallbackControllerTest.java", true},
		{"src/main/java/pay/CallbackControllerTests.java", true},
		{"web/src/checkout.spec.ts", true},
		{"web/src/checkout.test.tsx", true},
		{"api/tests/test_callback.py", true},
		{"spec/models/payment_spec.rb", true},
		{"internal/pay/callback.go", false},
		{"src/main/java/pay/Latest.java", false},
		{"internal/protest/handler.go", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := isTestPath(tc.path); got != tc.want {
				t.Errorf("isTestPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestMissingTestInScopeIsNamedSpecifically. The unknown has to say what is
// missing and what to do about it; "more information may be needed" is the
// thing this package exists not to emit.
func TestMissingTestInScopeIsNamedSpecifically(t *testing.T) {
	withoutTest := devctx.Pack{
		Interpretation: "change the handler",
		Items: []devctx.Item{
			{Kind: "method", Name: "handleCallback", Path: "src/pay/Callback.java", Relevance: 1, Why: "named by the task", Confidence: "exact"},
		},
	}
	c := compileOrFail(t, "Add idempotency", withoutTest, Options{})
	if !strings.Contains(strings.Join(c.Unknowns, "\n"), "No test file appears in scope") {
		t.Fatalf("Unknowns do not name the missing test; got %v", c.Unknowns)
	}

	withTest := withoutTest
	withTest.Items = append(append([]devctx.Item{}, withoutTest.Items...), devctx.Item{
		Kind: "file", Name: "CallbackTest.java", Path: "src/test/java/pay/CallbackTest.java",
		Relevance: 0.5, Why: "covers the handler", Confidence: "high",
	})
	c = compileOrFail(t, "Add idempotency", withTest, Options{})
	if strings.Contains(strings.Join(c.Unknowns, "\n"), "No test file appears in scope") {
		t.Errorf("the missing-test unknown fired with a test file in scope; got %v", c.Unknowns)
	}
	if _, ok := constraintContaining(c, "must still pass"); !ok {
		t.Errorf("a test file in scope produced no test constraint; got %v", c.Constraints)
	}
}

// TestInferredRelationshipBecomesARisk. The context engine's low confidence is
// about the RELATIONSHIP, and flattening it into a plain scope list would let
// an agent treat a name collision as a call edge.
func TestInferredRelationshipBecomesARisk(t *testing.T) {
	pack := devctx.Pack{
		Interpretation: "change the retry behaviour",
		Items: []devctx.Item{
			{Kind: "function", Name: "retryLoop", Path: "src/pay/Retry.java", Relevance: 1, Why: "name matched a task term", Confidence: "low"},
		},
	}
	c := compileOrFail(t, "Add idempotency", pack, Options{})

	r, ok := riskContaining(c, "inferred relationship")
	if !ok {
		t.Fatalf("no risk for a low-confidence item; got %v", c.Risks)
	}
	if !strings.Contains(r.Evidence, "src/pay/Retry.java") {
		t.Errorf("risk evidence = %q, want it to name the inferred item", r.Evidence)
	}

	pack.Items[0].Confidence = "exact"
	c = compileOrFail(t, "Add idempotency", pack, Options{})
	if _, ok := riskContaining(c, "inferred relationship"); ok {
		t.Errorf("the inference risk fired for a proven relationship; got %v", c.Risks)
	}
}

// TestEndpointAndTableTogetherRaiseAHighRisk pins the one high-severity rule,
// including that neither half raises it alone.
func TestEndpointAndTableTogetherRaiseAHighRisk(t *testing.T) {
	endpoint := devctx.Item{Kind: "endpoint", Name: "POST /pay", Path: "a.go", Relevance: 1, Confidence: "high"}
	table := devctx.Item{Kind: "table", Name: "payments", Relevance: 0.9, Confidence: "high"}

	cases := []struct {
		name  string
		items []devctx.Item
		want  bool
	}{
		{"endpoint only", []devctx.Item{endpoint}, false},
		{"table only", []devctx.Item{table}, false},
		{"both", []devctx.Item{endpoint, table}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileOrFail(t, "Add idempotency", devctx.Pack{Items: tc.items}, Options{})
			r, ok := riskContaining(c, "public API surface and persisted data")
			if ok != tc.want {
				t.Fatalf("cross-surface risk present = %v, want %v; risks %v", ok, tc.want, c.Risks)
			}
			if ok && r.Severity != severityHigh {
				t.Errorf("severity = %q, want %q", r.Severity, severityHigh)
			}
		})
	}
}

// TestRisksAreOrderedWorstFirst so the reader meets the high-severity item
// before deciding they have read enough.
func TestRisksAreOrderedWorstFirst(t *testing.T) {
	c := compileOrFail(t, "Add idempotency to payment callback processing", richPack(), Options{})
	if len(c.Risks) < 2 {
		t.Fatalf("fixture produced %d risk(s), want at least 2 to test ordering", len(c.Risks))
	}
	for i := 1; i < len(c.Risks); i++ {
		if severityRank(c.Risks[i-1].Severity) < severityRank(c.Risks[i].Severity) {
			t.Fatalf("risk %d (%s) outranks risk %d (%s)", i, c.Risks[i].Severity, i-1, c.Risks[i-1].Severity)
		}
	}
}

// TestScopeIsBoundedAndTheCutIsNamed. Truncating silently would make the
// contract's boundary a lie rather than a budget.
func TestScopeIsBoundedAndTheCutIsNamed(t *testing.T) {
	var items []devctx.Item
	for i := 0; i < 30; i++ {
		items = append(items, devctx.Item{
			Kind: "function", Name: string(rune('a'+i%26)) + "fn",
			Path: "pkg/f" + string(rune('a'+i%26)) + ".go", Relevance: float64(30-i) / 30,
			Why: "on the call path", Confidence: "high",
		})
	}
	pack := devctx.Pack{Interpretation: "wide change", Items: items}

	cases := []struct {
		name string
		opts Options
		want int
	}{
		{"zero uses the default", Options{}, defaultMaxScope},
		{"negative uses the default", Options{MaxScope: -5}, defaultMaxScope},
		{"explicit bound is honoured", Options{MaxScope: 3}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileOrFail(t, "Rename the helpers", pack, tc.opts)
			if len(c.Scope) != tc.want {
				t.Fatalf("len(Scope) = %d, want %d", len(c.Scope), tc.want)
			}
			if !strings.Contains(strings.Join(c.Unknowns, "\n"), "Scope was capped") {
				t.Errorf("the truncation was not named in Unknowns; got %v", c.Unknowns)
			}
		})
	}

	// The highest-ranked item must survive the cut, or the bound is discarding
	// the answer instead of the tail.
	c := compileOrFail(t, "Rename the helpers", pack, Options{MaxScope: 1})
	if c.Scope[0].Symbol != items[0].Name {
		t.Errorf("Scope[0].Symbol = %q, want the top-ranked %q", c.Scope[0].Symbol, items[0].Name)
	}
}

// TestOutOfScopeCarriesTheExclusionReasonVerbatim. A rewritten reason is a
// second opinion standing in for the engine's checkable claim.
func TestOutOfScopeCarriesTheExclusionReasonVerbatim(t *testing.T) {
	c := compileOrFail(t, "Add idempotency to payment callback processing", richPack(), Options{})
	if len(c.OutOfScope) != 1 {
		t.Fatalf("len(OutOfScope) = %d, want 1", len(c.OutOfScope))
	}
	got := c.OutOfScope[0]
	if got.Symbol != "MarketingReport" || got.Path != "unrelated/marketing/MarketingReport.java" {
		t.Errorf("OutOfScope[0] = %+v, want the excluded candidate", got)
	}
	if got.Why != "shares no dependency edge with the callback flow" {
		t.Errorf("Why = %q, want the pack's reason unchanged", got.Why)
	}
	if !strings.Contains(c.Markdown(), "## Out of scope") {
		t.Error("exclusions did not reach the rendered brief, where they do their work")
	}
}

// TestPackNotesAreCarriedIntoUnknowns. The engine's coverage caveats are the
// difference between a bounded answer and a partial one; dropping them here
// would launder a truncated traversal into a complete-looking brief.
func TestPackNotesAreCarriedIntoUnknowns(t *testing.T) {
	c := compileOrFail(t, "Add idempotency to payment callback processing", richPack(), Options{})
	if !strings.Contains(strings.Join(c.Unknowns, "\n"), "traversal stopped at depth 3") {
		t.Fatalf("pack note was dropped; Unknowns = %v", c.Unknowns)
	}
}

// TestMarkdownNeverEmitsAnEmptySection. A header with nothing under it reads as
// "we checked and found none", which is a claim, not a formatting detail.
func TestMarkdownNeverEmitsAnEmptySection(t *testing.T) {
	cases := []struct {
		name     string
		intent   string
		pack     devctx.Pack
		maxScope int
	}{
		{"rich pack", "Add idempotency to payment callback processing", richPack(), 0},
		{"empty pack", "Add idempotency", devctx.Pack{}, 0},
		{"no interpretation", "Add idempotency", devctx.Pack{Items: richPack().Items}, 0},
		{"no exclusions", "Add idempotency", devctx.Pack{Interpretation: "x", Items: richPack().Items}, 0},
		{"single item", "Tidy up", devctx.Pack{Interpretation: "x", Items: []devctx.Item{
			{Kind: "function", Name: "helper", Path: "a.go", Relevance: 1, Why: "named"},
		}}, 1},
		{"item identifying nothing", "Tidy up", devctx.Pack{Interpretation: "x", Items: []devctx.Item{
			{Kind: "function", Relevance: 1, Why: "no path, no name"},
		}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := compileOrFail(t, tc.intent, tc.pack, Options{MaxScope: tc.maxScope}).Markdown()
			if empty := emptySections(md); len(empty) > 0 {
				t.Fatalf("empty section(s) %v in:\n%s", empty, md)
			}
		})
	}
}

// TestIntentIsCarriedVerbatim. The reader compares Intent against
// Interpretation to spot a misread; normalising the intent hides the vagueness
// that caused it.
func TestIntentIsCarriedVerbatim(t *testing.T) {
	const intent = "  make  the callback thing\nidempotent  "
	c := compileOrFail(t, intent, richPack(), Options{})
	if c.Intent != intent {
		t.Errorf("Intent = %q, want it unchanged", c.Intent)
	}
	if c.Goal != "Make the callback thing idempotent." {
		t.Errorf("Goal = %q, want whitespace and punctuation normalised and nothing else", c.Goal)
	}
	if c.Interpretation != richPack().Interpretation {
		t.Errorf("Interpretation = %q, want the pack's", c.Interpretation)
	}
}

// TestGoalNormalisesOnlyShapeNeverContent. Any expansion here would be an
// invented requirement wearing the same typeface as a derived one.
func TestGoalNormalisesOnlyShapeNeverContent(t *testing.T) {
	cases := []struct {
		intent string
		want   string
	}{
		{"add idempotency", "Add idempotency."},
		{"Add idempotency.", "Add idempotency."},
		{"why is this slow?", "Why is this slow?"},
		{"fix it!", "Fix it!"},
		{"  spread   over\n  lines  ", "Spread over lines."},
	}
	for _, tc := range cases {
		t.Run(tc.intent, func(t *testing.T) {
			c := compileOrFail(t, tc.intent, devctx.Pack{}, Options{})
			if c.Goal != tc.want {
				t.Errorf("Goal = %q, want %q", c.Goal, tc.want)
			}
		})
	}
}

// TestFileEntitiesDoNotRepeatTheirPathAsASymbol: printing the same string twice
// reads as two different things being in scope.
func TestFileEntitiesDoNotRepeatTheirPathAsASymbol(t *testing.T) {
	pack := devctx.Pack{Interpretation: "x", Items: []devctx.Item{
		{Kind: "file", Name: "callback.go", Path: "internal/pay/callback.go", Relevance: 1, Why: "named by the task"},
	}}
	c := compileOrFail(t, "Add idempotency", pack, Options{})
	if c.Scope[0].Symbol != "" {
		t.Errorf("Scope[0].Symbol = %q, want empty for a file entity", c.Scope[0].Symbol)
	}
	if strings.Count(c.Markdown(), "callback.go") != 1 {
		t.Errorf("the file is named more than once in the brief:\n%s", c.Markdown())
	}
}

// TestEvidenceListIsCappedAndSorted. Evidence is a pointer into Scope, not a
// copy of it: a citation nobody reads is decorative.
func TestEvidenceListIsCappedAndSorted(t *testing.T) {
	var items []devctx.Item
	for _, name := range []string{"zeta", "alpha", "mu", "beta", "kappa"} {
		items = append(items, devctx.Item{
			Kind: "endpoint", Name: "GET /" + name, Path: name + ".go",
			Relevance: 1, Why: "an endpoint", Confidence: "high",
		})
	}
	c := compileOrFail(t, "Version the API", devctx.Pack{Interpretation: "x", Items: items}, Options{})

	con, ok := constraintContaining(c, "breaking change")
	if !ok {
		t.Fatal("no endpoint constraint")
	}
	if !strings.HasSuffix(con.Evidence, "and 2 more") {
		t.Errorf("evidence = %q, want it capped at %d refs", con.Evidence, maxEvidenceRefs)
	}
	if !strings.HasPrefix(con.Evidence, "GET /alpha") {
		t.Errorf("evidence = %q, want it sorted so it does not reorder with scores", con.Evidence)
	}
}

// TestTokenEstimateTracksTheRenderedBrief, which is the thing a caller is
// actually charged for.
func TestTokenEstimateTracksTheRenderedBrief(t *testing.T) {
	c := compileOrFail(t, "Add idempotency to payment callback processing", richPack(), Options{})
	if want := len(c.Markdown()) / bytesPerToken; c.TokenEstimate != want {
		t.Errorf("TokenEstimate = %d, want %d", c.TokenEstimate, want)
	}
	if c.TokenEstimate <= 0 {
		t.Error("TokenEstimate = 0 for a non-empty brief")
	}
	if strings.Contains(c.Markdown(), "token") {
		t.Error("the brief renders its own token estimate, which tells the agent nothing about the task")
	}
}

// TestKindVocabularyMatchesTheDomainModel pins the mirrored constants. Without
// it, a rename in pkg/model would switch a rule off silently — the exact
// failure mode that makes mirroring rather than importing a bad trade.
func TestKindVocabularyMatchesTheDomainModel(t *testing.T) {
	cases := []struct {
		local string
		want  model.EntityKind
	}{
		{kindTest, model.KindTest},
		{kindEndpoint, model.KindEndpoint},
		{kindTable, model.KindTable},
		{kindInterface, model.KindInterface},
		{kindConfiguration, model.KindConfiguration},
		{kindFile, model.KindFile},
	}
	for _, tc := range cases {
		if tc.local != string(tc.want) {
			t.Errorf("local kind %q does not match model kind %q", tc.local, tc.want)
		}
	}
	if confidenceLow != string(model.ConfidenceLow) {
		t.Errorf("local confidence %q does not match model %q", confidenceLow, model.ConfidenceLow)
	}
}

func constraintContaining(c Contract, substr string) (Constraint, bool) {
	if substr == "" {
		return Constraint{}, false
	}
	for _, con := range c.Constraints {
		if strings.Contains(con.Text, substr) {
			return con, true
		}
	}
	return Constraint{}, false
}

func riskContaining(c Contract, substr string) (Risk, bool) {
	for _, r := range c.Risks {
		if strings.Contains(r.Text, substr) {
			return r, true
		}
	}
	return Risk{}, false
}

// emptySections returns the titles of any "## " header with no content before
// the next header or the end of the document.
func emptySections(md string) []string {
	lines := strings.Split(md, "\n")
	var empty []string
	for i, line := range lines {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		hasBody := false
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "#") {
				break
			}
			if strings.TrimSpace(lines[j]) != "" {
				hasBody = true
				break
			}
		}
		if !hasBody {
			empty = append(empty, strings.TrimPrefix(line, "## "))
		}
	}
	return empty
}
