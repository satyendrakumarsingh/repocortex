package web

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/bench"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// adjudicationStub supplies an adjudication verbatim, so the RENDERING can be
// tested over shapes a fixture cannot cheaply produce — a perfect resolver, a
// silent one, a run where every miss has the same cause.
//
// It embeds the Registry PORT rather than the concrete registry, which is the
// point of the port: the accuracy screen asks the value it was given whether it
// can adjudicate, and anything that can, does. What the SQL produces is pinned
// separately, against a real seeded index, by
// TestAccuracyComputesRecallOverARealIndex below and by
// internal/registry's own tests.
type adjudicationStub struct {
	Registry
	adj registry.Adjudication
}

func (a adjudicationStub) AccuracyAdjudication(context.Context, int64) (registry.Adjudication, error) {
	return a.adj, nil
}

func accuracyServer(t *testing.T, adj registry.Adjudication) *Server {
	t.Helper()
	_, deps, _ := fixture(t)
	stub := adjudicationStub{Registry: realRegistry(t, deps), adj: adj}
	return newServer(t, stub, deps, config.Config{ProjectName: "fixture"})
}

// precisionOnlyRegistry is a Registry that can compare tiers and cannot
// adjudicate call sites.
//
// It exists because that combination is a real deployment — any Registry
// implementation predating AccuracyAdjudication — and the fallback path is only
// honest if something executes it. Written as an explicit method set rather
// than by embedding *registry.Registry, because embedding would promote
// AccuracyAdjudication and the type would silently stop being the thing this
// test is about.
type precisionOnlyRegistry struct {
	Registry
	inner *registry.Registry
}

func (p precisionOnlyRegistry) ResolutionPrecision(ctx context.Context, runID int64) (registry.PrecisionReport, error) {
	return p.inner.ResolutionPrecision(ctx, runID)
}

// TestAccuracyPublishesRecallAndTheMissCounts is the reason this screen exists.
//
// Precision alone rewards a resolver for staying silent, and silence is exactly
// what a missed caller looks like. So recall, the adjudicable denominator and
// the miss split are asserted as FIELDS — a payload that carried them only when
// they were flattering would be the failure this measurement is against.
func TestAccuracyPublishesRecallAndTheMissCounts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		adj  registry.Adjudication

		wantMeasured    bool
		wantPrecision   float64
		wantRecall      float64
		wantF1          float64
		wantAdjudicable int64
		wantMissed      int64
		wantUnresolved  int64
		wantAmbiguous   int64
	}{
		{
			name: "half the callers are missed entirely",
			adj: registry.Adjudication{
				Outcomes: []registry.CallSiteOutcome{
					{Language: "go", Tier: "high", Correct: true, Count: 8},
					{Language: "go", Tier: "high", Correct: false, Count: 2},
					{Language: "go", Tier: "", Correct: false, Count: 10},
				},
				Coverage: registry.AccuracyCoverage{
					ParsedCallSites: 100, HeuristicEdges: 40, CompilerFacts: 30,
					UnambiguousLines: 20, AmbiguousLines: 5,
				},
				Misses: []registry.CallSiteMiss{
					{Reason: "unresolved", Explanation: "bound to nothing", CallSites: 6},
					{Reason: "ambiguous", Explanation: "no way to choose", CallSites: 4},
				},
			},
			wantMeasured:    true,
			wantPrecision:   0.8,
			wantRecall:      0.4,
			wantF1:          2 * 0.8 * 0.4 / (0.8 + 0.4),
			wantAdjudicable: 20,
			wantMissed:      10,
			wantUnresolved:  6,
			wantAmbiguous:   4,
		},
		{
			name: "a perfect resolver still reports its denominator",
			adj: registry.Adjudication{
				Outcomes: []registry.CallSiteOutcome{
					{Language: "go", Tier: "high", Correct: true, Count: 5},
				},
				Coverage: registry.AccuracyCoverage{ParsedCallSites: 50, CompilerFacts: 12, UnambiguousLines: 5},
			},
			wantMeasured:    true,
			wantPrecision:   1,
			wantRecall:      1,
			wantF1:          1,
			wantAdjudicable: 5,
		},
		{
			name: "a silent resolver scores zero recall, not undefined precision",
			adj: registry.Adjudication{
				Outcomes: []registry.CallSiteOutcome{
					{Language: "go", Tier: "", Correct: false, Count: 7},
				},
				Coverage: registry.AccuracyCoverage{ParsedCallSites: 30, CompilerFacts: 9, UnambiguousLines: 7},
				Misses: []registry.CallSiteMiss{
					{Reason: "unresolved", Explanation: "bound to nothing", CallSites: 7},
				},
			},
			wantMeasured:    true,
			wantAdjudicable: 7,
			wantMissed:      7,
			wantUnresolved:  7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := accuracyServer(t, tc.adj)

			var got AccuracyResponse
			pillarGet(t, srv.handleAccuracy, "/api/accuracy", &got)

			if got.Measured != tc.wantMeasured {
				t.Errorf("measured = %v, want %v", got.Measured, tc.wantMeasured)
			}
			if !closeEnough(got.Scores.Precision, tc.wantPrecision) {
				t.Errorf("precision = %v, want %v", got.Scores.Precision, tc.wantPrecision)
			}
			if !closeEnough(got.Scores.Recall, tc.wantRecall) {
				t.Errorf("recall = %v, want %v", got.Scores.Recall, tc.wantRecall)
			}
			if !closeEnough(got.Scores.F1, tc.wantF1) {
				t.Errorf("f1 = %v, want %v", got.Scores.F1, tc.wantF1)
			}
			if got.Scores.AdjudicableCallSites != tc.wantAdjudicable {
				t.Errorf("adjudicable_call_sites = %d, want %d",
					got.Scores.AdjudicableCallSites, tc.wantAdjudicable)
			}
			if got.Scores.Missed != tc.wantMissed {
				t.Errorf("missed_call_sites = %d, want %d", got.Scores.Missed, tc.wantMissed)
			}
			if got.Scores.Answered != tc.wantAdjudicable-tc.wantMissed {
				t.Errorf("answered = %d, want %d", got.Scores.Answered, tc.wantAdjudicable-tc.wantMissed)
			}
			if got.Misses.Unresolved != tc.wantUnresolved || got.Misses.Ambiguous != tc.wantAmbiguous {
				t.Errorf("miss split = unresolved %d / ambiguous %d, want %d / %d",
					got.Misses.Unresolved, got.Misses.Ambiguous, tc.wantUnresolved, tc.wantAmbiguous)
			}
			if got.Misses.Total != tc.wantMissed {
				t.Errorf("misses.missed_call_sites = %d, want %d", got.Misses.Total, tc.wantMissed)
			}
			if !got.Scores.RecallMeasured {
				t.Error("recall_measured is false over a real adjudication")
			}
			if got.Restriction == "" || !strings.Contains(got.Restriction, "call sites") {
				t.Errorf("restriction does not carry the denominator argument: %q", got.Restriction)
			}
			if got.Coverage.AdjudicatedCallSites != tc.wantAdjudicable {
				t.Errorf("coverage.adjudicated_call_sites = %d, want %d",
					got.Coverage.AdjudicatedCallSites, tc.wantAdjudicable)
			}
		})
	}
}

// TestAccuracyRecallIsAFirstClassFieldEvenWhenUnmeasured: the recall fields are
// PRESENT and marked unmeasured, never omitted. A client that dropped them when
// they were unavailable would render a complete-looking screen on exactly the
// repositories where the honest answer is "we did not measure the thing that
// matters most".
func TestAccuracyRecallIsAFirstClassFieldEvenWhenUnmeasured(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	body := pillarRaw(t, srv.handleAccuracy, "/api/accuracy")
	scores, ok := body["scores"].(map[string]any)
	if !ok {
		t.Fatalf("no scores block: %v", body)
	}
	for _, field := range []string{
		"recall", "recall_measured", "missed_call_sites", "adjudicable_call_sites",
	} {
		if _, present := scores[field]; !present {
			t.Errorf("scores.%s is absent; it must never be omitempty", field)
		}
	}
	if _, present := body["misses"]; !present {
		t.Error("misses is absent")
	}
}

// noCompilerServer is a portal over an index nothing has adjudicated.
//
// Deliberately not fixture(): that one seeds an `exact` edge, and the store's
// CHECK constraint means an `exact` edge IS a compiler fact — so the standard
// fixture is a repository with a compiler index, which is the other branch.
func noCompilerServer(t *testing.T) *Server {
	t.Helper()
	deps, _ := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "hub", "internal/core/hub.go").Export(),
			captest.S(2, "callerHigh", "internal/api/high.go").Export(),
		},
		[]captest.Edge{captest.E(2, 1).WithConf(model.ConfidenceHigh)},
		captest.Refs{Resolved: 10, Unresolved: 4})
	return newServer(t, realRegistry(t, deps), deps, config.Config{ProjectName: "no-compiler"})
}

// TestAccuracyWithoutACompilerIndexGuidesRatherThanBlanks is the empty path,
// and it is the path a real repository takes.
func TestAccuracyWithoutACompilerIndexGuidesRatherThanBlanks(t *testing.T) {
	t.Parallel()
	srv := noCompilerServer(t)

	var got AccuracyResponse
	pillarGet(t, srv.handleAccuracy, "/api/accuracy", &got)

	if got.Measured {
		t.Error("measured is true with no compiler facts imported")
	}
	if got.Help == nil {
		t.Fatal("an unmeasured accuracy screen returned no guidance")
	}
	if !strings.Contains(got.Help.Title, "No compiler index") {
		t.Errorf("guidance title = %q, want it to name the missing compiler index", got.Help.Title)
	}
	if len(got.Help.Steps) == 0 || !strings.Contains(strings.Join(got.Help.Steps, "\n"), "repocortex scip import") {
		t.Errorf("guidance does not name the command that populates the screen: %+v", got.Help.Steps)
	}

	// COVERAGE, not absence. The fixture has heuristic CALLS edges and zero
	// compiler facts, and the payload must say which of the two is zero.
	if got.Coverage.CompilerFacts != 0 {
		t.Errorf("coverage.compiler_facts = %d, want 0", got.Coverage.CompilerFacts)
	}
	if got.Coverage.HeuristicEdges == 0 {
		t.Error("coverage.heuristic_calls_edges is 0: the fixture records CALLS edges, and an " +
			"empty screen must report what the index DOES hold")
	}
	if got.Scores.RecallMeasured {
		t.Error("recall_measured is true with nothing adjudicated")
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "Recall is UNMEASURED") {
		t.Errorf("warnings do not say recall was unmeasured: %v", got.Warnings)
	}
	if got.Next.Command == "" && got.Next.Screen == "" {
		t.Error("the screen ends with no next action")
	}
}

// TestAccuracyTiersCarryTheirMeasuredMeaning: a tier row is readable on its own,
// with the legend's sentence rather than a second phrasing of it.
func TestAccuracyTiersCarryTheirMeasuredMeaning(t *testing.T) {
	t.Parallel()
	srv := accuracyServer(t, registry.Adjudication{
		Outcomes: []registry.CallSiteOutcome{
			{Language: "go", Tier: "high", Correct: true, Count: 4},
			{Language: "go", Tier: "medium", Correct: false, Count: 6},
			{Language: "go", Tier: "", Correct: false, Count: 2},
		},
		Coverage: registry.AccuracyCoverage{ParsedCallSites: 40, CompilerFacts: 12, UnambiguousLines: 12},
	})

	var got AccuracyResponse
	pillarGet(t, srv.handleAccuracy, "/api/accuracy", &got)

	byTier := map[string]AccuracyTier{}
	for _, tier := range got.Tiers {
		byTier[tier.Tier] = tier
	}
	high, ok := byTier["high"]
	if !ok {
		t.Fatalf("no `high` row: %+v", got.Tiers)
	}
	if high.TruePositives != 4 || high.FalsePositives != 0 || high.Retrieved != 4 {
		t.Errorf("high = %+v, want tp 4 / fp 0 / retrieved 4", high)
	}
	if high.Relevant != 12 {
		t.Errorf("high.relevant = %d, want 12 — every tier shares the recall denominator", high.Relevant)
	}
	if high.Meaning == "" {
		t.Error("a tier row carries no meaning, so it is unreadable without the legend")
	}
	medium := byTier["medium"]
	if medium.Precision != 0 || !medium.PrecisionMeasured {
		t.Errorf("medium = %+v, want precision 0 and MEASURED — answered, and wrong", medium)
	}
	if len(got.Legend) != 4 {
		t.Errorf("legend has %d tiers, want 4", len(got.Legend))
	}
}

// TestAccuracyOnAnUnindexedStoreAnswersWithGuidance keeps the 200-with-guidance
// contract: a portal that answers an unindexed repository with a 500 teaches the
// reader the tool is broken.
func TestAccuracyOnAnUnindexedStoreAnswersWithGuidance(t *testing.T) {
	t.Parallel()
	srv := pillarUnindexedServer(t)

	var got AccuracyResponse
	rec := pillarGet(t, srv.handleAccuracy, "/api/accuracy", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got.Meta.Indexed {
		t.Error("meta.indexed is true over a store with no committed run")
	}
	if got.Help == nil {
		t.Fatal("no guidance for an unindexed store")
	}
}

// adjudicableServer is a portal over an index a compiler HAS adjudicated.
//
// captest cannot build one: its symbol_references carry no start_line, so no
// parsed call site can ever coincide with a compiler-resolved line and the
// recall denominator is empty by construction. The rows are therefore seeded
// directly, in the shape `repocortex accuracy`'s own fixture uses — which is what
// makes the numbers below comparable to the ones the terminal prints.
//
//	101-105  high   → right
//	106      high   → wrong
//	107      medium → right
//	108      medium → wrong
//	109      low    → wrong
//	110      no edge at all              MISSED
//	111      two compiler targets        ambiguous, excluded
//	112      no compiler fact            unadjudicated
//	200      compiler-resolved, NOT a call — excluded from the denominator
//
// precision 6/9, recall 6/10, over 10 adjudicated of 14 parsed call sites.
func adjudicableServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`INSERT INTO index_runs(id, status, committed, started_unix) VALUES(1,'ready',1,0)`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(1,1,'pkg/a.go','go','h1')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(2,1,'web/app.ts','typescript','h2')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		   VALUES(1,1,1,'function','Caller','pkg.Caller','high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		   VALUES(2,1,1,'function','B','pkg.B','high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		   VALUES(3,1,1,'function','C','pkg.C','high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		   VALUES(4,1,2,'function','tsCaller','web.tsCaller','high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		   VALUES(5,1,2,'function','tsTarget','web.tsTarget','high','source_code')`,
	}
	ref := func(file, line int, kind, res string) string {
		return fmt.Sprintf(
			`INSERT INTO symbol_references(index_run_id, file_id, name, kind, start_line, resolution)
			   VALUES(1,%d,'callee','%s',%d,'%s')`, file, kind, line, res)
	}
	for line := 101; line <= 109; line++ {
		stmts = append(stmts, ref(1, line, "reference.call", "resolved"))
	}
	stmts = append(stmts,
		ref(1, 110, "reference.call", "unresolved"),
		ref(1, 111, "reference.call", "resolved"),
		ref(1, 112, "reference.call", "resolved"),
		ref(1, 200, "reference.type", "resolved"),
		ref(2, 10, "reference.call", "resolved"),
		ref(2, 11, "reference.call", "resolved"))

	edge := func(src, dst int, conf, file string, line int) string {
		return fmt.Sprintf(
			`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
			                   evidence_file, evidence_line, source)
			   VALUES(1,%d,%d,'CALLS','%s','%s',%d,'source_code')`, src, dst, conf, file, line)
	}
	for line := 101; line <= 105; line++ {
		stmts = append(stmts, edge(1, 2, "high", "pkg/a.go", line))
	}
	stmts = append(stmts,
		edge(1, 3, "high", "pkg/a.go", 106),
		edge(1, 2, "medium", "pkg/a.go", 107),
		edge(1, 3, "medium", "pkg/a.go", 108),
		edge(1, 3, "low", "pkg/a.go", 109),
		edge(1, 2, "high", "pkg/a.go", 111),
		edge(1, 2, "high", "pkg/a.go", 112),
		edge(4, 5, "high", "web/app.ts", 10),
		edge(4, 5, "high", "web/app.ts", 11))

	fact := func(dst, line int) string {
		return fmt.Sprintf(
			`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
			                   evidence_file, evidence_line, source)
			   VALUES(1,1,%d,'REFERENCES','exact','pkg/a.go',%d,'compiler')`, dst, line)
	}
	for line := 101; line <= 110; line++ {
		stmts = append(stmts, fact(2, line))
	}
	stmts = append(stmts, fact(2, 111), fact(3, 111), fact(2, 200))

	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed adjudicable index: %v\n%s", err, q)
		}
	}

	reg := registry.New(db)
	return newServer(t, reg, capability.NewDeps(reg, graph.New(db)),
		config.Config{ProjectName: "adjudicated"})
}

// TestAccuracyComputesRecallOverARealIndex is the claim this screen was built
// to make and could not, and it is asserted end to end: SQLite rows in, JSON
// out, through the production registry.
//
// The numbers are hand-computed from the fixture above rather than recorded
// from a run, because a recall figure that only ever agreed with itself would
// prove nothing about the query. 6 of 10 adjudicable call sites got a correct
// CALLS edge; 1 of them got no edge at all and is the miss precision is
// structurally incapable of seeing.
func TestAccuracyComputesRecallOverARealIndex(t *testing.T) {
	t.Parallel()
	srv := adjudicableServer(t)

	var got AccuracyResponse
	pillarGet(t, srv.handleAccuracy, "/api/accuracy", &got)

	if !got.Measured {
		t.Fatalf("measured is false over an index a compiler adjudicated: %+v", got.Coverage)
	}
	if !got.Scores.RecallMeasured {
		t.Fatal("recall_measured is false; this test exists because it used to be")
	}
	for _, want := range []struct {
		what string
		got  float64
		exp  float64
	}{
		{"recall 6/10", got.Scores.Recall, 0.6},
		{"precision 6/9", got.Scores.Precision, 6.0 / 9.0},
		{"f1 over the same denominator", got.Scores.F1,
			2 * (6.0 / 9.0) * 0.6 / ((6.0 / 9.0) + 0.6)},
	} {
		if !closeEnough(want.got, want.exp) {
			t.Errorf("%s = %v, want %v", want.what, want.got, want.exp)
		}
	}
	for _, want := range []struct {
		what string
		got  int64
		exp  int64
	}{
		{"adjudicable call sites", got.Scores.AdjudicableCallSites, 10},
		{"answered call sites", got.Scores.Answered, 9},
		{"missed call sites", got.Scores.Missed, 1},
		{"true positives", got.Scores.TruePositives, 6},
		{"false positives", got.Scores.FalsePositives, 3},
		{"parsed call sites", got.Coverage.ParsedCallSites, 14},
		{"unambiguous compiler lines", got.Coverage.UnambiguousLines, 11},
		{"ambiguous compiler lines", got.Coverage.AmbiguousLines, 1},
		{"the miss breakdown total", got.Misses.Total, 1},
		{"the unresolved miss", got.Misses.Unresolved, 1},
	} {
		if want.got != want.exp {
			t.Errorf("%s = %d, want %d", want.what, want.got, want.exp)
		}
	}
	if !got.Misses.Measured || len(got.Misses.Causes) != 1 {
		t.Errorf("miss breakdown = %+v, want one measured cause", got.Misses)
	}

	// Line 200 is the restriction's own test: a compiler-resolved line the
	// parser recorded no CALL on. Counting it would make the denominator 11 and
	// the measurement the unsound one docs/accuracy.md refused to publish.
	if got.Scores.AdjudicableCallSites == got.Coverage.UnambiguousLines {
		t.Error("every unambiguous compiler line entered the recall denominator, " +
			"including the one that is not a call site")
	}

	// TypeScript has call sites and no compiler facts. It must be NAMED as
	// unmeasured, not dropped and not scored at 0%.
	if len(got.Unmeasured) != 1 || got.Unmeasured[0] != "typescript" {
		t.Errorf("unmeasured languages = %v, want [typescript]", got.Unmeasured)
	}
	var ts *AccuracyLanguage
	for i := range got.Languages {
		if got.Languages[i].Language == "typescript" {
			ts = &got.Languages[i]
		}
	}
	if ts == nil {
		t.Fatalf("typescript is absent from the language table entirely: %+v", got.Languages)
	}
	if ts.Measured || ts.Recall != 0 || ts.Adjudicated != 0 {
		t.Errorf("typescript = %+v, want unmeasured and carrying no rate", *ts)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "typescript") {
		t.Errorf("warnings do not name the unmeasured language: %v", got.Warnings)
	}
}

// TestAccuracyRecallMatchesTheCommandLine holds the portal and the terminal to
// one answer over one index.
//
// They share registry.AccuracyAdjudication and fold it with the same
// bench.AccuracyOf, so this cannot drift silently — which is exactly why it is
// worth asserting: the previous arrangement had the same measurement written
// twice, and two copies of a measurement are two measurements.
func TestAccuracyRecallMatchesTheCommandLine(t *testing.T) {
	t.Parallel()
	srv := adjudicableServer(t)

	var got AccuracyResponse
	pillarGet(t, srv.handleAccuracy, "/api/accuracy", &got)

	reg, ok := srv.deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("fixture registry is %T", srv.deps.Registry)
	}
	adj, err := reg.AccuracyAdjudication(context.Background(), 1)
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	outcomes := make([]bench.CallSiteOutcome, 0, len(adj.Outcomes))
	for _, o := range adj.Outcomes {
		outcomes = append(outcomes, bench.CallSiteOutcome{
			Language: o.Language, Tier: o.Tier, Correct: o.Correct, Count: o.Count,
		})
	}
	_, overall := bench.AccuracyOf(outcomes)

	if got.Scores.Recall != overall.All.Recall || got.Scores.Precision != overall.All.Precision {
		t.Errorf("the portal publishes precision %v / recall %v; folding the same "+
			"adjudication gives %v / %v",
			got.Scores.Precision, got.Scores.Recall,
			overall.All.Precision, overall.All.Recall)
	}
	if got.Scores.AdjudicableCallSites != overall.Adjudicated {
		t.Errorf("denominators differ: %d vs %d",
			got.Scores.AdjudicableCallSites, overall.Adjudicated)
	}
}

// TestAccuracyFallsBackToPrecisionForARegistryThatCannotAdjudicate.
//
// The fallback is not dead code kept for tidiness: any Registry implementation
// that predates AccuracyAdjudication takes this path, and half a measurement
// with the missing half NAMED is strictly more useful than an empty screen. The
// recall fields must be present and marked unmeasured, the omission must be
// recorded with a remedy, and `measured` must stay false — precision alone is a
// smaller claim and must not inherit the word.
func TestAccuracyFallsBackToPrecisionForARegistryThatCannotAdjudicate(t *testing.T) {
	t.Parallel()
	full := adjudicableServer(t)
	inner, ok := full.deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("fixture registry is %T", full.deps.Registry)
	}
	narrowed := precisionOnlyRegistry{Registry: inner, inner: inner}
	if _, wired := any(narrowed).(AccuracySource); wired {
		t.Fatal("the narrowed registry still satisfies AccuracySource, so this test " +
			"exercises the measured path and proves nothing about the fallback")
	}
	srv := newServer(t, narrowed, full.deps.Caps, config.Config{ProjectName: "precision-only"})

	var got AccuracyResponse
	pillarGet(t, srv.handleAccuracy, "/api/accuracy", &got)

	if got.Scores.RecallMeasured {
		t.Error("recall_measured is true, but this registry cannot compute recall")
	}
	if got.Measured {
		t.Error("measured must stay false: half of this measurement is a smaller claim")
	}
	if got.Scores.TruePositives+got.Scores.FalsePositives == 0 {
		t.Error("the precision half is empty too, so the fallback served nothing")
	}
	if got.Help == nil || !strings.Contains(got.Help.Detail, "RECALL") {
		t.Fatalf("guidance does not name the withheld half: %+v", got.Help)
	}
	if !strings.Contains(strings.Join(got.Help.Steps, "\n"), "repocortex accuracy") {
		t.Errorf("guidance does not name the command that produces recall: %+v", got.Help.Steps)
	}

	var found bool
	for _, om := range got.Omissions.Omitted {
		if strings.Contains(om.Detail, "Recall and F1 are NOT served") {
			found = true
			if om.Remedy == "" {
				t.Error("the withheld recall omission names no remedy")
			}
		}
	}
	if !found {
		t.Errorf("the withheld recall is not recorded as an omission: %+v", got.Omissions.Omitted)
	}
	if got.Omissions.Complete {
		t.Error("omissions.complete is true on a response that withheld a headline number")
	}
}

func closeEnough(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
