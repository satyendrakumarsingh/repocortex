package capability

import "testing"

func TestIsTestPath(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"svc/status_test.go":             true,
		"svc/status.go":                  false,
		"api/test_handler.py":            true,
		"api/handler_test.py":            true,
		"web/Button.spec.tsx":            true,
		"web/Button.test.ts":             true,
		"web/Button.tsx":                 false,
		"src/main/java/PaymentTest.java": true,
		"src/main/java/PaymentIT.java":   true,
		"src/main/java/Payment.java":     false,
		"tests/helpers/fixture.go":       true,
		"__tests__/setup.js":             true,
		"e2e/checkout.ts":                true,
		"tests/OrderTest.php":            true,
		"src/Order.php":                  false,
		"benches/bench.rs":               true,
		"internal/tester/tester.go":      false,
		// "Tester.java" must NOT match the Test prefix rule: the prefix only
		// counts when what follows starts a new word.
		"Tester.java": false,
		"":            false,
	}
	for path, want := range cases {
		if got := IsTestPath(path); got != want {
			t.Errorf("IsTestPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsTestName(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"TestUpdate":         true,
		"BenchmarkUpdate":    true,
		"FuzzSplit":          true,
		"ExampleClient":      true,
		"test_update":        true,
		"should_fail":        true,
		"PaymentTest":        true,
		"PaymentTests":       true,
		"ParserSpec":         true,
		"Testing":            false, // "ing" does not start a new word
		"updateStatus":       false,
		"testify":            false,
		"contestParticipant": false,
	}
	for name, want := range cases {
		if got := IsTestName(name); got != want {
			t.Errorf("IsTestName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestClassifyTestKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, name string
		want       TestKind
	}{
		{"svc/status_test.go", "TestUpdate", TestKindUnit},
		{"tests/e2e/checkout_test.go", "TestFlow", TestKindE2E},
		{"tests/integration/db_test.go", "TestDB", TestKindIntegration},
		{"src/PaymentIT.java", "shouldPay", TestKindIntegration},
		{"web/Login.spec.ts", "logs in", TestKindUnit},
		// e2e wins over integration when both markers are present: the more
		// specific one is the right answer.
		{"tests/e2e/checkout_integration_test.go", "TestFlow", TestKindE2E},
		// Not a test at all: "unknown" rather than a guess.
		{"svc/status.go", "updateStatus", TestKindUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyTestKind(tc.path, tc.name); got != tc.want {
			t.Errorf("ClassifyTestKind(%q, %q) = %q, want %q", tc.path, tc.name, got, tc.want)
		}
	}
}

func TestSubjectStemAndSiblingDetection(t *testing.T) {
	t.Parallel()
	stems := map[string]string{
		"svc/status_test.go":   "status",
		"api/test_handler.py":  "handler",
		"api/handler_test.py":  "handler",
		"web/Button.spec.tsx":  "Button",
		"web/Button.test.ts":   "Button",
		"src/PaymentTest.java": "Payment",
		"src/TestPayment.java": "Payment",
		"svc/status.go":        "", // not a test file at all
	}
	for path, want := range stems {
		if got := SubjectStem(path); got != want {
			t.Errorf("SubjectStem(%q) = %q, want %q", path, got, want)
		}
	}

	if !IsSiblingTestFile("svc/status.go", "svc/status_test.go") {
		t.Error("status_test.go is not recognised as the sibling of status.go")
	}
	if !IsSiblingTestFile("web/Button.tsx", "web/Button.spec.tsx") {
		t.Error("Button.spec.tsx is not recognised as the sibling of Button.tsx")
	}
	// The whole point of the narrow definition: a test beside a file is not
	// automatically a test OF that file.
	if IsSiblingTestFile("svc/parse.go", "svc/format_test.go") {
		t.Error("format_test.go claimed as the sibling test of parse.go")
	}
	if IsSiblingTestFile("svc/status.go", "other/status_test.go") {
		t.Error("a test in another directory was treated as co-located")
	}
	if IsSiblingTestFile("svc/status.go", "svc/status.go") {
		t.Error("a file was treated as its own sibling test")
	}
}

func TestTestNameCandidates(t *testing.T) {
	t.Parallel()
	got := map[string]bool{}
	for _, c := range TestNameCandidates("updateStatus") {
		got[c] = true
	}
	for _, want := range []string{
		"TestUpdateStatus", "test_update_status", "UpdateStatusTest",
		"UpdateStatusSpec", "updateStatusTest",
	} {
		if !got[want] {
			t.Errorf("candidate %q not generated; got %v", want, keys(got))
		}
	}
	if len(TestNameCandidates("")) != 0 {
		t.Error("candidates generated for an empty name")
	}
	// Duplicates would make the naming signal probe the same prefix twice.
	seen := map[string]int{}
	for _, c := range TestNameCandidates("Update") {
		seen[c]++
		if seen[c] > 1 {
			t.Fatalf("duplicate candidate %q", c)
		}
	}
}

func TestCamelToSnakeKeepsAcronymsTogether(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"updateStatus": "update_status",
		"HTTPServer":   "http_server",
		"ID":           "id",
		"parse":        "parse",
		"":             "",
	}
	for in, want := range cases {
		if got := camelToSnake(in); got != want {
			t.Errorf("camelToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConventionalTestSuffix(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":        true,  // exact match
		"_NotOK":  true,  // Go subtest convention
		"2":       false, // digits let `Sha25` claim TestSha256
		"Status":  false,
		"Handler": false,
	}
	for rest, want := range cases {
		if got := ConventionalTestSuffix(rest); got != want {
			t.Errorf("ConventionalTestSuffix(%q) = %v, want %v", rest, got, want)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
