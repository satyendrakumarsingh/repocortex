package bench

import (
	"encoding/json"
	"strings"
	"testing"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
)

func item(name, path string) devctx.Item {
	return devctx.Item{Kind: "class", Name: name, Path: path, Why: "test fixture"}
}

func TestPathSuffixMatchRequiresASegmentBoundary(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		path  string
		want  bool
	}{
		{"tail at a boundary matches", "svc/Repo.java", "src/main/svc/Repo.java", true},
		{"bare file name at a boundary matches", "Repo.java", "src/main/svc/Repo.java", true},
		{"whole path matches", "src/Repo.java", "src/Repo.java", true},
		{"mid-segment tail does not match", "Repo.java", "src/main/svc/OtherRepo.java", false},
		{"mid-segment directory tail does not match", "svc/Repo.java", "src/mysvc/Repo.java", false},
		{"entry longer than the path does not match", "a/b/Repo.java", "b/Repo.java", false},
		{"prefix of the path does not match", "src/main", "src/main/svc/Repo.java", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches(tc.entry, item("Whatever", tc.path)); got != tc.want {
				t.Fatalf("matches(%q, path=%q) = %v, want %v", tc.entry, tc.path, got, tc.want)
			}
		})
	}
}

func TestExpectMatchesSymbolNameFQNAndDirectoryPrefix(t *testing.T) {
	it := devctx.Item{Kind: "class", Name: "Repo", FQN: "com.acme.svc.Repo", Path: "src/main/svc/Repo.java"}
	for _, entry := range []string{"Repo", "com.acme.svc.Repo", "src/main/", "src/main/svc/Repo.java"} {
		if !matches(entry, it) {
			t.Errorf("matches(%q) = false, want true", entry)
		}
	}
	for _, entry := range []string{"", "repo", "Repository", "com.acme.svc", "src/other/"} {
		if matches(entry, it) {
			t.Errorf("matches(%q) = true, want false", entry)
		}
	}
}

func TestDirectoryPrefixEntryNeverMatchesASymbolName(t *testing.T) {
	// "payments/" naming a directory must not be satisfied by a symbol that
	// happens to be called "payments/", or the grammar means two things.
	it := devctx.Item{Name: "payments/", Path: "src/other/Thing.java"}
	if matches("payments/", it) {
		t.Fatal("directory entry matched a symbol name")
	}
}

func TestScorePartitionsExpectAndComputesRecall(t *testing.T) {
	c := Case{ID: "a", Task: "t", Expect: []string{"Repo.java", "Missing.java", "Controller"}}
	pack := devctx.Pack{Items: []devctx.Item{
		item("Repo", "src/svc/Repo.java"),
		item("Controller", "src/web/Controller.java"),
	}}

	got := Score(c, pack)
	if want := []string{"Controller", "Repo.java"}; !equal(got.Found, want) {
		t.Errorf("Found = %v, want %v", got.Found, want)
	}
	if want := []string{"Missing.java"}; !equal(got.Missing, want) {
		t.Errorf("Missing = %v, want %v", got.Missing, want)
	}
	if got.Recall < 0.66 || got.Recall > 0.67 {
		t.Errorf("Recall = %v, want 2/3", got.Recall)
	}
	if got.Items != 2 {
		t.Errorf("Items = %d, want 2", got.Items)
	}
	if got.Passed {
		t.Error("Passed = true with a missing expectation")
	}
}

func TestForbiddenItemFailsTheCaseEvenAtFullRecall(t *testing.T) {
	c := Case{
		ID:     "payment-callback",
		Task:   "Add idempotency to payment callback processing",
		Expect: []string{"CallbackController.java"},
		Forbid: []string{"unrelated/marketing/"},
	}
	pack := devctx.Pack{Items: []devctx.Item{
		item("CallbackController", "src/main/payments/CallbackController.java"),
		item("MarketingReport", "unrelated/marketing/MarketingReport.java"),
	}}

	got := Score(c, pack)
	if got.Recall != 1 {
		t.Fatalf("Recall = %v, want 1", got.Recall)
	}
	if len(got.Missing) != 0 {
		t.Fatalf("Missing = %v, want none", got.Missing)
	}
	if want := []string{"unrelated/marketing/"}; !equal(got.Violations, want) {
		t.Fatalf("Violations = %v, want %v", got.Violations, want)
	}
	if got.Passed {
		t.Fatal("Passed = true despite retrieving a forbidden item; recall alone is gameable")
	}
}

func TestExcludedCandidateIsNotAViolation(t *testing.T) {
	c := Case{ID: "a", Task: "t", Expect: []string{"Repo.java"}, Forbid: []string{"MarketingReport"}}
	pack := devctx.Pack{
		Items:    []devctx.Item{item("Repo", "src/svc/Repo.java")},
		Excluded: []devctx.Exclusion{{Name: "MarketingReport", Path: "unrelated/marketing/MarketingReport.java", Reason: "no edge to the task"}},
	}
	got := Score(c, pack)
	if len(got.Violations) != 0 {
		t.Fatalf("Violations = %v, want none: an exclusion is the engine rejecting the candidate", got.Violations)
	}
	if !got.Passed {
		t.Fatal("Passed = false, want true")
	}
}

func TestMaxTokensFailsAPackOverBudget(t *testing.T) {
	c := Case{ID: "a", Task: "t", Expect: []string{"Repo.java"}, MaxTokens: 1000}
	pack := devctx.Pack{
		Items:         []devctx.Item{item("Repo", "src/svc/Repo.java")},
		TokenEstimate: 4200,
	}
	got := Score(c, pack)
	if !got.OverBudget {
		t.Fatal("OverBudget = false at 4200 tokens against a 1000 token budget")
	}
	if got.Passed {
		t.Fatal("Passed = true over budget; a pack that contains everything is not a good pack")
	}
	if got.Recall != 1 {
		t.Fatalf("Recall = %v, want 1: budget failure must not disturb recall", got.Recall)
	}

	within := Score(Case{ID: "a", Task: "t", Expect: []string{"Repo.java"}, MaxTokens: 5000}, pack)
	if within.OverBudget || !within.Passed {
		t.Fatalf("case within budget did not pass: %+v", within)
	}

	// MaxTokens unset means unbounded, not zero.
	unbounded := Score(Case{ID: "a", Task: "t", Expect: []string{"Repo.java"}}, pack)
	if unbounded.OverBudget {
		t.Fatal("OverBudget = true with no MaxTokens set")
	}
}

func TestTokenEstimateFallsBackToThePacksOwnCount(t *testing.T) {
	// A hand-built pack never fills TokenEstimate; reading that zero literally
	// would let every budgeted case pass without measuring anything.
	pack := devctx.Pack{
		Task:  "some task",
		Items: []devctx.Item{{Name: "Repo", Path: "src/svc/Repo.java", Why: "x", Excerpt: strings.Repeat("x", 8000)}},
	}
	got := Score(Case{ID: "a", Task: "t", Expect: []string{"Repo.java"}, MaxTokens: 100}, pack)
	if got.TokenEstimate < 1000 {
		t.Fatalf("TokenEstimate = %d, want the pack's own content estimate", got.TokenEstimate)
	}
	if !got.OverBudget {
		t.Fatal("OverBudget = false on an 8KB excerpt against a 100 token budget")
	}
}

func TestRecallIsZeroRatherThanVacuouslyOneOnAnEmptyExpectList(t *testing.T) {
	got := Score(Case{ID: "a", Task: "t"}, devctx.Pack{})
	if got.Recall != 0 {
		t.Fatalf("Recall = %v, want 0", got.Recall)
	}
}

func TestValidateRejectsACaseWithAnEmptyExpectList(t *testing.T) {
	err := Validate([]Case{{ID: "a", Task: "do a thing"}})
	if err == nil {
		t.Fatal("Validate accepted a case with nothing to expect; it would pass vacuously")
	}
	if !strings.Contains(err.Error(), "empty expect") {
		t.Fatalf("error does not name the defect: %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cases := []Case{
		{ID: "", Task: "t", Expect: []string{"a"}},
		{ID: "dup", Task: "", Expect: []string{"a"}},
		{ID: "dup", Task: "t", Expect: []string{"a"}},
		{ID: "empty-expect", Task: "t"},
		{ID: "blank-entry", Task: "t", Expect: []string{"  "}},
	}
	err := Validate(cases)
	if err == nil {
		t.Fatal("Validate accepted five malformed cases")
	}
	msg := err.Error()
	for _, want := range []string{"empty id", "empty task", "duplicate id", "empty expect", "blank"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q; every problem must be reported at once:\n%s", want, msg)
		}
	}
}

func TestValidateAcceptsAWellFormedSuite(t *testing.T) {
	err := Validate([]Case{
		{ID: "a", Task: "t", Expect: []string{"Repo.java"}},
		{ID: "b", Task: "t2", Expect: []string{"Other.java"}, Forbid: []string{"unrelated/"}},
	})
	if err != nil {
		t.Fatalf("Validate rejected a valid suite: %v", err)
	}
}

func TestSummariseSortsByCaseIDAndAggregates(t *testing.T) {
	results := []Result{
		{CaseID: "c", Recall: 0.5, Violations: []string{"x"}},
		{CaseID: "a", Recall: 1, Passed: true},
		{CaseID: "b", Recall: 1, Passed: true},
	}
	s := Summarise(results)

	if got := []string{s.Results[0].CaseID, s.Results[1].CaseID, s.Results[2].CaseID}; !equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("results are not sorted by CaseID: %v", got)
	}
	if s.Cases != 3 || s.Passed != 2 || s.TotalViolations != 1 {
		t.Fatalf("aggregates = cases %d passed %d violations %d", s.Cases, s.Passed, s.TotalViolations)
	}
	if s.MeanRecall < 0.83 || s.MeanRecall > 0.84 {
		t.Fatalf("MeanRecall = %v, want 2.5/3", s.MeanRecall)
	}
	if results[0].CaseID != "c" {
		t.Fatal("Summarise reordered the caller's slice in place")
	}
}

func TestSummariseOfNoResultsIsEmptyNotDivideByZero(t *testing.T) {
	s := Summarise(nil)
	if s.Cases != 0 || s.MeanRecall != 0 {
		t.Fatalf("empty suite = %+v", s)
	}
	if !strings.Contains(s.Markdown(), "No cases") {
		t.Fatalf("empty markdown does not say so:\n%s", s.Markdown())
	}
}

func TestSuiteAndMarkdownAreIdenticalAcrossFiftyRuns(t *testing.T) {
	cases := []Case{
		{ID: "payments", Task: "Add idempotency to payment callback processing",
			Expect: []string{"CallbackController.java", "PaymentService"}, Forbid: []string{"unrelated/"}},
		{ID: "auth", Task: "Where is the login rate limit applied?",
			Expect: []string{"RateLimiter.java", "NotThere.java"}},
		{ID: "budget", Task: "Trace the order pipeline",
			Expect: []string{"Repo.java"}, MaxTokens: 10},
	}
	pack := devctx.Pack{
		Items: []devctx.Item{
			item("CallbackController", "src/main/payments/CallbackController.java"),
			item("PaymentService", "src/main/payments/PaymentService.java"),
			item("RateLimiter", "src/main/auth/RateLimiter.java"),
			item("Repo", "src/main/svc/Repo.java"),
			item("MarketingReport", "unrelated/marketing/MarketingReport.java"),
		},
		TokenEstimate: 1200,
	}

	var firstJSON, firstMD string
	for run := range 50 {
		// Rotate the scoring order each run: the report must depend on the
		// cases, never on the order the harness happened to finish them in.
		results := make([]Result, 0, len(cases))
		for i := range cases {
			results = append(results, Score(cases[(i+run)%len(cases)], pack))
		}
		s := Summarise(results)
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		md := s.Markdown()

		if run == 0 {
			firstJSON, firstMD = string(raw), md
			continue
		}
		if string(raw) != firstJSON {
			t.Fatalf("run %d suite differs:\n%s\n---\n%s", run, firstJSON, raw)
		}
		if md != firstMD {
			t.Fatalf("run %d markdown differs:\n%s\n---\n%s", run, firstMD, md)
		}
	}

	if !strings.Contains(firstMD, "FAIL") || !strings.Contains(firstMD, "## Failures") {
		t.Fatalf("markdown does not report the failing cases:\n%s", firstMD)
	}
	if !strings.Contains(firstMD, "Over budget") || !strings.Contains(firstMD, "forbidden") {
		t.Fatalf("markdown does not explain why cases failed:\n%s", firstMD)
	}
}

func TestMarkdownEscapesPipesSoColumnsDoNotShift(t *testing.T) {
	r := Score(Case{ID: "a|b", Task: "grep foo | wc -l\nsecond line", Expect: []string{"Nope.java"}}, devctx.Pack{})
	md := Summarise([]Result{r}).Markdown()
	if strings.Contains(md, "a|b") || strings.Contains(md, "foo | wc") {
		t.Fatalf("unescaped pipe reached the table:\n%s", md)
	}
	if strings.Contains(md, "second line\n") && strings.Contains(md, "| a\\|b |\n") {
		t.Fatalf("newline in a task broke the row:\n%s", md)
	}
}

const yamlCases = `
- id: payments
  task: Add idempotency to payment callback processing
  expect:
    - src/main/payments/
    - CallbackController.java
  forbid:
    - unrelated/marketing/
  max_tokens: 4000
- id: auth
  task: Where is the login rate limit applied?
  expect:
    - RateLimiter.java
`

const jsonCases = `[
  {"id": "payments", "task": "Add idempotency to payment callback processing",
   "expect": ["src/main/payments/", "CallbackController.java"],
   "forbid": ["unrelated/marketing/"], "max_tokens": 4000},
  {"id": "auth", "task": "Where is the login rate limit applied?",
   "expect": ["RateLimiter.java"]}
]`

func TestLoadCasesReadsBothYAMLAndJSON(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"yaml list", yamlCases},
		{"json list", jsonCases},
		{"yaml under a cases key", "cases:\n" + indent(yamlCases)},
		{"json under a cases key", `{"cases": ` + jsonCases + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadCases([]byte(tc.src))
			if err != nil {
				t.Fatalf("LoadCases: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("loaded %d cases, want 2", len(got))
			}
			if got[0].ID != "payments" || got[1].ID != "auth" {
				t.Fatalf("ids = %q, %q", got[0].ID, got[1].ID)
			}
			if got[0].MaxTokens != 4000 {
				t.Errorf("max_tokens = %d, want 4000", got[0].MaxTokens)
			}
			if !equal(got[0].Forbid, []string{"unrelated/marketing/"}) {
				t.Errorf("forbid = %v", got[0].Forbid)
			}
			if len(got[0].Expect) != 2 {
				t.Errorf("expect = %v", got[0].Expect)
			}
			if err := Validate(got); err != nil {
				t.Errorf("loaded cases do not validate: %v", err)
			}
		})
	}
}

func TestLoadCasesRejectsAdversarialInputWithoutPanicking(t *testing.T) {
	deepFlow := strings.Repeat("[", 300000) + strings.Repeat("]", 300000)

	var deepBlock strings.Builder
	for i := range 400 {
		deepBlock.WriteString(strings.Repeat(" ", i*2) + "k:\n")
	}

	tests := []struct{ name, src string }{
		{"empty", ""},
		{"whitespace only", "   \n\t\n"},
		{"truncated yaml", "- id: a\n  task: \"unterminated\n  expect:\n    - x"},
		{"truncated json", `[{"id": "a", "task": "t", "expect": ["x"`},
		{"json with a bad type", `[{"id": "a", "task": "t", "expect": 7}]`},
		{"not a case file at all", "just a sentence, not YAML at all: [ }"},
		{"empty list", "[]"},
		{"empty cases key", "cases:\n"},
		{"deeply nested flow", deepFlow},
		{"deeply nested block", deepBlock.String()},
		{"huge", strings.Repeat("- id: a\n  task: t\n  expect: [x]\n", 300000)},
		{"nul bytes", "\x00\x00\x00\x00"},
		{"bom then junk", "\xef\xbb\xbf\x01\x02:::"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("LoadCases panicked on %s input: %v", tc.name, r)
				}
			}()
			got, err := LoadCases([]byte(tc.src))
			if err == nil {
				t.Fatalf("LoadCases accepted %s input, returning %d cases", tc.name, len(got))
			}
			if got != nil {
				t.Fatalf("LoadCases returned cases alongside an error: %v", got)
			}
		})
	}
}

func TestLoadCasesRejectsAnEmptyCaseListAsALoadError(t *testing.T) {
	// A zero-case suite reports "0 of 0 passed" and reads as green, so it is an
	// error at load time rather than an empty result.
	if _, err := LoadCases([]byte("cases: []")); err == nil {
		t.Fatal("LoadCases accepted a file with no cases")
	}
}

func TestMistypedExpectKeySurfacesThroughValidate(t *testing.T) {
	got, err := LoadCases([]byte("- id: a\n  task: t\n  expects:\n    - Repo.java\n"))
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if err := Validate(got); err == nil {
		t.Fatal("a mistyped expect key produced a case that measures nothing and validated")
	}
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
