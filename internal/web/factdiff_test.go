package web

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/diffgraph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// twoRunSource is an index holding two runs.
//
// A fake, and it is the only way to reach the compared path from here:
// committing a run retires the one before it, so the real seeded fixture holds
// exactly one run — which is precisely the state the guidance test below
// asserts. Everything the fake returns is fed to the real internal/diffgraph,
// so what is under test is this handler's projection of a real comparison, not
// a stubbed report.
type twoRunSource struct {
	runs    map[int64]*model.IndexRun
	files   map[int64][]model.File
	symbols map[int64][]model.Symbol
	edges   map[int64][]model.Edge
	current int64
}

func (s *twoRunSource) CurrentRun(context.Context) (*model.IndexRun, error) {
	return s.runs[s.current], nil
}

func (s *twoRunSource) Run(_ context.Context, id int64) (*model.IndexRun, error) {
	run, ok := s.runs[id]
	if !ok {
		return nil, registry.ErrRunNotFound
	}
	return run, nil
}

func (s *twoRunSource) Files(_ context.Context, runID int64) ([]model.File, error) {
	return s.files[runID], nil
}

func (s *twoRunSource) Symbols(_ context.Context, runID int64) ([]model.Symbol, error) {
	return s.symbols[runID], nil
}

func (s *twoRunSource) Edges(_ context.Context, runID int64) ([]model.Edge, error) {
	return s.edges[runID], nil
}

func (s *twoRunSource) Counts(_ context.Context, runID int64) (registry.Counts, error) {
	return registry.Counts{
		Files:   int64(len(s.files[runID])),
		Symbols: int64(len(s.symbols[runID])),
		Edges:   int64(len(s.edges[runID])),
	}, nil
}

func (s *twoRunSource) LanguageCounts(context.Context, int64) (map[string]int, error) {
	return map[string]int{"go": 1}, nil
}

func (s *twoRunSource) ResolutionStats(context.Context, int64) (registry.ResolutionStats, error) {
	return registry.ResolutionStats{}, nil
}

func (s *twoRunSource) EndpointReport(context.Context, int64) ([]registry.Endpoint, error) {
	return nil, nil
}

func (s *twoRunSource) TableUsageReport(context.Context, int64) ([]registry.TableUsage, error) {
	return nil, nil
}

var (
	_ Registry   = (*twoRunSource)(nil)
	_ DiffSource = (*twoRunSource)(nil)
	_ RunLookup  = (*twoRunSource)(nil)
)

// newTwoRunSource seeds two runs: the base has a `high` CALLS edge and a test
// relationship; the head has downgraded the first, dropped the second, and
// gained one.
func newTwoRunSource() *twoRunSource {
	run := func(id int64) *model.IndexRun {
		return &model.IndexRun{
			ID: id, Status: model.StatusReady, IndexState: model.IndexStateComplete,
			StartedUnix: 100 * id, FinishedUnix: 100*id + 5,
			AccuracyMode: "auto", FilesIndexed: 2, FilesDiscovered: 2,
		}
	}
	files := []model.File{
		{ID: 1, Path: "internal/core/hub.go", Language: "go", ParseStatus: "ok"},
		{ID: 2, Path: "internal/core/hub_test.go", Language: "go", ParseStatus: "ok"},
	}
	sym := func(id int64, name, fqn string, fileID int64) model.Symbol {
		return model.Symbol{
			ID: id, Name: name, FQN: fqn, Kind: model.KindFunction, FileID: fileID,
			Range: model.Range{Start: model.Position{Line: int(id)}, End: model.Position{Line: int(id)}},
		}
	}
	symbols := []model.Symbol{
		sym(1, "hub", "core.hub", 1),
		sym(2, "caller", "core.caller", 1),
		sym(3, "TestHub", "core.TestHub", 2),
		sym(4, "newCaller", "core.newCaller", 1),
	}
	edge := func(src, dst int64, kind model.EdgeKind, c model.Confidence, src2 model.SourceType, line int) model.Edge {
		return model.Edge{
			SrcID: src, DstID: dst, Kind: kind, Confidence: c,
			Evidence: model.Evidence{
				FilePath: "internal/core/hub.go", Source: src2,
				Range: model.Range{Start: model.Position{Line: line}},
			},
		}
	}
	return &twoRunSource{
		current: 2,
		runs:    map[int64]*model.IndexRun{1: run(1), 2: run(2)},
		files:   map[int64][]model.File{1: files, 2: files},
		symbols: map[int64][]model.Symbol{1: symbols, 2: symbols},
		edges: map[int64][]model.Edge{
			1: {
				edge(2, 1, model.EdgeCalls, model.ConfidenceExact, model.SourceCompiler, 12),
				edge(1, 3, model.EdgeTestedBy, model.ConfidenceHigh, model.SourceSourceCode, 20),
			},
			2: {
				// The same relationship, no longer compiler-proven.
				edge(2, 1, model.EdgeCalls, model.ConfidenceHigh, model.SourceSourceCode, 12),
				// A new caller, and the covering test relationship is gone.
				edge(4, 1, model.EdgeCalls, model.ConfidenceMedium, model.SourceInferred, 31),
			},
		},
	}
}

func diffServer(t *testing.T) *Server {
	t.Helper()
	_, deps, _ := fixture(t)
	return newServer(t, newTwoRunSource(), deps, config.Config{ProjectName: "diff"})
}

// TestFactDiffReportsRelationshipsGainedLostAndRegraded is the populated case,
// and every assertion in it is a fact no line-based diff can produce.
func TestFactDiffReportsRelationshipsGainedLostAndRegraded(t *testing.T) {
	t.Parallel()
	srv := diffServer(t)

	for _, tc := range []struct {
		name  string
		query string

		wantFrom, wantTo int64
		wantDefaulted    []string
	}{
		{name: "both sides named", query: "?from=1&to=2", wantFrom: 1, wantTo: 2, wantDefaulted: []string{}},
		{name: "head defaults to the committed run", query: "?from=1", wantFrom: 1, wantTo: 2, wantDefaulted: []string{"to"}},
		{name: "both sides default", query: "", wantFrom: 1, wantTo: 2, wantDefaulted: []string{"to", "from"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got FactDiffResponse
			pillarGet(t, srv.handleFactDiff, "/api/diff"+tc.query, &got)

			if !got.Compared {
				t.Fatalf("compared is false: %+v", got.Help)
			}
			if got.Filters.From != tc.wantFrom || got.Filters.To != tc.wantTo {
				t.Errorf("compared %d..%d, want %d..%d",
					got.Filters.From, got.Filters.To, tc.wantFrom, tc.wantTo)
			}
			if len(got.Filters.Defaulted) != len(tc.wantDefaulted) {
				t.Errorf("defaulted = %v, want %v", got.Filters.Defaulted, tc.wantDefaulted)
			}
			if len(tc.wantDefaulted) > 0 && got.Filters.Detail == "" {
				t.Error("a defaulted side must say what it defaulted to")
			}

			// One CALLS edge gained, one regraded downward, one TESTED_BY lost.
			if got.Counts.RelationshipsGained != 1 {
				t.Errorf("relationships_gained = %d, want 1", got.Counts.RelationshipsGained)
			}
			if got.Counts.Downgraded != 1 || got.Counts.RelationshipsRegraded != 1 {
				t.Errorf("regraded = %d (downgraded %d), want 1 (1)",
					got.Counts.RelationshipsRegraded, got.Counts.Downgraded)
			}
			if got.Counts.TestCoverageLost != 1 {
				t.Errorf("test_coverage_lost = %d, want 1", got.Counts.TestCoverageLost)
			}
			if got.Counts.RelationshipsLost != 0 {
				t.Errorf("relationships_lost = %d, want 0 — the lost TESTED_BY belongs to the "+
					"test-coverage count, and double-counting it would break the arithmetic",
					got.Counts.RelationshipsLost)
			}
			if !got.Counts.Complete {
				t.Error("counts.complete is false with nothing capped")
			}
			if got.Counts.Scope == "" {
				t.Error("counts carry no scope sentence")
			}
		})
	}
}

// TestFactDiffRelationshipsCarryTierAndEvidence: a change to a relationship is
// only checkable if it says what tier it moved between and where to look.
func TestFactDiffRelationshipsCarryTierAndEvidence(t *testing.T) {
	t.Parallel()
	srv := diffServer(t)

	var got FactDiffResponse
	pillarGet(t, srv.handleFactDiff, "/api/diff?from=1&to=2", &got)

	var regraded *diffgraph.EdgeChange
	for i := range got.Relationships {
		if got.Relationships[i].Change == diffgraph.EdgeConfidenceChanged {
			regraded = &got.Relationships[i]
		}
	}
	if regraded == nil {
		t.Fatalf("no regrade in %+v", got.Relationships)
	}
	if regraded.From == nil || regraded.To == nil {
		t.Fatal("a regrade with no before and after states nothing")
	}
	if regraded.From.Confidence != model.ConfidenceExact || regraded.To.Confidence != model.ConfidenceHigh {
		t.Errorf("regrade = %s -> %s, want exact -> high",
			regraded.From.Confidence, regraded.To.Confidence)
	}
	if regraded.Direction != diffgraph.Downgraded {
		t.Errorf("direction = %q, want downgraded", regraded.Direction)
	}
	if len(regraded.To.Sites) == 0 {
		t.Fatal("a relationship change carries no evidence site")
	}
	site := regraded.To.Sites[0]
	if site.Path == "" || site.Line == 0 {
		t.Errorf("evidence = %s:%d, want a real file:line", site.Path, site.Line)
	}
	if regraded.Key.Kind != model.EdgeCalls {
		t.Errorf("key.kind = %q, want CALLS", regraded.Key.Kind)
	}
}

// TestFactDiffReportsCoverageMovement: a section that shrank because the head
// run indexed fewer files is not a deletion, and the two render identically.
func TestFactDiffReportsCoverageMovement(t *testing.T) {
	t.Parallel()
	srv := diffServer(t)

	var got FactDiffResponse
	pillarGet(t, srv.handleFactDiff, "/api/diff?from=1&to=2", &got)

	if got.Coverage.BaseFiles == 0 || got.Coverage.HeadFiles == 0 {
		t.Errorf("coverage reports no files on one side: %+v", got.Coverage)
	}
	if got.Coverage.Detail == "" {
		t.Error("coverage carries no sentence saying what its movement means")
	}
	if got.Coverage.EdgeDelta != got.Coverage.HeadEdges-got.Coverage.BaseEdges {
		t.Errorf("edges_delta = %d, want %d", got.Coverage.EdgeDelta,
			got.Coverage.HeadEdges-got.Coverage.BaseEdges)
	}
}

// TestFactDiffWithOneRunGuidesRatherThanBlanks is the ordinary state of a
// healthy index, and it must not read as "nothing changed".
func TestFactDiffWithOneRunGuidesRatherThanBlanks(t *testing.T) {
	t.Parallel()
	srv, _, runID := fixture(t)

	var got FactDiffResponse
	pillarGet(t, srv.handleFactDiff, "/api/diff", &got)

	if got.Compared {
		t.Fatal("compared is true over an index holding one run")
	}
	if got.Help == nil {
		t.Fatal("no guidance for an index with nothing to compare")
	}
	if !strings.Contains(got.Help.Title, "one run") {
		t.Errorf("guidance title = %q, want it to name the single-run state", got.Help.Title)
	}
	steps := strings.Join(got.Help.Steps, "\n")
	if !strings.Contains(steps, "cortex index") {
		t.Errorf("guidance names no command that produces a second run: %q", steps)
	}
	if got.Filters.To != runID {
		t.Errorf("head = %d, want the committed run %d", got.Filters.To, runID)
	}
	if got.Filters.Detail == "" {
		t.Error("the defaulted head is not explained")
	}
	if got.Filters.ProbeDepth == 0 {
		t.Error("the probe depth must be echoed: a default a reader cannot see is one they will misread")
	}
}

// TestFactDiffRefusesAnUnknownRun: a ref that matches no run must never fall
// back to whatever happens to be committed.
func TestFactDiffRefusesAnUnknownRun(t *testing.T) {
	t.Parallel()
	srv := diffServer(t)

	for _, tc := range []struct{ name, query string }{
		{"a run id that does not exist", "?from=99&to=2"},
		{"a ref that is not a run id", "?from=8f3c1ab&to=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got FactDiffResponse
			pillarGet(t, srv.handleFactDiff, "/api/diff"+tc.query, &got)
			if got.Compared {
				t.Fatal("an unmatched ref produced a comparison")
			}
			if got.Help == nil || !strings.Contains(got.Help.Title, "No indexed run matches") {
				t.Fatalf("guidance = %+v, want it to name the unmatched ref", got.Help)
			}
			if len(got.Relationships) != 0 {
				t.Error("an unmatched ref returned relationship rows")
			}
		})
	}
}

// TestFactDiffOfARunAgainstItselfSaysSo: identity is content-based, so this
// reports nothing by construction — which is not evidence the code is unchanged.
func TestFactDiffOfARunAgainstItselfSaysSo(t *testing.T) {
	t.Parallel()
	srv := diffServer(t)

	var got FactDiffResponse
	pillarGet(t, srv.handleFactDiff, "/api/diff?from=2&to=2", &got)
	if got.Compared {
		t.Fatal("a run compared against itself reported a comparison")
	}
	if got.Help == nil || !strings.Contains(got.Help.Title, fmt.Sprintf("run %d", 2)) {
		t.Fatalf("guidance = %+v, want it to name the repeated run", got.Help)
	}
}
