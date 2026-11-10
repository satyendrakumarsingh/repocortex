package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end tests for the last three surfaces. Each drives the real engine
// against a real index, because every defect worth catching here lives in the
// seam between the packages rather than inside any one of them.

func finalFixture(t *testing.T) string {
	t.Helper()
	root := isolate(t)
	files := map[string]string{
		"src/CallbackController.java": `package com.pay.controller;

@RestController
@RequestMapping("/payment")
public class CallbackController {
    @PostMapping("/callback")
    public void handleCallback(String id) {
        service.updateStatus(id);
    }
}`,
		"src/TransactionService.java": `package com.pay.service;

@Service
public class TransactionService {
    public void updateStatus(String id) {
        jdbc.update("UPDATE transactions SET status = ? WHERE id = ?", id);
    }
}`,
		"src/CallbackControllerTest.java": `package com.pay.controller;

public class CallbackControllerTest {
    public void handlesCallback() {}
}`,
		"db/V1__init.sql": "CREATE TABLE transactions (id VARCHAR(64) PRIMARY KEY, status VARCHAR(32));",
		"unrelated/MarketingReport.java": `package com.marketing;

public class MarketingReport {
    public String render() { return "report"; }
}`,
	}
	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return root
}

// ---------------------------------------------------------------- compile

func TestCompileEmitsNoConstraintWithoutEvidence(t *testing.T) {
	root := finalFixture(t)

	out, err := run(t, root, "compile", "Add idempotency to payment callback processing", "--json")
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, out)
	}
	var c struct {
		Constraints []struct{ Text, Evidence string } `json:"constraints"`
		Risks       []struct{ Text, Evidence string } `json:"risks"`
		Unknowns    []string                          `json:"unknowns"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("non-JSON: %v\n%s", err, out)
	}
	// The rule the whole package rests on: a constraint that cannot cite where
	// it came from is an opinion, and an agent cannot check an opinion.
	for _, con := range c.Constraints {
		if strings.TrimSpace(con.Evidence) == "" {
			t.Errorf("constraint with no evidence: %q", con.Text)
		}
	}
	for _, r := range c.Risks {
		if strings.TrimSpace(r.Evidence) == "" {
			t.Errorf("risk with no evidence: %q", r.Text)
		}
	}
	// A contract that pretends to be complete is worse than one naming its gaps.
	if len(c.Unknowns) == 0 {
		t.Error("no unknowns reported; the contract claims completeness it cannot have")
	}
}

func TestCompileDerivesConstraintsFromTheIndex(t *testing.T) {
	root := finalFixture(t)
	out, err := run(t, root, "compile", "Add idempotency to payment callback processing")
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, out)
	}
	// A test file in scope must produce the tests-still-pass constraint, and a
	// table in scope the migration constraint. Both are derived from facts the
	// index holds, not from the wording of the request.
	if !strings.Contains(out, "tests") {
		t.Errorf("a test file in scope produced no testing constraint:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "migration") {
		t.Errorf("a table in scope produced no migration constraint:\n%s", out)
	}
	if !strings.Contains(out, "## Goal") || !strings.Contains(out, "## Scope") {
		t.Errorf("the contract is missing its core sections:\n%s", out)
	}
}

func TestCompileIsDeterministic(t *testing.T) {
	root := finalFixture(t)
	var first string
	for i := 0; i < 5; i++ {
		out, err := run(t, root, "compile", "Add idempotency to payment callback processing")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatal("compile output varies between runs; a contract that changes " +
				"without the code changing cannot be diffed or reviewed")
		}
	}
}

func TestCompileRejectsAnEmptyIntent(t *testing.T) {
	root := finalFixture(t)
	if _, err := run(t, root, "compile", "   "); err == nil {
		t.Fatal("an empty intent was accepted")
	}
}

// ---------------------------------------------------------------- bench

func TestBenchDetectsEveryFailureMode(t *testing.T) {
	root := finalFixture(t)
	cases := filepath.Join(t.TempDir(), "cases.yml")
	if err := os.WriteFile(cases, []byte(`
- id: missing-expectation
  task: Add idempotency to payment callback processing
  expect: [ThisDoesNotExistAnywhere]
- id: forbidden-appears
  task: Add idempotency to payment callback processing
  expect: [CallbackController]
  forbid: [TransactionService]
- id: over-budget
  task: Add idempotency to payment callback processing
  expect: [CallbackController]
  max_tokens: 5
`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "bench", cases)
	if err != nil {
		t.Fatalf("bench: %v\n%s", err, out)
	}
	// A benchmark that cannot fail measures nothing, and reads green while
	// doing it — the most dangerous defect this file can have.
	if strings.Contains(out, "3/3 passed") {
		t.Fatalf("deliberately-failing cases all passed:\n%s", out)
	}
	if !strings.Contains(out, "0/3 passed") {
		t.Errorf("expected every case to fail:\n%s", out)
	}
	if !strings.Contains(out, "forbidden") {
		t.Errorf("a forbidden item retrieved at full recall was not surfaced:\n%s", out)
	}
}

func TestBenchFailOnRegressionGatesCI(t *testing.T) {
	root := finalFixture(t)
	cases := filepath.Join(t.TempDir(), "cases.yml")
	if err := os.WriteFile(cases, []byte("- id: c\n  task: t\n  expect: [NopeNotHere]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, root, "bench", cases, "--fail-on-regression"); err == nil {
		t.Error("--fail-on-regression did not fail on a failing case")
	}
	if _, err := run(t, root, "bench", cases); err != nil {
		t.Error("bench without --fail-on-regression should report, not fail")
	}
}

func TestBenchRejectsAVacuousCase(t *testing.T) {
	root := finalFixture(t)
	cases := filepath.Join(t.TempDir(), "cases.yml")
	// A case with nothing to expect passes vacuously and makes a suite look
	// green while measuring nothing.
	if err := os.WriteFile(cases, []byte("- id: empty\n  task: something\n  expect: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, root, "bench", cases); err == nil {
		t.Fatal("a case with no expectations was accepted")
	}
}

// ---------------------------------------------------------------- trace

func otelExport(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "otel.json")
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"pay"}}]},
"scopeSpans":[{"spans":[
{"traceId":"5b8aa5a2d2c872e8321cf37308d69df2","spanId":"051581bf3cb55c13","name":"POST /payment/callback",
 "startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000042000000",
 "attributes":[{"key":"http.route","value":{"stringValue":"/payment/callback"}}]},
{"traceId":"5b8aa5a2d2c872e8321cf37308d69df2","spanId":"1122334455667788","name":"UPDATE transactions",
 "startTimeUnixNano":1700000000010000000,"endTimeUnixNano":1700000000020000000,
 "attributes":[{"key":"db.sql.table","value":{"stringValue":"transactions"}},
               {"key":"db.statement","value":{"stringValue":"UPDATE transactions -- pw=hunter2SECRET"}}]}
]}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTraceCorrelatesWithTheGraph(t *testing.T) {
	root := finalFixture(t)

	out, err := run(t, root, "trace", otelExport(t))
	if err != nil {
		t.Fatalf("trace: %v\n%s", err, out)
	}
	if !strings.Contains(out, "POST /payment/callback") {
		t.Errorf("an http.route did not match the indexed endpoint:\n%s", out)
	}
	if !strings.Contains(out, "transactions") {
		t.Errorf("a db.sql.table did not match the indexed table:\n%s", out)
	}
	// Every match must say HOW, because they are not equally strong.
	if !strings.Contains(out, "route") || !strings.Contains(out, "table") {
		t.Errorf("matches do not report their evidence:\n%s", out)
	}
}

// TestTraceNeverLeaksASpanAttribute is the security property. Spans carry SQL
// statements, URLs with tokens and request bodies; only the correlation keys
// may survive parsing.
func TestTraceNeverLeaksASpanAttribute(t *testing.T) {
	root := finalFixture(t)
	export := otelExport(t)

	for _, args := range [][]string{{"trace", export}, {"trace", export, "--json"}} {
		out, err := run(t, root, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out, "hunter2SECRET") {
			t.Errorf("%v leaked a span attribute value:\n%s", args, out)
		}
	}
}

func TestTraceRejectsBadInput(t *testing.T) {
	root := finalFixture(t)
	dir := t.TempDir()

	// Not a trace export at all. The error must name what IS accepted, or the
	// reader has to guess which of two formats they got wrong.
	unrecognised := filepath.Join(dir, "unrecognised.json")
	if err := os.WriteFile(unrecognised, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A well-formed export that happens to carry no spans is a different
	// failure with a different cause — usually a sampling window that caught
	// nothing — and must not be reported as a malformed file.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"resourceSpans":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{
		{filepath.Join(dir, "nope.json"), "read trace export"},
		{dir, "is a directory"},
		{unrecognised, "OTLP/JSON"},
		{empty, "no spans"},
	} {
		_, err := run(t, root, "trace", tc.path)
		if err == nil {
			t.Errorf("%s accepted", tc.path)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q does not contain %q", err, tc.want)
		}
	}
}
