package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// Tests for the query commands.
//
// These build a real project, index it, resolve it and then ask questions
// through the CLI exactly as a user would. Driving the whole stack rather than
// the command functions in isolation is the point: the defects that matter here
// are wiring defects — a command reading the wrong run, a filter that drops
// everything, an empty result printed as though it were an answer — and none of
// those are visible when the registry is a stub.

// queryFixture builds and indexes a small Go project, returning its root.
//
// Deliberately small and hand-written rather than a copy of a real tree, so
// that every assertion below can name the exact symbol it expects and a failure
// says which relationship broke.
func queryFixture(t *testing.T) string {
	t.Helper()
	root := isolate(t)

	files := map[string]string{
		"svc/payment.go": `package svc

// PaymentService settles payments.
type PaymentService struct{ repo *Repo }

func (s *PaymentService) Settle(id string) error {
	return s.repo.Save(id)
}

func (s *PaymentService) Refund(id string) error {
	return s.repo.Save(id)
}
`,
		"svc/repo.go": `package svc

type Repo struct{}

func (r *Repo) Save(id string) error { return nil }
`,
		"svc/unrelated.go": `package svc

// MarketingReport has nothing to do with payments.
type MarketingReport struct{}

func (m *MarketingReport) Render() string { return "report" }
`,
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

func TestQueryCommandsRefuseWithoutAnIndex(t *testing.T) {
	root := isolate(t)

	// Every query command must fail the same way, and must name the fix. A
	// command that returns "no results" when there is no index at all sends
	// the user looking for a bug in their code.
	for _, args := range [][]string{
		{"search", "anything"},
		{"symbol", "Anything"},
		{"references", "Anything"},
		{"callers", "Anything"},
		{"callees", "Anything"},
		{"context", "do something"},
	} {
		out, err := run(t, root, args...)
		if err == nil {
			t.Errorf("%v succeeded without an index:\n%s", args, out)
			continue
		}
		if !strings.Contains(err.Error(), "repocortex init") {
			t.Errorf("%v: error should name the fix, got %q", args, err)
		}
	}
}

func TestQueryCommandsRefuseWithNothingCommitted(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}

	// Distinct from "no index": the schema exists, nothing is in it. Same
	// symptom for the user, different fix, so it must be a different message.
	_, err := run(t, root, "symbol", "Anything")
	if err == nil {
		t.Fatal("symbol succeeded against an empty index")
	}
	if !strings.Contains(err.Error(), "repocortex index") {
		t.Errorf("error should point at 'repocortex index', got %q", err)
	}
}

func TestSearchFindsIndexedText(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "search", "Settle")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, out)
	}
	if !strings.Contains(out, "svc/payment.go") {
		t.Errorf("search did not find the file containing Settle:\n%s", out)
	}
}

func TestSearchExplainsAnEmptyResult(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "search", "zzzznotpresentanywhere")
	if err != nil {
		t.Fatalf("an empty search is not an error: %v", err)
	}
	// The user needs to know what search does and does not cover; "no results"
	// alone invites the conclusion that the term is absent from the repository.
	if !strings.Contains(out, "No files match") {
		t.Errorf("empty search should say so plainly:\n%s", out)
	}
	if !strings.Contains(out, "indexed files only") {
		t.Errorf("empty search must state its coverage limits:\n%s", out)
	}
}

func TestSymbolFindsDefinitionWithProvenance(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "symbol", "Settle")
	if err != nil {
		t.Fatalf("symbol: %v\n%s", err, out)
	}
	if !strings.Contains(out, "svc/payment.go") {
		t.Errorf("symbol did not report the defining file:\n%s", out)
	}
	// Provenance is not decoration. A result with no confidence attached is
	// indistinguishable from a compiler-proven one, which is the single claim
	// this product must never make loosely.
	if !strings.Contains(out, "source_code") {
		t.Errorf("symbol must report where the fact came from:\n%s", out)
	}
}

func TestSymbolExactNotFuzzy(t *testing.T) {
	root := queryFixture(t)

	// "Settl" is a prefix of a real symbol. Matching it would make every
	// symbol lookup a guess, and an agent acting on the wrong definition is
	// worse than one told nothing was found.
	out, err := run(t, root, "symbol", "Settl")
	if err != nil {
		t.Fatalf("symbol: %v", err)
	}
	if !strings.Contains(out, "No definition named") {
		t.Errorf("prefix matched a symbol; lookup must be exact:\n%s", out)
	}
	if !strings.Contains(out, "repocortex search") {
		t.Errorf("a miss should point at the command that does match partially:\n%s", out)
	}
}

func TestCallersReportsTheCallAndItsEvidence(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "callers", "Save")
	if err != nil {
		t.Fatalf("callers: %v\n%s", err, out)
	}
	// Both Settle and Refund call Save.
	for _, want := range []string{"Settle", "Refund"} {
		if !strings.Contains(out, want) {
			t.Errorf("callers of Save missing %s:\n%s", want, out)
		}
	}
	// Every edge must carry the line that justifies it, or the claim is
	// unverifiable and the user has to take it on faith.
	if !strings.Contains(out, "svc/payment.go:") {
		t.Errorf("callers must cite evidence locations:\n%s", out)
	}
}

func TestCalleesLooksTheOtherWay(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "callees", "Settle")
	if err != nil {
		t.Fatalf("callees: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Save") {
		t.Errorf("Settle calls Save; callees did not report it:\n%s", out)
	}
	// Direction must actually differ. If callees returned incoming edges, this
	// would list Settle's callers instead, and the two commands would be one
	// command with two names.
	if strings.Contains(out, "Refund") {
		t.Errorf("callees returned a caller; direction is inverted:\n%s", out)
	}
}

// TestEmptyRelationshipResultCarriesCoverage is the most important test here.
//
// "No callers" and "no callers I could resolve" have opposite consequences: the
// first says a method is safe to delete, the second says nothing at all. The
// index resolves a measured fraction of references, so the second is frequently
// the true statement, and a bare empty result silently asserts the first.
func TestEmptyRelationshipResultCarriesCoverage(t *testing.T) {
	root := queryFixture(t)

	// Nothing calls Render.
	out, err := run(t, root, "callers", "Render")
	if err != nil {
		t.Fatalf("callers: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No callers found") {
		t.Fatalf("expected an empty result:\n%s", out)
	}
	if !strings.Contains(out, "lower bound") && !strings.Contains(out, "resolve") {
		t.Errorf("an empty relationship result must state that it is not proof of absence:\n%s", out)
	}
}

func TestRelationshipQueryReportsAmbiguity(t *testing.T) {
	root := queryFixture(t)

	// Two methods named Save would be ambiguous; here only one exists, so
	// assert the inverse — a unique name must NOT produce a disambiguation
	// prompt, or the warning becomes noise nobody reads.
	out, err := run(t, root, "callers", "Save")
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	if strings.Contains(out, "definitions share the name") {
		t.Errorf("unique name reported as ambiguous:\n%s", out)
	}
}

func TestRelationshipPathFilterRejectsAMissWithContext(t *testing.T) {
	root := queryFixture(t)

	_, err := run(t, root, "callers", "Save", "--path", "nowhere")
	if err == nil {
		t.Fatal("a --path that matches nothing should be an error, not an empty answer")
	}
	// The user filtered themselves out of the result; saying so is the
	// difference between a two-second fix and a hunt for a missing symbol.
	if !strings.Contains(err.Error(), "exist elsewhere") {
		t.Errorf("error should say the symbol exists elsewhere, got %q", err)
	}
}

func TestMinConfidenceIsValidated(t *testing.T) {
	root := queryFixture(t)

	_, err := run(t, root, "callers", "Save", "--min-confidence", "bogus")
	if err == nil {
		t.Fatal("invalid --min-confidence accepted")
	}
	if !strings.Contains(err.Error(), "exact") {
		t.Errorf("error should list the valid values, got %q", err)
	}
}

func TestMinConfidenceExactDropsHeuristicEdges(t *testing.T) {
	root := queryFixture(t)

	// This resolver never emits exact confidence — that is reserved for
	// compiler-backed SCIP indexes — so filtering to exact must empty the
	// result rather than quietly returning heuristics.
	out, err := run(t, root, "callers", "Save", "--min-confidence", "exact")
	if err != nil {
		t.Fatalf("callers: %v\n%s", err, out)
	}
	if strings.Contains(out, "Settle") {
		t.Errorf("--min-confidence=exact returned a heuristic edge:\n%s", out)
	}
}

func TestContextAssemblesAPack(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "context", "add idempotency to payment settlement")
	if err != nil {
		t.Fatalf("context: %v\n%s", err, out)
	}
	// Interpretation first: almost every bad pack is a misread task, and
	// showing what was understood turns a confusing result into an obvious one.
	if !strings.Contains(out, "Understood as:") {
		t.Errorf("context must show how it read the task:\n%s", out)
	}
	if !strings.Contains(out, "PaymentService") {
		t.Errorf("a payment task did not surface PaymentService:\n%s", out)
	}
	// Every item must justify its tokens.
	if !strings.Contains(out, "WHY") {
		t.Errorf("context must explain why each item was included:\n%s", out)
	}
}

func TestContextRejectsABlankTask(t *testing.T) {
	root := queryFixture(t)

	if _, err := run(t, root, "context", "   "); err == nil {
		t.Fatal("a blank task should be an error, not an empty pack")
	}
}

// TestQueryJSONIsMachineReadable covers every command's --json path in one
// place, because the failure mode is identical for all of them: log output or a
// human-readable header leaking onto stdout and breaking `jq`.
func TestQueryJSONIsMachineReadable(t *testing.T) {
	root := queryFixture(t)

	cases := []struct {
		name string
		args []string
		key  string
	}{
		{"search", []string{"search", "Settle", "--json"}, "matches"},
		{"symbol", []string{"symbol", "Settle", "--json"}, "symbols"},
		{"callers", []string{"callers", "Save", "--json"}, "callers"},
		{"callees", []string{"callees", "Settle", "--json"}, "callees"},
		{"references", []string{"references", "Save", "--json"}, "references"},
		{"context", []string{"context", "payment settlement", "--json"}, "items"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(t, root, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("--json emitted non-JSON on stdout: %v\n%s", err, out)
			}
			if _, ok := got[tc.key]; !ok {
				t.Errorf("JSON missing %q key; got keys %v", tc.key, mapKeys(got))
			}
		})
	}
}

func mapKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestQueryReadsOnlyTheCommittedRun guards the two-phase swap from the read
// side. An in-flight index must be invisible to queries; if a query could see
// staging rows, every command would intermittently report half a repository
// while `repocortex index` was running.
func TestQueryReadsOnlyTheCommittedRun(t *testing.T) {
	root := queryFixture(t)
	ctx := context.Background()

	s, err := openQuery(ctx, &Global{cfg: mustConfig(t, root)})
	if err != nil {
		t.Fatalf("openQuery: %v", err)
	}
	defer s.close()

	var committed int
	if err := s.db.QueryRowContext(ctx,
		`SELECT committed FROM index_runs WHERE id = ?`, s.runID).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 1 {
		t.Errorf("queries opened run %d, which is not committed", s.runID)
	}
}

// mustConfig loads the config for a project root, for tests that need a Global
// without going through cobra.
func mustConfig(t *testing.T, root string) config.Config {
	t.Helper()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}
