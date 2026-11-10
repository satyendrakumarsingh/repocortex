package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// seedOutcomes writes n references and marks them with the given mix of
// outcomes, so ResolutionStats has something real to aggregate.
//
// It goes through the writer and MarkResolutions rather than inserting counts,
// because the thing under test is a number the registry derives from rows; a
// fixture that set the aggregate directly would pass even if the GROUP BY were
// wrong.
func seedOutcomes(resolved, ambiguous, unresolved int) func(context.Context, *registry.Registry, int64) {
	return func(ctx context.Context, reg *registry.Registry, runID int64) {
		w, err := reg.NewWriter(ctx, runID)
		if err != nil {
			panic(err)
		}
		f := model.File{Path: "x/Refs.java", Language: "java", ContentHash: "hres", ParseStatus: "ok"}
		fid, _ := w.AddFile(ctx, &f)
		total := resolved + ambiguous + unresolved
		for i := 0; i < total; i++ {
			_ = w.AddReference(ctx, fid, registry.Reference{
				Name: "ref", Kind: "reference.call", Line: i + 1,
			})
		}
		_ = w.Flush(ctx)
		_ = w.Close()

		outcomes := make([]registry.RefOutcome, 0, total)
		add := func(n int, r registry.Resolution) {
			for i := 0; i < n; i++ {
				outcomes = append(outcomes, registry.RefOutcome{
					RefID: int64(len(outcomes) + 1), Resolution: r,
				})
			}
		}
		add(resolved, registry.ResolutionResolved)
		add(ambiguous, registry.ResolutionAmbiguous)
		add(unresolved, registry.ResolutionUnresolved)
		_ = reg.MarkResolutions(ctx, outcomes)
	}
}

// TestResolutionCoverageStatesTheRateAndWhatItExcluded is the honesty property
// of the whole tool.
//
// registry.ResolutionStats.Bindable documents the bargain: the published rate
// may exclude references that matched no definition here, but ONLY because the
// excluded population is printed beside it. A summary carrying the percentage
// alone would be the overclaim the exclusion was permitted to avoid.
func TestResolutionCoverageStatesTheRateAndWhatItExcluded(t *testing.T) {
	cases := []struct {
		name       string
		resolved   int
		ambiguous  int
		unresolved int
		want       []string
	}{
		{
			name: "a partially resolved index", resolved: 40, ambiguous: 5, unresolved: 15,
			// 40 of 45 bindable = 88.9%, and both excluded populations named.
			want: []string{"%", "40", "45", "5", "15", "60"},
		},
		{
			name: "everything bindable bound", resolved: 20, unresolved: 30,
			want: []string{"100", "20", "30", "50"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := newGraphTestServer(t, seedOutcomes(tc.resolved, tc.ambiguous, tc.unresolved))
			res := callTool(t, sess, "resolution_coverage", map[string]any{})

			sc, ok := res.StructuredContent.(map[string]any)
			if !ok {
				t.Fatalf("StructuredContent = %#v, want a JSON object", res.StructuredContent)
			}
			summary, _ := sc["summary"].(string)
			for _, want := range tc.want {
				if !strings.Contains(summary, want) {
					t.Errorf("Summary %q does not carry %q", summary, want)
				}
			}
			if rev, _ := sc["index_revision"].(float64); rev == 0 {
				t.Error("index_revision unset; every response must be attributable to a run")
			}
			if prov, ok := sc["provenance"].(map[string]any); !ok {
				t.Error("no provenance object: the tool bypassed reply()")
			} else if caveat, _ := prov["caveat"].(string); caveat == "" {
				t.Error("Provenance.Caveat is empty")
			}
		})
	}
}

// An index whose references were extracted but never resolved must not report
// 0%: "we looked and found little" and "we never looked" are different facts,
// and only the second is fixed by running 'cortex resolve'.
func TestResolutionCoverageDistinguishesUnresolvedFromNeverResolved(t *testing.T) {
	sess := newGraphTestServer(t, func(ctx context.Context, reg *registry.Registry, runID int64) {
		w, _ := reg.NewWriter(ctx, runID)
		f := model.File{Path: "x/Refs.java", Language: "java", ContentHash: "hp", ParseStatus: "ok"}
		fid, _ := w.AddFile(ctx, &f)
		for i := 0; i < 7; i++ {
			_ = w.AddReference(ctx, fid, registry.Reference{Name: "ref", Kind: "reference.call", Line: i + 1})
		}
		_ = w.Flush(ctx)
		_ = w.Close()
	})

	text := resultText(callTool(t, sess, "resolution_coverage", map[string]any{}))
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "cortex resolve") {
		t.Errorf("an unresolved index must name the command that fixes it:\n%s", text)
	}
	if !strings.Contains(lower, "call graph has not been built") {
		t.Errorf("an unresolved index must say the graph was never built:\n%s", text)
	}
}

// TestResolutionCoverageUsesItsOwnFloorNotCoverageFloor pins bindingFloor as a
// constant independent of tools.go's coverageFloor.
//
// 55 resolved, 45 ambiguous, 1000 unresolved gives Bindable = 100 and
// BindingRate = 0.55: at or above coverageFloor (0.5), so a check still wired
// to coverageFloor would stay silent, but below bindingFloor (0.6), so the
// binding-rate check must fire. If resolutionNotes were switched back to
// comparing against coverageFloor, this test fails.
func TestResolutionCoverageUsesItsOwnFloorNotCoverageFloor(t *testing.T) {
	sess := newGraphTestServer(t, seedOutcomes(55, 45, 1000))
	text := resultText(callTool(t, sess, "resolution_coverage", map[string]any{}))
	if !strings.Contains(text, "COVERAGE TOO LOW") {
		t.Errorf("BindingRate 55%% is below bindingFloor (60%%) and must trip the low-coverage "+
			"note even though it is at/above coverageFloor (50%%); got:\n%s", text)
	}
}

// The tool must be registered under a description, like every other one: a tool
// with no entry in toolDescriptions ships with an empty prompt.
func TestResolutionCoverageIsDescribed(t *testing.T) {
	desc := toolDescriptions()["resolution_coverage"]
	if desc == "" {
		t.Fatal("resolution_coverage has no description")
	}
	if !strings.Contains(strings.ToLower(desc), "lower bound") &&
		!strings.Contains(strings.ToLower(desc), "empty") {
		t.Errorf("the description must say what a low rate does to every other answer, got %q", desc)
	}
}
