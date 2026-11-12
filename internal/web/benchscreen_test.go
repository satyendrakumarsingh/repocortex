package web

import (
	"strings"
	"testing"
)

// benchCaseFile is two cases: one that measures both halves, and one that
// declares no forbid list and therefore measures recall only.
const benchCaseFile = `
- id: hub-change
  task: change the hub function and its callers
  expect:
    - hub
  forbid:
    - MarketingReport
    - BillingLedger
- id: recall-only
  task: change the hub function and its callers
  expect:
    - hub
`

// TestBenchCarriesBothHalvesOfEveryScore is the point of this screen.
//
// Recall alone is trivially gamed by returning the whole repository, so the
// payload must carry what was EXPECTED and what was FORBIDDEN, and a score for
// each. A client that renders recall alone should not be able to, because the
// data to do better is on every row.
func TestBenchCarriesBothHalvesOfEveryScore(t *testing.T) {
	t.Parallel()
	srv, _, _, root := repoFixture(t)
	writeRepoFile(t, root, benchCasesFile, benchCaseFile)

	var got BenchResponse
	pillarGet(t, srv.handleBench, "/api/bench", &got)

	if !got.Scored {
		t.Fatalf("scored is false: case file %+v, guidance %+v", got.CaseFile, got.Help)
	}
	if got.CasesInFile != 2 || got.Scores.Cases != 2 {
		t.Fatalf("cases = %d in file / %d scored, want 2 / 2", got.CasesInFile, got.Scores.Cases)
	}

	byID := map[string]BenchCase{}
	for _, c := range got.Cases {
		byID[c.ID] = c
	}

	both, ok := byID["hub-change"]
	if !ok {
		t.Fatalf("case hub-change is missing: %+v", got.Cases)
	}
	if len(both.Expected) != 1 || both.Expected[0] != "hub" {
		t.Errorf("expected = %v, want [hub] — the case as written must travel with its score", both.Expected)
	}
	if len(both.Forbidden) != 2 {
		t.Errorf("forbidden = %v, want the two entries the case declares", both.Forbidden)
	}
	if !both.ForbidMeasured {
		t.Error("forbid_measured is false on a case that declares a forbid list")
	}
	if both.ForbidScore != 1 {
		t.Errorf("forbid_score = %v, want 1 — neither forbidden name is in this index at all",
			both.ForbidScore)
	}
	if both.Recall < 0 || both.Recall > 1 {
		t.Errorf("recall = %v, which is not a rate", both.Recall)
	}
	if both.Found == nil || both.Missing == nil || both.Violations == nil {
		t.Errorf("a scored case carries a null list: %+v", both)
	}

	recallOnly, ok := byID["recall-only"]
	if !ok {
		t.Fatalf("case recall-only is missing: %+v", got.Cases)
	}
	if recallOnly.ForbidMeasured {
		t.Error("forbid_measured is true on a case that declares no forbid list")
	}
	if len(recallOnly.Forbidden) != 0 {
		t.Errorf("forbidden = %v on a case that declares none", recallOnly.Forbidden)
	}

	// The aggregate keeps the two means apart and counts what cannot contribute.
	if got.Scores.CasesWithForbid != 1 || got.Scores.CasesWithoutForbid != 1 {
		t.Errorf("with forbid %d / without %d, want 1 / 1",
			got.Scores.CasesWithForbid, got.Scores.CasesWithoutForbid)
	}
	if got.Scores.MeanForbidScore != 1 {
		t.Errorf("mean_forbid_score = %v, want 1 over the single measurable case",
			got.Scores.MeanForbidScore)
	}
	if got.Scores.Scope == "" {
		t.Error("the aggregate carries no scope sentence")
	}
	if !strings.Contains(got.Scores.Scope, "recall only") {
		t.Errorf("scope does not name the cases that measure recall only: %q", got.Scores.Scope)
	}

	// A case with no forbid list is a stated omission, not a silent one.
	var noted bool
	for _, om := range got.Omissions.Omitted {
		if strings.Contains(om.Detail, "no forbid list") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the recall-only case is not recorded as an omission: %+v", got.Omissions.Omitted)
	}
	if len(got.Caveats) == 0 {
		t.Error("no caveats: the number would then read as a claim about agent success")
	}
}

// TestBenchWithoutCasesGuidesRatherThanScoringZero.
//
// The distinction the empty path must preserve: the engine did not score zero,
// the measurement has not been defined.
func TestBenchWithoutCasesGuidesRatherThanScoringZero(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := repoFixture(t)

	var got BenchResponse
	pillarGet(t, srv.handleBench, "/api/bench", &got)

	if got.Scored {
		t.Fatal("scored is true with no case file")
	}
	if got.Scores.Cases != 0 || got.Scores.MeanRecall != 0 {
		t.Errorf("scores = %+v, want an untouched zero value", got.Scores)
	}
	if got.Help == nil {
		t.Fatal("no guidance for a missing case file")
	}
	if !strings.Contains(got.Help.Title, "No benchmark case file") {
		t.Errorf("guidance title = %q", got.Help.Title)
	}
	if !strings.Contains(got.Help.Detail, "not because the engine scored zero") {
		t.Errorf("guidance does not distinguish an undefined measurement from a failed one: %q",
			got.Help.Detail)
	}
	steps := strings.Join(got.Help.Steps, "\n")
	if !strings.Contains(steps, "forbid") {
		t.Errorf("guidance does not tell the reader to write a forbid list: %q", steps)
	}
	if !strings.Contains(steps, "repocortex bench") {
		t.Errorf("guidance names no command: %q", steps)
	}
	if got.CaseFile.Path != benchCasesFile || !got.CaseFile.Defaulted {
		t.Errorf("case_file = %+v, want the conventional name, marked defaulted", got.CaseFile)
	}
}

// TestBenchRejectsAnUnusableCaseFile.
func TestBenchRejectsAnUnusableCaseFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		body      string
		wantTitle string
	}{
		{name: "not YAML at all", body: "\t\x00 not: [valid", wantTitle: "is not a valid case file"},
		{name: "a case with no id", body: "- task: do a thing\n  expect: [hub]\n", wantTitle: "is not a valid case file"},
		// internal/bench refuses an empty suite at load time. The handler's own
		// zero-case guard stays behind it: "0 of 0 passed" reads as a pass, and
		// a loader that ever stopped rejecting it must not reach the scorer.
		{name: "an empty suite", body: "[]\n", wantTitle: "is not a valid case file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _, _, root := repoFixture(t)
			writeRepoFile(t, root, benchCasesFile, tc.body)

			var got BenchResponse
			pillarGet(t, srv.handleBench, "/api/bench", &got)

			if got.Scored {
				t.Fatal("scored is true over an unusable case file")
			}
			if got.Help == nil || !strings.Contains(got.Help.Title, tc.wantTitle) {
				t.Fatalf("guidance = %+v, want a title containing %q", got.Help, tc.wantTitle)
			}
		})
	}
}

// TestBenchOnAnUnindexedStoreAnswersWithGuidance.
func TestBenchOnAnUnindexedStoreAnswersWithGuidance(t *testing.T) {
	t.Parallel()
	srv := pillarUnindexedServer(t)

	var got BenchResponse
	pillarGet(t, srv.handleBench, "/api/bench", &got)
	if got.Scored {
		t.Error("scored is true with no index")
	}
	if got.Help == nil {
		t.Fatal("no guidance over a store with no committed run")
	}
}
