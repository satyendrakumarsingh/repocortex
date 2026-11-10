package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// runAccuracyCmd executes `cortex accuracy` against an isolated project root.
//
// It builds its own root rather than using NewRoot's, because the command tree
// is assembled elsewhere and a test that waits for registration is a test that
// cannot be written first. The persistent flags mirror NewRoot's, since the
// command reads --json through them.
func runAccuracyCmd(t *testing.T, projectRoot string, args ...string) (string, error) {
	t.Helper()
	g := &Global{}
	root := &cobra.Command{
		Use:               "cortex",
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return g.setup(cmd) },
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&g.JSON, "json", false, "")
	pf.BoolVarP(&g.Quiet, "quiet", "q", false, "")
	pf.StringVar(&g.Project, "project", "", "")
	root.AddCommand(newAccuracyCmd(g))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"accuracy", "--project", projectRoot, "--quiet"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// seedAccuracy builds an index whose accuracy report has known arithmetic.
//
// Go, in pkg/a.go. Twelve parsed call sites, ten of them adjudicable:
//
//	101-105  high   → right          precision 5/6, recall 5/10
//	106      high   → wrong
//	107      medium → right          precision 1/2, recall 1/10
//	108      medium → wrong
//	109      low    → wrong          precision 0/1, recall 0/10
//	110      (no edge at all)        MISSED — the error precision cannot see
//	111      compiler says two targets → ambiguous line, excluded
//	112      no compiler fact          → unadjudicated
//	all                                precision 6/9, recall 6/10
//
// Line 200 is the restriction's own test: the compiler resolved it
// unambiguously, but the parser recorded a reference.type there and no call, so
// it must NOT enter the recall denominator. A naive comparison would score 11.
//
// TypeScript, in web/app.ts: two call sites, heuristic edges, and no compiler
// facts at all — the NOT MEASURED case.
//
// withCompiler=false drops every compiler fact, which is "no SCIP index
// imported for anything".
func seedAccuracy(t *testing.T, root string, withCompiler bool) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(root, IndexDir, IndexFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

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

	// Parsed references. Every call site the parser saw, resolved or not.
	type ref struct {
		file, line int
		kind, res  string
	}
	refs := []ref{}
	for line := 101; line <= 109; line++ {
		refs = append(refs, ref{1, line, "reference.call", "resolved"})
	}
	// 110 resolved to nothing, which is why no edge exists for it.
	refs = append(refs, ref{1, 110, "reference.call", "unresolved"})
	refs = append(refs, ref{1, 111, "reference.call", "resolved"})
	refs = append(refs, ref{1, 112, "reference.call", "resolved"})
	// The restriction: a compiler-resolved line that is not a call.
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
		hEdge{1, 3, "high", "pkg/a.go", 106}, // compiler says 2
		hEdge{1, 2, "medium", "pkg/a.go", 107},
		hEdge{1, 3, "medium", "pkg/a.go", 108}, // compiler says 2
		hEdge{1, 3, "low", "pkg/a.go", 109},    // compiler says 2
		// 110 deliberately absent: the missed call site.
		hEdge{1, 2, "high", "pkg/a.go", 111}, // ambiguous line, excluded
		hEdge{1, 2, "high", "pkg/a.go", 112}, // no compiler fact
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
		// 111 resolves to two distinct targets: ambiguous, excluded.
		compiler = append(compiler, cEdge{2, 111}, cEdge{3, 111})
		// 200 is unambiguous but is not a call site.
		compiler = append(compiler, cEdge{2, 200})
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
			t.Fatalf("seed accuracy: %v\n%s", err, s)
		}
	}
}

func accuracyFixture(t *testing.T, withCompiler bool) string {
	t.Helper()
	root := isolate(t)
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	seedAccuracy(t, root, withCompiler)
	return root
}

// TestAccuracyReportIsActionable is the contract of the command as a trust
// document: precision AND recall AND F1 per tier, the restriction stated inline
// beside them, the missed call site named as a miss, and the coverage the rates
// were computed over.
func TestAccuracyReportIsActionable(t *testing.T) {
	root := accuracyFixture(t, true)

	out, err := runAccuracyCmd(t, root)
	if err != nil {
		t.Fatalf("accuracy: %v\n%s", err, out)
	}
	t.Logf("rendered report:\n%s", out)

	// Compared against whitespace-flattened output. The report wraps to a
	// terminal width, and a phrase that happens to straddle a fold is still
	// printed — asserting on the raw string would pin the line breaks instead
	// of the claims.
	flat := flattenSpace(out)

	want := []struct{ what, substr string }{
		{"a recall column at all", "RECALL"},
		{"an F1 column", "F1"},
		{"the restriction, inline rather than by reference", "ONLY over call sites"},
		{"why the restriction is needed", "superset of calls"},
		{"high precision 5/6", "83.3%"},
		{"high recall 5/10", "50.0%"},
		{"medium precision 1/2", "50.0%"},
		{"medium recall 1/10", "10.0%"},
		{"all-tiers recall 6/10", "60.0%"},
		{"the missed call site, named", "missed"},
		{"its cause", "the reference was bound to nothing"},
		{"the recall denominator", "10 adjudicated call site(s)"},
		{"coverage over parsed call sites", "of 14 parsed call site(s)"},
		{"the unmeasured language", "NOT MEASURED"},
		{"what the number is not", "WHAT THIS IS NOT"},
		{"that the oracle is our own parser", "RepoCortex's own parser"},
	}
	for _, w := range want {
		if !strings.Contains(flat, w.substr) {
			t.Errorf("report does not state %s (missing %q)", w.what, w.substr)
		}
	}
}

// flattenSpace collapses every run of whitespace to one space, so an assertion
// about what the report SAYS is independent of where it wrapped.
func flattenSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestAccuracyRecallExcludesNonCallReferences is the soundness argument as an
// executable claim. Line 200 is a compiler-resolved reference that is not a
// call; counting it would make the denominator 11 and the measurement exactly
// the unsound one docs/accuracy.md refused to publish.
func TestAccuracyRecallExcludesNonCallReferences(t *testing.T) {
	root := accuracyFixture(t, true)

	out, err := runAccuracyCmd(t, root, "--json")
	if err != nil {
		t.Fatalf("accuracy --json: %v\n%s", err, out)
	}
	var rep struct {
		Overall struct {
			Adjudicated int64 `json:"adjudicated_call_sites"`
			Missed      int64 `json:"missed_call_sites"`
			All         struct {
				TruePositives  int64   `json:"true_positives"`
				FalsePositives int64   `json:"false_positives"`
				Precision      float64 `json:"precision"`
				Recall         float64 `json:"recall"`
				F1             float64 `json:"f1"`
			} `json:"all_tiers"`
		} `json:"overall"`
		Coverage struct {
			ParsedCallSites  int64 `json:"parsed_call_sites"`
			UnambiguousLines int64 `json:"unambiguous_lines"`
			AmbiguousLines   int64 `json:"ambiguous_lines"`
		} `json:"coverage"`
		Restriction string `json:"restriction"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse accuracy JSON: %v\n%s", err, out)
	}

	// 11 unambiguous compiler lines (101-110 and 200); only 10 are call sites.
	if rep.Coverage.UnambiguousLines != 11 {
		t.Errorf("unambiguous lines = %d, want 11", rep.Coverage.UnambiguousLines)
	}
	if rep.Overall.Adjudicated != 10 {
		t.Errorf("adjudicated call sites = %d, want 10 — line 200 is a compiler-resolved "+
			"reference that is NOT a call, and including it is the unsound comparison",
			rep.Overall.Adjudicated)
	}
	if rep.Coverage.AmbiguousLines != 1 {
		t.Errorf("ambiguous lines = %d, want 1", rep.Coverage.AmbiguousLines)
	}
	if rep.Coverage.ParsedCallSites != 14 {
		t.Errorf("parsed call sites = %d, want 14", rep.Coverage.ParsedCallSites)
	}
	if rep.Overall.Missed != 1 {
		t.Errorf("missed = %d, want 1", rep.Overall.Missed)
	}
	if got, want := rep.Overall.All.Recall, 0.6; got != want {
		t.Errorf("all-tiers recall = %.4f, want %.4f", got, want)
	}
	if got := rep.Overall.All.Precision; got < 0.666 || got > 0.667 {
		t.Errorf("all-tiers precision = %.4f, want 6/9", got)
	}
	if rep.Restriction == "" {
		t.Error("JSON carries no restriction: a script reading recall must be able to read what it is over")
	}
}

// TestAccuracyNamesUnmeasuredLanguagesRatherThanScoringThem holds the line the
// precision report already holds. TypeScript has call sites and no SCIP index,
// so it must be present, marked, and carry no rate — never 0%, never absent.
func TestAccuracyNamesUnmeasuredLanguagesRatherThanScoringThem(t *testing.T) {
	root := accuracyFixture(t, true)

	out, err := runAccuracyCmd(t, root, "--json")
	if err != nil {
		t.Fatalf("accuracy --json: %v\n%s", err, out)
	}
	var rep struct {
		Languages []struct {
			Language    string `json:"language"`
			Adjudicated int64  `json:"adjudicated_call_sites"`
			Measured    bool   `json:"measured"`
		} `json:"languages"`
		LanguageCoverage []struct {
			Language  string `json:"language"`
			CallSites int64  `json:"parsed_call_sites"`
			Measured  bool   `json:"measured"`
		} `json:"language_coverage"`
		Unmeasured []string `json:"unmeasured_languages"`
		Warnings   []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse accuracy JSON: %v\n%s", err, out)
	}

	found := false
	for _, l := range rep.LanguageCoverage {
		switch l.Language {
		case "typescript":
			found = true
			if l.Measured || l.CallSites != 2 {
				t.Errorf("typescript = %+v, want 2 call sites and measured=false", l)
			}
		case "markdown":
			t.Error("markdown has no call sites and is listed anyway; it buries the language that genuinely is unmeasured")
		}
	}
	if !found {
		t.Fatal("typescript omitted from the report entirely — an unmeasured language must be named, not dropped")
	}
	if len(rep.Unmeasured) != 1 || rep.Unmeasured[0] != "typescript" {
		t.Errorf("unmeasured languages = %v, want [typescript]", rep.Unmeasured)
	}
	// Go is measured, so it must carry rows; TypeScript must not.
	for _, l := range rep.Languages {
		if l.Language == "typescript" && l.Measured {
			t.Error("typescript reports itself measured despite no compiler facts")
		}
		if l.Language == "go" && l.Adjudicated != 10 {
			t.Errorf("go adjudicated = %d, want 10", l.Adjudicated)
		}
	}

	joined := strings.Join(rep.Warnings, " ")
	if !strings.Contains(joined, "typescript") {
		t.Errorf("warnings do not name the unmeasured language: %v", rep.Warnings)
	}
	if !strings.Contains(joined, "not the same as accurate") {
		t.Errorf("warnings do not say unmeasured is not accurate: %v", rep.Warnings)
	}
}

// TestAccuracyWithoutCompilerFactsMeasuresNothing guards the reading that costs
// most: an index with no SCIP import must say it measured nothing, not report
// zeroes that read as a resolver that finds nothing.
func TestAccuracyWithoutCompilerFactsMeasuresNothing(t *testing.T) {
	root := accuracyFixture(t, false)

	out, err := runAccuracyCmd(t, root)
	if err != nil {
		t.Fatalf("accuracy: %v\n%s", err, out)
	}
	t.Logf("rendered report:\n%s", out)

	if !strings.Contains(out, "Not measured") {
		t.Errorf("report does not say it measured nothing:\n%s", out)
	}
	if strings.Contains(out, "0.0%") {
		t.Errorf("report prints a rate with no compiler facts behind it:\n%s", out)
	}
	if !strings.Contains(out, "cortex scip import") {
		t.Errorf("report does not name the fix:\n%s", out)
	}
	// The languages table still renders: which languages exist and that NONE of
	// them was checked is the whole content of this report.
	if !strings.Contains(out, "NOT MEASURED") {
		t.Errorf("languages are not marked unmeasured:\n%s", out)
	}

	jsonOut, err := runAccuracyCmd(t, root, "--json")
	if err != nil {
		t.Fatalf("accuracy --json: %v", err)
	}
	var rep struct {
		Measured bool `json:"measured"`
		Overall  struct {
			Adjudicated int64 `json:"adjudicated_call_sites"`
		} `json:"overall"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("parse accuracy JSON: %v\n%s", err, jsonOut)
	}
	if rep.Measured || rep.Overall.Adjudicated != 0 {
		t.Errorf("measured=%v adjudicated=%d, want false/0", rep.Measured, rep.Overall.Adjudicated)
	}
}

// TestAccuracyExitStatusIsNotAGate: the numbers never fail the command. The
// moment a measurement gates a build, somebody tunes the threshold instead of
// the resolver — the rule doctor's precision report keeps, kept here too.
func TestAccuracyExitStatusIsNotAGate(t *testing.T) {
	root := accuracyFixture(t, true)
	if _, err := runAccuracyCmd(t, root); err != nil {
		t.Fatalf("60%% recall must not be an error: %v", err)
	}
}

// TestAccuracyWithoutIndexNamesTheFix: the two failure modes a user hits before
// any of this works must not read identically.
func TestAccuracyWithoutIndexNamesTheFix(t *testing.T) {
	root := isolate(t)
	_, err := runAccuracyCmd(t, root)
	if err == nil {
		t.Fatal("accuracy without an index should fail")
	}
	if !strings.Contains(err.Error(), "cortex init") {
		t.Errorf("error %q does not name the fix", err)
	}
}
