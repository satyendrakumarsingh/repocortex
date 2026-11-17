package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// coverageServer is the wellCoveredGo index with a profile stored over it:
// serve (lines 5-15) ran on 6 of its 10 instrumented lines, start (20-30) ran
// on none, stop (40-50) ran fully. With changeAfter, app/app.go is re-indexed
// after the import, as an edit would.
func coverageServer(t *testing.T, withProfile, changeAfter bool) fullServer {
	t.Helper()
	b := &indexBuilder{}
	wellCoveredGo(b)
	return newFullServer(t, func(ctx context.Context, reg *registry.Registry, runID int64) {
		b.seed(ctx, reg, runID)
		if !withProfile {
			return
		}
		lines := map[int]int{}
		for l := 5; l <= 14; l++ {
			if l <= 10 {
				lines[l] = 2
			} else {
				lines[l] = 0
			}
		}
		lines[20], lines[21] = 0, 0
		lines[40], lines[41] = 1, 1
		if _, err := reg.ImportCoverage(ctx, runID, registry.CoverageInput{
			ContentHash: "p", Format: "go", GitRevision: "087b9cb1234",
			Files: []registry.CoverageFile{{Path: "example.com/app/app/app.go", Lines: lines}},
		}); err != nil {
			panic(err)
		}
		if changeAfter {
			f := &model.File{Path: "app/app.go", Language: "go", ContentHash: "edited", ParseStatus: "ok"}
			serve := model.Symbol{Kind: model.KindFunction, Name: "serve", FQN: model.DefaultFQN("app/app.go", "serve"),
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
				Range: model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 15}}}
			if err := reg.ReplaceFile(ctx, runID, f, []model.Symbol{serve}, nil, registry.AllowCommittedRun()); err != nil {
				panic(err)
			}
		}
	})
}

func TestImpactAndTestsUseStoredCoverage(t *testing.T) {
	fs := coverageServer(t, true, false)

	text, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "serve"})
	flat := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{
		"serve: 6 of 10 instrumented lines covered in the coverage imported at 087b9cb on ",
		"1 of 2 measured impacted caller(s) have NO covered lines in that run: start.",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("analyze_impact missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(flat, "have NO covered lines in that run: start, stop") {
		t.Errorf("stop ran and must not be listed:\n%s", text)
	}

	// start has zero covered lines: no test executed it, whatever the graph says.
	text, _ = answer(t, fs.sess, "find_tests", map[string]any{"symbol": "start"})
	flat = strings.Join(strings.Fields(text), " ")
	if !strings.Contains(flat, "no test executed start in the coverage imported at 087b9cb") {
		t.Errorf("find_tests missing the zero-coverage note:\n%s", text)
	}
	// serve ran, so there is nothing to add.
	text, _ = answer(t, fs.sess, "find_tests", map[string]any{"symbol": "serve"})
	if strings.Contains(text, "no test executed") {
		t.Errorf("serve ran, but find_tests says no test executed it:\n%s", text)
	}
}

func TestCoverageNotesAreSilentWithoutAProfile(t *testing.T) {
	fs := coverageServer(t, false, false)
	for tool, sym := range map[string]string{"analyze_impact": "serve", "find_tests": "start"} {
		text, _ := answer(t, fs.sess, tool, map[string]any{"symbol": sym})
		if strings.Contains(text, "Coverage:") || strings.Contains(text, "coverage imported") {
			t.Errorf("%s mentions coverage though none is stored:\n%s", tool, text)
		}
	}
}

func TestCoverageOfAChangedFileIsStaleAndNotCounted(t *testing.T) {
	fs := coverageServer(t, true, true)
	text, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "serve"})
	flat := strings.Join(strings.Fields(text), " ")
	if !strings.Contains(flat, "STALE for serve: its file changed after the coverage imported at 087b9cb") {
		t.Errorf("a changed file's coverage was not marked stale:\n%s", text)
	}
	if strings.Contains(flat, "instrumented lines covered") {
		t.Errorf("stale numbers were quoted:\n%s", text)
	}
	text, _ = answer(t, fs.sess, "find_tests", map[string]any{"symbol": "serve"})
	if strings.Contains(text, "no test executed") {
		t.Errorf("find_tests used stale coverage:\n%s", text)
	}
}
