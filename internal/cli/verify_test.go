package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exit codes ARE the output of `verify`. A CI job branches on them and never
// reads the prose, so they are the part that has to be tested — and until the
// command stopped calling os.Exit from inside RunE, they were the one part that
// could not be. These tests exist to keep that true.

// seedVerifyRepo indexes a tiny repository and returns its project root.
func seedVerifyRepo(t *testing.T) string {
	t.Helper()
	root := isolate(t)

	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("store.go", `package app

type Repo struct{}

func (r *Repo) CreateOrder(id string) error { return nil }
func (r *Repo) helper() {}
`)
	write("service.go", `package app

type Service struct{ repo *Repo }

func (s *Service) PlaceOrder(id string) error { return s.repo.CreateOrder(id) }
`)

	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := run(t, root, "index"); err != nil {
		t.Fatalf("index: %v\n%s", err, out)
	}
	if out, err := run(t, root, "resolve"); err != nil {
		t.Fatalf("resolve: %v\n%s", err, out)
	}
	return root
}

// contractFile compiles a contract and writes it, returning the path.
func contractFile(t *testing.T, root, intent string) string {
	t.Helper()
	out, err := run(t, root, "compile", intent, "--json")
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, out)
	}
	// Prove it parses as a contract before a later failure can be blamed on the
	// verify side of the loop.
	var probe map[string]any
	if err := json.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("compile --json did not emit JSON: %v\n%s", err, out)
	}
	p := filepath.Join(t.TempDir(), "contract.json")
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// exitCodeOf reports the code an error asks the process to exit with, using the
// same rule Execute applies.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ec *ExitCodeError
	if errors.As(err, &ec) {
		return ec.Code
	}
	return 1
}

// TestVerifyExitsZeroWhenTheChangeHonoursTheContract is the green path: the
// files the contract scoped are the files that changed.
func TestVerifyExitsZeroWhenTheChangeHonoursTheContract(t *testing.T) {
	root := seedVerifyRepo(t)
	c := contractFile(t, root, "Add idempotency to order creation")

	out, err := run(t, root, "verify", "--contract", c, "--changed", "store.go")
	if code := exitCodeOf(err); code != 0 {
		t.Fatalf("exit = %d, want 0 for an in-scope change\n%s", code, out)
	}
	if !strings.Contains(out, "VERIFY:") {
		t.Errorf("report was not rendered:\n%s", out)
	}
}

// TestVerifyStatesWhatItCouldNotCheck is the anti-false-confidence guarantee.
//
// A report that finds nothing must not read the same as a report that could not
// look. Every run says which questions the repository gave it no way to answer —
// here, that no architecture is declared — because a gate that quietly narrows
// its own scope stops gating without ever saying so.
func TestVerifyStatesWhatItCouldNotCheck(t *testing.T) {
	root := seedVerifyRepo(t)
	c := contractFile(t, root, "Add idempotency to order creation")

	out, _ := run(t, root, "verify", "--contract", c, "--changed", "store.go")

	if !strings.Contains(out, "UNKNOWN") {
		t.Fatalf("a clean report named nothing it could not check:\n%s", out)
	}
	if !strings.Contains(out, "declares no architecture") {
		t.Errorf("the report did not say the repository declares no architecture:\n%s", out)
	}
}

// TestVerifyRefusesToInventAContract: a missing contract is exit 1 with an
// explanation, never a pass. "Nothing to check against" must never render as
// "checked and clean".
func TestVerifyRefusesToInventAContract(t *testing.T) {
	root := seedVerifyRepo(t)

	out, err := run(t, root, "verify", "--changed", "store.go")
	if err == nil {
		t.Fatalf("verify with no contract succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--contract is required") {
		t.Errorf("error does not say what is missing: %v", err)
	}
}

// TestVerifyRejectsAContractItCannotParse: a corrupt contract must be a loud
// failure, not an empty one. Unmarshalling garbage into a struct yields a valid
// zero contract, which would verify clean against any change at all — the worst
// possible outcome for a gate.
func TestVerifyRejectsAContractItCannotParse(t *testing.T) {
	root := seedVerifyRepo(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "verify", "--contract", bad, "--changed", "store.go")
	if err == nil {
		t.Fatalf("an unparseable contract verified successfully:\n%s", out)
	}
	if !strings.Contains(err.Error(), "parse contract") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// TestVerifyNeedsToKnowWhatChanged: with no change set there is no question to
// ask, and inventing one by scanning git would silently disagree with the caller
// about staged, unstaged and untracked files.
func TestVerifyNeedsToKnowWhatChanged(t *testing.T) {
	root := seedVerifyRepo(t)
	c := contractFile(t, root, "Add idempotency to order creation")

	_, err := run(t, root, "verify", "--contract", c)
	if err == nil {
		t.Fatal("verify ran with no change set")
	}
	if !strings.Contains(err.Error(), "--changed") {
		t.Errorf("error does not name the missing flag: %v", err)
	}
}

// TestVerifyReadsAChangeSetFromAFile covers the form CI actually uses, where the
// path list comes from `git diff --name-only > changed.txt`.
func TestVerifyReadsAChangeSetFromAFile(t *testing.T) {
	root := seedVerifyRepo(t)
	c := contractFile(t, root, "Add idempotency to order creation")

	list := filepath.Join(t.TempDir(), "changed.txt")
	if err := os.WriteFile(list, []byte("store.go\n\nservice.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "verify", "--contract", c, "--changed-from", list)
	if code := exitCodeOf(err); code > 1 {
		t.Fatalf("exit = %d, want 0 or 1\n%s", code, out)
	}
	if !strings.Contains(out, "VERIFY:") {
		t.Errorf("report was not rendered:\n%s", out)
	}
}

// TestVerifyRefusesTwoSourcesOfTruthForTheChangeSet: taking both flags would
// mean silently preferring one, and a gate that ignores half its input is worse
// than one that refuses to start.
func TestVerifyRefusesTwoSourcesOfTruthForTheChangeSet(t *testing.T) {
	root := seedVerifyRepo(t)
	c := contractFile(t, root, "Add idempotency to order creation")

	_, err := run(t, root, "verify", "--contract", c,
		"--changed", "store.go", "--changed-from", "list.txt")
	if err == nil {
		t.Fatal("verify accepted two conflicting change sets")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error does not explain the conflict: %v", err)
	}
}

// TestVerifyRejectsAConfidenceFloorItDoesNotUnderstand: a typo in
// --min-confidence must not be read as "no floor". Silently widening a gate
// because its argument was misspelled is the failure this rejects.
func TestVerifyRejectsAConfidenceFloorItDoesNotUnderstand(t *testing.T) {
	root := seedVerifyRepo(t)
	c := contractFile(t, root, "Add idempotency to order creation")

	_, err := run(t, root, "verify", "--contract", c,
		"--changed", "store.go", "--min-confidence", "prettysure")
	if err == nil {
		t.Fatal("an unrecognised confidence level was accepted")
	}
	if !strings.Contains(err.Error(), "exact, high, medium, low") {
		t.Errorf("error does not list the valid levels: %v", err)
	}
}

// TestAMissedCallerOnAHeuristicEdgeIsALeadNotAFact is the red path, and it pins
// the distinction the whole report is built on.
//
// `store.go` changes, the blast radius says `service.go` calls into it, and
// `service.go` did not change — so the finding is real. But this repository has
// no SCIP index, so the edge that produced it is `high`, which is measured at
// 86.8% and not proven. A finding standing on an 86.8% edge is a LEAD: the edge
// itself may be wrong, and reporting it as a fact means roughly one in eight
// failures is the tool's error rather than the change's. That is how a gate
// earns a reputation for crying wolf and gets switched off.
//
// So the default disposition is `advisory` and the exit code is 0 — and a team
// that wants the stricter gate opts in with --fail-on-leads. The escalation is a
// choice the repository makes, never one the tool makes on its behalf.
func TestAMissedCallerOnAHeuristicEdgeIsALeadNotAFact(t *testing.T) {
	root := seedVerifyRepo(t)
	c := contractFile(t, root, "Change how Repo.CreateOrder validates its identifier")

	out, err := run(t, root, "verify", "--contract", c, "--changed", "store.go")
	if code := exitCodeOf(err); code != 0 {
		t.Fatalf("exit = %d, want 0: a lead must not fail the gate by default\n%s", code, out)
	}
	if !strings.Contains(out, "VERIFY: ADVISORY") {
		t.Fatalf("want an ADVISORY verdict:\n%s", out)
	}
	if !strings.Contains(out, "[LEAD]") {
		t.Errorf("a finding on a heuristic edge was not marked as a lead:\n%s", out)
	}
	if strings.Contains(out, "[FACT]") {
		t.Errorf("a finding on an unproven edge was reported as a fact:\n%s", out)
	}
	// It must name the caller and cite the source location. "You missed
	// something" with no location is the output that gets the gate switched off.
	for _, want := range []string{"PlaceOrder", "service.go", "evidence:"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q:\n%s", want, out)
		}
	}

	// The opt-in stricter gate.
	out2, err2 := run(t, root, "verify", "--contract", c,
		"--changed", "store.go", "--fail-on-leads")
	if code := exitCodeOf(err2); code != 1 {
		t.Errorf("--fail-on-leads exit = %d, want 1\n%s", code, out2)
	}
}
