package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A profile is stored per definition, replaced by the next import, kept honest
// when a file changes, and removed with the run: the four things a tool that
// quotes coverage depends on.
func TestCoverageRoundTrip(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	ids := writeContent(t, r, run.ID, []fileSpec{
		{path: "svc/a.go", lang: "go", hash: "h1", syms: []symSpec{
			{fqn: "svc.f", name: "f", line: 10, kind: model.KindFunction}, // lines 10-13
			{fqn: "svc.g", name: "g", line: 20, kind: model.KindFunction}, // lines 20-23
			{fqn: "svc.T", name: "T", line: 30, kind: model.KindField},    // no row for a field
		}},
		{path: "svc/b.go", lang: "go", hash: "h2", syms: []symSpec{
			{fqn: "svc.h", name: "h", line: 5, kind: model.KindMethod},
		}},
	}, nil)
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	count := func(table string, runID int64) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE index_run_id = ?", runID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if p, err := r.CoverageProfile(ctx, run.ID); err != nil || p != nil {
		t.Fatalf("no import yet: profile = %v, err = %v", p, err)
	}

	// A Go profile names files by import path; the index holds the short path.
	res, err := r.ImportCoverage(ctx, run.ID, CoverageInput{
		ContentHash: "p1", Format: "go", GitRevision: "abc1234def",
		Files: []CoverageFile{
			{Path: "example.com/m/svc/a.go", Lines: map[int]int{10: 1, 11: 0, 12: 3, 20: 0, 21: 0}},
			{Path: "example.com/m/gone/z.go", Lines: map[int]int{1: 1}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Symbols != 2 || res.FilesMatched != 1 || len(res.FilesUnmatched) != 1 {
		t.Fatalf("result = %+v, want 2 symbols, 1 matched, 1 unmatched", res)
	}
	got, err := r.SymbolCoverage(ctx, []int64{ids["svc.f"], ids["svc.g"], ids["svc.T"], ids["svc.h"]})
	if err != nil {
		t.Fatal(err)
	}
	if c := got[ids["svc.f"]]; c.State != CoverageMeasured || c.Covered != 2 || c.Total != 3 || c.MaxHits != 3 {
		t.Errorf("f = %+v, want 2 of 3 lines, max 3 hits", c)
	}
	if c := got[ids["svc.g"]]; c.State != CoverageMeasured || c.Covered != 0 || c.Total != 2 {
		t.Errorf("g = %+v, want 0 of 2", c)
	}
	if c := got[ids["svc.T"]]; c.State != CoverageUnmeasured {
		t.Errorf("T = %+v, want unmeasured: its file is in the profile but it has no instrumented line", c)
	}
	if c := got[ids["svc.h"]]; c.State != CoverageNotInProfile {
		t.Errorf("h = %+v, want not-in-profile", c)
	}
	p, _ := r.CoverageProfile(ctx, run.ID)
	if p == nil || p.GitRevision != "abc1234def" || p.ContentHash != "p1" || p.FilesUnmatched != 1 {
		t.Errorf("profile = %+v", p)
	}

	// A second import replaces the first.
	res, err = r.ImportCoverage(ctx, run.ID, CoverageInput{
		ContentHash: "p2", Format: "lcov",
		Files: []CoverageFile{{Path: "svc/b.go", Lines: map[int]int{5: 1, 6: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Symbols != 1 || count("symbol_coverage", run.ID) != 1 || count("coverage_files", run.ID) != 1 || count("coverage_imports", run.ID) != 1 {
		t.Fatalf("after replace: %+v, symbol rows %d, file rows %d", res, count("symbol_coverage", run.ID), count("coverage_files", run.ID))
	}

	// A file that changes after the import is stale, and its old rows are gone
	// with its old symbols (no delete of ours is needed).
	if _, err := r.ImportCoverage(ctx, run.ID, CoverageInput{
		ContentHash: "p3", Format: "go",
		Files: []CoverageFile{{Path: "svc/a.go", Lines: map[int]int{10: 1, 20: 1}}},
	}); err != nil {
		t.Fatal(err)
	}
	if n := count("symbol_coverage", run.ID); n != 2 {
		t.Fatalf("symbol rows = %d, want 2", n)
	}
	changed := &model.File{Path: "svc/a.go", Language: "go", ContentHash: "h1-changed", ParseStatus: "ok"}
	newSym := model.Symbol{Kind: model.KindFunction, Name: "f", FQN: "svc.f", Confidence: model.ConfidenceHigh,
		Source: model.SourceSourceCode, Range: model.Range{Start: model.Position{Line: 10}, End: model.Position{Line: 13}}}
	if err := r.ReplaceFile(ctx, run.ID, changed, []model.Symbol{newSym}, nil, AllowCommittedRun()); err != nil {
		t.Fatal(err)
	}
	if n := count("symbol_coverage", run.ID); n != 0 {
		t.Errorf("symbol rows after the file was re-indexed = %d, want 0", n)
	}
	var newID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE index_run_id = ? AND fqn = 'svc.f'`, run.ID).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	got, _ = r.SymbolCoverage(ctx, []int64{newID})
	if got[newID].State != CoverageStale {
		t.Errorf("re-indexed f = %+v, want stale", got[newID])
	}

	// The profile leaves with the run it described.
	next, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRun(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"coverage_imports", "coverage_files", "symbol_coverage"} {
		if n := count(table, run.ID); n != 0 {
			t.Errorf("%s rows for the retired run = %d, want 0", table, n)
		}
	}
	var left int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_coverage`).Scan(&left)
	if left != 0 {
		t.Errorf("symbol_coverage rows left = %d", left)
	}
}

// A short tail that names two files is not guessed.
func TestCoverageAmbiguousPathIsUnmatched(t *testing.T) {
	m := newPathMatcher(map[string]string{"a/Foo.java": "1", "b/Foo.java": "2", "a/Bar.java": "3"})
	if _, ok := m.match("Foo.java"); ok {
		t.Error("Foo.java names two files and must not match")
	}
	if p, ok := m.match("Bar.java"); !ok || p != "a/Bar.java" {
		t.Errorf("Bar.java = %q, %v", p, ok)
	}
	if p, ok := m.match("/home/me/repo/b/Foo.java"); !ok || p != "b/Foo.java" {
		t.Errorf("absolute path = %q, %v", p, ok)
	}
}
