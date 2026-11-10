package registry

import (
	"context"
	"fmt"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// The adjudication is the one query in this package whose arithmetic is a
// published number, so it is pinned against a hand-computed fixture rather than
// against whatever the code happens to produce.
//
// The fixture is deliberately the same shape `cortex accuracy`'s own fixture
// has always had, because that is the shape whose answer is already known and
// argued: ten adjudicable call sites, six answered correctly, one missed
// entirely, one ambiguous line excluded, and one unambiguous compiler line that
// is NOT a call and must therefore stay out of the denominator.
//
// Rows are inserted with direct SQL rather than through the writer for the same
// reason captest does it: this is a read model, and the test must fail when the
// READ breaks, not when an unrelated write path changes.
//
// Each test opens its own store. store.OpenMemory names every in-memory
// database uniquely (see memCounter in internal/store), so these are four
// independent databases rather than one shared one.

// seedAdjudication builds a run whose adjudication has known arithmetic.
//
// Go, in pkg/a.go. Fourteen parsed call sites across the run, ten adjudicable:
//
//	101-105  high   → right          precision 5/6, recall 5/10
//	106      high   → wrong
//	107      medium → right          precision 1/2, recall 1/10
//	108      medium → wrong
//	109      low    → wrong          precision 0/1, recall 0/10
//	110      (no edge at all)        MISSED — the error precision cannot see
//	111      compiler names two targets → ambiguous line, excluded
//	112      no compiler fact           → unadjudicated
//	all                                 precision 6/9, recall 6/10
//
// Line 200 is the restriction's own test: the compiler resolved it
// unambiguously, but the parser recorded a reference.type there and no call, so
// it must NOT enter the recall denominator. A naive comparison scores 11.
//
// TypeScript, in web/app.ts: two call sites, heuristic edges, and no compiler
// facts at all — the NOT MEASURED case, which only LanguageCoverage can carry
// because a language nothing adjudicated produces no outcome row by
// construction.
func seedAdjudication(t *testing.T, withCompiler bool) *Registry {
	t.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`INSERT INTO index_runs(id, status, committed, started_unix) VALUES(1,'ready',1,0)`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(1,1,'pkg/a.go','go','h1')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(2,1,'web/app.ts','typescript','h2')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(3,1,'README.md','markdown','h3')`,
	}
	for _, spec := range []struct {
		id, file int
		name     string
	}{
		{1, 1, "Caller"}, {2, 1, "B"}, {3, 1, "C"},
		{4, 2, "tsCaller"}, {5, 2, "tsTarget"},
	} {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
			   VALUES(%d,1,%d,'function','%s','pkg.%s','high','source_code')`,
			spec.id, spec.file, spec.name, spec.name))
	}

	type ref struct {
		file, line int
		kind, res  string
	}
	refs := []ref{}
	for line := 101; line <= 109; line++ {
		refs = append(refs, ref{1, line, "reference.call", "resolved"})
	}
	// 110 bound to nothing, which is why no edge exists for it.
	refs = append(refs, ref{1, 110, "reference.call", "unresolved"})
	refs = append(refs, ref{1, 111, "reference.call", "resolved"})
	refs = append(refs, ref{1, 112, "reference.call", "resolved"})
	refs = append(refs, ref{1, 200, "reference.type", "resolved"})
	refs = append(refs, ref{2, 10, "reference.call", "resolved"})
	refs = append(refs, ref{2, 11, "reference.call", "resolved"})
	for _, r := range refs {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO symbol_references(index_run_id, file_id, name, kind, start_line, resolution)
			   VALUES(1,%d,'callee','%s',%d,'%s')`, r.file, r.kind, r.line, r.res))
	}

	type hEdge struct {
		src, dst   int
		confidence string
		file       string
		line       int
	}
	heuristic := []hEdge{}
	for line := 101; line <= 105; line++ {
		heuristic = append(heuristic, hEdge{1, 2, "high", "pkg/a.go", line})
	}
	heuristic = append(heuristic,
		hEdge{1, 3, "high", "pkg/a.go", 106},
		hEdge{1, 2, "medium", "pkg/a.go", 107},
		hEdge{1, 3, "medium", "pkg/a.go", 108},
		hEdge{1, 3, "low", "pkg/a.go", 109},
		// 110 deliberately absent: the missed call site.
		hEdge{1, 2, "high", "pkg/a.go", 111},
		hEdge{1, 2, "high", "pkg/a.go", 112},
		hEdge{4, 5, "high", "web/app.ts", 10},
		hEdge{4, 5, "high", "web/app.ts", 11},
	)
	for _, e := range heuristic {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
			                   evidence_file, evidence_line, source)
			   VALUES(1,%d,%d,'CALLS','%s','%s',%d,'source_code')`,
			e.src, e.dst, e.confidence, e.file, e.line))
	}

	if withCompiler {
		type cEdge struct{ dst, line int }
		compiler := []cEdge{}
		for line := 101; line <= 110; line++ {
			compiler = append(compiler, cEdge{2, line})
		}
		compiler = append(compiler, cEdge{2, 111}, cEdge{3, 111}, cEdge{2, 200})
		for _, e := range compiler {
			stmts = append(stmts, fmt.Sprintf(
				`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
				                   evidence_file, evidence_line, source)
				   VALUES(1,1,%d,'REFERENCES','exact','pkg/a.go',%d,'compiler')`,
				e.dst, e.line))
		}
	}

	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed adjudication: %v\n%s", err, s)
		}
	}
	return New(db)
}

// outcomeKey identifies one verdict group in a way a table can state.
type outcomeKey struct {
	language string
	tier     string
	correct  bool
}

// TestAccuracyAdjudicationScoresEachCallSiteOnce pins the unit of the
// measurement as much as the numbers.
//
// A call site is ONE outcome, at the strongest tier that answered it and the
// strongest tier that was right. Counting edges instead would give precision
// and recall denominators that cannot be combined, and an F1 of two
// incompatible fractions is a number with no referent.
func TestAccuracyAdjudicationScoresEachCallSiteOnce(t *testing.T) {
	reg := seedAdjudication(t, true)

	adj, err := reg.AccuracyAdjudication(context.Background(), 1)
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}

	got := map[outcomeKey]int64{}
	var total int64
	for _, o := range adj.Outcomes {
		got[outcomeKey{o.Language, o.Tier, o.Correct}] += o.Count
		total += o.Count
	}

	want := map[outcomeKey]int64{
		{"go", "high", true}:    5, // 101-105
		{"go", "high", false}:   1, // 106 named the wrong definition
		{"go", "medium", true}:  1, // 107
		{"go", "medium", false}: 1, // 108
		{"go", "low", false}:    1, // 109
		{"go", "", false}:       1, // 110 — no edge at all, the miss
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("outcome %+v = %d, want %d (all outcomes: %+v)", k, got[k], n, adj.Outcomes)
		}
	}
	for k, n := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected outcome group %+v carrying %d call site(s)", k, n)
		}
	}
	if total != 10 {
		t.Errorf("adjudicated call sites = %d, want 10 — line 111 is ambiguous, 112 has no "+
			"compiler fact, and 200 is a compiler-resolved reference that is NOT a call",
			total)
	}
}

// TestAccuracyAdjudicationCoverageIsWhatTheRatesWereComputedOver.
//
// Table-driven over the two states a repository is actually in: a compiler
// index imported, and none. The second is the case every real repository starts
// in, and its whole job is to make an empty measurement explicable — zero
// compiler facts beside a non-zero resolver edge count says WHICH half is
// missing.
func TestAccuracyAdjudicationCoverageIsWhatTheRatesWereComputedOver(t *testing.T) {

	for _, tc := range []struct {
		name         string
		withCompiler bool
		want         AccuracyCoverage
		wantOutcomes int
	}{
		{
			name: "a compiler index has been imported", withCompiler: true,
			want: AccuracyCoverage{
				// 14 parsed call sites: 12 in pkg/a.go (101-112) and 2 in
				// web/app.ts. Line 200 is a reference.type and is not one.
				ParsedCallSites: 14,
				// 13 resolver CALLS edges: 101-109, 111 and 112 in Go, plus the
				// two TypeScript ones. 13 compiler facts: 101-110, two on the
				// ambiguous line 111, and the non-call line 200.
				HeuristicEdges: 13,
				CompilerFacts:  13,
				// 101-110 plus 200 resolve to exactly one target; 111 resolves
				// to two.
				UnambiguousLines: 11,
				AmbiguousLines:   1,
			},
			wantOutcomes: 6,
		},
		{
			name: "nothing has adjudicated this index", withCompiler: false,
			want: AccuracyCoverage{
				ParsedCallSites: 14,
				HeuristicEdges:  13,
				CompilerFacts:   0,
			},
			wantOutcomes: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := seedAdjudication(t, tc.withCompiler)

			adj, err := reg.AccuracyAdjudication(context.Background(), 1)
			if err != nil {
				t.Fatalf("adjudicate: %v", err)
			}
			if adj.Coverage != tc.want {
				t.Errorf("coverage = %+v,\n            want %+v", adj.Coverage, tc.want)
			}
			if len(adj.Outcomes) != tc.wantOutcomes {
				t.Errorf("%d outcome group(s), want %d: %+v",
					len(adj.Outcomes), tc.wantOutcomes, adj.Outcomes)
			}
		})
	}
}

// TestAccuracyAdjudicationNamesTheCauseOfEachMiss.
//
// "Missed" without a cause is unactionable, and the causes have different
// owners: unresolved is a resolver defect, pending means `cortex resolve` was
// never run, and resolved-with-no-edge is a call outside any indexed symbol.
func TestAccuracyAdjudicationNamesTheCauseOfEachMiss(t *testing.T) {
	reg := seedAdjudication(t, true)

	adj, err := reg.AccuracyAdjudication(context.Background(), 1)
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if len(adj.Misses) != 1 {
		t.Fatalf("%d miss cause(s), want 1: %+v", len(adj.Misses), adj.Misses)
	}
	m := adj.Misses[0]
	if m.Reason != "unresolved" || m.CallSites != 1 {
		t.Errorf("miss = %+v, want one unresolved call site", m)
	}
	if m.Explanation == "" {
		t.Error("the miss carries no explanation, so a reader cannot act on it")
	}
}

// TestAccuracyAdjudicationCarriesUnmeasuredLanguages is the claim a rate table
// cannot make for itself.
//
// TypeScript has call sites and no compiler facts, so it produces NO outcome
// row — bench sees nothing to fold and could not name it if it wanted to. It
// still has to be nameable, because an unmeasured language that reads like a
// measured one is the single failure a trust report cannot survive. That is the
// entire reason LanguageCoverage is on the adjudication.
func TestAccuracyAdjudicationCarriesUnmeasuredLanguages(t *testing.T) {
	reg := seedAdjudication(t, true)

	adj, err := reg.AccuracyAdjudication(context.Background(), 1)
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}

	byLang := map[string]LanguageCoverage{}
	for _, l := range adj.Languages {
		byLang[l.Language] = l
	}
	ts, ok := byLang["typescript"]
	if !ok {
		t.Fatalf("typescript is absent from the language coverage entirely: %+v", adj.Languages)
	}
	if ts.CallSites != 2 || ts.CompilerFacts != 0 {
		t.Errorf("typescript = %+v, want 2 call sites and no compiler facts", ts)
	}
	if _, listed := byLang["markdown"]; listed {
		t.Error("markdown carries no call sites and no edges; listing it buries the " +
			"language that genuinely is unmeasured")
	}
	if got := byLang["go"]; got.CallSites != 12 {
		t.Errorf("go = %+v, want 12 parsed call sites", got)
	}
}

// TestAccuracyAdjudicationAgreesWithResolutionPrecision holds the two read
// models in this package to one answer.
//
// They count different relations through different joins, and both publish a
// number a reader compares: doctor's precision and `cortex accuracy`'s. If
// they ever disagree about how many CALLS edges the RESOLVER claimed on one
// index, the disagreement is unlocatable from either report alone.
func TestAccuracyAdjudicationAgreesWithResolutionPrecision(t *testing.T) {
	ctx := context.Background()
	reg := seedAdjudication(t, true)

	adj, err := reg.AccuracyAdjudication(ctx, 1)
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	prec, err := reg.ResolutionPrecision(ctx, 1)
	if err != nil {
		t.Fatalf("precision: %v", err)
	}
	if adj.Coverage.HeuristicEdges != prec.HeuristicEdges {
		t.Errorf("adjudication counts %d resolver CALLS edge(s), precision counts %d",
			adj.Coverage.HeuristicEdges, prec.HeuristicEdges)
	}
	if adj.Coverage.CompilerFacts != prec.CompilerEdges {
		t.Errorf("adjudication counts %d compiler fact(s), precision counts %d",
			adj.Coverage.CompilerFacts, prec.CompilerEdges)
	}
	if adj.Coverage.UnambiguousLines != prec.JudgeableLines {
		t.Errorf("adjudication counts %d unambiguous line(s), precision counts %d",
			adj.Coverage.UnambiguousLines, prec.JudgeableLines)
	}
	if adj.Coverage.HeuristicEdges == 0 {
		t.Error("both report zero resolver edges, which agrees by reading nothing")
	}
}
