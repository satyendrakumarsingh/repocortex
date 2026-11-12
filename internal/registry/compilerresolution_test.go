package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A one-file repository with two definitions the compiler can point at, and a
// third that shares a name with one of them so ambiguity can be tested.
//
//	svc.go:10  Save     (the definition callers mean)
//	svc.go:20  Load
//	dup.go:10  Save     (a second Save, so a line naming both settles nothing)
//
// References live in use.go. Every one is recorded exactly as the parser writes
// them and as the resolver left them, because the whole question is whether a
// compiler fact can settle a row the resolver could not.
type crFixture struct {
	reg    *Registry
	runID  int64
	symIDs map[string]int64
	useID  int64
}

func newCRFixture(t *testing.T) *crFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := New(db)
	run, err := reg.BeginRun(ctx, "auto", "cr")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	f := &crFixture{reg: reg, runID: run.ID, symIDs: map[string]int64{}}
	fileIDs := map[string]int64{}
	file := func(path string) int64 {
		if id, ok := fileIDs[path]; ok {
			return id
		}
		id, err := w.AddFile(ctx, &model.File{
			Path: path, Language: "go", ContentHash: "h-" + path, ParseStatus: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		fileIDs[path] = id
		return id
	}
	add := func(path, name string, line int, key string) {
		fileID := file(path)
		s := model.Symbol{
			FileID: fileID, Kind: model.KindFunction, Name: name, FQN: path + ":" + name,
			Range:      model.Range{Start: model.Position{Line: line}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		id, err := w.AddSymbol(ctx, &s)
		if err != nil {
			t.Fatal(err)
		}
		f.symIDs[key] = id
	}
	add("svc.go", "Save", 10, "Save")
	add("svc.go", "Load", 20, "Load")
	add("dup.go", "Save", 10, "OtherSave")

	useID := file("use.go")
	f.useID = useID

	// caller is the symbol the edges hang off. Its own position is irrelevant.
	callerSym := model.Symbol{
		FileID: useID, Kind: model.KindFunction, Name: "caller", FQN: "use.go:caller",
		Range:      model.Range{Start: model.Position{Line: 1}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	callerID, err := w.AddSymbol(ctx, &callerSym)
	if err != nil {
		t.Fatal(err)
	}
	f.symIDs["caller"] = callerID

	for _, r := range []struct {
		name string
		line int
	}{
		{"Save", 100}, // the compiler settles this one
		{"Load", 101}, // and this one, on a line that also names Save
		{"Save", 101}, // two Saves reachable here: stays ambiguous
		{"Save", 102}, // no compiler fact at all
	} {
		if err := w.AddReference(ctx, useID, Reference{
			Name: r.name, Kind: "reference.call", Line: r.line, ArgCount: model.ArityUnknown,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	// The resolver's verdict: it could not choose for any of them.
	if _, err := db.ExecContext(ctx,
		`UPDATE symbol_references SET resolution = 'ambiguous' WHERE index_run_id = ?`,
		run.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

// addCompilerEdge writes one compiler fact at a source position.
func (f *crFixture) addCompilerEdge(t *testing.T, line int, dst string) {
	t.Helper()
	if _, err := f.reg.UpgradeEdges(context.Background(), f.runID, []model.Edge{{
		SrcID: f.symIDs["caller"], DstID: f.symIDs[dst],
		Kind: model.EdgeReferences, Confidence: model.ConfidenceExact,
		Evidence: model.Evidence{
			FilePath: "use.go",
			Range:    model.Range{Start: model.Position{Line: line}},
			Source:   model.SourceCompiler, Detail: "scip: scip-go v0.9.9",
		},
	}}); err != nil {
		t.Fatal(err)
	}
}

func (f *crFixture) refs(t *testing.T) map[string]struct {
	resolution, prior, source string
	dst                       int64
} {
	t.Helper()
	rows, err := f.reg.db.QueryContext(context.Background(), `
		SELECT name, start_line, resolution, prior_resolution, resolution_source,
		       COALESCE(resolved_symbol_id, 0)
		  FROM symbol_references WHERE index_run_id = ?
		 ORDER BY start_line, name`, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]struct {
		resolution, prior, source string
		dst                       int64
	}{}
	for rows.Next() {
		var name, res, prior, src string
		var line int
		var dst int64
		if err := rows.Scan(&name, &line, &res, &prior, &src, &dst); err != nil {
			t.Fatal(err)
		}
		out[key(name, line)] = struct {
			resolution, prior, source string
			dst                       int64
		}{res, prior, src, dst}
	}
	return out
}

func key(name string, line int) string {
	return name + "@" + itoa(line)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestCompilerSettlesReferencesTheResolverCouldNot is the feature. Until now
// symbol_references.resolution was written by the heuristic resolver and by
// nothing else, so a reference a typechecker had definitively resolved was
// still recorded as ambiguous and counted against coverage.
func TestCompilerSettlesReferencesTheResolverCouldNot(t *testing.T) {
	f := newCRFixture(t)
	f.addCompilerEdge(t, 100, "Save")
	f.addCompilerEdge(t, 101, "Load")

	st, err := f.reg.MarkCompilerResolutions(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Bound != 2 {
		t.Fatalf("bound %d reference(s), want 2 (Save@100, Load@101): %+v", st.Bound, st)
	}
	if st.FromAmbiguous != 2 {
		t.Errorf("the split does not say what the resolver had concluded: %+v", st)
	}

	got := f.refs(t)
	if r := got["Save@100"]; r.resolution != "resolved" || r.dst != f.symIDs["Save"] {
		t.Errorf("Save@100 is %q -> %d, want resolved -> %d", r.resolution, r.dst, f.symIDs["Save"])
	}
	if r := got["Load@101"]; r.resolution != "resolved" || r.dst != f.symIDs["Load"] {
		t.Errorf("Load@101 is %q -> %d, want resolved -> %d", r.resolution, r.dst, f.symIDs["Load"])
	}
}

// TestCompilerResolutionKeysByNameNotByLine. Line 101 carries a compiler fact
// for Load AND a reference written as Save. Binding by line alone would settle
// `Save` against `Load`'s definition — a wrong answer wearing the compiler's
// authority. The reference row says which identifier it is, so each is settled
// on its own name or not at all.
func TestCompilerResolutionKeysByNameNotByLine(t *testing.T) {
	f := newCRFixture(t)
	f.addCompilerEdge(t, 101, "Load")

	if _, err := f.reg.MarkCompilerResolutions(context.Background(), f.runID); err != nil {
		t.Fatal(err)
	}
	if r := f.refs(t)["Save@101"]; r.resolution != "ambiguous" {
		t.Errorf("Save@101 became %q -> %d; the only compiler fact on that line is Load",
			r.resolution, r.dst)
	}
}

// TestCompilerResolutionDeclinesTwoSymbolsOfTheSameName. Two definitions named
// Save are reachable at line 102. There is no single answer, so the row stays
// ambiguous — widening it on a guess is the lie the exact tier exists to
// prevent.
func TestCompilerResolutionDeclinesTwoSymbolsOfTheSameName(t *testing.T) {
	f := newCRFixture(t)
	f.addCompilerEdge(t, 102, "Save")
	f.addCompilerEdge(t, 102, "OtherSave")

	st, err := f.reg.MarkCompilerResolutions(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Bound != 0 {
		t.Fatalf("bound %d reference(s) where two definitions share the name, want 0", st.Bound)
	}
	if r := f.refs(t)["Save@102"]; r.resolution != "ambiguous" {
		t.Errorf("Save@102 became %q; two Saves is not an answer", r.resolution)
	}
}

// TestCompilerResolutionRecordsItsProvenance. "Bound by a typechecker" and
// "bound by name matching plus an import heuristic" are different claims to
// anyone deciding whether to trust an empty callers result, and the resolver's
// own verdict has to survive because `repocortex accuracy` reads it.
func TestCompilerResolutionRecordsItsProvenance(t *testing.T) {
	f := newCRFixture(t)
	f.addCompilerEdge(t, 100, "Save")

	if _, err := f.reg.MarkCompilerResolutions(context.Background(), f.runID); err != nil {
		t.Fatal(err)
	}
	r := f.refs(t)["Save@100"]
	if r.source != "compiler" {
		t.Errorf("resolution_source is %q, want compiler — a reader cannot tell who bound it", r.source)
	}
	if r.prior != "ambiguous" {
		t.Errorf("prior_resolution is %q, want ambiguous — the resolver's verdict was lost", r.prior)
	}

	stats, err := f.reg.ResolutionStats(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ByCompiler != 1 {
		t.Errorf("ResolutionStats does not surface the compiler-bound count: %+v", stats)
	}
}

// TestCompilerResolutionIsIdempotent. CI re-uploads the same index constantly.
// A second pass must change nothing and must not overwrite the preserved
// resolver verdict with 'resolved'.
func TestCompilerResolutionIsIdempotent(t *testing.T) {
	f := newCRFixture(t)
	f.addCompilerEdge(t, 100, "Save")
	ctx := context.Background()

	first, err := f.reg.MarkCompilerResolutions(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	before := f.refs(t)

	second, err := f.reg.MarkCompilerResolutions(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Bound != 1 || second.Bound != 0 {
		t.Errorf("first pass bound %d, second bound %d; want 1 then 0", first.Bound, second.Bound)
	}
	if got := f.refs(t)["Save@100"]; got != before["Save@100"] {
		t.Errorf("a second pass changed the row: %+v -> %+v", before["Save@100"], got)
	}
}

// TestBindingRateExcludesOnlyWhatItAlsoReports. The denominator change is only
// honest while the excluded population stays visible, so the two must be read
// off the same struct.
func TestBindingRateExcludesOnlyWhatItAlsoReports(t *testing.T) {
	s := ResolutionStats{Total: 100, Resolved: 20, Ambiguous: 30, Unresolved: 50}
	if s.Bindable() != 50 {
		t.Errorf("Bindable() = %d, want 50 (resolved + ambiguous)", s.Bindable())
	}
	if got := s.BindingRate(); got != 0.4 {
		t.Errorf("BindingRate() = %v, want 0.4", got)
	}
	// The excluded population stays on the struct. There is no longer a method
	// for Resolved/Total — that is the point — but the fields BindingRate
	// leaves out must remain readable, or the exclusion stops being honest and
	// a caller loses the ability to recompute the ratio for itself.
	if s.Unresolved != 50 || s.Total != 100 {
		t.Errorf("Unresolved/Total = %d/%d, want 50/100 — BindingRate's denominator "+
			"may exclude these only while they are still reported", s.Unresolved, s.Total)
	}
	empty := ResolutionStats{Total: 10, Unresolved: 10}
	if got := empty.BindingRate(); got != 0 {
		t.Errorf("BindingRate() with nothing bindable = %v, want 0", got)
	}
}
