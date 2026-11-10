package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// seedPrecision builds an index whose precision report has known arithmetic.
//
// The shape is chosen so every claim the report makes is checkable:
//
//	go          high    8 judged, 7 agree            → 87.5%
//	            medium  4 judged, 1 agree            → 25.0%
//	            low     2 judged, 0 agree            →  0.0%
//	            all    14 judged, 8 agree            → 57.1%
//	coverage    20 heuristic CALLS edges, 14 judged  → 70.0%
//	            3 go edges + 2 ts edges never adjudicated, 1 line ambiguous
//	typescript  2 heuristic edges, no compiler facts → unmeasured
//
// withCompiler=false drops every compiler fact, which is the "no SCIP index
// imported at all" case.
func seedPrecision(t *testing.T, root string, withCompiler bool) {
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

	type hEdge struct {
		src, dst   int
		confidence string
		file       string
		line       int
	}
	var heuristic []hEdge
	// high: lines 101..108, all adjudicated; 107 is the one disagreement.
	for line := 101; line <= 108; line++ {
		dst := 2
		if line == 107 {
			dst = 3 // the compiler says 2 here
		}
		heuristic = append(heuristic, hEdge{1, dst, "high", "pkg/a.go", line})
	}
	// medium: lines 201..204, one agreement.
	for line := 201; line <= 204; line++ {
		dst := 3
		if line == 201 {
			dst = 2
		}
		heuristic = append(heuristic, hEdge{1, dst, "medium", "pkg/a.go", line})
	}
	// low: lines 301..302, no agreement.
	for line := 301; line <= 302; line++ {
		heuristic = append(heuristic, hEdge{1, 3, "low", "pkg/a.go", line})
	}
	// Never adjudicated: no compiler fact on these lines.
	for line := 401; line <= 403; line++ {
		heuristic = append(heuristic, hEdge{1, 2, "high", "pkg/a.go", line})
	}
	// The ambiguous line: two compiler targets, so excluded from judgement.
	heuristic = append(heuristic, hEdge{1, 2, "high", "pkg/a.go", 501})
	// TypeScript, with no compiler index behind it.
	for line := 10; line <= 11; line++ {
		heuristic = append(heuristic, hEdge{4, 5, "high", "web/app.ts", line})
	}
	for _, e := range heuristic {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
			                   evidence_file, evidence_line, source)
			   VALUES(1,%d,%d,'CALLS','%s','%s',%d,'source_code')`,
			e.src, e.dst, e.confidence, e.file, e.line))
	}

	if withCompiler {
		type cEdge struct{ dst, line int }
		var compiler []cEdge
		for line := 101; line <= 108; line++ {
			compiler = append(compiler, cEdge{2, line})
		}
		for line := 201; line <= 204; line++ {
			compiler = append(compiler, cEdge{2, line})
		}
		for line := 301; line <= 302; line++ {
			compiler = append(compiler, cEdge{2, line})
		}
		// Line 501 resolves to two distinct targets: ambiguous, excluded.
		compiler = append(compiler, cEdge{2, 501}, cEdge{3, 501})
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
			t.Fatalf("seed precision: %v\n%s", err, s)
		}
	}
}

func precisionFixture(t *testing.T, withCompiler bool) string {
	t.Helper()
	root := isolate(t)
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	seedPrecision(t, root, withCompiler)
	return root
}

// TestPrecisionReportIsActionable is the contract of the report as a trust
// document: the tier precisions, the reading rule tied to THIS repository's
// numbers, the coverage denominator, the unmeasured language, and the caveat
// that it is not recall.
func TestPrecisionReportIsActionable(t *testing.T) {
	root := precisionFixture(t, true)

	out, err := run(t, root, "doctor", "--resolution-precision")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	t.Logf("rendered report:\n%s", out)

	want := []struct{ what, substr string }{
		// Tier precisions, in reading-rule order with their verdicts.
		{"high tier", "high"},
		{"high precision", "87.5%"},
		{"medium precision", "25.0%"},
		{"low precision", "0.0%"},
		{"all-tiers precision", "57.1%"},
		{"act-on verdict", "act on"},
		{"show verdict", "show"},
		{"ignore verdict", "ignore"},

		// The reading rule, and it tied to the measured numbers rather than
		// to the documented ones.
		{"reading rule", "Act on `high`, show `medium`, ignore `low`."},
		{"rule tied to this repo", "On THIS repository `high` is 87.5% and `medium` is 25.0%"},
		{"min-confidence advice", "--min-confidence high"},

		// Coverage: the denominator the precision was computed over.
		{"judged vs existing", "Judged 14 of 20 heuristic CALLS edge(s) (70.0%)"},
		{"unjudged edges", "6"},
		{"neither right nor wrong", "neither right nor wrong"},
		{"ambiguous exclusion", "1 ambiguous line(s) were excluded"},
		{"judgeable lines", "14 line(s) the compiler resolved to"},

		// An unmeasured language must not read as a passing one.
		{"language table", "LANGUAGES"},
		{"unmeasured language", "typescript"},
		{"unmeasured status", "NOT MEASURED — no SCIP index imported for it"},
		{"measured language", "go"},

		// The caveat that survives reformatting. Asserted against the raw
		// output, unwrapped: a caveat somebody greps for must be greppable.
		{"not recall", notRecall},
		{"points at the method", "docs/accuracy.md"},
	}
	for _, w := range want {
		if !strings.Contains(out, w.substr) {
			t.Errorf("report is missing the %s: %q\n---\n%s", w.what, w.substr, out)
		}
	}

	// Warnings are line-wrapped, so match them on flattened whitespace.
	flat := strings.Join(strings.Fields(out), " ")
	for _, w := range []struct{ what, substr string }{
		{"unmeasured warning", "typescript carries heuristic edges that no compiler index"},
		{"unmeasured is not accurate", "which is not the same as accurate"},
		{"small-sample warning", "Only 14 edge(s) were judged"},
	} {
		if !strings.Contains(flat, w.substr) {
			t.Errorf("report is missing the %s: %q\n---\n%s", w.what, w.substr, out)
		}
	}

	// A measurement must not become the build gate. See the note in the source.
	if strings.Contains(out, "[FAIL") {
		t.Errorf("precision reporting must not fail a check:\n%s", out)
	}
}

// TestPrecisionJSONShapeIsStable pins the contract a CI job gates on.
func TestPrecisionJSONShapeIsStable(t *testing.T) {
	root := precisionFixture(t, true)

	out, err := run(t, root, "doctor", "--resolution-precision", "--json")
	if err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}

	t.Logf("emitted document:\n%s", out)

	// One document. Two top-level objects on the same stream is not JSON, and
	// was what this command used to emit.
	dec := json.NewDecoder(strings.NewReader(out))
	var doc struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
		Failed    int `json:"failed"`
		Precision struct {
			Measured    bool   `json:"measured"`
			Measures    string `json:"measures"`
			ReadingRule string `json:"reading_rule"`
			Tiers       []struct {
				Confidence string  `json:"confidence"`
				Verdict    string  `json:"verdict"`
				Judged     int64   `json:"judged"`
				Agree      int64   `json:"agree"`
				Disagree   int64   `json:"disagree"`
				Precision  float64 `json:"precision"`
				Measured   bool    `json:"measured"`
			} `json:"tiers"`
			All struct {
				Judged    int64   `json:"judged"`
				Precision float64 `json:"precision"`
			} `json:"all"`
			Coverage struct {
				HeuristicEdges int64   `json:"heuristic_edges"`
				JudgedEdges    int64   `json:"judged_edges"`
				UnjudgedEdges  int64   `json:"unjudged_edges"`
				JudgedFraction float64 `json:"judged_fraction"`
				CompilerFacts  int64   `json:"compiler_facts"`
				JudgeableLines int64   `json:"judgeable_lines"`
				AmbiguousLines int64   `json:"ambiguous_lines"`
			} `json:"coverage"`
			Languages []struct {
				Language       string `json:"language"`
				Files          int64  `json:"files"`
				HeuristicEdges int64  `json:"heuristic_edges"`
				CompilerFacts  int64  `json:"compiler_facts"`
				Measured       bool   `json:"measured"`
			} `json:"languages"`
			Unmeasured []string `json:"unmeasured_languages"`
			Warnings   []string `json:"warnings"`
			Caveats    []string `json:"caveats"`
			Reference  string   `json:"reference"`
		} `json:"resolution_precision"`
	}
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("doctor --json emitted non-JSON: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatalf("doctor --json emitted more than one document:\n%s", out)
	}

	if len(doc.Checks) == 0 {
		t.Error("checks disappeared from the JSON document")
	}
	p := doc.Precision
	if !p.Measured {
		t.Error("measured = false with compiler facts present")
	}
	if p.Measures != "precision" {
		t.Errorf("measures = %q, want precision", p.Measures)
	}
	if p.ReadingRule == "" {
		t.Error("reading_rule is empty")
	}
	if len(p.Tiers) != 3 {
		t.Fatalf("tiers = %d, want high/medium/low", len(p.Tiers))
	}
	wantTiers := []struct {
		confidence, verdict string
		judged, agree       int64
		precision           float64
	}{
		{"high", "act on", 8, 7, 0.875},
		{"medium", "show", 4, 1, 0.25},
		{"low", "ignore", 2, 0, 0},
	}
	for i, w := range wantTiers {
		got := p.Tiers[i]
		if got.Confidence != w.confidence || got.Verdict != w.verdict {
			t.Errorf("tier %d = %s/%s, want %s/%s", i, got.Confidence, got.Verdict,
				w.confidence, w.verdict)
		}
		if got.Judged != w.judged || got.Agree != w.agree {
			t.Errorf("tier %s judged/agree = %d/%d, want %d/%d",
				w.confidence, got.Judged, got.Agree, w.judged, w.agree)
		}
		if got.Disagree != w.judged-w.agree {
			t.Errorf("tier %s disagree = %d, want %d", w.confidence, got.Disagree, w.judged-w.agree)
		}
		if diff := got.Precision - w.precision; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("tier %s precision = %v, want %v", w.confidence, got.Precision, w.precision)
		}
		if !got.Measured {
			t.Errorf("tier %s measured = false with %d judged", w.confidence, got.Judged)
		}
	}
	if p.All.Judged != 14 {
		t.Errorf("all.judged = %d, want 14", p.All.Judged)
	}

	c := p.Coverage
	for _, w := range []struct {
		name      string
		got, want int64
	}{
		{"heuristic_edges", c.HeuristicEdges, 20},
		{"judged_edges", c.JudgedEdges, 14},
		{"unjudged_edges", c.UnjudgedEdges, 6},
		{"compiler_facts", c.CompilerFacts, 16},
		{"judgeable_lines", c.JudgeableLines, 14},
		{"ambiguous_lines", c.AmbiguousLines, 1},
	} {
		if w.got != w.want {
			t.Errorf("coverage.%s = %d, want %d", w.name, w.got, w.want)
		}
	}
	if diff := c.JudgedFraction - 0.7; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("coverage.judged_fraction = %v, want 0.7", c.JudgedFraction)
	}

	if len(p.Unmeasured) != 1 || p.Unmeasured[0] != "typescript" {
		t.Errorf("unmeasured_languages = %v, want [typescript]", p.Unmeasured)
	}
	var sawTS, sawGo bool
	for _, l := range p.Languages {
		switch l.Language {
		case "typescript":
			sawTS = true
			if l.Measured || l.CompilerFacts != 0 || l.HeuristicEdges != 2 {
				t.Errorf("typescript row = %+v, want unmeasured with 2 heuristic edges", l)
			}
		case "go":
			sawGo = true
			if !l.Measured || l.HeuristicEdges != 18 {
				t.Errorf("go row = %+v, want measured with 18 heuristic edges", l)
			}
		case "markdown":
			t.Errorf("markdown has no call edges and must not be listed: %+v", l)
		}
	}
	if !sawTS || !sawGo {
		t.Errorf("languages = %+v, want both go and typescript", p.Languages)
	}

	if len(p.Caveats) == 0 || p.Caveats[0] != notRecall {
		t.Errorf("caveats = %v, want the not-recall sentence first", p.Caveats)
	}
	if p.Reference != "docs/accuracy.md" {
		t.Errorf("reference = %q, want docs/accuracy.md", p.Reference)
	}
	// A small seeded sample must say so rather than let 87.5% read as settled.
	if len(p.Warnings) == 0 {
		t.Error("warnings are empty on a 14-edge sample with an unmeasured language")
	}
}

// TestPrecisionWithoutCompilerFactsRefusesToScore proves the report says
// "unmeasured" rather than printing a table of zeros — and still names every
// language it did not measure.
func TestPrecisionWithoutCompilerFactsRefusesToScore(t *testing.T) {
	root := precisionFixture(t, false)

	out, err := run(t, root, "doctor", "--resolution-precision")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	t.Logf("rendered report:\n%s", out)

	for _, want := range []string{
		"Not measured",
		"no compiler facts",
		"cortex scip import",
		"NOT MEASURED — no SCIP index imported for it",
		"go",
		"typescript",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unmeasured report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PRECISION\n  high") || strings.Contains(out, "0.0%") {
		t.Errorf("an unmeasured index must not print scores:\n%s", out)
	}

	// And the same in JSON, because a CI job gates on that field.
	jsonOut, err := run(t, root, "doctor", "--resolution-precision", "--json")
	if err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, jsonOut)
	}
	var doc struct {
		Precision struct {
			Measured   bool     `json:"measured"`
			Unmeasured []string `json:"unmeasured_languages"`
			Caveats    []string `json:"caveats"`
		} `json:"resolution_precision"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatalf("doctor --json invalid: %v\n%s", err, jsonOut)
	}
	if doc.Precision.Measured {
		t.Error("measured = true with no compiler facts in the index")
	}
	if len(doc.Precision.Unmeasured) != 2 {
		t.Errorf("unmeasured_languages = %v, want both go and typescript",
			doc.Precision.Unmeasured)
	}
	if len(doc.Precision.Caveats) == 0 || doc.Precision.Caveats[0] != notRecall {
		t.Errorf("the not-recall caveat must survive the unmeasured path: %v",
			doc.Precision.Caveats)
	}
}

// TestRenderPrecisionAtMeasuredScale renders the report at the size a real
// repository produces, and checks the two things only scale can break: the
// small-sample and below-band warnings must stay silent when the numbers are
// large and in band, and six-figure counts must stay readable.
//
// The tier percentages, the judged-line count and the ambiguous-line count are
// the ones measured against prometheus (727 Go files, scip-go). The per-tier
// judged/agree splits are reconstructed to reproduce those percentages, and
// the heuristic-edge total is illustrative — this test is about rendering, and
// the numbers are here so the layout is exercised at a real magnitude rather
// than to restate a measurement. docs/accuracy.md is the measurement.
func TestRenderPrecisionAtMeasuredScale(t *testing.T) {
	rep := buildPrecisionReport(registry.PrecisionReport{
		Rows: []registry.PrecisionRow{
			{Confidence: "high", Judged: 39_361, Agree: 34_126, Disagree: 5_235},
			{Confidence: "low", Judged: 2_000, Agree: 46, Disagree: 1_954},
			{Confidence: "medium", Judged: 12_880, Agree: 1_030, Disagree: 11_850},
		},
		HeuristicEdges: 69_504,
		CompilerEdges:  188_402,
		JudgeableLines: 54_241,
		AmbiguousLines: 17_448,
	}, []precisionLanguage{
		{Language: "go", Files: 727, HeuristicEdges: 69_504, CompilerFacts: 188_402, Measured: true},
	})

	var buf bytes.Buffer
	if err := renderPrecision(&buf, &rep); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Logf("rendered report:\n%s", out)

	for _, want := range []string{
		"86.7%", "8.0%", "2.3%", "64.9%", // the measured tier figures
		"Judged 54,241 of 69,504 heuristic CALLS edge(s) (78.0%)",
		"17,448 ambiguous line(s) were excluded",
		notRecall,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("a large, in-band measurement should raise no warnings: %v", rep.Warnings)
	}
	if strings.Contains(out, "NOT MEASURED") {
		t.Errorf("the only language has compiler facts; nothing should be unmeasured:\n%s", out)
	}
}

// TestHighTierBandWarning pins when the report stops vouching for `high`.
func TestHighTierBandWarning(t *testing.T) {
	rows := func(highJudged, highAgree int64) registry.PrecisionReport {
		return registry.PrecisionReport{
			Rows: []registry.PrecisionRow{
				{Confidence: "high", Judged: highJudged, Agree: highAgree,
					Disagree: highJudged - highAgree},
			},
			HeuristicEdges: highJudged, CompilerEdges: highJudged,
			JudgeableLines: highJudged,
		}
	}
	const phrase = "materially below the 86.8–96.1%"

	// A tier that has stopped meaning what the label says.
	broken := buildPrecisionReport(rows(10_000, 6_000), nil)
	if !strings.Contains(strings.Join(broken.Warnings, " "), phrase) {
		t.Errorf("60%% `high` raised no band warning: %v", broken.Warnings)
	}
	// prometheus as measured today: a tenth of a point under the documented
	// low is inside the spread of measuring a different codebase.
	prometheus := buildPrecisionReport(rows(39_361, 34_126), nil)
	if strings.Contains(strings.Join(prometheus.Warnings, " "), phrase) {
		t.Errorf("86.7%% `high` must not warn — that is the project's own "+
			"current number: %v", prometheus.Warnings)
	}
}

func TestHumanCountGroupsDigits(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"}, {7, "7"}, {999, "999"}, {1000, "1,000"},
		{54241, "54,241"}, {17448, "17,448"}, {1234567, "1,234,567"}, {-1000, "-1,000"},
	} {
		if got := humanCount(tc.in); got != tc.want {
			t.Errorf("humanCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
