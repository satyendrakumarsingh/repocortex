package architecture

import (
	"context"
	"errors"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A fake registry rather than SQLite: these tests are about rule evaluation and
// boundary attribution, and standing up a database would make the assertions
// harder to read without making them stronger.
type fakeRegistry struct {
	files   []model.File
	symbols []model.Symbol
	edges   []model.Edge
}

func (f *fakeRegistry) Files(context.Context, int64) ([]model.File, error) {
	return f.files, nil
}
func (f *fakeRegistry) Symbols(context.Context, int64) ([]model.Symbol, error) {
	return f.symbols, nil
}
func (f *fakeRegistry) Edges(context.Context, int64) ([]model.Edge, error) {
	return f.edges, nil
}

func layered() (*fakeRegistry, arch.Model) {
	reg := &fakeRegistry{
		files: []model.File{
			{ID: 1, Path: "src/controller/C.java"},
			{ID: 2, Path: "src/service/S.java"},
			{ID: 3, Path: "src/repository/R.java"},
			{ID: 4, Path: "scripts/tool.go"}, // in no component
		},
		symbols: []model.Symbol{
			{ID: 10, FileID: 1, Name: "handle"},
			{ID: 20, FileID: 2, Name: "update"},
			{ID: 30, FileID: 3, Name: "findById"},
			{ID: 40, FileID: 4, Name: "main"},
		},
	}
	m := arch.Model{
		Components: []arch.Component{
			{Name: "controller", Include: []string{"src/controller/**"}},
			{Name: "service", Include: []string{"src/service/**"}},
			{Name: "repository", Include: []string{"src/repository/**"}},
		},
		Rules: []arch.Rule{{
			ID: "layering", From: []string{"controller"},
			AllowedTo: []string{"service"}, Forbidden: []string{"repository"},
			Severity: "high",
		}},
	}
	return reg, m
}

func edge(src, dst int64, conf model.Confidence, line int) model.Edge {
	return model.Edge{
		SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: conf,
		Evidence: model.Evidence{
			FilePath: "src/controller/C.java",
			Range:    model.Range{Start: model.Position{Line: line}},
			Source:   model.SourceSourceCode,
		},
	}
}

func TestForbiddenCrossingIsReported(t *testing.T) {
	reg, m := layered()
	reg.edges = []model.Edge{edge(10, 30, model.ConfidenceHigh, 12)}

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(rep.Violations), rep.Violations)
	}
	v := rep.Violations[0]
	if v.RuleID != "layering" || v.From != "controller" || v.To != "repository" {
		t.Errorf("violation misattributed: %+v", v)
	}
	if v.Severity != "high" {
		t.Errorf("severity = %q, want high", v.Severity)
	}
	// Evidence is what makes the claim checkable.
	if v.FilePath == "" || v.Line != 12 {
		t.Errorf("no evidence location: %+v", v)
	}
	if v.Confidence != string(model.ConfidenceHigh) {
		t.Errorf("relationship confidence not carried: %+v", v)
	}
}

func TestAllowedCrossingIsNotAViolation(t *testing.T) {
	reg, m := layered()
	reg.edges = []model.Edge{
		edge(10, 20, model.ConfidenceHigh, 5), // controller -> service: allowed
		edge(20, 30, model.ConfidenceHigh, 7), // service -> repository: no rule
	}
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Errorf("allowed dependencies flagged: %+v", rep.Violations)
	}
	// Both crossings must still appear in the map: the inventory is how a
	// missing rule gets noticed.
	if len(rep.Crossings) != 2 {
		t.Errorf("got %d crossings, want 2: %+v", len(rep.Crossings), rep.Crossings)
	}
}

// TestIntraComponentEdgesAreNotCrossings. A call inside one component crosses
// no boundary, and counting it would drown the report.
func TestIntraComponentEdgesAreNotCrossings(t *testing.T) {
	reg, m := layered()
	reg.symbols = append(reg.symbols, model.Symbol{ID: 11, FileID: 1, Name: "helper"})
	reg.edges = []model.Edge{edge(10, 11, model.ConfidenceHigh, 3)}

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Crossings) != 0 || rep.EdgesConsidered != 0 {
		t.Errorf("an intra-component call was counted as a crossing: %+v", rep.Crossings)
	}
}

// TestUnassignedSymbolsAreCountedNotChecked. A rule can only be broken by code
// the model covers, so the uncovered part has to be visible or the check
// overstates what it proved.
func TestUnassignedSymbolsAreCountedNotChecked(t *testing.T) {
	reg, m := layered()
	reg.edges = []model.Edge{edge(40, 30, model.ConfidenceHigh, 1)} // unassigned -> repository

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unassigned == 0 {
		t.Error("a symbol outside every component was not counted")
	}
	if len(rep.Violations) != 0 {
		t.Errorf("a crossing from an unassigned symbol was judged: %+v", rep.Violations)
	}
}

func TestMinConfidenceFiltersRelationships(t *testing.T) {
	reg, m := layered()
	reg.edges = []model.Edge{edge(10, 30, model.ConfidenceLow, 12)}

	rep, err := Check(context.Background(), reg, 1, m, model.ConfidenceHigh)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Errorf("a low-confidence edge survived --min-confidence high: %+v", rep.Violations)
	}

	rep, err = Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("the low-confidence violation was dropped without a filter: %+v", rep.Violations)
	}
	if rep.Violations[0].Confidence != string(model.ConfidenceLow) {
		t.Error("the weak confidence was not carried through, so a lead reads as a fact")
	}
}

// TestNoModelIsNotAPass. Reporting zero violations for a repository that
// declared no architecture is a pass mark nobody earned.
func TestNoModelIsNotAPass(t *testing.T) {
	reg, _ := layered()
	_, err := Check(context.Background(), reg, 1, arch.Model{}, "")
	if err == nil {
		t.Fatal("an empty model returned a clean report")
	}
	if !errors.Is(err, ErrNoModel) {
		t.Errorf("want ErrNoModel, got %v", err)
	}
}

// TestEmptyComponentIsSurfaced. A component whose patterns match nothing makes
// every rule naming it silently inert, which is the worst failure this report
// can have.
func TestEmptyComponentIsSurfaced(t *testing.T) {
	reg, m := layered()
	m.Components = append(m.Components, arch.Component{
		Name: "ghost", Include: []string{"nowhere/**"},
	})
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range rep.Components {
		if c.Name == "ghost" {
			found = true
			if c.Symbols != 0 {
				t.Errorf("ghost reported %d symbols", c.Symbols)
			}
		}
	}
	if !found {
		t.Errorf("a component matching nothing was omitted entirely: %+v", rep.Components)
	}
}

// TestOutputIsDeterministic. This report belongs in CI, and a diff that
// reorders on every run makes "did this get worse?" unanswerable.
func TestOutputIsDeterministic(t *testing.T) {
	reg, m := layered()
	reg.edges = []model.Edge{
		edge(10, 30, model.ConfidenceHigh, 12),
		edge(10, 30, model.ConfidenceHigh, 40),
		edge(10, 20, model.ConfidenceHigh, 5),
	}
	var first string
	for i := 0; i < 20; i++ {
		rep, err := Check(context.Background(), reg, 1, m, "")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, v := range rep.Violations {
			got += v.RuleID + v.FilePath + string(rune(v.Line)) + ";"
		}
		for _, c := range rep.Crossings {
			got += c.From + ">" + c.To + ";"
		}
		for _, c := range rep.Components {
			got += c.Name + ";"
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("report order varies between runs:\n%s\n%s", first, got)
		}
	}
}
