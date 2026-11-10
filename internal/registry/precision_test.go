package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// precisionContent lays out one file and a controlled mix of heuristic and
// compiler edges, so the arithmetic can be checked against a hand-computed
// answer rather than against whatever the code happens to produce.
func precisionContent(withCompiler bool) ([]fileSpec, []edgeSpec) {
	files := []fileSpec{{
		path: "a.go", lang: "go", hash: "h",
		syms: []symSpec{
			{fqn: "a.go#A", name: "A", line: 1, kind: model.KindFunction},
			{fqn: "a.go#B", name: "B", line: 50, kind: model.KindFunction},
			{fqn: "a.go#C", name: "C", line: 60, kind: model.KindFunction},
			{fqn: "a.go#D", name: "D", line: 70, kind: model.KindFunction},
		},
	}}

	h := func(dst string, conf model.Confidence, line int) edgeSpec {
		return edgeSpec{srcFQN: "a.go#A", dstFQN: dst, kind: model.EdgeCalls,
			conf: conf, source: model.SourceSourceCode, evFile: "a.go", evLine: line}
	}
	c := func(dst string, line int) edgeSpec {
		return edgeSpec{srcFQN: "a.go#A", dstFQN: dst, kind: model.EdgeReferences,
			conf: model.ConfidenceExact, source: model.SourceCompiler, evFile: "a.go", evLine: line}
	}

	edges := []edgeSpec{
		h("a.go#B", model.ConfidenceHigh, 10),   // compiler agrees below
		h("a.go#D", model.ConfidenceHigh, 20),   // compiler says C below: disagree
		h("a.go#B", model.ConfidenceMedium, 30), // line 30 is ambiguous below
		h("a.go#B", model.ConfidenceLow, 40),    // compiler silent on line 40
	}
	if withCompiler {
		edges = append(edges,
			c("a.go#B", 10),
			c("a.go#C", 20),
			// Two distinct targets on line 30 make it ambiguous: which one the
			// heuristic "should" have picked is not defined, so it is excluded.
			c("a.go#B", 30),
			c("a.go#C", 30),
		)
	}
	return files, edges
}

// TestResolutionPrecisionCountsOnlyUnambiguousLines pins the arithmetic AND the
// restriction that makes it meaningful.
//
// The restriction is the whole method. Matching heuristic edges to compiler
// facts by line alone would score an edge against a reference it was never
// about, on the quarter of real lines that carry more than one target; matching
// by column discards two thirds of the comparison, because the two tools
// disagree about which column a call sits at (`svc.Method()` — one records
// `svc`, the other `Method`). Judging only lines the compiler resolved to
// exactly one target is what makes the published figure defensible, so a change
// that quietly widened it would inflate every number in docs/accuracy.md.
func TestResolutionPrecisionCountsOnlyUnambiguousLines(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestRegistry(t)
	run, err := reg.BeginRun(ctx, "auto", "precision")
	if err != nil {
		t.Fatal(err)
	}
	files, edges := precisionContent(true)
	writeContent(t, reg, run.ID, files, edges)

	rep, err := reg.ResolutionPrecision(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	byTier := map[string]PrecisionRow{}
	for _, r := range rep.Rows {
		byTier[r.Confidence] = r
	}

	if got := byTier["high"]; got.Judged != 2 || got.Agree != 1 || got.Disagree != 1 {
		t.Errorf("high = %+v, want judged 2, agree 1, disagree 1", got)
	}
	if got, ok := byTier["medium"]; ok {
		t.Errorf("medium was judged (%+v); its only edge sits on an ambiguous line "+
			"and must be excluded", got)
	}
	if got, ok := byTier["low"]; ok {
		t.Errorf("low was judged (%+v); the compiler has no fact on its line", got)
	}
	if rep.AmbiguousLines != 1 {
		t.Errorf("AmbiguousLines = %d, want 1 (line 30)", rep.AmbiguousLines)
	}
	if rep.JudgeableLines != 2 {
		t.Errorf("JudgeableLines = %d, want 2 (lines 10 and 20)", rep.JudgeableLines)
	}
	if rep.HeuristicEdges != 4 {
		t.Errorf("HeuristicEdges = %d, want 4", rep.HeuristicEdges)
	}
	if rep.CompilerEdges != 4 {
		t.Errorf("CompilerEdges = %d, want 4", rep.CompilerEdges)
	}
	if p := byTier["high"].Precision(); p != 0.5 {
		t.Errorf("high precision = %v, want 0.5", p)
	}
}

// TestResolutionPrecisionWithoutCompilerFactsJudgesNothing guards the reading
// that would be most flattering and least true.
//
// With nothing imported, every heuristic edge is unadjudicated. That has to
// report as nothing judged, never as nothing disagreed: 100% precision over an
// empty denominator is exactly the shape of a number that ends up quoted in a
// README.
func TestResolutionPrecisionWithoutCompilerFactsJudgesNothing(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestRegistry(t)
	run, err := reg.BeginRun(ctx, "auto", "precision-empty")
	if err != nil {
		t.Fatal(err)
	}
	files, edges := precisionContent(false)
	writeContent(t, reg, run.ID, files, edges)

	rep, err := reg.ResolutionPrecision(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 0 {
		t.Errorf("rows = %+v, want none: nothing is judgeable without compiler facts", rep.Rows)
	}
	if rep.CompilerEdges != 0 {
		t.Errorf("CompilerEdges = %d, want 0", rep.CompilerEdges)
	}
	if rep.HeuristicEdges != 4 {
		t.Errorf("HeuristicEdges = %d, want 4", rep.HeuristicEdges)
	}
	var zero PrecisionRow
	if p := zero.Precision(); p != 0 {
		t.Errorf("Precision() over an empty denominator = %v, want 0", p)
	}
}
