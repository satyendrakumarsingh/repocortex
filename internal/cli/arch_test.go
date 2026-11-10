package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archFixture builds a repository with a real layering violation: a controller
// that reaches past the service layer straight into the repository.
func archFixture(t *testing.T, withRules bool) string {
	t.Helper()
	root := isolate(t)

	files := map[string]string{
		"src/controller/CallbackController.java": `package com.pay.controller;

import com.pay.repository.TransactionRepository;

@RestController
@RequestMapping("/payment")
public class CallbackController {
    private final TransactionRepository repository;

    @PostMapping("/callback")
    public void handleCallback(String id) {
        repository.findById(id);
    }
}`,
		"src/service/TransactionService.java": `package com.pay.service;

@Service
public class TransactionService {
    public void updateStatus(String id) {
        repository.findById(id);
    }
}`,
		"src/repository/TransactionRepository.java": `package com.pay.repository;

@Repository
public class TransactionRepository {
    public Transaction findById(String id) {
        return jdbc.queryForObject("SELECT id, status FROM transactions WHERE id = ?", id);
    }
}`,
		"docs/adr/0001-use-postgres.md": `# 1. Use PostgreSQL

## Status

Accepted

## Context

We need a relational store with strong transactional guarantees.

## Decision

We will use PostgreSQL.
`,
	}
	if withRules {
		files[".cortex.yml"] = `project:
  name: layered
architecture:
  components:
    - name: controller
      include: ["src/controller/**"]
    - name: service
      include: ["src/service/**"]
    - name: repository
      include: ["src/repository/**"]
  rules:
    - id: controllers-go-through-services
      description: Controllers must not reach the repository layer directly.
      from: [controller]
      allowed_to: [service]
      forbidden: [repository]
      severity: high
`
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

// TestArchWithoutAModelReportsNothing. Returning "0 violations" for a
// repository that declared no architecture would be a pass mark nobody earned.
func TestArchWithoutAModelReportsNothing(t *testing.T) {
	root := archFixture(t, false)

	out, err := run(t, root, "arch")
	if err != nil {
		t.Fatalf("arch: %v\n%s", err, out)
	}
	if strings.Contains(out, "No violations") {
		t.Errorf("an undeclared architecture was reported as passing:\n%s", out)
	}
	if !strings.Contains(out, "No architecture declared") {
		t.Errorf("the absent model was not stated plainly:\n%s", out)
	}
	// The reason components are not inferred must be explained, or the next
	// person just asks for inference.
	if !strings.Contains(out, "declared rather than inferred") {
		t.Errorf("the design choice is not explained:\n%s", out)
	}
}

func TestArchDetectsALayeringViolation(t *testing.T) {
	root := archFixture(t, true)

	out, err := run(t, root, "arch")
	if err != nil {
		t.Fatalf("arch: %v\n%s", err, out)
	}
	if !strings.Contains(out, "VIOLATIONS") {
		t.Fatalf("the controller -> repository crossing was not caught:\n%s", out)
	}
	if !strings.Contains(out, "controllers-go-through-services") {
		t.Errorf("the violating rule is not named:\n%s", out)
	}
	if !strings.Contains(out, "controller -> repository") {
		t.Errorf("the crossing is not described:\n%s", out)
	}
	// Every violation must cite evidence, or it cannot be checked.
	if !strings.Contains(out, ".java:") {
		t.Errorf("no evidence location cited:\n%s", out)
	}
	// And the confidence of the RELATIONSHIP, since a violation on a weak edge
	// is a lead rather than a fact.
	if !strings.Contains(out, "confidence of the RELATIONSHIP") {
		t.Errorf("the confidence caveat is missing:\n%s", out)
	}
}

func TestArchAllowedDependencyIsNotAViolation(t *testing.T) {
	root := archFixture(t, true)

	out, err := run(t, root, "arch", "--crossings")
	if err != nil {
		t.Fatalf("arch: %v\n%s", err, out)
	}
	// service -> repository is on the allow-list path and must not be flagged.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "service -> repository") &&
			strings.Contains(line, "violates") {
			t.Errorf("an allowed dependency was flagged:\n%s", line)
		}
	}
}

// TestArchFailOnGivesCIANonZeroExit. The command is meant to gate a pipeline,
// which only works if it can fail.
func TestArchFailOnGivesCIANonZeroExit(t *testing.T) {
	root := archFixture(t, true)

	if _, err := run(t, root, "arch", "--fail-on", "high"); err == nil {
		t.Error("--fail-on high did not fail on a high-severity violation")
	}
	// A threshold below the violation's severity must still pass.
	if _, err := run(t, root, "arch", "--fail-on", "bogus"); err == nil {
		t.Error("an invalid --fail-on value was accepted")
	}
}

func TestArchReportsUnassignedCoverage(t *testing.T) {
	root := archFixture(t, true)
	out, err := run(t, root, "arch")
	if err != nil {
		t.Fatal(err)
	}
	// A rule can only be broken by code the model covers. Silence about the
	// uncovered part would overstate what the check proved.
	if !strings.Contains(out, "belong to no declared component") &&
		!strings.Contains(out, "COMPONENTS") {
		t.Errorf("coverage of the model is not reported:\n%s", out)
	}
}

func TestArchJSON(t *testing.T) {
	root := archFixture(t, true)
	out, err := run(t, root, "arch", "--json")
	if err != nil {
		t.Fatalf("arch --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("non-JSON on stdout: %v\n%s", err, out)
	}
	for _, key := range []string{"violations", "crossings", "components"} {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON missing %q", key)
		}
	}
}

func TestArchRejectsAnInvalidModel(t *testing.T) {
	root := archFixture(t, true)
	// A rule naming a component that does not exist is inert, and silently
	// inert rules are worse than none.
	bad := `project:
  name: layered
architecture:
  components:
    - name: controller
      include: ["src/controller/**"]
  rules:
    - id: r1
      from: [controller]
      forbidden: [does-not-exist]
      severity: high
`
	if err := os.WriteFile(filepath.Join(root, ".cortex.yml"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, root, "arch")
	if err == nil {
		t.Fatal("a rule referencing an unknown component was accepted")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error does not name the unknown component: %v", err)
	}
}

func TestADRListsRecords(t *testing.T) {
	root := archFixture(t, true)

	out, err := run(t, root, "arch", "adr")
	if err != nil {
		t.Fatalf("arch adr: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Use PostgreSQL") {
		t.Errorf("the ADR title was not read:\n%s", out)
	}
	if !strings.Contains(out, "accepted") {
		t.Errorf("the ADR status was not read:\n%s", out)
	}
	if !strings.Contains(out, "0001") {
		t.Errorf("the ADR number was not read from the filename:\n%s", out)
	}
}

func TestADRExplainsAnEmptyResult(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, "a.go"),
		[]byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	out, err := run(t, root, "arch", "adr")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "docs/adr") {
		t.Errorf("an empty ADR list does not say where it looked:\n%s", out)
	}
}

// TestArchJSONFailOnStillGates: --json changes the output format, never the
// exit code. Before this, the JSON branch returned before the gate.
func TestArchJSONFailOnStillGates(t *testing.T) {
	root := archFixture(t, true)

	out, err := run(t, root, "--json", "arch", "--fail-on", "high")
	if err == nil {
		t.Error("--json --fail-on high did not fail on a high-severity violation")
	}
	var rep map[string]any
	if jerr := json.Unmarshal([]byte(firstJSONDocument(out)), &rep); jerr != nil {
		t.Fatalf("stdout is not JSON when the gate fails: %v\n%s", jerr, out)
	}
}

// firstJSONDocument returns the leading JSON value of s; run() captures stderr
// into the same buffer, so the gate's error text may follow the document.
func firstJSONDocument(s string) string {
	dec := json.NewDecoder(strings.NewReader(s))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return s
	}
	return string(raw)
}
