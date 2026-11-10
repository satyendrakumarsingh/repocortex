package web

import (
	"net/http"
	"strings"
	"testing"
)

// TestWebTestsProvesCoverageFromTheGraphAndCarriesTierAndEvidence.
//
// The positive claim: a test reaches a symbol through a recorded edge. Every
// such row must arrive with the edge kind, the tier and the file:line, because
// a coverage claim without them is the filename guess this screen exists to
// refuse.
func TestWebTestsProvesCoverageFromTheGraphAndCarriesTierAndEvidence(t *testing.T) {
	t.Parallel()
	srv, _, _ := mindFixture(t)

	var got TestsResponse
	rec := get(t, srv, "/api/tests", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !got.Meta.Indexed {
		t.Fatal("meta.indexed is false over a committed run")
	}

	if len(got.Covered) != 1 {
		t.Fatalf("covered_symbols = %+v, want exactly `charge`", got.Covered)
	}
	cov := got.Covered[0]
	if cov.Name != "charge" {
		t.Errorf("covered symbol = %q, want charge", cov.Name)
	}
	if cov.Coverage != CoverageProven {
		t.Errorf("coverage = %q, want %q", cov.Coverage, CoverageProven)
	}
	if len(cov.ProvenBy) != 1 {
		t.Fatalf("proven_by = %+v, want exactly TestCharge", cov.ProvenBy)
	}
	proof := cov.ProvenBy[0]
	if proof.Name != "TestCharge" {
		t.Errorf("proving test = %q, want TestCharge", proof.Name)
	}
	if proof.Via != "CALLS" {
		t.Errorf("via = %q, want CALLS — the edge kind is the claim", proof.Via)
	}
	if proof.Confidence != "high" {
		t.Errorf("confidence = %q, want high", proof.Confidence)
	}
	if proof.Evidence.FilePath == "" || proof.Evidence.Line == 0 {
		t.Errorf("evidence = %+v, want a real file:line", proof.Evidence)
	}
	if proof.TestKind == "" {
		t.Error("test_kind is empty; `unknown` is a value, an empty string is not")
	}

	// Both test files are listed, including the one whose coverage the graph
	// does not prove — dropping it would make a parser or binding gap look like
	// an absence of tests.
	paths := map[string]TestFileRow{}
	for _, f := range got.Files {
		paths[f.Path] = f
	}
	for _, want := range []string{"internal/billing/charge_test.go", "internal/billing/orphan_test.go"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("%s is missing from test_files: %+v", want, got.Files)
		}
	}
	if paths["internal/billing/charge_test.go"].ReachesSymbols != 1 {
		t.Errorf("charge_test.go reaches_symbols = %d, want 1",
			paths["internal/billing/charge_test.go"].ReachesSymbols)
	}
	if paths["internal/billing/orphan_test.go"].ReachesSymbols != 0 {
		t.Errorf("orphan_test.go reaches_symbols = %d, want 0",
			paths["internal/billing/orphan_test.go"].ReachesSymbols)
	}

	if got.Counts.TestFiles != 2 {
		t.Errorf("counts.test_files = %d, want 2", got.Counts.TestFiles)
	}
	if got.Counts.SymbolsWithProvenTest != 1 {
		t.Errorf("counts.symbols_with_a_proven_covering_test = %d, want 1", got.Counts.SymbolsWithProvenTest)
	}
	if got.Counts.NonTestSymbols == 0 {
		t.Error("counts.non_test_symbols is zero; the denominator is what makes the numerator readable")
	}
	if got.Counts.SymbolsWithoutProvenTest != got.Counts.NonTestSymbols-got.Counts.SymbolsWithProvenTest {
		t.Errorf("the two coverage counts do not sum to the denominator: %+v", got.Counts)
	}
	if got.RepoScope == "" || !strings.Contains(got.RepoScope, "depth 1") {
		t.Errorf("scope does not state that the repository-level list is depth 1: %q", got.RepoScope)
	}
}

// TestWebTestsNeverPublishesAnUntestedVerdict is the claim this whole screen
// exists for.
//
// Not a ban on the WORD — the coverage legend and the capability layer's own
// completeness caveats both use it, inside sentences that exist to deny it
// ("it does NOT mean the symbol is untested"), and a test that failed on those
// would push the honest sentence out of the payload. What must not exist is a
// FIELD or a VALUE a client can render as a verdict: `"untested": true` beside
// a symbol name is a claim about the code, and blast.go's TestSection publishes
// exactly that shape, which is why this screen has its own type.
func TestWebTestsNeverPublishesAnUntestedVerdict(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/api/tests", "/api/tests?symbol=refund", "/api/tests?symbol=charge"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := mindFixture(t)
			rec := get(t, srv, path, nil)
			body := rec.Body.String()
			if strings.Contains(body, `"untested"`) {
				t.Errorf("GET %s: the payload carries an `untested` field or value. \"No covering "+
					"test was proven\" is a statement about this index; \"untested\" is a claim "+
					"about the code, and the two must not be published as the same thing.", path)
			}
		})
	}

	srv, _, _ := mindFixture(t)
	var got TestsResponse
	get(t, srv, "/api/tests", &got)
	if len(got.Verdicts) != 3 {
		t.Fatalf("coverage_legend has %d verdicts, want 3", len(got.Verdicts))
	}
	for _, v := range got.Verdicts {
		if v.Verdict == "untested" {
			t.Error("the coverage vocabulary contains an `untested` verdict")
		}
		if v.Means == "" || v.DoesNotMean == "" {
			t.Errorf("verdict %q does not say both what it means and what it does not: %+v", v.Verdict, v)
		}
	}
	// And the negative verdict must spell the distinction out, because that is
	// the sentence a reader copies into a decision.
	var sawDenial bool
	for _, v := range got.Verdicts {
		if v.Verdict == CoverageNotProven && strings.Contains(v.DoesNotMean, "does NOT mean the symbol is untested") {
			sawDenial = true
		}
	}
	if !sawDenial {
		t.Error("the `no_covering_test_proven` verdict does not state that it is not a claim of absence")
	}
}

// TestWebTestsPerSymbolDistinguishesProofFromConvention.
//
// The table is the whole point of the screen: three symbols, three different
// verdicts, and the verdict for a symbol nothing proved must say in its own
// words that it is a statement about the index rather than about the code.
func TestWebTestsPerSymbolDistinguishesProofFromConvention(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		symbol      string
		wantVerdict string
		wantProven  int
		wantPhrase  string
	}{
		{
			name:        "proven by a recorded edge",
			symbol:      "charge",
			wantVerdict: CoverageProven,
			wantProven:  1,
			wantPhrase:  "edges this index recorded",
		},
		{
			name:        "nothing proved it",
			symbol:      "refund",
			wantVerdict: CoverageNotProven,
			wantProven:  0,
			wantPhrase:  "does NOT say",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _, _ := mindFixture(t)

			var got TestsResponse
			rec := get(t, srv, "/api/tests?symbol="+tc.symbol, &got)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got.Symbol == nil {
				t.Fatalf("no per-symbol answer for %q", tc.symbol)
			}
			cov := got.Symbol
			if cov.Verdict != tc.wantVerdict {
				t.Errorf("coverage = %q, want %q (detail: %q)", cov.Verdict, tc.wantVerdict, cov.VerdictDetail)
			}
			if cov.ProvenCount != tc.wantProven {
				t.Errorf("proven_count = %d, want %d", cov.ProvenCount, tc.wantProven)
			}
			if !strings.Contains(cov.VerdictDetail, tc.wantPhrase) {
				t.Errorf("coverage_detail = %q, want it to contain %q", cov.VerdictDetail, tc.wantPhrase)
			}
			if cov.Target == nil || cov.Target.Name != tc.symbol {
				t.Errorf("target = %+v, want %q", cov.Target, tc.symbol)
			}
			if cov.Proven == nil || cov.Suggested == nil {
				t.Error("proven_by_graph or suggested_by_convention is null rather than an array; " +
					"the client must not have to tell null from empty on a coverage claim")
			}
			// Proof and convention are never merged.
			for _, hit := range cov.Proven {
				if string(hit.Strongest) != "graph" {
					t.Errorf("proven_by_graph carries a %q hit; a naming convention is not proof",
						hit.Strongest)
				}
			}
			for _, hit := range cov.Suggested {
				if string(hit.Strongest) == "graph" {
					t.Errorf("suggested_by_convention carries a graph hit, which is proof and belongs "+
						"in the other list: %+v", hit)
				}
			}
		})
	}
}

// TestWebTestsUnknownSymbolAndAmbiguityAnswerWithGuidance: neither is a 500,
// and neither is a guess.
func TestWebTestsUnknownSymbolAndAmbiguityAnswerWithGuidance(t *testing.T) {
	t.Parallel()
	srv, _, _ := mindFixture(t)

	var got TestsResponse
	rec := get(t, srv, "/api/tests?symbol=nosuchthing", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want a 200 with guidance: %s", rec.Code, rec.Body.String())
	}
	if got.Symbol != nil {
		t.Errorf("a per-symbol answer was returned for a symbol that does not exist: %+v", got.Symbol)
	}
	if got.Help == nil || !strings.Contains(got.Help.Title, "No symbol named") {
		t.Errorf("guidance does not name the missing symbol: %+v", got.Help)
	}
}

// TestWebTestsOnARepositoryWithNoTestFilesExplainsItself is the empty case.
func TestWebTestsOnARepositoryWithNoTestFilesExplainsItself(t *testing.T) {
	t.Parallel()
	srv, _, _ := bareFixture(t)

	var got TestsResponse
	rec := get(t, srv, "/api/tests", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(got.Files) != 0 || len(got.Covered) != 0 {
		t.Fatalf("expected nothing, got %d file(s) and %d covered symbol(s)", len(got.Files), len(got.Covered))
	}
	if got.Help == nil {
		t.Fatal("an empty payload was returned with no guidance")
	}
	if !strings.Contains(got.Help.Detail, "not a claim that this repository has no tests") {
		t.Errorf("guidance does not distinguish what the index saw from what the code has: %q", got.Help.Detail)
	}
	if len(got.Help.Steps) == 0 {
		t.Error("guidance names no next step")
	}
	if got.Next.Label == "" {
		t.Error("the empty screen ends in no next action")
	}

	body := raw(t, srv, "/api/tests")
	for _, field := range []string{"test_files", "covered_symbols", "coverage_legend"} {
		if _, ok := body[field].([]any); !ok {
			t.Errorf("%s did not serialise as an array: %T", field, body[field])
		}
	}
}

// TestWebTestsCountsWhatTheFloorHid: a coverage screen that silently drops weak
// edges is a coverage screen that reports absence it did not establish.
func TestWebTestsCountsWhatTheFloorHid(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var high TestsResponse
	get(t, srv, "/api/tests?min_confidence=exact", &high)

	if high.Counts.SymbolsWithProvenTest != 0 {
		t.Fatalf("at floor `exact` the fixture's high-confidence test edge should be hidden, got %d covered",
			high.Counts.SymbolsWithProvenTest)
	}
	hidden := omissionsFor(high.Omissions, ReasonBelowConfidenceFloor)
	if len(hidden) == 0 {
		t.Fatal("edges were excluded by the floor and nothing recorded it")
	}
	total := 0
	for _, om := range hidden {
		total += om.Count
		if om.Remedy == "" {
			t.Error("a floor omission carries no remedy")
		}
		if !strings.Contains(om.Detail, "not making") && !strings.Contains(om.Detail, "may be reached") {
			t.Errorf("the floor omission does not say what its exclusion costs: %q", om.Detail)
		}
	}
	if total == 0 {
		t.Error("the hidden-by-floor total is zero while coverage went from proven to unproven")
	}

	var low TestsResponse
	get(t, srv, "/api/tests?min_confidence=low", &low)
	if low.Counts.SymbolsWithProvenTest == 0 {
		t.Error("lowering the floor recovered nothing; the omission's remedy is not real")
	}
}
